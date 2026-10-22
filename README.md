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
  subscribers. **Show on map** (Missions tab) draws a mission's map items
  and the files in it the map can read (KML, KMZ, GeoJSON, CoT, data
  packages) as one layer on the console map.
- **Send to devices**: from the admin console, send a chat message or offer
  a data package (including settings `.pref` packages) to everyone, chosen
  groups or chosen devices, with a history of what was sent.
  - **Direct messages**: the server can appear on devices as an HQ contact.
    Its messages open as a private conversation, users can reply, and
    replies show up in the console's Inbox.
  - **Scheduled sends**: a message or data package at a set time, once or
    on a repeat (every few hours, every day, weekdays, chosen days), to
    everyone or chosen groups, with an optional end date. Times follow the
    server's time zone; a run missed while EdgeTAK was off is skipped
    rather than sent late. **Send now**, pause and resume; each run is in
    the Sent list and the audit log.
- **Self-registration**: invite links (with groups, a use limit and an expiry)
  let people create their own account. Without an invite, sign-up can be
  off, require admin approval, or be open. Afterwards the sign-up page shows
  how to connect.
- **Live map** beside every console tab (full screen on phones), with base
  maps like ATAK's: Streets, Satellite, Hybrid, Google Hybrid, Topo, USGS Topo, USGS
  Imagery Topo and Dark. Add your own tile sources or import ATAK map source
  `.xml` files; overlays for devices, callsigns and the server contact.
  **Map sources to devices:** tick *Send to ATAK devices* on a source (or
  pick a built-in one such as Google Hybrid) and it is added to ATAK's map
  list on the devices in the chosen groups: straight away for connected
  devices, in their connection profile for the others.
  **Weather and terrain** overlays: weather radar worldwide (RainViewer,
  coarse past zoom 7) and detailed over the US (NEXRAD, from the Iowa
  Environmental Mesonet), refreshed every 5 minutes with its age shown, and
  hillshade (Esri) to show the shape of the land on any base map.
  **Show on map** on the Data Packages tab draws a package's CoT markers,
  routes, polygons, rectangles and circles, and any KML, KMZ or GeoJSON in
  it, as its own overlay. The console's libraries are built in, so it works
  without internet access; only online base maps and the weather and terrain
  overlays need it.
- **Position history** (off by default): turn on recording under the map's
  **History** button and choose how long to keep it (6 hours to 30 days).
  Positions are thinned (a point when a device moves 5 m, or once a minute)
  and kept in `history/` in the data directory. The **Trails** overlay
  shows each device's path in its team colour over the last 15 minutes to
  everything kept, and **Replay this period** plays it back with a time
  slider, play/pause and speeds from 1× to 1200×. **Export trails** downloads
  the chosen devices' trails for a time range as KML (Google Earth, ATAK) or
  GPX (GIS and GPS tools), split where a device went quiet for over 5
  minutes, or adds them as an overlay to send to devices.
- **Video**: feeds for ATAK's and WinTAK's Video Player, limited by group,
  with camera markers on devices. The installer and the Docker image include
  the MediaMTX media server (on by default): cameras, drones and ATAK publish over RTSP, RTMP or
  SRT with an EdgeTAK login, streams become feeds automatically, viewers get
  personal links, and the console plays them live. See [Video](#video).
- **Health**: server graphs for the last hour to 24 hours: CPU, memory,
  disk, network, temperature (Raspberry Pi), connected devices, CoT messages
  and video bandwidth, with warnings for a full disk, overheating or dropped
  messages. A device table lists each device's battery (with history and
  drain rate), last report, GPS fix and connection, worst problems first.
  System figures need Linux; elsewhere only EdgeTAK's own are shown.
- **One sign-in page** at `https://<host>:8446/` for everyone: administrators
  go on to the admin console, users to their **account page**, where they get
  their connection packages and ATAK quick-connect code, see their connected
  devices and certificates (with expiry), revoke the certificate of a lost
  device, and change their password. Share the link from the console's
  Connect tab.
- **Events** (People → Events): a temporary operation with its own group and
  Data Sync mission. People join by scanning one QR code (printable as a
  join sheet): either each person signs up for their own account, or
  everyone enrolls with one shared account. When the event ends, at the time
  you set, every event account is erased (certificates revoked, devices
  disconnected) and its group removed; the mission is kept for review.
  Extend or end it early from the console.
- **Disable a user** (People → Users) instead of deleting them: they can't
  sign in, their certificates stop working and their devices are
  disconnected at once, while their account, groups and certificates are
  kept. **Enable** brings everything back. You can't disable yourself.
- **Federation** with TAK Servers and other EdgeTAKs (TAK Server Federation
  v2): positions, markers, chat, direct messages and data packages cross
  between servers, in either direction, with certificate or token sign-in,
  approval of servers that connect, and per-federate group sharing. See
  [Federation](#federation).
- **Directory sign-in** (Users tab): LDAP or Active Directory users sign in
  with their directory password everywhere (console, account page, ATAK /
  iTAK / WinTAK enrollment), with directory groups as EdgeTAK groups. See
  [Directory sign-in](#directory-sign-in-ldap--active-directory).
- **Drawings** (Maps tab): draw markers (friendly, hostile, neutral,
  unknown, or a colored spot), routes, areas and circles on the console map
  and they appear on devices' maps like items a teammate placed, for every
  device or chosen groups. EdgeTAK keeps them and sends them to each device
  as it connects, so phones that were off get them too; editing or deleting
  one updates devices. EdgeTAK's copy is the reference: changes made on a
  device are replaced the next time it connects.
- **Overlays** (Data → Overlays): import a KML, KMZ or GeoJSON file once
  and every device in the chosen groups gets it as a map overlay, including
  devices that connect later. See [Overlays](#overlays).
- **Offline maps** (Maps tab): MBTiles maps served by EdgeTAK, on the
  console map and in ATAK (as map sources in device profiles), with no
  internet access. See [Offline maps](#offline-maps).
- **Plugins** (update server): upload ATAK plugin and app APKs, and devices
  install and update them from ATAK's **Plugins** screen. Names, versions,
  icons and the ATAK version each plugin is built for are read from the
  APK; each device is offered the builds its ATAK version runs. Device profiles
  point ATAK at it. Link a tak.gov account to add tak.gov's plugins in one
  click. See [Plugins](#plugins).
- **Device profiles** (Groups tab): settings and files sent to the TAK
  devices of a group's members, or of every user, as on the official server.
  - **Contents:** ATAK settings (entered as key, type and value), `.pref`
    files, plugin APKs, map source XML, overlays, up to 512 MB per file.
  - **When:** devices get them when they enroll, and each time they connect
    if a profile changed since their last sync. **Send now** reaches devices
    that are already connected.
  - **Download** shows the exact package devices receive.
- **Emergency alerts:** when someone presses ATAK's emergency button (911
  Alert, Ring The Bell, In Contact, Geo-fence Breached, custom), every open
  console shows a red banner and sounds an alarm until an admin acknowledges
  it, with desktop notifications if allowed. The device is marked on the map
  and the Dashboard. The alert stays until the device cancels it or an admin
  dismisses it, even if the device goes offline. The log is on the Settings
  tab.
- **Lost contact alerts** (Settings tab): an amber alert with the last known
  position when a device stops reporting. EdgeTAK tells a deliberate
  sign-off from lost contact by how the connection ends: turning the server
  connection off in ATAK, quitting ATAK or shutting the phone down closes it
  cleanly, and is only listed under **Devices offline**; a connection that
  goes silent or is cut (out of coverage, flat battery, crashed app) raises
  the alert after a grace period (5 minutes by default), unless the device
  is back first, as when a phone switches networks. A device that stays
  connected without sending a position (10 minutes by default) alerts too.
  Alerts end by themselves when the device is back; they can be limited to
  chosen groups. A device whose user quits ATAK on purpose because something
  is wrong looks like a sign-off.
- **Geofences** (Settings tab): draw an area (corner by corner) or a circle
  on the console map, and get an amber alert with the device's position
  when a device enters it, leaves it, or both, for every device or chosen
  groups. The alert ends by itself when the device crosses back. A device's
  first position only notes where it is, so drawing an area doesn't alert
  for everyone already in it. Areas can also be drawn on devices' maps (as
  ATAK shapes), and show on the console map as the **Geofences** layer.
- **Chat log** (optional, off by default): records GeoChat messages sent
  through the server (All Chat Rooms, team and group chats, one-to-one
  messages and the console's own), with search, filters by conversation,
  sender and time, and CSV or JSON export for after-action reviews. How long
  messages are kept is a data retention rule.
- **Certificate status**: when the CA, the server certificate, the public
  certificate and every device certificate expire, with how each one renews.
  Device certificates are flagged a set number of days before they expire
  (30 by default), marked when a newer enrollment replaced them, and show
  which device is connected with each one. Warnings appear on the Dashboard,
  the Health tab and in the system log.
- **Data retention**: how long to keep data packages, idle missions, Inbox
  messages, pending sign-ups, old invites and expired certificate records
  (Settings tab). Rules run hourly, show what they would remove before you save,
  and never remove files a mission uses or records of certificates that are
  still valid.
- **Install as an app**: the console installs on phones and desktops
  (Install app in the top bar on Chrome/Edge; Share → Add to Home Screen on
  iPhone). It needs a trusted certificate, so use port 8446 with Let's Encrypt.
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

Prefer containers? See [Option C: Docker](#option-c-docker).

### One-line install (Raspberry Pi OS and other systemd Linux)

On the device itself, run:

```sh
# A. Local network
curl -fsSL https://github.com/brandongotak/EdgeTAK/releases/latest/download/install.sh | sudo sh -s -- --public-host <device-ip>

# B. Internet server with Let's Encrypt (do the DNS and firewall steps below first)
curl -fsSL https://github.com/brandongotak/EdgeTAK/releases/latest/download/install.sh | sudo sh -s -- \
    --public-host tak.example.com --letsencrypt --no-plaintext
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
    --public-host tak.example.com --letsencrypt --no-plaintext
```

- `--letsencrypt` serves a Let's Encrypt certificate on 8446. EdgeTAK
  requests and renews it itself, without certbot, cron or restarts. Port 80
  must stay reachable for renewal. An email address is optional
  (`--letsencrypt you@example.com`), since Let's Encrypt no longer sends
  expiry emails.
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

### Option C: Docker

Each release publishes an image for amd64, arm64 and 32-bit ARM (armv7):
`ghcr.io/brandongotak/edgetak`. It is small (distroless, no shell), runs as
a non-root user, and includes MediaMTX for video. Download
[`docker-compose.yml`](docker-compose.yml), set `EDGETAK_PUBLIC_HOST`, and:

```sh
docker compose up -d
docker compose logs edgetak     # the first admin password
```

The compose file's settings apply on every start:

| Variable | Effect |
|---|---|
| `EDGETAK_PUBLIC_HOST` | the address devices use (an IP or a domain); **set this**, since the container cannot see the host's address |
| `EDGETAK_LETSENCRYPT` | `1` (or an email address) for Let's Encrypt on 8446; needs a domain and port 80 |
| `EDGETAK_NO_PLAINTEXT` | `1` turns off ports 8087 and 8080 |
| `EDGETAK_VIDEO` | video is on (the compose file maps 8554, 1935 and 8890/udp); `0` turns it off |
| `EDGETAK_SERVER_NAME`, `EDGETAK_LOG_LEVEL` | the server name; `debug` logging |

Everything else lives in the `edgetak-data` volume (`/data`): users, the CA,
packages, missions and `edgetak.json`. **Update** with
`docker compose pull && docker compose up -d`; the console's update card
does not apply in a container. MediaMTX updates from the Video tab do work,
and are kept in the volume. **Forgotten password** (or a lost two-factor
phone): `docker compose stop`, then
`docker compose run --rm edgetak -reset-password admin` (or `-reset-2fa admin`),
then `docker compose start`. A bind mount instead of the volume must be
writable by user 65532.

Through Docker's port mapping, the admin console's plain-HTTP port (8080) is
not considered local, so use `https://<host>:8443/admin/` (or 8446 with Let's
Encrypt). Raspberry Pis with a 32-bit ARMv6 CPU (Pi Zero, Pi 1) are not
covered by the image; use the installer.

## First steps in the admin console

The console's pages are grouped into five sections in the top bar, with the
current section's pages in the row below:

| Section | Pages |
|---|---|
| Dashboard | Dashboard, Health |
| People | Users, Groups, Sign-ups, Certificates |
| Data | Data Packages, Missions, Maps, Overlays, Plugins, Video |
| Messages | Send, Chat log |
| Server | Connect, Federation, Settings, Webhooks, Audit log, Simulator |

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

### Video

Video is on by default. The installer downloads
[MediaMTX](https://github.com/bluenviron/mediamtx), checks it against a pinned
checksum, and puts it in EdgeTAK's data directory; the Docker image has it
built in. EdgeTAK runs MediaMTX itself (and restarts it if it stops), writes
its configuration, and approves every publish and view, so there are no
separate video accounts.

- **No video:** install with `--no-video` (remembered for later updates), or
  set `EDGETAK_VIDEO: "0"` in Docker.
- **Offline devices:** if MediaMTX cannot be downloaded, the install goes on
  without video and the next update tries again. To add it offline, use
  `--video-tarball mediamtx_v1.21.1_linux_<arch>.tar.gz`. `--video` makes a
  failed download stop the install instead.

**Updating MediaMTX.** The **MediaMTX** card on the Video tab shows the
running version and checks GitHub for newer releases. **Update** downloads the
release for this device, verifies it against the release's SHA-256 checksums
and checks it is built for this processor, keeps the current version, and
restarts. If the new version does not start, EdgeTAK goes back to the old one
automatically. **Roll back** returns to the previous version, and the card
also shows MediaMTX's log and has a **Restart** button. Live streams drop for a
few seconds during a restart. The server needs internet access to reach
github.com for updates.

**Publishing.** Use any EdgeTAK username and password, and any stream name:

| Protocol | Address |
|---|---|
| RTSP | `rtsp://USER:PASSWORD@<host>:8554/NAME` |
| RTMP (DJI, ATAK UAS tool) | `rtmp://<host>:1935/NAME?user=USER&pass=PASSWORD` |
| SRT | `srt://<host>:8890?streamid=publish:NAME:USER:PASSWORD` |

**Apps that cannot log in** (TAK ICU, simple cameras): on the Video tab, set
**Publishing without a login** to *Only to feeds marked…*. Then add a feed
with a stream name, its groups, and **Allow publishing without a login**
ticked. The app publishes to `rtsp://<host>:8554/NAME` with no username or
password, and only those stream names accept that. *Any stream name* is
also available, but it lets anyone who can reach the server publish.
Watching always needs a login or a device's personal link.

A live stream appears on the **Video** tab right away, visible to the
publisher's groups. **Save as feed** gives it a name, chooses who can watch,
and can give it a position. MPEG-TS sent inside RTSP, which UAS and ISR
cameras use, is unpacked automatically.

**Watching.**
- **ATAK and WinTAK:** open **Video Player** and import from the server.
  Feeds with a position also appear as camera markers; tap one to play it.
  Each user's links carry a personal read-only token, never their password.
  Deleting the user revokes the token.
- **Admin console:** click **Watch** on the Video tab, or on a camera marker
  on the map. The video is relayed over the console's HTTPS port with a few
  seconds of delay. Browsers play H.264, not H.265.

**Recording** (Video tab, off by default):
- **What gets recorded:** *Saved feeds marked Record this stream*, or *Every
  stream*. Streams are recorded while they are live, into
  `<data>/recordings/<stream>/` in 10-minute parts.
- **How long they're kept:** a number of days you set (7 by default; 0 keeps
  them until deleted). The media server deletes older recordings itself.
- **In the console:** the **Recordings** card lists each stream's recordings
  with their start time, length and size. **Play** plays a recording part by
  part, with seeking; **Download** saves it as one MP4; **Delete** removes it.
- **Full disk:** recording pauses by itself when the disk is 95% full, shown
  on the Video and Health tabs, and resumes below 90%. On a Raspberry Pi,
  1080p at about 4 Mbit/s fills roughly 1.8 GB per hour per stream.

External cameras or media servers can be added with their own URL (`rtsp://`,
`rtmp://`, `srt://`, `http(s)://`). Devices can watch those, but the console
cannot play them. Bandwidth is the main limit: every viewer receives a full
copy of the stream, about 2 to 4 Mbit/s for 1080p.

### Directory sign-in (LDAP / Active Directory)

On the **Users** tab, **Directory sign-in** lets people sign in with their
directory account: the console, the account page, and TAK app enrollment
all accept their directory username and password.

- **Settings:** the server (`ldaps://dc1.example.com:636`, or `ldap://` with
  StartTLS), the directory's CA certificate if it isn't from a public CA, a
  search account (bind DN and password; leave empty for anonymous search),
  where users are (base DN) and, if needed, a user filter (by default
  `uid`, `sAMAccountName` or `userPrincipalName` matches what was typed).
- **Groups:** a user's `memberOf` groups (Active Directory, OpenLDAP with
  memberOf), or the groups under **Groups are under** that list them as a
  member. Their names become EdgeTAK groups; **Use only these directory
  groups** keeps out the rest (Domain Users and the like). **Admins are
  members of** makes directory admins EdgeTAK admins; **Only members of
  this group may sign in** limits who can sign in.
- **Test these settings** checks the connection, the search account and a
  user's sign-in before saving, and shows the groups found.
- A directory user gets an EdgeTAK record on first sign-in (marked
  *directory* on the Users tab), holding their certificates; groups are
  refreshed at each sign-in and every 30 minutes (needs the search
  account), so devices that only use their certificate follow directory
  changes. Users no longer found are marked *not in directory*; their
  certificates keep working until revoked.
- Directory users change their password in the directory. Local accounts,
  the admin included, keep working alongside, also when the directory is
  unreachable; a local account wins over a directory user with the same
  name.

### Offline maps

The **Maps** tab serves maps from MBTiles files, so the console and ATAK have
maps where there is no internet.

1. **Make or get an MBTiles file** with raster tiles (PNG, JPEG or WebP):
   QGIS (*Generate XYZ tiles (MBTiles)*), MOBAC, TileMill, gdal2tiles,
   `gdal_translate -of MBTiles`, or downloads from map providers. Vector
   tiles (`pbf`) are not supported.
2. **Add it:** upload it on the Maps tab, or copy large files straight into
   the server's `maps` folder (shown on the tab, e.g.
   `/var/lib/edgetak/maps`; readable by the `edgetak` user) and click
   **Rescan the folder**.
3. **Console:** each map is a base map (or an overlay, if its metadata says
   so) named "... (offline)" in the map's layer list; **Show on map** zooms
   to it.
4. **Devices** get each map as an ATAK map source in their device profile
   when they enroll or connect (turn it off per map). ATAK loads the tiles
   from EdgeTAK's HTTPS port with its certificate, and caches what it has
   seen. The map is in ATAK's map layers under its name.

Maps are left out of backups unless **Offline maps** is ticked, since they
can be large and usually can be made again.

### Overlays

The **Overlays** page sends map overlays (boundaries, search sectors,
routes, points) to devices without passing data packages around by hand:

1. **Import** a KML, KMZ or GeoJSON file, name it, and pick the groups
   whose devices get it (none = every device). GeoJSON, which ATAK doesn't
   read, is converted to KML, keeping names, descriptions (or the other
   properties) and [simplestyle](https://github.com/mapbox/simplestyle-spec)
   colors.
2. EdgeTAK offers it to the connected devices in those groups as a data
   package, which ATAK imports as one overlay to show or hide in its Overlay
   Manager. Devices that were off get it when they next connect (once
   they've sent their position); each device is sent each version once.
3. **Replace file** (or a rename) makes a new version: devices get it again
   and it replaces the old one. **Edit** changes the name and groups; newly
   included devices get it straight away. **Send again** re-offers it to
   every connected device that should have it; **Stop sending** pauses it.

**Add as overlay** in the map's **History** dialog makes an overlay of
recorded trails; it isn't sent until you choose its groups and resume it.

**Show on map** draws it on the console map. ATAK has no way to be told to
remove an overlay, so **Delete** only stops EdgeTAK sending it; people
remove it on their device in ATAK's Overlay Manager or Data Packages.

### Plugins

The **Plugins** tab is an update server for ATAK, like the one ATAK's
**Plugins** screen reads from tak.gov:

1. **Upload** plugin or app APKs (several at once). EdgeTAK reads each one's
   name, version, description, icon, minimum Android version and, for
   plugins, the ATAK version it is built for (its plugin API).
2. **Devices** find it at `https://<server>:8443/update`. With **Point
   devices at this update server** on (the default), their device profile
   turns ATAK's update server on with that address when they enroll or
   connect. To set it by hand: in ATAK's **Plugins** screen, open the
   settings, turn on **Update Server** and enter the address.
3. **Installing:** ATAK lists the plugins and apps, and installs or updates
   them over the device's own connection (its certificate, or a username and
   password, which ATAK asks for).

ATAK loads plugins built for its own version or an older one (from 4.10),
never for a newer one, and asks for its own version's list first. Each
device is offered, per plugin, the build for the nearest ATAK version at or
below its own: a device on ATAK 5.7.0 gets a plugin's 5.7.0 build if there
is one, else its 5.6.0 build. Builds for several ATAK versions can be
uploaded side by side; a newer build for the same ATAK version replaces the
old one.

Icons come from the APK: pictures (PNG, JPEG, WebP), and icons Android draws
from XML (vector drawables, adaptive icons, layer lists, shapes), which
EdgeTAK draws itself. Gradients are not drawn, and an icon EdgeTAK cannot
draw shows as a plain placeholder. **Change icon** uploads your own (PNG,
JPEG or WebP); a newer build without a readable icon keeps the icon of the
build it replaces.

#### Plugins from tak.gov

Link a tak.gov account to add the plugins tak.gov publishes without
downloading and uploading them by hand:

1. **Link tak.gov account** shows a code. **Open tak.gov**, sign in and
   confirm the code; the console notices within a few seconds. EdgeTAK never
   sees the password: it keeps only tak.gov's sign-in (refresh) token, which
   tak.gov replaces each time it is used.
2. Pick the **product** (ATAK-CIV, or another your account can see) and
   **ATAK version** from the list, and **Show plugins**. Plugins already on this server
   are marked.
3. **Add** downloads the plugin from tak.gov, checks it against the hash
   tak.gov lists, and adds it to the update server like an upload.

**Unlink** forgets the token (and signs it out at tak.gov); plugins already
added stay. The server needs to reach `tak.gov` and `auth.tak.gov` over
HTTPS. If the account is unused for long enough that tak.gov ends the
sign-in, the console asks to link again.

### Federation

EdgeTAK federates with TAK Servers and other EdgeTAKs using TAK Server
Federation v2 (gRPC, the same protocol TAK Servers use with each other). To
the other server, EdgeTAK looks like another TAK Server.

- **What crosses:** positions, markers, shapes, chat and direct messages.
  Contacts are kept in sync, so direct messages to the other side's users
  arrive. Data Sync missions stay on each server for now.
- **Data packages:** a file uploaded on one server is copied to the other
  (as TAK Servers do, for the groups shared with the federate), and a
  package sent to a user on the other server reaches them: their server
  offers its own copy, so they download it from the server they are
  connected to. Files over 64 MB stay local. Turn it off per federate with
  the **Data packages** box.
- **What each side gets:** per federate, share everything EdgeTAK's devices
  send, or only what is sent to chosen groups. Choose which groups receive
  what the other server sends (none selected = every device).

**Connecting to a TAK Server** (Federation tab, **Connect to a server**):
1. **Address and sign-in:** enter its address and choose how EdgeTAK signs
   in.
   - **Certificate (port 9001):** the standard way, and works with any TAK
     Server. The TAK Server must trust EdgeTAK's CA: open **Set up with a TAK
     Server admin login**, enter an admin's username and password, and click
     **Make the TAK Server trust EdgeTAK**. EdgeTAK uploads its CA as a
     federate CA, with the TAK Server groups you list. Or upload
     **Download EdgeTAK CA** in the TAK Server's Federation Manager yourself.
   - **Federation token (port 9002, TAK Server 5.6+):** paste a token, or
     click **Get a token** with an admin login. The TAK Server's token
     federation must be turned on in its CoreConfig.xml.
2. **Its CA:** click **Get it from the server**, check the fingerprint with
   the TAK Server's admin, and trust it. Or paste its `ca.pem`.
3. **Save:** EdgeTAK connects and reconnects by itself, and the list shows
   the connection's state, message counts, errors and recent messages.

**Letting servers connect to EdgeTAK:**
1. Turn on **Let other servers connect on port 9001**.
2. Open the port: reinstall or update with `--federation`, or with Docker,
   uncomment the `9001` line in `docker-compose.yml`.
3. A server that connects is turned away and listed under **Waiting for
   approval**. Compare its fingerprint with its admin, then **Approve** it.

The other server needs EdgeTAK's CA or certificate fingerprint, both shown
under **This server**. Two EdgeTAKs federate the same way: one connects out,
the other approves it.

**Groups on the TAK Server side.** A TAK Server only passes traffic between a
federate and its users through groups they share, in both directions. With
LDAP, `__ANON__` is usually not one of them: give EdgeTAK's federate one of
your users' LDAP groups. List it under **TAK Server groups for EdgeTAK**, or
set it in the TAK Server's Federation Manager. The TAK Server applies a
federate's groups when it connects, so disable and enable the federate in
EdgeTAK afterwards.

**OpenTAKServer** federates through its federation plugin
(OTS-Federation-Plugin). **Make the TAK Server trust EdgeTAK** works there too, with an OpenTAKServer
administrator login (without two-factor login): it adds EdgeTAK on the
plugin's page. For EdgeTAK to connect, turn on the plugin's federation
listener (port 9001). For OpenTAKServer to connect to EdgeTAK, use the
address its certificate names, or set the plugin's **TLS server name**:
unlike TAK Server, the plugin checks it.

### Webhooks

**Server → Webhooks** sends alerts where people already look, so they hear
about them with the console closed:

Setting one up (the Webhooks page has the same steps):

- **Discord**: on the channel, the gear (Edit Channel) → Integrations →
  Webhooks → New Webhook (needs the Manage Webhooks permission) → Copy
  Webhook URL.
- **Slack**: at api.slack.com/apps, Create New App → From scratch; turn on
  Incoming Webhooks; Add New Webhook to Workspace, pick the channel, Allow;
  copy the `https://hooks.slack.com/services/…` URL.
- **Microsoft Teams**: on the channel, ⋯ → Workflows → "Post to a channel
  when a webhook request is received" → name it, choose the team and
  channel, Add workflow; copy the URL it shows. (Microsoft is retiring the
  old Incoming Webhook connector.)
- **ntfy** (phone notifications): install the ntfy app, subscribe to a topic
  nobody could guess (anyone who knows a topic on ntfy.sh can read it), and
  use `https://ntfy.sh/<topic>` (or your own server,
  `https://user:password@host/topic` if it needs a login). Emergencies
  arrive at priority 5.
- **Any URL**: EdgeTAK posts JSON: `event` (emergency, geofence,
  lost_contact, ended, signup, admin, server or test), `title`,
  `callsign`, `username`, `text`, `lat`, `lon`, `map_url`, `time`,
  `server`. With a signing secret, `X-EdgeTAK-Signature` is `sha256=` and
  the hex HMAC-SHA256 of the body. Answer with a 2xx; 429 and 5xx are
  retried.

Then **Add webhook**, choose the service, paste the URL, choose the alerts,
save and press **Send test**.

Each webhook chooses its alerts (emergencies, geofence alerts, lost
contact, alerts ending, sign-ups, admin changes, server health) and,
optionally, groups: then it only hears about devices of users in those
groups (and gets no sign-ups, admin changes or server health).

**Server health** alerts are about the server itself: the issues on the
Health tab (disk, memory, temperature, CPU, dropped messages, certificates
expiring), plus a federation link down for over 5 minutes, an automatic
backup failing and a new EdgeTAK version (checked daily when the server can
reach GitHub). Each is sent when it starts, again once a day while it
lasts, and when it is resolved.
Messages include the callsign, user, position with a map link, time and
server; ntfy delivers emergencies at top priority. **Send test** checks a
webhook at once. Delivery is retried, and each webhook gets at most 20
messages a minute; more are counted in the next one. The console shows only
each URL's host, since chat services put their token in the URL.

### Audit log

**Server → Audit log** records every change an administrator makes, in the
console or through the admin API: who, from which address, what, to what
(named even after it was deleted), the settings it changed and whether it
worked (refused changes are kept, marked). Sign-ins, failed sign-ins and
device enrollments are recorded too. Passwords, tokens, keys and webhook
addresses never are. Search it, show failures only, page back through it
and download it as CSV. It is kept in `audit/` in the data directory, one
file per month, for as long as **Settings → Data retention** says (forever
by default). A webhook with **Admin changes** ticked hears about each
change as it happens.

### Simulator (load testing)

Simulated TAK devices show how the server copes with many users. Each one
connects to the TLS streaming port like ATAK, walks around a point sending
position reports (and chat, if chosen), and reads everything the server
relays. One device's reports carry a timestamp, so the others measure the
**relay time**: how long the server takes to pass a position on.

- **Console** (Server → Simulator): choose how many devices (up to 5,000),
  how fast they join, how often they report, chat and how long to run, and
  watch connections, events per second, relay times and the server's CPU
  and memory live. The devices sign in as a temporary account in the
  **Simulator** group (or another you name), so real devices don't see
  them; the account and its certificate are removed when the run ends.
  They run inside EdgeTAK, sharing the machine's CPU.
- **Another computer**: `edgetak sim` runs the same simulator over the
  network. Create an account in a test group, then:

  ```
  edgetak sim -server tak.example.org -user loadtest -lat 39.74 -lon -104.99 -devices 500 -duration 5m
  ```

  The password comes from `-password` or `EDGETAK_SIM_PASSWORD`. It prints
  statistics every few seconds and a summary at the end (exit status 1 if
  any connection failed or dropped). Add `-insecure` when the enrollment
  port uses EdgeTAK's own certificate, `-tcp` for the plain TCP port, and
  see `edgetak sim -h` for the rest. It works against any TAK server.

Every device in a group receives every other device's reports, so traffic
grows with the square of the devices: 300 devices reporting every 2
seconds is about 150 reports a second in and 45,000 events a second out.
Turn position history off first if you don't want the simulated devices
recorded.

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

- **Upgrade from the console:** the **EdgeTAK updates** card on the Dashboard
  checks GitHub for new releases and shows the release notes. **Update**
  installs one: a small root updater, set up by the installer, downloads that
  release's installer, verifies it against the release's checksums, and
  reruns it with this server's options. EdgeTAK restarts, and the page reloads
  when it is back. Servers installed before v0.2.15 need one manual upgrade
  (below) to get this.
- **Upgrade by hand:** rerun the one-line install (or a newer installer file).
  The installer remembers the options from the last install in
  `/etc/edgetak/install.conf`, so flags can be left out; flags you give
  override them. Users, groups, certificates, packages and missions are kept.
  Reload the admin console with Ctrl+F5 afterwards.
- **Back up and restore** (Settings tab). A backup is one `.tar.gz` file with
  everything needed to bring the server back: users, groups, the CA and
  certificates, settings, device profiles, federation, data packages,
  missions, plugins and the chat log, plus the configuration; position
  history, video recordings and offline maps are optional. `certs/ca-key.pem` is the key
  every enrolled device trusts, so a restored server is the same server to
  them: devices reconnect without enrolling again. The backup holds that key
  and password hashes: keep it as safe as the server, and somewhere else.
  - **Download a backup** any time, or turn on **automatic backups** (daily,
    kept on the server in `backups/`, the newest 7 by default).
  - **Restore** an uploaded backup or a kept one: EdgeTAK checks it, restarts
    and puts it in place before opening its data. What it replaced is kept in
    `.pre-restore` in the data folder until the next restore. Sign in with the
    restored server's admin password (`-reset-password` if it is lost).
  - **From the command line:** `edgetak -data /var/lib/edgetak -backup FILE`
    (EdgeTAK may be running), and `-restore FILE` with EdgeTAK stopped, as
    the `edgetak` user (`sudo -u edgetak`).
- **Move a manual install into the service:** `--import-data <old data dir>`.
- **Uninstall:** `sudo sh /usr/local/lib/edgetak/edgetak-installer.sh --uninstall`
  keeps the data; add `--purge` to delete it and the `edgetak` user. If you
  installed from a copied file, run that file with `--uninstall` instead.
- **Forgotten password:** stop EdgeTAK, then
  `sudo -u edgetak /usr/local/bin/edgetak -data /var/lib/edgetak -reset-password admin`
  (manual run: `./edgetak -data ./data -reset-password admin`), then start it again.
- **Two-factor login** (optional, for every user).
  - **Turning it on:** on the account page (or, for administrators, the
    **Users** tab), click **Turn on two-factor login**. Scan the QR code with
    an authenticator app (Google or Microsoft Authenticator, Authy, 1Password,
    ...) and save the 10 single-use recovery codes.
  - **Effect:** signing in then asks for a code. TAK apps keep signing in with
    the password, since they can't ask for a code; for administrators, that
    password alone no longer opens the admin API.
  - **Lost phone:** use a recovery code, ask an admin to click **Reset 2FA**
    on the Users tab, or stop EdgeTAK and run
    `sudo -u edgetak /usr/local/bin/edgetak -data /var/lib/edgetak -reset-2fa USER`.

## Ports

| Port | Purpose |
|---|---|
| 8089 | CoT streaming, TLS with client certificate |
| 8443 | Marti API and admin console (client certificate or password) |
| 8446 | Certificate enrollment; with Let's Encrypt also the trusted admin console |
| 80 | Let's Encrypt challenges (only with `--letsencrypt`) |
| 9001 | Federation: other servers connecting here (only when turned on, firewall with `--federation`) |
| 8087 | CoT streaming, plain TCP, anonymous (off with `--no-plaintext`) |
| 8080 | Marti API, plaintext, for 8087 clients; admin console from localhost only (off with `--no-plaintext`) |
| 8554 | Video: RTSP (not with `--no-video`) |
| 1935 | Video: RTMP (not with `--no-video`) |
| 8890/udp | Video: SRT (not with `--no-video`) |

With video on, MediaMTX's API (9997), its HLS server (8888) and EdgeTAK's
video auth endpoint (8099) listen on localhost only. `video.bind_host` in the
config limits the stream ports to one address, such as a VPN interface.

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
| `cert_validity_days` | `365` | client certificates; an admin can change it on the Certificates tab, which then takes precedence |
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
internal/federation/ TAK Server Federation v2 (gRPC): federates, listener, CoT and contacts
internal/apps/      ATAK update server: APK reading, product.inf(z) indexes
internal/maps/      offline maps: MBTiles reading and tiles
internal/overlay/   map overlays: KML/KMZ checks, GeoJSON to KML, data packages
internal/sim/       simulated devices for load testing (console and edgetak sim)
internal/audit/     the audit log: monthly JSON-lines files, search
internal/backup/    backup files, checking, staged restore
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
- Packaging: deb/rpm, Windows service

## License

Copyright (C) 2026 the EdgeTAK contributors.

EdgeTAK is free software: you can redistribute it and/or modify it under the
terms of the [GNU Affero General Public License](LICENSE) as published by the
Free Software Foundation, version 3 of the License. It is distributed in the
hope that it will be useful, but WITHOUT ANY WARRANTY; without even the
implied warranty of MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See
the license for details.

**Running a modified EdgeTAK for others:** the AGPL's section 13 requires
offering your modified source code to the people who use it over the network.
The admin console and sign-up page link to the source; if you run a fork,
point those links (in `internal/web/static/index.html` and `register.html`)
at your fork's source.

EdgeTAK includes or uses these third-party components under their own
licenses, all compatible with the AGPL:

| Component | Use | License |
|---|---|---|
| [golang.org/x/crypto](https://pkg.go.dev/golang.org/x/crypto), x/net, x/text | password hashing, ACME, TLS | BSD-3-Clause |
| [google.golang.org/protobuf](https://pkg.go.dev/google.golang.org/protobuf) | TAK protobuf | BSD-3-Clause |
| [grpc-go](https://pkg.go.dev/google.golang.org/grpc), genproto | federation | Apache-2.0 |
| [go-pkcs12](https://pkg.go.dev/software.sslmate.com/src/go-pkcs12) | client certificates | BSD-3-Clause |
| [androidbinary](https://github.com/shogo82148/androidbinary) | reading APKs (Plugins) | MIT |
| [go-ldap](https://github.com/go-ldap/ldap) | directory sign-in | MIT |
| [modernc.org/sqlite](https://pkg.go.dev/modernc.org/sqlite) | reading MBTiles maps | BSD-3-Clause |
| [golang.org/x/image](https://pkg.go.dev/golang.org/x/image) | WebP plugin icons | BSD-3-Clause |
| [Leaflet](https://leafletjs.com/) 1.9.4 | console map | BSD-2-Clause |
| [milsymbol](https://github.com/spatialillusions/milsymbol) 2.2.0 | MIL-STD-2525 symbols | MIT |
| [JSZip](https://stuk.github.io/jszip/) 3.10.1 | reading data packages | MIT (dual MIT/GPLv3) |
| [toGeoJSON](https://github.com/mapbox/togeojson) 0.16.0 | KML on the map | BSD-2-Clause |
| [qrcodejs](https://github.com/davidshimjs/qrcodejs) 1.0.0 | QR codes | MIT |
| [hls.js](https://github.com/video-dev/hls.js) 1.7.2 | console video player | Apache-2.0 |
| [MediaMTX](https://github.com/bluenviron/mediamtx) | media server: downloaded by the installer, included in the Docker image | MIT |

The bundled libraries' license texts are in
[`internal/web/static/vendor/`](internal/web/static/vendor/).
