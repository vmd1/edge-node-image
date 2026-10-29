#!/usr/bin/env python3
"""A staged slot whose verity data is damaged: the initrd cannot mount the root, must reboot (not sit in emergency mode), the
boot counter runs down, and the bootloader falls back to the previous version by itself. Also: power cut during staging."""
import os, shutil, subprocess, sys, time
sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "scripts"))
from vmdrive import VM

ROOT = os.getcwd(); SITE = f"{ROOT}/.vm/site"; NODECTL = f"{ROOT}/.vm/bin/nodectl"
results = []
def check(name, ok, detail=""):
    results.append((name, ok)); print(("PASS " if ok else "FAIL ") + name + (f"  [{detail}]" if detail and not ok else ""), flush=True)
def nc(*a, timeout=300):
    p = subprocess.run([NODECTL, "--addr", "127.0.0.1:7777", "--no-kube", "--yes", *a], capture_output=True, text=True, timeout=timeout)
    return p.returncode, (p.stdout + p.stderr).strip()
def publish(v):
    shutil.rmtree(SITE, ignore_errors=True); os.makedirs(SITE)
    for f in os.listdir(f"{ROOT}/.vm/releases/{v}"):
        subprocess.run(["cp", "-c", f"{ROOT}/.vm/releases/{v}/{f}", f"{SITE}/{f}"], check=True)
def serve(rate=0):
    return subprocess.Popen([sys.executable, f"{ROOT}/tests/serve.py", SITE, "8000"], env=dict(os.environ, RATE_KBPS=str(rate)))

work = ".vm/run-damaged"; shutil.rmtree(work, ignore_errors=True); os.makedirs(work)
disk = f"{work}/disk.raw"
shutil.copyfile(".vm/builds/1.0.0/edge-node.raw", disk); os.truncate(disk, 12 * 1024**3)
shutil.copyfile("/opt/homebrew/share/qemu/edk2-arm-vars.fd", f"{work}/vars.fd")
publish("2.0.0"); srv = serve()
vm = VM(disk, f"{work}/vars.fd", work, fwd=("tcp:127.0.0.1:7777-:7777",))
vm.start()
try:
    vm.login()
    nc("channel", "x", "set", "http://10.0.2.2:8000/")
    rc, out = nc("stage", "x")
    check("stage 2.0.0", rc == 0, out[-300:])
    rc, out = vm.sh("udevadm settle; dev=$(readlink -f /dev/disk/by-partlabel/edge-node_2.0.0_verity); echo $dev; dd if=/dev/urandom of=$dev bs=1M count=4 conv=notrunc 2>&1 | tail -1; sync")
    print("damaged:", out)
    rc, bid0 = vm.sh("cat /proc/sys/kernel/random/boot_id")
    t0 = time.time()
    vm.send("systemctl reboot\n")
    time.sleep(8)
    back = None; bid1 = None
    while time.time() - t0 < 420:
        time.sleep(10)
        try:
            rc, out = vm.sh("cat /proc/sys/kernel/random/boot_id; grep IMAGE_VERSION /etc/os-release", timeout=15)
        except TimeoutError:
            continue
        if bid0 not in out and "IMAGE_VERSION" in out:
            back = out; break
    print(f"back after {int(time.time()-t0)}s: {back}")
    check("damaged slot: node ends up on the previous version by itself", back is not None and "1.0.0" in back, str(back))
    rc, out = vm.sh("bootctl list --no-pager 2>/dev/null | grep -E 'id:|tries'; ls /boot/EFI/Linux/")
    print(out)
    check("the damaged entry's tries ran out (marked bad)", "tries: 0 left" in out, out)

    # power cut during staging: kill QEMU while a throttled download is in progress
    publish("2.0.0"); srv.terminate(); srv = serve(rate=4000)
    vm.sh("echo hi", timeout=20)
    p = subprocess.Popen([NODECTL, "--addr", "127.0.0.1:7777", "--no-kube", "--yes", "stage", "x"], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    time.sleep(25); vm.proc.kill(); vm.proc.wait(); p.kill()
    vm2 = VM(disk, f"{work}/vars.fd", work, fwd=("tcp:127.0.0.1:7777-:7777",)); vm2.start(); vm = vm2
    vm.login(300)
    rc, out = vm.sh("grep IMAGE_VERSION /etc/os-release; ls /boot/EFI/Linux/")
    check("after a power cut mid-staging the node still boots the running version", "1.0.0" in out, out)
    srv.terminate(); srv = serve()
    rc, out = nc("stage", "x")
    check("staging again after the power cut succeeds", rc == 0, out[-300:])
    rc, out = vm.sh("udevadm settle; lsblk -no PARTLABEL /dev/vda | tr '\\n' ' '; ls /boot/EFI/Linux/")
    print(out)
finally:
    vm.stop(); srv.terminate()
ok = sum(1 for _, o in results if o); print(f"\n{ok}/{len(results)} passed"); sys.exit(0 if ok == len(results) else 1)
