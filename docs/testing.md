# Testing log

Last updated at the end of the first long session. **Local VM = arm64 QEMU with HVF on an Apple Silicon Mac; nothing here has
run on OCI or on x86.** "Real run" means the output was observed; "not run" means exactly that.

## Passed (real runs)

| Check | Where | Notes |
| :--- | :--- | :--- |
| `go vet`, `go test` for auth, server, wire (in-memory gRPC), nodectl, with and without `-tags debugauth` | host | Auth fails closed (empty allowlist, lookup error, no peer); input validation; no exec RPC in the API; release `noded` contains no allow-all code. |
| mkosi builds the image (arm64) | Docker | Needed: `SectorSize=512` (FAT otherwise unmountable by edk2), ESP `CopyFiles`, `systemd-boot-efi` for the UKI stub, `Verity=` only in repart files (mkosi 25.3 wants a key for `Verity=yes`), 256 MB verity partitions. |
| Boot: UEFI → systemd-boot → UKI → initrd → dm-verity root (ext4, ro) | VM | A/B layout present (root-a, root-b, both verity partitions). |
| `/var` grows to the disk | VM | 1 GB → 6.5 GB on a 12 GB disk (`edge-grow-var`, needs `systemd-repart` in the image). |
| Partition labels `edge-node_<ver>`, `edge-node_<ver>_verity`, `_empty`; UKI `edge-node_<ver>.efi` | VM | `Label=%M_%A`, `UnifiedKernelImageFormat=%i_%v`. |
| zram 481 MB, sysctls, `eth0`, static resolv.conf, nothing on port 53, journald caps | VM | systemd-resolved removed on purpose. |
| `nodectl` ⇄ `noded` over real gRPC against the booted image: status, channel set/show/refuse non-https, check | VM | debug build (`-tags debugauth`), hostfwd 7777. |
| `sysupdate` refuses a **tampered** payload, a **modified** manifest and a manifest signed by an **unknown key**; nothing is written to the inactive slot | VM | Only meaningful since `gpg` was added to the image: `systemd-pull` runs `gpg`, not `gpgv`. The first run "passed" these for the wrong reason (every signature failed). |
| `edge-sysupdate update` installs verity + root + UKI into slot B; labels and `edge-node_2.0.0+3-0.efi` correct | VM | Observed directly, with signature verified. |
| One-shot rollback to the previous version, and the next reboot returns to the default | VM | via `nodectl rollback`. |

## Failed or unexplained (open, in the order to fix)

1. **`nodectl update` to 2.0.0 ended on 1.0.0 after 185 s.** The node rebooted and came back on the old slot. Cause not found: either
   2.0.0 failed its health gate and the bootloader fell back (the designed behaviour, but it should not have failed), or it never
   booted. The journal does not persist across the test's hard stops, so there is no record. Reproduce with the console log kept
   (`.vm/run-update/console.log`) and `journalctl -b -1`.
2. **`systemd-oomd` fails to start** in the early boots of that run (four `[FAILED]` lines) although the basic boot test showed no failed
   units. Cause unknown (cgroup/PSI?). The memory-pressure killer is therefore **unverified**.
3. **Debian's default presets started `systemd-sysupdate.timer` and `systemd-sysupdate-reboot.timer`**, i.e. nodes would update *and
   reboot themselves* on a timer, against the plan (staging and flipping are by `nodectl` only). The units are now masked in
   `mkosi.extra/etc/systemd/system/` (**untested**: needs a rebuild and a look at `systemctl list-timers`).
4. The bad-image fallback test (3.0.0, always-failing health gate) and the power-cut-mid-download test did **not run**: the session ended
   while the first was waiting.
5. Test harness fixes made but not re-run: the slot-label check needs `udevadm settle` (udev reports stale labels right after sysupdate
   writes them), and the try-counter name is `+3-0`.

## Not run yet

- Health gate end to end (boot counting, `+3-0` → blessed, bad image falls back after 3 tries), including the probation reboot.
- Power loss during staging.
- First-boot provisioning (`edge-firstboot`, `edge-render-k3s`, `edge-external-ip` against ip.me, `edge-node-annotate`), k3s agent join against a
  throwaway k3s server, Tailscale join, `noded` authorization through real `tailscaled` WhoIs.
- Pre-seeded images (only the k3s airgap tarball is baked in; app images are not).
- oomd killing a hog under memory pressure; memory use with k3s running.
- x86-64 build and boot (UEFI and BIOS); CI workflows (never executed; `actionlint` not run).
- Anything on OCI: boot of a Debian image on `VM.Standard.E2.1.Micro`, launch mode, UEFI vs BIOS, kernel modules, real RAM numbers,
  replace-boot-volume behaviour.

## Facts learned that change the plan

- `systemd-sysupdate` is in `systemd-container` on Debian 13 (no separate package). It verifies with **`gpg`** and trusts
  `/usr/lib/systemd/import-pubring.gpg`, which Debian fills with ~45 KB of distribution keys; the image replaces it with its own key.
- Each release is ~665 MB to download (UKI 128 MB because the initrd carries all kernel modules, root 487 MB incl. k3s airgap images).
  Worth shrinking before OCI (module list, airgap tarball).
- `noded` runs as root with systemd sandboxing, not as an unprivileged user with polkit: `bootctl`, `sysupdate` and `reboot` need real
  privileges that polkit cannot grant (the plan's open question 8 is answered).
- `edge-health` reboots on failure **only while the boot is on probation** (a `+N` try counter exists), so a tailnet outage on a blessed
  boot cannot cause a reboot loop.
