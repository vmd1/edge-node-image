# Testing log

Nothing is verified yet. Each line moves from "untested" to a result, with where it was tested.

| Check | Where | Result |
| :--- | :--- | :--- |
| mkosi builds the image (arm64) | local Docker | **passes** (5.3 GB sparse raw, 377 MB on disk, kernel + initrd emitted). First attempt failed with "no kernel found" until `linux-image-arm64` was added per architecture. x86-64 not built locally. |
| Image boots, root is read-only, /var mounts and grows | local VM | **boots, root ro, /var rw** (single-root test image). `/var` did **not** grow: 973 MB, needs a first-boot `systemd-repart` definition. |
| sysupdate stages root-b and flips; broken update falls back | local VM | untested. **With root-a and root-b both present the initrd fails to mount the root** (emergency shell): it cannot tell which slot to use. With root-b removed the image boots. Fix needed: dm-verity (roothash selects the slot) or per-slot boot entries. |
| zram, sysctl, journald settings applied | local VM | zram 481 MB at priority 100, all five sysctls applied, `eth0` (ifnames=0 works). Journald caps not checked yet. `systemd-resolved` fails to start (cause unknown). No listening sockets. Idle memory 118 MB used of 963 MB (arm64 VM, no k3s yet). |
| Image boots on VM.Standard.E2.1.Micro (launch mode, UEFI vs BIOS) | OCI | untested |
| Debian x86 kernel has the modules k3s/podflow/Tailscale need | OCI | untested |
| Idle RAM and k3s agent RAM | OCI | untested |
