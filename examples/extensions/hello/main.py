#!/usr/bin/env python3
"""Hello: an example EdgeTAK extension (Python 3, standard library only).

It reads the CoT of its groups, keeps a marker on the map where you put it
from its page, and shows what it receives. See docs/extensions.md.
"""

import datetime
import html
import json
import os
import threading
import time
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

API = os.environ["EDGETAK_URL"]
TOKEN = os.environ["EDGETAK_TOKEN"]
EXT_ID = os.environ["EDGETAK_EXTENSION_ID"]
DATA = os.environ["EDGETAK_DATA_DIR"]
PAGE_PORT = int(os.environ.get("EDGETAK_PAGE_PORT", "0"))
SETTINGS = os.path.join(DATA, "settings.json")

lock = threading.Lock()
state = {"received": 0, "recent": [], "sent": 0, "error": ""}


def api(method, path, body=None, timeout=30):
    """Calls EdgeTAK's extension API."""
    headers = {"Authorization": "Bearer " + TOKEN}
    if body is not None:
        headers["Content-Type"] = "application/xml"
    req = urllib.request.Request(API + path, data=body, method=method, headers=headers)
    return urllib.request.urlopen(req, timeout=timeout)


def load_settings():
    try:
        with open(SETTINGS, encoding="utf-8") as f:
            return json.load(f)
    except (OSError, ValueError):
        return {"callsign": "Hello marker", "lat": None, "lon": None}


def save_settings(s):
    with open(SETTINGS, "w", encoding="utf-8") as f:
        json.dump(s, f)


def read_stream():
    """Reads the CoT stream: one JSON object per line, blank lines to keep it open."""
    while True:
        try:
            with api("GET", "/api/v1/cot", timeout=90) as resp:
                print("reading the CoT stream", flush=True)
                for line in resp:
                    line = line.strip()
                    if not line:
                        continue
                    ev = json.loads(line)
                    with lock:
                        state["received"] += 1
                        state["recent"].insert(0, {k: ev.get(k) for k in ("time", "type", "uid", "callsign", "lat", "lon")})
                        del state["recent"][20:]
        except Exception as e:  # EdgeTAK restarting, or the network
            with lock:
                state["error"] = "stream: %s" % e
            print("stream:", e, flush=True)
            time.sleep(5)


def marker(s):
    now = datetime.datetime.now(datetime.timezone.utc)
    fmt = lambda t: t.strftime("%Y-%m-%dT%H:%M:%S.%fZ")
    return (
        '<event version="2.0" uid="%s" type="a-f-G-U-C" how="h-e" time="%s" start="%s" stale="%s">'
        '<point lat="%f" lon="%f" hae="0" ce="10" le="10"/>'
        '<detail><contact callsign="%s"/><remarks>Placed by the Hello extension</remarks></detail></event>'
    ) % ("hello-" + EXT_ID, fmt(now), fmt(now), fmt(now + datetime.timedelta(minutes=2)),
         s["lat"], s["lon"], html.escape(s["callsign"], quote=True))


def send_marker():
    """Sends the marker every 30 s (it goes stale after 2 minutes without one)."""
    while True:
        s = load_settings()
        if s.get("lat") is not None and s.get("lon") is not None:
            try:
                api("POST", "/api/v1/cot", marker(s).encode()).close()
                with lock:
                    state["sent"] += 1
            except Exception as e:
                with lock:
                    state["error"] = "send: %s" % e
                print("send:", e, flush=True)
        time.sleep(30)


PAGE = """<!doctype html><html><head><meta charset="utf-8"><title>Hello</title>
<meta name="viewport" content="width=device-width, initial-scale=1">
<style>
 :root { color-scheme: light dark; font: 14px system-ui, sans-serif; }
 body { margin: 16px; } table { border-collapse: collapse; width: 100%%; }
 td, th { text-align: left; padding: 4px 8px; border-bottom: 1px solid #8884; }
 input { padding: 4px; } .muted { opacity: .7; }
</style></head><body>
<p class="muted">Signed in to the console as <b>%s</b>. Devices: <span id="devices">…</span></p>
<h3>Marker</h3>
<form id="f"><input name="callsign" placeholder="Callsign"> <input name="lat" placeholder="Latitude" size="9">
 <input name="lon" placeholder="Longitude" size="9"> <button>Place marker</button></form>
<h3>Received <span id="count"></span></h3><p id="error" class="muted"></p>
<table><thead><tr><th>Time</th><th>Type</th><th>Callsign</th><th>Position</th></tr></thead><tbody id="recent"></tbody></table>
<script>
const f = document.getElementById('f');
async function refresh() {
  const s = await (await fetch('state')).json();
  document.getElementById('count').textContent = '(' + s.received + '; ' + s.sent + ' markers sent)';
  document.getElementById('error').textContent = s.error;
  document.getElementById('devices').textContent = s.devices;
  const tb = document.getElementById('recent');
  tb.replaceChildren(...s.recent.map((e) => {
    const tr = document.createElement('tr');
    for (const v of [e.time, e.type, e.callsign || e.uid, e.lat.toFixed(5) + ', ' + e.lon.toFixed(5)]) {
      const td = document.createElement('td'); td.textContent = v; tr.append(td);
    }
    return tr;
  }));
  if (!f.contains(document.activeElement)) {
    f.callsign.value = s.settings.callsign; f.lat.value = s.settings.lat ?? ''; f.lon.value = s.settings.lon ?? '';
  }
}
f.onsubmit = async (e) => {
  e.preventDefault();
  // Changes from a page need the X-EdgeTAK header, as in the console.
  await fetch('marker', { method: 'POST', headers: { 'Content-Type': 'application/json', 'X-EdgeTAK': '1' },
    body: JSON.stringify({ callsign: f.callsign.value, lat: parseFloat(f.lat.value), lon: parseFloat(f.lon.value) }) });
  document.activeElement.blur(); refresh();
};
refresh(); setInterval(refresh, 3000);
</script></body></html>"""


class Page(BaseHTTPRequestHandler):
    def send(self, status, ctype, body):
        self.send_response(status)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        path = self.path.split("?")[0]
        if path == "/":
            user = html.escape(self.headers.get("X-EdgeTAK-User", "?"))
            self.send(200, "text/html; charset=utf-8", (PAGE % user).encode())
        elif path == "/state":
            try:  # data:read lets it see the connected devices
                with api("GET", "/api/v1/clients") as r:
                    devices = len(json.load(r))
            except Exception:
                devices = "?"
            with lock:
                body = dict(state, settings=load_settings(), devices=devices)
            self.send(200, "application/json", json.dumps(body).encode())
        else:
            self.send(404, "text/plain", b"not found")

    def do_POST(self):
        if self.path != "/marker":
            return self.send(404, "text/plain", b"not found")
        req = json.loads(self.rfile.read(int(self.headers.get("Content-Length", 0))) or b"{}")
        try:
            s = {"callsign": str(req.get("callsign") or "Hello marker")[:40],
                 "lat": max(-90.0, min(90.0, float(req["lat"]))), "lon": max(-180.0, min(180.0, float(req["lon"])))}
        except (KeyError, TypeError, ValueError):
            return self.send(400, "application/json", b'{"error": "latitude and longitude are numbers"}')
        save_settings(s)
        api("POST", "/api/v1/cot", marker(s).encode()).close()
        with lock:
            state["sent"] += 1
        self.send(200, "application/json", json.dumps(s).encode())

    def log_message(self, *args):
        pass


if __name__ == "__main__":
    with api("GET", "/api/v1/info") as r:
        info = json.load(r)
    print("Hello %s on %s (EdgeTAK %s), groups in %s, out %s" % (
        info["version"], info["server_name"], info["edgetak_version"], info["groups_in"], info["groups_out"]), flush=True)
    threading.Thread(target=read_stream, daemon=True).start()
    threading.Thread(target=send_marker, daemon=True).start()
    ThreadingHTTPServer(("127.0.0.1", PAGE_PORT), Page).serve_forever()
