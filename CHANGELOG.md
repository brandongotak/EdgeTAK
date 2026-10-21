# Changelog

What changed in each EdgeTAK release. The release workflow publishes each
version's section as its GitHub release notes, and the console's update card
shows them, so add a section here before tagging a release.

From v0.3.0, versions follow [Semantic Versioning](https://semver.org/):
new features bump the minor number, fixes the patch number, and while below
1.0, changes that need attention when upgrading bump the minor number too,
with an **Upgrading** section. Releases up to v0.2.24 bumped only the patch
number.

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
