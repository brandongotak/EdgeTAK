package mission

import (
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/brandongotak/EdgeTAK/internal/cot"
	"github.com/brandongotak/EdgeTAK/internal/router"
)

// View controls what a mission rendering includes.
type View struct {
	Changes bool
	Logs    bool
	Since   time.Time // only changes/logs after this time
	Token   string    // token to embed (create/subscribe responses)
}

// List returns the missions visible to c. Password-protected missions are
// included (flagged passwordProtected) only when withProtected is set.
func (s *Service) List(c Caller, withProtected bool, tool string) []map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []map[string]any{}
	for _, m := range s.sorted() {
		if !m.visibleTo(c) || (tool != "" && m.Tool != tool) {
			continue
		}
		if m.PasswordHash != "" && !withProtected && s.role(m, c) == "" {
			continue
		}
		if m.InviteOnly && s.role(m, c) == "" {
			continue
		}
		out = append(out, s.render(m, View{}))
	}
	return out
}

func (s *Service) sorted() []*Mission {
	ms := make([]*Mission, 0, len(s.missions))
	for _, m := range s.missions {
		ms = append(ms, m)
	}
	sort.Slice(ms, func(i, j int) bool { return strings.ToLower(ms[i].Name) < strings.ToLower(ms[j].Name) })
	return ms
}

// Get renders one mission for a caller holding read access.
func (s *Service) Get(name string, c Caller, password string, v View) (map[string]any, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, ok := s.get(name)
	if !ok {
		return nil, ErrNotFound
	}
	r, err := s.roleWithPassword(m, c, password)
	if err != nil {
		return nil, err
	}
	if !Can(r, PermRead) {
		return nil, ErrForbidden
	}
	return s.render(m, v), nil
}

// CreateParams mirrors the query parameters of PUT /Marti/api/missions/{name}.
type CreateParams struct {
	Name, CreatorUID, Description, ChatRoom, BaseLayer, BBox, Path, Classification, Tool string
	Password, DefaultRole                                                                string
	Groups                                                                               []string
	InviteOnly                                                                           bool
	Expiration                                                                           int64
}

// Create makes a new mission owned by c (or updates it if c already owns a
// mission of that name). The creator is subscribed as owner.
func (s *Service) Create(p CreateParams, c Caller) (view map[string]any, created bool, err error) {
	p.Name = strings.TrimSpace(p.Name)
	if p.Name == "" || len(p.Name) > 256 || strings.ContainsAny(p.Name, "/\\") {
		return nil, false, ErrInvalid
	}
	if p.DefaultRole == "" {
		p.DefaultRole = RoleSubscriber
	}
	if !ValidRole(p.DefaultRole) {
		return nil, false, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()

	if m, ok := s.get(p.Name); ok {
		if !m.isOwner(c) {
			return nil, false, ErrExists
		}
		for dst, v := range map[*string]string{&m.Description: p.Description, &m.ChatRoom: p.ChatRoom, &m.BaseLayer: p.BaseLayer,
			&m.BBox: p.BBox, &m.Path: p.Path, &m.Classification: p.Classification} {
			if v != "" {
				*dst = v
			}
		}
		m.LastEdited = now
		if err := s.save(m); err != nil {
			return nil, false, err
		}
		return s.render(m, View{Token: s.ownerToken(m, c)}), false, nil
	}

	m := &Mission{
		GUID: newGUID(), Name: p.Name, Description: p.Description, ChatRoom: p.ChatRoom, BaseLayer: p.BaseLayer,
		BBox: p.BBox, Path: p.Path, Classification: p.Classification, Tool: firstNonEmpty(p.Tool, "public"),
		Keywords: []string{}, CreatorUID: p.CreatorUID, CreatorUser: c.Username, Created: now, LastEdited: now,
		Groups: missionGroups(p.Groups, c), DefaultRole: p.DefaultRole, InviteOnly: p.InviteOnly, Expiration: -1,
		Subscriptions: map[string]*Subscription{}, CoT: map[string]string{},
	}
	if p.Expiration > 0 {
		m.Expiration = p.Expiration
	}
	if p.Password != "" {
		h, err := bcrypt.GenerateFromPassword([]byte(p.Password), bcrypt.DefaultCost)
		if err != nil {
			return nil, false, err
		}
		m.PasswordHash = string(h)
	}
	if p.CreatorUID != "" {
		m.Subscriptions[p.CreatorUID] = &Subscription{ClientUID: p.CreatorUID, Username: c.Username, Role: RoleOwner, Created: now}
	}
	m.addChange(Change{Type: ChangeCreate, Timestamp: now, CreatorUID: p.CreatorUID})
	if err := s.save(m); err != nil {
		return nil, false, err
	}
	s.missions[strings.ToLower(m.Name)] = m
	s.log.Info("mission created", "mission", m.Name, "user", c.Username, "creatorUid", p.CreatorUID, "groups", m.Groups)
	s.router.Inject(missionEvent(m, "t-x-m-n", "CREATE", p.CreatorUID, ""), m.Groups)
	return s.render(m, View{Token: s.ownerToken(m, c)}), true, nil
}

func (s *Service) ownerToken(m *Mission, c Caller) string {
	return s.newToken(m, c.ClientUID, RoleOwner)
}

// missionGroups keeps the requested groups the caller belongs to, falling
// back to all of the caller's groups.
func missionGroups(requested []string, c Caller) []string {
	var out []string
	for _, g := range requested {
		g = strings.TrimSpace(g)
		if g != "" && (c.Admin || slices.Contains(c.Groups, g)) && !slices.Contains(out, g) {
			out = append(out, g)
		}
	}
	if len(out) == 0 {
		out = slices.Clone(c.Groups)
	}
	return out
}

// Delete removes a mission. Subscribers are notified.
func (s *Service) Delete(name string, c Caller) (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, _, err := s.require(name, c, PermDelete)
	if err != nil {
		return nil, err
	}
	view := s.render(m, View{})
	if err := os.Remove(filepath.Join(s.dir, m.GUID+".json")); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	delete(s.missions, strings.ToLower(m.Name))
	s.router.SendTo(missionEvent(m, "t-x-m-d", "DELETE", c.ClientUID, ""), subscriberUIDs(m), nil)
	s.log.Info("mission deleted", "mission", m.Name, "user", c.Username)
	return view, nil
}

// ---- subscriptions ----

// Subscribe adds c.ClientUID as a subscriber and returns the subscription.
func (s *Service) Subscribe(name string, c Caller, password string, since time.Time) (map[string]any, error) {
	if c.ClientUID == "" {
		return nil, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.get(name)
	if !ok {
		return nil, ErrNotFound
	}
	role, err := s.roleWithPassword(m, c, password)
	if err != nil {
		return nil, err
	}
	sub, ok := m.Subscriptions[c.ClientUID]
	if !ok {
		sub = &Subscription{ClientUID: c.ClientUID, Username: c.Username, Role: role, Created: time.Now().UTC()}
		m.Subscriptions[c.ClientUID] = sub
		if err := s.save(m); err != nil {
			return nil, err
		}
		s.log.Info("mission subscribe", "mission", m.Name, "clientUid", c.ClientUID, "user", c.Username, "role", role)
	}
	token := s.newToken(m, c.ClientUID, sub.Role)
	out := subscriptionJSON(m, sub, token)
	out["mission"] = s.render(m, View{Changes: !since.IsZero(), Since: since, Token: token})
	return out, nil
}

// Unsubscribe removes c.ClientUID from the mission.
func (s *Service) Unsubscribe(name string, c Caller) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.get(name)
	if !ok {
		return ErrNotFound
	}
	if _, ok := m.Subscriptions[c.ClientUID]; !ok {
		return nil
	}
	delete(m.Subscriptions, c.ClientUID)
	s.log.Info("mission unsubscribe", "mission", m.Name, "clientUid", c.ClientUID)
	return s.save(m)
}

// Subscription returns uid's subscription to the mission.
func (s *Service) Subscription(name string, c Caller, uid string) (map[string]any, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, _, err := s.require(name, c, PermRead)
	if err != nil {
		return nil, err
	}
	sub, ok := m.Subscriptions[uid]
	if !ok {
		return nil, ErrNotFound
	}
	return subscriptionJSON(m, sub, ""), nil
}

// Subscribers returns subscriber client UIDs, or their roles when withRoles.
func (s *Service) Subscribers(name string, c Caller, withRoles bool) (any, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, _, err := s.require(name, c, PermRead)
	if err != nil {
		return nil, err
	}
	if !withRoles {
		return subscriberUIDs(m), nil
	}
	out := []map[string]any{}
	for _, uid := range subscriberUIDs(m) {
		out = append(out, subscriptionJSON(m, m.Subscriptions[uid], ""))
	}
	return out, nil
}

// AllSubscriptions lists (mission name, client UID) pairs visible to c.
func (s *Service) AllSubscriptions(c Caller) []map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []map[string]string{}
	for _, m := range s.sorted() {
		if !m.visibleTo(c) {
			continue
		}
		for _, uid := range subscriberUIDs(m) {
			out = append(out, map[string]string{m.Name: uid})
		}
	}
	return out
}

// Role returns the caller's role on the mission.
func (s *Service) Role(name string, c Caller) (map[string]any, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, ok := s.get(name)
	if !ok {
		return nil, ErrNotFound
	}
	r := s.role(m, c)
	if r == "" {
		return nil, ErrForbidden
	}
	return roleJSON(r), nil
}

// SetRole changes a subscriber's role (owner only).
func (s *Service) SetRole(name string, c Caller, clientUID, role string) error {
	if !ValidRole(role) || clientUID == "" {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m, _, err := s.require(name, c, PermSetRole)
	if err != nil {
		return err
	}
	sub, ok := m.Subscriptions[clientUID]
	if !ok {
		return ErrNotFound
	}
	sub.Role = role
	if err := s.save(m); err != nil {
		return err
	}
	ev := missionEvent(m, "t-x-m-r", "INVITE", c.ClientUID, roleXML(role))
	s.router.SendTo(ev, []string{clientUID}, nil)
	return nil
}

// ---- contents ----

// AddContents adds files (by hash) and map items (by UID) to a mission.
func (s *Service) AddContents(name string, c Caller, hashes, uids []string) (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, _, err := s.require(name, c, PermWrite)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	changed := false
	for _, h := range hashes {
		h = strings.ToLower(strings.TrimSpace(h))
		if h == "" || m.HasHash(h) {
			continue
		}
		if _, ok := s.store.Package(h); !ok {
			s.log.Warn("mission content references unknown file", "mission", m.Name, "hash", h)
			continue
		}
		m.Contents = append(m.Contents, ContentItem{Hash: h, Timestamp: now, CreatorUID: c.ClientUID})
		s.recordChange(m, Change{Type: ChangeAddContent, Timestamp: now, CreatorUID: c.ClientUID, ContentHash: h})
		changed = true
	}
	for _, uid := range uids {
		if uid == "" || m.HasUID(uid) {
			continue
		}
		s.addUID(m, uid, c.ClientUID, now)
		changed = true
	}
	if changed {
		if err := s.save(m); err != nil {
			return nil, err
		}
	}
	return s.render(m, View{}), nil
}

// addUID adds a map item, using the latest CoT seen for it. Callers hold s.mu.
func (s *Service) addUID(m *Mission, uid, creatorUID string, now time.Time) {
	var details *UIDDetails
	if ev := s.latest.get(uid); ev != nil {
		details = uidDetails(ev)
		m.CoT[uid] = string(ev.XML())
	}
	m.UIDs = append(m.UIDs, UIDItem{UID: uid, Timestamp: now, CreatorUID: creatorUID, Details: details})
	s.recordChange(m, Change{Type: ChangeAddContent, Timestamp: now, CreatorUID: creatorUID, ContentUID: uid, Details: details})
}

// RemoveContent removes a file and/or map item from a mission.
func (s *Service) RemoveContent(name string, c Caller, hash, uid string) (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, _, err := s.require(name, c, PermWrite)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	changed := false
	if hash = strings.ToLower(hash); hash != "" && m.HasHash(hash) {
		m.Contents = slices.DeleteFunc(m.Contents, func(ci ContentItem) bool { return ci.Hash == hash })
		s.recordChange(m, Change{Type: ChangeRemoveContent, Timestamp: now, CreatorUID: c.ClientUID, ContentHash: hash})
		changed = true
	}
	if uid != "" && m.HasUID(uid) {
		var details *UIDDetails
		m.UIDs = slices.DeleteFunc(m.UIDs, func(u UIDItem) bool {
			if u.UID == uid {
				details = u.Details
				return true
			}
			return false
		})
		delete(m.CoT, uid)
		s.recordChange(m, Change{Type: ChangeRemoveContent, Timestamp: now, CreatorUID: c.ClientUID, ContentUID: uid, Details: details})
		changed = true
	}
	if changed {
		if err := s.save(m); err != nil {
			return nil, err
		}
	}
	return s.render(m, View{}), nil
}

// recordChange appends to the change log and notifies subscribers.
func (s *Service) recordChange(m *Mission, ch Change) {
	m.addChange(ch)
	ev := missionEvent(m, "t-x-m-c", "CHANGE", ch.CreatorUID, s.changesXML(m, ch))
	s.router.SendTo(ev, subscriberUIDs(m), nil)
}

// Changes returns the change log since a time; squashed keeps only the
// latest change per item.
func (s *Service) Changes(name string, c Caller, since time.Time, squashed bool) ([]map[string]any, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, _, err := s.require(name, c, PermRead)
	if err != nil {
		return nil, err
	}
	return s.changesJSON(m, since, squashed), nil
}

// ---- keywords ----

func (s *Service) SetKeywords(name string, c Caller, keywords []string) (map[string]any, error) {
	return s.editKeywords(name, c, func(m *Mission) { m.Keywords = cleanList(keywords) })
}

func (s *Service) RemoveKeyword(name string, c Caller, keyword string) (map[string]any, error) {
	return s.editKeywords(name, c, func(m *Mission) {
		m.Keywords = slices.DeleteFunc(m.Keywords, func(k string) bool { return keyword == "" || k == keyword })
	})
}

func (s *Service) editKeywords(name string, c Caller, fn func(m *Mission)) (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, _, err := s.require(name, c, PermWrite)
	if err != nil {
		return nil, err
	}
	fn(m)
	m.LastEdited = time.Now().UTC()
	if err := s.save(m); err != nil {
		return nil, err
	}
	s.router.SendTo(missionEvent(m, "t-x-m-c-k", "CHANGE", c.ClientUID, ""), subscriberUIDs(m), nil)
	return s.render(m, View{}), nil
}

func cleanList(in []string) []string {
	out := []string{}
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v != "" && !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return out
}

// ---- logs ----

// AddLog stores a log entry in every mission it names.
func (s *Service) AddLog(e LogEntry, c Caller) (LogEntry, error) {
	if len(e.MissionNames) == 0 {
		return e, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var targets []*Mission
	for _, name := range e.MissionNames {
		m, _, err := s.require(name, c, PermWrite)
		if err != nil {
			return e, err
		}
		targets = append(targets, m)
	}
	now := time.Now().UTC()
	e.ID, e.ServerTime = newGUID(), now
	if e.Created.IsZero() {
		e.Created = now
	}
	if e.DTG.IsZero() {
		e.DTG = now
	}
	e.CreatorUID = firstNonEmpty(e.CreatorUID, c.ClientUID)
	e.ContentHashes, e.Keywords = nonNil(e.ContentHashes), nonNil(e.Keywords)
	for _, m := range targets {
		m.Logs = append(m.Logs, e)
		m.LastEdited = now
		if err := s.save(m); err != nil {
			return e, err
		}
		s.router.SendTo(missionEvent(m, "t-x-m-c-l", "CHANGE", e.CreatorUID, ""), subscriberUIDs(m), nil)
	}
	return e, nil
}

// UpdateLog replaces the content of an existing entry.
func (s *Service) UpdateLog(e LogEntry, c Caller) (LogEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var found *LogEntry
	for _, m := range s.missions {
		if s.role(m, c) == "" || !Can(s.role(m, c), PermWrite) {
			continue
		}
		for i := range m.Logs {
			if m.Logs[i].ID == e.ID {
				l := &m.Logs[i]
				l.Content, l.DTG, l.ServerTime = e.Content, firstTime(e.DTG, l.DTG), time.Now().UTC()
				if e.Keywords != nil {
					l.Keywords = e.Keywords
				}
				if e.ContentHashes != nil {
					l.ContentHashes = e.ContentHashes
				}
				found = l
				if err := s.save(m); err != nil {
					return e, err
				}
				s.router.SendTo(missionEvent(m, "t-x-m-c-l", "CHANGE", c.ClientUID, ""), subscriberUIDs(m), nil)
			}
		}
	}
	if found == nil {
		return e, ErrNotFound
	}
	return *found, nil
}

// DeleteLog removes an entry from every mission the caller can write.
func (s *Service) DeleteLog(id string, c Caller) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	found := false
	for _, m := range s.missions {
		if !Can(s.role(m, c), PermWrite) {
			continue
		}
		n := len(m.Logs)
		m.Logs = slices.DeleteFunc(m.Logs, func(l LogEntry) bool { return l.ID == id })
		if len(m.Logs) != n {
			found = true
			if err := s.save(m); err != nil {
				return err
			}
		}
	}
	if !found {
		return ErrNotFound
	}
	return nil
}

// Logs returns a mission's log entries since a time.
func (s *Service) Logs(name string, c Caller, since time.Time) ([]LogEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, _, err := s.require(name, c, PermRead)
	if err != nil {
		return nil, err
	}
	return filterLogs(m.Logs, since), nil
}

// AllLogs returns every log entry visible to c.
func (s *Service) AllLogs(c Caller) []LogEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	seen := map[string]bool{}
	out := []LogEntry{}
	for _, m := range s.sorted() {
		if !Can(s.role(m, c), PermRead) {
			continue
		}
		for _, l := range m.Logs {
			if !seen[l.ID] {
				seen[l.ID] = true
				out = append(out, l)
			}
		}
	}
	return out
}

func filterLogs(logs []LogEntry, since time.Time) []LogEntry {
	out := []LogEntry{}
	for _, l := range logs {
		if since.IsZero() || l.ServerTime.After(since) {
			out = append(out, l)
		}
	}
	return out
}

func nonNil(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

func firstTime(a, b time.Time) time.Time {
	if !a.IsZero() {
		return a
	}
	return b
}

// ---- invitations ----

var inviteTypes = []string{"clientUid", "callsign", "userName", "group", "team"}

// Invite records an invitation and notifies the invitee if connected.
func (s *Service) Invite(name string, c Caller, typ, invitee, role string) error {
	if !slices.Contains(inviteTypes, typ) || invitee == "" {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m, myRole, err := s.require(name, c, PermWrite)
	if err != nil {
		return err
	}
	if role == "" {
		role = m.DefaultRole
	}
	if !ValidRole(role) || (role == RoleOwner && myRole != RoleOwner) {
		return ErrForbidden
	}
	m.Invitations = slices.DeleteFunc(m.Invitations, func(i Invitation) bool { return i.Type == typ && i.Invitee == invitee })
	m.Invitations = append(m.Invitations, Invitation{Type: typ, Invitee: invitee, CreatorUID: c.ClientUID, Role: role, Created: time.Now().UTC()})
	if err := s.save(m); err != nil {
		return err
	}
	ev := missionEvent(m, "t-x-m-i", "INVITE", c.ClientUID, roleXML(role))
	switch typ {
	case "clientUid":
		ev.Detail.XML = strings.Replace(ev.Detail.XML, "<mission ", `<mission token="`+xmlEsc(s.newToken(m, invitee, role))+`" `, 1)
		ev.Changed()
		s.router.SendTo(ev, []string{invitee}, nil)
	case "callsign":
		s.router.SendTo(ev, nil, []string{invitee})
	}
	s.log.Info("mission invite", "mission", m.Name, "type", typ, "invitee", invitee, "role", role)
	return nil
}

// Uninvite removes an invitation.
func (s *Service) Uninvite(name string, c Caller, typ, invitee string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, _, err := s.require(name, c, PermWrite)
	if err != nil {
		return err
	}
	m.Invitations = slices.DeleteFunc(m.Invitations, func(i Invitation) bool { return i.Type == typ && i.Invitee == invitee })
	return s.save(m)
}

// Invitations returns the missions c has been invited to.
func (s *Service) Invitations(c Caller) []map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []map[string]any{}
	for _, m := range s.sorted() {
		if inv, ok := m.invited(c); ok && m.visibleTo(c) {
			out = append(out, map[string]any{
				"missionName": m.Name, "missionGuid": m.GUID, "invitee": inv.Invitee, "type": inv.Type,
				"creatorUid": inv.CreatorUID, "createTime": fmtTime(inv.Created), "role": roleJSON(inv.Role),
				"token": s.newToken(m, c.ClientUID, inv.Role),
			})
		}
	}
	return out
}

// ---- password ----

func (s *Service) SetPassword(name string, c Caller, password string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, _, err := s.require(name, c, PermSetPassword)
	if err != nil {
		return err
	}
	if password == "" {
		m.PasswordHash = ""
	} else {
		h, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
		if err != nil {
			return err
		}
		m.PasswordHash = string(h)
	}
	return s.save(m)
}

// ---- CoT ----

// CoT returns the latest event for every map item in the mission, wrapped
// in <events>, with <marti> routing elements removed.
func (s *Service) CoT(name string, c Caller) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, _, err := s.require(name, c, PermRead)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?><events>`)
	for _, u := range m.UIDs {
		if x, ok := m.CoT[u.UID]; ok {
			b.WriteString(stripMarti(x))
			b.WriteByte('\n')
		}
	}
	b.WriteString("</events>")
	return b.String(), nil
}

// CanReadHash reports whether c can read a mission that contains the file.
func (s *Service) CanReadHash(hash string, c Caller) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, m := range s.missions {
		if m.HasHash(hash) && Can(s.role(m, c), PermRead) {
			return true
		}
	}
	return false
}

// ---- routing integration ----

// hook watches routed events: it remembers the latest event per UID, keeps
// mission CoT current, and adds items sent to <dest mission="..."/> to the
// mission like the official server does.
func (s *Service) hook(src *router.Client, ev *cot.Event) bool {
	if cot.IsControl(ev.Type) || strings.HasPrefix(ev.Type, "t-x-m-") {
		return true // must not take s.mu: these are injected while it is held
	}
	var groups []string
	if src != nil {
		groups = src.Groups
	}
	s.latest.put(ev, groups)

	var missions []string
	for _, d := range ev.Dests() {
		if d.Mission != "" {
			missions = append(missions, d.Mission)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	xml := ""
	for _, m := range s.missions {
		if m.HasUID(ev.UID) {
			if xml == "" {
				xml = string(ev.XML())
			}
			m.CoT[ev.UID] = xml
			s.markDirty(m)
		}
	}
	if len(missions) == 0 || src == nil {
		return true
	}
	c := callerFor(src)
	now := time.Now().UTC()
	for _, name := range missions {
		m, ok := s.get(name)
		if !ok || !Can(s.role(m, c), PermWrite) {
			s.log.Debug("ignoring mission content from client without write access", "mission", name, "uid", c.ClientUID)
			continue
		}
		if !m.HasUID(ev.UID) {
			s.addUID(m, ev.UID, c.ClientUID, now)
			m.CoT[ev.UID] = string(ev.XML())
			if err := s.save(m); err != nil {
				s.log.Error("save mission", "mission", m.Name, "err", err)
			}
		}
	}
	return true
}

// resolve returns the subscribers of a mission for the router.
func (s *Service) resolve(src *router.Client, name string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, ok := s.get(name)
	if !ok {
		return nil
	}
	if src != nil && s.role(m, callerFor(src)) == "" {
		return nil
	}
	return subscriberUIDs(m)
}

func callerFor(c *router.Client) Caller {
	uid, _ := c.Identity()
	return Caller{Username: c.Username, Groups: c.Groups, Anonymous: c.Username == "", ClientUID: uid}
}

// markDirty schedules a debounced save for frequent CoT updates.
func (s *Service) markDirty(m *Mission) {
	if s.dirty == nil {
		s.dirty = map[*Mission]bool{}
	}
	s.dirty[m] = true
}

// FlushLoop periodically persists missions with pending CoT updates.
func (s *Service) FlushLoop(done <-chan struct{}) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			s.Flush()
		case <-done:
			s.Flush()
			return
		}
	}
}

// Flush writes every mission with pending changes.
func (s *Service) Flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for m := range s.dirty {
		if _, live := s.missions[strings.ToLower(m.Name)]; live {
			if err := s.save(m); err != nil {
				s.log.Error("save mission", "mission", m.Name, "err", err)
			}
		}
	}
	s.dirty = nil
}

// Summary is the admin console's view of a mission.
type Summary struct {
	Name        string    `json:"name"`
	GUID        string    `json:"guid"`
	Description string    `json:"description"`
	Creator     string    `json:"creator"`
	CreatorUID  string    `json:"creator_uid"`
	Groups      []string  `json:"groups"`
	Tool        string    `json:"tool"`
	Protected   bool      `json:"password_protected"`
	InviteOnly  bool      `json:"invite_only"`
	DefaultRole string    `json:"default_role"`
	Items       int       `json:"items"`
	Files       int       `json:"files"`
	Subscribers []string  `json:"subscribers"`
	Changes     int       `json:"changes"`
	Logs        int       `json:"logs"`
	Created     time.Time `json:"created"`
	LastEdited  time.Time `json:"last_edited"`
}

// Summaries lists every mission for administrators.
func (s *Service) Summaries() []Summary {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []Summary{}
	for _, m := range s.sorted() {
		out = append(out, Summary{
			Name: m.Name, GUID: m.GUID, Description: m.Description, Creator: m.CreatorUser, CreatorUID: m.CreatorUID,
			Groups: nonNil(m.Groups), Tool: m.Tool, Protected: m.PasswordHash != "", InviteOnly: m.InviteOnly,
			DefaultRole: m.DefaultRole, Items: len(m.UIDs), Files: len(m.Contents), Subscribers: subscriberUIDs(m),
			Changes: len(m.Changes), Logs: len(m.Logs), Created: m.Created, LastEdited: m.LastEdited,
		})
	}
	return out
}

// NameByGUID resolves a mission GUID to its name.
func (s *Service) NameByGUID(guid string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, m := range s.missions {
		if strings.EqualFold(m.GUID, guid) {
			return m.Name, true
		}
	}
	return "", false
}
