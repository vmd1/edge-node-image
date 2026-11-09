#!/usr/bin/env bash
# Build the image with mkosi inside a privileged Debian container (mkosi does not run on macOS).
# Builds for the container's architecture: arm64 on Apple Silicon, which is what the local VM uses.
#
#   EDGE_PROFILE=debug   add the debug profile (serial autologin, self-check, test key)
#   EDGE_VERSION=1.0.0   image version (os-release IMAGE_VERSION, partition labels, UKI name)
#   EDGE_OUT=dir         output directory (default mkosi.output)
set -euo pipefail
cd "$(dirname "$0")/.."
# EDGE_ARCH=amd64 cross-builds the x86-64 image from the native container (mkosi's sandbox does not run under x86 emulation);
# the default is the host architecture (arm64 on Apple Silicon).
plat=""; img=edge-mkosi; march=""
if [ "${EDGE_ARCH:-}" = amd64 ]; then march="--architecture x86-64"; fi
# shellcheck disable=SC2086
docker image inspect $img >/dev/null 2>&1 || docker build $plat -q -t $img -f scripts/Dockerfile.mkosi scripts
out="${EDGE_OUT:-mkosi.output}"
# reproducible inputs: file timestamps come from the commit, and EDGE_DEBIAN_SNAPSHOT (YYYYMMDDTHHMMSSZ) pins the Debian
# archive to a snapshot.debian.org state so the same commit builds the same root hash. Both are recorded in the release notes.
export SOURCE_DATE_EPOCH="${SOURCE_DATE_EPOCH:-$(git log -1 --format=%ct 2>/dev/null || date +%s)}"
mirror=""; [ -z "${EDGE_DEBIAN_SNAPSHOT:-}" ] || mirror="--mirror https://snapshot.debian.org/archive/debian/${EDGE_DEBIAN_SNAPSHOT}/"
mkdir -p "$out"
[ -n "${EDGE_SKIP_TREE:-}" ] || EDGE_DEBUG_BUILD="$(case ",${EDGE_PROFILE:-}," in *,debug,*) echo 1;; esac)" scripts/prepare-tree.sh "${EDGE_ARCH:-}"
# The mkosi cache must live on a filesystem with xattr support: the Mac bind mount does not have one. A named volume keeps it
# between builds (docker volume rm edge-mkosi-cache to start clean).
# shellcheck disable=SC2086
docker run --rm --privileged $plat -v "$PWD":/work -v "edge-mkosi-cache-${EDGE_ARCH:-native}:/cache" -w /work \
  -e EDGE_PROFILE="${EDGE_PROFILE:-}" -e MARCH="$march" -e MIRROR="$mirror" -e SOURCE_DATE_EPOCH -e EDGE_VERSION="${EDGE_VERSION:-1.0.0}" -e OUT="$out" \
  $img bash -euxc '
    mkosi --force $MARCH $MIRROR ${EDGE_PROFILE:+--profile "$EDGE_PROFILE"} --image-version "$EDGE_VERSION" --output-directory "$OUT" --cache-directory /cache build
  '
