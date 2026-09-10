#!/usr/bin/env bash
# Build and push a development driver image, and echo the reference.
#
# No container daemon is involved: ko cross-compiles with the Go toolchain and
# pushes the OCI image itself. See .ko.yaml for why.
#
#   ./hack/dev-image.sh            # tag from the current commit
#   ./hack/dev-image.sh my-tag     # explicit tag
set -euo pipefail

cd "$(dirname "$0")/.."

tag="${1:-dev-$(git rev-parse --short HEAD)}"
# The cluster this driver is developed against is arm64; override for another.
platform="${KO_PLATFORM:-linux/arm64}"

export KO_DOCKER_REPO="${KO_DOCKER_REPO:-ghcr.io/piwi3910/truenas-csi}"
export VERSION="$tag"

# --bare keeps the repository exactly as given rather than appending a hash of
# the import path, so the reference matches what the chart and the release
# images use.
built=$(ko build ./cmd/truenas-csi --bare --platform="$platform" --tags="$tag" 2>/dev/null | tail -1)

# ko puts the binary at /ko-app/truenas-csi; the release image puts it at
# /truenas-csi, and docs/pool-administration.md tells operators to exec it
# there. Append a one-file layer carrying a symlink so a dev image answers to
# both paths, and nothing that works against a release image fails against this.
linkdir=$(mktemp -d)
python3 - "$linkdir/link.tar" <<'PYTAR'
import sys, tarfile

with tarfile.open(sys.argv[1], "w") as t:
    info = tarfile.TarInfo("truenas-csi")
    info.type = tarfile.SYMTYPE
    info.linkname = "/ko-app/truenas-csi"
    t.addfile(info)
PYTAR
ref=$(crane append -b "$built" -f "$linkdir/link.tar" -t "$KO_DOCKER_REPO:$tag")
rm -rf "$linkdir"

# Prove the platform is what the cluster needs before anything is rolled out: a
# mismatch here is invisible until every pod reports "exec format error".
got=$(crane config "$ref" | python3 -c 'import json,sys; c=json.load(sys.stdin); print(c["os"]+"/"+c["architecture"])')
if [ "$got" != "$platform" ]; then
  echo "built $got, wanted $platform" >&2
  exit 1
fi
echo "$ref"
