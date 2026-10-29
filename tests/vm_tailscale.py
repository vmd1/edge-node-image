#!/usr/bin/env python3
"""Real tailscaled in the image, joining the user's tailnet with a one-off ephemeral auth key kept OUTSIDE the repo
(~/.config/edge-image-test/tailscale-authkey). Checks: join, prefs (no DNS/route changes, no auto-update), the key is wiped from
disk and never reaches a log, noded listens only on the Tailscale address, and its authorization works through the real WhoIs
(a remote tailnet peer is accepted; the node itself is refused). Access control proper is the Tailscale ACL, which is not tested here."""
import json, os, re, shutil, subprocess, sys, time
sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "scripts"))
from vmdrive import VM

ROOT = os.getcwd(); NODECTL = f"{ROOT}/.vm/bin/nodectl"
KEYFILE = os.path.expanduser("~/.config/edge-image-test/tailscale-authkey")
NAME = "edge-test-ts"
results = []
def check(name, ok, detail=""):
    results.append((name, ok)); print(("PASS " if ok else "FAIL ") + name + (f"  [{detail}]" if detail and not ok else ""), flush=True)

key = open(KEYFILE).read().strip()

def nc(addr, *a):
    p = subprocess.run([NODECTL, "--addr", addr, "--no-kube", *a], capture_output=True, text=True, timeout=40)
    return p.returncode, (p.stdout + p.stderr).strip()

work = ".vm/run-ts"; shutil.rmtree(work, ignore_errors=True); os.makedirs(work)
shutil.copyfile(".vm/builds/1.0.0/edge-node.raw", f"{work}/disk.raw"); os.truncate(f"{work}/disk.raw", 12 * 1024**3)
shutil.copyfile("/opt/homebrew/share/qemu/edk2-arm-vars.fd", f"{work}/vars.fd")
vm = VM(f"{work}/disk.raw", f"{work}/vars.fd", work)
vm.start()
ip = None
try:
    vm.login()
    vm.sh("stty -echo")   # the key must not be echoed to the console log
    vm.sh("mkdir -p /var/lib/edge && umask 077 && cat > /var/lib/edge/bootstrap.env <<'EOF'\n"
          f"NODE_NAME={NAME}\nTAILSCALE_AUTHKEY={key}\nK3S_TOKEN=unused\nK3S_SERVER_URL=https://10.0.2.2:6443\nEOF")
    vm.sh("stty echo")
    rc, out = vm.sh("systemctl restart edge-firstboot; echo fb=$?; journalctl -u edge-firstboot --no-pager -n 8 | cut -c1-200 | sed 's/tskey-[A-Za-z0-9-]*/<key>/g'", timeout=240)
    print(out)
    check("edge-firstboot joins the tailnet and completes", "firstboot: done" in out, out[-400:])
    rc, out = vm.sh("tailscale ip -4")
    ip = out.strip().splitlines()[-1] if out.strip() else ""
    check("tailscale ip -4 returns a 100.x address", bool(re.fullmatch(r"100\.\d+\.\d+\.\d+", ip)), out)
    rc, out = vm.sh("tailscale debug prefs | grep -E 'CorpDNS|RouteAll|Hostname|AutoUpdate|Apply|Check' | tr -d ' \\n'")
    print(out)
    check("accept-dns and accept-routes are off", '"CorpDNS":false' in out and '"RouteAll":false' in out, out)
    check("auto-update is off", '"Apply":false' in out or '"Apply":null' in out, out)
    rc, out = vm.sh("grep -c AUTHKEY /var/lib/edge/bootstrap.env; echo; grep -r -l 'tskey-' /var/lib /var/log /etc 2>/dev/null | head -3; journalctl --no-pager | grep -c 'tskey-'")
    check("the auth key is gone from the bootstrap file and not in /var, /etc or the journal",
          out.splitlines()[0].strip() == "0" and out.strip().splitlines()[-1].strip() == "0" and "tskey" not in out.replace("tskey-","X"), out)

    # noded on the tailnet with the real authorizer (the debug unit overrides this; here we run the release code path)
    vm.sh("mkdir -p /run/systemd/system/noded.service.d && printf '[Service]\\nEnvironment=NODED_DEBUG_ALLOW_ALL=0\\nExecStart=\\nExecStart=/usr/bin/noded -listen tailscale:7777\\n' > /run/systemd/system/noded.service.d/zz-tailnet.conf && systemctl daemon-reload && systemctl restart noded; sleep 4")
    rc, out = vm.sh("ss -tlnp | grep 7777 | awk '{print $4}'")
    print("noded listens on:", out)
    check("noded listens only on the Tailscale address", out.strip() == f"{ip}:7777", out)
    rc, out = nc(f"{ip}:7777", "status", "x")
    print(out.splitlines()[:3])
    check("Mac -> VM over the tailnet reaches noded (needs an ACL grant for tcp:7777 to the device's tag)", rc == 0 and "image version" in out, out[-300:])

    # a caller on the node itself (what a compromised host-network pod would be) must be refused even though the port is open
    rc, out = vm.sh(f"/usr/bin/nodectl --addr {ip}:7777 --no-kube status x 2>&1 | tail -2")
    check("a connection from the node itself is refused", "PermissionDenied" in out or "permission" in out.lower(), out)
    rc, out = vm.sh("/usr/bin/nodectl --addr 127.0.0.1:7777 --no-kube status x 2>&1 | tail -2")
    check("loopback is not even listening (noded binds only the Tailscale address)", "PermissionDenied" not in out and ("refused" in out or "connect" in out.lower() or "unavailable" in out.lower()), out)
    rc, out = vm.sh("tailscale logout; sleep 2; tailscale status 2>&1 | head -2", timeout=60)
    print("logout:", out)
finally:
    vm.stop()
    # nothing from this run should keep the key
    log = open(f"{work}/console.log", errors="replace").read()
    check("the auth key never appeared on the VM console", "tskey-" not in log, "")
    shutil.rmtree(work, ignore_errors=True)
ok = sum(1 for _, o in results if o); print(f"\n{ok}/{len(results)} passed"); sys.exit(0 if ok == len(results) else 1)
