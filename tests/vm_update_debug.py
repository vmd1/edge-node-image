#!/usr/bin/env python3
"""Reproduce one update cycle with every log kept: stage 2.0.0, reboot, and print what each boot did."""
import os, re, shutil, subprocess, sys, time
sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "scripts"))
from vmdrive import VM

ROOT = os.getcwd(); SITE = f"{ROOT}/.vm/site"; NODECTL = f"{ROOT}/.vm/bin/nodectl"
ver = sys.argv[1] if len(sys.argv) > 1 else "2.0.0"
shutil.rmtree(SITE, ignore_errors=True); os.makedirs(SITE)
for f in os.listdir(f"{ROOT}/.vm/releases/{ver}"):
    subprocess.run(["cp", "-c", f"{ROOT}/.vm/releases/{ver}/{f}", f"{SITE}/{f}"], check=True)
srv = subprocess.Popen([sys.executable, f"{ROOT}/tests/serve.py", SITE, "8000"])
work = ".vm/run-debug"; shutil.rmtree(work, ignore_errors=True); os.makedirs(work)
shutil.copyfile(".vm/builds/1.0.0/edge-node.raw", f"{work}/disk.raw"); os.truncate(f"{work}/disk.raw", 12 * 1024**3)
shutil.copyfile("/opt/homebrew/share/qemu/edk2-arm-vars.fd", f"{work}/vars.fd")
vm = VM(f"{work}/disk.raw", f"{work}/vars.fd", work, fwd=("tcp:127.0.0.1:7777-:7777",))

def nc(*a, timeout=600):
    p = subprocess.run([NODECTL, "--addr", "127.0.0.1:7777", "--no-kube", "--yes", *a], capture_output=True, text=True, timeout=timeout)
    return p.returncode, (p.stdout + p.stderr).strip()

vm.start()
try:
    vm.login()
    print(nc("channel", "x", "set", "http://10.0.2.2:8000/"))
    print(nc("stage", "x")[1][-500:])
    print(vm.sh("udevadm settle; lsblk -no NAME,PARTLABEL /dev/vda; ls /boot/EFI/Linux/")[1])
    print("--- rebooting through nodectl")
    print(nc("reboot", "x"))
    vm.login(300)
    print(vm.sh("grep IMAGE_VERSION /etc/os-release; cat /proc/cmdline | cut -c1-120; bootctl list --no-pager 2>/dev/null | grep -E 'id:|tries|current|default'", 60)[1])
    time.sleep(90)  # let the health gate finish
    print(vm.sh("journalctl --list-boots --no-pager; journalctl -b --no-pager -q -u edge-health -u systemd-bless-boot | tail -12; bootctl list --no-pager 2>/dev/null | grep -E 'id:|tries'", 60)[1])
finally:
    vm.stop(); srv.terminate()
log = open(f"{work}/console.log", errors="replace").read()
log = re.sub(r"\x1b\[[0-9;?]*[A-Za-z]", "", log)
print("\n=== console highlights")
for l in log.splitlines():
    if re.search(r"health:|FAILED|emergency|Reached target Multi-User|Linux version|Command line|dm-verity|verity|root hash|No root|Timed out|Kernel panic", l):
        print(l[:200])
