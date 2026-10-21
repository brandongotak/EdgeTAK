#!/bin/sh
# EdgeTAK installer for Raspberry Pi OS and other systemd-based Linux.
#
# Normally run through the self-extracting edgetak-<version>-rpi-installer.sh,
# which unpacks this script next to bin/edgetak-linux-<arch>. It installs or
# upgrades in place; the data directory (config, users, CA, packages) is never
# touched except by --uninstall --purge.
set -eu

PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin

VERSION=${EDGETAK_VERSION:-dev}
SELF=${EDGETAK_INSTALLER:-$(basename "$0")}
BIN_PATH=/usr/local/bin/edgetak
DATA_DIR=/var/lib/edgetak
SVC_USER=edgetak
UNIT=/etc/systemd/system/edgetak.service
ACTION=install
PUBLIC_HOST=""
FIREWALL=1
PURGE=0
BINARY=""
IMPORT_DATA=""
LE_EMAIL=""
NO_PLAINTEXT=0
VIDEO=0
VIDEO_TARBALL=""

# MediaMTX companion (--video), pinned with its official release checksums.
# EdgeTAK runs it as a child process from its data directory, so the admin
# console can restart and update it without root.
MTX_VERSION=1.21.1
MTX_DIR=$DATA_DIR/mediamtx
mtx_sha256() {
	case "$1" in
		amd64) echo 653abc672a3e693f8d3b2717752492fdcfb8072291ec108d03d3dd857411b0ee ;;
		arm64) echo 6a3aa635fb60ea9b8d566ec306f0a42ff1b6b52a3942bc2baffbe55880d4c3dd ;;
		armv7) echo d032bc572a07a139417db9584a33b91d48c7eb8f27e447645f25c259447e9c07 ;;
		armv6) echo 47c0f461693e27df1fa6b3cf91dc550224fabffb96e9e7ec458c5f112779bdf4 ;;
	esac
}

usage() {
	cat <<EOF
EdgeTAK $VERSION installer

Usage: sudo sh $SELF [options]

  --public-host HOST   address clients use to reach this server
                       (default: detected automatically at each start)
  --letsencrypt EMAIL  serve a Let's Encrypt certificate on the enrollment port
                       (8446) so clients can connect with only address,
                       username and password. Needs --public-host to be a
                       domain pointing at this server and port 80 reachable.
  --no-plaintext       disable the anonymous plaintext ports 8087 (CoT) and
                       8080 (HTTP API); recommended on internet-facing servers
  --video              also install the MediaMTX media server ($MTX_VERSION) so
                       cameras, drones and ATAK can stream video through this
                       server (RTSP 8554, RTMP 1935, SRT 8890/udp). EdgeTAK runs
                       it; update it later from the console's Video tab. Kept
                       on upgrades once installed.
  --video-tarball FILE use this mediamtx_v${MTX_VERSION}_linux_<arch>.tar.gz instead
                       of downloading it (for offline installs)
  --no-firewall        do not open ports in ufw/firewalld
  --binary PATH        install this edgetak binary instead of the bundled one
  --import-data DIR    copy an existing EdgeTAK data directory (e.g. from a
                       manual run) into $DATA_DIR, keeping its CA and users
  --uninstall          remove the service and binary (keeps $DATA_DIR)
  --purge              with --uninstall: also delete $DATA_DIR and the $SVC_USER user
  -h, --help           show this help
EOF
}

say()  { printf '\033[1;32m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33mwarning:\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31merror:\033[0m %s\n' "$*" >&2; exit 1; }

while [ $# -gt 0 ]; do
	case "$1" in
		--public-host) [ $# -ge 2 ] || die "--public-host needs a value"; PUBLIC_HOST=$2; shift 2 ;;
		--public-host=*) PUBLIC_HOST=${1#*=}; shift ;;
		--binary) [ $# -ge 2 ] || die "--binary needs a value"; BINARY=$2; shift 2 ;;
		--binary=*) BINARY=${1#*=}; shift ;;
		--import-data) [ $# -ge 2 ] || die "--import-data needs a value"; IMPORT_DATA=$2; shift 2 ;;
		--import-data=*) IMPORT_DATA=${1#*=}; shift ;;
		--letsencrypt) [ $# -ge 2 ] || die "--letsencrypt needs an email address"; LE_EMAIL=$2; shift 2 ;;
		--letsencrypt=*) LE_EMAIL=${1#*=}; shift ;;
		--no-plaintext) NO_PLAINTEXT=1; shift ;;
		--video) VIDEO=1; shift ;;
		--video-tarball) [ $# -ge 2 ] || die "--video-tarball needs a file"; VIDEO=1; VIDEO_TARBALL=$2; shift 2 ;;
		--video-tarball=*) VIDEO=1; VIDEO_TARBALL=${1#*=}; shift ;;
		--no-firewall) FIREWALL=0; shift ;;
		--uninstall) ACTION=uninstall; shift ;;
		--purge) PURGE=1; shift ;;
		-h|--help) usage; exit 0 ;;
		*) usage >&2; die "unknown option: $1" ;;
	esac
done

[ "$(id -u)" -eq 0 ] || die "must be run as root (try: sudo sh $0 $*)"
[ -d /run/systemd/system ] || die "systemd is not running; this installer requires systemd"
if [ -n "$PUBLIC_HOST" ]; then
	printf '%s' "$PUBLIC_HOST" | grep -Eq '^[A-Za-z0-9.:-]+$' || die "invalid --public-host: $PUBLIC_HOST"
fi
if [ -n "$LE_EMAIL" ]; then
	printf '%s' "$PUBLIC_HOST" | grep -Eq '[A-Za-z]' || die "--letsencrypt needs --public-host set to a domain name (not an IP address)"
	printf '%s' "$LE_EMAIL" | grep -Eq '^[^@ ]+@[^@ ]+$' || die "invalid --letsencrypt email: $LE_EMAIL"
fi

# Upgrades keep video once it is installed.
[ -f "$MTX_DIR/mediamtx" ] && [ "$ACTION" = install ] && VIDEO=1

# Ports to open in the host firewall and to check for conflicts (tcp unless
# given as port/udp).
PORTS="8089 8443 8446"
[ "$NO_PLAINTEXT" -eq 1 ] || PORTS="8087 $PORTS 8080"
[ -z "$LE_EMAIL" ] || PORTS="80 $PORTS"
VIDEO_PORTS="8554 1935 8890/udp"
[ "$VIDEO" -eq 0 ] || PORTS="$PORTS $VIDEO_PORTS"

HERE=$(cd "$(dirname "$0")" && pwd)

as_service_user() {
	if command -v runuser >/dev/null 2>&1; then
		runuser -u "$SVC_USER" -- "$@"
	else
		su -s /bin/sh "$SVC_USER" -c "$(printf '%s ' "$@")"
	fi
}

portspec() { case "$1" in */*) echo "$1" ;; *) echo "$1/tcp" ;; esac; }

firewall() { # firewall open|close
	[ "$FIREWALL" -eq 1 ] || return 0
	if command -v ufw >/dev/null 2>&1 && ufw status 2>/dev/null | grep -q '^Status: active'; then
		for p in $PORTS; do
			p=$(portspec "$p")
			if [ "$1" = open ]; then ufw allow "$p" comment EdgeTAK >/dev/null; else ufw delete allow "$p" >/dev/null 2>&1 || true; fi
		done
		say "ufw: ${1}ed ports $PORTS"
	elif command -v firewall-cmd >/dev/null 2>&1 && firewall-cmd --state >/dev/null 2>&1; then
		for p in $PORTS; do
			p=$(portspec "$p")
			if [ "$1" = open ]; then firewall-cmd -q --permanent --add-port="$p"; else firewall-cmd -q --permanent --remove-port="$p" || true; fi
		done
		firewall-cmd -q --reload
		say "firewalld: ${1}ed ports $PORTS"
	fi
}

do_uninstall() {
	say "Removing EdgeTAK service"
	[ -f "$MTX_DIR/mediamtx" ] && PORTS="$PORTS $VIDEO_PORTS" # stopped with EdgeTAK; files go with --purge
	systemctl disable --now edgetak.service >/dev/null 2>&1 || true
	rm -f "$UNIT"
	systemctl daemon-reload
	rm -f "$BIN_PATH"
	firewall close
	if [ "$PURGE" -eq 1 ]; then
		say "Deleting $DATA_DIR and user $SVC_USER"
		rm -rf "$DATA_DIR" /usr/local/lib/edgetak # the latter: one-line installer's copy
		id "$SVC_USER" >/dev/null 2>&1 && userdel "$SVC_USER" 2>/dev/null || true
	else
		say "Kept $DATA_DIR (config, users, CA, packages). Use --uninstall --purge to delete it."
	fi
	say "EdgeTAK uninstalled"
}

pick_binary() {
	if [ -n "$BINARY" ]; then
		[ -f "$BINARY" ] || die "binary not found: $BINARY"
		echo "$BINARY"
		return
	fi
	case "$(uname -m)" in
		aarch64|arm64) arch=arm64 ;;
		armv6l|armv7l|armv8l) arch=armv6 ;;
		x86_64|amd64) arch=amd64 ;;
		*) die "unsupported CPU architecture: $(uname -m)" ;;
	esac
	b="$HERE/bin/edgetak-linux-$arch"
	[ -f "$b" ] || die "no bundled binary for $arch ($b)"
	echo "$b"
}

write_unit() {
	cat >"$UNIT" <<EOF
# Installed by the EdgeTAK installer. Local changes belong in
# /etc/systemd/system/edgetak.service.d/override.conf (systemctl edit edgetak).
[Unit]
Description=EdgeTAK server
After=network-online.target time-sync.target
Wants=network-online.target

[Service]
Type=simple
User=$SVC_USER
Group=$SVC_USER
ExecStart=$BIN_PATH -data $DATA_DIR
Restart=always
RestartSec=3
LimitNOFILE=65536

# Hardening: EdgeTAK only needs its data directory and the network.
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
PrivateDevices=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectControlGroups=yes
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX AF_NETLINK
RestrictNamespaces=yes
RestrictRealtime=yes
RestrictSUIDSGID=yes
LockPersonality=yes
MemoryDenyWriteExecute=yes
ProtectClock=yes
ProtectHostname=yes
ProtectKernelLogs=yes
ProtectProc=invisible
ProcSubset=pid
# Only to bind port 80 for Let's Encrypt challenges.
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
AmbientCapabilities=CAP_NET_BIND_SERVICE
SystemCallArchitectures=native
SystemCallFilter=@system-service
SystemCallFilter=~@privileged @resources
UMask=0027
ReadWritePaths=$DATA_DIR

[Install]
WantedBy=multi-user.target
EOF
}

# install_mediamtx downloads (or takes --video-tarball), verifies against the
# pinned checksum and installs MediaMTX into $MTX_DIR.
install_mediamtx() {
	case "$(uname -m)" in
		aarch64|arm64) march=arm64 ;;
		armv7l|armv8l) march=armv7 ;;
		armv6l) march=armv6 ;;
		x86_64|amd64) march=amd64 ;;
		*) die "MediaMTX has no build for $(uname -m)" ;;
	esac
	# Keep what is there if it is at least the pinned version (the console may
	# have updated it since).
	have=$(sed 's/^v//' "$MTX_DIR/mediamtx.version" 2>/dev/null || true)
	if [ -x "$MTX_DIR/mediamtx" ] && [ -n "$have" ] &&
		[ "$(printf '%s\n%s\n' "$have" "$MTX_VERSION" | sort -V | tail -n1)" = "$have" ]; then
		say "MediaMTX v$have already installed"
		return 0
	fi
	name="mediamtx_v${MTX_VERSION}_linux_${march}.tar.gz"
	tmp=$(mktemp -d)
	if [ -n "$VIDEO_TARBALL" ]; then
		[ -f "$VIDEO_TARBALL" ] || die "--video-tarball: $VIDEO_TARBALL not found"
		cp "$VIDEO_TARBALL" "$tmp/$name"
	else
		say "Downloading MediaMTX $MTX_VERSION ($march)"
		url="https://github.com/bluenviron/mediamtx/releases/download/v${MTX_VERSION}/$name"
		if command -v curl >/dev/null 2>&1; then
			curl -fsSL -o "$tmp/$name" "$url" || die "download failed: $url (offline? use --video-tarball)"
		elif command -v wget >/dev/null 2>&1; then
			wget -q -O "$tmp/$name" "$url" || die "download failed: $url (offline? use --video-tarball)"
		else
			die "curl or wget is needed to download MediaMTX (or use --video-tarball)"
		fi
	fi
	got=$(sha256sum "$tmp/$name" | cut -d' ' -f1)
	[ "$got" = "$(mtx_sha256 "$march")" ] || { rm -rf "$tmp"; die "MediaMTX checksum mismatch for $name (got $got); refusing to install it"; }
	tar -xzf "$tmp/$name" -C "$tmp" mediamtx LICENSE || die "unpacking $name failed"
	mkdir -p "$MTX_DIR"
	install -m 0750 "$tmp/mediamtx" "$MTX_DIR/mediamtx.new"
	mv -f "$MTX_DIR/mediamtx.new" "$MTX_DIR/mediamtx"
	install -m 0640 "$tmp/LICENSE" "$MTX_DIR/LICENSE"
	echo "v$MTX_VERSION" >"$MTX_DIR/mediamtx.version"
	rm -rf "$tmp"
	say "Installed MediaMTX v$MTX_VERSION in $MTX_DIR"
}

do_install() {
	src=$(pick_binary)
	model=$( { tr -d '\0' </proc/device-tree/model; } 2>/dev/null || uname -m)
	say "Installing EdgeTAK $VERSION on $model"

	upgrading=0
	if systemctl is-active --quiet edgetak.service 2>/dev/null; then
		upgrading=1
		say "Stopping running EdgeTAK for upgrade"
		systemctl stop edgetak.service
	fi

	# A manually started edgetak (or another TAK server) would block the ports.
	if command -v ss >/dev/null 2>&1; then
		for p in $PORTS; do
			case "$p" in */*) continue ;; esac # only TCP listeners are checked
			if ss -Hltn "sport = :$p" 2>/dev/null | grep -q .; then
				ss -Hltnp "sport = :$p" 2>/dev/null | sed 's/^/    /' >&2
				die "port $p is already in use. Stop whatever is listening (e.g. an edgetak you started by hand: pkill edgetak) and rerun."
			fi
		done
	fi

	install -m 0755 "$src" "$BIN_PATH.new"
	mv -f "$BIN_PATH.new" "$BIN_PATH"
	"$BIN_PATH" -version >/dev/null || die "installed binary does not run on this system"

	if ! id "$SVC_USER" >/dev/null 2>&1; then
		say "Creating system user $SVC_USER"
		useradd --system --home-dir "$DATA_DIR" --no-create-home --shell /usr/sbin/nologin "$SVC_USER"
	fi
	mkdir -p "$DATA_DIR"
	if [ -n "$IMPORT_DATA" ]; then
		[ -f "$IMPORT_DATA/db.json" ] || die "$IMPORT_DATA does not look like an EdgeTAK data directory (no db.json)"
		if [ -f "$DATA_DIR/db.json" ]; then
			backup="$DATA_DIR.bak-$(date +%Y%m%d%H%M%S)"
			say "Backing up existing $DATA_DIR to $backup"
			cp -a "$DATA_DIR" "$backup"
		fi
		say "Importing data from $IMPORT_DATA"
		cp -a "$IMPORT_DATA/." "$DATA_DIR/"
	fi
	chown -R "$SVC_USER:$SVC_USER" "$DATA_DIR"
	chmod 0750 "$DATA_DIR"

	# Create config, CA, server certificate and admin account on first install.
	if ! out=$(as_service_user "$BIN_PATH" -data "$DATA_DIR" -init 2>&1); then
		printf '%s\n' "$out" >&2
		die "initialising $DATA_DIR failed"
	fi

	cfg="$DATA_DIR/edgetak.json"
	if [ -n "$PUBLIC_HOST" ]; then
		sed -i "s|^\(  \"public_host\": \)\".*\"|\1\"$PUBLIC_HOST\"|" "$cfg"
		say "Set public_host to $PUBLIC_HOST"
	fi
	if [ "$NO_PLAINTEXT" -eq 1 ]; then
		sed -i 's|^\(  "tcp_addr": \)".*"|\1""|; s|^\(  "http_addr": \)".*"|\1""|' "$cfg"
		say "Disabled plaintext ports 8087 and 8080"
	fi
	if [ -n "$LE_EMAIL" ]; then
		if ! out=$(as_service_user "$BIN_PATH" -data "$DATA_DIR" -enable-letsencrypt "$LE_EMAIL" 2>&1); then
			printf '%s\n' "$out" >&2
			die "enabling Let's Encrypt failed"
		fi
		say "$out"
	fi
	if [ "$VIDEO" -eq 1 ]; then
		install_mediamtx
		chown -R "$SVC_USER:$SVC_USER" "$MTX_DIR"
		if ! out=$(as_service_user "$BIN_PATH" -data "$DATA_DIR" -enable-video 2>&1); then
			printf '%s\n' "$out" >&2
			die "enabling video failed"
		fi
	fi

	write_unit
	systemctl daemon-reload
	systemctl enable edgetak.service >/dev/null 2>&1
	firewall open
	systemctl restart edgetak.service

	# Wait for the plaintext API to answer (or the service to give up).
	i=0
	while [ $i -lt 20 ]; do
		if ! systemctl is-active --quiet edgetak.service; then
			journalctl -u edgetak.service -n 20 --no-pager >&2 || true
			die "EdgeTAK failed to start (see log above)"
		fi
		if command -v curl >/dev/null 2>&1; then
			# The enrollment port is always on; -k because it may not have its
			# public certificate yet and localhost is not in it anyway.
			curl -fsk -o /dev/null https://127.0.0.1:8446/Marti/api/version && break
		else
			sleep 2 && break
		fi
		i=$((i + 1))
		sleep 0.5
	done

	if command -v timedatectl >/dev/null 2>&1 && [ "$(timedatectl show -p NTPSynchronized --value 2>/dev/null)" = no ]; then
		warn "the system clock is not NTP-synchronized. Raspberry Pis have no battery-backed"
		warn "clock; a wrong date makes TLS certificates appear invalid to clients."
	fi

	host=$(grep -o '"public_host": "[^"]*"' "$DATA_DIR/edgetak.json" | cut -d'"' -f4)
	[ -n "$host" ] || host=$(hostname -I 2>/dev/null | awk '{print $1}')
	[ -n "$host" ] || host=$(hostname)

	echo
	if [ "$upgrading" -eq 1 ]; then
		say "EdgeTAK upgraded to $VERSION and restarted"
	else
		say "EdgeTAK $VERSION is installed and running"
	fi
	if grep -q '"mode": "letsencrypt"' "$cfg"; then
		cat <<EOF

  Admin console   https://$host:8446/admin/   (Let's Encrypt, no browser warning)
  Quick connect   in ATAK/WinTAK/TAKX/iTAK add server $host with
                  "Enroll for client certificate" and "Use authentication",
                  then just enter a username and password.
  CoT TLS         $host:8089

  The certificate is requested from Let's Encrypt now; watch for
  "Let's Encrypt certificate ready" in the log. Port 80 must stay open
  for issuance and renewal.

EOF
	else
		cat <<EOF

  Admin console   https://$host:8443/admin/
  CoT TCP / TLS   $host:8087  /  $host:8089
  Enrollment      $host:8446

EOF
	fi
	if [ -f "$DATA_DIR/initial-admin-password.txt" ] && [ "$upgrading" -eq 1 ]; then
		warn "$DATA_DIR/initial-admin-password.txt still exists. Delete it once you have changed the admin password."
		echo
	elif [ -f "$DATA_DIR/initial-admin-password.txt" ]; then
		echo "  Initial administrator login:"
		sed 's/^/    /' "$DATA_DIR/initial-admin-password.txt"
		echo "  (stored in $DATA_DIR/initial-admin-password.txt; delete it after you change the password)"
		echo
	fi
	if [ "$VIDEO" -eq 1 ]; then
		cat <<EOF
  Video           publish with an EdgeTAK username and password:
                    rtsp://USER:PASSWORD@$host:8554/NAME
                    rtmp://$host:1935/NAME?user=USER&pass=PASSWORD
                    srt://$host:8890?streamid=publish:NAME:USER:PASSWORD
                  Streams show up in the console's Video tab and in ATAK's
                  Video Player. MediaMTX runs inside EdgeTAK; its log, restart
                  and updates are on the Video tab.

EOF
	fi
	cat <<EOF
  Logs            journalctl -u edgetak -f
  Restart         sudo systemctl restart edgetak
  Config          $DATA_DIR/edgetak.json  (restart after editing)
  Reset password  sudo -u $SVC_USER $BIN_PATH -data $DATA_DIR -reset-password admin
  Uninstall       sudo sh $SELF --uninstall [--purge]
EOF
}

case "$ACTION" in
	install) do_install ;;
	uninstall) do_uninstall ;;
esac
