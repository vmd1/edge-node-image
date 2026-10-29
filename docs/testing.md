# Testing log

Nothing is verified yet. Each line moves from "untested" to a result, with where it was tested.

| Check | Where | Result |
| :--- | :--- | :--- |
| mkosi builds the image (arm64) | local Docker | **passes** (now with verity: `Verity=` in the repart files, no `Verity=` in mkosi.conf, since 25.3 treats yes as signed; verity hash partitions need 256 MB at 512-byte sectors; `systemd-boot-efi` is needed for the UKI stub) (5.3 GB sparse raw, 377 MB on disk, kernel + initrd emitted). First attempt failed with "no kernel found" until `linux-image-arm64` was added per architecture. x86-64 not built locally. |
| Image boots, root is read-only, /var mounts and grows | local VM | **boots with dm-verity and A/B layout present**: root is `/dev/mapper/root` (ext4, ro), roothash on the kernel command line, root-b and root-verity-b present and empty, /var rw. **/var still does not grow** (973 MB on an 8 GB disk): the first-boot repart definition has no effect yet. |
| sysupdate stages root-b and flips; broken update falls back | local VM | untested. The slot-selection problem is solved by verity (initrd boots with both slots present). Needs sysupdate transfer files and a v2 image. |
| zram, sysctl, journald settings applied | local VM | zram 481 MB (prio 100), five sysctls, `eth0`, static resolv.conf (1.1.1.1, 8.8.8.8), DHCPv4 address. **No systemd-resolved; nothing listens on port 53.** No failed units. Journald caps not checked yet. Idle memory 138 MB used of 963 MB (arm64 VM with verity, no k3s). |
| Image boots on VM.Standard.E2.1.Micro (launch mode, UEFI vs BIOS) | OCI | untested |
| Debian x86 kernel has the modules k3s/podflow/Tailscale need | OCI | untested |
| Idle RAM and k3s agent RAM | OCI | untested |
