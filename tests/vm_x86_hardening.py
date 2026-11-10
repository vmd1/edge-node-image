#!/usr/bin/env python3
"""Phase 1.A checks on a booted x86-64 image (KVM on another machine, serial through a forwarded TCP port 4555)."""
import os, sys, time
sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "scripts"))
from vmdrive import TcpSerial

results = []
def check(name, ok, detail=""):
    results.append((name, ok)); print(("PASS " if ok else "FAIL ") + name + (f"  [{detail}]" if detail and not ok else ""), flush=True)

vm = TcpSerial("127.0.0.1", 4555, logfile=".vm/hardening-console.log"); vm.start()
try:
    vm.login(300)
    time.sleep(25)
    def sh(c, t=60): return vm.sh(c, timeout=t)[1]

    out = sh("systemctl --failed --no-legend | cut -c1-120; echo END")
    check("no failed units", out.strip().splitlines()[0].strip() == "END", out)
    cmd = sh("cat /proc/cmdline")
    for flag in ["slab_nomerge", "init_on_alloc=1", "pti=on", "module.sig_enforce=1", "lockdown=integrity"]:
        check(f"kernel command line has {flag}", flag in cmd, cmd[-200:])
    lock = sh("cat /sys/kernel/security/lockdown 2>&1")
    check("kernel lockdown is integrity", "[integrity]" in lock, lock)
    for k, v in [("kernel.kptr_restrict", "2"), ("kernel.kexec_load_disabled", "1"), ("kernel.yama.ptrace_scope", "1"),
                 ("net.core.bpf_jit_harden", "2"), ("kernel.dmesg_restrict", "1"), ("kernel.unprivileged_bpf_disabled", "2")]:
        got = sh(f"sysctl -n {k}").strip().splitlines()[-1]
        check(f"sysctl {k} = {v}", got == v, got)
    out = sh("for b in bash rbash perl dpkg apt apt-get su mount; do command -v $b || true; done; echo END")
    check("bash, perl, dpkg, apt are gone", out.strip().splitlines()[0].strip() == "END" or all(x not in out for x in ["/bash", "/perl", "/dpkg", "/apt"]), out)
    out = sh("find / -xdev \\( -perm -4000 -o -perm -2000 \\) -type f 2>/dev/null | head -5; echo END", 120)
    check("no setuid or setgid files", out.strip().splitlines()[0].strip() == "END", out)
    out = sh("ls /dev/mem /dev/kmem /dev/port 2>&1 | head -3")
    check("/dev/mem, /dev/kmem, /dev/port are not present", "No such file" in out and "/dev/mem" in out and "cannot access '/dev/mem'" in out, out)
    out = sh("systemctl show k3s-agent -p CapabilityBoundingSet --value")
    print("k3s-agent CapabilityBoundingSet:", out.strip()[:200])
    check("k3s-agent cannot load modules or reboot", "cap_sys_module" not in out and "cap_sys_boot" not in out and len(out.strip()) > 20, out)
    out = sh("grep -c . /usr/share/edge/packages.txt; ls /usr/lib/modules-load.d/edge.conf; systemctl is-enabled edge-lock-modules")
    check("package record and module preload list exist", "edge.conf" in out and "enabled" in out, out)
    out = sh("cat /proc/sys/kernel/modules_disabled; systemctl is-active edge-lock-modules; systemctl status edge-lock-modules --no-pager 2>&1 | tail -4 | cut -c1-140")
    print("module lock state before health:", out.strip())
    out = sh("for s in edge-health edge-external-ip; do systemd-analyze security $s.service --no-pager 2>/dev/null | tail -1; done")
    print("sandbox scores:", out.strip())
    out = sh("free -m | sed -n 2p; ps -eo rss,comm --sort=-rss | head -5")
    print(out)
finally:
    vm.stop()
ok = sum(1 for _, o in results if o); print(f"\n{ok}/{len(results)} passed")
sys.exit(0 if ok == len(results) else 1)
