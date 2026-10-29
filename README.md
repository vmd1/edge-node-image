# edge-node-image

A read-only Debian 13 image for the six 1 GB OCI edge nodes of the `vmd1` k3s cluster, updated in place with
`systemd-sysupdate` (A/B root slots), with a small gRPC agent (`noded`) instead of SSH.

**Status: scaffold. Nothing here has been run on OCI. Everything below is untested until the matching test in
`docs/testing.md` says otherwise.**

The plan, decisions and open questions live in the homelab repo: `docs/edge-node-image.md`.

## Layout

| Path | What |
| :--- | :--- |
| `mkosi.conf` | Image definition (Debian trixie, no SSH, no cloud-init). |
| `mkosi.repart/` | Partition layout: ESP, root A, root B, `/var`. |
| `mkosi.extra/` | Files copied into the image (sysctl, zram, journald, fstab, sysupdate transfers). |
| `scripts/` | Local build (Docker) and local VM boot (QEMU). |
| `docs/testing.md` | What has been verified, where (local VM or OCI), and what has not. |

## Local loop (before touching OCI)

```
scripts/build-local.sh      # builds the image in a privileged Docker container with mkosi
scripts/run-vm.sh           # boots it in QEMU
```

The Mac is arm64, so the local VM is an arm64 guest with hardware acceleration. That checks the layout, boot counting,
sysupdate flips, first-boot units and `noded`. It cannot check what is specific to `VM.Standard.E2.1.Micro`: x86, BIOS
or UEFI, launch mode, kernel modules on the Debian x86 kernel, real memory pressure. Those are OCI tests.
