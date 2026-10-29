# Testing log

Nothing is verified yet. Each line moves from "untested" to a result, with where it was tested.

| Check | Where | Result |
| :--- | :--- | :--- |
| mkosi builds the image (arm64) | local Docker | untested |
| Image boots, root is read-only, /var mounts and grows | local VM | untested |
| sysupdate stages root-b and flips; broken update falls back | local VM | untested |
| zram, sysctl, journald settings applied | local VM | untested |
| Image boots on VM.Standard.E2.1.Micro (launch mode, UEFI vs BIOS) | OCI | untested |
| Debian x86 kernel has the modules k3s/podflow/Tailscale need | OCI | untested |
| Idle RAM and k3s agent RAM | OCI | untested |
