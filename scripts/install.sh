#!/bin/sh
set -eu

REPO="${DYNAPP_AGENT_REPO:-amitbet/dynapp-agent}"
APT_REPO="https://amitbet.github.io/dynapp-agent/deb"
API="https://api.github.com/repos/${REPO}/releases/latest"

need_cmd() {
  command -v "$1" >/dev/null 2>&1
}

run_root() {
  if [ "$(id -u)" -eq 0 ]; then
    "$@"
  elif need_cmd sudo; then
    sudo "$@"
  else
    echo "need root to run: $*" >&2
    exit 1
  fi
}

cpu() {
  case "$(uname -m)" in
    x86_64 | amd64) echo amd64 ;;
    aarch64 | arm64) echo arm64 ;;
    *)
      echo "unsupported architecture $(uname -m)" >&2
      exit 1
      ;;
  esac
}

github_os() {
  case "$(uname -s)" in
    Darwin) echo darwin ;;
    Linux) echo linux ;;
    *)
      echo "unsupported OS $(uname -s)" >&2
      exit 1
      ;;
  esac
}

latest_tag() {
  curl -fsSL "$API" | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -n 1
}

file_sha256() {
  if need_cmd sha256sum; then
    sha256sum "$1" | awk '{print tolower($1)}'
  elif need_cmd shasum; then
    shasum -a 256 "$1" | awk '{print tolower($1)}'
  else
    echo "need sha256sum or shasum to verify the download" >&2
    exit 1
  fi
}

# want_app_host: the Linux app host (native desktop apps) needs GTK and
# WebKitGTK, so it is installed on desktops only. DYNAPP_APP_HOST=1 forces it,
# DYNAPP_APP_HOST=0 skips it.
want_app_host() {
  [ "$(uname -s)" = Linux ] || return 1
  case "${DYNAPP_APP_HOST:-}" in
    1 | yes | true) return 0 ;;
    0 | no | false) return 1 ;;
  esac
  [ -n "${DISPLAY:-}" ] || [ -n "${WAYLAND_DISPLAY:-}" ]
}

# app_host_note: outside apt, this script installs only the agent. The agent
# downloads dynapp-app-host itself (signed, like its own updates) the first
# time it runs in a desktop session; a script that fetches further programs
# is what endpoint protection treats as a dropper.
app_host_note() {
  echo "The agent downloads the native app host (dynapp-app-host) itself when it runs in your desktop session."
  if need_cmd ldconfig && ! ldconfig -p 2>/dev/null | grep -q 'libwebkit2gtk-4\.1'; then
    echo "Native apps also need GTK 3 and WebKitGTK 4.1: install libwebkit2gtk-4.1-0 (Debian/Ubuntu), webkit2gtk4.1 (Fedora), or webkit2gtk-4.1 (Arch)." >&2
  fi
}

bin_dir() {
  echo "${DYNAPP_AGENT_BIN_DIR:-/usr/local/bin}"
}

install_file() {
  src=$1
  dest_dir=$2
  dest="${dest_dir}/${3:-dynapp-shell-agent}"
  if [ -d "$dest_dir" ] && [ -w "$dest_dir" ]; then
    install -m 0755 "$src" "$dest"
  elif mkdir -p "$dest_dir" 2>/dev/null && [ -w "$dest_dir" ]; then
    install -m 0755 "$src" "$dest"
  else
    run_root mkdir -p "$dest_dir"
    run_root install -m 0755 "$src" "$dest"
  fi
  echo "$dest"
}

install_from_github() {
  arch=$(cpu)
  goos=$(github_os)
  tag=$(latest_tag)
  if [ -z "$tag" ]; then
    echo "could not resolve the latest GitHub release" >&2
    exit 1
  fi
  version="${tag#v}"
  asset="dynapp-shell-agent-${version}-${goos}-${arch}"
  tmpdir=$(mktemp -d)
  curl -fsSL "https://github.com/${REPO}/releases/download/${tag}/${asset}" -o "${tmpdir}/${asset}"
  curl -fsSL "https://github.com/${REPO}/releases/download/${tag}/${asset}.sha256" -o "${tmpdir}/${asset}.sha256"
  expected=$(awk '{print tolower($1); exit}' "${tmpdir}/${asset}.sha256")
  actual=$(file_sha256 "${tmpdir}/${asset}")
  if [ -z "$expected" ] || [ "$expected" != "$actual" ]; then
    rm -rf "$tmpdir"
    echo "checksum mismatch for ${asset}" >&2
    exit 1
  fi
  dest=$(install_file "${tmpdir}/${asset}" "$(bin_dir)")
  rm -rf "$tmpdir"
  echo "installed ${dest} from ${tag}"
  if want_app_host; then
    app_host_note
  fi
}

install_with_apt() {
  arch=$(cpu)
  if curl -fsSL "$APT_REPO/Packages" >/dev/null 2>&1; then
    echo "deb [trusted=yes] ${APT_REPO} ./" | run_root tee /etc/apt/sources.list.d/dynapp-agent.list >/dev/null
    run_root apt-get update -y
    if want_app_host; then
      run_root apt-get install -y dynapp-shell-agent dynapp-app-host
    else
      run_root apt-get install -y dynapp-shell-agent
    fi
    return
  fi
  tag=$(latest_tag)
  if [ -z "$tag" ]; then
    echo "could not resolve the latest GitHub release" >&2
    exit 1
  fi
  version="${tag#v}"
  deb="dynapp-shell-agent_${version}_${arch}.deb"
  tmpdir=$(mktemp -d)
  curl -fsSL "https://github.com/${REPO}/releases/download/${tag}/${deb}" -o "${tmpdir}/${deb}"
  run_root apt-get install -y "${tmpdir}/${deb}"
  if want_app_host; then
    host_deb="dynapp-app-host_${version}_${arch}.deb"
    curl -fsSL "https://github.com/${REPO}/releases/download/${tag}/${host_deb}" -o "${tmpdir}/${host_deb}"
    run_root apt-get install -y "${tmpdir}/${host_deb}"
  fi
  rm -rf "$tmpdir"
}

start_linux_agent() {
  if [ "$(id -u)" -eq 0 ] || ! systemctl --user show-environment >/dev/null 2>&1; then
    echo "DynApp agent installed. To start it, run dynapp-shell-agent install && dynapp-shell-agent start from your Linux user session." >&2
    return
  fi

  if ! systemctl --user cat dynapp-shell-agent.service >/dev/null 2>&1; then
    "$1" install
  fi
  systemctl --user add-wants default.target dynapp-shell-agent.service
  systemctl --user start dynapp-shell-agent.service
  if [ -n "${DISPLAY:-}${WAYLAND_DISPLAY:-}" ]; then
    "$1" enroll --if-needed || echo "To link this computer, run dynapp-shell-agent enroll." >&2
  fi
  echo "DynApp agent is running. In Dyner, select Check native apps."
}

os=$(uname -s)
case "$os" in
  MINGW* | MSYS* | CYGWIN*)
    echo "On Windows use: irm https://raw.githubusercontent.com/${REPO}/main/scripts/install.ps1 | iex" >&2
    exit 1
    ;;
esac

if [ "$os" = Darwin ]; then
  install_from_github
  echo "installed dynapp-shell-agent. Start it with: dynapp-shell-agent install && dynapp-shell-agent start"
elif [ "$os" = Linux ]; then
  if need_cmd apt-get; then
    install_with_apt
    start_linux_agent /usr/bin/dynapp-shell-agent
  else
    install_from_github
    start_linux_agent "$(bin_dir)/dynapp-shell-agent"
  fi
else
  echo "download a release from https://github.com/${REPO}/releases/latest" >&2
  exit 1
fi
