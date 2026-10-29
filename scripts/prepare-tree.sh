#!/usr/bin/env bash
# Assemble build/tree-<arch>: the files mkosi copies into the image that are not in the repo as text
# (k3s, tailscale, the k3s airgap images, noded, nodectl). Run before mkosi; CI runs the same script.
#
#   scripts/prepare-tree.sh [arm64|amd64]     (default: the host's, i.e. arm64 on Apple Silicon)
#   EDGE_DEBUG_BUILD=1                        build noded with -tags debugauth (debug profile only)
set -euo pipefail
cd "$(dirname "$0")/.."
. scripts/versions.env
arch="${1:-}"
if [ -z "$arch" ]; then case "$(uname -m)" in arm64|aarch64) arch=arm64;; *) arch=amd64;; esac; fi
cache="build/download/$arch"; tree="build/tree-$arch"
mkdir -p "$cache" "$tree/usr/local/bin" "$tree/usr/bin" "$tree/usr/lib/edge/images"

fetch() { # url file sha256-of-file(optional)
  [ -s "$cache/$2" ] || curl -fsSL --retry 3 -o "$cache/$2" "$1"
}
k3s_url="https://github.com/k3s-io/k3s/releases/download/${K3S_VERSION//+/%2B}"
k3s_bin="k3s"; [ "$arch" = arm64 ] && k3s_bin="k3s-arm64"
fetch "$k3s_url/$k3s_bin" "$k3s_bin"
fetch "$k3s_url/sha256sum-$arch.txt" "sha256sum-$arch.txt"
fetch "$k3s_url/k3s-airgap-images-$arch.tar.zst" "k3s-airgap-images-$arch.tar.zst"
# verify against the release's own checksum file
(cd "$cache" && grep -E " (\./)?($k3s_bin|k3s-airgap-images-$arch.tar.zst)$" "sha256sum-$arch.txt" | sed 's#  \./#  #' | shasum -a 256 -c -)
install -m 0755 "$cache/$k3s_bin" "$tree/usr/local/bin/k3s"
install -m 0644 "$cache/k3s-airgap-images-$arch.tar.zst" "$tree/usr/lib/edge/images/k3s-airgap-images.tar.zst"

ts="tailscale_${TAILSCALE_VERSION}_${arch}.tgz"
fetch "https://pkgs.tailscale.com/stable/$ts" "$ts"
fetch "https://pkgs.tailscale.com/stable/$ts.sha256" "$ts.sha256"
want=$(tr -d ' \n' < "$cache/$ts.sha256"); got=$(shasum -a 256 "$cache/$ts" | cut -d' ' -f1)
[ "$want" = "$got" ] || { echo "tailscale checksum mismatch ($want != $got)" >&2; exit 1; }
tar -xzf "$cache/$ts" -C "$cache"
install -m 0755 "$cache/tailscale_${TAILSCALE_VERSION}_${arch}/tailscale" "$tree/usr/local/bin/tailscale"
install -m 0755 "$cache/tailscale_${TAILSCALE_VERSION}_${arch}/tailscaled" "$tree/usr/local/bin/tailscaled"

# update source: a rolling release per architecture (sysupdate cannot tell architectures apart by file name)
case "$arch" in arm64) ch=stable-arm64;; *) ch=stable-amd64;; esac
echo "https://github.com/vmd1/edge-node-image/releases/download/$ch/" > "$tree/usr/lib/edge/update-url"
# trust only our own key for update manifests (Debian's default keyring is replaced); the debug profile brings its own
if [ -s signing/edge-node-image.pub.gpg ]; then
  install -D -m 0644 signing/edge-node-image.pub.gpg "$tree/usr/lib/systemd/import-pubring.gpg"
elif [ "${EDGE_RELEASE:-}" = 1 ]; then echo "release build without signing/edge-node-image.pub.gpg" >&2; exit 1; fi

tags=""; [ "${EDGE_DEBUG_BUILD:-}" = 1 ] && tags="-tags debugauth"
for c in noded nodectl; do
  # shellcheck disable=SC2086
  CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath -ldflags "-s -w" $tags -o "$tree/usr/bin/$c" "./cmd/$c"
done
echo "tree ready for $arch: $(du -sh "$tree" | cut -f1)"
