package web

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"
)

// Certificate status: when the CA, the server's own certificates and the
// client certificates expire, so admins see trouble before devices drop off.

const certCheckEvery = 12 * time.Hour

// caWarnDays is the minimum warning for the CA: replacing it means every
// device must re-enroll, so it needs more notice than a client certificate.
const caWarnDays = 180

type serverCertView struct {
	Kind      string     `json:"kind"` // ca, server, public
	Name      string     `json:"name"`
	Subject   string     `json:"subject,omitempty"`
	Hosts     []string   `json:"hosts,omitempty"`
	Ports     string     `json:"ports"`
	Expires   *time.Time `json:"expires,omitempty"` // nil while a public certificate is pending
	DaysLeft  int        `json:"days_left"`
	State     string     `json:"state"` // valid, expiring, expired, pending
	Renewal   string     `json:"renewal"`
	AutoRenew bool       `json:"auto_renew"`
}

type clientCertView struct {
	Serial     string    `json:"serial"`
	CommonName string    `json:"common_name"`
	ClientUID  string    `json:"client_uid,omitempty"`
	Issued     time.Time `json:"issued"`
	Expires    time.Time `json:"expires"`
	Revoked    bool      `json:"revoked"`
	DaysLeft   int       `json:"days_left"`
	State      string    `json:"state"`            // valid, expiring, expired, revoked
	Replaced   bool      `json:"replaced"`         // a newer certificate exists for the same user and device
	InUse      []string  `json:"in_use,omitempty"` // callsigns connected with it now
	Attention  bool      `json:"attention"`        // expiring or expired and not replaced or revoked
}

type certReport struct {
	WarnDays int              `json:"warn_days"`
	Server   []serverCertView `json:"server"`
	Certs    []clientCertView `json:"certs"`
	Summary  map[string]int   `json:"summary"` // valid, expiring, expired, revoked, replaced, attention
}

func daysLeft(now, t time.Time) int { return int(math.Floor(t.Sub(now).Hours() / 24)) }

func expiryState(now, t time.Time, warnDays int) string {
	switch {
	case !t.After(now):
		return "expired"
	case t.Sub(now) <= days(warnDays):
		return "expiring"
	}
	return "valid"
}

func (s *Server) certReport() certReport {
	now := time.Now()
	warn := s.Store.CertWarnDays()
	rep := certReport{WarnDays: warn, Server: []serverCertView{}, Certs: []clientCertView{}, Summary: map[string]int{}}

	ports := func(names ...string) string {
		var out []string
		for _, n := range names {
			if a := s.Addrs[n]; a != "" {
				out = append(out, a[strings.LastIndex(a, ":")+1:])
			}
		}
		return strings.Join(out, ", ")
	}
	add := func(v serverCertView, t time.Time, warnDays int) {
		v.Expires, v.DaysLeft, v.State = &t, daysLeft(now, t), expiryState(now, t, warnDays)
		rep.Server = append(rep.Server, v)
	}
	ca := s.CA.Cert
	add(serverCertView{Kind: "ca", Name: "Certificate authority", Subject: ca.Subject.CommonName, Ports: "signs every device and server certificate",
		Renewal: "Not renewed automatically. Replacing the CA means every device has to enroll again."}, ca.NotAfter, max(warn, caWarnDays))
	if leaf := s.CA.ServerCert.Leaf; leaf != nil {
		hosts := append([]string{}, leaf.DNSNames...)
		for _, ip := range leaf.IPAddresses {
			hosts = append(hosts, ip.String())
		}
		add(serverCertView{Kind: "server", Name: "Server certificate", Subject: leaf.Subject.CommonName, Hosts: hosts, Ports: ports("tls", "https", "enroll"), AutoRenew: true,
			Renewal: "Renewed by EdgeTAK at startup once fewer than 30 days are left; restart EdgeTAK if it is closer than that."}, leaf.NotAfter, warn)
	}
	if info := s.Public.Info(); info["mode"] == "letsencrypt" || info["mode"] == "files" {
		v := serverCertView{Kind: "public", Name: "Public certificate", Ports: ports("enroll")}
		if d, ok := info["domains"].([]string); ok {
			v.Hosts = d
		}
		if is, ok := info["issuer"].(string); ok {
			v.Subject = "issued by " + is
		}
		pubWarn := warn
		if info["mode"] == "letsencrypt" {
			// Let's Encrypt renews with 30 days left, so less than that means renewal is failing.
			v.AutoRenew, v.Renewal, pubWarn = true, "Let's Encrypt: EdgeTAK renews it automatically about 30 days before it expires.", min(warn, 20)
		} else {
			v.Renewal = "Certificate files: renew them with your tool (such as certbot); EdgeTAK reloads them when they change."
		}
		if t, ok := info["not_after"].(time.Time); ok {
			add(v, t, pubWarn)
		} else {
			v.State = "pending"
			rep.Server = append(rep.Server, v)
		}
	}

	inUse := map[string][]string{}
	for _, c := range s.Router.Clients() {
		if c.CertSerial != "" {
			_, cs := c.Identity()
			if cs == "" {
				cs = c.Username
			}
			inUse[c.CertSerial] = append(inUse[c.CertSerial], cs)
		}
	}
	certs := s.Store.Certs() // newest first
	newest := map[string]string{}
	for _, c := range certs {
		if k := c.CommonName + "\x00" + c.ClientUID; newest[k] == "" {
			newest[k] = c.Serial
		}
	}
	for _, c := range certs {
		v := clientCertView{Serial: c.Serial, CommonName: c.CommonName, ClientUID: c.ClientUID, Issued: c.Issued, Expires: c.Expires, Revoked: c.Revoked,
			DaysLeft: daysLeft(now, c.Expires), State: expiryState(now, c.Expires, warn), InUse: inUse[c.Serial]}
		if c.Revoked {
			v.State = "revoked"
		}
		v.Replaced = newest[c.CommonName+"\x00"+c.ClientUID] != c.Serial
		// A certificate still in use matters even when a newer one exists.
		v.Attention = (v.State == "expiring" || v.State == "expired") && (!v.Replaced || len(v.InUse) > 0)
		rep.Summary[v.State]++
		if v.Replaced {
			rep.Summary["replaced"]++
		}
		if v.Attention {
			rep.Summary["attention"]++
		}
		rep.Certs = append(rep.Certs, v)
	}
	return rep
}

// certIssues are the certificate warnings shown with the server's health.
func (s *Server) certIssues(rep certReport) []issue {
	out := []issue{}
	for _, c := range rep.Server {
		switch c.State {
		case "expired":
			out = append(out, issue{"critical", fmt.Sprintf("%s expired %s: devices cannot connect over TLS", c.Name, dayText(-c.DaysLeft, "ago"))})
		case "expiring":
			level := "warning"
			if c.DaysLeft < 7 {
				level = "critical"
			}
			out = append(out, issue{level, fmt.Sprintf("%s expires in %s. %s", c.Name, dayText(c.DaysLeft, ""), c.Renewal)})
		}
	}
	expiring, expired, live := 0, 0, 0
	for _, c := range rep.Certs {
		if !c.Attention {
			continue
		}
		if c.State == "expired" {
			expired++
		} else {
			expiring++
		}
		if len(c.InUse) > 0 {
			live++
		}
	}
	if expiring > 0 {
		verb := "expire"
		if expiring == 1 {
			verb = "expires"
		}
		text := fmt.Sprintf("%d device certificate%s %s within %d days", expiring, plural(expiring), verb, rep.WarnDays)
		if live > 0 {
			text += fmt.Sprintf(" (%d in use now)", live)
		}
		out = append(out, issue{"warning", text + ": see Certificates"})
	}
	if expired > 0 {
		out = append(out, issue{"info", fmt.Sprintf("%d device certificate%s expired and not replaced: those devices must enroll again", expired, plural(expired))})
	}
	return out
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func dayText(n int, suffix string) string {
	t := fmt.Sprintf("%d day%s", n, plural(n))
	if n == 0 {
		t = "less than a day"
	}
	if suffix != "" {
		t += " " + suffix
	}
	return t
}

func (s *Server) certStatusRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/api/certs/status", s.admin(func(w http.ResponseWriter, r *http.Request, _ *identity) {
		rep := s.certReport()
		writeJSON(w, http.StatusOK, map[string]any{"report": rep, "issues": s.certIssues(rep)})
	}))
	mux.HandleFunc("PUT /admin/api/certs/settings", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		var req struct {
			WarnDays int `json:"warn_days"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&req); err != nil || req.WarnDays < 1 || req.WarnDays > 365 {
			jsonError(w, http.StatusBadRequest, "warn_days must be 1 to 365")
			return
		}
		if err := s.Store.SetCertWarnDays(req.WarnDays); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.Log.Info("certificate warning period changed", "admin", id.Username, "days", req.WarnDays)
		w.WriteHeader(http.StatusNoContent)
	}))
}

// certWatchStart logs certificate warnings at startup and twice a day, so
// they reach the system log as well as the console.
func (s *Server) certWatchStart(ctx context.Context) {
	go func() {
		t := time.NewTicker(certCheckEvery)
		defer t.Stop()
		for {
			for _, i := range s.certIssues(s.certReport()) {
				if i.Level == "info" {
					continue
				}
				s.Log.Warn("certificate: " + i.Text)
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
}
