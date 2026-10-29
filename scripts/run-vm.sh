#!/usr/bin/env bash
# Boot mkosi.output/edge-node.raw in QEMU (arm64 guest, HVF). The disk is copied to .vm/ first so the build output
# stays pristine; delete .vm/ to start from scratch.
#
#   scripts/run-vm.sh              interactive: serial console on this terminal (Ctrl-A X quits)
#   scripts/run-vm.sh --log SECS   headless: run for SECS seconds, console to .vm/console.log, then stop
#
# Memory is 1024 MB to match the 954 MB OCI micro as closely as a local VM can.
set -euo pipefail
cd "$(dirname "$0")/.."
q=/opt/homebrew/share/qemu
mkdir -p .vm
[ -f .vm/disk.raw ] || cp -c mkosi.output/edge-node.raw .vm/disk.raw 2>/dev/null || cp mkosi.output/edge-node.raw .vm/disk.raw
[ -f .vm/vars.fd ] || cp "$q/edk2-arm-vars.fd" .vm/vars.fd
args=(-machine virt -accel hvf -cpu host -smp 2 -m 1024
  -drive if=pflash,format=raw,readonly=on,file="$q/edk2-aarch64-code.fd"
  -drive if=pflash,format=raw,file=.vm/vars.fd
  -drive if=none,id=disk0,format=raw,file=.vm/disk.raw -device virtio-blk-pci,drive=disk0
  -netdev user,id=net0 -device virtio-net-pci,netdev=net0,romfile=
  -display none -monitor none)
if [ "${1:-}" = "--log" ]; then
  secs="${2:?seconds}"
  : > .vm/console.log
  qemu-system-aarch64 "${args[@]}" -serial file:.vm/console.log &
  pid=$!
  sleep "$secs"; kill "$pid" 2>/dev/null || true; wait "$pid" 2>/dev/null || true
  echo "console log: .vm/console.log"
else
  exec qemu-system-aarch64 "${args[@]}" -serial mon:stdio
fi
