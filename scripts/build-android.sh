#!/usr/bin/env bash
# Builds the agent for the DynApp Android app.
#
#   scripts/build-android.sh <output-jniLibs-dir> [version]
#
# Writes <out>/<abi>/libdynappagent.so for arm64-v8a and x86_64 (emulators).
# The file is an executable, not a shared library: Android installs it under
# the app's nativeLibraryDir, the only app location it may execute from.
# Needs the Android NDK (ANDROID_NDK_HOME, or the newest ndk/ under
# ANDROID_HOME / ANDROID_SDK_ROOT).
set -euo pipefail

out="${1:?usage: build-android.sh <output-jniLibs-dir> [version]}"
mkdir -p "$out"
out="$(cd "$out" && pwd)"
version="${2:-${VERSION:-dev}}"
root="$(cd "$(dirname "$0")/.." && pwd)"
api=26

ndk="${ANDROID_NDK_HOME:-}"
if [ -z "$ndk" ]; then
  sdk="${ANDROID_HOME:-${ANDROID_SDK_ROOT:-}}"
  if [ -n "$sdk" ] && [ -d "$sdk/ndk" ]; then
    ndk="$sdk/ndk/$(ls "$sdk/ndk" | sort -V | tail -n1)"
  fi
fi
if [ -z "$ndk" ] || [ ! -d "$ndk" ]; then
  echo "build-android.sh: set ANDROID_NDK_HOME or ANDROID_HOME with an installed NDK" >&2
  exit 1
fi
host_tag="$(uname -s | tr '[:upper:]' '[:lower:]')-x86_64"
toolchain="$ndk/toolchains/llvm/prebuilt/$host_tag/bin"

# pion's Android interface lookup (wlynxg/anet) links against net internals,
# which Go 1.23+ only allows with -checklinkname=0.
ldflags="-checklinkname=0 -s -w -X github.com/amitbet/dynapp-agent/shellagent.AgentVersion=${version}"

build() {
  local goarch="$1" abi="$2" clang="$3"
  mkdir -p "$out/$abi"
  (cd "$root" && GOOS=android GOARCH="$goarch" CGO_ENABLED=1 CC="$toolchain/$clang" \
    go build -trimpath -ldflags="$ldflags" -o "$out/$abi/libdynappagent.so" .)
  echo "built $out/$abi/libdynappagent.so"
}

build arm64 arm64-v8a "aarch64-linux-android${api}-clang"
build amd64 x86_64 "x86_64-linux-android${api}-clang"
