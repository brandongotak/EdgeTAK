package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/brandongotak/EdgeTAK/internal/audit"
	"github.com/brandongotak/EdgeTAK/internal/store"
)

// Login protection (Server -> Settings -> Login protection): an address
// that fails to sign in with a password too often (10 times in 15 minutes
// by default) is blocked from password sign-ins for a while (30 minutes).
// That covers the console and account page, basic auth on the device API
// (enrollment, the update server) and ATAK's OAuth token sign-in.
// Certificate-authenticated devices and existing sessions keep working, so
// one person mistyping behind a shared address doesn't cut off a team's
// devices. Addresses on the allow list are never blocked (this machine
// always is); addresses on the deny list are refused everything, streaming
// ports included.

var errBlocked = errors.New("blocked")

type loginGuard struct {
	mu      sync.Mutex
	fails   map[string][]failure // by address, oldest first
	blocked map[string]blockInfo
}

type failure struct {
	at   time.Time
	user string
}

type blockInfo struct {
	Since    time.Time `json:"since"`
	Until    time.Time `json:"until"`
	Failures int       `json:"failures"`
	LastUser string    `json:"last_user"`
}

// addrIn reports whether ip is in the list of addresses and CIDR ranges.
func addrIn(ip string, list []string) bool {
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return false
	}
	a = a.Unmap()
	for _, e := range list {
		e = strings.TrimSpace(e)
		if p, err := netip.ParsePrefix(e); err == nil && p.Contains(a) {
			return true
		}
		if b, err := netip.ParseAddr(e); err == nil && b.Unmap() == a {
			return true
		}
	}
	return false
}

func isLoopbackIP(ip string) bool {
	a, err := netip.ParseAddr(ip)
	return err == nil && a.Unmap().IsLoopback()
}

// AddressRefused reports whether a connection from remote (host:port or
// host) is on the deny list; the streaming ports ask it too.
func (s *Server) AddressRefused(remote string) bool {
	ip := remote
	if h, _, err := net.SplitHostPort(remote); err == nil {
		ip = h
	}
	set := s.Store.LoginProtection()
	return !set.Off && len(set.Deny) > 0 && !isLoopbackIP(ip) && addrIn(ip, set.Deny)
}

// blockedUntil is when an address's block ends (zero if it isn't blocked).
func (s *Server) blockedUntil(ip string, now time.Time) time.Time {
	s.guard.mu.Lock()
	defer s.guard.mu.Unlock()
	b, ok := s.guard.blocked[ip]
	if !ok {
		return time.Time{}
	}
	if !now.Before(b.Until) {
		delete(s.guard.blocked, ip)
		return time.Time{}
	}
	return b.Until
}

// passwordSignIn checks a username and password from r's address: refused
// while the address is blocked (errBlocked), and failures counted towards
// a block.
func (s *Server) passwordSignIn(r *http.Request, name, password string) (*store.User, error) {
	return s.passwordSignInFrom(clientIP(r), name, password)
}

// passwordSignInFrom is passwordSignIn for an address known otherwise (the
// video server passes on its clients' addresses).
func (s *Server) passwordSignInFrom(ip, name, password string) (*store.User, error) {
	if s.passwordBlocked(ip) {
		return nil, errBlocked
	}
	u, ok := s.authenticate(name, password)
	if !ok {
		s.signInFailedFrom(ip, name)
		return nil, errBadPassword
	}
	s.guard.mu.Lock()
	delete(s.guard.fails, ip)
	s.guard.mu.Unlock()
	return u, nil
}

// passwordBlocked reports whether password sign-ins from ip are refused now.
func (s *Server) passwordBlocked(ip string) bool {
	return !s.Store.LoginProtection().Off && !s.blockedUntil(ip, time.Now()).IsZero()
}

// signInFailed counts a failed sign-in (a wrong password or second factor)
// and blocks the address when it has failed too often.
func (s *Server) signInFailed(r *http.Request, name string) { s.signInFailedFrom(clientIP(r), name) }

func (s *Server) signInFailedFrom(ip, name string) {
	if ip == "" {
		return // no address to hold it against
	}
	set := s.Store.LoginProtection().WithDefaults()
	if set.Off || isLoopbackIP(ip) || addrIn(ip, set.Allow) {
		return
	}
	now := time.Now()
	window := time.Duration(set.WindowMinutes) * time.Minute
	s.guard.mu.Lock()
	if s.guard.fails == nil {
		s.guard.fails, s.guard.blocked = map[string][]failure{}, map[string]blockInfo{}
	}
	if len(s.guard.fails) > 10000 { // bound memory under address churn
		s.guard.fails = map[string][]failure{}
	}
	list := append(s.guard.fails[ip], failure{now, truncate(name, 64)})
	for len(list) > 0 && now.Sub(list[0].at) > window {
		list = list[1:]
	}
	s.guard.fails[ip] = list
	var block *blockInfo
	if len(list) >= set.MaxFailures {
		b := blockInfo{Since: now, Until: now.Add(time.Duration(set.BlockMinutes) * time.Minute), Failures: len(list), LastUser: truncate(name, 64)}
		s.guard.blocked[ip] = b
		delete(s.guard.fails, ip)
		block = &b
	}
	s.guard.mu.Unlock()
	if block == nil {
		return
	}
	users := map[string]bool{}
	for _, f := range list {
		users[f.user] = true
	}
	tried := make([]string, 0, len(users))
	for u := range users {
		tried = append(tried, u)
	}
	sort.Strings(tried)
	text := fmt.Sprintf("%d failed sign-ins in %d minutes (usernames tried: %s); blocked from password sign-in until %s.",
		block.Failures, set.WindowMinutes, truncate(strings.Join(tried, ", "), 120), block.Until.Local().Format("15:04"))
	s.Log.Warn("address blocked after failed sign-ins", "address", ip, "failures", block.Failures, "until", block.Until)
	if s.Audit != nil {
		s.Audit.Add(audit.Entry{Actor: ip, IP: ip, Action: "Address blocked", Target: "password sign-in", Details: text, Failed: true})
	}
	s.notify(notice{Kind: "security", Title: "Sign-ins blocked from " + ip, Text: text, Groups: []string{}})
}

func (s *Server) unblock(ip string) bool {
	s.guard.mu.Lock()
	defer s.guard.mu.Unlock()
	_, ok := s.guard.blocked[ip]
	delete(s.guard.blocked, ip)
	delete(s.guard.fails, ip)
	return ok
}

type blockedView struct {
	Address string `json:"address"`
	blockInfo
}

type recentFailures struct {
	Address  string    `json:"address"`
	Failures int       `json:"failures"`
	Last     time.Time `json:"last"`
	LastUser string    `json:"last_user"`
}

func (s *Server) loginProtectionView() map[string]any {
	set := s.Store.LoginProtection()
	now := time.Now()
	window := time.Duration(set.WithDefaults().WindowMinutes) * time.Minute
	blocked, recent := []blockedView{}, []recentFailures{}
	s.guard.mu.Lock()
	for ip, b := range s.guard.blocked {
		if now.Before(b.Until) {
			blocked = append(blocked, blockedView{ip, b})
		}
	}
	for ip, list := range s.guard.fails {
		n := 0
		for _, f := range list {
			if now.Sub(f.at) <= window {
				n++
			}
		}
		if n > 0 {
			last := list[len(list)-1]
			recent = append(recent, recentFailures{ip, n, last.at, last.user})
		}
	}
	s.guard.mu.Unlock()
	slices.SortFunc(blocked, func(a, b blockedView) int { return b.Since.Compare(a.Since) })
	slices.SortFunc(recent, func(a, b recentFailures) int { return b.Last.Compare(a.Last) })
	d := set.WithDefaults()
	return map[string]any{"off": set.Off, "max_failures": d.MaxFailures, "window_minutes": d.WindowMinutes, "block_minutes": d.BlockMinutes,
		"allow": nonNilStrings(set.Allow), "deny": nonNilStrings(set.Deny), "blocked": blocked, "recent": recent}
}

func nonNilStrings(l []string) []string {
	if l == nil {
		return []string{}
	}
	return l
}

// cleanAddresses checks a list of IP addresses and CIDR ranges.
func cleanAddresses(in []string) ([]string, error) {
	out := []string{}
	for _, e := range in {
		e = strings.TrimSpace(e)
		if e == "" || slices.Contains(out, e) {
			continue
		}
		if _, err := netip.ParsePrefix(e); err != nil {
			if _, err := netip.ParseAddr(e); err != nil {
				return nil, fmt.Errorf("%q is not an IP address or range (like 192.168.1.0/24)", e)
			}
		}
		out = append(out, e)
	}
	if len(out) > 500 {
		return nil, errors.New("at most 500 entries")
	}
	return out, nil
}

func (s *Server) loginGuardRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/api/login-protection", s.admin(func(w http.ResponseWriter, r *http.Request, _ *identity) {
		v := s.loginProtectionView()
		v["your_address"] = clientIP(r)
		writeJSON(w, http.StatusOK, v)
	}))
	mux.HandleFunc("PUT /admin/api/login-protection", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		var req store.LoginProtection
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
			jsonError(w, http.StatusBadRequest, "invalid request")
			return
		}
		var err error
		if req.Allow, err = cleanAddresses(req.Allow); err == nil {
			req.Deny, err = cleanAddresses(req.Deny)
		}
		if err != nil {
			jsonError(w, http.StatusBadRequest, err.Error())
			return
		}
		// Don't let an admin lock themselves out of the console.
		if me := clientIP(r); addrIn(me, req.Deny) && !isLoopbackIP(me) {
			jsonError(w, http.StatusBadRequest, "the deny list includes your own address ("+me+")")
			return
		}
		req.MaxFailures = max(0, min(req.MaxFailures, 1000))
		req.WindowMinutes = max(0, min(req.WindowMinutes, 24*60))
		req.BlockMinutes = max(0, min(req.BlockMinutes, 30*24*60))
		if err := s.Store.SetLoginProtection(req); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.Log.Info("login protection changed", "admin", id.Username, "off", req.Off, "allow", len(req.Allow), "deny", len(req.Deny))
		writeJSON(w, http.StatusOK, s.loginProtectionView())
	}))
	mux.HandleFunc("DELETE /admin/api/login-protection/blocked/{ip}", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		if !s.unblock(r.PathValue("ip")) {
			jsonError(w, http.StatusNotFound, "that address isn't blocked")
			return
		}
		s.Log.Info("address unblocked", "admin", id.Username, "address", r.PathValue("ip"))
		writeJSON(w, http.StatusOK, s.loginProtectionView())
	}))
}
