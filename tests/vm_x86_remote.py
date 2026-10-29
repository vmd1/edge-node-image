#!/usr/bin/env python3
"""Inspect the x86-64 trial image running under KVM on another machine, through a forwarded TCP serial port."""
import os, sys, time
sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "scripts"))
from vmdrive import TcpSerial

vm = TcpSerial("127.0.0.1", 4555)
vm.start()
try:
    vm.login(240)
    time.sleep(20)  # let the boot finish settling
    for title, cmd in [
        ("kernel / arch / image", "uname -srm; grep -E '^(IMAGE_ID|IMAGE_VERSION)=' /etc/os-release; cat /proc/cmdline | cut -c1-140"),
        ("root and var", "findmnt -no SOURCE,FSTYPE,OPTIONS /; findmnt -no SOURCE,SIZE,FSTYPE /var; lsblk -o NAME,SIZE,PARTLABEL,FSTYPE,MOUNTPOINT"),
        ("write test", "touch /etc/x 2>&1 | head -1; touch /var/x && echo 'var writable' && rm /var/x"),
        ("memory", "free -m; echo; ps -eo rss,comm --sort=-rss | head -6"),
        ("swap and sysctl", "swapon --show; sysctl vm.swappiness vm.min_free_kbytes vm.vfs_cache_pressure fs.inotify.max_user_instances"),
        ("listeners (must be none open to the world)", "ss -tulpn | cut -c1-110"),
        ("units", "systemctl --failed --no-legend; echo ---; systemctl is-active tailscaled noded systemd-oomd systemd-networkd edge-health k3s-agent edge-firstboot 2>&1 | paste -sd' '"),
        ("network", "ip -br addr; cat /etc/resolv.conf; curl -4 -s --max-time 8 https://ip.me; echo"),
        ("boot errors", "dmesg --level=err,warn | grep -v -E 'regulatory|ACPI BIOS|pnp' | head -12 | cut -c1-150"),
        ("boot entry / counter", "bootctl status --no-pager 2>/dev/null | grep -E 'Current Entry|Firmware|Boot into|Secure' | head -5; ls /boot/EFI/Linux/"),
        ("selfcheck + health", "journalctl -u edge-health -u edge-selfcheck --no-pager -n 14 | cut -c1-150"),
    ]:
        rc, out = vm.sh(cmd, timeout=90)
        print(f"=== {title} (rc={rc})\n{out}\n")
finally:
    vm.stop()
