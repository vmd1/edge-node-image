#!/usr/bin/env python3
"""Boot an image in the local VM and check the basics: /var growth, labels, UKI name, os-release, sysupdate, journald."""
import os, shutil, sys, time
sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "scripts"))
from vmdrive import VM

img = sys.argv[1]
work = ".vm/run-basic"
shutil.rmtree(work, ignore_errors=True); os.makedirs(work)
disk = f"{work}/disk.raw"
shutil.copyfile(img, disk)
os.truncate(disk, 12 * 1024**3)
shutil.copyfile("/opt/homebrew/share/qemu/edk2-arm-vars.fd", f"{work}/vars.fd")
vm = VM(disk, f"{work}/vars.fd", work)
vm.start()
try:
    vm.login()
    for title, cmd in [
        ("os-release", "grep -E '^(IMAGE_ID|IMAGE_VERSION|ID)=' /etc/os-release"),
        ("partitions", "lsblk -o NAME,SIZE,PARTLABEL,FSTYPE,MOUNTPOINT"),
        ("var size", "df -h /var | tail -1; systemctl status edge-grow-var --no-pager | head -12"),
        ("ESP", "ls -la /boot/EFI/Linux/; ls /boot/loader/entries/ 2>&1"),
        ("bootctl", "bootctl status --no-pager 2>&1 | head -30"),
        ("sysupdate list", "/usr/libexec/edge-sysupdate --no-pager list 2>&1 | head -20"),
        ("journald", "journalctl --disk-usage; ls /var/log/journal | head -3; cat /etc/systemd/journald.conf.d/10-edge.conf | head -4"),
        ("failed units", "systemctl --failed --no-legend"),
        ("trust keyring", "ls -la /usr/lib/systemd/import-pubring.* ; gpg --version >/dev/null 2>&1; gpgv --version | head -1"),
    ]:
        rc, out = vm.sh(cmd, timeout=90)
        print(f"=== {title} (rc={rc})\n{out}\n")
finally:
    vm.stop()
