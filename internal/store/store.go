// Package store persists users, issued certificates and data package metadata
// in a single JSON file. It is intended for edge deployments with tens to low
// thousands of records, where avoiding an external database matters more than
// write throughput.
package store

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
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

	// Directory users (LDAP / Active Directory) sign in with their directory
	// password and get their groups from it; they have no local password.
	Source       string `json:"source,omitempty"` // "" (local) or "ldap"
	DirectoryDN  string `json:"directory_dn,omitempty"`
	DirectoryOff bool   `json:"directory_missing,omitempty"` // not found at the last sync

	// Disabled users can't sign in, use their certificates or stay
	// connected; everything else about them is kept for re-enabling.
	Disabled   bool      `json:"disabled,omitempty"`
	DisabledBy string    `json:"disabled_by,omitempty"`
	DisabledAt time.Time `json:"disabled_at,omitzero"`

	// Event is the event this temporary account belongs to; it is erased
	// when the event ends.
	Event string `json:"event,omitempty"`

	// Two-factor login for the admin console (authenticator app codes).
	TOTPSecret   string   `json:"totp_secret,omitempty"`
	TOTPPending  string   `json:"totp_pending,omitempty"` // being set up, not yet confirmed
	TOTPLastStep int64    `json:"totp_last_step,omitempty"`
	RecoveryHash []string `json:"recovery_hashes,omitempty"` // SHA-256 of unused recovery codes
}

// TwoFactor reports whether the user signs in with an authenticator code.
func (u *User) TwoFactor() bool { return u.TOTPSecret != "" }

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

	VideoFeeds    map[string]*VideoFeed `json:"video_feeds"` // by uid
	VideoSecret   string                `json:"video_secret"`
	VideoSettings VideoSettings         `json:"video_settings"`

	Retention RetentionSettings `json:"retention"`

	CertWarnDays int `json:"cert_warn_days"` // flag certificates this long before they expire (0 = default)
	// CertValidityDays overrides the config's cert_validity_days for client
	// certificates issued from now on (0 = use the config).
	CertValidityDays int `json:"cert_validity_days,omitempty"`

	ChatLog ChatLogSettings `json:"chat_log"`

	VideoRecording RecordingSettings `json:"video_recording"`

	Emergencies []*Emergency `json:"emergencies"` // oldest first, the last maxEmergencies

	DeviceProfiles map[string]*DeviceProfile `json:"device_profiles"` // by id

	Federation Federation `json:"federation"`

	Apps        map[string]*App      `json:"apps"` // ATAK plugins and apps for the update server, by SHA-256
	AppSettings UpdateServerSettings `json:"app_settings"`

	Backups BackupSettings `json:"backups"`

	OfflineMaps map[string]OfflineMap `json:"offline_maps"` // by file name in the maps folder

	LostContact LostContactSettings `json:"lost_contact"`

	Geofences map[string]*Geofence `json:"geofences"` // by id

	Drawings map[string]*Drawing `json:"drawings"` // by id

	LDAP LDAPSettings `json:"ldap"`

	Events map[string]*Event `json:"events"` // by id

	TakGov TakGovLink `json:"takgov"`

	Overlays map[string]*Overlay `json:"overlays"` // by id
}

// RetentionSettings are how long kinds of data are kept, in days (0 = keep
// forever). Position history has its own HistorySettings.
type RetentionSettings struct {
	PackagesDays     int `json:"packages_days"`      // data packages, by upload time (files missions use are kept)
	MissionsIdleDays int `json:"missions_idle_days"` // missions with no changes for this long
	InboxDays        int `json:"inbox_days"`         // messages to the server contact
	RequestsDays     int `json:"requests_days"`      // pending sign-up requests (auto-rejected)
	InvitesDays      int `json:"invites_days"`       // used-up, expired or revoked invites
	CertsDays        int `json:"certs_days"`         // records of certificates expired this long ago
	ChatDays         int `json:"chat_days"`          // chat log messages
}

// RecordingSettings controls video recording: Mode "off" (default), "feeds"
// (saved feeds marked Record) or "all" (every stream). KeepDays is how long
// recordings are kept (0 = forever).
type RecordingSettings struct {
	Mode     string `json:"mode"`
	KeepDays int    `json:"keep_days"`
}

// DefaultRecordingDays is how long recordings are kept unless set.
const DefaultRecordingDays = 7

// ChatLogSettings controls recording of GeoChat messages (off by default).
type ChatLogSettings struct {
	Enabled bool `json:"enabled"`
}

// VideoSettings controls publishing to the server's media server without
// a login (for apps such as TAK ICU that cannot send one): "off"
// (default), "feeds" (only to feeds with OpenPublish) or "any" (any stream
// name; such streams are visible to AnonGroups).
type VideoSettings struct {
	AnonPublish string   `json:"anon_publish"`
	AnonGroups  []string `json:"anon_groups"`
}

// VideoFeed is a registered video stream. Path names a stream on the
// server's MediaMTX (its URL is built per viewer); otherwise URL points at
// an external source.
type VideoFeed struct {
	UID    string   `json:"uid"`
	Alias  string   `json:"alias"`
	URL    string   `json:"url,omitempty"`
	Path   string   `json:"path,omitempty"`
	Groups []string `json:"groups"`
	Lat    *float64 `json:"lat,omitempty"`
	Lon    *float64 `json:"lon,omitempty"`
	// OpenPublish lets devices publish to Path without a login (when the
	// server's AnonPublish is "feeds" or "any").
	OpenPublish bool `json:"open_publish,omitempty"`
	// Record saves the stream to disk when recording is set to saved feeds.
	Record    bool      `json:"record,omitempty"`
	Created   time.Time `json:"created"`
	CreatedBy string    `json:"created_by"`
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
	// Devices: also sent to ATAK as a map source, to the devices in Groups
	// (none = every device). Updated is when it last changed.
	Devices bool      `json:"devices,omitempty"`
	Groups  []string  `json:"groups,omitempty"`
	Updated time.Time `json:"updated,omitzero"`
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
	Event     string    `json:"event,omitempty"` // accounts it creates belong to this event
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
		Invites: map[string]*Invite{}, Requests: map[string]*SignupRequest{}, VideoFeeds: map[string]*VideoFeed{}, DeviceProfiles: map[string]*DeviceProfile{}, Apps: map[string]*App{}, Geofences: map[string]*Geofence{}, Drawings: map[string]*Drawing{}, Overlays: map[string]*Overlay{}, Events: map[string]*Event{}, NextSeq: 1}}
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
	if s.db.VideoFeeds == nil {
		s.db.VideoFeeds = map[string]*VideoFeed{}
	}
	if s.db.DeviceProfiles == nil {
		s.db.DeviceProfiles = map[string]*DeviceProfile{}
	}
	if s.db.Apps == nil {
		s.db.Apps = map[string]*App{}
	}
	if s.db.Geofences == nil {
		s.db.Geofences = map[string]*Geofence{}
	}
	if s.db.Overlays == nil {
		s.db.Overlays = map[string]*Overlay{}
	}
	if s.db.Drawings == nil {
		s.db.Drawings = map[string]*Drawing{}
	}
	if s.db.Events == nil {
		s.db.Events = map[string]*Event{}
	}
	// Groups used to exist only as names on users; give them records.
	for _, u := range s.db.Users {
		s.ensureGroupsLocked(append(append(slices.Clone(u.Groups), u.GroupsIn...), u.GroupsOut...))
	}
	return s, nil
}

// save writes the database atomically. Callers hold s.mu.
// Snapshot returns the database as it is saved, for backups.
func (s *Store) Snapshot() ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return json.MarshalIndent(&s.db, "", " ")
}

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
	c.RecoveryHash = slices.Clone(u.RecoveryHash)
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

// UpdateInvite changes an invite.
func (s *Store) UpdateInvite(code string, fn func(i *Invite)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	inv, ok := s.db.Invites[code]
	if !ok {
		return ErrNotFound
	}
	fn(inv)
	return s.save()
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
	u := &User{Username: username, PasswordHash: passwordHash, Groups: slices.Clone(inv.Groups), Created: time.Now().UTC(), Event: inv.Event}
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

// ---- data retention ----

func (s *Store) RetentionSettings() RetentionSettings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.db.Retention
}

func (s *Store) SetRetentionSettings(r RetentionSettings) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.db.Retention = r
	return s.save()
}

func (s *Store) RecordingSettings() RecordingSettings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r := s.db.VideoRecording
	if r.Mode == "" {
		r.Mode, r.KeepDays = "off", DefaultRecordingDays
	}
	return r
}

func (s *Store) SetRecordingSettings(r RecordingSettings) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.db.VideoRecording = r
	return s.save()
}

func (s *Store) ChatLogSettings() ChatLogSettings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.db.ChatLog
}

func (s *Store) SetChatLogSettings(c ChatLogSettings) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.db.ChatLog = c
	return s.save()
}

// DefaultCertWarnDays is how long before expiry certificates are flagged
// unless an admin sets otherwise.
const DefaultCertWarnDays = 30

// CertWarnDays is how many days before expiry a certificate is flagged.
func (s *Store) CertWarnDays() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db.CertWarnDays <= 0 {
		return DefaultCertWarnDays
	}
	return s.db.CertWarnDays
}

// CertValidityDays is the admin's client certificate lifetime (0 = not set).
func (s *Store) CertValidityDays() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.db.CertValidityDays
}

func (s *Store) SetCertValidityDays(n int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.db.CertValidityDays = n
	return s.save()
}

func (s *Store) SetCertWarnDays(n int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.db.CertWarnDays = n
	return s.save()
}

// PruneInbox removes server-contact messages older than before; with dry
// it only counts them.
func (s *Store) PruneInbox(before time.Time, dry bool) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	keep := s.db.Inbox[:0:0]
	n := 0
	for _, m := range s.db.Inbox {
		if m.Time.Before(before) {
			n++
		} else {
			keep = append(keep, m)
		}
	}
	if dry || n == 0 {
		return n, nil
	}
	s.db.Inbox = keep
	return n, s.save()
}

// PruneRequests rejects sign-up requests waiting since before.
func (s *Store) PruneRequests(before time.Time, dry bool) (int, error) {
	return s.pruneMap(dry, func() int {
		n := 0
		for id, r := range s.db.Requests {
			if r.Created.Before(before) {
				n++
				if !dry {
					delete(s.db.Requests, id)
				}
			}
		}
		return n
	})
}

// PruneInvites removes invites that can no longer be used and ended before
// before: by their expiry, or by when they were made if used up or revoked.
func (s *Store) PruneInvites(before, now time.Time, dry bool) (int, error) {
	return s.pruneMap(dry, func() int {
		n := 0
		for code, inv := range s.db.Invites {
			if inv.Usable(now) {
				continue
			}
			ended := inv.Created
			if !inv.Expires.IsZero() && inv.Expires.Before(now) {
				ended = inv.Expires
			}
			if ended.Before(before) {
				n++
				if !dry {
					delete(s.db.Invites, code)
				}
			}
		}
		return n
	})
}

// PruneCerts removes records of certificates that expired before before.
// Unexpired ones always stay, revoked or not: the record is what keeps a
// revoked certificate from working.
func (s *Store) PruneCerts(before time.Time, dry bool) (int, error) {
	return s.pruneMap(dry, func() int {
		n := 0
		for serial, c := range s.db.Certs {
			if !c.Expires.IsZero() && c.Expires.Before(before) {
				n++
				if !dry {
					delete(s.db.Certs, serial)
				}
			}
		}
		return n
	})
}

func (s *Store) pruneMap(dry bool, fn func() int) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := fn()
	if dry || n == 0 {
		return n, nil
	}
	return n, s.save()
}

// ---- two-factor login ----

// SetTOTPPending stores a secret being set up; it takes effect with EnableTOTP.
func (s *Store) SetTOTPPending(name, secret string) error {
	_, err := s.UpdateUser(name, func(u *User) error { u.TOTPPending = secret; return nil })
	return err
}

// EnableTOTP turns on two-factor login with the pending secret if code
// matches it, and stores the recovery code hashes.
func (s *Store) EnableTOTP(name, code string, recoveryHashes []string, verify func(secret, code string, lastStep int64) (int64, bool)) error {
	_, err := s.UpdateUser(name, func(u *User) error {
		if u.TOTPPending == "" {
			return errors.New("start the setup again")
		}
		step, ok := verify(u.TOTPPending, code, 0)
		if !ok {
			return errors.New("that code is not right; check the time on your phone and try the next code")
		}
		u.TOTPSecret, u.TOTPPending, u.TOTPLastStep = u.TOTPPending, "", step
		u.RecoveryHash = slices.Clone(recoveryHashes)
		return nil
	})
	return err
}

// DisableTOTP turns two-factor login off.
func (s *Store) DisableTOTP(name string) error {
	_, err := s.UpdateUser(name, func(u *User) error {
		u.TOTPSecret, u.TOTPPending, u.TOTPLastStep, u.RecoveryHash = "", "", 0, nil
		return nil
	})
	return err
}

// SetRecoveryCodes replaces the user's recovery code hashes.
func (s *Store) SetRecoveryCodes(name string, hashes []string) error {
	_, err := s.UpdateUser(name, func(u *User) error { u.RecoveryHash = slices.Clone(hashes); return nil })
	return err
}

// CheckSecondFactor accepts an authenticator code (each only once) or an
// unused recovery code (which is then used up). It reports which one matched.
func (s *Store) CheckSecondFactor(name, code string, verify func(secret, code string, lastStep int64) (int64, bool), hash func(string) string) (ok, recovery bool) {
	s.UpdateUser(name, func(u *User) error {
		if u.TOTPSecret == "" {
			return nil
		}
		if step, good := verify(u.TOTPSecret, code, u.TOTPLastStep); good {
			u.TOTPLastStep, ok = step, true
			return nil
		}
		h := hash(code)
		if i := slices.Index(u.RecoveryHash, h); i >= 0 {
			u.RecoveryHash = slices.Delete(u.RecoveryHash, i, i+1)
			ok, recovery = true, true
			return nil
		}
		return errors.New("no match") // nothing to save
	})
	return ok, recovery
}

// VideoFeeds returns copies of the registered feeds, sorted by alias.
func (s *Store) VideoFeeds() []VideoFeed {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]VideoFeed, 0, len(s.db.VideoFeeds))
	for _, f := range s.db.VideoFeeds {
		c := *f
		c.Groups = slices.Clone(f.Groups)
		out = append(out, c)
	}
	slices.SortFunc(out, func(a, b VideoFeed) int { return strings.Compare(strings.ToLower(a.Alias), strings.ToLower(b.Alias)) })
	return out
}

func (s *Store) VideoFeed(uid string) (VideoFeed, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	f, ok := s.db.VideoFeeds[uid]
	if !ok {
		return VideoFeed{}, false
	}
	c := *f
	c.Groups = slices.Clone(f.Groups)
	return c, true
}

// PutVideoFeed adds or replaces a feed (by UID).
func (s *Store) PutVideoFeed(f VideoFeed) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.db.VideoFeeds) >= 1000 {
		if _, exists := s.db.VideoFeeds[f.UID]; !exists {
			return errors.New("too many video feeds")
		}
	}
	f.Groups = slices.Clone(f.Groups)
	s.db.VideoFeeds[f.UID] = &f
	return s.save()
}

func (s *Store) DeleteVideoFeed(uid string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.db.VideoFeeds[uid]; !ok {
		return ErrNotFound
	}
	delete(s.db.VideoFeeds, uid)
	return s.save()
}

func (s *Store) VideoSettings() VideoSettings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v := s.db.VideoSettings
	if v.AnonPublish == "" {
		v.AnonPublish = "off"
	}
	v.AnonGroups = slices.Clone(v.AnonGroups)
	return v
}

func (s *Store) SetVideoSettings(v VideoSettings) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	v.AnonGroups = slices.Clone(v.AnonGroups)
	s.db.VideoSettings = v
	return s.save()
}

// VideoSecret returns the key for video access tokens, creating it once.
func (s *Store) VideoSecret() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db.VideoSecret == "" {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return "", err
		}
		s.db.VideoSecret = hex.EncodeToString(b)
		if err := s.save(); err != nil {
			return "", err
		}
	}
	return s.db.VideoSecret, nil
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
		for _, p := range s.db.DeviceProfiles {
			if i := slices.Index(p.Groups, name); i >= 0 {
				p.Groups[i] = newName
			}
		}
	}
	return affected, s.save()
}

// DeleteGroup removes a group and every membership in it. It returns the
// users whose memberships changed. Device profiles keep the name: an empty
// group list means everyone, so dropping it would widen who gets them.
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

// ---- emergency alerts ----

// maxEmergencies is how many alerts the log keeps.
const maxEmergencies = 500

// Emergency is an alert raised with a TAK client's emergency button (911,
// Ring the Bell, In Contact, Geo-fence Breached, custom). It is active until
// the device cancels it or an admin dismisses it.
type Emergency struct {
	ID        string     `json:"id"`
	Kind      string     `json:"kind,omitempty"` // "" for ATAK's emergency button, "lost" for lost contact
	EventUID  string     `json:"event_uid"`      // the alert's CoT UID (<device uid>-9-1-1)
	DeviceUID string     `json:"device_uid"`
	Callsign  string     `json:"callsign"`
	Username  string     `json:"username,omitempty"`
	Type      string     `json:"type"`     // as ATAK names it, e.g. "911 Alert"
	CotType   string     `json:"cot_type"` // e.g. b-a-o-tbl
	Lat       float64    `json:"lat"`
	Lon       float64    `json:"lon"`
	Started   time.Time  `json:"started"`
	Updated   time.Time  `json:"updated"`
	Acked     *time.Time `json:"acked,omitempty"`
	AckedBy   string     `json:"acked_by,omitempty"`
	Ended     *time.Time `json:"ended,omitempty"`
	EndedBy   string     `json:"ended_by,omitempty"` // "device" (cancelled, or back in contact) or the admin who dismissed it
}

func (e *Emergency) Active() bool { return e.Ended == nil }

func cloneEmergency(e *Emergency) Emergency {
	c := *e
	if e.Acked != nil {
		t := *e.Acked
		c.Acked = &t
	}
	if e.Ended != nil {
		t := *e.Ended
		c.Ended = &t
	}
	return c
}

// Emergencies returns the alert log, newest first.
func (s *Store) Emergencies() []Emergency {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Emergency, 0, len(s.db.Emergencies))
	for i := len(s.db.Emergencies) - 1; i >= 0; i-- {
		out = append(out, cloneEmergency(s.db.Emergencies[i]))
	}
	return out
}

func (s *Store) AddEmergency(e Emergency) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.db.Emergencies = append(s.db.Emergencies, &e)
	if n := len(s.db.Emergencies); n > maxEmergencies {
		s.db.Emergencies = slices.Delete(s.db.Emergencies, 0, n-maxEmergencies)
	}
	return s.save()
}

// UpdateEmergency changes the alert with id.
func (s *Store) UpdateEmergency(id string, fn func(e *Emergency)) (Emergency, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.db.Emergencies {
		if e.ID == id {
			fn(e)
			return cloneEmergency(e), s.save()
		}
	}
	return Emergency{}, ErrNotFound
}

// ---- device profiles ----

// DeviceProfile is a set of settings and files sent to the TAK clients of
// users in its groups (everyone when Groups is empty): when a device enrolls,
// when it connects, or on demand. Files live in <data>/profiles/<id>/.
type DeviceProfile struct {
	ID           string        `json:"id"`
	Name         string        `json:"name"`
	Groups       []string      `json:"groups"`
	Active       bool          `json:"active"`
	OnEnrollment bool          `json:"on_enrollment"`
	OnConnect    bool          `json:"on_connect"`
	Prefs        []ProfilePref `json:"prefs"`
	Files        []ProfileFile `json:"files"`
	Created      time.Time     `json:"created"`
	CreatedBy    string        `json:"created_by"`
	Updated      time.Time     `json:"updated"` // devices that synced before this get it again
}

// ProfilePref is one ATAK preference; Type is string, boolean, integer,
// long or float.
type ProfilePref struct {
	Key   string `json:"key"`
	Value string `json:"value"`
	Type  string `json:"type"`
}

// ProfileFile is a file in a profile: a .pref file, an ATAK plugin APK, a
// map source XML, an overlay, or anything else ATAK imports.
type ProfileFile struct {
	Name   string    `json:"name"`
	Size   int64     `json:"size"`
	SHA256 string    `json:"sha256"`
	Added  time.Time `json:"added"`
}

func cloneProfile(p *DeviceProfile) DeviceProfile {
	c := *p
	c.Groups = slices.Clone(p.Groups)
	c.Prefs = slices.Clone(p.Prefs)
	c.Files = slices.Clone(p.Files)
	return c
}

// DeviceProfiles returns the profiles sorted by name.
func (s *Store) DeviceProfiles() []DeviceProfile {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]DeviceProfile, 0, len(s.db.DeviceProfiles))
	for _, p := range s.db.DeviceProfiles {
		out = append(out, cloneProfile(p))
	}
	slices.SortFunc(out, func(a, b DeviceProfile) int { return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)) })
	return out
}

func (s *Store) DeviceProfile(id string) (DeviceProfile, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.db.DeviceProfiles[id]
	if !ok {
		return DeviceProfile{}, false
	}
	return cloneProfile(p), true
}

// PutDeviceProfile adds or replaces a profile.
func (s *Store) PutDeviceProfile(p DeviceProfile) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := cloneProfile(&p)
	s.db.DeviceProfiles[p.ID] = &c
	return s.save()
}

// UpdateDeviceProfile changes a profile and marks it updated.
func (s *Store) UpdateDeviceProfile(id string, fn func(p *DeviceProfile) error) (DeviceProfile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.db.DeviceProfiles[id]
	if !ok {
		return DeviceProfile{}, ErrNotFound
	}
	c := cloneProfile(p)
	if err := fn(&c); err != nil {
		return DeviceProfile{}, err
	}
	c.ID, c.Updated = id, time.Now().UTC()
	s.db.DeviceProfiles[id] = &c
	return cloneProfile(&c), s.save()
}

func (s *Store) DeleteDeviceProfile(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.db.DeviceProfiles[id]; !ok {
		return ErrNotFound
	}
	delete(s.db.DeviceProfiles, id)
	return s.save()
}

// ---- ATAK plugins and apps (update server) ----

// App is an APK offered to devices through ATAK's update server. One build
// is kept per package and ATAK plugin API, so plugins for different ATAK
// versions can be offered side by side.
type App struct {
	SHA256      string    `json:"sha256"` // of the APK, also its file name and ID
	Package     string    `json:"package"`
	Name        string    `json:"name"`
	Version     string    `json:"version"`
	VersionCode int64     `json:"version_code"`
	MinSDK      int       `json:"min_sdk"`
	PluginAPI   string    `json:"plugin_api,omitempty"` // e.g. com.atakmap.app@5.4.0.CIV; empty for apps
	Description string    `json:"description"`
	Size        int64     `json:"size"`
	HasIcon     bool      `json:"has_icon"`
	FileName    string    `json:"file_name"` // as uploaded
	Uploader    string    `json:"uploader"`
	Uploaded    time.Time `json:"uploaded"`
}

// UpdateServerSettings: whether devices are pointed at EdgeTAK's update
// server through their device profile (on by default).
type UpdateServerSettings struct {
	DontConfigureDevices bool `json:"dont_configure_devices,omitempty"`
}

// Apps returns the APKs, by name.
func (s *Store) Apps() []App {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]App, 0, len(s.db.Apps))
	for _, a := range s.db.Apps {
		out = append(out, *a)
	}
	slices.SortFunc(out, func(a, b App) int {
		if c := strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)); c != 0 {
			return c
		}
		return strings.Compare(b.PluginAPI, a.PluginAPI)
	})
	return out
}

// PutApp adds an APK, replacing (and returning) the builds of the same
// package for the same plugin API.
func (s *Store) PutApp(a App) (replaced []App, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, old := range s.db.Apps {
		if id != a.SHA256 && old.Package == a.Package && old.PluginAPI == a.PluginAPI {
			replaced = append(replaced, *old)
			delete(s.db.Apps, id)
		}
	}
	s.db.Apps[a.SHA256] = &a
	return replaced, s.save()
}

// SetAppIcon records whether an APK has an icon file.
func (s *Store) SetAppIcon(id string, has bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.db.Apps[id]
	if !ok {
		return ErrNotFound
	}
	a.HasIcon = has
	return s.save()
}

func (s *Store) DeleteApp(id string) (App, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.db.Apps[id]
	if !ok {
		return App{}, ErrNotFound
	}
	delete(s.db.Apps, id)
	return *a, s.save()
}

func (s *Store) UpdateServer() UpdateServerSettings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.db.AppSettings
}

func (s *Store) SetUpdateServer(v UpdateServerSettings) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.db.AppSettings = v
	return s.save()
}

// ---- lost contact ----

// LostContactSettings: when a device that did not sign off goes quiet for
// GraceMinutes, or stays connected without a position for
// NoPositionMinutes, an alert is raised. Groups limits it to devices in
// those groups (none = every device).
type LostContactSettings struct {
	Off               bool     `json:"off,omitempty"`
	GraceMinutes      int      `json:"grace_minutes"`       // 0 = 5
	NoPositionMinutes int      `json:"no_position_minutes"` // 0 = 10; -1 = off
	Groups            []string `json:"groups"`
}

func (s *Store) LostContact() LostContactSettings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	l := s.db.LostContact
	l.Groups = slices.Clone(l.Groups)
	if l.GraceMinutes <= 0 {
		l.GraceMinutes = 5
	}
	if l.NoPositionMinutes == 0 {
		l.NoPositionMinutes = 10
	}
	if l.Groups == nil {
		l.Groups = []string{}
	}
	return l
}

func (s *Store) SetLostContact(l LostContactSettings) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.db.LostContact = l
	return s.save()
}

// ---- geofences ----

// Geofence is an area drawn on the console map: a polygon (Points, as lat,
// lon pairs) or a circle (Center and Radius in meters). Devices in Groups
// (none = every device) raise an alert when they enter or leave it, per
// Trigger: "enter", "exit" or "both".
type Geofence struct {
	ID      string       `json:"id"`
	Name    string       `json:"name"`
	Shape   string       `json:"shape"` // "polygon" or "circle"
	Points  [][2]float64 `json:"points,omitempty"`
	Center  [2]float64   `json:"center"`
	Radius  float64      `json:"radius,omitempty"`
	Trigger string       `json:"trigger"`
	Groups  []string     `json:"groups"`
	Color   string       `json:"color"`         // #RRGGBB
	Devices bool         `json:"devices"`       // also drawn on devices' maps
	Off     bool         `json:"off,omitempty"` // no alerts
	Created time.Time    `json:"created"`
	Updated time.Time    `json:"updated"`
}

func cloneGeofence(g *Geofence) Geofence {
	c := *g
	c.Points = slices.Clone(g.Points)
	c.Groups = slices.Clone(g.Groups)
	return c
}

// Geofences returns the geofences by name.
func (s *Store) Geofences() []Geofence {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Geofence, 0, len(s.db.Geofences))
	for _, g := range s.db.Geofences {
		out = append(out, cloneGeofence(g))
	}
	slices.SortFunc(out, func(a, b Geofence) int { return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)) })
	return out
}

func (s *Store) PutGeofence(g Geofence) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := cloneGeofence(&g)
	s.db.Geofences[g.ID] = &c
	return s.save()
}

func (s *Store) DeleteGeofence(id string) (Geofence, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.db.Geofences[id]
	if !ok {
		return Geofence{}, ErrNotFound
	}
	delete(s.db.Geofences, id)
	return cloneGeofence(g), s.save()
}

// ---- directory sign-in (LDAP / Active Directory) ----

// LDAPSettings: users without a local account sign in against a directory.
// The user is found with UserFilter ({username} replaced) under BaseDN,
// using the BindDN account (or anonymously), then its own password is
// checked by binding as it. Its groups are its memberOf values and, when
// GroupBaseDN is set, the groups found there listing it as a member; their
// common names become EdgeTAK groups (only those in Groups, when set).
type LDAPSettings struct {
	Enabled       bool     `json:"enabled"`
	URL           string   `json:"url"` // ldap://host:389 or ldaps://host:636
	StartTLS      bool     `json:"start_tls,omitempty"`
	SkipVerify    bool     `json:"skip_verify,omitempty"` // don't check the server's certificate
	CAPEM         string   `json:"ca_pem,omitempty"`      // the directory's CA, if not a public one
	BindDN        string   `json:"bind_dn"`
	BindPassword  string   `json:"bind_password,omitempty"` // secret
	BaseDN        string   `json:"base_dn"`
	UserFilter    string   `json:"user_filter"`
	GroupBaseDN   string   `json:"group_base_dn,omitempty"`
	Groups        []string `json:"groups"`                   // directory groups used (none = all)
	AdminGroup    string   `json:"admin_group,omitempty"`    // members are EdgeTAK admins
	RequiredGroup string   `json:"required_group,omitempty"` // only members may sign in
}

func (s *Store) LDAP() LDAPSettings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	l := s.db.LDAP
	l.Groups = slices.Clone(l.Groups)
	if l.Groups == nil {
		l.Groups = []string{}
	}
	return l
}

func (s *Store) SetLDAP(l LDAPSettings) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.db.LDAP = l
	return s.save()
}

// UpsertDirectoryUser creates or updates a directory user after a
// successful directory sign-in or sync: its DN, groups and, when admin is
// non-nil, whether it is an admin.
func (s *Store) UpsertDirectoryUser(name, dn string, groups []string, admin *bool) (*User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.db.Users[name]
	if ok && u.Source != "ldap" {
		return nil, ErrExists // a local account with that name
	}
	if !ok {
		u = &User{Username: name, Source: "ldap", Created: time.Now().UTC()}
		s.db.Users[name] = u
	}
	u.DirectoryDN, u.DirectoryOff = dn, false
	u.Groups = slices.Clone(groups)
	if admin != nil {
		u.Admin = *admin
	}
	s.ensureGroupsLocked(groups)
	return cloneUser(u), s.save()
}

// ---- tak.gov ----

// TakGovLink is the tak.gov account EdgeTAK is linked to, for downloading
// plugins: its refresh token (secret; tak.gov replaces it at each use).
type TakGovLink struct {
	RefreshToken string    `json:"refresh_token,omitempty"`
	Account      string    `json:"account,omitempty"` // the account's name or email, for display
	Linked       time.Time `json:"linked,omitzero"`
	LinkedBy     string    `json:"linked_by,omitempty"`
	Product      string    `json:"product,omitempty"`         // last browsed, e.g. ATAK-CIV
	Version      string    `json:"product_version,omitempty"` // e.g. 5.6.0
}

func (s *Store) TakGov() TakGovLink {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.db.TakGov
}

func (s *Store) UpdateTakGov(fn func(t *TakGovLink)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.db.TakGov)
	return s.save()
}

// ---- events ----

// Event is a temporary operation: its own group and Data Sync mission, and
// temporary accounts that are erased when it expires. People join through
// a sign-up invite (each gets an account) or a shared account everyone
// enrolls with, per Mode.
type Event struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Mode      string    `json:"mode"` // "person" or "shared"
	Group     string    `json:"group"`
	Mission   string    `json:"mission,omitempty"`
	Invite    string    `json:"invite,omitempty"`     // person mode: the invite code
	Account   string    `json:"account,omitempty"`    // shared mode: the shared username
	Password  string    `json:"password,omitempty"`   // shared mode: its password (for the QR code)
	MaxPeople int       `json:"max_people,omitempty"` // person mode: 0 = no limit
	Expires   time.Time `json:"expires"`
	Created   time.Time `json:"created"`
	CreatedBy string    `json:"created_by"`
	Ended     time.Time `json:"ended,omitzero"`
	EndedBy   string    `json:"ended_by,omitempty"` // "expired" or the admin who ended it
	Removed   int       `json:"removed,omitempty"`  // accounts erased when it ended
}

func (s *Store) Events() []Event {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Event, 0, len(s.db.Events))
	for _, e := range s.db.Events {
		out = append(out, *e)
	}
	slices.SortFunc(out, func(a, b Event) int { return b.Created.Compare(a.Created) })
	return out
}

func (s *Store) Event(id string) (Event, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.db.Events[id]
	if !ok {
		return Event{}, false
	}
	return *e, true
}

func (s *Store) PutEvent(e Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.db.Events[e.ID] = &e
	return s.save()
}

func (s *Store) DeleteEvent(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.db.Events[id]; !ok {
		return ErrNotFound
	}
	delete(s.db.Events, id)
	return s.save()
}

// ---- drawings ----

// Drawing is a marker, route, area or circle drawn on the console map and
// kept on devices' maps: sent to the devices in Groups (none = every device)
// and again to each as it connects.
type Drawing struct {
	ID      string       `json:"id"`
	Kind    string       `json:"kind"` // "marker", "route", "area" or "circle"
	Name    string       `json:"name"`
	Remarks string       `json:"remarks"`
	Type    string       `json:"type,omitempty"` // a marker's CoT type, e.g. a-h-G or b-m-p-s-m
	Points  [][2]float64 `json:"points,omitempty"`
	Center  [2]float64   `json:"center"` // a marker's or circle's position
	Radius  float64      `json:"radius,omitempty"`
	Color   string       `json:"color"` // #RRGGBB
	Groups  []string     `json:"groups"`
	Author  string       `json:"author"`
	Created time.Time    `json:"created"`
	Updated time.Time    `json:"updated"`
}

func cloneDrawing(d *Drawing) Drawing {
	c := *d
	c.Points = slices.Clone(d.Points)
	c.Groups = slices.Clone(d.Groups)
	return c
}

// Drawings returns the drawings, newest first.
func (s *Store) Drawings() []Drawing {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Drawing, 0, len(s.db.Drawings))
	for _, d := range s.db.Drawings {
		out = append(out, cloneDrawing(d))
	}
	slices.SortFunc(out, func(a, b Drawing) int { return b.Created.Compare(a.Created) })
	return out
}

func (s *Store) PutDrawing(d Drawing) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := cloneDrawing(&d)
	s.db.Drawings[d.ID] = &c
	return s.save()
}

func (s *Store) DeleteDrawing(id string) (Drawing, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.db.Drawings[id]
	if !ok {
		return Drawing{}, ErrNotFound
	}
	delete(s.db.Drawings, id)
	return cloneDrawing(d), s.save()
}

// ---- overlays ----

// Overlay is a KML, KMZ or GeoJSON file sent to the devices in Groups (none
// = every device) as a data package ATAK imports as a map overlay: when it
// is added or replaced, and to each device as it connects until it has the
// current version.
type Overlay struct {
	ID       string     `json:"id"`
	Name     string     `json:"name"`
	Groups   []string   `json:"groups"`
	Format   string     `json:"format"`    // kml, kmz or geojson (as uploaded)
	FileName string     `json:"file_name"` // as uploaded
	Hash     string     `json:"hash"`      // sha256 of the data package
	Size     int64      `json:"size"`      // of the data package
	Items    int        `json:"items"`     // placemarks
	Bounds   [4]float64 `json:"bounds"`    // south, west, north, east
	Version  int        `json:"version"`
	Author   string     `json:"author"`
	Created  time.Time  `json:"created"`
	Updated  time.Time  `json:"updated"`
	Paused   bool       `json:"paused,omitempty"` // not sent to devices
	// Delivered is the version each device (by UID) was last sent.
	Delivered map[string]int `json:"delivered,omitempty"`
}

func cloneOverlay(o *Overlay) Overlay {
	c := *o
	c.Groups = slices.Clone(o.Groups)
	c.Delivered = maps.Clone(o.Delivered)
	return c
}

// Overlays returns the overlays, newest first.
func (s *Store) Overlays() []Overlay {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Overlay, 0, len(s.db.Overlays))
	for _, o := range s.db.Overlays {
		out = append(out, cloneOverlay(o))
	}
	slices.SortFunc(out, func(a, b Overlay) int { return b.Created.Compare(a.Created) })
	return out
}

func (s *Store) Overlay(id string) (Overlay, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	o, ok := s.db.Overlays[id]
	if !ok {
		return Overlay{}, false
	}
	return cloneOverlay(o), true
}

// PutOverlay saves an overlay, keeping the delivery record it has.
func (s *Store) PutOverlay(o Overlay) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := cloneOverlay(&o)
	if old, ok := s.db.Overlays[o.ID]; ok {
		c.Delivered = old.Delivered
	}
	s.db.Overlays[o.ID] = &c
	return s.save()
}

// OverlayDelivered records that a device was sent an overlay's version.
func (s *Store) OverlayDelivered(id, uid string, version int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.db.Overlays[id]
	if !ok {
		return ErrNotFound
	}
	if o.Delivered == nil {
		o.Delivered = map[string]int{}
	}
	o.Delivered[uid] = version
	return s.save()
}

func (s *Store) DeleteOverlay(id string) (Overlay, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.db.Overlays[id]
	if !ok {
		return Overlay{}, ErrNotFound
	}
	delete(s.db.Overlays, id)
	return cloneOverlay(o), s.save()
}

// ---- offline maps ----

// OfflineMap is how an MBTiles map is offered. Maps are sent to devices as
// ATAK map sources unless NoDevices is set.
type OfflineMap struct {
	NoDevices bool      `json:"no_devices,omitempty"`
	Updated   time.Time `json:"updated"`
}

func (s *Store) OfflineMap(file string) OfflineMap {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.db.OfflineMaps[file]
}

func (s *Store) SetOfflineMap(file string, m OfflineMap) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db.OfflineMaps == nil {
		s.db.OfflineMaps = map[string]OfflineMap{}
	}
	m.Updated = time.Now().UTC()
	s.db.OfflineMaps[file] = m
	return s.save()
}

func (s *Store) DeleteOfflineMap(file string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.db.OfflineMaps, file)
	return s.save()
}

// ---- backups ----

// BackupSettings: automatic daily backups into the data directory, keeping
// the newest Keep.
type BackupSettings struct {
	Auto    bool `json:"auto"`
	Keep    int  `json:"keep"`    // 0 = 7
	History bool `json:"history"` // include position history
}

func (s *Store) BackupSettings() BackupSettings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	b := s.db.Backups
	if b.Keep <= 0 {
		b.Keep = 7
	}
	return b
}

func (s *Store) SetBackupSettings(b BackupSettings) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.db.Backups = b
	return s.save()
}

// ---- federation ----

// Federation holds TAK Server Federation v2 settings: whether other servers
// may connect here (inbound), and every federate.
type Federation struct {
	ListenEnabled bool       `json:"listen_enabled"`
	ListenPort    int        `json:"listen_port"` // 0 = 9001
	Federates     []Federate `json:"federates"`
}

// Federate is one server EdgeTAK federates with.
type Federate struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Enabled   bool   `json:"enabled"`
	Direction string `json:"direction"` // "outbound" (EdgeTAK connects) or "inbound" (it connects here)

	// Outbound
	Address         string `json:"address,omitempty"`
	Port            int    `json:"port,omitempty"`
	TLSServerName   string `json:"tls_server_name,omitempty"`
	AuthMode        string `json:"auth_mode,omitempty"` // "certificate" (port 9001) or "token" (9002)
	Token           string `json:"token,omitempty"`     // secret
	TokenExpiration int64  `json:"token_expiration,omitempty"`
	UseTLS          bool   `json:"use_tls"`

	// CAPEM is the CA certificate(s) that signed the other server's
	// certificate: it is trusted for this federate only.
	CAPEM string `json:"ca_pem,omitempty"`

	// Inbound: the other server's client certificate must have this SHA-256
	// fingerprint, or be issued by CAPEM with this common name.
	Fingerprint string `json:"fingerprint,omitempty"`
	ClientCN    string `json:"client_cn,omitempty"`

	// What is shared: SendMode "all" (everything devices send) or "groups"
	// (only what is sent to OutboundGroups). Received events go to
	// InboundGroups (none = every device), plus groups named like the
	// remote federate groups when MapRemoteGroups is set.
	SendMode        string   `json:"send_mode"`
	OutboundGroups  []string `json:"outbound_groups"`
	InboundGroups   []string `json:"inbound_groups"`
	MapRemoteGroups bool     `json:"map_remote_groups,omitempty"`
	// NoPackages keeps data packages from crossing (on by default).
	NoPackages bool `json:"no_packages,omitempty"`

	Created time.Time `json:"created"`
}

func cloneFederate(f Federate) Federate {
	f.OutboundGroups = slices.Clone(f.OutboundGroups)
	f.InboundGroups = slices.Clone(f.InboundGroups)
	return f
}

// FederationSettings returns the federation settings and federates.
func (s *Store) FederationSettings() Federation {
	s.mu.RLock()
	defer s.mu.RUnlock()
	f := s.db.Federation
	f.Federates = make([]Federate, len(s.db.Federation.Federates))
	for i, x := range s.db.Federation.Federates {
		f.Federates[i] = cloneFederate(x)
	}
	if f.ListenPort == 0 {
		f.ListenPort = 9001
	}
	return f
}

// SetFederationListener changes whether other servers may connect here.
func (s *Store) SetFederationListener(enabled bool, port int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.db.Federation.ListenEnabled, s.db.Federation.ListenPort = enabled, port
	return s.save()
}

// PutFederate adds or replaces a federate (by ID).
func (s *Store) PutFederate(f Federate) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	f = cloneFederate(f)
	for i := range s.db.Federation.Federates {
		if s.db.Federation.Federates[i].ID == f.ID {
			s.db.Federation.Federates[i] = f
			return s.save()
		}
	}
	s.db.Federation.Federates = append(s.db.Federation.Federates, f)
	return s.save()
}

func (s *Store) DeleteFederate(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fs := s.db.Federation.Federates
	i := slices.IndexFunc(fs, func(f Federate) bool { return f.ID == id })
	if i < 0 {
		return ErrNotFound
	}
	s.db.Federation.Federates = slices.Delete(fs, i, i+1)
	return s.save()
}
