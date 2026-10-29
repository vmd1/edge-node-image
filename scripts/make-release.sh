#!/usr/bin/env bash
# Turn a mkosi build (with SplitArtifacts=uki,partitions) into a sysupdate release directory:
#   edge-node_<ver>_<uuid>.root.raw.xz, edge-node_<ver>_<uuid>.root-verity.raw.xz, edge-node_<ver>.efi,
#   SHA256SUMS and SHA256SUMS.gpg (detached signature).
# The two partition UUIDs are derived from the root hash exactly as systemd expects (first and last 128 bits).
#
#   scripts/make-release.sh <mkosi-output-dir> <version> <release-dir> [gpg-key-id]
# GNUPGHOME selects the keyring; the local tests use .vm/gnupg (see scripts/test-key.sh).
set -euo pipefail
out="${1:?mkosi output dir}"; ver="${2:?version}"; rel="${3:?release dir}"; key="${4:-edge-test}"
id=edge-node
root=$(ls "$out"/$id.root-*.raw | grep -v verity | head -1)
verity=$(ls "$out"/$id.root-*-verity.raw | head -1)
uki="$out/$id.efi"
hash=$(grep -a -o 'roothash=[0-9a-f]\{64\}' "$uki" | head -1 | cut -d= -f2)
[ -n "$hash" ] || { echo "no roothash in $uki" >&2; exit 1; }
uuid() { echo "$1" | sed -E 's/^(.{8})(.{4})(.{4})(.{4})(.{12})$/\1-\2-\3-\4-\5/'; }
ru=$(uuid "${hash:0:32}"); vu=$(uuid "${hash:32:32}")
mkdir -p "$rel"; rm -f "$rel"/*
# xz, not zstd: systemd-sysupdate 257 writes .zst payloads into the partition still compressed; .xz is decompressed
lvl="${XZ_LEVEL:-3}"
xz -T0 "-$lvl" -c "$root" > "$rel/${id}_${ver}_${ru}.root.raw.xz"
xz -T0 "-$lvl" -c "$verity" > "$rel/${id}_${ver}_${vu}.root-verity.raw.xz"
# the UKI is published uncompressed: sysupdate does not decompress regular-file targets (it wrote the .zst bytes as the UKI)
cp "$uki" "$rel/${id}_${ver}.efi"
(cd "$rel" && shasum -a 256 -b -- *.xz *.efi | sed 's/ \*/  /' > SHA256SUMS && gpg --batch --yes --detach-sign -u "$key" -o SHA256SUMS.gpg SHA256SUMS)
echo "release $ver: root uuid $ru, verity uuid $vu"; ls -la "$rel"
