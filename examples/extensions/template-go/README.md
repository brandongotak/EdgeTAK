# EdgeTAK extension template (Go)

A starting point for an EdgeTAK extension in Go, using only the standard
library. The extension ships as one binary per platform, so the server
needs nothing installed. As it comes, it:

- counts the CoT it receives and shows the latest events on its page;
- keeps a marker on the map, where an admin puts it on its page;
- saves its settings in its data folder, which updates keep.

The API itself is described in the
[extension guide](../../../docs/extensions.md).

## Files

| File | |
|---|---|
| `plugin.json` | The manifest: id, name, version, the binary for each platform, permissions. |
| `main.go` | The extension. The parts to replace are marked `TODO`. |
| `edgetak/` | A small client for the API: `Info`, `Get`, `Send`, `Stream` (which reconnects), `Marker` and `Settings`. Copy it as it is. |
| `page/` | The page shown in the console, built into the binary. |
| `package/` | `go run ./package` builds every platform and writes `dist/<id>-<version>.zip`; it can also install it on a server. |

## Make it yours

1. Copy this folder and rename it. Change the module path in `go.mod` (and
   the `edgetak` import in `main.go`) if you like.
2. In `plugin.json`:
   - Set `id`. It is lowercase letters, digits and dashes, and identifies
     the extension: uploading a zip with the same id updates it.
   - Set `name`, `description`, `author` and `homepage`.
   - Use your id in the binaries' names under `run`. Keep only the
     platforms you need. Raspberry Pi OS 64-bit is `linux/arm64`; 32-bit is
     `linux/arm`.
   - Keep only the `permissions` you need:
     - `cot:read`: receive CoT from the extension's groups;
     - `cot:write`: send CoT to them;
     - `data:read`: read groups, devices, missions and position history.

     Remove `page` if you don't need a page.
3. In `main.go`:
   - Put your logic in `onEvent` (CoT in) and `worker` (things to do every
     few seconds, such as polling another service and sending what it
     reports).
   - Put your settings in `Settings` and `defaults`. Check them in
     `checkSettings`, and add their fields to `page/index.html`.

Building needs Go 1.22 or later. The binaries are built without cgo, so
they run on any machine of their platform.

## Build, install, repeat

```sh
go run ./package                                       # dist/my-extension-0.1.0.zip
go run ./package -install https://myserver:8443 -user admin -insecure
```

- **The first install:** a new extension is installed stopped. On the
  Extensions page, choose its groups and press **Start**.
- **Updates:** after that, each `-install` updates the extension and
  restarts it, keeping its settings, so testing a change takes one
  command. Bump `version` when you give the zip to others.
- **Password:** `-install` asks for the password and shows it as you type.
  To avoid that, set `EDGETAK_PASSWORD` for the command.
- **Certificates:** EdgeTAK's certificate isn't trusted by Go. `-insecure`
  skips the check on a test server; `-ca ca.pem` trusts the server's CA
  (download it from the console's Certificates page).
- **Two-factor login:** accounts with two-factor login can't install from
  the command line. Use **Install extension…** in the console instead.

When something goes wrong, look at **Log** on the Extensions page. It shows
what the extension printed, including panics. The page's own errors show at
its top.
