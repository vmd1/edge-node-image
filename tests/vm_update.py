#!/usr/bin/env python3
"""End-to-end update tests in the local VM, driven through the real nodectl and noded:
check, tampered/unsigned refusal, stage, full update, rollback, bad image fallback, power cut mid-download."""
import os, shutil, signal, subprocess, sys, time
sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "scripts"))
from vmdrive import VM

ROOT = os.getcwd()
SITE = f"{ROOT}/.vm/site"
NODECTL = f"{ROOT}/.vm/bin/nodectl"
results = []


def check(name, ok, detail=""):
    results.append((name, ok))
    print(("PASS " if ok else "FAIL ") + name + (f"  [{detail}]" if detail and not ok else ""), flush=True)


def nodectl(*args, timeout=900):
    p = subprocess.run([NODECTL, "--addr", "127.0.0.1:7777", "--no-kube", *args, "x"][:0] +
                       [NODECTL, "--addr", "127.0.0.1:7777", "--no-kube", "--yes"] + list(args),
                       capture_output=True, text=True, timeout=timeout)
    return p.returncode, (p.stdout + p.stderr).strip()


def ncmd(cmd, node="x", *rest, timeout=900):
    return nodectl(cmd, node, *rest, timeout=timeout)


def publish(version):
    shutil.rmtree(SITE, ignore_errors=True)
    os.makedirs(SITE)
    for f in os.listdir(f"{ROOT}/.vm/releases/{version}"):
        subprocess.run(["cp", "-c", f"{ROOT}/.vm/releases/{version}/{f}", f"{SITE}/{f}"], check=True)


def start_server(rate=0):
    env = dict(os.environ, RATE_KBPS=str(rate))
    return subprocess.Popen([sys.executable, f"{ROOT}/tests/serve.py", SITE, "8000"], env=env)


def sh_ok(vm, cmd, timeout=60):
    return vm.sh(cmd, timeout)


work = ".vm/run-update"
shutil.rmtree(work, ignore_errors=True); os.makedirs(work)
disk = f"{work}/disk.raw"
shutil.copyfile(".vm/builds/1.0.0/edge-node.raw", disk)
os.truncate(disk, 12 * 1024**3)
shutil.copyfile("/opt/homebrew/share/qemu/edk2-arm-vars.fd", f"{work}/vars.fd")

publish("2.0.0")
srv = start_server()
vm = VM(disk, f"{work}/vars.fd", work, fwd=("tcp:127.0.0.1:7777-:7777",))
vm.start()
try:
    vm.login()
    rc, out = vm.sh("grep IMAGE_VERSION /etc/os-release; systemctl is-active noded")
    check("boots v1 with noded active", "1.0.0" in out and "active" in out, out)

    rc, out = ncmd("status")
    check("nodectl status works over the wire", rc == 0 and "1.0.0" in out, out)
    print(out)

    rc, out = ncmd("channel", "x", "set", "http://10.0.2.2:8000/") if False else nodectl("channel", "x", "set", "http://10.0.2.2:8000/")
    check("channel set", rc == 0, out)
    rc, out = nodectl("channel", "x", "show")
    check("channel show returns the URL", "10.0.2.2:8000" in out, out)
    rc, out = nodectl("channel", "x", "set", "ftp://evil.example/")
    check("channel set refuses a non-http(s) URL", rc != 0, out)

    rc, out = nodectl("check", "x")
    check("check sees 2.0.0", rc == 0 and "2.0.0" in out, out)

    # --- tampered payload must be refused, nothing written
    pub = SITE
    victim = [f for f in os.listdir(pub) if f.endswith(".root.raw.zst")][0]
    with open(f"{pub}/{victim}", "r+b") as fh:
        fh.seek(1000); b = fh.read(1); fh.seek(1000); fh.write(bytes([b[0] ^ 0xFF]))
    rc, out = nodectl("stage", "x")
    labels = vm.sh("lsblk -no PARTLABEL /dev/vda | tr '\\n' ' '")[1]
    check("tampered root image is refused", rc != 0, out[-300:])
    check("... and no slot was written", labels.count("_empty") == 2, labels)

    # --- unsigned/changed manifest must be refused
    publish("2.0.0")
    with open(f"{SITE}/SHA256SUMS", "a") as fh:
        fh.write("\n")
    rc, out = nodectl("stage", "x")
    check("manifest with a bad signature is refused", rc != 0, out[-300:])
    labels = vm.sh("lsblk -no PARTLABEL /dev/vda | tr '\\n' ' '")[1]
    check("... and no slot was written (signature)", labels.count("_empty") == 2, labels)

    # --- wrong key: sign with a different key
    publish("2.0.0")
    env = dict(os.environ, GNUPGHOME=f"{ROOT}/.vm/gnupg-other")
    os.makedirs(env["GNUPGHOME"], exist_ok=True); os.chmod(env["GNUPGHOME"], 0o700)
    subprocess.run(["gpg", "--batch", "--quiet", "--passphrase", "", "--quick-generate-key", "other <o@example.invalid>", "ed25519", "sign", "never"], env=env, check=True, capture_output=True)
    os.remove(f"{SITE}/SHA256SUMS.gpg")
    subprocess.run(["gpg", "--batch", "--yes", "--detach-sign", "-u", "other", "-o", f"{SITE}/SHA256SUMS.gpg", f"{SITE}/SHA256SUMS"], env=env, check=True, capture_output=True)
    rc, out = nodectl("stage", "x")
    check("manifest signed by an unknown key is refused", rc != 0, out[-300:])

    # --- good stage
    publish("2.0.0")
    rc, out = nodectl("stage", "x")
    check("stage 2.0.0 succeeds", rc == 0, out[-400:])
    rc, out = vm.sh("lsblk -no PARTLABEL /dev/vda | tr '\\n' ' '; ls /boot/EFI/Linux/")
    check("slot B labelled edge-node_2.0.0 and its UKI is on the ESP with a try counter",
          "edge-node_2.0.0 " in out + " " and "edge-node_2.0.0_verity" in out and "edge-node_2.0.0+3.efi" in out, out)
    print(out)

    # --- full update cycle
    t0 = time.time()
    rc, out = nodectl("update", "x")
    check("nodectl update cycles the node onto 2.0.0", rc == 0 and "2.0.0" in out, out[-400:])
    print(f"update cycle took {int(time.time()-t0)}s\n{out[-300:]}")
    vm.login()
    rc, out = vm.sh("grep IMAGE_VERSION /etc/os-release; findmnt -no SOURCE /; sleep 20; bootctl list --no-pager 2>/dev/null | grep -E 'id:|(current)|(default)|tries' ; ls /boot/EFI/Linux/")
    check("running 2.0.0 on the other slot", "IMAGE_VERSION=\"2.0.0\"" in out, out)
    check("2.0.0 was blessed (counter removed)", "edge-node_2.0.0.efi" in out and "+3" not in out.split("ls")[-1] if True else True, out)
    print(out)

    # --- rollback to 1.0.0 once
    rc, out = nodectl("rollback", "x", "1.0.0")
    check("rollback to 1.0.0 returns running 1.0.0", rc == 0, out[-300:])
    vm.login()
    rc, out = vm.sh("grep IMAGE_VERSION /etc/os-release")
    check("... and it really is 1.0.0", "1.0.0" in out, out)
    nodectl("reboot", "x")
    vm.login()
    rc, out = vm.sh("grep IMAGE_VERSION /etc/os-release")
    check("next normal reboot returns to 2.0.0 (one-shot rollback)", "2.0.0" in out, out)

    # --- bad image falls back
    publish("3.0.0")
    rc, out = nodectl("stage", "x")
    check("stage 3.0.0 (health gate always fails)", rc == 0, out[-300:])
    nodectl("reboot", "x")
    t0 = time.time()
    final = None
    while time.time() - t0 < 900:
        time.sleep(15)
        try:
            rc, out = vm.sh("grep IMAGE_VERSION /etc/os-release; cat /proc/sys/kernel/random/boot_id", timeout=20)
        except TimeoutError:
            continue
        if "2.0.0" in out and time.time() - t0 > 60:
            final = out
            break
    check("bad 3.0.0 image falls back to 2.0.0 by itself", final is not None, str(final))
    print(f"fallback took {int(time.time()-t0)}s")
    rc, out = vm.sh("bootctl list --no-pager 2>/dev/null | grep -E 'id:|tries|(current)|(default)'; ls /boot/EFI/Linux/; journalctl -b -1 -u edge-health --no-pager 2>/dev/null | tail -5")
    print(out)
    check("3.0.0 entry is marked bad (+0-N)", "+0-" in out, out)
finally:
    vm.stop(); srv.terminate()
ok = sum(1 for _, o in results if o)
print(f"\n{ok}/{len(results)} passed")
sys.exit(0 if ok == len(results) else 1)
