#!/bin/sh
# Run on a Linux host with KVM (hades-02): boot an x86-64 image, serial console on 127.0.0.1:4555 (tunnel it with
# `ssh -L 4555:127.0.0.1:4555 host`), user-mode networking, 1 GB RAM like the OCI nodes. The guest runs as a transient systemd
# service (unit edge-vm) as the calling user with group kvm, capped in memory with no swap, so a runaway guest cannot hurt the
# host's real workloads. Needs passwordless sudo for systemd-run and systemctl.
#   scripts/run-vm-kvm-remote.sh <disk.raw>      start
#   scripts/run-vm-kvm-remote.sh stop            stop
set -eu
dir=$(cd "$(dirname "$0")" && pwd)
sudo systemctl stop edge-vm 2>/dev/null || true
[ "${1:?disk.raw or stop}" = stop ] && exit 0
disk=$(readlink -f "$1")
vars="$dir/vars-edge-vm.fd"; cp /usr/share/OVMF/OVMF_VARS_4M.fd "$vars"
sudo systemd-run --unit=edge-vm --uid="$(id -u)" --gid=kvm --collect -p MemoryMax=1400M -p MemorySwapMax=0 -p Nice=10 -p CPUQuota=200% -- \
  qemu-system-x86_64 -name edge-vm -machine q35 -enable-kvm -cpu host -smp 2 -m 1024 \
  -drive if=pflash,format=raw,readonly=on,file=/usr/share/OVMF/OVMF_CODE_4M.fd -drive if=pflash,format=raw,file="$vars" \
  -drive if=none,id=disk0,format=raw,file="$disk" -device virtio-blk-pci,drive=disk0 \
  -netdev user,id=net0,hostfwd=tcp:127.0.0.1:7777-:7777 -device virtio-net-pci,netdev=net0 \
  -display none -monitor none -serial tcp:127.0.0.1:4555,server,nowait
