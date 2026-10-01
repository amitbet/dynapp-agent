#!/bin/sh
# Rebuilds the universal macOS app host embedded by the agent.
#
# Every installed app bundle carries a copy of this binary. macOS ties camera,
# microphone, and Local Network grants of those ad-hoc signed bundles to the
# binary's exact bytes, so a rebuild makes every installed app ask again after
# the agent reinstalls it. Rebuild only for real host changes; the page bridge
# lives in the agent (shellagent/native_host_bridge.js) and needs no rebuild.
set -eu
cd "$(dirname "$0")"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
for arch in arm64 x86_64; do
  xcrun swiftc -O -target "$arch-apple-macos13.0" -o "$work/host-$arch" main.swift
done
lipo -create "$work/host-arm64" "$work/host-x86_64" -output "$work/dynapp-app-host"
strip -x "$work/dynapp-app-host"
mv "$work/dynapp-app-host" dynapp-app-host
shasum -a 256 dynapp-app-host
