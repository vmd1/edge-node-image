#!/usr/bin/env bash
# SPDX SBOM of the root filesystem plus the Go binaries, and a vulnerability scan. Run by the release workflow (Linux runner,
# needs root for the loop mount, syft and grype on PATH).
#   scripts/make-sbom.sh <root.raw> <out-dir> <version>
# Fails the build on a critical vulnerability that has a fix available and is not listed in .grype.yaml (the VEX/ignore file).
set -euo pipefail
root="${1:?root partition image}"; out="${2:?output dir}"; ver="${3:?version}"
mkdir -p "$out"; mnt=$(mktemp -d)
cleanup() { sudo umount "$mnt" 2>/dev/null || true; rmdir "$mnt" 2>/dev/null || true; }
trap cleanup EXIT
sudo mount -o loop,ro "$root" "$mnt"
syft "dir:$mnt" --source-name edge-node --source-version "$ver" -o "spdx-json=$out/edge-node_${ver}.spdx.json"
syft "dir:$mnt" -o "table=$out/edge-node_${ver}.packages.txt" >/dev/null
grype "sbom:$out/edge-node_${ver}.spdx.json" -c .grype.yaml --only-fixed --fail-on critical -o table | tee "$out/edge-node_${ver}.vulns.txt"
