// Command package builds the extension for each platform in plugin.json's
// run and zips it as dist/<id>-<version>.zip; with -install it also
// installs it on an EdgeTAK server.
//
//	go run ./package
//	go run ./package -install https://myserver:8443 -user admin -insecure
//
// -install asks for the admin's password (or reads EDGETAK_PASSWORD). An
// installed extension is updated and restarted if it was running. EdgeTAK's
// own certificate isn't trusted by Go: pass -ca with the server's CA (the
// console's Certificates page has it) or, on a test server, -insecure.
// Accounts with two-factor login can't install this way; use the console.
package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type manifest struct {
	ID      string              `json:"id"`
	Version string              `json:"version"`
	Run     map[string][]string `json:"run"`
}

func main() {
	install := flag.String("install", "", "the server's console address, e.g. https://myserver:8443")
	user := flag.String("user", "admin", "an administrator")
	caFile := flag.String("ca", "", "the server's CA certificate (PEM), to trust its certificate")
	insecure := flag.Bool("insecure", false, "don't check the server's certificate (test servers)")
	flag.Parse()
	log.SetFlags(0)

	b, err := os.ReadFile("plugin.json")
	if err != nil {
		log.Fatal("run this in the extension's folder: ", err)
	}
	var m manifest
	if err := json.Unmarshal(b, &m); err != nil {
		log.Fatal("plugin.json: ", err)
	}

	// One binary per "os/arch" key of run, where its first word says.
	os.RemoveAll("bin")
	for platform, cmd := range m.Run {
		goos, goarch, ok := strings.Cut(platform, "/")
		if !ok {
			continue
		}
		build := exec.Command("go", "build", "-trimpath", "-ldflags=-s -w", "-o", cmd[0], ".")
		build.Env = append(os.Environ(), "GOOS="+goos, "GOARCH="+goarch, "CGO_ENABLED=0")
		build.Stdout, build.Stderr = os.Stdout, os.Stderr
		if err := build.Run(); err != nil {
			log.Fatalf("building for %s: %v", platform, err)
		}
		fmt.Println("built", cmd[0])
	}

	out := filepath.Join("dist", m.ID+"-"+m.Version+".zip")
	if err := writeZip(out); err != nil {
		log.Fatal(err)
	}
	fmt.Println("packaged", out)
	if *install != "" {
		if err := upload(*install, *user, *caFile, *insecure, out); err != nil {
			log.Fatal("install failed: ", err)
		}
	}
}

// writeZip packs plugin.json, bin/ and anything else the extension reads at
// run time (put it in assets/); the page is built into the binary.
func writeZip(out string) error {
	os.MkdirAll(filepath.Dir(out), 0o755)
	f, err := os.Create(out)
	if err != nil {
		return err
	}
	zw := zip.NewWriter(f)
	for _, root := range []string{"plugin.json", "bin", "assets", "LICENSE"} {
		filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			info, _ := d.Info()
			h, _ := zip.FileInfoHeader(info)
			h.Name, h.Method = filepath.ToSlash(p), zip.Deflate
			if strings.HasPrefix(h.Name, "bin/") {
				h.SetMode(0o755)
			}
			w, err := zw.CreateHeader(h)
			if err != nil {
				return err
			}
			src, err := os.Open(p)
			if err != nil {
				return err
			}
			defer src.Close()
			_, err = io.Copy(w, src)
			return err
		})
	}
	if err := zw.Close(); err != nil {
		return err
	}
	return f.Close()
}

func upload(server, user, caFile string, insecure bool, zipPath string) error {
	password := os.Getenv("EDGETAK_PASSWORD")
	if password == "" {
		fmt.Printf("%s's password on %s (shown as you type; or set EDGETAK_PASSWORD): ", user, server)
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		password = strings.TrimRight(line, "\r\n")
	}
	tc := &tls.Config{InsecureSkipVerify: insecure}
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return err
		}
		tc.RootCAs = x509.NewCertPool()
		tc.RootCAs.AppendCertsFromPEM(pem)
	}
	body, err := os.ReadFile(zipPath)
	if err != nil {
		return err
	}
	req, err := http.NewRequest("POST", strings.TrimRight(server, "/")+"/admin/api/extensions", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.SetBasicAuth(user, password)
	req.Header.Set("Content-Type", "application/zip")
	req.Header.Set("X-EdgeTAK", "1")
	client := &http.Client{Timeout: 2 * time.Minute, Transport: &http.Transport{TLSClientConfig: tc}}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var res struct {
		Error     string
		Updated   bool
		Extension struct {
			Name, Version string
			Enabled       bool
		}
	}
	json.NewDecoder(resp.Body).Decode(&res)
	if resp.StatusCode != http.StatusOK {
		if res.Error == "" {
			res.Error = resp.Status
		}
		return fmt.Errorf("%s", res.Error)
	}
	x := res.Extension
	if res.Updated {
		fmt.Printf("updated %s to %s\n", x.Name, x.Version)
	} else {
		fmt.Printf("installed %s %s: on the Extensions page, choose its groups and start it\n", x.Name, x.Version)
	}
	return nil
}
