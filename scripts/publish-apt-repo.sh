#!/usr/bin/env bash
set -euo pipefail

dir=${1:?usage: publish-apt-repo.sh <deb-directory>}
cd "$dir"
if ! command -v dpkg-scanpackages >/dev/null; then
  echo "dpkg-scanpackages is required (apt install dpkg-dev)" >&2
  exit 1
fi
dpkg-scanpackages . /dev/null >Packages
gzip -9nc Packages >Packages.gz
cat >Release <<EOF
Origin: DynApp Agent
Label: dynapp-agent
Architectures: amd64 arm64
Description: DynApp agent packages
Date: $(date -Ru)
EOF
