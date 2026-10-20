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
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

type User struct {
	Username     string    `json:"username"`
	PasswordHash string    `json:"password_hash"`
	Groups       []string  `json:"groups"`
	Admin        bool      `json:"admin"`
	Created      time.Time `json:"created"`
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
	NextSeq  int64               `json:"next_seq"`
}

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
	s := &Store{path: path, db: db{Users: map[string]*User{}, Certs: map[string]*Cert{}, Packages: map[string]*Package{}, NextSeq: 1}}
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
	return &c
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
