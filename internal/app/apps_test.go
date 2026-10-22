package app

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"image"
	"image/color"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"strings"
	"testing"
)

// TestUpdateServer covers ATAK's update server: an admin uploads an APK,
// devices read the index and download it, and their device profile points
// ATAK at it.
func TestUpdateServer(t *testing.T) {
	a := startApp(t)
	if _, err := a.Store.CreateUser("cyan1", "password-cyan1", []string{"Cyan"}, false); err != nil {
		t.Fatal(err)
	}
	call, hc := adminJSON(t, a)
	base := "https://" + a.Addrs["https"]
	apk, err := os.ReadFile("../apps/testdata/helloworld.apk")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(apk)
	hash := hex.EncodeToString(sum[:])

	// Upload: one APK and one file that isn't one.
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("apk", "helloworld.apk")
	fw.Write(apk)
	fw, _ = mw.CreateFormFile("apk", "notes.apk")
	io.WriteString(fw, "not an apk")
	mw.Close()
	req, _ := http.NewRequest("POST", base+"/admin/api/apps", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-EdgeTAK", "1")
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	up, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(up), `"com.example.helloworld"`) || !strings.Contains(string(up), "notes.apk: not an Android app") {
		t.Fatalf("upload = %d %s", resp.StatusCode, up)
	}

	device := newAPI(t, a, enroll(t, a, "cyan1", "password-cyan1"))
	st, _, raw := device.do("GET", "/update/product.infz", nil)
	if st != 200 {
		t.Fatalf("product.infz = %d", st)
	}
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	for _, f := range zr.File {
		rc, _ := f.Open()
		b, _ := io.ReadAll(rc)
		rc.Close()
		files[f.Name] = string(b)
	}
	cols := strings.Split(strings.TrimSpace(files["product.inf"]), ",")
	if len(cols) != 13 || cols[1] != "app" || cols[2] != "com.example.helloworld" || cols[6] != "apk/"+hash+".apk" || cols[9] != hash {
		t.Errorf("product.inf = %q", files["product.inf"])
	}
	if files["icons/"+hash+".png"] == "" {
		t.Errorf("no icon in the index: %v", files)
	}
	// ATAK asks for its own version first: the apps and the plugins it loads.
	if st, _, raw := device.do("GET", "/update/5.7.0/product.inf", nil); st != 200 || !strings.Contains(string(raw), "com.example.helloworld") {
		t.Errorf("version index = %d %s", st, raw)
	}
	if st, _, _ := device.do("GET", "/update/nonsense/product.infz", nil); st != 404 {
		t.Errorf("unknown folder = %d, want 404", st)
	}
	st, _, raw = device.do("GET", "/update/apk/"+hash+".apk", nil)
	if st != 200 || !bytes.Equal(raw, apk) {
		t.Errorf("APK download = %d (%d bytes)", st, len(raw))
	}

	// Without a certificate ATAK is asked for a username and password.
	plain := &http.Client{Transport: memTransport(&tls.Config{RootCAs: a.CA.Pool()})}
	resp, _ = plain.Get(base + "/update/product.infz")
	resp.Body.Close()
	if resp.StatusCode != 401 || resp.Header.Get("WWW-Authenticate") == "" {
		t.Errorf("no credentials = %d", resp.StatusCode)
	}
	req, _ = http.NewRequest("GET", base+"/update/product.inf", nil)
	req.SetBasicAuth("cyan1", "password-cyan1")
	resp, _ = plain.Do(req)
	inf, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(inf), "com.example.helloworld") {
		t.Errorf("basic auth product.inf = %d %s", resp.StatusCode, inf)
	}

	// An admin's own icon replaces the APK's, in the index too.
	icon := image.NewRGBA(image.Rect(0, 0, 64, 64))
	icon.Set(1, 1, color.RGBA{255, 0, 0, 255})
	var iconPNG bytes.Buffer
	png.Encode(&iconPNG, icon)
	req, _ = http.NewRequest("PUT", base+"/admin/api/apps/"+hash+"/icon", bytes.NewReader(iconPNG.Bytes()))
	req.Header.Set("X-EdgeTAK", "1")
	resp, err = hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("change icon = %d", resp.StatusCode)
	}
	_, _, raw = device.do("GET", "/update/product.infz", nil)
	zr, _ = zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	for _, f := range zr.File {
		if f.Name == "icons/"+hash+".png" {
			rc, _ := f.Open()
			got, err := png.Decode(rc)
			rc.Close()
			if err != nil || got.Bounds().Dx() != 64 || color.RGBAModel.Convert(got.At(1, 1)) != (color.RGBA{255, 0, 0, 255}) {
				t.Errorf("index icon is not the new one: %v", err)
			}
		}
	}

	// The device profile turns the update server on.
	_, pf := profileFiles(t, device, "/Marti/api/tls/profile/enrollment?clientUid=ANDROID-cyan1")
	if p := pf["update-server.pref"]; !strings.Contains(p, `key="appMgmtEnableUpdateServer"`) || !strings.Contains(p, "/update</entry>") {
		t.Errorf("update-server.pref = %q", p)
	}
	if code, _ := call("PUT", "/admin/api/apps/settings", map[string]bool{"configure_devices": false}); code != 200 {
		t.Fatalf("settings = %d", code)
	}
	if _, pf := profileFiles(t, device, "/Marti/api/tls/profile/enrollment?clientUid=ANDROID-cyan1"); pf["update-server.pref"] != "" {
		t.Error("update server still sent after turning it off")
	}

	// Deleting removes it from the index and the disk.
	if code, _ := call("DELETE", "/admin/api/apps/"+hash, nil); code != 200 {
		t.Fatalf("delete = %d", code)
	}
	if st, _, _ := device.do("GET", "/update/apk/"+hash+".apk", nil); st != 404 {
		t.Errorf("deleted APK = %d", st)
	}
}
