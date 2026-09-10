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
mkdir -p "$root/usr/bin" "$root/DEBIAN"
install -m 0755 "$binary" "$root/usr/bin/dynapp-shell-agent"
cat >"$root/DEBIAN/control" <<EOF
Package: dynapp-shell-agent
Version: ${version}
Section: utils
Priority: optional
Architecture: ${debarch}
Maintainer: DynApp <amit.bet@gmail.com>
Homepage: https://github.com/amitbet/dynapp-agent
Description: Local DynApp agent
 The agent lets hosted DynApps use local files, network, processes,
 and other approved capabilities.
EOF

out="dynapp-shell-agent_${version}_${debarch}.deb"
dpkg-deb --root-owner-group --build "$root" "$out" >/dev/null
echo "$out"
