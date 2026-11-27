package mission

import (
	"slices"
	"strings"
	"time"

	"github.com/brandongotak/EdgeTAK/internal/cot"
)

// Mission templates: a snapshot of a mission's settings, files and map
// items, from which new missions are made (the web package keeps them).

// Snapshot is what a template takes from a mission.
type Snapshot struct {
	Description string   `json:"description"`
	Groups      []string `json:"groups"`
	DefaultRole string   `json:"default_role"`
	InviteOnly  bool     `json:"invite_only"`
	Tool        string   `json:"tool,omitempty"`
	Keywords    []string `json:"keywords,omitempty"`
	ChatRoom    string   `json:"chat_room,omitempty"`
	BaseLayer   string   `json:"base_layer,omitempty"`
	BBox        string   `json:"bbox,omitempty"`
	Files       []string `json:"files"` // data package hashes
	Items       []string `json:"items"` // map items' CoT XML
}

// TakeSnapshot copies a mission for a template (admins only).
func (s *Service) TakeSnapshot(name string, c Caller) (Snapshot, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, _, err := s.require(name, c, PermRead)
	if err != nil {
		return Snapshot{}, err
	}
	snap := Snapshot{Description: m.Description, Groups: slices.Clone(m.Groups), DefaultRole: m.DefaultRole, InviteOnly: m.InviteOnly,
		Tool: m.Tool, Keywords: slices.Clone(m.Keywords), ChatRoom: m.ChatRoom, BaseLayer: m.BaseLayer, BBox: m.BBox,
		Files: []string{}, Items: []string{}}
	for _, ci := range m.Contents {
		snap.Files = append(snap.Files, ci.Hash)
	}
	for _, u := range m.UIDs {
		if x := m.CoT[u.UID]; x != "" {
			snap.Items = append(snap.Items, x)
		}
	}
	return snap, nil
}

// AddTemplateItems adds map items from a template to a mission. Each gets
// a new UID (missions made from one template mustn't share items), with
// references between items (a route's points) renamed to match, and fresh
// times, stale no sooner than a year from now. It returns how many it added.
func (s *Service) AddTemplateItems(name string, c Caller, items []string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, _, err := s.require(name, c, PermWrite)
	if err != nil {
		return 0, err
	}
	var events []*cot.Event
	rename := map[string]string{}
	for _, x := range items {
		ev, err := cot.ParseXML([]byte(x))
		if err != nil || ev.UID == "" {
			continue
		}
		rename[ev.UID] = newGUID()
		events = append(events, ev)
	}
	now := time.Now().UTC()
	added := 0
	for _, ev := range events {
		life := ev.Stale.Sub(ev.Start)
		if life < 365*24*time.Hour {
			life = 365 * 24 * time.Hour
		}
		ev.UID = rename[ev.UID]
		ev.Time, ev.Start, ev.Stale = now, now, now.Add(life)
		for old, nu := range rename {
			ev.Detail.XML = strings.ReplaceAll(ev.Detail.XML, `uid="`+old+`"`, `uid="`+nu+`"`)
			ev.Detail.XML = strings.ReplaceAll(ev.Detail.XML, `uid='`+old+`'`, `uid='`+nu+`'`)
		}
		ev.Changed()
		m.CoT[ev.UID] = string(ev.XML())
		details := uidDetails(ev)
		m.UIDs = append(m.UIDs, UIDItem{UID: ev.UID, Timestamp: now, CreatorUID: c.ClientUID, Details: details})
		s.recordChange(m, Change{Type: ChangeAddContent, Timestamp: now, CreatorUID: c.ClientUID, ContentUID: ev.UID, Details: details})
		added++
	}
	if added > 0 {
		if err := s.save(m); err != nil {
			return 0, err
		}
	}
	return added, nil
}

// Exists reports whether a mission has this name.
func (s *Service) Exists(name string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.get(name)
	return ok
}
