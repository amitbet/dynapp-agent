#!/usr/bin/env bash
set -euo pipefail

binary=${1:?usage: package-deb.sh <linux-binary> <version> <goarch>}
version=${2:?}
goarch=${3:?}

case "$goarch" in
  amd64) debarch=amd64 ;;
  arm64) debarch=arm64 ;;
  *)
    echo "unsupported GOARCH $goarch" >&2
    exit 1
    ;;
esac

root=$(mktemp -d)
trap 'rm -rf "$root"' EXIT
mkdir -p "$root/usr/bin" "$root/usr/lib/dynapp-agent" "$root/lib/systemd/system" "$root/DEBIAN"
install -m 0755 "$binary" "$root/usr/bin/dynapp-shell-agent"

# The agent runs as the user and cannot replace a binary dpkg owns, so the
# package upgrades itself: a root timer runs apt for the DynApp packages only,
# and the running agent restarts into each newer version it finds in place.
cat >"$root/usr/lib/dynapp-agent/apt-upgrade" <<'SCRIPT'
#!/bin/sh
# Upgrades the DynApp agent packages from their apt source. Run as root by
# dynapp-agent-upgrade.service; the running agent restarts into the new
# version by itself.
set -eu
list=/etc/apt/sources.list.d/dynapp-agent.list
if [ ! -f "$list" ]; then
  echo "no $list; nothing to upgrade from"
  exit 0
fi
export DEBIAN_FRONTEND=noninteractive
apt-get -q update -o Dir::Etc::sourcelist="$list" -o Dir::Etc::sourceparts=- -o APT::Get::List-Cleanup=0
apt-get -q install -y --only-upgrade --no-install-recommends \
  -o DPkg::Lock::Timeout=600 -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold \
  dynapp-shell-agent dynapp-app-host
SCRIPT
chmod 0755 "$root/usr/lib/dynapp-agent/apt-upgrade"

cat >"$root/lib/systemd/system/dynapp-agent-upgrade.service" <<'UNIT'
[Unit]
Description=Upgrade the DynApp agent packages
Documentation=https://github.com/amitbet/dynapp-agent
Wants=network-online.target
After=network-online.target

[Service]
Type=oneshot
ExecStart=/usr/lib/dynapp-agent/apt-upgrade
UNIT

cat >"$root/lib/systemd/system/dynapp-agent-upgrade.timer" <<'UNIT'
[Unit]
Description=Check hourly for DynApp agent package upgrades

[Timer]
OnBootSec=5min
OnUnitActiveSec=1h
RandomizedDelaySec=5min
Persistent=true

[Install]
WantedBy=timers.target
UNIT

cat >"$root/DEBIAN/postinst" <<'SCRIPT'
#!/bin/sh
set -e
repo=https://amitbet.github.io/dynapp-agent/deb
list=/etc/apt/sources.list.d/dynapp-agent.list
if [ "$1" = configure ]; then
  # Installed from a downloaded .deb: add the source the upgrade timer reads.
  if [ ! -e "$list" ] && ! grep -rqsF "$repo" /etc/apt/sources.list /etc/apt/sources.list.d/; then
    printf '# Added by dynapp-shell-agent; dynapp-agent-upgrade.timer upgrades from it.\ndeb [trusted=yes] %s ./\n' "$repo" >"$list"
  fi
  if [ -d /run/systemd/system ]; then
    systemctl daemon-reload >/dev/null 2>&1 || true
    systemctl enable --now dynapp-agent-upgrade.timer >/dev/null 2>&1 || true
  fi
fi
exit 0
SCRIPT

cat >"$root/DEBIAN/prerm" <<'SCRIPT'
#!/bin/sh
set -e
if [ "$1" = remove ] && [ -d /run/systemd/system ]; then
  systemctl disable --now dynapp-agent-upgrade.timer >/dev/null 2>&1 || true
fi
exit 0
SCRIPT

cat >"$root/DEBIAN/postrm" <<'SCRIPT'
#!/bin/sh
set -e
list=/etc/apt/sources.list.d/dynapp-agent.list
if [ "$1" = purge ] && grep -qs '^# Added by dynapp-shell-agent' "$list"; then
  rm -f "$list"
fi
if [ -d /run/systemd/system ]; then
  systemctl daemon-reload >/dev/null 2>&1 || true
fi
exit 0
SCRIPT
chmod 0755 "$root/DEBIAN/postinst" "$root/DEBIAN/prerm" "$root/DEBIAN/postrm"
cat >"$root/DEBIAN/control" <<EOF
Package: dynapp-shell-agent
Version: ${version}
Section: utils
Priority: optional
Architecture: ${debarch}
Suggests: dynapp-app-host
Maintainer: DynApp <amit.bet@gmail.com>
Homepage: https://github.com/amitbet/dynapp-agent
Description: Local DynApp agent
 The agent lets hosted DynApps use local files, network, processes,
 and other approved capabilities.
EOF

out="dynapp-shell-agent_${version}_${debarch}.deb"
dpkg-deb --root-owner-group --build "$root" "$out" >/dev/null
echo "$out"
