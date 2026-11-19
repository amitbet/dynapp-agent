#!/usr/bin/env bash
set -euo pipefail

dir=${1:?usage: publish-apt-repo.sh <deb-directory>}
cd "$dir"
if ! command -v dpkg-scanpackages >/dev/null; then
  echo "dpkg-scanpackages is required (apt install dpkg-dev)" >&2
  exit 1
fi
# -m keeps every architecture of a package; without it only one of the
# amd64/arm64 builds of each package would be listed.
dpkg-scanpackages -m . /dev/null >Packages
gzip -9nc Packages >Packages.gz

# apt ignores a flat repository whose Release file lists no checksums.
hashes() {
  local tool=$1
  for file in Packages Packages.gz; do
    printf ' %s %s %s\n' "$("$tool" "$file" | cut -d' ' -f1)" "$(wc -c <"$file" | tr -d ' ')" "$file"
  done
}
{
  echo "Origin: DynApp Agent"
  echo "Label: dynapp-agent"
  echo "Architectures: amd64 arm64"
  echo "Description: DynApp agent packages"
  echo "Date: $(date -Ru)"
  echo "MD5Sum:"
  hashes md5sum
  echo "SHA256:"
  hashes sha256sum
} >Release
