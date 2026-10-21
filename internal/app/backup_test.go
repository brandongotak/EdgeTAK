package app

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/brandongotak/EdgeTAK/internal/backup"
)

// TestBackupRestore covers the console's backup endpoints: a download is a
// complete backup, one can be kept on the server, and a restore is checked
// and staged for the next start.
func TestBackupRestore(t *testing.T) {
	a := startApp(t)
	if _, err := a.Store.CreateUser("alice", "password-alice", nil, false); err != nil {
		t.Fatal(err)
	}
	call, hc := adminJSON(t, a)
	base := "https://" + a.Addrs["https"]

	resp, err := hc.Get(base + "/admin/api/backup/download?history=1")
	if err != nil {
		t.Fatal(err)
	}
	file, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	m, err := backup.Check(bytes.NewReader(file))
	if resp.StatusCode != 200 || err != nil || m.ServerName != a.Cfg.ServerName || !m.History {
		t.Fatalf("download = %d %+v %v", resp.StatusCode, m, err)
	}

	if code, v := call("POST", "/admin/api/backups", nil); code != 200 || len(v["backups"].([]any)) != 1 {
		t.Fatalf("back up now = %d %v", code, v)
	}
	if code, _ := call("PUT", "/admin/api/backup/settings", map[string]any{"auto": true, "keep": 0}); code != 400 {
		t.Errorf("keep 0 accepted: %d", code)
	}

	// A file that isn't a backup is refused and nothing is staged.
	req, _ := http.NewRequest("POST", base+"/admin/api/backup/restore", bytes.NewReader([]byte("not a backup")))
	req.Header.Set("X-EdgeTAK", "1")
	resp, _ = hc.Do(req)
	resp.Body.Close()
	if resp.StatusCode != 400 || backup.Pending(a.DataDir) {
		t.Errorf("bad restore = %d, pending %v", resp.StatusCode, backup.Pending(a.DataDir))
	}
	req, _ = http.NewRequest("POST", base+"/admin/api/backup/restore", bytes.NewReader(file))
	req.Header.Set("X-EdgeTAK", "1")
	resp, _ = hc.Do(req)
	var r struct {
		Restarting bool            `json:"restarting"`
		Manifest   backup.Manifest `json:"manifest"`
	}
	json.NewDecoder(resp.Body).Decode(&r)
	resp.Body.Close()
	if resp.StatusCode != 200 || r.Manifest.ServerName != a.Cfg.ServerName || !backup.Pending(a.DataDir) {
		t.Errorf("restore = %d %+v, pending %v", resp.StatusCode, r, backup.Pending(a.DataDir))
	}
}
