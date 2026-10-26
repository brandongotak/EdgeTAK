#!/usr/bin/env python3
"""Packages the extension as dist/<id>-<version>.zip, and optionally installs it.

    python3 package.py
    python3 package.py --install https://myserver:8443 --user admin

--install uploads the zip to that EdgeTAK server (asking for the admin's
password, or reading EDGETAK_PASSWORD), like Install extension... on the
Extensions page. An installed
extension is updated and restarted if it was running. EdgeTAK's own
certificate isn't trusted by Python: pass --ca with the server's CA (the
console's Certificates page has it) or, on a test server, --insecure.
Accounts with two-factor login can't install this way; use the console.
"""

import argparse
import base64
import getpass
import json
import os
import ssl
import sys
import urllib.error
import urllib.request
import zipfile

HERE = os.path.dirname(os.path.abspath(__file__))
SKIP_DIRS = {"dist", "__pycache__", ".git", ".venv", "venv", "tests", ".idea", ".vscode"}
SKIP_FILES = {"package.py", ".gitignore"}


def build():
    with open(os.path.join(HERE, "plugin.json"), encoding="utf-8") as f:
        manifest = json.load(f)
    out = os.path.join(HERE, "dist", "%s-%s.zip" % (manifest["id"], manifest["version"]))
    os.makedirs(os.path.dirname(out), exist_ok=True)
    with zipfile.ZipFile(out, "w", zipfile.ZIP_DEFLATED) as z:
        for root, dirs, files in os.walk(HERE):
            dirs[:] = sorted(d for d in dirs if d not in SKIP_DIRS)
            for name in sorted(files):
                if name in SKIP_FILES or name.endswith((".pyc", ".log")):
                    continue
                path = os.path.join(root, name)
                z.write(path, os.path.relpath(path, HERE).replace(os.sep, "/"))
    print("built", os.path.relpath(out, HERE))
    return manifest, out


def install(url, user, zip_path, ctx):
    password = os.environ.get("EDGETAK_PASSWORD") or getpass.getpass("%s's password on %s: " % (user, url))
    with open(zip_path, "rb") as f:
        body = f.read()
    req = urllib.request.Request(url.rstrip("/") + "/admin/api/extensions", data=body, method="POST", headers={
        "Content-Type": "application/zip",
        "X-EdgeTAK": "1",
        "Authorization": "Basic " + base64.b64encode(("%s:%s" % (user, password)).encode()).decode(),
    })
    try:
        with urllib.request.urlopen(req, context=ctx, timeout=120) as r:
            res = json.load(r)
    except urllib.error.HTTPError as e:
        try:
            msg = json.load(e).get("error", e.reason)
        except ValueError:
            msg = e.reason
        sys.exit("install failed: %s" % msg)
    x = res["extension"]
    if res["updated"]:
        print("updated %s to %s%s" % (x["name"], x["version"], ", restarting" if x["enabled"] else ""))
    else:
        print("installed %s %s: on the Extensions page, choose its groups and start it" % (x["name"], x["version"]))


def main():
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--install", metavar="URL", help="the server's console address, e.g. https://myserver:8443")
    p.add_argument("--user", default="admin", help="an administrator (default admin)")
    p.add_argument("--ca", help="the server's CA certificate (PEM), to trust its certificate")
    p.add_argument("--insecure", action="store_true", help="don't check the server's certificate (test servers)")
    a = p.parse_args()
    _, out = build()
    if a.install:
        ctx = ssl.create_default_context(cafile=a.ca)
        if a.insecure:
            ctx.check_hostname, ctx.verify_mode = False, ssl.CERT_NONE
        install(a.install, a.user, out, ctx)


if __name__ == "__main__":
    main()
