#!/usr/bin/env python3
"""Does systemd-oomd resolve memory pressure quickly? Same memory hog twice in a 1 GB VM (zram swap on, like the nodes):
once with oomd stopped (the kernel OOM killer / thrash), once with oomd watching the hog's slice. Reports time to kill and the
worst shell round-trip during the episode. Needs no ports."""
import os, re, shutil, sys, time
sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "scripts"))
from vmdrive import VM

results = []
def check(name, ok, detail=""):
    results.append((name, ok)); print(("PASS " if ok else "FAIL ") + name + (f"  [{detail}]" if detail and not ok else ""), flush=True)

work = ".vm/run-oomd"; shutil.rmtree(work, ignore_errors=True); os.makedirs(work)
shutil.copyfile(".vm/builds/1.0.0/edge-node.raw", f"{work}/disk.raw"); os.truncate(f"{work}/disk.raw", 12 * 1024**3)
shutil.copyfile("/opt/homebrew/share/qemu/edk2-arm-vars.fd", f"{work}/vars.fd")
import subprocess
srv = subprocess.Popen([sys.executable, "-m", "http.server", "8001", "--directory", ".vm/hogsrv", "--bind", "127.0.0.1"], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
vm = VM(f"{work}/disk.raw", f"{work}/vars.fd", work, mem=1024)
vm.start()

def probe():
    s = time.time()
    rc, out = vm.sh("awk '/^some/{print $2}' /proc/pressure/memory | cut -d= -f2; awk '/MemAvailable/{print int($2/1024)}' /proc/meminfo; systemctl is-active hog.service", timeout=170)
    f = out.split()
    return time.time() - s, (f[0] if f else "?"), (f[1] if len(f) > 1 else "?"), (f[-1] if f else "?")

def episode(label, mb, limit=150):
    """start the hog, poll until it is gone; return (seconds to die or None, worst probe latency)"""
    vm.sh("systemctl reset-failed hog.service 2>/dev/null; systemd-run --quiet --unit=hog --slice=hog.slice /var/tmp/hog %d" % mb, timeout=30)
    t0 = time.time(); worst = 0.0; died = None; nxt = 0
    while time.time() - t0 < limit:
        try:
            lat, psi, avail, st = probe()
        except TimeoutError:
            worst = max(worst, 170); print(f"  {label} t={time.time()-t0:5.0f}s probe timed out (shell unresponsive)"); continue
        worst = max(worst, lat)
        if time.time() - t0 >= nxt:
            print(f"  {label} t={time.time()-t0:5.0f}s round-trip={lat:5.1f}s mem-pressure(some,avg10)={psi}% available={avail}MiB hog={st}"); nxt += 10
        if st != "active" and st != "activating":
            died = time.time() - t0; break
        time.sleep(1)
    vm.sh("systemctl kill hog.service 2>/dev/null; sleep 3; systemctl reset-failed hog.service 2>/dev/null; true", timeout=170)
    print(f"{label}: hog gone after {died if died is None else round(died,1)}s, worst shell round-trip {worst:.1f}s")
    return died, worst

try:
    vm.login()
    rc, out = vm.sh("systemctl is-active systemd-oomd; swapon --show --noheadings | awk '{print $1,$3}'; free -m | sed -n 2p")
    print(out)
    check("systemd-oomd is active at boot", out.splitlines()[0].strip() == "active", out)
    vm.sh("printf '[Slice]\\nManagedOOMMemoryPressure=kill\\nManagedOOMMemoryPressureLimit=20%%\\n' > /run/systemd/system/hog.slice && systemctl daemon-reload")

    vm.sh("curl -s -o /var/tmp/hog http://10.0.2.2:8001/hog && chmod +x /var/tmp/hog && ls -la /var/tmp/hog | cut -c1-40")
    MB = int(os.environ.get("HOG_MB", "780"))
    vm.sh("systemctl stop systemd-oomd systemd-oomd.socket", timeout=30)
    d0, w0 = episode("WITHOUT oomd", MB)
    rc, out = vm.sh("dmesg | grep -c -i 'out of memory'; free -m | sed -n 2p")
    print("kernel OOM events:", out.replace("\n", " | "))

    vm.sh("systemctl start systemd-oomd", timeout=30)
    time.sleep(3)
    d1, w1 = episode("WITH oomd", MB)
    rc, out = vm.sh("journalctl -u systemd-oomd --no-pager | grep -E 'Killed|Memory pressure' | tail -3 | cut -c1-220")
    print("oomd journal:", out)
    check("oomd itself killed the memory hog (journal says Killed)", d1 is not None and "Killed" in out, out)
    check("oomd resolved it (hog gone) within 90 s", d1 is not None and d1 < 90, str(d1))
    check("the shell stayed responsive during the episode (worst round-trip < 20 s)", w1 < 20, f"{w1:.1f}")
    print(f"SUMMARY without oomd: gone={d0} worst_latency={w0:.1f}s | with oomd: gone={d1} worst_latency={w1:.1f}s")
    rc, out = vm.sh("systemctl is-active systemd-oomd; free -m | sed -n 2p")
    check("system is healthy afterwards", out.startswith("active"), out)
finally:
    vm.stop(); srv.terminate(); shutil.rmtree(work, ignore_errors=True)
ok = sum(1 for _, o in results if o); print(f"\n{ok}/{len(results)} passed"); sys.exit(0 if ok == len(results) else 1)
