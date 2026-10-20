// Package mission implements TAK Data Sync missions: named, group-scoped
// collections of map items (CoT UIDs) and files that clients subscribe to and
// receive live change notifications for. It mirrors the official server's
// /Marti/api/missions model closely enough for ATAK, WinTAK and iTAK Data Sync.
package mission

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/brandongotak/EdgeTAK/internal/router"
	"github.com/brandongotak/EdgeTAK/internal/store"
)

// Role names and their permissions, as in the official server.
const (
	RoleOwner      = "MISSION_OWNER"
	RoleSubscriber = "MISSION_SUBSCRIBER"
	RoleReadOnly   = "MISSION_READONLY_SUBSCRIBER"

	PermRead        = "MISSION_READ"
	PermWrite       = "MISSION_WRITE"
	PermDelete      = "MISSION_DELETE"
	PermSetRole     = "MISSION_SET_ROLE"
	PermSetPassword = "MISSION_SET_PASSWORD"
)

var rolePermissions = map[string][]string{
	RoleOwner:      {PermRead, PermWrite, PermDelete, PermSetRole, PermSetPassword},
	RoleSubscriber: {PermRead, PermWrite},
	RoleReadOnly:   {PermRead},
}

// ValidRole reports whether r is a known role name.
func ValidRole(r string) bool { _, ok := rolePermissions[r]; return ok }

// Can reports whether role grants perm.
func Can(role, perm string) bool { return slices.Contains(rolePermissions[role], perm) }

// Change types recorded in a mission's change log.
const (
	ChangeCreate        = "CREATE_MISSION"
	ChangeDelete        = "DELETE_MISSION"
	ChangeAddContent    = "ADD_CONTENT"
	ChangeRemoveContent = "REMOVE_CONTENT"
)

const maxChanges = 2000

type Location struct {
	Lat float64 `json:"lat"`
	Lon float64 `json:"lon"`
}

// UIDDetails summarises a map item for change feeds (official UidDetails).
type UIDDetails struct {
	Type        string    `json:"type,omitempty"`
	Callsign    string    `json:"callsign,omitempty"`
	Title       string    `json:"title,omitempty"`
	IconsetPath string    `json:"iconsetPath,omitempty"`
	Color       string    `json:"color,omitempty"`
	Location    *Location `json:"location,omitempty"`
}

type UIDItem struct {
	UID        string      `json:"uid"`
	Timestamp  time.Time   `json:"timestamp"`
	CreatorUID string      `json:"creator_uid"`
	Details    *UIDDetails `json:"details,omitempty"`
}

type ContentItem struct {
	Hash       string    `json:"hash"`
	Timestamp  time.Time `json:"timestamp"`
	CreatorUID string    `json:"creator_uid"`
}

type Subscription struct {
	ClientUID string    `json:"client_uid"`
	Username  string    `json:"username"`
	Role      string    `json:"role"`
	Created   time.Time `json:"created"`
}

type Invitation struct {
	Type       string    `json:"type"` // clientUid, callsign, userName, group, team
	Invitee    string    `json:"invitee"`
	CreatorUID string    `json:"creator_uid"`
	Role       string    `json:"role"`
	Created    time.Time `json:"created"`
}

type Change struct {
	Type        string      `json:"type"`
	Timestamp   time.Time   `json:"timestamp"`
	CreatorUID  string      `json:"creator_uid"`
	ContentUID  string      `json:"content_uid,omitempty"`
	ContentHash string      `json:"content_hash,omitempty"`
	Details     *UIDDetails `json:"details,omitempty"`
}

type LogEntry struct {
	ID            string    `json:"id"`
	Content       string    `json:"content"`
	CreatorUID    string    `json:"creatorUid"`
	EntryUID      string    `json:"entryUid,omitempty"`
	MissionNames  []string  `json:"missionNames"`
	ServerTime    time.Time `json:"servertime"`
	DTG           time.Time `json:"dtg"`
	Created       time.Time `json:"created"`
	ContentHashes []string  `json:"contentHashes"`
	Keywords      []string  `json:"keywords"`
}

// Mission is the persisted state of one mission.
type Mission struct {
	GUID           string    `json:"guid"`
	Name           string    `json:"name"`
	Description    string    `json:"description"`
	ChatRoom       string    `json:"chat_room"`
	BaseLayer      string    `json:"base_layer"`
	BBox           string    `json:"bbox"`
	Path           string    `json:"path"`
	Classification string    `json:"classification"`
	Tool           string    `json:"tool"`
	Keywords       []string  `json:"keywords"`
	CreatorUID     string    `json:"creator_uid"`
	CreatorUser    string    `json:"creator_user"`
	Created        time.Time `json:"created"`
	LastEdited     time.Time `json:"last_edited"`
	Groups         []string  `json:"groups"`
	PasswordHash   string    `json:"password_hash,omitempty"`
	DefaultRole    string    `json:"default_role"`
	InviteOnly     bool      `json:"invite_only"`
	Expiration     int64     `json:"expiration"`

	UIDs          []UIDItem                `json:"uids"`
	Contents      []ContentItem            `json:"contents"`
	Subscriptions map[string]*Subscription `json:"subscriptions"` // by client UID
	Invitations   []Invitation             `json:"invitations"`
	Changes       []Change                 `json:"changes"`
	Logs          []LogEntry               `json:"logs"`
	CoT           map[string]string        `json:"cot"` // latest XML per member UID
}

func (m *Mission) HasUID(uid string) bool {
	return slices.ContainsFunc(m.UIDs, func(u UIDItem) bool { return u.UID == uid })
}

func (m *Mission) HasHash(hash string) bool {
	return slices.ContainsFunc(m.Contents, func(c ContentItem) bool { return c.Hash == hash })
}

func (m *Mission) addChange(c Change) {
	m.Changes = append(m.Changes, c)
	if n := len(m.Changes); n > maxChanges {
		m.Changes = slices.Clone(m.Changes[n-maxChanges:])
	}
	m.LastEdited = c.Timestamp
}

// Caller identifies who is acting on a mission.
type Caller struct {
	Username  string
	Groups    []string
	Admin     bool
	Anonymous bool
	ClientUID string // device UID from the request (uid/creatorUid/clientUid)
	Token     string // mission token from MissionAuthorization / Authorization: Bearer
}

var (
	ErrNotFound    = errors.New("mission not found")
	ErrForbidden   = errors.New("forbidden")
	ErrExists      = errors.New("mission already exists")
	ErrBadPassword = errors.New("invalid mission password")
	ErrInvalid     = errors.New("invalid request")
)

// Service owns all missions. Every mission lives in memory and is written
// to <dir>/<guid>.json whenever it changes.
type Service struct {
	dir    string
	log    *slog.Logger
	store  *store.Store
	router *router.Router
	key    []byte

	mu       sync.RWMutex
	missions map[string]*Mission // by lower-cased name
	dirty    map[*Mission]bool   // CoT updated, save pending (see FlushLoop)
	latest   *latestCache

	// PackageDir holds data package files, for mission archives.
	PackageDir string
}

// Open loads every mission from dir and registers the routing hook.
func Open(dir string, st *store.Store, rt *router.Router, log *slog.Logger) (*Service, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	s := &Service{dir: dir, log: log, store: st, router: rt, missions: map[string]*Mission{}, latest: newLatestCache(5000)}
	key, err := loadKey(filepath.Join(dir, ".token-key"))
	if err != nil {
		return nil, err
	}
	s.key = key
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		var m Mission
		if err := json.Unmarshal(data, &m); err != nil {
			return nil, fmt.Errorf("mission %s: %w", e.Name(), err)
		}
		if m.Subscriptions == nil {
			m.Subscriptions = map[string]*Subscription{}
		}
		if m.CoT == nil {
			m.CoT = map[string]string{}
		}
		s.missions[strings.ToLower(m.Name)] = &m
	}
	rt.AddHook(s.hook)
	rt.SetMissionResolver(s.resolve)
	return s, nil
}

func loadKey(path string) ([]byte, error) {
	if b, err := os.ReadFile(path); err == nil && len(b) >= 32 {
		return b, nil
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	return key, os.WriteFile(path, key, 0o600)
}

// save persists m. Callers hold s.mu for writing.
func (s *Service) save(m *Mission) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	path := filepath.Join(s.dir, m.GUID+".json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o640); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (s *Service) get(name string) (*Mission, bool) {
	m, ok := s.missions[strings.ToLower(strings.TrimSpace(name))]
	return m, ok
}

// ---- access control ----

func (m *Mission) visibleTo(c Caller) bool {
	if c.Admin {
		return true
	}
	for _, g := range m.Groups {
		if slices.Contains(c.Groups, g) {
			return true
		}
	}
	return false
}

func (m *Mission) invited(c Caller) (Invitation, bool) {
	for _, inv := range m.Invitations {
		switch inv.Type {
		case "clientUid":
			if c.ClientUID != "" && inv.Invitee == c.ClientUID {
				return inv, true
			}
		case "userName":
			if c.Username != "" && inv.Invitee == c.Username {
				return inv, true
			}
		case "group":
			if slices.Contains(c.Groups, inv.Invitee) {
				return inv, true
			}
		}
	}
	return Invitation{}, false
}

func (m *Mission) isOwner(c Caller) bool {
	if c.Admin {
		return true
	}
	if !c.Anonymous && c.Username != "" && c.Username == m.CreatorUser {
		return true
	}
	return c.ClientUID != "" && c.ClientUID == m.CreatorUID
}

// role resolves the caller's role on m without a password, or "" for none.
func (s *Service) role(m *Mission, c Caller) string {
	if m.isOwner(c) {
		return RoleOwner
	}
	if !m.visibleTo(c) {
		return ""
	}
	if r := s.tokenRole(m, c.Token); r != "" {
		return r
	}
	if c.ClientUID != "" {
		if sub, ok := m.Subscriptions[c.ClientUID]; ok {
			return sub.Role
		}
	}
	if !c.Anonymous && c.Username != "" {
		for _, sub := range m.Subscriptions {
			if sub.Username == c.Username {
				return sub.Role
			}
		}
	}
	if inv, ok := m.invited(c); ok {
		return firstNonEmpty(inv.Role, m.DefaultRole)
	}
	if m.PasswordHash != "" || m.InviteOnly {
		return ""
	}
	return m.DefaultRole
}

// roleWithPassword is role(), additionally accepting the mission password.
func (s *Service) roleWithPassword(m *Mission, c Caller, password string) (string, error) {
	if r := s.role(m, c); r != "" {
		return r, nil
	}
	if !m.visibleTo(c) {
		return "", ErrNotFound
	}
	if m.PasswordHash != "" {
		if password == "" || bcrypt.CompareHashAndPassword([]byte(m.PasswordHash), []byte(password)) != nil {
			return "", ErrBadPassword
		}
		if !m.InviteOnly {
			return m.DefaultRole, nil
		}
	}
	return "", ErrForbidden
}

// require returns the mission if the caller holds perm on it.
func (s *Service) require(name string, c Caller, perm string) (*Mission, string, error) {
	m, ok := s.get(name)
	if !ok {
		return nil, "", ErrNotFound
	}
	r := s.role(m, c)
	if r == "" {
		if !m.visibleTo(c) {
			return nil, "", ErrNotFound
		}
		return nil, "", ErrForbidden
	}
	if !Can(r, perm) {
		return nil, r, ErrForbidden
	}
	return m, r, nil
}

// ---- tokens ----

// Token format: base64url(guid|clientUid|role|expiryUnix).base64url(hmac)
func (s *Service) newToken(m *Mission, clientUID, role string) string {
	payload := strings.Join([]string{m.GUID, clientUID, role, strconv.FormatInt(time.Now().Add(365*24*time.Hour).Unix(), 10)}, "|")
	enc := base64.RawURLEncoding.EncodeToString([]byte(payload))
	return enc + "." + s.sign(enc)
}

func (s *Service) sign(v string) string {
	h := hmac.New(sha256.New, s.key)
	h.Write([]byte(v))
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}

func (s *Service) tokenRole(m *Mission, token string) string {
	enc, sig, ok := strings.Cut(token, ".")
	if !ok || !hmac.Equal([]byte(sig), []byte(s.sign(enc))) {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(enc)
	if err != nil {
		return ""
	}
	parts := strings.Split(string(raw), "|")
	if len(parts) != 4 || parts[0] != m.GUID || !ValidRole(parts[2]) {
		return ""
	}
	exp, _ := strconv.ParseInt(parts[3], 10, 64)
	if time.Now().Unix() > exp {
		return ""
	}
	// A token never grants more than the holder's current subscription.
	if sub, ok := m.Subscriptions[parts[1]]; ok {
		return sub.Role
	}
	return parts[2]
}

// ---- helpers ----

func newGUID() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// subscriberUIDs lists the client UIDs subscribed to m.
func subscriberUIDs(m *Mission) []string {
	out := make([]string, 0, len(m.Subscriptions))
	for uid := range m.Subscriptions {
		out = append(out, uid)
	}
	sort.Strings(out)
	return out
}
