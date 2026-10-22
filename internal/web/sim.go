package web

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net"
	"net/http"
	"runtime"
	"sync"
	"time"

	"github.com/brandongotak/EdgeTAK/internal/pki"
	"github.com/brandongotak/EdgeTAK/internal/sim"
	"github.com/brandongotak/EdgeTAK/internal/store"
)

// Simulator (Server -> Simulator): load testing from the console. Simulated
// devices connect to this server's TLS streaming port as a temporary
// account (simUser) in a group of the admin's choosing (Simulator by
// default, so real devices don't see them), report positions and
// optionally chat. The account and its certificate are removed when the run
// ends. They share this machine's CPU with the server; `edgetak sim` runs
// the same simulator from another computer.

const (
	simUser       = "edgetak-sim"
	simGroup      = "Simulator"
	maxSimDevices = 5000
)

type simState struct {
	ctx       context.Context // the server's: runs end when it stops
	mu        sync.Mutex
	run       *sim.Run
	cfg       simRequest
	by        string
	madeGroup string     // the group this run created, removed afterwards if empty
	last      *sim.Stats // the previous run's result
}

type simRequest struct {
	Devices   int        `json:"devices"`
	Ramp      float64    `json:"ramp"`       // devices per second
	Interval  float64    `json:"interval"`   // seconds between positions
	ChatEvery float64    `json:"chat_every"` // seconds; 0 = no chat
	Minutes   float64    `json:"minutes"`    // 0 = until stopped
	Group     string     `json:"group"`      // default Simulator
	Center    [2]float64 `json:"center"`     // lat, lon
	Radius    float64    `json:"radius"`     // metres
}

// simStart removes a simulator account left by a run that didn't finish
// (EdgeTAK stopped mid-run).
func (s *Server) simStart(ctx context.Context) {
	s.sim.ctx = ctx
	if _, ok := s.Store.User(simUser); ok {
		s.removeUser(simUser)
		s.Log.Info("removed the simulator account left from an unfinished run")
	}
}

func (s *Server) simView() map[string]any {
	s.sim.mu.Lock()
	defer s.sim.mu.Unlock()
	v := map[string]any{"running": false, "max_devices": maxSimDevices}
	if s.sim.run != nil {
		st := s.sim.run.Stats()
		v["stats"], v["running"], v["config"], v["by"] = st, st.Running, s.sim.cfg, s.sim.by
	} else if s.sim.last != nil {
		v["stats"], v["config"], v["by"] = s.sim.last, s.sim.cfg, s.sim.by
	}
	// The server's side: its process and its whole traffic.
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	in, out, dropped := s.Router.Totals()
	srv := map[string]any{"goroutines": runtime.NumGoroutine(), "heap": m.HeapAlloc, "clients": len(s.Router.Clients()),
		"events_in": in, "events_out": out, "dropped": dropped}
	if s.Health != nil {
		if smp, ok := s.Health.Latest(); ok {
			srv["cpu"], srv["mem_used"], srv["mem_total"] = smp.CPU, smp.MemUsed, smp.MemTotal
		}
	}
	v["server"] = srv
	return v
}

func (s *Server) simRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/api/sim", s.admin(func(w http.ResponseWriter, r *http.Request, _ *identity) {
		writeJSON(w, http.StatusOK, s.simView())
	}))
	mux.HandleFunc("POST /admin/api/sim/start", s.admin(s.startSim))
	mux.HandleFunc("POST /admin/api/sim/stop", s.admin(func(w http.ResponseWriter, r *http.Request, id *identity) {
		s.sim.mu.Lock()
		run := s.sim.run
		s.sim.mu.Unlock()
		if run != nil {
			run.Stop()
			s.Log.Info("simulation stopped", "admin", id.Username)
		}
		s.finishSim(run)
		writeJSON(w, http.StatusOK, s.simView())
	}))
}

func (s *Server) startSim(w http.ResponseWriter, r *http.Request, id *identity) {
	var req simRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if req.Devices < 1 || req.Devices > maxSimDevices {
		jsonError(w, http.StatusBadRequest, "simulate 1 to 5000 devices")
		return
	}
	if !validLatLon(req.Center) || (req.Center == [2]float64{}) {
		jsonError(w, http.StatusBadRequest, "choose where the devices are (the map's centre)")
		return
	}
	req.Group = firstNonEmpty(req.Group, simGroup)
	if !groupNameRE.MatchString(req.Group) {
		jsonError(w, http.StatusBadRequest, "not a valid group name")
		return
	}
	_, port, err := net.SplitHostPort(s.Addrs["tls"])
	if err != nil {
		jsonError(w, http.StatusBadRequest, "the TLS streaming port isn't on")
		return
	}
	s.sim.mu.Lock()
	defer s.sim.mu.Unlock()
	if s.sim.run != nil {
		jsonError(w, http.StatusConflict, "a simulation is already running")
		return
	}
	// A temporary account in the chosen group, with its own certificate.
	s.sim.madeGroup = ""
	if _, ok := s.Store.Group(req.Group); !ok {
		if _, err := s.Store.CreateGroup(req.Group, "Simulated devices (Server → Simulator)"); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.sim.madeGroup = req.Group
	}
	if _, ok := s.Store.User(simUser); ok {
		s.removeUser(simUser)
	}
	if _, err := s.Store.CreateUser(simUser, randomToken(16), []string{req.Group}, false); err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	cert, key, err := s.CA.NewClient(simUser)
	if err == nil {
		err = s.Store.AddCert(store.Cert{Serial: pki.SerialHex(cert), CommonName: simUser, ClientUID: "simulator",
			Issued: time.Now().UTC(), Expires: cert.NotAfter.UTC()})
	}
	if err != nil {
		s.removeUser(simUser)
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	cfg := sim.Config{
		Addr: net.JoinHostPort("127.0.0.1", port),
		// Our own listener on this machine: its certificate needn't be checked.
		TLS:     &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{cert.Raw}, PrivateKey: key, Leaf: cert}}, InsecureSkipVerify: true},
		Devices: req.Devices, Ramp: req.Ramp, Interval: seconds(req.Interval), ChatEvery: seconds(req.ChatEvery),
		Duration: time.Duration(req.Minutes * float64(time.Minute)), Center: req.Center, Radius: req.Radius, Version: s.Version,
		Dial: s.SimDial,
	}
	run, err := sim.Start(s.sim.ctx, cfg)
	if err != nil {
		s.removeUser(simUser)
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.sim.run, s.sim.cfg, s.sim.by, s.sim.last = run, req, id.Username, nil
	s.Log.Warn("simulation started", "admin", id.Username, "devices", req.Devices, "group", req.Group, "interval", cfg.Interval, "minutes", req.Minutes)
	go func() {
		<-run.Done()
		s.finishSim(run)
	}()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func seconds(v float64) time.Duration { return time.Duration(v * float64(time.Second)) }

// finishSim removes a finished run's account (and the group it made, if
// nobody else is in it), keeping its result to show.
func (s *Server) finishSim(run *sim.Run) {
	if run == nil {
		return
	}
	s.sim.mu.Lock()
	defer s.sim.mu.Unlock()
	if s.sim.run != run {
		return // already finished
	}
	st := run.Stats()
	s.sim.last, s.sim.run = &st, nil
	s.removeUser(simUser)
	if g := s.sim.madeGroup; g != "" {
		empty := true
		for _, u := range s.Store.Users() {
			for _, ug := range u.Groups {
				empty = empty && ug != g
			}
		}
		if empty {
			s.Store.DeleteGroup(g)
		}
	}
	s.Log.Info("simulation finished", "devices", st.Devices, "sent", st.Sent, "received", st.Received,
		"failed", st.Failed, "dropped", st.Dropped, "latency_p95_ms", st.LatencyP95)
}
