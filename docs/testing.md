# Testing log

Nothing is verified yet. Each line moves from "untested" to a result, with where it was tested.

| Check | Where | Result |
| :--- | :--- | :--- |
| mkosi builds the image (arm64) | local Docker | **passes** (5.3 GB sparse raw, 377 MB on disk, kernel + initrd emitted). First attempt failed with "no kernel found" until `linux-image-arm64` was added per architecture. x86-64 not built locally. |
| Image boots, root is read-only, /var mounts and grows | local VM | untested |
| sysupdate stages root-b and flips; broken update falls back | local VM | untested |
| zram, sysctl, journald settings applied | local VM | untested |
| Image boots on VM.Standard.E2.1.Micro (launch mode, UEFI vs BIOS) | OCI | untested |
| Debian x86 kernel has the modules k3s/podflow/Tailscale need | OCI | untested |
| Idle RAM and k3s agent RAM | OCI | untested |
