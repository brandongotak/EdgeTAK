# EdgeTAK

A lightweight TAK server in a single Go binary. No Java, no PostgreSQL, no
Python runtime. Copy one file to a Raspberry Pi, a Linux server or a Windows PC,
run it, and ATAK, WinTAK, TAKX and iTAK can connect.

| | Official TAK Server | OpenTAKServer / FreeTAKServer | **EdgeTAK** |
|---|---|---|---|
| Runtime | Java 17 + PostgreSQL | Python + deps (+ RabbitMQ for OTS) | single static binary |
| Idle memory | ~4 GB recommended | hundreds of MB | a few MB |
| First run | install guide + cert scripts | install script | one installer command (creates CA, certs, admin) |

- [Features](#features)
- [Install](#install)
  - [Option A: local network](#option-a-local-network-raspberry-pi-linux-or-windows)
  - [Option B: internet server with Let's Encrypt](#option-b-internet-server-with-lets-encrypt)
- [First steps in the admin console](#first-steps-in-the-admin-console)
- [Connect clients](#connect-clients)
- [Upgrade, back up, uninstall](#upgrade-back-up-uninstall)
- [Ports](#ports) · [Configuration](#configuration) · [Troubleshooting](#troubleshooting) · [Development](#development)

## Features

- **CoT streaming** on plain TCP `8087` and mutual-TLS `8089`. Clients can use
  XML or TAK protobuf on the same server, and events are converted between
  the two as they pass through. Routing is group scoped, with directed
  delivery for GeoChat and direct sends, and new clients receive the latest
  position of everyone they can see.
- **Certificate enrollment** on `8446`, using the official `/Marti/api/tls`
  flow. The CA is created on first start, and certificates can be revoked
  from the admin console.
- **Quick connect with Let's Encrypt.** Clients join with only an address,
  username and password. EdgeTAK requests and renews the certificate itself.
- **Groups and channels**, as on the official server. Memberships have a
  direction: **in** (receive), **out** (send) or both. Changes apply to
  connected devices immediately, and users can switch their groups on and off
  from the client (ATAK, WinTAK and TAKX Channels).
- **Data packages**: upload, search, download and delete, limited to the
  uploader's groups.
- **Data Sync (missions)**: subscriptions and roles, map items and files, the
  change feed, logs, invitations and export. Changes are pushed live to
  subscribers.
- **Send to devices**: from the admin console, send a chat message or offer
  a data package (including settings `.pref` packages) to everyone, chosen
  groups or chosen devices, with a history of what was sent.
  - **Direct messages**: the server can appear on devices as an HQ contact.
    Its messages open as a private conversation, users can reply, and
    replies show up in the console's Inbox.
- **Self-registration**: invite links (with groups, a use limit and an expiry)
  let people create their own account. Without an invite, sign-up can be
  off, require admin approval, or be open. Afterwards the sign-up page shows
  how to connect.
- **Live map** beside every console tab (full screen on phones), with base
  maps like ATAK's: Streets, Satellite, Hybrid, Topo, USGS Topo, USGS
  Imagery Topo and Dark. Add your own tile sources or import ATAK map source
  `.xml` files; overlays for devices, callsigns and the server contact.
- **Web admin console**: connected clients, users, groups,
  certificates, data packages, missions, and connection packages for each
  client.
- **Verified clients**: ATAK, WinTAK 5.6, TAKX-MIL 5.8 and iTAK 2.12,
  including Data Sync and channels between them.

## Install

Pick the setup that fits:

| | **A. Local network** | **B. Internet server with Let's Encrypt** |
|---|---|---|
| Typical host | Raspberry Pi, laptop, home or field server | Cloud VM (DigitalOcean, AWS, ...) |
| Clients connect with | a connection package (zip) from the admin console | just address, username and password |
| Needs | nothing extra | a domain name pointing at the server, and port 80 open |

### One-line install (Raspberry Pi OS and other systemd Linux)

On the device itself, run:

```sh
# A. Local network
curl -fsSL https://github.com/brandongotak/EdgeTAK/releases/latest/download/install.sh | sudo sh -s -- --public-host <device-ip>

# B. Internet server with Let's Encrypt (do the DNS and firewall steps below first)
curl -fsSL https://github.com/brandongotak/EdgeTAK/releases/latest/download/install.sh | sudo sh -s -- \
    --public-host tak.example.com --letsencrypt you@example.com --no-plaintext
```

The script downloads the latest release's installer, verifies its SHA-256
checksum, and runs it. Run the same command again to upgrade. To install a
specific release, use `EDGETAK_VERSION=v0.2.0` (write it after `sudo`, as
`sudo EDGETAK_VERSION=v0.2.0 sh -s -- ...`). A copy of the installer stays
in `/usr/local/lib/edgetak/` for `--uninstall`.

The installer supports 64-bit and 32-bit Raspberry Pi OS (every Pi, including
Zero) and x86-64 Linux, and sets up a systemd service that starts at boot.
Standalone binaries for Linux, Windows and macOS are attached to each
[release](https://github.com/brandongotak/EdgeTAK/releases).

**Offline or air-gapped devices:** download `edgetak-installer.sh` from a
release, or build it yourself (needs Go 1.24+ and `sh`; on Windows, use Git
Bash) with `sh packaging/rpi/build-installer.sh`. Then copy it over and run
it as shown below; it needs no internet access.

### Option A: local network (Raspberry Pi, Linux or Windows)

**Raspberry Pi or any systemd Linux:** use the [one-line install](#one-line-install-raspberry-pi-os-and-other-systemd-linux).
Without internet on the device, copy the installer over instead:

```sh
scp edgetak-installer.sh pi@<device-ip>:
ssh pi@<device-ip> sudo sh edgetak-installer.sh --public-host <device-ip>
```

- `--public-host` is the address phones use to reach the server. Leave it out
  to detect it automatically. A DHCP reservation in your router keeps the
  address stable.
- The installer creates an `edgetak` system user and `/var/lib/edgetak`, sets
  up a sandboxed service, opens the ports in ufw or firewalld, and prints the
  **admin login** at the end.
- From Windows PowerShell, give `scp` the full file name; Windows does not
  expand `*`.

Then open `https://<device-ip>:8443/admin/` and accept the browser's
certificate warning. The server uses its own CA, which browsers do not know.
Continue with [First steps](#first-steps-in-the-admin-console).

**Windows, or a quick test anywhere** (no service, runs in the foreground):

```sh
go build -o edgetak ./cmd/edgetak        # edgetak.exe on Windows
./edgetak -data ./data
```

The admin password is printed in the log and saved to
`data/initial-admin-password.txt`. On the same machine, the console is also at
`http://localhost:8080/admin/`. To keep it running on Windows, wrap it in a
service manager such as [NSSM](https://nssm.cc); EdgeTAK does not install a
Windows service itself.

### Option B: internet server with Let's Encrypt

Use this when clients connect over the internet. They add the server with only
its address, username and password, with no files to transfer.

**1. Before installing**

- **DNS:** create an `A` record such as `tak.example.com` pointing at the
  server's public IP, and wait until it resolves.
- **Firewall:** allow inbound **80** (Let's Encrypt), **8089** (CoT), **8443**
  (API and admin) and **8446** (enrollment), plus 22 for SSH. On cloud VMs,
  this usually means the provider's firewall as well as the VM's own.

**2. Install** on the server:

```sh
curl -fsSL https://github.com/brandongotak/EdgeTAK/releases/latest/download/install.sh | sudo sh -s -- \
    --public-host tak.example.com --letsencrypt you@example.com --no-plaintext
```

- `--letsencrypt EMAIL` serves a Let's Encrypt certificate on 8446. EdgeTAK
  requests and renews it itself, without certbot, cron or restarts. Port 80
  must stay reachable for renewal.
- `--no-plaintext` turns off the anonymous ports 8087 and 8080, which should
  not be open on the internet.
- Watch for `Let's Encrypt certificate ready` with `journalctl -u edgetak -f`.
  Until then, 8446 serves the EdgeTAK certificate, so quick connect starts
  working once the certificate is issued.

**3. Log in**

Open **`https://tak.example.com:8446/admin/`**. It has a trusted certificate,
so there is no browser warning. Continue with [First steps](#first-steps-in-the-admin-console).

How it works (the same design as installTAK): only 8446 has the public
certificate. Enrollment hands each client the EdgeTAK CA, which it then uses
to trust 8089 and 8443. Connection packages work on this server too.

To use certificates you already have, for example from certbot, set
`"public_cert": {"mode": "files", "cert_file": "...", "key_file": "..."}` in
`/var/lib/edgetak/edgetak.json` and restart. The `edgetak` user must be able
to read both files, and EdgeTAK reloads them when they change.

## First steps in the admin console

1. **Change the admin password**: on the **Users** tab, click **Password** on
   `admin`. Then delete the file holding the first-run password:
   `sudo rm /var/lib/edgetak/initial-admin-password.txt` (installer) or
   `data/initial-admin-password.txt` (manual run).
2. **Create groups** on the **Groups** tab, for example `Cyan` and `Command`.
   Users without a group are in `__ANON__`, together with anonymous plaintext
   clients.
3. **Create users** on the **Users** tab and pick their groups. Use the
   **Groups** button on a user to set directions:
   - **in + out**: the usual case.
   - **in**: receive only.
   - **out**: send only.

   Give each person or device its own user, so you can revoke one without
   affecting the rest. You can also let people create their own accounts;
   see [Self-registration](#self-registration).
4. Check that **Public host** on the **Dashboard** shows the address clients
   will use.

### Self-registration

The **Sign-ups** tab lets people make their own accounts on the sign-up page,
`https://<public host>:8446/register`:

- **Invite links**: pick the groups, how many people may use the link
  (`0` = unlimited) and how long it lasts. Share the link or its QR code.
  Anyone with it gets an account right away, in those groups.
- **Sign-up without an invite**:
  - **Off** (the default): invite links only.
  - **Approval**: people request an account, and it appears under **Pending
    requests** for you to approve or reject.
  - **Open**: anyone who can reach the server gets an account.

  Approval and Open accounts go into the **groups for new accounts**.

After signing up, the page shows the server address, an ATAK quick-connect QR
code (on servers with Let's Encrypt), and connection packages for ATAK, WinTAK,
TAKX and iTAK. Sign-up only works over HTTPS, and attempts are rate-limited per
address. Use **Open** only on servers you are happy for anyone to join.

## Connect clients

There are two ways to connect, depending on the server:

- **Quick connect** (servers with Let's Encrypt): add a server in the client
  with the server's address, turn on **Use authentication** and **Enroll for
  client certificate**, then enter a username and password. This works in
  ATAK, WinTAK, TAKX and iTAK.
- **Connection package** (any server): on the **Connect** tab, download a
  package and import it on the device. For ATAK, WinTAK and TAKX, choose
  *SSL with certificate enrollment*; the client then asks for the password.
  For iTAK, choose *iTAK*. That package includes a client certificate for
  one user, so treat it like a password and make a new one per device.

| Client | Quick connect | Package to import |
|---|---|---|
| ATAK (Android) | Settings → Network → servers → add | SSL package (import from the file manager or *Import → Local SD*) |
| WinTAK | Settings → network / server connections → add, with certificate enrollment | SSL package (import data package) |
| TAKX | add a TAK server with username and password | SSL package |
| iTAK | add a server with address, username and password | iTAK package (Settings → Network → upload server package) |

**Channels:** devices enrolled on this server get ATAK's **Channels** button
automatically. ATAK devices enrolled before channels were added need **Settings →
Network Preferences → Network Connection Preferences → Apply TAK Server
profile updates** turned on, followed by a reconnect. Alternatively, import a
fresh SSL package.

**Plain TCP** (`8087`, local networks only): add the server as TCP with no
authentication. These clients are anonymous and only see `__ANON__`.

## Upgrade, back up, uninstall

- **Upgrade:** rerun the one-line install (or a newer installer file) with
  the **same flags**. Users, groups, certificates, packages and missions are
  kept. Reconnect clients afterwards, and reload the admin console with Ctrl+F5.
- **Back up** `/var/lib/edgetak` (or `data/`). `certs/ca-key.pem` is the key
  every enrolled device trusts: if it is lost, every device must enroll again.
- **Move a manual install into the service:** `--import-data <old data dir>`.
- **Uninstall:** `sudo sh /usr/local/lib/edgetak/edgetak-installer.sh --uninstall`
  keeps the data; add `--purge` to delete it and the `edgetak` user. If you
  installed from a copied file, run that file with `--uninstall` instead.
- **Forgotten password:**
  `sudo -u edgetak /usr/local/bin/edgetak -data /var/lib/edgetak -reset-password admin`
  (manual run: `./edgetak -data ./data -reset-password admin`).

## Ports

| Port | Purpose |
|---|---|
| 8089 | CoT streaming, TLS with client certificate |
| 8443 | Marti API and admin console (client certificate or password) |
| 8446 | Certificate enrollment; with Let's Encrypt also the trusted admin console |
| 80 | Let's Encrypt challenges (only with `--letsencrypt`) |
| 8087 | CoT streaming, plain TCP, anonymous (off with `--no-plaintext`) |
| 8080 | Marti API, plaintext, for 8087 clients; admin console from localhost only (off with `--no-plaintext`) |

## Configuration

The config is `/var/lib/edgetak/edgetak.json` (installer) or
`data/edgetak.json` (manual run). It is written with defaults on first run;
restart after editing. Set any listener address (`tcp_addr`, `tls_addr`,
`https_addr`, `enroll_addr`, `http_addr`) to `""` to turn it off.

| Key | Default | Notes |
|---|---|---|
| `public_host` | first LAN IPv4 | address clients use; goes into packages and the server certificate |
| `hostnames` | `[]` | extra DNS names or IPs for the server certificate |
| `public_cert` | off | `mode`: `letsencrypt` or `files`; also `email`, `domains`, `cert_file`, `key_file`, `listeners` (default `["enroll"]`) |
| `anonymous_group` | `__ANON__` | group for ungrouped users and plaintext clients |
| `allow_anonymous_http` | `true` | plaintext Marti API without login |
| `cert_validity_days` | `365` | client certificates |
| `truststore_password` | `atakatak` | password of the `.p12` files handed to clients |
| `send_latest_sa` | `true` | send peers' latest positions to new clients |
| `idle_timeout_seconds` | `180` | disconnect silent stream clients |
| `max_upload_mb` | `500` | data package size limit |
| `log_level` | `info` | `debug` logs every request and traces failed TLS handshakes |

## Troubleshooting

- **See what a client is doing:** set `"log_level": "debug"` and restart, then
  run `journalctl -u edgetak -f`. Every HTTP request is logged with its path,
  status and client, and failed TLS handshakes are traced step by step. Set it
  back to `info` afterwards, especially on internet servers.
- **Packages point at the wrong address:** check **Public host** on the
  Dashboard. The server warns at startup when `public_host` is neither a local
  address nor a resolvable name.
- **Let's Encrypt is stuck on "pending":** the domain must resolve to this
  server and port 80 must be reachable from the internet. The log explains
  why issuance failed.
- **Certificates rejected after a reboot (Raspberry Pi):** the Pi has no
  battery-backed clock. Make sure NTP has synchronised the time.
- **Admin console shows an AVG/Avast certificate on the server's own PC:** that
  antivirus intercepts local HTTPS. Browse from another device, or exclude the
  server's address. Phones are not affected.

## Development

```sh
go test ./...
go install github.com/brandongotak/EdgeTAK/cmd/edgetak@latest   # needs Go 1.24+
```

**Releases:** pushing a version tag (`git tag v0.2.0 && git push origin
v0.2.0`) runs the tests and publishes a GitHub release (see
`.github/workflows/release.yml`). The release carries `install.sh`,
`edgetak-installer.sh` (which the one-line install fetches), a versioned copy
of the installer, standalone binaries and `SHA256SUMS`.

The end-to-end tests use an in-memory network instead of real sockets,
because some desktop antivirus products intercept local TLS and break mutual
TLS.

```
cmd/edgetak/        entry point and flags
internal/cot/       CoT event model, XML <-> TAK protobuf codec, stream framing
internal/router/    client registry, group routing (IN/OUT, live changes), hooks
internal/stream/    TCP/TLS stream sessions and protocol negotiation
internal/web/       Marti API, enrollment, OAuth, groups/channels, admin console (embedded)
internal/mission/   Data Sync missions: roles, contents, change feed, notifications
internal/publiccert/ Let's Encrypt / file-based public certificate for enrollment
internal/pki/       CA, CSR signing, PKCS#12
internal/store/     JSON-file store for users, groups, certificates, packages
internal/tlstrace/  TLS handshake tracing for debugging clients
internal/app/       wiring, listeners, end-to-end tests
packaging/rpi/      self-extracting installer and systemd service
```

`router.Router.AddHook(func(src *router.Client, ev *cot.Event) bool)` runs
before every routing decision. A hook can inspect events, change them (call
`ev.Changed()` afterwards), or drop them by returning `false`.
`Router.Inject(ev, groups)` sends events generated by the server. These are
the starting points for the planned plugin API.

### Roadmap

- Plugin API (out-of-process plugins on top of the hooks)
- Federation with the official TAK Server, OpenTAKServer and FreeTAKServer
- Data Sync extras: mission feeds, map layers, external data, mission hierarchy
- CoT history and replay, video stream registry, custom device profiles
- Packaging: deb/rpm, Docker, Windows service
