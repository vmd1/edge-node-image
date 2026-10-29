#!/usr/bin/env bash
# Build the image with mkosi inside a privileged Debian container (mkosi does not run on macOS).
# Builds for the container's architecture: arm64 on Apple Silicon, which is what the local VM uses.
# Output lands in mkosi.output/.
set -euo pipefail
cd "$(dirname "$0")/.."
docker run --rm --privileged -e EDGE_PROFILE="${EDGE_PROFILE:-}" -v "$PWD":/work -w /work debian:trixie bash -euxc '
  export DEBIAN_FRONTEND=noninteractive
  apt-get update -qq
  apt-get install -y -qq --no-install-recommends mkosi debian-archive-keyring apt dpkg bubblewrap \
    systemd-boot-efi systemd-ukify systemd-repart dosfstools mtools e2fsprogs squashfs-tools zstd xz-utils \
    ca-certificates python3 kmod fdisk
  mkosi --force ${EDGE_PROFILE:+--profile "$EDGE_PROFILE"} build
'
