#!/usr/bin/env bash
# Build the image with mkosi inside a privileged Debian container (mkosi does not run on macOS).
# Builds for the container's architecture: arm64 on Apple Silicon, which is what the local VM uses.
#
#   EDGE_PROFILE=debug   add the debug profile (serial autologin, self-check, test key)
#   EDGE_VERSION=1.0.0   image version (os-release IMAGE_VERSION, partition labels, UKI name)
#   EDGE_OUT=dir         output directory (default mkosi.output)
set -euo pipefail
cd "$(dirname "$0")/.."
docker image inspect edge-mkosi >/dev/null 2>&1 || docker build -q -t edge-mkosi -f scripts/Dockerfile.mkosi scripts
out="${EDGE_OUT:-mkosi.output}"
mkdir -p "$out" mkosi.cache
docker run --rm --privileged -v "$PWD":/work -w /work \
  -e EDGE_PROFILE="${EDGE_PROFILE:-}" -e EDGE_VERSION="${EDGE_VERSION:-1.0.0}" -e OUT="$out" \
  edge-mkosi bash -euxc '
    mkosi --force ${EDGE_PROFILE:+--profile "$EDGE_PROFILE"} --image-version "$EDGE_VERSION" --output-directory "$OUT" build
  '
