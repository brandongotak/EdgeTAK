# EdgeTAK extension template (Python)

A starting point for an EdgeTAK extension in Python, using only the
standard library. As it comes, it:

- counts the CoT it receives and shows the latest events on its page;
- keeps a marker on the map, where an admin puts it on its page;
- saves its settings in its data folder, which updates keep.

The API itself is described in the
[extension guide](../../../docs/extensions.md).

## Files

| File | |
|---|---|
| `plugin.json` | The manifest: id, name, version, how to run it, permissions. |
| `main.py` | The extension. The parts to replace are marked `TODO`. |
| `edgetak.py` | A small client for the API: `info()`, `get()`, `send()`, `stream()` (which reconnects), `marker()` and `Settings`. Copy it as it is. |
| `page/` | The page shown in the console (`index.html`, and any other files it needs). |
| `package.py` | Builds `dist/<id>-<version>.zip`, and can install it on a server. |

## Make it yours

1. Copy this folder and rename it.
2. In `plugin.json`:
   - Set `id`. It is lowercase letters, digits and dashes, and identifies
     the extension: uploading a zip with the same id updates it.
   - Set `name`, `description`, `author` and `homepage`.
   - Keep only the `permissions` you need:
     - `cot:read`: receive CoT from the extension's groups;
     - `cot:write`: send CoT to them;
     - `data:read`: read groups, devices, missions and position history.

     Remove `page` if you don't need a page.
3. In `main.py`:
   - Put your logic in `on_event()` (CoT in) and `worker()` (things to do
     every few seconds, such as polling another service and sending what it
     reports).
   - Put your settings, with their defaults, in `settings`. Check them in
     `check_settings()`, and add their fields to `page/index.html`.

The server needs Python 3.8 or later. Raspberry Pi OS and most Linux
servers have it. If you need packages from PyPI, bundle them inside the
extension: `pip install --target lib <package>`, then put
`sys.path.insert(0, "lib")` at the top of `main.py`. Packages with compiled
code must be built for the server's platform.

## Build, install, repeat

```sh
python3 package.py                                     # dist/my-extension-0.1.0.zip
python3 package.py --install https://myserver:8443 --user admin --insecure
```

- **The first install:** a new extension is installed stopped. On the
  Extensions page, choose its groups and press **Start**.
- **Updates:** after that, each `--install` updates the extension and
  restarts it, keeping its settings, so testing a change takes one
  command. Bump `version` when you give the zip to others.
- **Certificates:** EdgeTAK's certificate isn't trusted by Python.
  `--insecure` skips the check on a test server; `--ca ca.pem` trusts the
  server's CA (download it from the console's Certificates page).
- **Two-factor login:** accounts with two-factor login can't install from
  the command line. Use **Install extension…** in the console instead.

When something goes wrong, look at **Log** on the Extensions page. It shows
what the extension printed, including Python tracebacks. The page's own
errors show at its top.
