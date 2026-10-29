#!/usr/bin/env bash
# Boot an x86-64 image under QEMU software emulation (no acceleration on Apple Silicon: minutes, not seconds), console to a file.
#   scripts/run-vm-x86.sh <image.raw> <seconds> [bios]
# Default is UEFI (OVMF); "bios" boots with SeaBIOS instead (systemd-boot cannot boot that way: it shows what happens).
set -euo pipefail
img="$1"; secs="$2"; mode="${3:-uefi}"
q=/opt/homebrew/share/qemu; d=.vm/run-x86; rm -rf "$d"; mkdir -p "$d"
cp -c "$img" "$d/disk.raw" 2>/dev/null || cp "$img" "$d/disk.raw"; truncate -s 12G "$d/disk.raw"
fw=(-drive "if=pflash,format=raw,readonly=on,file=$q/edk2-x86_64-code.fd")
if [ "$mode" = uefi ]; then cp "$q/edk2-i386-vars.fd" "$d/vars.fd"; fw+=(-drive "if=pflash,format=raw,file=$d/vars.fd"); else fw=(-bios "$q/bios-256k.bin"); fi
qemu-system-x86_64 -machine q35 -accel tcg,thread=multi -cpu max -smp 2 -m 1024 "${fw[@]}" \
  -drive if=none,id=disk0,format=raw,file="$d/disk.raw" -device virtio-blk-pci,drive=disk0 \
  -netdev user,id=net0 -device virtio-net-pci,netdev=net0,romfile= \
  -display none -monitor none -serial file:"$d/console.log" &
pid=$!; sleep "$secs"; kill "$pid" 2>/dev/null || true; wait "$pid" 2>/dev/null || true
echo "console: $d/console.log"
