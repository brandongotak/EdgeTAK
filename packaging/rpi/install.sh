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
LE=0
LE_EMAIL=""
NO_PLAINTEXT=0
VIDEO=1 # on by default; --no-video turns it off (remembered)
FEDERATION=0 # --federation opens the federation port (9001) in the firewall (remembered)
TERRATAK_REMOVE=0
VIDEO_TARBALL=""
TERRATAK=0 # --terratak FILE runs TerraTAK in a container (remembered); --no-terratak removes it
TERRATAK_FILE=""
INSTALL_DOCKER=0 # --install-docker: install Docker for TerraTAK without asking

# Options are remembered: reruns of the installer and updates from the admin
# console reuse them, and options given on the command line override them.
CONF=/etc/edgetak/install.conf
if [ -f "$CONF" ] && [ ! -L "$CONF" ]; then
	. "$CONF"
fi
# Whether --video/--no-video was given now: with video only on by default, a
# failed MediaMTX download or a taken port skips video instead of failing.
VIDEO_EXPLICIT=0

# Updates from the admin console: EdgeTAK writes a version into REQUEST; the
# path unit runs the root-owned UPDATER, which reruns this installer.
UPDATER=/usr/local/lib/edgetak/edgetak-update.sh
UPDATE_STATE=/var/lib/edgetak-update
UPDATE_PATH_UNIT=/etc/systemd/system/edgetak-update.path
UPDATE_SVC_UNIT=/etc/systemd/system/edgetak-update.service

# ZeroTier from the admin console: EdgeTAK writes "install" into
# $DATA_DIR/zerotier/request; the path unit runs the root-owned ZT_HELPER,
# which installs ZeroTier and gives EdgeTAK its API token.
ZT_HELPER=/usr/local/lib/edgetak/edgetak-zerotier.sh
ZT_STATE=/var/lib/edgetak-zerotier
ZT_PATH_UNIT=/etc/systemd/system/edgetak-zerotier.path
ZT_SVC_UNIT=/etc/systemd/system/edgetak-zerotier.service

# XMPP chat from the admin console: EdgeTAK writes "install" into
# $DATA_DIR/xmpp/request; the path unit runs the root-owned XMPP_HELPER,
# which installs the system's prosody package. EdgeTAK runs Prosody itself.
XMPP_HELPER=/usr/local/lib/edgetak/edgetak-xmpp.sh
XMPP_STATE=/var/lib/edgetak-xmpp
XMPP_PATH_UNIT=/etc/systemd/system/edgetak-xmpp.path
XMPP_SVC_UNIT=/etc/systemd/system/edgetak-xmpp.service

# TerraTAK from the admin console and --terratak: EdgeTAK writes a request
# word into $DATA_DIR/terratak/request; the path unit runs the root-owned
# TT_HELPER, which installs Docker, loads the TerraTAK image and runs it as
# edgetak-terratak-app.service.
TT_HELPER=/usr/local/lib/edgetak/edgetak-terratak.sh
TT_STATE=/var/lib/edgetak-terratak
TT_PATH_UNIT=/etc/systemd/system/edgetak-terratak.path
TT_SVC_UNIT=/etc/systemd/system/edgetak-terratak.service
TT_APP_UNIT=/etc/systemd/system/edgetak-terratak-app.service
TT_PORT=8447

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
  --letsencrypt [EMAIL] serve a Let's Encrypt certificate on the enrollment port
                       (8446) so clients can connect with only address,
                       username and password. Needs --public-host to be a
                       domain pointing at this server and port 80 reachable.
                       The email is optional (Let's Encrypt no longer sends
                       expiry emails; EdgeTAK renews by itself).
  --no-plaintext       disable the anonymous plaintext ports 8087 (CoT) and
                       8080 (HTTP API); recommended on internet-facing servers
  --no-video           do not install the MediaMTX media server. Video is on by
                       default: MediaMTX ($MTX_VERSION) lets cameras, drones and
                       ATAK stream through this server (RTSP 8554, RTMP 1935,
                       SRT 8890/udp); EdgeTAK runs it and the console's Video
                       tab updates it. If it cannot be downloaded, the install
                       continues without video.
  --video              require video (fail instead of skipping it)
  --video-tarball FILE use this mediamtx_v${MTX_VERSION}_linux_<arch>.tar.gz instead
                       of downloading it (for offline installs)
  --terratak FILE      run TerraTAK, the TAK Product Center's tile server, from
                       this image archive (terratak-<version>-<amd64|arm64>.tar
                       from tak.gov) in a container next to EdgeTAK. Needs
                       Docker or Podman; if neither is installed, the installer
                       offers to install it. Its web app is on port $TT_PORT for
                       administrators; the console's TerraTAK page manages it.
                       Rerun with a newer archive to update it.
  --install-docker     with --terratak: install Docker without asking
  --no-terratak        remove TerraTAK (its tilesets are kept)
  --federation         open port 9001 in the firewall for TAK Server federation
                       (other servers connecting to this one; turn that on on
                       the console's Federation tab). --no-federation closes it.
  --no-firewall        do not open ports in ufw/firewalld
  --binary PATH        install this edgetak binary instead of the bundled one
  --import-data DIR    copy an existing EdgeTAK data directory (e.g. from a
                       manual run) into $DATA_DIR, keeping its CA and users
Options are remembered in $CONF for reruns and for updates from the admin
console; options given again override them.

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
		--letsencrypt)
			LE=1
			# The email is optional; take the next argument only if it is one.
			if [ $# -ge 2 ] && printf '%s' "$2" | grep -Eq '^[^-@ ][^@ ]*@[^@ ]+$'; then LE_EMAIL=$2; shift 2; else shift; fi ;;
		--letsencrypt=*) LE=1; LE_EMAIL=${1#*=}; shift ;;
		--no-plaintext) NO_PLAINTEXT=1; shift ;;
		--video) VIDEO=1; VIDEO_EXPLICIT=1; shift ;;
		--no-video) VIDEO=0; VIDEO_EXPLICIT=1; shift ;;
		--video-tarball) [ $# -ge 2 ] || die "--video-tarball needs a file"; VIDEO=1; VIDEO_EXPLICIT=1; VIDEO_TARBALL=$2; shift 2 ;;
		--video-tarball=*) VIDEO=1; VIDEO_EXPLICIT=1; VIDEO_TARBALL=${1#*=}; shift ;;
		--terratak) [ $# -ge 2 ] || die "--terratak needs the image archive (terratak-<version>-<arch>.tar)"; TERRATAK=1; TERRATAK_FILE=$2; shift 2 ;;
		--terratak=*) TERRATAK=1; TERRATAK_FILE=${1#*=}; shift ;;
		--no-terratak) TERRATAK=0; TERRATAK_FILE=""; TERRATAK_REMOVE=1; shift ;;
		--install-docker) INSTALL_DOCKER=1; shift ;;
		--federation) FEDERATION=1; shift ;;
		--no-federation) FEDERATION=0; shift ;;
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
if [ "$LE" -eq 1 ]; then
	printf '%s' "$PUBLIC_HOST" | grep -Eq '[A-Za-z]' || die "--letsencrypt needs --public-host set to a domain name (not an IP address)"
	[ -z "$LE_EMAIL" ] || printf '%s' "$LE_EMAIL" | grep -Eq '^[^@ ]+@[^@ ]+$' || die "invalid --letsencrypt email: $LE_EMAIL"
fi

# Ports to open in the host firewall and to check for conflicts (tcp unless
# given as port/udp).
PORTS="8089 8443 8446"
[ "$NO_PLAINTEXT" -eq 1 ] || PORTS="8087 $PORTS 8080"
[ "$LE" -eq 0 ] || PORTS="80 $PORTS"
[ "$FEDERATION" -eq 0 ] || PORTS="$PORTS 9001"
[ "$TERRATAK" -eq 0 ] || PORTS="$PORTS $TT_PORT"
BASE_PORTS=$PORTS
VIDEO_PORTS="8554 1935 8890/udp"
[ "$VIDEO" -eq 0 ] || PORTS="$PORTS $VIDEO_PORTS"

# skip_video turns video off for this run (it stays wanted in install.conf).
skip_video() {
	warn "video skipped: $*"
	VIDEO=0
	VIDEO_SKIPPED=1
	PORTS=$BASE_PORTS
}
VIDEO_SKIPPED=0
VIDEO_WANTED=$VIDEO # what install.conf remembers, even if video is skipped this time

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
	systemctl disable --now edgetak-update.path >/dev/null 2>&1 || true
	rm -f "$UPDATE_PATH_UNIT" "$UPDATE_SVC_UNIT" "$UPDATER"
	rm -rf "$UPDATE_STATE"
	systemctl disable --now edgetak-zerotier.path >/dev/null 2>&1 || true
	rm -f "$ZT_PATH_UNIT" "$ZT_SVC_UNIT" "$ZT_HELPER"
	rm -rf "$ZT_STATE" # ZeroTier itself stays installed
	systemctl disable --now edgetak-xmpp.path >/dev/null 2>&1 || true
	rm -f "$XMPP_PATH_UNIT" "$XMPP_SVC_UNIT" "$XMPP_HELPER"
	rm -rf "$XMPP_STATE" # Prosody itself stays installed (EdgeTAK stopped it)
	if [ -f "$TT_HELPER" ] && [ -f "$TT_APP_UNIT" ]; then
		PORTS="$PORTS $TT_PORT"
		mkdir -p "$DATA_DIR/terratak"
		if [ "$PURGE" -eq 1 ]; then echo purge >"$DATA_DIR/terratak/request"; else echo remove >"$DATA_DIR/terratak/request"; fi
		EDGETAK_INSTALLER_RUN=1 sh "$TT_HELPER" >/dev/null 2>&1 || true
	fi
	systemctl disable --now edgetak-terratak.path >/dev/null 2>&1 || true
	rm -f "$TT_PATH_UNIT" "$TT_SVC_UNIT" "$TT_HELPER"
	rm -rf "$TT_STATE" # Docker itself stays installed
	systemctl disable --now edgetak.service >/dev/null 2>&1 || true
	rm -f "$UNIT"
	systemctl daemon-reload
	rm -f "$BIN_PATH"
	firewall close
	if [ "$PURGE" -eq 1 ]; then
		say "Deleting $DATA_DIR and user $SVC_USER"
		rm -rf /etc/edgetak
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

# save_options records this install's options for reruns and console updates.
save_options() {
	q() { printf "'%s'" "$(printf '%s' "$1" | sed "s/'/'\\\\''/g")"; }
	mkdir -p /etc/edgetak
	cat >"$CONF.tmp" <<EOF
# Written by the EdgeTAK installer: the options this server was installed
# with. Reruns of the installer and updates from the admin console use them;
# options given on the command line override them.
PUBLIC_HOST=$(q "$PUBLIC_HOST")
LE=$LE
LE_EMAIL=$(q "$LE_EMAIL")
NO_PLAINTEXT=$NO_PLAINTEXT
FIREWALL=$FIREWALL
VIDEO=$VIDEO_WANTED
FEDERATION=$FEDERATION
TERRATAK=$TERRATAK
EOF
	chmod 0644 "$CONF.tmp"
	mv -f "$CONF.tmp" "$CONF"
}

# write_updater installs the root-owned updater and the units that run it
# when EdgeTAK requests an update.
write_updater() {
	mkdir -p "$(dirname "$UPDATER")" "$UPDATE_STATE"
	chmod 0755 "$UPDATE_STATE"
	install -d -o "$SVC_USER" -g "$SVC_USER" -m 0750 "$DATA_DIR/update"
	# Written to a new file and renamed: a running updater keeps reading the old one.
	cat >"$UPDATER.tmp" <<'EOF'
#!/bin/sh
# EdgeTAK updater, installed by the EdgeTAK installer. edgetak-update.service
# runs it as root when the admin console asks for an update. The only input
# it takes from EdgeTAK is a version number, which it validates; it downloads
# that release's installer itself and verifies it against the release's
# SHA256SUMS, then reruns it with the options in /etc/edgetak/install.conf.
set -eu
PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
REPO=brandongotak/EdgeTAK
REQUEST=/var/lib/edgetak/update/request
STATE=/var/lib/edgetak-update
DEST=/usr/local/lib/edgetak
MIN=0.2.15

status() { # state message
	printf '%s %s %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$1" "$2" >"$STATE/status.tmp"
	mv -f "$STATE/status.tmp" "$STATE/status"
	echo "$1: $2"
}
fail() { status error "$*"; exit 1; }
fetch() { # url file
	if command -v curl >/dev/null 2>&1; then curl -fsSL --retry 3 -o "$2" "$1"
	else wget -q -O "$2" "$1"; fi
}

mkdir -p "$STATE"
[ -f "$REQUEST" ] && [ ! -L "$REQUEST" ] || exit 0
v=$(head -c 32 "$REQUEST" | tr -d '\r\n ')
printf '%s' "$v" | grep -Eq '^v[0-9]{1,4}\.[0-9]{1,4}\.[0-9]{1,6}(-(alpha|beta|rc)(\.[0-9]{1,3})?)?$' || fail "invalid version requested"
[ "$(printf '%s\n%s\n' "${v#v}" "$MIN" | sort -V | head -n1)" = "$MIN" ] || fail "versions before v$MIN cannot be installed from the console"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM
base=https://github.com/$REPO/releases/download/$v
status running "downloading $v"
fetch "$base/edgetak-installer.sh" "$tmp/edgetak-installer.sh" || fail "download of $v failed (no internet, or no such release)"
fetch "$base/SHA256SUMS" "$tmp/SHA256SUMS" || fail "download of the $v checksums failed"
want=$(awk '$2 == "edgetak-installer.sh" || $2 == "*edgetak-installer.sh" { print $1 }' "$tmp/SHA256SUMS")
got=$(sha256sum "$tmp/edgetak-installer.sh" | awk '{print $1}')
[ -n "$want" ] && [ "$got" = "$want" ] || fail "checksum mismatch for the $v installer; not installing"
mkdir -p "$DEST"
install -m 0755 "$tmp/edgetak-installer.sh" "$DEST/edgetak-installer.sh"

status running "installing $v"
if EDGETAK_INSTALLER="$DEST/edgetak-installer.sh" sh "$DEST/edgetak-installer.sh" >"$tmp/log" 2>&1; then
	cat "$tmp/log"
	status done "updated to $v"
else
	cat "$tmp/log"
	esc=$(printf '\033')
	fail "the $v installer failed: $(sed "s/${esc}\[[0-9;]*m//g" "$tmp/log" | grep -i 'error' | tail -n1 | cut -c1-200)"
fi
EOF
	chmod 0755 "$UPDATER.tmp"
	mv -f "$UPDATER.tmp" "$UPDATER"

	cat >"$UPDATE_PATH_UNIT" <<EOF
# Installed by the EdgeTAK installer: runs edgetak-update.service when the
# admin console writes an update request.
[Unit]
Description=Watch for EdgeTAK update requests from the admin console

[Path]
PathChanged=$DATA_DIR/update/request
Unit=edgetak-update.service

[Install]
WantedBy=multi-user.target
EOF
	cat >"$UPDATE_SVC_UNIT" <<EOF
# Installed by the EdgeTAK installer. Runs as root: it reruns the installer.
[Unit]
Description=Update EdgeTAK (requested from the admin console)

[Service]
Type=oneshot
ExecStart=/bin/sh $UPDATER
TimeoutStartSec=20min
EOF
}

# write_zerotier_helper installs the root-owned ZeroTier helper and the
# units that run it when the admin console asks for ZeroTier.
write_zerotier_helper() {
	mkdir -p "$(dirname "$ZT_HELPER")" "$ZT_STATE"
	chmod 0755 "$ZT_STATE"
	install -d -o "$SVC_USER" -g "$SVC_USER" -m 0750 "$DATA_DIR/zerotier"
	cat >"$ZT_HELPER.tmp" <<'ZTEOF'
#!/bin/sh
# EdgeTAK ZeroTier helper, installed by the EdgeTAK installer.
# edgetak-zerotier.service runs it as root when the admin console asks for
# ZeroTier. The only input it takes from EdgeTAK is the word "install". It
# installs zerotier-one from ZeroTier's apt repository, which apt verifies
# with ZeroTier's release signing key pinned below (fingerprint
# 74A5 E9C4 58E1 A431 F1DA 57A7 1657 1988 23E5 2A61), and gives EdgeTAK a
# copy of the ZeroTier service's API token. No script is downloaded and run.
set -eu
PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
REQUEST=/var/lib/edgetak/zerotier/request
TOKEN=/var/lib/edgetak/zerotier/authtoken
STATE=/var/lib/edgetak-zerotier
KEYRING=/usr/share/keyrings/zerotier-edgetak.asc
LIST=/etc/apt/sources.list.d/zerotier.list
ZT_TOKEN=/var/lib/zerotier-one/authtoken.secret

status() { # state message
	printf '%s %s %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$1" "$2" >"$STATE/status.tmp"
	mv -f "$STATE/status.tmp" "$STATE/status"
	echo "$1: $2"
}
fail() { status error "$*"; exit 1; }

# codename picks ZeroTier's repository for this system (as ZeroTier's own
# install script does): Debian and Raspberry Pi OS 10 to 13 by codename
# (newer use trixie), Ubuntu LTS releases (others use the LTS before them).
codename() {
	[ -r /etc/os-release ] || return 1
	. /etc/os-release
	case " ${ID:-} ${ID_LIKE:-} " in
	*" ubuntu "*)
		v=${VERSION_ID:-0}
		[ "${ID:-}" = ubuntu ] || { case "${UBUNTU_CODENAME:-}" in focal|jammy|noble) echo "$UBUNTU_CODENAME"; return 0 ;; *) return 1 ;; esac; }
		major=${v%%.*}
		if [ "$major" -ge 24 ]; then echo noble
		elif [ "$major" -ge 22 ]; then echo jammy
		elif [ "$major" -ge 20 ]; then echo focal
		else return 1; fi ;;
	*" debian "*|*" raspbian "*)
		major=${VERSION_ID:-99}
		major=${major%%.*}
		case "$major" in
		10) echo buster ;; 11) echo bullseye ;; 12) echo bookworm ;;
		*) [ "$major" -ge 13 ] 2>/dev/null && echo trixie || return 1 ;;
		esac ;;
	*) return 1 ;;
	esac
}

copy_token() {
	i=0
	while [ ! -s "$ZT_TOKEN" ] && [ $i -lt 30 ]; do sleep 1; i=$((i + 1)); done
	[ -s "$ZT_TOKEN" ] || fail "ZeroTier is installed, but its API token didn't appear; is the zerotier-one service running?"
	install -o edgetak -g edgetak -m 0600 "$ZT_TOKEN" "$TOKEN.tmp"
	mv -f "$TOKEN.tmp" "$TOKEN"
}

mkdir -p "$STATE"
[ -f "$REQUEST" ] && [ ! -L "$REQUEST" ] || exit 0
req=$(head -c 16 "$REQUEST" | tr -d '\r\n ')
[ "$req" = install ] || fail "unknown request"

if [ -x /usr/sbin/zerotier-one ] || command -v zerotier-one >/dev/null 2>&1; then
	status running "ZeroTier is already installed; connecting EdgeTAK to it"
else
	command -v apt-get >/dev/null 2>&1 || fail "installing ZeroTier from the console works on Debian, Ubuntu and Raspberry Pi OS; install it yourself (zerotier.com/download), then click Install again"
	cn=$(codename) || fail "this system's version isn't one ZeroTier publishes packages for; install it yourself (zerotier.com/download), then click Install again"
	tmp=$(mktemp)
	trap 'rm -f "$tmp"' EXIT INT TERM
	cat >"$KEYRING.tmp" <<'ZTKEY'
-----BEGIN PGP PUBLIC KEY BLOCK-----
Comment: GPGTools - https://gpgtools.org

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
=JgAv
-----END PGP PUBLIC KEY BLOCK-----
ZTKEY
	chmod 0644 "$KEYRING.tmp"
	mv -f "$KEYRING.tmp" "$KEYRING"
	echo "deb [signed-by=$KEYRING] http://download.zerotier.com/debian/$cn $cn main" >"$LIST"
	status running "downloading ZeroTier ($cn)"
	apt-get update -o Dir::Etc::sourcelist="$LIST" -o Dir::Etc::sourceparts=- -o APT::Get::List-Cleanup=0 >"$tmp" 2>&1 ||
		fail "the ZeroTier package list couldn't be downloaded or verified: $(tail -n1 "$tmp" | cut -c1-200)"
	status running "installing ZeroTier"
	DEBIAN_FRONTEND=noninteractive apt-get install -y zerotier-one >"$tmp" 2>&1 ||
		fail "installing zerotier-one failed: $(grep -i 'e:' "$tmp" | tail -n1 | cut -c1-200)"
fi
systemctl enable --now zerotier-one >/dev/null 2>&1 || true
copy_token
status done "ZeroTier is installed and EdgeTAK is connected to it"
ZTEOF
	chmod 0755 "$ZT_HELPER.tmp"
	mv -f "$ZT_HELPER.tmp" "$ZT_HELPER"

	cat >"$ZT_PATH_UNIT" <<EOF
# Installed by the EdgeTAK installer: runs edgetak-zerotier.service when the
# admin console asks for ZeroTier.
[Unit]
Description=Watch for ZeroTier requests from the EdgeTAK admin console

[Path]
PathChanged=$DATA_DIR/zerotier/request
Unit=edgetak-zerotier.service

[Install]
WantedBy=multi-user.target
EOF
	cat >"$ZT_SVC_UNIT" <<EOF
# Installed by the EdgeTAK installer. Runs as root: it installs ZeroTier.
[Unit]
Description=Install ZeroTier for EdgeTAK (requested from the admin console)

[Service]
Type=oneshot
ExecStart=/bin/sh $ZT_HELPER
TimeoutStartSec=15min
EOF
	# ZeroTier installed already (here or by hand): keep EdgeTAK's token copy current.
	if [ -s /var/lib/zerotier-one/authtoken.secret ]; then
		install -o "$SVC_USER" -g "$SVC_USER" -m 0600 /var/lib/zerotier-one/authtoken.secret "$DATA_DIR/zerotier/authtoken"
	fi
}

# write_xmpp_helper installs the root-owned helper that installs Prosody
# when the admin console asks for XMPP chat, and the units that run it.
write_xmpp_helper() {
	mkdir -p "$(dirname "$XMPP_HELPER")" "$XMPP_STATE"
	chmod 0755 "$XMPP_STATE"
	install -d -o "$SVC_USER" -g "$SVC_USER" -m 0750 "$DATA_DIR/xmpp"
	cat >"$XMPP_HELPER.tmp" <<'XMPPEOF'
#!/bin/sh
# EdgeTAK XMPP chat helper, installed by the EdgeTAK installer.
# edgetak-xmpp.service runs it as root when the admin console asks for XMPP
# chat. The only input it takes from EdgeTAK is the word "install". It
# installs the system's prosody package (verified by apt as usual), turns
# off the system's own Prosody service, since EdgeTAK runs Prosody itself
# with its own configuration, and opens the chat ports in the firewall.
set -eu
PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
REQUEST=/var/lib/edgetak/xmpp/request
STATE=/var/lib/edgetak-xmpp
PORTS="5222 5281"

status() { # state message
	printf '%s %s %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$1" "$2" >"$STATE/status.tmp"
	mv -f "$STATE/status.tmp" "$STATE/status"
	echo "$1: $2"
}
fail() { status error "$*"; exit 1; }

open_ports() {
	if command -v ufw >/dev/null 2>&1 && ufw status 2>/dev/null | grep -q '^Status: active'; then
		for p in $PORTS; do ufw allow "$p/tcp" comment "EdgeTAK XMPP" >/dev/null || true; done
	elif command -v firewall-cmd >/dev/null 2>&1 && firewall-cmd --state >/dev/null 2>&1; then
		for p in $PORTS; do firewall-cmd -q --permanent --add-port="$p/tcp" || true; done
		firewall-cmd -q --reload || true
	fi
}

mkdir -p "$STATE"
[ -f "$REQUEST" ] && [ ! -L "$REQUEST" ] || exit 0
req=$(head -c 16 "$REQUEST" | tr -d '\r\n ')
[ "$req" = install ] || fail "unknown request"

if command -v prosody >/dev/null 2>&1; then
	status running "Prosody is already installed"
else
	command -v apt-get >/dev/null 2>&1 || fail "installing Prosody from the console works on Debian, Ubuntu and Raspberry Pi OS; install it yourself (prosody.im/download)"
	tmp=$(mktemp)
	trap 'rm -f "$tmp"' EXIT INT TERM
	status running "downloading the package list"
	DEBIAN_FRONTEND=noninteractive apt-get update >"$tmp" 2>&1 ||
		fail "the package list couldn't be downloaded: $(tail -n1 "$tmp" | cut -c1-200)"
	status running "installing Prosody"
	DEBIAN_FRONTEND=noninteractive apt-get install -y prosody >"$tmp" 2>&1 ||
		fail "installing prosody failed: $(grep -i 'e:' "$tmp" | tail -n1 | cut -c1-200)"
fi
# EdgeTAK runs its own Prosody; the system's would hold the same ports.
systemctl disable --now prosody >/dev/null 2>&1 || true
open_ports
status done "Prosody is installed; EdgeTAK runs it when XMPP chat is on"
XMPPEOF
	chmod 0755 "$XMPP_HELPER.tmp"
	mv -f "$XMPP_HELPER.tmp" "$XMPP_HELPER"

	cat >"$XMPP_PATH_UNIT" <<EOF
# Installed by the EdgeTAK installer: runs edgetak-xmpp.service when the
# admin console asks for XMPP chat.
[Unit]
Description=Watch for XMPP chat requests from the EdgeTAK admin console

[Path]
PathChanged=$DATA_DIR/xmpp/request
Unit=edgetak-xmpp.service

[Install]
WantedBy=multi-user.target
EOF
	cat >"$XMPP_SVC_UNIT" <<EOF
# Installed by the EdgeTAK installer. Runs as root: it installs Prosody.
[Unit]
Description=Install Prosody for EdgeTAK XMPP chat (requested from the admin console)

[Service]
Type=oneshot
ExecStart=/bin/sh $XMPP_HELPER
TimeoutStartSec=15min
EOF
}

# write_terratak_helper installs the root-owned TerraTAK helper and the
# units that run it when the admin console asks.
write_terratak_helper() {
	# The installer runs the helper itself below; the path unit starts again
	# when the install enables it.
	systemctl stop edgetak-terratak.path >/dev/null 2>&1 || true
	mkdir -p "$(dirname "$TT_HELPER")" "$TT_STATE"
	chmod 0755 "$TT_STATE"
	install -d -o "$SVC_USER" -g "$SVC_USER" -m 0750 "$DATA_DIR/terratak"
	cat >"$TT_HELPER.tmp" <<'TTEOF'
#!/bin/sh
# EdgeTAK TerraTAK helper, installed by the EdgeTAK installer.
# edgetak-terratak.service runs it as root when the admin console asks, and
# the installer runs it for --terratak/--no-terratak. The only input it takes
# from EdgeTAK is one request word (install-docker, load, restart, remove)
# and, for load, the image archive EdgeTAK saved, which only the container
# runtime reads. TerraTAK runs as edgetak-terratak-app.service, publishing
# its port on loopback only: EdgeTAK serves it to administrators.
set -eu
PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
DATA_DIR=/var/lib/edgetak
REQUEST=$DATA_DIR/terratak/request
IMAGE_TAR=$DATA_DIR/terratak/image.tar
STATE=/var/lib/edgetak-terratak
ENV_FILE=/etc/edgetak/terratak.env
APP_UNIT=/etc/systemd/system/edgetak-terratak-app.service
CONTAINER=edgetak-terratak
VOLUME=edgetak-terratak
PORT=8447 # EdgeTAK's port for the TerraTAK app (opened in the firewall)
FIREWALL=1
[ ! -f /etc/edgetak/install.conf ] || FIREWALL=$(sed -n 's/^FIREWALL=//p' /etc/edgetak/install.conf | tail -n1)
FIREWALL=${FIREWALL:-1}

status() { # state message
	mkdir -p "$STATE"
	printf '%s %s %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$1" "$2" >"$STATE/status.tmp"
	mv -f "$STATE/status.tmp" "$STATE/status"
	echo "$1: $2"
}
fail() { status error "$*"; exit 1; }

# runtime prints the container runtime to use: Docker (its command and its
# daemon; Debian can leave the command without the daemon) or Podman.
runtime() {
	if command -v docker >/dev/null 2>&1 && command -v dockerd >/dev/null 2>&1; then command -v docker; return 0; fi
	if command -v podman >/dev/null 2>&1; then command -v podman; return 0; fi
	return 1
}

as_edgetak() {
	if command -v runuser >/dev/null 2>&1; then runuser -u edgetak -- "$@"
	else su -s /bin/sh edgetak -c "$(printf '%s ' "$@")"; fi
}

# set_edgetak on|off turns TerraTAK on or off in EdgeTAK's configuration and
# restarts EdgeTAK if that changed it (not when the installer runs this; it
# restarts EdgeTAK itself).
set_edgetak() {
	cfg=$DATA_DIR/edgetak.json
	want=false
	[ "$1" = on ] && want=true
	if grep -A3 '"terratak"' "$cfg" 2>/dev/null | grep -q "\"enabled\": $want"; then
		return 0
	fi
	if [ "$1" = on ]; then
		as_edgetak /usr/local/bin/edgetak -data "$DATA_DIR" -enable-terratak >/dev/null
	else
		as_edgetak /usr/local/bin/edgetak -data "$DATA_DIR" -disable-terratak >/dev/null
	fi
	firewall "$1"
	if [ -z "${EDGETAK_INSTALLER_RUN:-}" ]; then
		# Delayed, so the console's request that started this one completes.
		systemd-run --on-active=3 --unit=edgetak-restart-terratak systemctl restart edgetak.service >/dev/null 2>&1 ||
			systemctl restart edgetak.service
	fi
}

firewall() { # on|off
	[ "$FIREWALL" = 1 ] || return 0
	if command -v ufw >/dev/null 2>&1 && ufw status 2>/dev/null | grep -q '^Status: active'; then
		if [ "$1" = on ]; then ufw allow "$PORT/tcp" comment EdgeTAK >/dev/null; else ufw delete allow "$PORT/tcp" >/dev/null 2>&1 || true; fi
	elif command -v firewall-cmd >/dev/null 2>&1 && firewall-cmd --state >/dev/null 2>&1; then
		if [ "$1" = on ]; then firewall-cmd -q --permanent --add-port="$PORT/tcp"; else firewall-cmd -q --permanent --remove-port="$PORT/tcp" || true; fi
		firewall-cmd -q --reload
	fi
}

install_docker() {
	if rt=$(runtime); then
		status done "$(basename "$rt") is already installed"
		return 0
	fi
	tmp=$(mktemp)
	trap 'rm -f "$tmp"' EXIT INT TERM
	if command -v apt-get >/dev/null 2>&1; then
		status running "installing Docker (docker.io)"
		DEBIAN_FRONTEND=noninteractive apt-get update >"$tmp" 2>&1 || fail "apt-get update failed: $(tail -n1 "$tmp" | cut -c1-200)"
		DEBIAN_FRONTEND=noninteractive apt-get install -y docker.io >"$tmp" 2>&1 ||
			fail "installing docker.io failed: $(grep -i 'e:' "$tmp" | tail -n1 | cut -c1-200)"
		systemctl enable --now docker >/dev/null 2>&1 || true
	elif command -v dnf >/dev/null 2>&1; then
		status running "installing Podman"
		dnf install -y podman >"$tmp" 2>&1 || fail "installing podman failed: $(tail -n1 "$tmp" | cut -c1-200)"
	elif command -v yum >/dev/null 2>&1; then
		status running "installing Podman"
		yum install -y podman >"$tmp" 2>&1 || fail "installing podman failed: $(tail -n1 "$tmp" | cut -c1-200)"
	else
		fail "this system has no apt-get, dnf or yum; install Docker yourself (docs.docker.com/engine/install), then try again"
	fi
	rt=$(runtime) || fail "Docker was installed but isn't on the PATH"
	status done "$(basename "$rt") is installed; now upload the TerraTAK image"
}

write_app_unit() { # runtime path
	rt=$1
	after="network-online.target"
	wants="network-online.target"
	if [ "$(basename "$rt")" = docker ]; then
		after="$after docker.service"
		wants="$wants docker.service"
	fi
	cat >"$APP_UNIT.tmp" <<EOF
# Installed by the EdgeTAK TerraTAK helper: TerraTAK in a container. It
# listens on loopback only; EdgeTAK serves it to administrators on port $PORT.
[Unit]
Description=TerraTAK tile server (for EdgeTAK)
After=$after
Wants=$wants

[Service]
EnvironmentFile=$ENV_FILE
ExecStartPre=-$rt rm -f $CONTAINER
ExecStart=$rt run --rm --name $CONTAINER -p 127.0.0.1:8098:8080 -v $VOLUME:/opt/terratak -e TERRATAK_MAX_MEMORY=\${TERRATAK_MAX_MEMORY} \${TERRATAK_IMAGE}
ExecStop=$rt stop -t 20 $CONTAINER
Restart=always
RestartSec=5
TimeoutStartSec=5min
# docker stop ends the container with SIGTERM.
SuccessExitStatus=143

[Install]
WantedBy=multi-user.target
EOF
	mv -f "$APP_UNIT.tmp" "$APP_UNIT"
	systemctl daemon-reload
}

load_image() {
	rt=$(runtime) || fail "install Docker first"
	# Docker's daemon may be stopped (or never started after installing it).
	[ "$(basename "$rt")" != docker ] || systemctl enable --now docker >/dev/null 2>&1 || true
	[ -f "$IMAGE_TAR" ] && [ ! -L "$IMAGE_TAR" ] || fail "no image was uploaded"
	tmp=$(mktemp)
	trap 'rm -f "$tmp"' EXIT INT TERM
	status running "loading the TerraTAK image (this takes a minute or two)"
	if ! "$rt" load -i "$IMAGE_TAR" >"$tmp" 2>&1; then
		rm -f "$IMAGE_TAR"
		fail "loading the image failed: $(tail -n1 "$tmp" | cut -c1-200)"
	fi
	rm -f "$IMAGE_TAR"
	image=$(sed -n 's/^Loaded image[^:]*: *//p' "$tmp" | tail -n1 | awk '{print $NF}')
	printf '%s' "$image" | grep -Eq '^[A-Za-z0-9._/-]+:[A-Za-z0-9._-]+$' || fail "the archive held no tagged image"
	case "$image" in *terratak*) ;; *) "$rt" rmi "$image" >/dev/null 2>&1 || true; fail "that image ($image) isn't TerraTAK" ;; esac
	old=$(sed -n 's/^TERRATAK_IMAGE=//p' "$ENV_FILE" 2>/dev/null || true)
	mkdir -p "$(dirname "$ENV_FILE")"
	printf 'TERRATAK_IMAGE=%s\nTERRATAK_MAX_MEMORY=512M\n' "$image" >"$ENV_FILE"
	write_app_unit "$rt"
	status running "starting TerraTAK ($image)"
	systemctl enable edgetak-terratak-app.service >/dev/null 2>&1
	systemctl restart edgetak-terratak-app.service || fail "TerraTAK didn't start: $(journalctl -u edgetak-terratak-app -n 3 --no-pager 2>/dev/null | tail -n1 | cut -c1-200)"
	if [ -n "$old" ] && [ "$old" != "$image" ]; then
		"$rt" rmi "$old" >/dev/null 2>&1 || true
	fi
	set_edgetak on
	status done "TerraTAK ${image##*:} is running"
}

remove() {
	status running "removing TerraTAK"
	systemctl disable --now edgetak-terratak-app.service >/dev/null 2>&1 || true
	rm -f "$APP_UNIT"
	systemctl daemon-reload
	systemctl reset-failed edgetak-terratak-app.service >/dev/null 2>&1 || true
	if rt=$(runtime); then
		"$rt" rm -f "$CONTAINER" >/dev/null 2>&1 || true
		image=$(sed -n 's/^TERRATAK_IMAGE=//p' "$ENV_FILE" 2>/dev/null || true)
		[ -z "$image" ] || "$rt" rmi "$image" >/dev/null 2>&1 || true
		[ "${1:-}" != purge ] || "$rt" volume rm "$VOLUME" >/dev/null 2>&1 || true
	fi
	rm -f "$ENV_FILE"
	set_edgetak off
	status done "TerraTAK was removed; its tilesets are kept for when you load it again"
}

mkdir -p "$STATE"
chmod 0755 "$STATE"
# One at a time: the installer runs this directly while the path unit may
# start it for the same request; whoever comes second finds it taken.
if command -v flock >/dev/null 2>&1; then
	exec 9>"$STATE/lock"
	flock 9
fi
[ -f "$REQUEST" ] && [ ! -L "$REQUEST" ] || exit 0
req=$(head -c 32 "$REQUEST" | tr -d '\r\n ')
rm -f "$REQUEST"
case "$req" in
	install-docker) install_docker ;;
	load) load_image ;;
	restart)
		[ -f "$APP_UNIT" ] || fail "TerraTAK isn't installed"
		status running "restarting TerraTAK"
		systemctl restart edgetak-terratak-app.service || fail "TerraTAK didn't start"
		status done "TerraTAK restarted" ;;
	remove) remove ;;
	purge) remove purge ;;
	unit)
		# An upgrade of EdgeTAK: rewrite TerraTAK's unit as this version has it.
		if [ -f "$APP_UNIT" ] && rt=$(runtime); then
			write_app_unit "$rt"
		fi ;;
	*) fail "unknown request" ;;
esac
TTEOF
	chmod 0755 "$TT_HELPER.tmp"
	mv -f "$TT_HELPER.tmp" "$TT_HELPER"

	cat >"$TT_PATH_UNIT" <<EOF
# Installed by the EdgeTAK installer: runs edgetak-terratak.service when the
# admin console asks for something about TerraTAK.
[Unit]
Description=Watch for TerraTAK requests from the EdgeTAK admin console

[Path]
PathChanged=$DATA_DIR/terratak/request
Unit=edgetak-terratak.service

[Install]
WantedBy=multi-user.target
EOF
	cat >"$TT_SVC_UNIT" <<EOF
# Installed by the EdgeTAK installer. Runs as root: it installs Docker and
# runs TerraTAK's container.
[Unit]
Description=Set up TerraTAK for EdgeTAK (requested from the admin console)

[Service]
Type=oneshot
ExecStart=/bin/sh $TT_HELPER
TimeoutStartSec=30min
EOF
}

# container_runtime prints the installed container runtime, if any.
container_runtime() {
	if command -v docker >/dev/null 2>&1 && command -v dockerd >/dev/null 2>&1; then echo docker; return 0; fi
	if command -v podman >/dev/null 2>&1; then echo podman; return 0; fi
	return 1
}

# terratak_preflight checks --terratak before anything changes: the archive
# exists, and Docker is installed or may be (--install-docker, or the person
# running the installer agrees).
terratak_preflight() {
	[ -f "$TERRATAK_FILE" ] || die "--terratak: $TERRATAK_FILE not found"
	if ! container_runtime >/dev/null; then
		if [ "$INSTALL_DOCKER" -eq 0 ]; then
			# Ask on the terminal (stdin is the script itself with curl | sh).
			if [ -r /dev/tty ] && [ -w /dev/tty ] && { printf '' >/dev/tty; } 2>/dev/null; then
				printf '\033[1;33m==>\033[0m TerraTAK runs in a container, and Docker is not installed.\n    Install Docker now (docker.io, or Podman on Red Hat-type systems)? [y/N] ' >/dev/tty
				read -r ans </dev/tty || ans=""
				case "$ans" in y|Y|yes|YES|Yes) INSTALL_DOCKER=1 ;; esac
			fi
		fi
		[ "$INSTALL_DOCKER" -eq 1 ] || die "TerraTAK needs Docker or Podman. Rerun with --install-docker to install it, or install it yourself (docs.docker.com/engine/install) and rerun."
	fi
}

# setup_terratak loads --terratak FILE, installing Docker first if needed.
setup_terratak() {
	if ! container_runtime >/dev/null; then
		say "Installing Docker"
		mkdir -p "$DATA_DIR/terratak"
		echo install-docker >"$DATA_DIR/terratak/request"
		if ! EDGETAK_INSTALLER_RUN=1 sh "$TT_HELPER"; then
			warn "installing Docker failed (see above); EdgeTAK is installed without TerraTAK. Try again on the console's TerraTAK page."
			return 0
		fi
	fi
	say "Loading TerraTAK from $TERRATAK_FILE (this takes a minute or two)"
	install -d -o "$SVC_USER" -g "$SVC_USER" -m 0750 "$DATA_DIR/terratak"
	cp "$TERRATAK_FILE" "$DATA_DIR/terratak/image.tar" || { warn "copying $TERRATAK_FILE failed"; return 0; }
	echo load >"$DATA_DIR/terratak/request"
	EDGETAK_INSTALLER_RUN=1 sh "$TT_HELPER" ||
		warn "loading TerraTAK failed (see above); EdgeTAK is installed without it. Try again on the console's TerraTAK page."
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

	[ -z "$TERRATAK_FILE" ] || terratak_preflight

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
				case " $VIDEO_PORTS " in
					*" $p "*) if [ "$VIDEO_EXPLICIT" -eq 0 ]; then skip_video "port $p is already in use (another media server?)"; continue; fi ;;
				esac
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
	if [ "$LE" -eq 1 ]; then
		if ! out=$(as_service_user "$BIN_PATH" -data "$DATA_DIR" "-enable-letsencrypt=$LE_EMAIL" 2>&1); then
			printf '%s\n' "$out" >&2
			die "enabling Let's Encrypt failed"
		fi
		say "$out"
	fi
	if [ "$VIDEO" -eq 1 ]; then
		# In a subshell so a failed download can skip video rather than end the install.
		if ( install_mediamtx ); then
			chown -R "$SVC_USER:$SVC_USER" "$MTX_DIR"
			if ! out=$(as_service_user "$BIN_PATH" -data "$DATA_DIR" -enable-video 2>&1); then
				printf '%s\n' "$out" >&2
				die "enabling video failed"
			fi
		elif [ "$VIDEO_EXPLICIT" -eq 1 ]; then
			die "installing MediaMTX failed (see above)"
		else
			skip_video "MediaMTX could not be installed (offline?); the next update tries again, or use --no-video"
		fi
	fi
	if [ "$VIDEO" -eq 0 ] && [ "$VIDEO_SKIPPED" -eq 0 ]; then
		# --no-video: turn off video an earlier install set up.
		as_service_user "$BIN_PATH" -data "$DATA_DIR" -disable-video >/dev/null 2>&1 || true
	fi

	write_unit
	write_updater
	write_zerotier_helper
	write_xmpp_helper
	write_terratak_helper
	if [ -n "$TERRATAK_FILE" ]; then
		setup_terratak
	elif [ "$TERRATAK_REMOVE" -eq 1 ] && [ -f "$TT_APP_UNIT" ]; then
		say "Removing TerraTAK (its tilesets are kept)"
		echo remove >"$DATA_DIR/terratak/request"
		EDGETAK_INSTALLER_RUN=1 sh "$TT_HELPER" || warn "removing TerraTAK failed"
	elif [ -f "$TT_APP_UNIT" ]; then
		TERRATAK=1 # loaded from the console, or earlier
		echo unit >"$DATA_DIR/terratak/request"
		EDGETAK_INSTALLER_RUN=1 sh "$TT_HELPER" >/dev/null 2>&1 || true
	fi
	save_options
	systemctl daemon-reload
	systemctl enable edgetak.service >/dev/null 2>&1
	systemctl enable --now edgetak-update.path >/dev/null 2>&1 || warn "could not enable updates from the console (edgetak-update.path)"
	systemctl enable --now edgetak-zerotier.path >/dev/null 2>&1 || warn "could not enable installing ZeroTier from the console (edgetak-zerotier.path)"
	systemctl enable --now edgetak-xmpp.path >/dev/null 2>&1 || warn "could not enable XMPP chat from the console (edgetak-xmpp.path)"
	systemctl enable --now edgetak-terratak.path >/dev/null 2>&1 || warn "could not enable TerraTAK from the console (edgetak-terratak.path)"
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
	if [ -f "$TT_APP_UNIT" ]; then
		cat <<EOF
  TerraTAK        https://$host:$TT_PORT/  (sign in to the admin console first)
                  Log: journalctl -u edgetak-terratak-app -f

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
