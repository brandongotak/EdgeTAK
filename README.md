# EdgeTAK

A lightweight TAK server in a single Go binary. No Java, no PostgreSQL, no
Python runtime. Copy one file to a Raspberry Pi, laptop or Windows box, run it,
and ATAK, WinTAK and iTAK can connect.

| | Official TAK Server | OpenTAKServer / FreeTAKServer | **EdgeTAK** |
|---|---|---|---|
| Runtime | Java 17 + PostgreSQL | Python + deps (+ RabbitMQ for OTS) | single static binary |
| Idle memory | ~4 GB recommended | hundreds of MB | ~2 MB Go heap (measured at idle) |
| First run | install guide + cert scripts | install script | `./edgetak` (creates CA, certs, admin) |

## Status: MVP

Working and covered by an end-to-end test (`internal/app/app_test.go`):

- **CoT streaming**: plain TCP `8087` and mutual-TLS `8089`
  - TAK protocol negotiation (`t-x-takp-v/q/r`) and switching to protobuf on
    each connection. XML and protobuf clients can share a server, and events
    are converted between the two formats as they pass through.
  - Group-scoped routing, directed delivery via `<marti><dest callsign|uid/>`
    (GeoChat, direct sends), ping/pong, and latest-SA replay to new clients
  - Slow clients never stall the router: each one has a bounded queue, and
    events beyond it are dropped and counted
- **Certificate enrollment**: `8446`, the official `/Marti/api/tls/config` +
  `/Marti/api/tls/signClient/v2` flow used by ATAK, WinTAK and iTAK
  - Built-in CA generated on first start; the server cert is reissued
    automatically when host names or IPs change
  - Revocation (per cert, or every cert of a deleted user) disconnects live sessions
- **Data packages**: `missionupload`, `missionquery`, `content`, `search`,
  `sync/upload`, metadata tool and delete endpoints. Storage is
  content-addressed by SHA-256, and access is limited to the uploader's groups.
- **Data Sync / missions**: the `/Marti/api/missions` API used by ATAK, WinTAK
  and iTAK Data Sync
  - Create, list, delete; group scoped, optionally password protected or invite only
  - Subscriptions with owner, subscriber and read-only roles, plus signed mission tokens
  - Map items and files as mission contents, with a change feed
    (`/changes`), latest CoT (`/cot`), logs, keywords, invitations and archive export
  - Live push to subscribers: CoT sent to `<dest mission="..."/>` reaches
    subscribers only and is added to the mission automatically. `t-x-m-c` /
    `-n` / `-d` / `-i` / `-r` notifications are sent like the official server's.
  - Stored as one JSON file per mission in `data/missions/`
- **Client compatibility**: verified with ATAK, WinTAK 5.6, TAKX-MIL 5.8
  (including Data Sync between them) and iTAK
  - iTAK connection packages: a flat zip with a pre-issued client certificate,
    since iTAK does not enroll; each one is revocable on the Certificates tab
  - OAuth2 password login (`/oauth/token`) issuing RS256 JWTs signed by the
    server key, accepted as Bearer tokens on every API (TAKX)
  - Newer `/Marti/api/...` forms of the file, search, contacts and CoT APIs
  - No TLS session tickets on HTTPS: Windows SChannel (WinTAK) cannot handle
    them when it presents a client certificate
  - `log_level: debug` logs every HTTP request and traces failed TLS
    handshakes, for diagnosing new clients
- **Web admin console**: `https://<host>:8443/admin/`
  - Live map and connected-client table (callsign, team, battery, device, traffic)
  - Users and groups, issuing `.p12` certificates, revoking certificates
  - Data package upload, download and delete
  - Missions overview with export and delete
  - One-click **connection data package** (truststore + enrollment prefs) and a
    quick-connect QR code

## Quick install (Go 1.24+)

```sh
go install github.com/brandongotak/EdgeTAK/cmd/edgetak@latest
edgetak -data ./data
```

While the repository is private, tell Go to fetch it directly with your
GitHub credentials (git must be able to clone the repo):
`GOPRIVATE=github.com/brandongotak go install github.com/brandongotak/EdgeTAK/cmd/edgetak@latest`

For a server that should run at boot, use the installer below instead.

## Install on a Raspberry Pi (or any systemd Linux)

```sh
sh packaging/rpi/build-installer.sh        # -> dist/edgetak-<version>-rpi-installer.sh
scp dist/edgetak-*-rpi-installer.sh pi@<pi>:
ssh pi@<pi> sudo sh edgetak-*-rpi-installer.sh
```

A single self-extracting file covers 64-bit Pi OS, 32-bit Pi OS (every model,
including Zero and Pi 1) and x86-64 Linux. It installs `/usr/local/bin/edgetak`,
creates an `edgetak` system user and `/var/lib/edgetak`, sets up a sandboxed
systemd service enabled at boot, opens the ports in ufw or firewalld, and
prints the admin login. Rerun the same command to upgrade; the data directory
is kept. Options: `--public-host HOST`, `--import-data DIR` (move a manual
install's data, CA and users into the service), `--no-firewall`,
`--uninstall [--purge]`.

## Run

```sh
go build -o edgetak ./cmd/edgetak
./edgetak -data ./data
```

On first start the log prints the generated `admin` password, which is also
saved to `data/initial-admin-password.txt`. Open `https://<server-ip>:8443/admin/`,
or `http://localhost:8080/admin/` on the server itself. Then:

1. **Users** tab: create users and give them groups (e.g. `Cyan`). Users with
   no groups land in `__ANON__` together with plain-TCP clients.
2. **Connect** tab: download the SSL connection package and import it in
   ATAK (*Import → Local SD* or open it from the file manager). ATAK asks for
   the username and password, enrolls for a certificate on 8446, then
   connects on 8089.

Reset a forgotten password: `./edgetak -data ./data -reset-password admin`

Cross-compile: `GOOS=linux GOARCH=arm64 go build ./cmd/edgetak` (Pi 4/5), or
run `./build.sh` to build every target into `dist/`.

### Ports

| Port | Purpose |
|---|---|
| 8087 | CoT streaming, plain TCP (anonymous, `__ANON__` group) |
| 8089 | CoT streaming, TLS with client certificate |
| 8443 | Marti API + admin console (TLS, client cert or password) |
| 8446 | Certificate enrollment (TLS, password) |
| 8080 | Marti API, plaintext (for 8087 clients); admin console from localhost only |

Set any address to `""` in `data/edgetak.json` to disable that listener.

## Configuration

`data/edgetak.json` is written with defaults on first run:

| Key | Default | Notes |
|---|---|---|
| `public_host` | first LAN IPv4 | address written into connection packages |
| `hostnames` | `[]` | extra DNS names / IPs for the server certificate |
| `anonymous_group` | `__ANON__` | |
| `allow_anonymous_http` | `true` | plaintext Marti API without auth |
| `cert_validity_days` | `365` | client certificates |
| `truststore_password` | `atakatak` | password of the `.p12` files handed to clients |
| `send_latest_sa` | `true` | replay peers' positions to new clients |
| `idle_timeout_seconds` | `180` | disconnect silent stream clients |
| `max_upload_mb` | `500` | |
| `log_level` | `info` | `debug` logs handshake failures and malformed messages |

## Layout

```
cmd/edgetak/        entry point and flags
internal/cot/       CoT event model, XML <-> TAK protobuf codec, stream framing
internal/router/    client registry, group/directed routing, plugin hooks
internal/stream/    TCP/TLS stream sessions and protocol negotiation
internal/web/       Marti API, enrollment, data packages, admin console (embedded)
internal/mission/   Data Sync missions: roles, contents, change feed, notifications
internal/pki/       CA, CSR signing, PKCS#12
internal/store/     JSON-file store for users, certificates, packages
internal/app/       wiring, listeners, end-to-end test
```

## Extending

`router.Router.AddHook(func(src *router.Client, ev *cot.Event) bool)` runs
before every routing decision. A hook can inspect events, change them (call
`ev.Changed()` afterwards), or drop them by returning `false`.
`Router.Inject(ev, groups)` sends events generated by the server. These are the
seams for the planned plugin API.

## Roadmap

- Plugin API (out-of-process plugins over gRPC/WebSocket on top of the hooks)
- Data Sync extras: mission feeds, map layers, external data, mission hierarchy
- Federation with official TAK Server, OTS and FTS
- Group selection from the client (`t-x-g-c`), CoT history and replay
- Video stream registry, device profiles, and packaging (deb/rpm, Docker, Windows service)

## Development

```sh
go test ./...
```

The end-to-end test uses an in-memory network instead of real sockets, because
some desktop antivirus products (AVG/Avast Web Shield, for example) intercept
local TLS connections and re-sign them, which breaks mutual TLS. The same
products can make the admin console show AVG's certificate when you browse to
it from the server machine. Phones and other machines are not affected.
