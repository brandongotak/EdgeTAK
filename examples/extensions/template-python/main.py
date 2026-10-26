#!/usr/bin/env python3
"""My Extension: a starting point for an EdgeTAK extension.

As it comes, it counts the CoT it receives, keeps a marker on the map where
an admin puts it on its page, and shows its status there. Replace the parts
marked TODO with your own. See README.md.
"""

import collections
import json
import os
import signal
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from edgetak import APIError, EdgeTAK, Settings, marker

tak = EdgeTAK()
PAGE_DIR = os.path.join(os.path.dirname(os.path.abspath(__file__)), "page")

# TODO: your settings, with their defaults. The page edits them.
settings = Settings(tak.data_dir, {
    "marker_on": False,
    "callsign": "My marker",
    "lat": None,
    "lon": None,
    "interval": 30,
})

lock = threading.Lock()
status = {
    "started": time.time(),
    "received": 0,
    "types": collections.Counter(),
    "recent": collections.deque(maxlen=20),
    "sent": 0,
    "error": "",
}


def log(*args):
    """What the extension prints shows under Log on the Extensions page."""
    print(*args, flush=True)


def error(where, e):
    with lock:
        status["error"] = "%s: %s (%s)" % (where, e, time.strftime("%H:%M:%S"))
    log(where + ":", e)


# ---- CoT in ----

def on_event(ev):
    """Called for each CoT event of the extension's in groups.

    TODO: your logic: forward it somewhere, record it, react to it.
    """
    with lock:
        status["received"] += 1
        status["types"][ev["type"][:5]] += 1
        status["recent"].appendleft({k: ev.get(k) for k in ("time", "type", "uid", "callsign", "lat", "lon")})


# ---- CoT out ----

def send_marker(s):
    if s["lat"] is None or s["lon"] is None:
        return
    # TODO: send your own markers. Positions stay on the map until their
    # stale time, and devices connecting later get the latest of each uid.
    tak.send(marker("%s-marker" % tak.id, s["lat"], s["lon"], s["callsign"],
                    stale_seconds=max(60, 3 * s["interval"]), remarks="Placed by %s" % tak.id))
    with lock:
        status["sent"] += 1


def worker():
    """Runs every few seconds. TODO: poll an outside service, send what it reports."""
    while True:
        s = settings.load()
        if s["marker_on"]:
            try:
                send_marker(s)
            except Exception as e:
                error("send", e)
        time.sleep(max(5, s["interval"]))


# ---- the page ----

def check_settings(new):
    """Checks settings from the page; raises ValueError with a message for the admin."""
    s = settings.load()
    s["marker_on"] = bool(new.get("marker_on"))
    s["callsign"] = str(new.get("callsign") or "").strip()[:40] or "My marker"
    lat, lon = new.get("lat"), new.get("lon")
    if lat in (None, "") or lon in (None, ""):
        s["lat"] = s["lon"] = None
    else:
        s["lat"], s["lon"] = float(lat), float(lon)
        if not (-90 <= s["lat"] <= 90 and -180 <= s["lon"] <= 180):
            raise ValueError("latitude is -90 to 90 and longitude -180 to 180")
    s["interval"] = int(new.get("interval") or 30)
    if not 5 <= s["interval"] <= 3600:
        raise ValueError("the interval is 5 to 3600 seconds")
    if s["marker_on"] and s["lat"] is None:
        raise ValueError("set a position for the marker")
    return s


class Page(BaseHTTPRequestHandler):
    """The page the console shows at /admin/ext/<id>/. EdgeTAK strips that
    prefix, so this sees "/", "/api/status"... Only signed-in admins get
    here; X-EdgeTAK-User says which."""

    def reply(self, code, body, ctype="application/json"):
        if not isinstance(body, bytes):
            body = json.dumps(body).encode()
        self.send_response(code)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Cache-Control", "no-cache")
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        path = self.path.split("?")[0]
        if path == "/api/status":
            with lock:
                st = dict(status, types=dict(status["types"]), recent=list(status["recent"]))
            st["settings"] = settings.load()
            st["user"] = self.headers.get("X-EdgeTAK-User", "")
            try:  # needs data:read
                st["devices"] = len(tak.get("/api/v1/clients"))
            except (APIError, OSError):
                st["devices"] = None
            return self.reply(200, st)
        # Static files from page/ ("/" is index.html).
        name = "index.html" if path == "/" else path.lstrip("/")
        full = os.path.realpath(os.path.join(PAGE_DIR, name))
        if not full.startswith(os.path.realpath(PAGE_DIR) + os.sep) or not os.path.isfile(full):
            return self.reply(404, {"error": "not found"})
        ctype = {".html": "text/html; charset=utf-8", ".js": "text/javascript", ".css": "text/css",
                 ".png": "image/png", ".svg": "image/svg+xml"}.get(os.path.splitext(full)[1], "application/octet-stream")
        with open(full, "rb") as f:
            self.reply(200, f.read(), ctype)

    def do_PUT(self):
        if self.path != "/api/settings":
            return self.reply(404, {"error": "not found"})
        try:
            new = json.loads(self.rfile.read(int(self.headers.get("Content-Length", 0))) or b"{}")
            s = check_settings(new)
        except (ValueError, TypeError) as e:
            return self.reply(400, {"error": str(e)})
        settings.save(s)
        log("settings changed by", self.headers.get("X-EdgeTAK-User", "?"))
        if s["marker_on"]:
            try:
                send_marker(s)
            except Exception as e:
                error("send", e)
        self.reply(200, s)

    def log_message(self, *args):
        pass  # don't fill the log with every request


def main():
    signal.signal(signal.SIGTERM, lambda *_: sys.exit(0))  # EdgeTAK stopping it
    info = tak.info()
    log("%s %s on %s (EdgeTAK %s): in %s, out %s" % (info["name"], info["version"], info["server_name"],
        info["edgetak_version"], info["groups_in"] or "no groups", info["groups_out"] or "no groups"))
    if "cot:read" in info["permissions"]:
        tak.stream(on_event, on_error=lambda e: error("stream", e))
    threading.Thread(target=worker, name="worker", daemon=True).start()
    if tak.page_port:
        ThreadingHTTPServer(("127.0.0.1", tak.page_port), Page).serve_forever()
    else:
        threading.Event().wait()


if __name__ == "__main__":
    main()
