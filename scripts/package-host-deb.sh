#!/usr/bin/env bash
# Packages dynapp-app-host, the Linux native app host, as its own .deb so a
# headless agent install never pulls in GTK and WebKitGTK.
set -euo pipefail

binary=${1:?usage: package-host-deb.sh <linux-host-binary> <version> <goarch>}
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
mkdir -p "$root/usr/bin" "$root/DEBIAN"
install -m 0755 "$binary" "$root/usr/bin/dynapp-app-host"
cat >"$root/DEBIAN/control" <<CONTROL
Package: dynapp-app-host
Version: ${version}
Section: utils
Priority: optional
Architecture: ${debarch}
Depends: dynapp-shell-agent (>= ${version}), libgtk-3-0, libwebkit2gtk-4.1-0 (>= 2.40), libglib2.0-0, desktop-file-utils, shared-mime-info
Recommends: gstreamer1.0-plugins-good, xdg-utils
Maintainer: DynApp <amit.bet@gmail.com>
Homepage: https://github.com/amitbet/dynapp-agent
Description: Native desktop windows for DynApps
 Runs DynApps installed from Dyner as desktop apps, each in its own
 window with its own launcher entry, using GTK and WebKitGTK. The DynApp
 agent starts it.
CONTROL

out="dynapp-app-host_${version}_${debarch}.deb"
dpkg-deb --root-owner-group --build "$root" "$out" >/dev/null
echo "$out"
