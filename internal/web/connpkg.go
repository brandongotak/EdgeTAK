package web

import (
	"archive/zip"
	"bytes"
	"crypto/rand"
	"fmt"
	"net"
	"net/http"
	"strings"
	"text/template"
	"time"
)

// connectionPackage builds an ATAK/WinTAK data package that adds this server
// as a streaming connection. In "ssl" mode it carries the CA truststore and
// enables certificate enrollment, so the client only has to enter its
// password; in "tcp" mode it configures the plaintext port.
func (s *Server) connectionPackage(w http.ResponseWriter, r *http.Request, _ *identity) {
	q := r.URL.Query()
	mode := firstNonEmpty(q.Get("mode"), "ssl")
	host := firstNonEmpty(q.Get("host"), s.Cfg.PublicHost)
	username := q.Get("username")
	if host == "" {
		jsonError(w, http.StatusBadRequest, "public_host is not configured; pass ?host=")
		return
	}
	if mode != "ssl" && mode != "tcp" {
		jsonError(w, http.StatusBadRequest, "mode must be ssl or tcp")
		return
	}
	data, err := s.buildConnectionPackage(mode, host, username)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	name := fmt.Sprintf("%s-%s-%s.zip", sanitize(s.Cfg.ServerName), sanitize(host), mode)
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Write(data)
}

var prefTmpl = template.Must(template.New("pref").Parse(`<?xml version='1.0' encoding='ASCII' standalone='yes'?>
<preferences>
  <preference version="1" name="cot_streams">
    <entry key="count" class="class java.lang.Integer">1</entry>
    <entry key="description0" class="class java.lang.String">{{.Name}}</entry>
    <entry key="enabled0" class="class java.lang.Boolean">true</entry>
    <entry key="connectString0" class="class java.lang.String">{{.Connect}}</entry>
{{- if .SSL}}
    <entry key="caLocation0" class="class java.lang.String">cert/{{.Truststore}}</entry>
    <entry key="caPassword0" class="class java.lang.String">{{.Password}}</entry>
    <entry key="enrollForCertificateWithTrust0" class="class java.lang.Boolean">true</entry>
    <entry key="useAuth0" class="class java.lang.Boolean">true</entry>
    <entry key="cacheCreds0" class="class java.lang.String">Cache credentials</entry>
{{- if .Username}}
    <entry key="username0" class="class java.lang.String">{{.Username}}</entry>
{{- end}}
{{- end}}
  </preference>
  <preference version="1" name="com.atakmap.app_preferences">
    <entry key="displayServerConnectionWidget" class="class java.lang.Boolean">true</entry>
{{- if .SSL}}
    <entry key="clientPassword" class="class java.lang.String">{{.Password}}</entry>
    <entry key="caPassword" class="class java.lang.String">{{.Password}}</entry>
    <entry key="caLocation" class="class java.lang.String">cert/{{.Truststore}}</entry>
{{- end}}
  </preference>
</preferences>
`))

var manifestTmpl = template.Must(template.New("manifest").Parse(`<MissionPackageManifest version="2">
  <Configuration>
    <Parameter name="uid" value="{{.UID}}"/>
    <Parameter name="name" value="{{.Name}}"/>
    <Parameter name="onReceiveDelete" value="true"/>
  </Configuration>
  <Contents>
    <Content ignore="false" zipEntry="certs/server.pref"/>
{{- if .SSL}}
    <Content ignore="false" zipEntry="certs/{{.Truststore}}"/>
{{- end}}
  </Contents>
</MissionPackageManifest>
`))

func (s *Server) buildConnectionPackage(mode, host, username string) ([]byte, error) {
	ssl := mode == "ssl"
	addr := s.Addrs["tcp"]
	if ssl {
		addr = s.Addrs["tls"]
	}
	if addr == "" {
		return nil, fmt.Errorf("the %s listener is disabled", mode)
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	vals := map[string]any{
		"Name":       xmlEscape(s.Cfg.ServerName),
		"Connect":    xmlEscape(fmt.Sprintf("%s:%s:%s", host, port, mode)),
		"SSL":        ssl,
		"Truststore": "truststore-" + sanitize(s.Cfg.ServerName) + ".p12",
		"Password":   xmlEscape(s.Cfg.TruststorePassword),
		"Username":   xmlEscape(username),
		"UID":        uuid(),
	}

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	add := func(name string, render func(f *bytes.Buffer) error) error {
		var b bytes.Buffer
		if err := render(&b); err != nil {
			return err
		}
		f, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate, Modified: time.Now()})
		if err != nil {
			return err
		}
		_, err = f.Write(b.Bytes())
		return err
	}
	if err := add("MANIFEST/manifest.xml", func(b *bytes.Buffer) error { return manifestTmpl.Execute(b, vals) }); err != nil {
		return nil, err
	}
	if err := add("certs/server.pref", func(b *bytes.Buffer) error { return prefTmpl.Execute(b, vals) }); err != nil {
		return nil, err
	}
	if ssl {
		p12, err := s.CA.TrustStoreP12(s.Cfg.TruststorePassword)
		if err != nil {
			return nil, err
		}
		if err := add("certs/"+vals["Truststore"].(string), func(b *bytes.Buffer) error { _, err := b.Write(p12); return err }); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '.' {
			return r
		}
		return '_'
	}, s)
}

func uuid() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
