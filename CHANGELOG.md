# Changelog

What changed in each EdgeTAK release. The release workflow publishes each
version's section as its GitHub release notes, and the console's update card
shows them, so add a section here before tagging a release.

From v0.3.0, versions follow [Semantic Versioning](https://semver.org/):
new features bump the minor number, fixes the patch number, and while below
1.0, changes that need attention when upgrading bump the minor number too,
with an **Upgrading** section. Releases up to v0.2.24 bumped only the patch
number.

## v1.7.0 (2026-10-23)

**Aircraft and ships on devices' maps.** A new **Aircraft and ships** card
on the Settings page sends live ADS-B aircraft and AIS ships around an area
(up to 250 nautical miles) to devices as map markers, to the groups you
choose:

- **Aircraft** from a free community ADS-B API (adsb.lol or adsb.fi, no
  account), as civil or military aircraft and helicopters, with callsign,
  type, registration, altitude, speed, squawk and emergencies.
- **Ships** from aisstream.io's live feed (free API key), by kind (cargo,
  tanker, passenger, fishing, pleasure craft...), with name, speed,
  heading, destination and status.
- Both show on the console map as the **Aircraft** and **Ships** layers.
  They aren't federated or kept in position history.

## v1.7.0-rc.2 (2026-10-23)

A release candidate for v1.7.0 (above), to try the aircraft and ship feeds
on real devices, with a real aisstream.io key. (rc.1 wasn't published: a
test failed on the build server.) A pre-release: servers only get it with
**Include pre-releases** ticked on the update card.

## v1.6.0 (2026-10-23)

**ZeroTier from the console.** A **ZeroTier** card on the Settings page puts
the server on a ZeroTier network, so devices anywhere reach it without
opening router ports:

- **Install ZeroTier** installs it on servers set up with the Linux
  installer, through a root-owned helper the installer adds (like the one for
  updates). It installs `zerotier-one` from ZeroTier's apt repository,
  verified against ZeroTier's release signing key pinned in EdgeTAK; no
  script is downloaded and run. If ZeroTier is already installed, the same
  button connects EdgeTAK to it.
- Join and leave networks by ID, see each network's state (including
  "waiting for authorization", with the node ID to authorize in ZeroTier
  Central) and the server's ZeroTier addresses.
- **Use for devices** points the Connect page's packages and QR codes at a
  ZeroTier address (or **open console** there); **Restart EdgeTAK** adds a new address to the server
  certificate.

Servers get the install helper when they update to this version.

**Connection packages include the user's certificate.** An ATAK, WinTAK or
TAKX package downloaded for a user (Connect page with a user picked, the
account page, after signing up) now carries a client certificate for them,
like the iTAK package and the QR code's package, so the app connects without
asking for a password or enrolling. Each package gets its own
certificate, listed on the Certificates tab. A package made without a user
still enrolls, asking for a username and password.

## v1.6.0-rc.2 (2026-10-23)

The second release candidate for v1.6.0: rc.1 plus connection packages with
the user's certificate and **Use for devices** on the ZeroTier card. See
v1.6.0 above. A pre-release: servers only get it with **Include
pre-releases** ticked.

## v1.6.0-rc.1 (2026-10-23)

A release candidate for v1.6.0 (above), to try installing ZeroTier from the
console on a real server. A pre-release: servers only get it with **Include
pre-releases** ticked on the update card.

## v1.5.0 (2026-10-23)

**Packages and QR codes follow the address you opened the console at.** On
a local server (no Let's Encrypt), connection packages, QR codes, sign-up
and invite links, the account page and events now use the address in your
browser's address bar, as OpenTAKServer does, instead of the fixed public
host. A Raspberry Pi whose IP address changes needs no setting: open the
console at its new address and everything made there points at it. Opening
it as `localhost` on the server itself still uses the public host, and
servers with Let's Encrypt keep using their domain (the certificate only
covers that name). The address field on the Connect page still overrides
it.

**iTAK QR codes.** The Connect page's QR code now has an **App** choice:
ATAK or iTAK. With Let's Encrypt, the iTAK code is iTAK's registration QR
code: it carries the server and a one-time code (never the user's
password), so iTAK signs in and enrolls by itself after one scan. On a local
server, where iTAK can't trust the server's certificate yet, it's a link
the iPhone's camera opens to download the iTAK package (with a certificate
for the user), which you add in iTAK with **Upload Server Package**. Both
follow the same rules as ATAK codes: expiry, one device, New phone, and
the live code list.

**Enrollment reply for iPhone clients.** iTAK, and clients that send no
User-Agent, now get the certificates in the enrollment reply as single-line
base64 and the CA twice (`ca0` and `ca1`, as OpenTAKServer sends it), which
iOS reads more easily. iTAK enrolls as before; ATAK, WinTAK and TAKX get the
reply they had. This came from looking into TROP (an iOS TAK client), which
enrolls but still doesn't connect.

## v1.5.0-rc.1 (2026-10-23)

A release candidate for v1.5.0 (above), to try the iTAK QR codes on an
iPhone and check that iTAK still enrolls normally with the enrollment reply
changes from the v1.4.2 candidates (below), which v1.5.0 includes. A
pre-release: servers only get it with **Include pre-releases** ticked.

## v1.4.2 (not released)

Not released on its own: its change is part of v1.5.0. It changed the
enrollment reply for iOS clients while investigating TROP, which still
doesn't connect.

## v1.4.2-rc.2 (2026-10-23)

The second release candidate for v1.4.2: rc.1 (which added `ca1` for TROP but
didn't fix it) plus the certificates on one line for iOS clients. See v1.4.2
above. A pre-release: servers only get it with **Include pre-releases**
ticked.

## v1.4.2-rc.1 (2026-10-23)

A release candidate for v1.4.2, to check the TROP enrollment fix on a real
device (see v1.4.2 above). It is a pre-release: servers only get it with
**Include pre-releases** ticked on the update card.

## v1.4.1 (2026-10-23)

**Fix: plain TCP couldn't be turned on** on servers installed with
`--no-plaintext` (as the Let's Encrypt setup suggests): the Settings switch
was greyed out, because those servers have no TCP address in their
configuration. It now turns on there too, on the usual port 8087, and the
choice is kept. Those installs left 8087 closed in the firewall, so open it
there as well (for example `sudo ufw allow 8087/tcp`); the card says so.

## v1.4.0 (2026-10-23)

**Turn plain TCP on and off from the console.** A **Plain TCP** card on the
Settings page opens or closes port 8087 (unencrypted, anonymous devices that
only see `__ANON__`) while EdgeTAK runs, with no restart. Turning it off
closes the port and disconnects the devices connected that way; the choice
is kept across restarts. While it's off, the Dashboard shows CoT TCP as off
and TCP connection packages aren't offered. Servers installed with
`--no-plaintext` show it as off in the server configuration.

**Clear the audit log.** **Clear log…** on the Audit log page deletes every
entry, after a confirmation. The clearing itself becomes the first entry of
the new log (who, when, how many entries were removed) and goes to webhooks
with **Admin changes** ticked, so a cleared log is never a silent one.

## v1.3.0 (2026-10-23)

**Help in the console.** A new **Help** section in the top bar explains every
page of the console: what it is for, the steps for common tasks, and tips,
from getting started and connecting devices to federation and the
simulator. Opening Help from a page shows that page's topic; a search box
finds topics, and each topic has a button that opens its page.

**See what each QR code set up.** The code list on the Connect page now
updates live and shows, for each code, the certificate it issued and the
device using it: its callsign and whether it is online, or **never
connected** (the sign of a phone that was already connected to the server as
someone else: restart ATAK). **Disconnect** revokes the certificates a code
issued, cutting those devices off; **Revoke** still just stops the code.

**New phone QR codes.** Tick **New phone: replace their devices** when making
a QR code for someone whose phone was lost or replaced. After you confirm,
nothing changes until the code is scanned; then the user's other devices are
disconnected and their certificates revoked, as the new phone connects. It
works for both kinds of QR code (Let's Encrypt quick connect and local
packages), is recorded in the audit log, and users can do the same from
their account page. It isn't offered for an event's shared account, whose
devices all use one account.

## v1.2.0 (2026-10-23)

**QR codes for local servers.** The user QR code from v1.1.0 now works on
servers without Let's Encrypt too. ATAK's quick connect only enrolls with a
server whose certificate it already trusts, so on those servers the QR code
instead makes ATAK download a connection package (over the plain HTTP port,
8080) holding the EdgeTAK CA and a client certificate issued for the user.
ATAK imports it and connects: one scan, no typing.

- The Connect page picks the right kind of QR code by itself.
- The package only sets the trust for its own connection, so other servers
  on the phone are left alone.
- Each download's certificate is listed on the Certificates tab (where it
  can be revoked) and recorded in the audit log.
- The same rules as before apply to the code: expiry, one device only, and
  revoking.
- With plain HTTP turned off, the QR code only works on phones that already
  imported a connection package from the server.

**The other QR codes work on local servers too**, the same way:

- **Account page:** users get a QR code for one device (working for an
  hour) with one click, without typing their password.
- **After signing up:** the page shows a QR code for the new account (one
  device, 24 hours).
- **Shared events:** the event's QR code works until the event ends.

**Printable QR sheet.** **Print a QR sheet…** on the Connect page or above
the Users list: pick users (or a whole group), how long the codes work and
whether each is for one device, and print a page of labelled QR cards to
hand out at a briefing.

## v1.2.0-rc.2 (2026-10-23)

The second release candidate for v1.2.0: rc.1 plus QR codes on the account
page, after signing up and for shared events that work on local servers,
and the printable QR sheet. See v1.2.0 above. It is a pre-release: servers
only get it with **Include pre-releases** ticked on the update card.

## v1.2.0-rc.1 (2026-10-23)

A release candidate for v1.2.0, for testing QR codes on a local server
(without Let's Encrypt) on a real phone: the user QR code makes ATAK import
a connection package with a client certificate for the user, so it connects
with one scan and no typing. See v1.2.0 above for the details. It is a
pre-release: servers only get it with **Include pre-releases** ticked on the
update card, and the Docker `latest` tag doesn't move.

## v1.1.0 (2026-10-23)

**Quick-connect QR codes without the password.** The Connect page's
quick-connect QR code no longer asks for a username and password: pick a
user (or click **QR** next to them on the Users page) and the code is ready
to scan in ATAK. It holds a one-time enrollment code rather than the
password, so the admin never needs to know it:

- The code only enrolls a device; it doesn't sign in to the console, the
  account page or any other API.
- Choose how long it works (1 hour to 30 days, 24 hours by default) and
  whether it's for one device only (it then stops 10 minutes after its
  first use).
- Codes are listed under the QR code with their status and can be revoked.
  Making, revoking and using a code is recorded in the audit log.

## v1.0.0 (2026-10-23)

EdgeTAK 1.0: a complete TAK server in one small program, for a Raspberry Pi
in the field or a server on the internet. The same code as v1.0.0-rc.2,
which passed the device checklist on real ATAK, iTAK and WinTAK devices.

**What 1.0 means:** upgrades keep users, groups, passwords, certificates,
data packages, missions and settings, and enrolled devices keep working
without enrolling again; backups restore into the same or later releases;
from here on, changes that would break a working setup wait for 2.0 (see
*Upgrading and compatibility* in the README). Every release is checked
against an upgrade from the very first one before it is published.

**In 1.0:**
- **Connecting:** CoT over TLS and TCP (XML and TAK protobuf), certificate
  enrollment, quick connect with Let's Encrypt, ATAK Channels.
- **People:** users and groups with directions, device profiles,
  self-registration, events with expiring accounts, disabling users, LDAP /
  Active Directory, two-factor login, login protection, an audit log.
- **Data and maps:** data packages, Data Sync missions, a live map with
  drawings, overlays, map sources, offline maps, weather and terrain,
  position history with replay and export, an ATAK update server with
  plugins from tak.gov, video with MediaMTX and recording.
- **Messages and alerts:** sends now or on a schedule, direct messages, a
  chat log, emergency, lost contact and geofence alerts, webhooks to
  Discord, Slack, Teams, ntfy or any URL.
- **Running it:** federation with TAK Servers, OpenTAKServer and other
  EdgeTAKs; health, certificate status, data retention, backup and restore,
  updates from the console (pre-releases optional), and a load-test
  simulator.

## v1.0.0-rc.2 (2026-10-23)

The second release candidate for EdgeTAK 1.0, replacing rc.1: everything in
v0.23.0 (including **Include pre-releases** on the update card) under the
1.0 version number. Test it with the device checklist
(`docs/device-checklist.md`). It is a pre-release: servers only get it with
**Include pre-releases** ticked (on v0.23.0 or later), with the one-line
install (`sudo EDGETAK_VERSION=v1.0.0-rc.2 sh`), or with the image
`ghcr.io/brandongotak/edgetak:v1.0.0-rc.2`.

## v0.23.0 (2026-10-23)

### Added
- **Include pre-releases** (Dashboard → EdgeTAK updates, off by default):
  the console offers release candidates for testing (such as
  v1.0.0-rc.1) as updates too, marked *pre-release*, with a warning before
  installing one. Leave it off on servers people rely on. A server on a
  release candidate is offered the release when it is out.
- **Compatibility promise** (README, *Upgrading and compatibility*): upgrades
  keep users, groups, passwords, certificates, packages, missions and
  settings, and enrolled devices keep working without enrolling again;
  backups restore into the same or later releases; from 1.0, changes that
  would break a working setup wait for 2.0.
- **Upgrade tests** in the release workflow: the oldest release, filled with
  data, upgraded in place, and a backup from the first release with backups
  restored, checked before every release is published.
- **Device checklist** (`docs/device-checklist.md`) for real ATAK, iTAK and
  WinTAK devices before major releases.

### Changed
- **README** reorganised for new users: install, connecting devices and
  first steps, then a guide arranged like the console.
- Release candidates (`-rc.N`) are published as GitHub pre-releases and
  don't move the Docker `latest` tag.

### Upgrading
- Updating to v0.23.0 from the console also updates the server's updater,
  which then accepts pre-release versions; so after this update, turning on
  **Include pre-releases** is all a test server needs.

## v1.0.0-rc.1 (2026-10-23)

The first release candidate for EdgeTAK 1.0: the features are complete and
frozen; this candidate is for checking on real devices before 1.0.0. It is a
pre-release, so servers don't update to it on their own and the Docker
`latest` tag doesn't move. To try it: `sudo EDGETAK_VERSION=v1.0.0-rc.1 sh`
with the one-line install (or rerun `edgetak-installer.sh` from this
release), or the image `ghcr.io/brandongotak/edgetak:v1.0.0-rc.1`. A server
on a release candidate is offered 1.0.0 when it is out.

### Added
- **Compatibility promise** (README, *Upgrading and compatibility*): upgrades
  keep users, groups, passwords, certificates, packages, missions and
  settings, and enrolled devices keep working without enrolling again;
  backups restore into the same or later releases; from 1.0, changes that
  would break a working setup wait for 2.0.
- **Upgrade tests** in the release workflow: the oldest release, filled with
  data, upgraded in place, and a backup from the first release with backups
  restored, checked before every release is published.
- **Device checklist** (`docs/device-checklist.md`) for real ATAK, iTAK and
  WinTAK devices before major releases.

### Changed
- **README** reorganised for new users: install, connecting devices and
  first steps, then a guide arranged like the console.
- Release candidates (`-rc.N`) are published as pre-releases.

### Fixed
- A server running a release candidate is offered the final release as an
  update (the version check saw them as the same).

## v0.22.1 (2026-10-23)

No changes to EdgeTAK since v0.21.2. v0.22.0 was never released: its
Docker image (`ghcr.io/brandongotak/edgetak:v0.22.0`, briefly also
`latest`) was built from a release-signing change that has been taken back
out. This release makes `latest` match a published release again; Docker
servers that pulled the v0.22.0 image should pull `latest` (or `v0.22.1`).

## v0.21.2 (2026-10-23)

### Fixed
- **Lost contact alerts**: turning them off (Server → Settings → Lost
  contact alerts, untick *Alert when contact is lost*) now also ends the
  alerts already open, so they leave the console, map and dashboard at
  once (recorded as ended by you; no "back in contact" webhooks). The card
  shows On or Off, and its options are greyed out while it is off.

## v0.21.1 (2026-10-22)

Security fixes from a review of the whole codebase.

### Security
- **Console map**: a data package's file name, a mission's name or an
  offline map's name could carry HTML that ran in an administrator's
  browser when its layer appeared in the map's layer menu (Leaflet shows
  layer names as HTML). Any device user could name a data package. Layer
  names are now shown as text.
- **Video sign-in**: passwords tried on the video ports (RTSP, RTMP, SRT)
  are now covered by login protection, which they bypassed.
- **Passwords over plain HTTP**: usernames and passwords sent to the plain
  HTTP port from another machine (device API basic auth, ATAK's OAuth
  sign-in) are refused; they would cross the network unencrypted. TLS
  ports, and requests from the server itself, are unchanged.
- **Clickjacking**: pages can no longer be shown inside another site's
  frame (`X-Frame-Options: DENY`, `frame-ancestors 'none'`), and console
  addresses aren't sent to other sites (`Referrer-Policy: same-origin`).
- **First-run password**: `initial-admin-password.txt` is deleted once the
  admin password is changed.
- **Dependencies**: built with Go 1.26.10 and gRPC 1.84.0, fixing known
  vulnerabilities in the Go standard library (HTTP, TLS, X.509, XML,
  templates) and in gRPC (including an authorization bypass affecting the
  federation listener).

## v0.21.0 (2026-10-22)

### Added
- **Login protection** (Server → Settings, on by default): an address with
  10 failed password sign-ins in 15 minutes (adjustable) is blocked from
  password sign-ins for 30 minutes: console and account page, device
  enrollment and the device API's username/password requests, and ATAK's
  username sign-in. Wrong two-factor codes count. Certificate-connected
  devices and existing sessions are not affected; this machine is never
  blocked.
  - **Never block** list (one click for your address or all private
    networks) and **Always refuse** list (refused everything, streaming
    ports included).
  - Blocked addresses and recent failures, with **Unblock**; blocks go to
    the audit log and to webhooks with **Blocked sign-ins** ticked.

### Upgrading
- Login protection is on after the upgrade. If all connections reach
  EdgeTAK from one address (Docker Desktop on Windows or macOS, or a
  reverse proxy), add that address under **Never block**.

## v0.20.2 (2026-10-22)

### Fixed
- **Plugins**: an app whose APK declares ATAK's plugin API (common for apps
  built from ATAK's plugin template) was offered to ATAK as a plugin. Each
  APK now has **Treat as app** / **Treat as plugin**, which overrides the
  detection; apps are offered to every ATAK version. The list says
  "Plugin" or "App" and when you set it.

## v0.20.1 (2026-10-22)

### Changed
- **Webhooks**: step-by-step setup instructions for Discord, Slack,
  Microsoft Teams, ntfy and any URL (JSON, with the payload and how to
  check its signature), on the Webhooks page and in the Add webhook dialog
  for the service chosen.

## v0.20.0 (2026-10-22)

### Added
- **Webhooks** (Server → Webhooks): alerts sent to **Discord**, **Slack**,
  **Microsoft Teams**, **ntfy** (phone notifications) or **any URL**
  (JSON, optionally signed with HMAC-SHA256).
  - Choose the alerts: emergencies, geofence alerts, lost contact, alerts
    ending (cancelled, dismissed, back in contact) and sign-ups; optionally
    only about devices in chosen groups.
  - Messages carry the callsign, user, position with a map link, time and
    server, formatted for each service; ntfy delivers emergencies at top
    priority.
  - **Send test**, last result and counts per webhook; retries, and at most
    20 messages a minute per webhook (the rest are counted in the next).
  - Webhook URLs and secrets are never shown back in the console.
- **Server health alerts**: webhooks with *Server health* ticked hear when
  the server needs attention: disk, memory, temperature, CPU or dropped
  messages; certificates expiring; a federation link down for over 5
  minutes; an automatic backup failing; a new EdgeTAK version. Sent when it
  starts, daily while it lasts, and when resolved. Federation links down,
  backup failures and updates also show on the Health tab.
- **Scheduled sends** (Messages → Send): a message or data package at a set
  time, once or repeating (every few hours, daily, weekdays, chosen days),
  to everyone or chosen groups, with an optional end date. Send now, pause
  and resume; runs missed while EdgeTAK was off are skipped, not sent late.
- **Audit log** (Server → Audit log): every change an administrator makes
  (console or admin API), with who, from where, what, the target and the
  settings changed, and whether it worked; plus sign-ins, failed sign-ins
  and device enrollments. Secrets are never recorded. Search, failures
  only, paging and CSV download; kept per **Data retention** (forever by
  default). Webhooks can send **Admin changes** as they happen.

## v0.19.0 (2026-10-22)

### Added
- **Simulator** (Server → Simulator) for load testing: up to 5,000
  simulated TAK devices connect to the TLS streaming port, walk around the
  map's centre reporting positions (and chat, if chosen) and read
  everything relayed. Live connections, events per second, relay time
  (median, 95%, worst), failures and the server's CPU and memory. The
  devices use a temporary account in the Simulator group, so real devices
  don't see them; it is removed when the run ends.
- **`edgetak sim`**: the same simulator from another computer, over the
  network, against EdgeTAK or any TAK server (enrolls with a username and
  password, or uses the plain TCP port). Prints statistics as it runs and
  a summary at the end.

## v0.18.0 (2026-10-22)

### Added
- **History export** (map → History → Export trails): download the chosen
  devices' trails for a time range as **KML** (Google Earth, ATAK) or
  **GPX** (GIS and GPS tools). Trails are split where a device went quiet
  for more than 5 minutes, coloured by team, with start and end points.
- **Add as overlay** turns those trails into an overlay on the Overlays
  page, to send to the groups you choose (it isn't sent until you resume
  it).
- **Missions on the map** (Data → Missions → Show on map): a Data Sync
  mission's map items and the files in it the map can read (KML, KMZ,
  GeoJSON, CoT, data packages) drawn as one layer on the console map, drawn
  fresh each time so it shows the mission as it is now.

### Fixed
- Stopped overlays no longer count connected devices as waiting for them.

## v0.17.0 (2026-10-22)

### Added
- **Overlays** (Data → Overlays): import a KML, KMZ or GeoJSON file once
  and every device in the chosen groups (or every device) gets it as a map
  overlay ATAK can show or hide.
  - Sent as a data package ATAK imports on its own; devices that were off
    get it when they next connect, and each device gets each version once.
  - GeoJSON is converted to KML, keeping names, descriptions and
    simplestyle colors.
  - **Replace file** sends a new version that replaces the old one on
    devices; **Edit** changes the name and groups; **Send again**, **Stop
    sending**, **Download** and **Show on map** on the console.
  - The list shows how many devices have the current version and how many
    connected ones are still waiting for it.
- **Map sources to devices** (map panel → Map sources): tick *Send to ATAK
  devices* on a map source, or pick a built-in map such as Google Hybrid or
  USGS Topo, and it is added to ATAK's map list on the devices in the
  chosen groups (or every device). Connected devices get it straight away;
  others get it in their connection profile when they next connect.

### Fixed
- **Show on map** for data packages opens the map panel again (broken
  since v0.11.0).

## v0.16.1 (2026-10-22)

### Changed
- **Plugins from tak.gov:** the ATAK version is picked from a list (5.5.0
  and later) instead of typed, and picking one shows its plugins straight
  away.

## v0.16.0 (2026-10-22)

### Added
- **Plugins from tak.gov** (Data → Plugins): link a tak.gov account and add
  its plugins to the update server in one click.
  - **Linking** uses tak.gov's device sign-in: the console shows a code,
    you confirm it at tak.gov. EdgeTAK keeps only the refresh token (never
    the password); **Unlink** forgets it and signs it out at tak.gov.
  - **Browse** the plugins tak.gov lists for a product and ATAK version
    (defaulting to the newest version this server has plugins for), with
    their icons; ones already on the server are marked.
  - **Add** downloads the APK, checks it against tak.gov's hash, and adds
    it like an upload; tak.gov's icon is used when the APK has none.

### Fixed
- On phones, the plugin list's name column no longer splits the name,
  package and description into narrow columns.

## v0.15.0 (2026-10-22)

### Added
- **Events** (People → Events): temporary operations, natively (no LDAP).
  - Creating one makes its **group** and, optionally, its **Data Sync
    mission** (visible to the group).
  - **Joining with one QR code**, chosen per event: *each person signs up*
    (the code opens the sign-up page, everyone gets their own account and
    ATAK quick-connect code; optional people limit) or *one shared account*
    (the code enrolls ATAK straight away). **Print a join sheet** for the
    check-in point.
  - **At the end time** every event account is erased (certificates
    revoked, devices disconnected), the group and invite removed, and the
    mission kept for review. **Change end** extends or shortens it; **End
    now** ends it early.
  - Event accounts are labelled on the Users page, and the sign-up page
    tells people when their account will be removed.

## v0.14.0 (2026-10-22)

### Added
- **Disable users** (People → Users): a disabled user can't sign in
  anywhere (console, account page, enrollment, API), their certificates stop
  working, and their devices and console sessions are cut off at once.
  Their account, groups and certificates are kept, so **Enable** brings
  everything back. Works for directory (LDAP) users too, without asking the
  directory. You can't disable your own account.

## v0.13.1 (2026-10-22)

### Changed
- **Tidier console navigation.** The 16 tabs are grouped into five sections
  in the top bar (Dashboard, People, Data, Messages, Server), with the
  current section's pages in a row below. Each section reopens the page last
  used in it, and a section shows a dot when one of its pages has news (an
  update, certificates needing attention). Links to pages (`#users`,
  `#settings`) still work.
- **On phones, the account buttons fold into a ☰ menu** (Install app, My
  account, Sign out), so the server name fits again; **Map** stays on the
  bar.

## v0.13.0 (2026-10-22)

### Added
- **Directory sign-in (LDAP / Active Directory)** on the Users tab. People
  sign in with their directory username and password everywhere: the
  console, the account page, and ATAK, iTAK and WinTAK enrollment.
  - **Groups** come from the directory (`memberOf`, or a group search) and
    become EdgeTAK groups, refreshed at each sign-in and every 30 minutes;
    optionally only chosen directory groups are used.
  - **Admins** and **who may sign in** can follow directory groups.
  - **Test these settings** checks the connection, the search account and a
    user's sign-in before saving, and shows the groups found.
  - LDAPS and StartTLS, with the directory's own CA.
  - Local accounts, the admin included, keep working alongside, also when
    the directory is down.

## v0.12.0 (2026-10-22)

### Added
- **Drawings** (Maps tab): draw on the console map and it appears on
  devices' maps, like items a teammate placed.
  - **Markers** (friendly, hostile, neutral, unknown, or a colored spot),
    **routes** (navigable in ATAK), **areas** and **circles**, each with a
    name, remarks and color.
  - **Who gets them:** every device, or devices in chosen groups.
  - **Persistent:** EdgeTAK keeps them and sends them to each device as it
    connects, so phones that were off when you drew get them too.
  - **Edit or delete** from the console and devices update; EdgeTAK's copy
    is the reference, so changes made on a device are replaced the next time
    it connects.
  - The console map shows them as the **Drawings** layer.

### Fixed
- **Show** on a geofence left an extra copy of the area on the console map.

## v0.11.0 (2026-10-21)

### Added
- **Lost contact alerts** (Settings tab): an amber alert, with a gentler
  alarm than emergencies, the last known position and a marker on the map,
  when a device stops reporting.
  - **Signing off doesn't alert.** EdgeTAK tells how a connection ended: a
    device that turns its server connection off, quits ATAK or shuts down
    closes it cleanly and is only listed under **Devices offline**. A
    connection that goes silent or is cut raises the alert after a grace
    period (5 minutes by default).
  - **Switching networks doesn't alert**: a device back within the grace
    period is fine, and a new connection replacing a dropped one counts as
    back.
  - **Connected without a position** for 10 minutes (adjustable, or off)
    alerts too, for a frozen app or lost GPS.
  - Alerts end by themselves when the device is back, can be acknowledged
    or dismissed like emergencies, and can be limited to chosen groups.
- **Geofences** (Settings tab): draw an area or a circle on the console map
  and get an amber alert, with the device's position, when a device enters
  it, leaves it, or both.
  - **Who:** every device, or devices in chosen groups.
  - **Ends by itself** when the device crosses back.
  - **No alert storm:** a device's first position only notes where it is, so
    drawing an area around people already in it doesn't alert.
  - **On devices' maps** too (as ATAK shapes, on by default per area), sent
    when the area is saved and to devices as they connect; deleting an area
    removes it there. The console map shows them as the **Geofences** layer.

## v0.10.2 (2026-10-21)

### Fixed
- **Plugin icons drawn as vectors now show.** Many plugins have only icons
  that Android draws from XML (vector drawables, adaptive icons, layer
  lists), which EdgeTAK showed as a plain placeholder (WxHazards, Drone
  Hone, RIDAR, REDQUEEN and others). EdgeTAK now draws them: paths with
  fills and strokes, arcs and curves, groups, and colors and layers from
  the app's resources. Plugins already uploaded get their icon when EdgeTAK
  starts (icons set with **Change icon** are kept).

## v0.10.1 (2026-10-21)

### Fixed
- **Plugins built for an older ATAK version are offered to newer ATAK.**
  ATAK runs plugins built for its own version or an older one (from 4.10),
  but EdgeTAK only offered each device the builds for its exact version, so
  a 5.6 plugin did not show on ATAK 5.7. Each device now gets, per plugin,
  the build for the nearest ATAK version at or below its own. The Plugins
  tab shows builds as "ATAK 5.6.0+".

## v0.10.0 (2026-10-21)

### Added
- **Offline maps** (Maps tab): EdgeTAK serves maps from MBTiles files, so the
  console and ATAK have maps without internet access.
  - **Add maps** by uploading them, or by copying large files into the
    server's `maps` folder and clicking **Rescan the folder**. Raster tiles
    (PNG, JPEG, WebP) are supported; vector tiles are not.
  - **Console:** each map appears in the map's layer list as "... (offline)";
    **Show on map** zooms to it.
  - **Devices** get each map as an ATAK map source in their device profile
    when they enroll or connect (per map, on by default), loading tiles from
    EdgeTAK's HTTPS port with their certificate.
  - Backups can include the maps (**Offline maps**, off by default).

## v0.9.0 (2026-10-21)

### Added
- **Backup and restore** (Settings tab). One file holds everything needed to
  bring the server back, including the CA, so devices reconnect to a restored
  server without enrolling again.
  - **Download a backup** any time; position history and video recordings
    are optional.
  - **Automatic backups:** daily, kept on the server, the newest 7 by
    default.
  - **Restore** an uploaded or kept backup: EdgeTAK checks it, restarts, and
    puts it in place before opening its data. What it replaced is kept in
    `.pre-restore`.
  - **Command line:** `-backup FILE` and `-restore FILE`.
- **Change icon** on the Plugins tab: upload your own icon (PNG, JPEG or
  WebP, scaled to ATAK's launcher size) for plugins whose icon EdgeTAK can't
  read (vector and adaptive icons) or that you want to look different. A
  newer build of the plugin without a readable icon keeps it.

## v0.8.0 (2026-10-21)

### Added
- **Plugins tab: an update server for ATAK.** Upload ATAK plugin and app
  APKs, and devices install and update them from ATAK's **Plugins** screen.
  - EdgeTAK reads each APK's name, version, description, icon, minimum
    Android version and, for plugins, the ATAK version it is built for.
  - Each device sees the builds for its ATAK version (ATAK asks for its own
    version's list first), so plugins for several ATAK versions can be
    offered side by side. A newer build for the same version replaces the
    old one.
  - Devices are pointed at it through their device profile when they enroll
    or connect (a setting on the tab, on by default), at
    `https://<server>:8443/update`. They download over their own connection:
    their certificate, or a username and password.

## v0.7.0 (2026-10-21)

### Added
- **Weather and terrain on the console map,** as overlays in the layer
  switcher:
  - **Weather radar:** worldwide, from RainViewer. Its free radar stops at
    zoom 7, so it is coarse when zoomed in.
  - **Weather radar (US, detailed):** NEXRAD reflectivity from the Iowa
    Environmental Mesonet, sharper over the US.
  - Radar refreshes every 5 minutes while shown, and a label on the map
    says how old the picture is.
  - **Hillshade:** the shape of the land (Esri World Hillshade), blended into
    any base map.
  - They need internet access, like the online base maps.

## v0.6.0 (2026-10-21)

### Added
- **Data packages over federation**, with TAK Servers and other EdgeTAKs,
  the way TAK Servers federate them:
  - **Uploads are copied:** a file uploaded on one server is copied to the
    other, for the groups shared with the federate, and appears on its Data
    Packages tab. Device profiles stay local; files over 64 MB too.
  - **Sending to the other side's users works:** a package sent from ATAK to
    a contact on the other server reaches them, and they download it from
    the server they are connected to.
  - **On by default,** with a **Data packages** box per federate to turn it
    off. Files sent and received are listed in the federate's recent
    messages.
  - If a TAK Server announces a package whose file never arrives, the
    federate shows a warning saying what to check on the TAK Server (shared
    groups, and mission federation, which TAK Server allows by default).
- **OpenTAKServer in the admin-login helper.** OpenTAKServer has no TAK
  Server admin API, so **Make the TAK Server trust EdgeTAK** failed there
  with HTTP 404. It now recognizes OpenTAKServer and uses its federation
  plugin (OTS-Federation-Plugin) instead: EdgeTAK is added as an approved
  federate, matched by its certificate, delivering to the groups listed
  (`__ANON__` if none). If the plugin's federation listener is off, it says
  so, since EdgeTAK can't connect until it is on.

## v0.5.1 (2026-10-21)

### Fixed
- **Federation groups are harder to miss.** A TAK Server only passes traffic
  between a federate and devices in the same group, so a federate without
  one connects but shares nothing.
  - **Make the TAK Server trust EdgeTAK** now asks before going ahead with no
    groups, and says where to find your TAK devices' group.
  - A connected federate that has sent for a minute and received nothing
    shows a hint on its card explaining the group fix and the reconnect.

## v0.5.0 (2026-10-21)

### Added
- **Federation** with TAK Servers and other EdgeTAKs, using TAK Server
  Federation v2 (gRPC). To the other server, EdgeTAK looks like another TAK
  Server. On the new **Federation** tab:
  - **What crosses:** positions, markers, chat and direct messages. Contacts
    are kept in sync, so direct messages reach the other side's users, and
    devices are told when someone there leaves.
  - **Connect to a TAK Server or EdgeTAK** with a certificate (port 9001) or
    a TAK Server 5.6 federation token (port 9002).
    - **Its CA:** **Get it from the server** fetches the other server's CA,
      for you to check the fingerprint.
    - **TAK Server admin login:** EdgeTAK can make the TAK Server trust its
      CA, with groups, or get a federation token. The login isn't saved.
  - **Let servers connect to EdgeTAK** on port 9001. Servers you haven't
    approved are turned away and listed with their certificate fingerprint,
    for you to approve.
  - **Per federate:** share everything, or only what's sent to chosen groups,
    and choose which groups receive what the other server sends.
  - **Status:** live connection state, message counts, last error and recent
    messages for each federate. EdgeTAK reconnects by itself.
- **Tested against TAK Server** (with LDAP users), with certificate and
  token sign-in. Positions, chat and contacts crossed in both directions. With
  LDAP, give EdgeTAK's federate one of your users' groups: `__ANON__` usually
  isn't one (see the README).
- Installer option **`--federation`** opens port 9001 in the firewall
  (remembered for updates). `docker-compose.yml` has the port ready to
  uncomment.

### Not yet federated
- Data packages and Data Sync missions, which TAK Server sends on a separate
  stream. EdgeTAK keeps that stream open but ignores it.

## v0.4.0 (2026-10-21)

### Added
- **Device profiles** (Groups tab): settings and files for the TAK devices of
  chosen groups' members (or of every user), as on the official server.
  - **Settings:** ATAK settings entered as key, type and value, sent as a
    `.pref` file.
  - **Files:** `.pref` files, plugin APKs, map source XML, overlays and
    anything else ATAK imports, up to 512 MB each.
  - **When:** when a device enrolls, and when it connects (only profiles
    changed since its last sync). **Send now** offers a profile to its users'
    connected devices.
  - **Controls:** each profile can be turned off, and the list shows how many
    users and connected devices it reaches. **Download** gives the exact
    package devices receive.
  - **Groups:** renaming a group keeps its profiles pointed at it.
- **Choose how long device certificates are valid,** on the Certificates
  tab: "New certificates are valid for N days" (1 to 3650; 365 by default).
  - **Where it applies:** certificates issued from then on, through
    enrollment, iTAK packages and **Issue .p12**. Existing certificates keep
    their dates.
  - **Enrollment:** the validity the server reports to enrolling TAK clients
    follows the setting.
  - **CA limit:** a certificate never outlives the certificate authority that
    signed it; the page says so if the CA expires sooner.
  - **Config file:** the setting takes precedence over `cert_validity_days`.

### Fixed
- Disabled buttons in the console looked the same as enabled ones.

## v0.3.0 (2026-10-21)

The first release to follow semantic versioning (see the top of this file).

### Added
- **Emergency alerts.** When someone presses ATAK's emergency button (911
  Alert, Ring The Bell, In Contact, Geo-fence Breached or a custom alert):
  - **Banner:** every open console shows a red banner on every tab, with the
    callsign, user, time and position.
  - **Alarm:** sounds until an admin clicks **Acknowledge**. Browsers only
    play sound after the page has been clicked once; the banner says so.
  - **Desktop notification,** when turned on under Settings → Emergency
    alerts.
  - **Map and Dashboard:** a pulsing red marker with **Show on map**, a
    highlighted Dashboard row, and an issue at the top of the Health tab.
  - **Until cancelled:** the alert stays until the device cancels it, even if
    the device goes offline. An admin can **Dismiss** one that can't be
    cancelled anymore. Alerts still reach devices as before.
  - **Log:** under Settings → Emergency alerts (the last 100), and in the
    system log.
- **Two-factor login for every user.** Users turn it on from their account
  page, with a QR code and 10 recovery codes, and can turn it off or make new
  recovery codes there. Signing in then asks for a code. TAK apps keep using
  only the password, since they can't ask for one. Admins can still **Reset
  2FA** for anyone on the Users tab.

## v0.2.24 (2026-10-21)

### Added
- **Video recording** (Video tab, off by default):
  - **What gets recorded:** *Saved feeds marked Record this stream*, or
    *Every stream*. Streams are recorded while they are live, in 10-minute
    parts in EdgeTAK's data folder.
  - **How long they're kept:** a number of days you set (7 by default; 0
    keeps them until deleted). The media server deletes older recordings
    itself, and the rule also shows under Settings → Data retention.
  - **Recordings card:** each stream's recordings with start time, length and
    size. **Play** plays in the console part by part, with seeking;
    **Download** saves a whole recording as one MP4; **Delete** removes one
    recording or all of a stream's.
  - **Feeds list:** live streams being recorded show **● rec**.
  - **Full disk:** recording pauses by itself when the disk is 95% full
    (shown on the Video and Health tabs) and resumes below 90%.
- **Pick the user for a connection package from a dropdown** on the Connect
  tab, instead of typing the name.
  - **ATAK / WinTAK / TAKX packages:** the username is optional ("they type
    it in the app").
  - **iTAK packages:** a user is required, since the package includes their
    certificate.
  - **TCP packages:** the dropdown is off, since TCP is anonymous.
- **Package** button for each user on the Users tab: opens the Connect tab
  with that user picked.

### Changed
- Downloaded packages are named after the user, such as
  `EdgeTAK-tak.example.com-alice-ssl.zip`.

## v0.2.23 (2026-10-21)

### Added
- **One sign-in page for everyone** at `https://<host>:8446/`.
  - **Where you land:** administrators go on to the admin console, everyone
    else to their account page. One sign-in covers both, and signing out of
    either ends both.
  - **Old links keep working:** opening `/admin/` or `/account` while signed
    out leads to the sign-in page and back. A user who opens an admin link
    lands on their account page.
  - **Admins** get a **My account** link in the console's top bar.
- **Account page for users** at `https://<host>:8446/account`. Every user,
  not only admins, can use it to:
  - **Connect:** get their connection packages (ATAK, WinTAK, TAKX and iTAK)
    and the ATAK quick-connect code. The code holds the password, so it's
    only shown after they type the password.
  - **See their devices:** what's connected with their account right now,
    with battery and last report.
  - **See their certificates:** each with its expiry and a warning when it's
    expiring, plus **Revoke** for a lost device, which disconnects it.
  - **Change their password,** which signs them out of every other session.
  - **See their groups.**
- **Security:**
  - **No back door:** a user's sign-in never gives access to the admin
    console or the TAK APIs. The account session only works on `/account`,
    and only administrators get a console session.
  - **Two-factor:** accounts with two-factor login need their code.
  - **Failed sign-ins:** after repeated wrong passwords from one address,
    sign-in is blocked for a few minutes, now for the console too.
- **Finding the page:** the console's Connect tab shows the sign-in link to
  share with users, and the sign-up page points new users to their account
  page.

## v0.2.22 (2026-10-21)

### Added
- **Chat log** (Chat log tab, off by default). Records the GeoChat messages
  sent through the server: All Chat Rooms, team and group chats, one-to-one
  messages, and messages sent from the console. You can search them (matches
  are highlighted), filter by conversation, sender and time, and export the
  current view as CSV or JSON for after-action reviews. Both directions of a
  one-to-one chat show as one conversation. Turning recording on asks you to
  confirm, because admins can then read every message.
- A **Chat log** rule in data retention sets how long messages are kept.

### Changed
- Data retention moved from the Dashboard to a new **Settings** tab.
- When the tab bar is too narrow for every tab, it scrolls to keep the
  selected tab in view.

### Fixed
- Small link buttons, such as Export, were drawn at full size.

## v0.2.21 (2026-10-21)

### Added
- **Certificate status** on the Certificates tab:
  - **Server certificates:** the certificate authority, the server
    certificate and the public certificate (Let's Encrypt or your own files),
    each with its expiry date, days left, status and how it renews.
  - **Device certificates:** days left, counts of valid, expiring, expired,
    revoked and replaced certificates, filters, and which connected device
    uses each one. A certificate is **replaced** when the same user and
    device enrolled again since, so old certificates don't raise warnings.
- Adjustable warning period, 30 days by default. The certificate authority is
  always flagged at least 180 days ahead. Let's Encrypt is only flagged when
  automatic renewal looks stuck.
- Certificate warnings on the Dashboard, the Health tab, a count badge on the
  Certificates tab, and in the system log at startup and every 12 hours.

### Fixed
- Row borders in the console's tables lined up badly next to the action
  buttons on desktop.

## v0.2.20 (2026-10-21)

### Added
- **Install the console as an app** on phones and desktops: Install app in
  the top bar (Chrome and Edge), or Share → Add to Home Screen (iPhone and
  iPad). The browser only offers it on a trusted certificate (port 8446 with
  Let's Encrypt).
- **Data retention rules:** how many days to keep data packages, idle
  missions, Inbox messages, pending sign-up requests, old invites and expired
  certificate records (0 = keep forever). Shows what each rule would remove
  before you save, runs every hour, and has Run now. Files a mission uses,
  and records of certificates that are still valid, are never removed.

### Changed
- **Video is on by default** for the installer and Docker.
  - **To turn it off:** use `--no-video` with the installer (remembered for
    updates) or set `EDGETAK_VIDEO=0` in Docker.
  - **Install doesn't stop over video:** if MediaMTX can't be downloaded or a
    video port is already taken, the installer skips video and finishes. Pass
    `--video` to make it stop instead.
  - **Upgrading:** an existing install turns video on at its next update
    unless you rerun the installer with `--no-video`.

## v0.2.19 (2026-10-21)

### Added
- **Docker image** (optional): `ghcr.io/brandongotak/edgetak` for amd64,
  arm64 and arm/v7, with MediaMTX included, plus a `docker-compose.yml`
  example. Containers are set up with `EDGETAK_PUBLIC_HOST`,
  `EDGETAK_SERVER_NAME`, `EDGETAK_LETSENCRYPT`, `EDGETAK_NO_PLAINTEXT`,
  `EDGETAK_VIDEO` and `EDGETAK_LOG_LEVEL`. See "Option C: Docker" in the
  README.
- EdgeTAK is licensed under the **GNU AGPL 3.0**. The console and the sign-up
  page link to the source code.

### Changed
- MediaMTX is only reported as unreachable when it stays down, not during
  the first few seconds after startup.

## v0.2.18 (2026-10-21)

### Added
- **Health tab:**
  - **Server graphs** for the last hour, 6 hours or 24 hours: CPU, memory,
    disk, network, temperature (Raspberry Pi), connected devices, CoT
    messages and video bandwidth. System figures need Linux.
  - **Server warnings** for a full disk, overheating, high memory or CPU, and
    dropped messages, and when video is down.
  - **Device table:** battery with history and drain rate, last report, GPS
    fix, connection and version, worst problems first.

## v0.2.17 (2026-10-21)

### Added
- **Two-factor login** for the admin console (optional, per administrator),
  with an authenticator app.
  - **Setup:** on the Users tab, with a QR code and 10 single-use recovery
    codes.
  - **No way around it:** once it's on, the admin API only accepts a console
    sign-in. Device certificates and passwords still work for TAK clients.
  - **Lost phone:** another admin can reset it, or run
    `edgetak -reset-2fa USER` on the server.

## v0.2.16 (2026-10-21)

### Added
- A dot on the Dashboard tab when a newer EdgeTAK release is available, and
  when the updates card last checked.

## v0.2.15 (2026-10-21)

### Added
- **Update EdgeTAK from the console:** the Dashboard's updates card shows the
  latest release and its notes, and installs it. EdgeTAK itself never gets
  root: a separate root-owned updater downloads and verifies the release
  and reruns the installer.
- The installer remembers its options in `/etc/edgetak/install.conf` for
  reruns and console updates.

### Upgrading
- Console updates work from this version on. Update to v0.2.15 once with the
  installer (the one-line install command); later versions can be installed
  from the console.

## v0.2.14 (2026-10-20)

### Changed
- **The Let's Encrypt email is optional.** Let's Encrypt no longer sends
  expiry emails, and EdgeTAK renews the certificate itself. `--letsencrypt`
  works on its own; `--letsencrypt you@example.com` still works.

## v0.2.13 (2026-10-20)

### Added
- **Publishing video without a login,** for apps such as TAK ICU that can't
  send a username and password (Video tab).
  - **Off (default).**
  - **Saved feeds only:** feeds marked "Allow publishing without a login".
  - **Any stream name:** visible to the groups you choose.

  Watching still needs a login, and a feed's groups decide who may watch it.

## v0.2.12 (2026-10-20)

### Added
- **Video:**
  - **Feeds for TAK clients:** ATAK and WinTAK's Video Player lists them.
    Feeds with a position appear on devices as camera markers.
  - **Video tab:** add, edit and delete feeds (a server stream or an
    external URL), with groups and a position, plus a Video overlay on the
    map.
  - **MediaMTX media server** (installer `--video`): cameras, drones and ATAK
    publish over RTSP, RTMP or SRT with an EdgeTAK login. Live streams become
    feeds automatically, and viewers get personal links.
  - **Watching in the console:** streams play over EdgeTAK's HTTPS.
  - **MediaMTX updates from the Video tab:** downloads and checksum checks,
    rollback if the new version doesn't start, restart and log.

## v0.2.11 (2026-10-20)

### Added
- **Position history** (off by default; the map's History button), kept from
  6 hours to 30 days.
  - **Trails:** each device's path in its team colour.
  - **Replay:** a slider with play and pause at up to 1200× speed.

## v0.2.10 (2026-10-20)

### Changed
- The console's libraries (Leaflet, milsymbol, JSZip, toGeoJSON and the QR
  code library) are served by EdgeTAK itself. The console works without
  internet access, apart from online base maps, and units in data packages
  draw correctly on phones that block CDNs.

### Fixed
- On phones, Show on map zooms to the package.

## v0.2.9 (2026-10-20)

### Changed
- Data package markers are drawn as ATAK draws them: MIL-STD-2525 symbols for
  units, coloured dots for spot map markers, and pins for waypoints and icon
  set markers.

## v0.2.8 (2026-10-20)

### Added
- **Data packages on the map:** Show on map on the Data Packages tab draws a
  package's markers, lines, shapes, circles and routes in ATAK's colours,
  along with any KML, KMZ or GeoJSON inside it.

## v0.2.7 (2026-10-20)

### Added
- Google Hybrid base map.

## v0.2.6 (2026-10-20)

### Added
- **The map stays open beside every tab** (full screen on phones), and keeps
  updating device positions.
- **Base maps:** Streets, Satellite, Hybrid, Topo, USGS Topo, USGS Imagery
  Topo and Dark.
- **Overlays:** devices, callsigns and the server contact.
- **Custom map sources** shared by all admins, including import of ATAK map
  source XML files.
- Click a callsign in the clients table to show it on the map.

## v0.2.5 (2026-10-20)

### Changed
- The admin console works on phones: tabs swipe, tables turn into cards,
  tap targets are larger, and forms use a single column.

## v0.2.4 (2026-10-20)

### Added
- **Self-registration** at `https://<host>:8446/register`:
  - **Invite links:** with groups, use limits and expiry, a QR code, and
    revocation.
  - **Without an invite:** off (default), approval by an admin, or open
    sign-up into default groups.
  - **After signing up:** the page shows how to connect, with a quick-connect
    QR code and connection packages.

## v0.2.3 (2026-10-20)

### Added
- **Server contact:** the server appears on devices as an HQ contact, so
  devices can chat with it one-to-one.
- **Direct messages** from the console, and an **Inbox** for replies, with
  Reply.

## v0.2.2 (2026-10-20)

### Added
- **Send tab:** send a chat message, or offer a data package, to everyone,
  to groups or to chosen devices, with a history of the last 100 sends.

## v0.2.1 (2026-10-20)

### Added
- The server version shows in the console's top bar and on the sign-in page.

## v0.2.0 (2026-10-20)

The first public release.

### Added
- **One-line install** on Raspberry Pi and Linux, with releases published
  automatically and checked against SHA256SUMS:

  ```
  curl -fsSL https://github.com/brandongotak/EdgeTAK/releases/latest/download/install.sh | sudo sh
  ```

- **Server:**
  - **TAK server core:** CoT over TCP and TLS, the Marti API and certificate
    enrollment.
  - **Data packages and Data Sync missions.**
  - **Admin console:** users, certificates, data packages and connected
    clients.
- **Let's Encrypt** certificates, so quick-connect enrollment trusts the
  server without importing a CA.
- **Groups** with IN (receive) and OUT (send) memberships, managed in the
  console. Users can switch channels from their devices, and changes apply
  without reconnecting.
- **Connection packages** for ATAK, WinTAK, TAKX and iTAK.
- **Sign-in:** OAuth sign-in and the newer API forms that TAKX uses.

### Fixed
- iTAK quick-connect enrollment.
- WinTAK over HTTPS.
- A Dashboard crash on devices reporting a 0,0 position.
