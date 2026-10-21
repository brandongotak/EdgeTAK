package federation

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/brandongotak/EdgeTAK/internal/cot"
	"github.com/brandongotak/EdgeTAK/internal/store"
)

// Data packages over federation, as TAK Server does it:
//   - a file uploaded here is copied to federates ("create resource", with
//     the file as the payload), and a received copy is stored like an upload;
//   - a file transfer request (b-f-t-r) to devices on a federate becomes
//     "announce package"; the receiving server offers the file to its devices
//     from its own copy, so they download it from their own server.
//
// TAK Server parses ROL parameters with Jackson, which rejects unknown
// properties, and the ROL grammar has no null, so the JSON below carries
// exactly TAK Server's field names and never null.

const (
	maxSendFile  = 64 << 20        // larger files stay local
	announceWait = 2 * time.Minute // how long an announced file may take to arrive
)

// rolResource is TAK Server's enterprise sync Resource.
type rolResource struct {
	Filename       string   `json:"filename,omitempty"`
	Keywords       []string `json:"keywords"`
	MIMEType       string   `json:"mimeType,omitempty"`
	Name           string   `json:"name,omitempty"`
	SubmissionTime string   `json:"submissionTime,omitempty"`
	Submitter      string   `json:"submitter,omitempty"`
	UID            string   `json:"uid,omitempty"`
	CreatorUID     string   `json:"creatorUid,omitempty"`
	Hash           string   `json:"hash"`
	Size           int64    `json:"size"`
	Tool           string   `json:"tool,omitempty"`
}

// rolDetails is TAK Server's ResourceDetails: a package announcement.
// SenderUID holds the recipients as JSON (rolDestinations).
type rolDetails struct {
	Type           string `json:"type"`
	SHA256         string `json:"sha256"`
	SenderUID      string `json:"senderUid"`
	Name           string `json:"name"`
	Filename       string `json:"filename"`
	SenderURL      string `json:"senderUrl"`
	TSStored       int64  `json:"tsStored"`
	Size           int64  `json:"sizeInBytes"`
	SenderCallsign string `json:"senderCallsign"`
}

type rolDestinations struct {
	Callsigns []string `json:"callsigns"`
	UIDs      []string `json:"uids"`
}

// submissionTime is TAK Server's date format for resources.
const submissionTime = "2006-01-02T15:04:05.000Z"

// parseROL splits a one-statement program ("create resource\n{...};") into
// its operation, resource and JSON parameters.
func parseROL(program string) (op, res string, params []byte) {
	p := strings.TrimSuffix(strings.TrimSpace(program), ";")
	head, body := p, ""
	if i := strings.IndexByte(p, '{'); i >= 0 {
		head, body = p[:i], p[i:]
	}
	f := strings.Fields(head)
	if len(f) < 2 {
		return "", "", nil
	}
	return strings.ToLower(f[0]), strings.ToLower(f[1]), []byte(strings.TrimSpace(body))
}

func rolProgram(op, res string, params any) (string, error) {
	b, err := json.Marshal(params)
	if err != nil {
		return "", err
	}
	return op + " " + res + "\n" + string(b) + ";", nil
}

// waitingAnnounce is a package announcement whose file has not arrived yet.
type waitingAnnounce struct {
	details rolDetails
	dests   rolDestinations
	groups  []string
	at      time.Time
}

// ---- local -> federates ----

// PackageStored copies a file just uploaded here to the federates that share
// its groups. Device profiles stay local.
func (m *Manager) PackageStored(p *store.Package) {
	if p == nil || p.Tool == "profile" {
		return
	}
	for _, s := range m.connected() {
		if s.fed.NoPackages {
			continue
		}
		if shared, ok := sharedGroups(s.fed, p.Groups); ok {
			m.sendFile(s, p, shared)
		}
	}
}

// sendFile queues a file for a federate unless it already has it.
func (m *Manager) sendFile(s *session, p *store.Package, groups []string) bool {
	s.mu.Lock()
	sent := s.filesSent[p.Hash]
	s.mu.Unlock()
	if sent {
		return true
	}
	if p.Size > maxSendFile {
		s.record("out", "data package not sent", p.Hash, p.Name, fmt.Sprintf("larger than %d MB", maxSendFile>>20))
		return false
	}
	data, err := os.ReadFile(filepath.Join(m.cfg.PackageDir, p.Hash))
	if err != nil {
		m.cfg.Log.Warn("federation: could not read a data package to send", "package", p.Name, "err", err)
		return false
	}
	keywords := p.Keywords
	if keywords == nil {
		keywords = []string{}
	}
	program, err := rolProgram("create", "resource", rolResource{
		Filename: p.Name, Keywords: keywords, MIMEType: p.MIMEType, Name: p.Name,
		SubmissionTime: p.Submitted.UTC().Format(submissionTime), Submitter: p.Submitter, UID: p.Hash,
		CreatorUID: p.CreatorUID, Hash: p.Hash, Size: p.Size, Tool: p.Tool,
	})
	if err != nil {
		return false
	}
	if !s.enqueueROL(&ROL{Program: program, Payload: [][]byte{data}, FederateGroups: groups, Provenance: m.provenance()}) {
		return false
	}
	s.mu.Lock()
	s.filesSent[p.Hash] = true
	s.mu.Unlock()
	s.record("out", "data package", p.Hash, p.Name, sizeText(p.Size))
	return true
}

// fileShare reads the <fileshare> element of a file transfer request.
func fileShare(ev *cot.Event) map[string]string {
	dec := xml.NewDecoder(strings.NewReader(ev.Detail.XML))
	for {
		tok, err := dec.Token()
		if err != nil {
			return nil
		}
		if se, ok := tok.(xml.StartElement); ok && se.Name.Local == "fileshare" {
			attrs := map[string]string{}
			for _, a := range se.Attr {
				attrs[a.Name.Local] = a.Value
			}
			return attrs
		}
	}
}

// sharedPackage is the stored package a file transfer request offers, if
// EdgeTAK holds it.
func (m *Manager) sharedPackage(ev *cot.Event) (*store.Package, map[string]string) {
	if ev.Type != "b-f-t-r" {
		return nil, nil
	}
	fs := fileShare(ev)
	if fs == nil || fs["sha256"] == "" {
		return nil, nil
	}
	p, ok := m.cfg.Store.Package(strings.ToLower(fs["sha256"]))
	if !ok {
		return nil, nil
	}
	return p, fs
}

// announcePackage sends a file and its announcement in place of a file
// transfer request, as TAK Server does.
func (m *Manager) announcePackage(s *session, p *store.Package, fs map[string]string, dests []cot.Dest, groups []string) {
	if !m.sendFile(s, p, groups) {
		return
	}
	to := rolDestinations{Callsigns: []string{}, UIDs: []string{}}
	for _, d := range dests {
		if d.UID != "" {
			to.UIDs = append(to.UIDs, d.UID)
		}
		if d.Callsign != "" {
			to.Callsigns = append(to.Callsigns, d.Callsign)
		}
	}
	toJSON, _ := json.Marshal(to)
	program, err := rolProgram("announce", "package", rolDetails{
		Type: "ResourceDetails", SHA256: p.Hash, SenderUID: string(toJSON),
		Name: firstNonEmpty(fs["filename"], p.Name), Filename: firstNonEmpty(fs["filename"], p.Name),
		TSStored: time.Now().UnixMilli(), Size: p.Size, SenderCallsign: fs["senderCallsign"],
	})
	if err != nil {
		return
	}
	if s.enqueueROL(&ROL{Program: program, FederateGroups: groups, Provenance: m.provenance()}) {
		route := "to groups"
		if len(dests) > 0 {
			route = "to " + destNames(dests)
		}
		s.record("out", "b-f-t-r", p.Hash, p.Name, route)
	}
}

// ---- federates -> local ----

// handleROL acts on a ROL command from a federate. Files are stored and
// announcements offered to devices; missions are not federated yet.
func (m *Manager) handleROL(s *session, rol *ROL) {
	if hasProvenance(rol.Provenance, m.cfg.NodeID) {
		return
	}
	op, res, params := parseROL(rol.Program)
	m.cfg.Log.Debug("federation: received ROL", "federate", s.fed.Name, "op", op, "resource", res, "payload", len(rol.Payload), "groups", rol.FederateGroups)
	if res != "resource" && res != "package" {
		return // missions and data feeds
	}
	if s.fed.NoPackages {
		return
	}
	s.countIn()
	groups := m.deliveryGroups(s.fed, rol.FederateGroups)
	var err error
	switch {
	case res == "resource" && op == "create":
		err = m.receiveFile(s, rol, params, groups)
	case res == "package" && op == "announce":
		err = m.receiveAnnounce(s, params, groups)
	}
	if err != nil {
		s.setWarning("a data package could not be received: " + err.Error())
		m.cfg.Log.Warn("federation: data package not received", "federate", s.fed.Name, "err", err)
	}
}

func (m *Manager) receiveFile(s *session, rol *ROL, params []byte, groups []string) error {
	var r rolResource
	if err := json.Unmarshal(params, &r); err != nil {
		return fmt.Errorf("unreadable resource: %w", err)
	}
	if len(rol.Payload) == 0 {
		return nil // metadata only: nothing to store
	}
	data := rol.Payload[0]
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	if r.Hash != "" && !strings.EqualFold(r.Hash, hash) {
		return fmt.Errorf("%s: the file does not match its SHA-256", firstNonEmpty(r.Filename, r.Name))
	}
	if err := writeFile(m.cfg.PackageDir, hash, data); err != nil {
		return err
	}
	if old, ok := m.cfg.Store.Package(hash); ok {
		for _, g := range old.Groups {
			if !slices.Contains(groups, g) {
				groups = append(groups, g)
			}
		}
	}
	submitted, err := time.Parse(submissionTime, r.SubmissionTime)
	if err != nil {
		submitted = time.Now().UTC()
	}
	name := filepath.Base(firstNonEmpty(r.Filename, r.Name, hash))
	p, err := m.cfg.Store.PutPackage(store.Package{
		Hash: hash, Name: name, MIMEType: firstNonEmpty(r.MIMEType, "application/octet-stream"), Size: int64(len(data)),
		Keywords: r.Keywords, Tool: firstNonEmpty(r.Tool, "public"), CreatorUID: r.CreatorUID,
		Submitter: strings.TrimSpace(r.Submitter + " (" + s.fed.Name + ")"), Groups: groups, Submitted: submitted,
	})
	if err != nil {
		return err
	}
	// It came from this federate: never send it back.
	s.mu.Lock()
	s.filesSent[hash] = true
	w, waiting := s.waiting[hash]
	delete(s.waiting, hash)
	s.mu.Unlock()
	s.record("in", "data package", hash, name, sizeText(p.Size))
	m.cfg.Log.Info("federation: received data package", "federate", s.fed.Name, "name", name, "hash", hash, "size", p.Size)
	if waiting {
		m.offer(s, w)
	}
	return nil
}

func (m *Manager) receiveAnnounce(s *session, params []byte, groups []string) error {
	var d rolDetails
	if err := json.Unmarshal(params, &d); err != nil {
		return fmt.Errorf("unreadable announcement: %w", err)
	}
	if d.SHA256 == "" {
		return errors.New("an announcement without a file hash")
	}
	d.SHA256 = strings.ToLower(d.SHA256)
	w := waitingAnnounce{details: d, groups: groups, at: time.Now()}
	if d.SenderUID != "" {
		json.Unmarshal([]byte(d.SenderUID), &w.dests)
	}
	if _, ok := m.cfg.Store.Package(d.SHA256); ok {
		m.offer(s, w)
		return nil
	}
	s.mu.Lock()
	s.waiting[d.SHA256] = w
	s.mu.Unlock()
	return nil
}

// offer offers an announced file, now stored here, to local devices.
func (m *Manager) offer(s *session, w waitingAnnounce) {
	d := w.details
	route := "to groups " + strings.Join(w.groups, ", ")
	if len(w.dests.UIDs)+len(w.dests.Callsigns) > 0 {
		route = "to " + strings.Join(append(slices.Clone(w.dests.Callsigns), w.dests.UIDs...), ", ")
	}
	n := 0
	if m.cfg.OfferPackage != nil {
		n = m.cfg.OfferPackage(d.SHA256, firstNonEmpty(d.SenderCallsign, d.Name, s.fed.Name), w.dests.UIDs, w.dests.Callsigns, w.groups)
	}
	s.record("in", "b-f-t-r", d.SHA256, firstNonEmpty(d.Filename, d.Name), route+" ("+strconv.Itoa(n)+" devices)")
}

// sweepAnnouncements gives up on announced files that never arrived.
func (m *Manager) sweepAnnouncements() {
	m.mu.Lock()
	sessions := make([]*session, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}
	m.mu.Unlock()
	cutoff := time.Now().Add(-announceWait)
	for _, s := range sessions {
		var lost []waitingAnnounce
		s.mu.Lock()
		for h, w := range s.waiting {
			if w.at.Before(cutoff) {
				lost = append(lost, w)
				delete(s.waiting, h)
			}
		}
		s.mu.Unlock()
		for _, w := range lost {
			name := firstNonEmpty(w.details.Filename, w.details.Name, w.details.SHA256)
			s.setWarning("a data package (" + name + ") was sent to devices here, but its file never arrived. " +
				"On a TAK Server, the uploader's groups must be shared with this federate, and mission federation must be allowed.")
			s.record("in", "data package missing", w.details.SHA256, name, "")
		}
	}
}

// writeFile stores data as dir/name, atomically.
func writeFile(dir, name string, data []byte) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "federated-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(data)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, name))
}

func sizeText(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d bytes", n)
}
