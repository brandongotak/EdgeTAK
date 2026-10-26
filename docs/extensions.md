# Writing EdgeTAK extensions

An extension is a server plugin: a program that adds a feature to EdgeTAK
without changing EdgeTAK. Examples: a bridge from trackers, APRS or
Meshtastic radios to TAK, an export or replay tool, an integration with
another system.

You can write it in any language. EdgeTAK runs it as a child process and
keeps it running. It talks to EdgeTAK over a small local HTTP API, which
lets it:

- receive the CoT of the groups an admin gives it, and send CoT to them;
- read server data (groups, connected devices, missions, position history);
- serve a web page that the console shows behind the admin login.

To start your own, copy a template:

- [template-python](../examples/extensions/template-python): Python 3,
  standard library only.
- [template-go](../examples/extensions/template-go): one binary per
  platform, so the server needs nothing installed.

Each comes with a small API client (a CoT stream that reconnects, marker
building, settings kept in the data folder), a settings and status page,
and a script that builds the zip and installs it on a server in one step.
The [Hello example](../examples/extensions/hello) is a shorter, single-file
demo of the whole API.

> An extension is code running on the server with EdgeTAK's access to the
> machine. Permissions limit what the API lets it do, not what the program
> can do. Admins should install only extensions they trust, as with any
> software.

## The package

An extension is a zip. It contains `plugin.json`, either at the root of the
zip or inside one top folder (zipping a folder works), and whatever the
program needs. The zip can be up to 200 MB.

```json
{
  "id": "hello",
  "name": "Hello",
  "version": "1.0.0",
  "description": "What it does, in a sentence or two.",
  "author": "You",
  "homepage": "https://example.com/hello",
  "run": {
    "windows": ["python", "main.py"],
    "linux/arm64": ["bin/hello-arm64"],
    "*": ["python3", "main.py"]
  },
  "permissions": ["cot:read", "cot:write", "data:read"],
  "page": true
}
```

| Field | |
|---|---|
| `id` | 1–40 lowercase letters, digits and dashes. Uploading a zip with an id that is already installed updates that extension. |
| `name`, `version` | Shown in the console. |
| `description`, `author`, `homepage` | Optional. Shown in the console. |
| `run` | The command to start, by platform. EdgeTAK tries the keys in this order: `os/arch` (for example `linux/arm64`, `linux/amd64`, `windows/amd64`), then `os`, then `*`. If the first word names a file in the extension, that file is run (on Linux, EdgeTAK marks it executable); otherwise the word is looked up on the PATH, for example `python3` or `node`. The program runs in the extension's folder. |
| `permissions` | Any of `cot:read`, `cot:write` and `data:read` (see below). The console shows them to the admin before the extension is started. |
| `page` | `true` if the extension serves a page (see [Its page](#its-page)). |

A Go program can ship as one binary per platform, cross-compiled with
`GOOS=linux GOARCH=arm64 go build`. A Python program needs Python on the
server. Raspberry Pi OS and most Linux servers have it.

## Lifecycle

- **Installing:** a new extension is installed stopped and without
  groups. The admin checks its permissions, chooses its groups and presses
  **Start**.
- **Restarts:** EdgeTAK starts the extension again whenever it exits. It
  waits 2 seconds after the first exit, doubling each time up to a minute,
  and resets the wait once the extension has run for a minute. It also
  starts the extension again when EdgeTAK itself starts.
- **Stopping:** to stop an extension, EdgeTAK sends SIGTERM to its whole
  process group, so any processes it started stop too. If it is still
  running 10 seconds later, EdgeTAK sends SIGKILL. On Windows the process is
  ended immediately.
- **Updating:** an update replaces the extension's files and restarts it.
  Its data folder (`EDGETAK_DATA_DIR`) is kept.
- **Logs:** whatever the extension prints, on stdout or stderr, shows under
  **Log** on the Extensions page (the last 300 lines).
- **Backups:** backups include extensions and their data folders.

## Environment

| Variable | |
|---|---|
| `EDGETAK_URL` | The API's address, for example `http://127.0.0.1:41873`. It only listens on this machine. |
| `EDGETAK_TOKEN` | Send it as `Authorization: Bearer <token>`. A new token is made each time the extension starts. |
| `EDGETAK_EXTENSION_ID` | The manifest's id. |
| `EDGETAK_DATA_DIR` | A folder for the extension's own files (settings, state). It is kept across updates and restarts. |
| `EDGETAK_SERVER_NAME` | The server's name. |
| `EDGETAK_PAGE_PORT` | With `"page": true`: the port to serve the page on, at 127.0.0.1. |
| `EDGETAK_PAGE_PATH` | With `"page": true`: where the console shows the page, `/admin/ext/<id>/`. |

## The API

Every request needs the token. Errors come back as JSON:
`{"error": "…"}`.

### `GET /api/v1/info`

Returns information about the extension and the server. It needs no permission.

```json
{"id": "hello", "name": "Hello", "version": "1.0.0", "permissions": ["cot:read", "cot:write"],
 "server_name": "Field Server", "edgetak_version": "v1.11.0",
 "groups_in": ["Cyan"], "groups_out": ["Cyan"], "page_path": "/admin/ext/hello/"}
```

### `GET /api/v1/cot` (`cot:read`)

Streams the CoT of the extension's **in groups**: what devices in those
groups send, and what is sent to them. That includes chat and messages
addressed to particular devices.

The response is one JSON object per line:

```json
{"uid": "ANDROID-1234", "type": "a-f-G-U-C", "how": "m-g", "time": "2026-10-26T08:00:00Z",
 "stale": "2026-10-26T08:06:00Z", "lat": 40.7, "lon": -74.0, "hae": 10,
 "callsign": "Alpha 1", "xml": "<event version=\"2.0\" …</event>"}
```

- **Start:** the stream begins with the latest position of every device
  the extension can see.
- **Keep-alive:** a blank line every 30 seconds keeps the connection open.
  Skip blank lines.
- **Filter:** `?types=a-f,b-t-f` keeps only events whose type starts with
  one of those prefixes.
- **One reader:** an extension can read the stream once at a time. If you
  reconnect, wait for the old connection to close.
- **Dropped events:** events the extension doesn't read quickly enough are
  dropped and counted on the Extensions page.

### `POST /api/v1/cot` (`cot:write`)

Sends CoT to the extension's **out groups**. The body is one or more
`<event>` elements, up to 1 MB. Each needs a `uid` and a `type`. The answer
is `{"sent": 2}`.

- **Replay:** EdgeTAK remembers the latest position event (`a-…` type) of
  each uid the extension sends, and gives it to devices that connect
  later, until its stale time. An extension doesn't need to repeat its
  markers for newcomers, but should send them again before they go stale.
- **Addressing:** to send to particular devices, add
  `<marti><dest callsign="…"/></marti>` to the event's `<detail>`, as ATAK
  does.
- **Where it goes:** events reach devices and federated servers like
  events from a device. They are not in the position history, which
  records devices' own positions.
- **No out groups:** if the admin hasn't chosen out groups, the answer is
  409.

### Server data (`data:read`)

| | |
|---|---|
| `GET /api/v1/groups` | Groups with their description and members. |
| `GET /api/v1/clients` | Connected devices. Each has `uid`, `callsign`, `username`, `transport`, `groups_in`, `groups_out`, `connected` and `last_seen`, plus `lat`, `lon` and `position_time` once it has reported a position. |
| `GET /api/v1/missions` | Data Sync missions (name, description, creator, groups, item counts). |
| `GET /api/v1/history?since=…&until=…&uid=…` | Recorded tracks. `since` and `until` are RFC 3339 times; the default is the last 24 hours. Each track has `uid`, `callsign`, `team` and `points` (`t` in Unix milliseconds, `lat`, `lon`, `speed` in m/s, `course` in degrees). The answer is 404 if position history is off. |

## Its page

With `"page": true`, serve plain HTTP on `127.0.0.1:$EDGETAK_PAGE_PORT`.
Admins open it with **Open page** on the Extensions page, inside the console
or in a tab of its own, at `/admin/ext/<id>/`.

- **Paths:** EdgeTAK strips the prefix, so your server sees `/` for
  `/admin/ext/<id>/`. Use **relative** links and fetches (`state`,
  `api/save`, not `/state`). Redirects to an absolute path are moved under
  the prefix. The prefix is in the `X-Forwarded-Prefix` header.
- **Who is signed in:** only signed-in administrators reach the page. The
  `X-EdgeTAK-User` header names the admin. The page never gets the console's
  cookies, and cookies it sets are dropped.
- **Changes:** requests that change something (POST, PUT, PATCH, DELETE)
  must carry the header `X-EdgeTAK: 1`, as the console's own requests do,
  so other sites can't make them. A plain HTML form post can't add it, so
  send changes with `fetch`.
- **Same origin:** the page is on the console's origin, which is why only
  extensions you trust belong on a server.

## Try it

```sh
cd examples/extensions
zip -r hello.zip hello        # or right-click → Compress on Windows
```

Then, in the console:

1. On **Server → Extensions**, choose **Install extension…** and pick
   `hello.zip`.
2. Choose the groups it receives from and sends to, then press **Start**.
3. Press **Open page**, enter a position and choose **Place marker**.
   Devices in its out groups see the marker.

When something doesn't work, look at **Log**.
