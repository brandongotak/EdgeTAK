// Package store persists users, issued certificates and data package metadata
// in a single JSON file. It is intended for edge deployments with tens to low
// thousands of records, where avoiding an external database matters more than
// write throughput.
package store

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// User group membership follows the official server's directions: IN groups
// are what the user receives and sees, OUT groups are what the user's own
// traffic and uploads go to. Groups lists memberships in both directions.
type User struct {
	Username     string   `json:"username"`
	PasswordHash string   `json:"password_hash"`
	Groups       []string `json:"groups"`               // IN and OUT
	GroupsIn     []string `json:"groups_in,omitempty"`  // receive only
	GroupsOut    []string `json:"groups_out,omitempty"` // send only
	// Inactive lists memberships the user switched off from their client
	// (ATAK "Channels"), as "group:IN" / "group:OUT".
	Inactive []string  `json:"inactive,omitempty"`
	Admin    bool      `json:"admin"`
	Created  time.Time `json:"created"`
}

// Memberships returns all of the user's IN and OUT groups, active or not. A
// user without any group is in anon both ways, like anonymous clients.
func (u *User) Memberships(anon string) (in, out []string) {
	if len(u.Groups)+len(u.GroupsIn)+len(u.GroupsOut) == 0 {
		return []string{anon}, []string{anon}
	}
	return union(u.Groups, u.GroupsIn), union(u.Groups, u.GroupsOut)
}

// Effective returns the IN and OUT groups currently in force: memberships
// minus those the user deactivated.
func (u *User) Effective(anon string) (in, out []string) {
	in, out = u.Memberships(anon)
	active := func(l []string, dir string) []string {
		return slices.DeleteFunc(l, func(g string) bool { return !u.IsActive(g, dir) })
	}
	return active(in, "IN"), active(out, "OUT")
}

// IsActive reports whether the membership group/dir ("IN" or "OUT") is on.
func (u *User) IsActive(group, dir string) bool {
	return !slices.Contains(u.Inactive, group+":"+dir)
}

// Direction returns how the user belongs to group: "both", "in", "out" or "".
func (u *User) Direction(group string) string {
	switch {
	case slices.Contains(u.Groups, group):
		return "both"
	case slices.Contains(u.GroupsIn, group):
		return "in"
	case slices.Contains(u.GroupsOut, group):
		return "out"
	}
	return ""
}

// SetDirection makes the user a member of group in direction ("both", "in",
// "out"), or removes the membership for "".
func (u *User) SetDirection(group, dir string) {
	drop := func(l []string) []string {
		return slices.DeleteFunc(slices.Clone(l), func(g string) bool { return g == group })
	}
	u.Groups, u.GroupsIn, u.GroupsOut = drop(u.Groups), drop(u.GroupsIn), drop(u.GroupsOut)
	if dir == "" {
		u.Inactive = slices.DeleteFunc(slices.Clone(u.Inactive), func(k string) bool { return strings.HasPrefix(k, group+":") })
	}
	switch dir {
	case "both":
		u.Groups = append(u.Groups, group)
	case "in":
		u.GroupsIn = append(u.GroupsIn, group)
	case "out":
		u.GroupsOut = append(u.GroupsOut, group)
	}
}

func union(a, b []string) []string {
	out := slices.Clone(a)
	for _, g := range b {
		if !slices.Contains(out, g) {
			out = append(out, g)
		}
	}
	if out == nil {
		out = []string{}
	}
	return out
}

// Group is a named group managed in the admin console.
type Group struct {
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Created     time.Time `json:"created"`
}

type Cert struct {
	Serial     string    `json:"serial"` // hex
	CommonName string    `json:"common_name"`
	ClientUID  string    `json:"client_uid,omitempty"`
	Issued     time.Time `json:"issued"`
	Expires    time.Time `json:"expires"`
	Revoked    bool      `json:"revoked"`
}

type Package struct {
	Hash       string    `json:"hash"` // sha256 hex, also the storage key
	Name       string    `json:"name"`
	MIMEType   string    `json:"mime_type"`
	Size       int64     `json:"size"`
	Keywords   []string  `json:"keywords"`
	Tool       string    `json:"tool"` // "public" packages appear in search
	CreatorUID string    `json:"creator_uid"`
	Submitter  string    `json:"submitter"`
	Groups     []string  `json:"groups"`
	Submitted  time.Time `json:"submitted"`
	Seq        int64     `json:"seq"` // stable numeric id (Marti PrimaryKey)
}

type db struct {
	Users    map[string]*User    `json:"users"`
	Certs    map[string]*Cert    `json:"certs"`
	Packages map[string]*Package `json:"packages"`
	Groups   map[string]*Group   `json:"groups"`
	NextSeq  int64               `json:"next_seq"`

	ServerContact ServerContact  `json:"server_contact"`
	Inbox         []InboxMessage `json:"inbox"`

	Registration RegistrationSettings      `json:"registration"`
	Invites      map[string]*Invite        `json:"invites"`  // by code
	Requests     map[string]*SignupRequest `json:"requests"` // by id

	MapSources []MapSource `json:"map_sources"`

	History HistorySettings `json:"history"`
}

// HistorySettings controls position history recording (off by default).
type HistorySettings struct {
	Enabled        bool `json:"enabled"`
	RetentionHours int  `json:"retention_hours"` // 0 = default (24)
}

// MapSource is a custom tile layer for the console map. URL is a template
// with {z}, {x} and {y} (or {-y} for TMS), optionally {s} for subdomains.
type MapSource struct {
	Name        string `json:"name"`
	URL         string `json:"url"`
	MinZoom     int    `json:"min_zoom"`
	MaxZoom     int    `json:"max_zoom"`
	Overlay     bool   `json:"overlay"`              // drawn on top of the base map
	Subdomains  string `json:"subdomains,omitempty"` // values for {s}, e.g. "abc" or "mt0,mt1"
	Attribution string `json:"attribution,omitempty"`
}

// RegistrationSettings controls sign-up without an invite: "off" (default),
// "approval" (requests wait for an admin) or "open" (accounts are created
// at once in DefaultGroups). Invites work in every mode.
type RegistrationSettings struct {
	Mode          string   `json:"mode"`
	DefaultGroups []string `json:"default_groups"`
}

// Invite lets people create their own account in Groups.
type Invite struct {
	Code      string    `json:"code"`
	Label     string    `json:"label"`
	Groups    []string  `json:"groups"`
	MaxUses   int       `json:"max_uses"` // 0 = unlimited
	Uses      int       `json:"uses"`
	Expires   time.Time `json:"expires"` // zero = never
	Created   time.Time `json:"created"`
	CreatedBy string    `json:"created_by"`
	Revoked   bool      `json:"revoked"`
}

// Usable reports whether the invite can still create an account.
func (i *Invite) Usable(now time.Time) bool {
	return !i.Revoked && (i.MaxUses == 0 || i.Uses < i.MaxUses) && (i.Expires.IsZero() || now.Before(i.Expires))
}

// SignupRequest is a pending self-registration awaiting approval.
type SignupRequest struct {
	ID           string    `json:"id"`
	Username     string    `json:"username"`
	PasswordHash string    `json:"password_hash,omitempty"`
	Note         string    `json:"note"`
	Remote       string    `json:"remote"`
	Created      time.Time `json:"created"`
}

// ServerContact is the server's presence as a contact on devices, which
// direct messages come from and replies go to.
type ServerContact struct {
	Enabled  bool    `json:"enabled"`
	Callsign string  `json:"callsign"`
	Lat      float64 `json:"lat"`
	Lon      float64 `json:"lon"`
}

// InboxMessage is a chat message a device sent to the server contact.
type InboxMessage struct {
	ID           string    `json:"id"`
	Time         time.Time `json:"time"`
	FromUID      string    `json:"from_uid"`
	FromCallsign string    `json:"from_callsign"`
	Username     string    `json:"username"`
	Text         string    `json:"text"`
}

const maxInbox = 500

var (
	ErrNotFound = errors.New("not found")
	ErrExists   = errors.New("already exists")
)

type Store struct {
	mu   sync.RWMutex
	path string
	db   db
}

// Open loads the store at path, creating an empty one if needed.
func Open(path string) (*Store, error) {
	s := &Store{path: path, db: db{Users: map[string]*User{}, Certs: map[string]*Cert{}, Packages: map[string]*Package{}, Groups: map[string]*Group{},
		Invites: map[string]*Invite{}, Requests: map[string]*SignupRequest{}, NextSeq: 1}}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, s.save()
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &s.db); err != nil {
		return nil, err
	}
	if s.db.Users == nil {
		s.db.Users = map[string]*User{}
	}
	if s.db.Certs == nil {
		s.db.Certs = map[string]*Cert{}
	}
	if s.db.Packages == nil {
		s.db.Packages = map[string]*Package{}
	}
	if s.db.Groups == nil {
		s.db.Groups = map[string]*Group{}
	}
	if s.db.Invites == nil {
		s.db.Invites = map[string]*Invite{}
	}
	if s.db.Requests == nil {
		s.db.Requests = map[string]*SignupRequest{}
	}
	// Groups used to exist only as names on users; give them records.
	for _, u := range s.db.Users {
		s.ensureGroupsLocked(append(append(slices.Clone(u.Groups), u.GroupsIn...), u.GroupsOut...))
	}
	return s, nil
}

// save writes the database atomically. Callers hold s.mu.
func (s *Store) save() error {
	data, err := json.MarshalIndent(&s.db, "", " ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// ---- users ----

func cloneUser(u *User) *User {
	c := *u
	c.Groups = slices.Clone(u.Groups)
	c.GroupsIn = slices.Clone(u.GroupsIn)
	c.GroupsOut = slices.Clone(u.GroupsOut)
	c.Inactive = slices.Clone(u.Inactive)
	return &c
}

// ---- self-registration ----

func (s *Store) Registration() RegistrationSettings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r := s.db.Registration
	r.DefaultGroups = slices.Clone(r.DefaultGroups)
	if r.Mode == "" {
		r.Mode = "off"
	}
	return r
}

func (s *Store) SetRegistration(r RegistrationSettings) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.db.Registration = r
	s.ensureGroupsLocked(r.DefaultGroups)
	return s.save()
}

func cloneInvite(i *Invite) *Invite {
	c := *i
	c.Groups = slices.Clone(i.Groups)
	return &c
}

func (s *Store) CreateInvite(i Invite) (*Invite, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.db.Invites[i.Code]; ok || i.Code == "" {
		return nil, ErrExists
	}
	s.db.Invites[i.Code] = &i
	s.ensureGroupsLocked(i.Groups)
	return cloneInvite(&i), s.save()
}

// Invites lists invites, newest first.
func (s *Store) Invites() []*Invite {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Invite, 0, len(s.db.Invites))
	for _, i := range s.db.Invites {
		out = append(out, cloneInvite(i))
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Created.After(out[b].Created) })
	return out
}

func (s *Store) Invite(code string) (*Invite, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	i, ok := s.db.Invites[code]
	if !ok {
		return nil, false
	}
	return cloneInvite(i), true
}

func (s *Store) RevokeInvite(code string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i, ok := s.db.Invites[code]
	if !ok {
		return ErrNotFound
	}
	i.Revoked = true
	return s.save()
}

var ErrInviteUnusable = errors.New("invite expired, used up or revoked")

// RegisterWithInvite creates a user in the invite's groups and counts the
// use, atomically, so a single-use invite cannot be used twice.
func (s *Store) RegisterWithInvite(code, username, passwordHash string) (*User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	inv, ok := s.db.Invites[code]
	if !ok || !inv.Usable(time.Now()) {
		return nil, ErrInviteUnusable
	}
	if err := s.usernameFreeLocked(username); err != nil {
		return nil, err
	}
	inv.Uses++
	u := &User{Username: username, PasswordHash: passwordHash, Groups: slices.Clone(inv.Groups), Created: time.Now().UTC()}
	s.db.Users[username] = u
	return cloneUser(u), s.save()
}

// CreateUserWithHash adds a user whose password is already hashed.
func (s *Store) CreateUserWithHash(username, passwordHash string, groups []string) (*User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usernameFreeLocked(username); err != nil {
		return nil, err
	}
	u := &User{Username: username, PasswordHash: passwordHash, Groups: slices.Clone(groups), Created: time.Now().UTC()}
	s.db.Users[username] = u
	s.ensureGroupsLocked(groups)
	return cloneUser(u), s.save()
}

// usernameFreeLocked fails if the name is taken by a user or a pending
// request (case-insensitively, to avoid look-alike accounts).
func (s *Store) usernameFreeLocked(name string) error {
	for n := range s.db.Users {
		if strings.EqualFold(n, name) {
			return ErrExists
		}
	}
	for _, r := range s.db.Requests {
		if strings.EqualFold(r.Username, name) {
			return ErrExists
		}
	}
	return nil
}

func (s *Store) AddRequest(r SignupRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usernameFreeLocked(r.Username); err != nil {
		return err
	}
	if len(s.db.Requests) >= 200 {
		return errors.New("too many pending requests")
	}
	s.db.Requests[r.ID] = &r
	return s.save()
}

// Requests lists pending sign-ups, oldest first. Password hashes are omitted.
func (s *Store) Requests() []SignupRequest {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]SignupRequest, 0, len(s.db.Requests))
	for _, r := range s.db.Requests {
		c := *r
		c.PasswordHash = ""
		out = append(out, c)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Created.Before(out[b].Created) })
	return out
}

// TakeRequest removes and returns a pending request.
func (s *Store) TakeRequest(id string) (*SignupRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.db.Requests[id]
	if !ok {
		return nil, ErrNotFound
	}
	delete(s.db.Requests, id)
	return r, s.save()
}

func (s *Store) ServerContact() ServerContact {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.db.ServerContact
}

func (s *Store) SetServerContact(c ServerContact) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.db.ServerContact = c
	return s.save()
}

func (s *Store) HistorySettings() HistorySettings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	h := s.db.History
	if h.RetentionHours <= 0 {
		h.RetentionHours = 24
	}
	return h
}

func (s *Store) SetHistorySettings(h HistorySettings) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.db.History = h
	return s.save()
}

func (s *Store) MapSources() []MapSource {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]MapSource{}, s.db.MapSources...)
}

func (s *Store) SetMapSources(m []MapSource) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.db.MapSources = append([]MapSource{}, m...)
	return s.save()
}

// AddInbox stores a message sent to the server contact, keeping the newest.
func (s *Store) AddInbox(m InboxMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.db.Inbox = append(s.db.Inbox, m)
	if n := len(s.db.Inbox); n > maxInbox {
		s.db.Inbox = slices.Clone(s.db.Inbox[n-maxInbox:])
	}
	return s.save()
}

// Inbox returns messages to the server contact, newest first.
func (s *Store) Inbox() []InboxMessage {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := slices.Clone(s.db.Inbox)
	slices.Reverse(out)
	if out == nil {
		out = []InboxMessage{}
	}
	return out
}

// SetActive records which of username's memberships are switched on;
// active maps "group:IN" / "group:OUT" to its state. Memberships not named
// keep their current state.
func (s *Store) SetActive(username string, active map[string]bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.db.Users[username]
	if !ok {
		return ErrNotFound
	}
	for key, on := range active {
		has := slices.Contains(u.Inactive, key)
		if on && has {
			u.Inactive = slices.DeleteFunc(u.Inactive, func(k string) bool { return k == key })
		} else if !on && !has {
			u.Inactive = append(u.Inactive, key)
		}
	}
	return s.save()
}

// ---- groups ----

// GroupInfo is a group with its current members.
type GroupInfo struct {
	Group
	Members []Member `json:"members"`
}

type Member struct {
	Username  string `json:"username"`
	Direction string `json:"direction"` // both, in, out
}

// ensureGroupsLocked creates records for unknown group names. Callers hold s.mu.
func (s *Store) ensureGroupsLocked(names []string) {
	for _, n := range names {
		if _, ok := s.db.Groups[n]; !ok && n != "" {
			s.db.Groups[n] = &Group{Name: n, Created: time.Now().UTC()}
		}
	}
}

// Groups lists every group with its members, sorted by name.
func (s *Store) Groups() []GroupInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]GroupInfo, 0, len(s.db.Groups))
	for _, g := range s.db.Groups {
		gi := GroupInfo{Group: *g, Members: []Member{}}
		for _, u := range s.db.Users {
			if d := u.Direction(g.Name); d != "" {
				gi.Members = append(gi.Members, Member{u.Username, d})
			}
		}
		sort.Slice(gi.Members, func(i, j int) bool { return gi.Members[i].Username < gi.Members[j].Username })
		out = append(out, gi)
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out
}

func (s *Store) Group(name string) (*Group, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	g, ok := s.db.Groups[name]
	if !ok {
		return nil, false
	}
	c := *g
	return &c, true
}

func (s *Store) CreateGroup(name, description string) (*Group, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.db.Groups[name]; ok {
		return nil, ErrExists
	}
	g := &Group{Name: name, Description: description, Created: time.Now().UTC()}
	s.db.Groups[name] = g
	c := *g
	return &c, s.save()
}

// UpdateGroup changes a group's description and optionally renames it; a
// rename is applied to every user and data package. It returns the users
// whose memberships changed.
func (s *Store) UpdateGroup(name, newName, description string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.db.Groups[name]
	if !ok {
		return nil, ErrNotFound
	}
	g.Description = description
	var affected []string
	if newName != "" && newName != name {
		if _, ok := s.db.Groups[newName]; ok {
			return nil, ErrExists
		}
		delete(s.db.Groups, name)
		g.Name = newName
		s.db.Groups[newName] = g
		for _, u := range s.db.Users {
			if d := u.Direction(name); d != "" {
				u.SetDirection(name, "")
				u.SetDirection(newName, d)
				affected = append(affected, u.Username)
			}
		}
		for _, p := range s.db.Packages {
			if i := slices.Index(p.Groups, name); i >= 0 {
				p.Groups[i] = newName
			}
		}
	}
	return affected, s.save()
}

// DeleteGroup removes a group and every membership in it. It returns the
// users whose memberships changed.
func (s *Store) DeleteGroup(name string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.db.Groups[name]; !ok {
		return nil, ErrNotFound
	}
	delete(s.db.Groups, name)
	var affected []string
	for _, u := range s.db.Users {
		if u.Direction(name) != "" {
			u.SetDirection(name, "")
			affected = append(affected, u.Username)
		}
	}
	return affected, s.save()
}

// SetMembership sets username's direction in group ("" removes it).
func (s *Store) SetMembership(group, username, dir string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.db.Groups[group]; !ok {
		return ErrNotFound
	}
	u, ok := s.db.Users[username]
	if !ok {
		return ErrNotFound
	}
	u.SetDirection(group, dir)
	return s.save()
}

func HashPassword(pw string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	return string(h), err
}

func (s *Store) User(name string) (*User, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	u, ok := s.db.Users[name]
	if !ok {
		return nil, false
	}
	return cloneUser(u), true
}

func (s *Store) Users() []*User {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*User, 0, len(s.db.Users))
	for _, u := range s.db.Users {
		out = append(out, cloneUser(u))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Username < out[j].Username })
	return out
}

func (s *Store) HasAdmin() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, u := range s.db.Users {
		if u.Admin {
			return true
		}
	}
	return false
}

// CreateUser adds a new user with the given plaintext password.
func (s *Store) CreateUser(name, password string, groups []string, admin bool) (*User, error) {
	hash, err := HashPassword(password)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.db.Users[name]; ok {
		return nil, ErrExists
	}
	u := &User{Username: name, PasswordHash: hash, Groups: slices.Clone(groups), Admin: admin, Created: time.Now().UTC()}
	s.db.Users[name] = u
	s.ensureGroupsLocked(groups)
	return cloneUser(u), s.save()
}

// UpdateUser applies fn to the stored user and persists the result.
func (s *Store) UpdateUser(name string, fn func(u *User) error) (*User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.db.Users[name]
	if !ok {
		return nil, ErrNotFound
	}
	c := cloneUser(u)
	if err := fn(c); err != nil {
		return nil, err
	}
	c.Username = name
	s.db.Users[name] = c
	s.ensureGroupsLocked(append(append(slices.Clone(c.Groups), c.GroupsIn...), c.GroupsOut...))
	return cloneUser(c), s.save()
}

// SetPassword replaces a user's password.
func (s *Store) SetPassword(name, password string) error {
	hash, err := HashPassword(password)
	if err != nil {
		return err
	}
	_, err = s.UpdateUser(name, func(u *User) error { u.PasswordHash = hash; return nil })
	return err
}

// DeleteUser removes a user and revokes every certificate issued to them.
func (s *Store) DeleteUser(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.db.Users[name]; !ok {
		return ErrNotFound
	}
	delete(s.db.Users, name)
	for _, c := range s.db.Certs {
		if c.CommonName == name {
			c.Revoked = true
		}
	}
	return s.save()
}

// dummyHash makes failed lookups cost the same as failed passwords.
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("edgetak-dummy"), bcrypt.DefaultCost)

// Authenticate checks a username and password.
func (s *Store) Authenticate(name, password string) (*User, bool) {
	u, ok := s.User(name)
	if !ok {
		bcrypt.CompareHashAndPassword(dummyHash, []byte(password))
		return nil, false
	}
	if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)) != nil {
		return nil, false
	}
	return u, true
}

// ---- certificates ----

func (s *Store) AddCert(c Cert) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.db.Certs[c.Serial] = &c
	return s.save()
}

func (s *Store) Certs() []Cert {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Cert, 0, len(s.db.Certs))
	for _, c := range s.db.Certs {
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Issued.After(out[j].Issued) })
	return out
}

func (s *Store) RevokeCert(serial string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.db.Certs[serial]
	if !ok {
		return ErrNotFound
	}
	c.Revoked = true
	return s.save()
}

// IsRevoked reports whether serial was revoked. Unknown serials (e.g. the
// server's own certificate) are not revoked.
func (s *Store) IsRevoked(serial string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.db.Certs[serial]
	return ok && c.Revoked
}

// ---- data packages ----

func clonePackage(p *Package) *Package {
	c := *p
	c.Keywords = slices.Clone(p.Keywords)
	c.Groups = slices.Clone(p.Groups)
	return &c
}

func (s *Store) Package(hash string) (*Package, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.db.Packages[hash]
	if !ok {
		return nil, false
	}
	return clonePackage(p), true
}

func (s *Store) Packages() []*Package {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Package, 0, len(s.db.Packages))
	for _, p := range s.db.Packages {
		out = append(out, clonePackage(p))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Submitted.After(out[j].Submitted) })
	return out
}

// PutPackage inserts or replaces package metadata, preserving its sequence id.
func (s *Store) PutPackage(p Package) (*Package, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.db.Packages[p.Hash]; ok {
		p.Seq = old.Seq
	} else {
		p.Seq = s.db.NextSeq
		s.db.NextSeq++
	}
	s.db.Packages[p.Hash] = &p
	return clonePackage(&p), s.save()
}

func (s *Store) UpdatePackage(hash string, fn func(p *Package)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.db.Packages[hash]
	if !ok {
		return ErrNotFound
	}
	fn(p)
	return s.save()
}

func (s *Store) DeletePackage(hash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.db.Packages[hash]; !ok {
		return ErrNotFound
	}
	delete(s.db.Packages, hash)
	return s.save()
}
