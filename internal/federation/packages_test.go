package federation

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestROLRoundTrip(t *testing.T) {
	in := &ROL{Program: "create resource\n{};", Payload: [][]byte{[]byte("PK file")}, FederateGroups: []string{"Cyan"},
		Provenance: []Provenance{{ServerID: "a", ServerName: "A"}}}
	var out ROL
	if err := out.unmarshal(in.marshal(nil)); err != nil {
		t.Fatal(err)
	}
	if out.Program != in.Program || len(out.Payload) != 1 || !bytes.Equal(out.Payload[0], in.Payload[0]) ||
		len(out.FederateGroups) != 1 || !hasProvenance(out.Provenance, "a") {
		t.Errorf("got %+v", out)
	}
}

// TAK Server's programs, as its MissionActionROLConverter and
// GroupFederationUtil write them.
func TestParseTAKServerROL(t *testing.T) {
	op, res, params := parseROL("create resource\n" + `{"filename":"a.zip","keywords":["missionpackage"],"mimeType":"application/x-zip-compressed","name":"a.zip",` +
		`"submissionTime":"2026-10-21T18:13:27.5Z","submitter":"tuser","uid":"u1","creatorUid":"ANDROID-1","hash":"ABC","size":3,"tool":"public","latitude":1.5}` + ";")
	var r rolResource
	if op != "create" || res != "resource" || json.Unmarshal(params, &r) != nil || r.Hash != "ABC" || r.Submitter != "tuser" {
		t.Errorf("resource: %s %s %+v", op, res, r)
	}
	op, res, params = parseROL("announce package\n" + `{"type":"ResourceDetails","sha256":"abc","senderUid":"{\"callsigns\":[\"BOB\"],\"uids\":[]}",` +
		`"name":"a.zip","filename":"a.zip","senderUrl":"","tsStored":1700000000000,"sizeInBytes":441359,"senderCallsign":"ALICE"}` + ";")
	var d rolDetails
	var to rolDestinations
	if op != "announce" || res != "package" || json.Unmarshal(params, &d) != nil || json.Unmarshal([]byte(d.SenderUID), &to) != nil ||
		d.Size != 441359 || len(to.Callsigns) != 1 || to.Callsigns[0] != "BOB" {
		t.Errorf("announce: %s %s %+v %+v", op, res, d, to)
	}
}

// What EdgeTAK sends must parse under TAK Server's ROL grammar, which has
// no null.
func TestROLHasNoNull(t *testing.T) {
	p, err := rolProgram("create", "resource", rolResource{Keywords: []string{}, Hash: "abc"})
	if err != nil || strings.Contains(p, "null") || !strings.HasPrefix(p, "create resource\n{") || !strings.HasSuffix(p, "};") {
		t.Errorf("program = %q", p)
	}
}
