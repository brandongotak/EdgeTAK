package web

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/brandongotak/EdgeTAK/internal/store"
	"github.com/go-ldap/ldap/v3"
)

// Directory sign-in (LDAP / Active Directory). Every password check goes
// through authenticate: local accounts are checked as before, and other
// names against the directory when it is on. A directory user gets a local
// record (no password) holding its groups and certificates; its groups are
// refreshed at each sign-in and by a periodic sync, for devices that only
// use their certificate. Local accounts, the admin included, keep working
// when the directory is unreachable.

const (
	ldapTimeout   = 10 * time.Second
	ldapCacheFor  = 5 * time.Minute // a successful sign-in is reused this long (ATAK signs in often)
	ldapSyncEvery = 30 * time.Minute
)

// DefaultUserFilter finds a user by uid (OpenLDAP, FreeIPA), sAMAccountName
// or userPrincipalName (Active Directory).
const DefaultUserFilter = "(&(|(objectClass=person)(objectClass=inetOrgPerson)(objectClass=user))(|(uid={username})(sAMAccountName={username})(userPrincipalName={username})))"

type ldapState struct {
	mu    sync.Mutex
	cache map[string]time.Time // username + password hash -> until
}

// directoryUser is what a directory says about a user.
type directoryUser struct {
	DN       string   `json:"dn"`
	Groups   []string `json:"groups"`     // as used by EdgeTAK
	AllRaw   []string `json:"all_groups"` // every group found, for the Test button
	Admin    bool     `json:"admin"`
	Allowed  bool     `json:"allowed"` // in the required group, if one is set
	Username string   `json:"username"`
}

// errDirectory wraps failures to reach or search the directory (as opposed
// to a wrong password).
type errDirectory struct{ err error }

func (e errDirectory) Error() string { return e.err.Error() }

var errBadPassword = errors.New("the directory rejected the username or password")

// authenticate checks a username and password, locally or against the
// directory.
func (s *Server) authenticate(name, password string) (*store.User, bool) {
	if u, ok := s.Store.User(name); ok && u.Source != "ldap" {
		return s.Store.Authenticate(name, password)
	}
	set := s.Store.LDAP()
	if !set.Enabled || password == "" || !usernameRE.MatchString(name) {
		return s.Store.Authenticate(name, password) // fails the same way, in the same time
	}
	name = strings.ToLower(name)
	key := name + "\x00" + fmt.Sprintf("%x", sha256.Sum256([]byte(password)))
	s.ldap.mu.Lock()
	until, cached := s.ldap.cache[key]
	s.ldap.mu.Unlock()
	if cached && time.Now().Before(until) {
		if u, ok := s.Store.User(name); ok && u.Source == "ldap" {
			return u, true
		}
	}
	d, err := ldapLookup(set, name, password)
	if err != nil {
		var de errDirectory
		if errors.As(err, &de) {
			s.Log.Warn("directory sign-in failed", "user", name, "err", err)
		}
		return nil, false
	}
	if !d.Allowed {
		s.Log.Info("directory sign-in refused: not in the required group", "user", name, "group", set.RequiredGroup)
		return nil, false
	}
	var admin *bool
	if set.AdminGroup != "" {
		admin = &d.Admin
	}
	u, err := s.Store.UpsertDirectoryUser(name, d.DN, d.Groups, admin)
	if err != nil {
		s.Log.Warn("directory sign-in", "user", name, "err", err)
		return nil, false
	}
	s.ldap.mu.Lock()
	if s.ldap.cache == nil {
		s.ldap.cache = map[string]time.Time{}
	}
	for k, t := range s.ldap.cache {
		if time.Now().After(t) {
			delete(s.ldap.cache, k)
		}
	}
	s.ldap.cache[key] = time.Now().Add(ldapCacheFor)
	s.ldap.mu.Unlock()
	if !cached {
		s.Log.Info("directory sign-in", "user", name, "groups", d.Groups, "admin", u.Admin)
	}
	return u, true
}

func ldapDial(set store.LDAPSettings) (*ldap.Conn, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: set.SkipVerify}
	if set.CAPEM != "" {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(set.CAPEM)) {
			return nil, errDirectory{errors.New("the directory CA certificate is not PEM")}
		}
		cfg.RootCAs = pool
	}
	if host := hostOf(set.URL); host != "" {
		cfg.ServerName = host
	}
	conn, err := ldap.DialURL(set.URL, ldap.DialWithTLSConfig(cfg), ldap.DialWithDialer(&net.Dialer{Timeout: ldapTimeout}))
	if err != nil {
		return nil, errDirectory{fmt.Errorf("could not connect to %s: %w%s", set.URL, err, tlsHint(err))}
	}
	conn.SetTimeout(ldapTimeout)
	if set.StartTLS {
		if err := conn.StartTLS(cfg); err != nil {
			conn.Close()
			return nil, errDirectory{fmt.Errorf("StartTLS: %w%s", err, tlsHint(err))}
		}
	}
	return conn, nil
}

// tlsHint explains certificate errors.
func tlsHint(err error) string {
	if strings.Contains(err.Error(), "x509") || strings.Contains(err.Error(), "certificate") {
		return " (paste the directory's CA certificate, or, only for testing, turn off the certificate check)"
	}
	return ""
}

func hostOf(u string) string {
	u = strings.TrimPrefix(strings.TrimPrefix(u, "ldaps://"), "ldap://")
	if i := strings.IndexAny(u, ":/"); i >= 0 {
		u = u[:i]
	}
	return u
}

// serviceBind signs in as the search account, or anonymously.
func serviceBind(conn *ldap.Conn, set store.LDAPSettings) error {
	if set.BindDN == "" {
		return nil
	}
	if err := conn.Bind(set.BindDN, set.BindPassword); err != nil {
		return errDirectory{fmt.Errorf("the search account (bind DN) could not sign in: %w", err)}
	}
	return nil
}

// ldapLookup finds a user and, with a password, checks it; without one
// (sync) it only reads the user's groups.
func ldapLookup(set store.LDAPSettings, name, password string) (*directoryUser, error) {
	conn, err := ldapDial(set)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if err := serviceBind(conn, set); err != nil {
		return nil, err
	}
	filter := strings.ReplaceAll(firstNonEmpty(set.UserFilter, DefaultUserFilter), "{username}", ldap.EscapeFilter(name))
	res, err := conn.Search(ldap.NewSearchRequest(set.BaseDN, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 2, int(ldapTimeout/time.Second), false,
		filter, []string{"memberOf"}, nil))
	if err != nil {
		return nil, errDirectory{fmt.Errorf("searching for the user: %w", err)}
	}
	if len(res.Entries) != 1 {
		if len(res.Entries) > 1 {
			return nil, errDirectory{fmt.Errorf("more than one entry matches %q; make the user filter stricter", name)}
		}
		return nil, errBadPassword // not found: same answer as a wrong password
	}
	entry := res.Entries[0]
	if password != "" {
		if err := conn.Bind(entry.DN, password); err != nil {
			return nil, errBadPassword
		}
		if err := serviceBind(conn, set); err != nil { // back to the search account for the groups
			return nil, err
		}
	}
	d := &directoryUser{DN: entry.DN, Username: name}
	for _, g := range entry.GetAttributeValues("memberOf") {
		if cn := firstCN(g); cn != "" {
			d.AllRaw = append(d.AllRaw, cn)
		}
	}
	if set.GroupBaseDN != "" {
		gf := fmt.Sprintf("(|(member=%s)(uniqueMember=%s)(memberUid=%s))", ldap.EscapeFilter(entry.DN), ldap.EscapeFilter(entry.DN), ldap.EscapeFilter(name))
		gres, err := conn.Search(ldap.NewSearchRequest(set.GroupBaseDN, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 0, int(ldapTimeout/time.Second), false,
			gf, []string{"cn"}, nil))
		if err != nil {
			return nil, errDirectory{fmt.Errorf("searching for the user's groups: %w", err)}
		}
		for _, g := range gres.Entries {
			if cn := g.GetAttributeValue("cn"); cn != "" {
				d.AllRaw = append(d.AllRaw, cn)
			}
		}
	}
	slices.Sort(d.AllRaw)
	d.AllRaw = slices.Compact(d.AllRaw)
	has := func(group string) bool {
		return slices.ContainsFunc(d.AllRaw, func(g string) bool { return strings.EqualFold(g, group) })
	}
	for _, g := range d.AllRaw {
		if len(set.Groups) == 0 || slices.ContainsFunc(set.Groups, func(w string) bool { return strings.EqualFold(w, g) }) {
			d.Groups = append(d.Groups, g)
		}
	}
	d.Admin = set.AdminGroup != "" && has(set.AdminGroup)
	d.Allowed = set.RequiredGroup == "" || has(set.RequiredGroup)
	return d, nil
}

// firstCN is the common name in a DN ("CN=TAK Users,OU=Groups,DC=..." ->
// "TAK Users").
func firstCN(dn string) string {
	parsed, err := ldap.ParseDN(dn)
	if err != nil || len(parsed.RDNs) == 0 {
		return ""
	}
	for _, a := range parsed.RDNs[0].Attributes {
		if strings.EqualFold(a.Type, "cn") {
			return a.Value
		}
	}
	return ""
}

// ldapSyncStart refreshes directory users' groups, so devices that only
// use their certificate follow directory changes.
func (s *Server) ldapSyncStart(ctx context.Context) {
	go func() {
		t := time.NewTicker(ldapSyncEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.SyncDirectoryUsers()
			}
		}
	}()
}

// SyncDirectoryUsers reads every directory user's groups again. Users no
// longer found are marked so the Users tab shows it; their certificates
// are left to the admin.
func (s *Server) SyncDirectoryUsers() {
	set := s.Store.LDAP()
	if !set.Enabled || set.BindDN == "" {
		return // without a search account a user can only be read with its password
	}
	for _, u := range s.Store.Users() {
		if u.Source != "ldap" {
			continue
		}
		d, err := ldapLookup(set, u.Username, "")
		var de errDirectory
		switch {
		case errors.As(err, &de):
			s.Log.Warn("directory sync stopped", "err", err)
			return // the directory is unreachable: change nothing
		case err != nil || !d.Allowed:
			if !u.DirectoryOff {
				s.Store.UpdateUser(u.Username, func(x *store.User) error { x.DirectoryOff = true; return nil })
				s.Log.Warn("directory user not found or no longer allowed", "user", u.Username)
			}
			continue
		}
		var admin *bool
		if set.AdminGroup != "" {
			admin = &d.Admin
		}
		if slices.Equal(u.Groups, d.Groups) && !u.DirectoryOff && (admin == nil || *admin == u.Admin) {
			continue
		}
		if _, err := s.Store.UpsertDirectoryUser(u.Username, d.DN, d.Groups, admin); err == nil {
			s.applyUserGroups(u.Username)
			s.Log.Info("directory user updated", "user", u.Username, "groups", d.Groups)
		}
	}
}

// ---- API ----

func (s *Server) ldapView() map[string]any {
	set := s.Store.LDAP()
	hasPassword := set.BindPassword != ""
	set.BindPassword = ""
	users := 0
	for _, u := range s.Store.Users() {
		if u.Source == "ldap" {
			users++
		}
	}
	return map[string]any{"settings": set, "has_bind_password": hasPassword, "users": users, "default_filter": DefaultUserFilter}
}

func cleanLDAP(l store.LDAPSettings, old store.LDAPSettings) (store.LDAPSettings, error) {
	l.URL, l.BaseDN, l.BindDN = strings.TrimSpace(l.URL), strings.TrimSpace(l.BaseDN), strings.TrimSpace(l.BindDN)
	l.UserFilter, l.GroupBaseDN = strings.TrimSpace(l.UserFilter), strings.TrimSpace(l.GroupBaseDN)
	if l.BindPassword == "" {
		l.BindPassword = old.BindPassword // the console never sees it: empty keeps it
	}
	var groups []string
	for _, g := range l.Groups {
		if g = strings.TrimSpace(g); g != "" {
			groups = append(groups, g)
		}
	}
	l.Groups = groups
	if !l.Enabled {
		return l, nil
	}
	if !strings.HasPrefix(l.URL, "ldap://") && !strings.HasPrefix(l.URL, "ldaps://") {
		return l, errors.New("the server address starts with ldap:// or ldaps://")
	}
	if l.BaseDN == "" {
		return l, errors.New("the base DN to search for users is needed")
	}
	if l.UserFilter != "" && !strings.Contains(l.UserFilter, "{username}") {
		return l, errors.New("the user filter must contain {username}")
	}
	return l, nil
}

func (s *Server) ldapRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/api/ldap", s.admin(func(w http.ResponseWriter, r *http.Request, _ *identity) {
		writeJSON(w, http.StatusOK, s.ldapView())
	}))
	mux.HandleFunc("PUT /admin/api/ldap", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		var l store.LDAPSettings
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&l); err != nil {
			jsonError(w, http.StatusBadRequest, "invalid request")
			return
		}
		l, err := cleanLDAP(l, s.Store.LDAP())
		if err != nil {
			jsonError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := s.Store.SetLDAP(l); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.ldap.mu.Lock()
		s.ldap.cache = nil // settings changed: check against the directory again
		s.ldap.mu.Unlock()
		s.Log.Info("directory sign-in settings changed", "admin", id.Username, "enabled", l.Enabled, "url", l.URL)
		writeJSON(w, http.StatusOK, s.ldapView())
	}))
	// Test the settings in the form (not yet saved) with a user's sign-in.
	mux.HandleFunc("POST /admin/api/ldap/test", s.admin(func(w http.ResponseWriter, r *http.Request, _ *identity) {
		var req struct {
			Settings store.LDAPSettings `json:"settings"`
			Username string             `json:"username"`
			Password string             `json:"password"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
			jsonError(w, http.StatusBadRequest, "invalid request")
			return
		}
		req.Settings.Enabled = true
		set, err := cleanLDAP(req.Settings, s.Store.LDAP())
		if err != nil {
			jsonError(w, http.StatusBadRequest, err.Error())
			return
		}
		if req.Username == "" {
			// Only the connection and the search account.
			conn, err := ldapDial(set)
			if err == nil {
				err = serviceBind(conn, set)
				conn.Close()
			}
			if err != nil {
				jsonError(w, http.StatusBadGateway, err.Error())
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"connected": true})
			return
		}
		d, err := ldapLookup(set, strings.ToLower(req.Username), req.Password)
		if err != nil {
			jsonError(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"connected": true, "user": d})
	}))
	mux.HandleFunc("POST /admin/api/ldap/sync", s.admin(func(w http.ResponseWriter, r *http.Request, _ *identity) {
		s.SyncDirectoryUsers()
		writeJSON(w, http.StatusOK, s.ldapView())
	}))
}
