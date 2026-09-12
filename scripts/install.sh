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

bin_dir() {
  echo "${DYNAPP_AGENT_BIN_DIR:-/usr/local/bin}"
}

install_file() {
  src=$1
  dest_dir=$2
  dest="${dest_dir}/dynapp-shell-agent"
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
}

install_with_apt() {
  arch=$(cpu)
  if curl -fsSL "$APT_REPO/Packages" >/dev/null 2>&1; then
    echo "deb [trusted=yes] ${APT_REPO} ./" | run_root tee /etc/apt/sources.list.d/dynapp-agent.list >/dev/null
    run_root apt-get update -y
    run_root apt-get install -y dynapp-shell-agent
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
  rm -rf "$tmpdir"
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
elif need_cmd apt-get; then
  install_with_apt
elif [ "$os" = Linux ]; then
  install_from_github
else
  echo "download a release from https://github.com/${REPO}/releases/latest" >&2
  exit 1
fi

echo "installed dynapp-shell-agent. Start it with: dynapp-shell-agent install && dynapp-shell-agent start"
