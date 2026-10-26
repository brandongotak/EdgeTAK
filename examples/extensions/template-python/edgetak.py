"""A small client for EdgeTAK's extension API (Python 3.8+, standard library only).

Copy this file into your extension as it is. See docs/extensions.md in the
EdgeTAK repository for the API it wraps.
"""

import datetime
import json
import os
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
from xml.sax.saxutils import quoteattr


class APIError(Exception):
    """EdgeTAK answered with an error ({"error": ...})."""

    def __init__(self, status, message):
        super().__init__("%d: %s" % (status, message))
        self.status = status


class EdgeTAK:
    """The extension's connection to EdgeTAK, set up from the environment."""

    def __init__(self):
        self.url = os.environ["EDGETAK_URL"]
        self.token = os.environ["EDGETAK_TOKEN"]
        self.id = os.environ["EDGETAK_EXTENSION_ID"]
        self.data_dir = os.environ["EDGETAK_DATA_DIR"]
        self.server_name = os.environ.get("EDGETAK_SERVER_NAME", "")
        self.page_port = int(os.environ.get("EDGETAK_PAGE_PORT", "0"))

    def _request(self, method, path, body=None, content_type=None, timeout=30):
        headers = {"Authorization": "Bearer " + self.token}
        if content_type:
            headers["Content-Type"] = content_type
        req = urllib.request.Request(self.url + path, data=body, method=method, headers=headers)
        try:
            return urllib.request.urlopen(req, timeout=timeout)
        except urllib.error.HTTPError as e:
            try:
                msg = json.load(e).get("error", e.reason)
            except ValueError:
                msg = e.reason
            raise APIError(e.code, msg) from None

    def get(self, path, **params):
        """GETs a JSON API path, e.g. get("/api/v1/clients")."""
        if params:
            path += "?" + urllib.parse.urlencode(params)
        with self._request("GET", path) as r:
            return json.load(r)

    def info(self):
        """The extension's id, version, permissions and groups, and the server's name and version."""
        return self.get("/api/v1/info")

    def send(self, *events):
        """Sends CoT events (XML strings, e.g. from marker()) to the extension's out groups."""
        body = "".join(events).encode("utf-8")
        with self._request("POST", "/api/v1/cot", body, "application/xml") as r:
            return json.load(r)["sent"]

    def stream(self, on_event, types=None, on_error=None):
        """Calls on_event(event) for each CoT event of the extension's in groups,
        reconnecting whenever the connection drops. Runs in a background thread.

        event is a dict: uid, type, how, time, stale, lat, lon, hae, callsign, xml.
        types limits it to types starting with these prefixes, e.g. ["a-f", "b-t-f"].
        """

        def run():
            path = "/api/v1/cot"
            if types:
                path += "?" + urllib.parse.urlencode({"types": ",".join(types)})
            while True:
                try:
                    # A blank line comes every 30 s, so a longer silence means the connection is gone.
                    with self._request("GET", path, timeout=90) as r:
                        for line in r:
                            line = line.strip()
                            if line:
                                on_event(json.loads(line))
                except Exception as e:  # EdgeTAK restarting, or a bug in on_event
                    if on_error:
                        on_error(e)
                time.sleep(5)

        t = threading.Thread(target=run, name="cot-stream", daemon=True)
        t.start()
        return t


def iso(t):
    """A datetime as CoT time."""
    return t.astimezone(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%S.%fZ")


def marker(uid, lat, lon, callsign, cot_type="a-f-G-U-C", stale_seconds=120, hae=0, remarks="", how="h-e"):
    """A CoT position event. Send it again before stale_seconds pass, or it disappears from the map."""
    now = datetime.datetime.now(datetime.timezone.utc)
    detail = "<contact callsign=%s/>" % quoteattr(callsign)
    if remarks:
        detail += "<remarks>%s</remarks>" % remarks.replace("&", "&amp;").replace("<", "&lt;")
    return (
        '<event version="2.0" uid=%s type=%s how=%s time="%s" start="%s" stale="%s">'
        '<point lat="%.7f" lon="%.7f" hae="%.1f" ce="10" le="10"/><detail>%s</detail></event>'
    ) % (quoteattr(uid), quoteattr(cot_type), quoteattr(how), iso(now), iso(now),
         iso(now + datetime.timedelta(seconds=stale_seconds)), lat, lon, hae, detail)


class Settings:
    """Settings kept as JSON in the extension's data folder, which updates keep."""

    def __init__(self, data_dir, defaults):
        self.path = os.path.join(data_dir, "settings.json")
        self.defaults = dict(defaults)
        self.lock = threading.Lock()

    def load(self):
        with self.lock:
            s = dict(self.defaults)
            try:
                with open(self.path, encoding="utf-8") as f:
                    s.update(json.load(f))
            except (OSError, ValueError):
                pass
            return s

    def save(self, values):
        with self.lock:
            tmp = self.path + ".tmp"
            with open(tmp, "w", encoding="utf-8") as f:
                json.dump(values, f, indent=1)
            os.replace(tmp, self.path)
