# edge-node-image

A read-only Debian 13 image for the six 1 GB OCI edge nodes of the `vmd1` k3s cluster, updated in place with
`systemd-sysupdate` (A/B root slots), with a small gRPC agent (`noded`) and its client (`nodectl`) instead of SSH.

**Status (2026-11-10): in production.** All six ingress nodes run this image: 0.2.0 since 2026-10-29/30, and
**0.3.0-rc1 from the `candidate-amd64` channel since 2026-11-10** (declarative node config, introspection, rolling
updates with health gates, host firewall, roles). 0.3.0 is not yet promoted to `stable-amd64`, which still carries
the 2026-10-29 release. Only amd64 is built: no node uses arm64 and the image has never been booted on arm64 hardware.
`apiRoles` is `enforce` fleet-wide (set through the cluster's `EdgeNodeConfig` objects).
`docs/testing.md` is the original test log from the first arm64 VM session; see the note at its top for what has
changed since.

The plan, decisions and roadmap live in the homelab repo: `docs/edge-node-image.md` and `docs/edge-node-roadmap.md`.
`nodectl` is also built into the vmd1 CLI as `vmd1 node`.

## Layout

| Path | What |
| :--- | :--- |
| `mkosi.conf`, `mkosi.conf.d/`, `mkosi.profiles/`, `mkosi.images/` | Image definition (Debian trixie, no SSH, no cloud-init) and its profiles (for example `debug` for VM tests). |
| `mkosi.repart/` | Partition layout: ESP, root A, root B (with dm-verity), `/var`. |
| `mkosi.extra/`, `mkosi.finalize` | Files copied into the image (sysctl, zram, journald, fstab, sysupdate transfers) and the kernel-module trimming. |
| `cmd/noded`, `cmd/nodectl`, `internal/` | The on-node agent (`auth`, `config`, `firewall`, `inspect`, `server`); `cmd/nodectl` is a thin wrapper. |
| `nodectl/` | The `nodectl` client as an importable package (also used by the vmd1 CLI). |
| `proto/`, `gen/` | The gRPC API and its generated code. |
| `signing/` | The public release key baked into the image. |
| `scripts/` | Local build (Docker), VM runners (QEMU on the Mac, KVM on a remote host), release and SBOM scripts, image checks. |
| `tests/` | VM tests (arm64 and x86): boot, update, damaged slot, join, tailscale, oomd, hardening, full x86 run. |
| `.github/workflows/` | `ci.yml` (tests, shellcheck), `release.yml` (tag `vX.Y.Z`, builds and signs to the candidate channel), `promote.yml` (candidate to stable). |
| `docs/testing.md` | The test log, with what has been verified, where, and what had not been when it was written. |

## Releases

Nodes follow a rolling GitHub release: `stable-amd64`, or `candidate-amd64` for release candidates. Files are the UKI,
the xz-compressed root and verity partitions, `SHA256SUMS` and its detached signature; nodes accept only manifests
signed by the baked release key. Update one node at a time with `nodectl --yes update <node>` or the whole fleet with
`nodectl rollout`.

## Local loop (before touching OCI)

```
scripts/build-local.sh        # builds the image in a privileged Docker container with mkosi
scripts/run-vm.sh             # boots it in QEMU (arm64 guest with hardware acceleration on an Apple Silicon Mac)
scripts/run-vm-kvm-remote.sh  # runs the x86 VM tests on a KVM host
```

The arm64 VM on the Mac checks the layout, boot counting, sysupdate flips, first-boot units and `noded`. What is
specific to `VM.Standard.E2.1.Micro` (x86, launch mode, real memory pressure) is checked with the x86 KVM tests and on
the nodes themselves.
