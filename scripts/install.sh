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

latest_tag() {
  curl -fsSL "$API" | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -n 1
}

install_with_brew() {
  brew tap amitbet/dynapp-agent "https://github.com/${REPO}"
  if brew list dynapp-shell-agent >/dev/null 2>&1; then
    brew upgrade dynapp-shell-agent
  else
    brew install dynapp-shell-agent
  fi
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

if [ "$(uname -s)" = Darwin ] && need_cmd brew; then
  install_with_brew
elif need_cmd apt-get; then
  install_with_apt
else
  echo "install Homebrew or apt, or download a release from https://github.com/${REPO}/releases/latest" >&2
  exit 1
fi

echo "installed dynapp-shell-agent. Start it with: dynapp-shell-agent install && dynapp-shell-agent start"
