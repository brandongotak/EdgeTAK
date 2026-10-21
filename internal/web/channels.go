package web

import (
	"encoding/json"
	"io"
	"net/http"
	"slices"

	"github.com/brandongotak/EdgeTAK/internal/store"
)

// Channels: users switch their group memberships on and off from the client
// (ATAK "Channels"), like the official server's active-group cache. Clients
// only offer this when groupCacheEnabled is true. Admins decide membership;
// users decide which of their memberships are active.

func (s *Server) channelRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /Marti/api/groups/all", s.authed(s.groupsAll))
	mux.HandleFunc("GET /Marti/api/groups/groupCacheEnabled", s.authed(func(w http.ResponseWriter, r *http.Request, _ *identity) {
		writeJSON(w, http.StatusOK, s.martiEnvelope("java.lang.Boolean", true))
	}))
	mux.HandleFunc("PUT /Marti/api/groups/active", s.authed(s.setActiveGroups))
	mux.HandleFunc("PUT /Marti/api/groups/activebits", s.authed(s.setActiveBits))
}

// groupBitpos numbers groups stably (by name order) for bitpos/activebits.
func (s *Server) groupBitpos() map[string]int {
	pos := map[string]int{}
	for i, g := range s.Store.Groups() {
		pos[g.Name] = i
	}
	return pos
}

// groupsAll lists the caller's memberships, one entry per group and
// direction, with their active state (official /Marti/api/groups/all).
func (s *Server) groupsAll(w http.ResponseWriter, r *http.Request, id *identity) {
	bitpos := s.groupBitpos()
	in, out := id.Groups, id.OutGroups
	isActive := func(string, string) bool { return true }
	if u, ok := s.Store.User(id.Username); ok && !id.Anonymous {
		in, out = u.Memberships(s.Cfg.AnonymousGroup) // include switched-off channels
		isActive = u.IsActive
	}
	data := []map[string]any{}
	add := func(name, dir string) {
		entry := map[string]any{"name": name, "direction": dir, "created": s.Started.Format("2006-01-02"),
			"type": "SYSTEM", "bitpos": bitpos[name], "active": isActive(name, dir), "description": ""}
		if g, ok := s.Store.Group(name); ok {
			entry["created"], entry["description"] = g.Created.Format("2006-01-02"), g.Description
		}
		data = append(data, entry)
	}
	for _, g := range in {
		add(g, "IN")
	}
	for _, g := range out {
		add(g, "OUT")
	}
	writeJSON(w, http.StatusOK, s.martiEnvelope("com.bbn.marti.remote.groups.Group", data))
}

// setActiveGroups takes the client's group list with active flags.
func (s *Server) setActiveGroups(w http.ResponseWriter, r *http.Request, id *identity) {
	var groups []struct {
		Name      string `json:"name"`
		Direction string `json:"direction"`
		Active    bool   `json:"active"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 256<<10)).Decode(&groups); err != nil {
		http.Error(w, "body must be a JSON array of groups", http.StatusBadRequest)
		return
	}
	active := map[string]bool{}
	for _, g := range groups {
		for _, dir := range directions(g.Direction) {
			active[g.Name+":"+dir] = g.Active
		}
	}
	s.applyActive(w, id, active)
}

// setActiveBits takes the bitpos values of the groups to keep active; every
// other membership is switched off.
func (s *Server) setActiveBits(w http.ResponseWriter, r *http.Request, id *identity) {
	var bits []int
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&bits); err != nil {
		http.Error(w, "body must be a JSON array of bit positions", http.StatusBadRequest)
		return
	}
	u, ok := s.Store.User(id.Username)
	if !ok || id.Anonymous {
		w.WriteHeader(http.StatusOK)
		return
	}
	bitpos := s.groupBitpos()
	in, out := u.Memberships(s.Cfg.AnonymousGroup)
	active := map[string]bool{}
	for _, l := range []struct {
		groups []string
		dir    string
	}{{in, "IN"}, {out, "OUT"}} {
		for _, g := range l.groups {
			active[g+":"+l.dir] = slices.Contains(bits, bitpos[g])
		}
	}
	s.applyActive(w, id, active)
}

func (s *Server) applyActive(w http.ResponseWriter, id *identity, active map[string]bool) {
	if id.Anonymous {
		w.WriteHeader(http.StatusOK) // anonymous clients have no stored channels
		return
	}
	if err := s.Store.SetActive(id.Username, active); err == store.ErrNotFound {
		w.WriteHeader(http.StatusOK) // certificate user without an account
		return
	} else if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.applyUserGroups(id.Username)
	s.Log.Info("active groups changed", "user", id.Username, "groups", active)
	w.WriteHeader(http.StatusOK)
}

// channelPrefEntries are the ATAK preferences that enable Channels for host
// (as the official server sends them), plus "Apply TAK Server profile
// updates", without which ATAK never requests the connection profile and
// so never learns about later changes. ATAK reads that one as a Boolean.
func channelPrefEntries(host string) string {
	return `<entry key="prefs_enable_channels" class="class java.lang.String">true</entry>` +
		`<entry key="prefs_enable_channels_host-` + xmlEscape(host) + `" class="class java.lang.String">true</entry>` +
		`<entry key="deviceProfileEnableOnConnect" class="class java.lang.Boolean">true</entry>`
}

func directions(d string) []string {
	switch d {
	case "IN", "OUT":
		return []string{d}
	}
	return []string{"IN", "OUT"}
}
