# Device checklist

What the automated tests can't cover: real TAK apps on real devices. Run it
before a major release (1.0, then each minor release that touches what
devices see). Use a test group (for example `QA`) and test users so nothing
reaches a live team.

You need: the admin console, one Android phone with ATAK, and if you have
them, an iPhone with iTAK and a Windows PC with WinTAK. Allow 30 to 60
minutes. Note the EdgeTAK and app versions at the top of your results.

## 1. Connecting

- [ ] **ATAK, quick connect** (servers with Let's Encrypt): add the server
  with address, username and password, certificate enrollment on. It
  connects, and the device appears on the Dashboard with its callsign.
- [ ] **ATAK, connection package**: import a package from Server → Connect.
  It connects.
- [ ] **iTAK**: connect with an iTAK package or quick connect.
- [ ] **WinTAK**: connect with a package.
- [ ] **Positions**: each device sees the others on its map, and the console
  map shows all of them.
- [ ] **Channels** (ATAK): the Channels button lists the user's groups;
  turning one off stops traffic from it.

## 2. Messages

- [ ] **Chat** between ATAK, iTAK and WinTAK in All Chat Rooms, both ways.
- [ ] **Send** (Messages → Send): a message to the test group arrives in
  chat on each device.
- [ ] **Direct message**: with the server contact on, a direct message opens
  a private conversation; a reply shows in the console's Inbox.
- [ ] **Scheduled send**: schedule a message two minutes ahead; it arrives,
  and the schedule's Last column says it was sent.

## 3. Data

- [ ] **Data package**: send one from the console; each device is offered it
  and opens it.
- [ ] **Data Sync**: create a mission on ATAK, add a marker; another device
  subscribed to it gets the marker. **Show on map** on the Missions page
  draws it.
- [ ] **Overlay** (Data → Overlays): import a KML for the test group; ATAK
  gets it in Overlay Manager and can turn it on and off. **Replace file**
  updates it on the device.
- [ ] **Drawing** (Data → Maps): draw a marker and an area for the test
  group; they appear on ATAK's map. Editing one moves it on the device.
- [ ] **Map source**: in Map sources, send Google Hybrid to the test group;
  it appears in ATAK's map list and loads.
- [ ] **Offline map** (if you have an MBTiles file): ATAK lists it and shows
  tiles.

## 4. Plugins

- [ ] ATAK's Plugins screen lists this server's plugins and apps (the
  device profile turned the update server on), and installs one.
- [ ] An APK set to **Treat as app** is listed by ATAK as an app.

## 5. Alerts

- [ ] **Emergency**: press ATAK's emergency button; the console shows the
  red banner and the alarm; cancelling on the phone ends it.
- [ ] **Lost contact**: with alerts on, turn on airplane mode for longer than
  the grace period; an amber alert appears; it ends when the phone is back.
  Quitting ATAK normally does not alert.
- [ ] **Geofence**: draw a small area around the phone's position, walk out
  of it (or move the phone's position); the alert appears.
- [ ] **Webhook**: add one for the service you use and press **Send test**;
  the message arrives and looks right. Trigger an emergency; the webhook
  gets it with a working map link.

## 6. Video (if used)

- [ ] Publish from a phone or camera (RTSP or RTMP with an EdgeTAK login);
  it appears on the Video page and plays in the console.
- [ ] ATAK's Video Player imports it from the server and plays it.

## 7. Accounts

- [ ] **Disable** a test user: their devices disconnect at once and can't
  reconnect; **Enable** lets them back.
- [ ] **Event**: create a short event (shared account), join with the QR
  code on a phone; when the event ends the device is disconnected and the
  account is gone.
- [ ] **Login protection**: wrong passwords from one phone (more than the
  limit) block that phone's password sign-ins; devices already connected
  stay connected; **Unblock** works.

## 8. Upgrade

- [ ] Update the server from the console (or the new Docker image) with
  devices connected: devices reconnect by themselves without re-enrolling,
  and everything above still works.

Anything that fails: note the device, app version and what happened, and
check the server log (`journalctl -u edgetak -f`, or `docker compose logs
edgetak`).
