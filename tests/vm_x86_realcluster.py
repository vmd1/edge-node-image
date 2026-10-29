#!/usr/bin/env python3
"""The x86-64 trial image (KVM on hades-02, serial through a forwarded port) joins the REAL cluster as a test node (edge-test-x86).
Config is written to /var/lib/edge/bootstrap.env over the serial console with echo off (secrets never printed or logged), then the
VM is rebooted so the natural boot chain runs unattended: first boot -> tailnet -> k3s agent -> labels/taint -> annotation -> health
gate. The k3s token is read from the control plane and the tag:k3s auth key from the user's key file. Cleanup is a separate step."""
import os, re, subprocess, sys, time
sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "scripts"))
from vmdrive import TcpSerial

NODE = "edge-test-x86"; CP = "100.70.92.5"
KEY = open(os.path.expanduser("~/.config/edge-image-test/tailscale-authkey-k3s")).read().strip()
TOK = subprocess.check_output(["ssh", "-o", "BatchMode=yes", f"vivaan@{CP}", "sudo cat /var/lib/rancher/k3s/server/node-token"]).decode().strip()
assert KEY.startswith("tskey-auth-") and len(TOK) > 20
results = []
def check(name, ok, detail=""):
    results.append((name, ok)); print(("PASS " if ok else "FAIL ") + name + (f"  [{detail}]" if detail and not ok else ""), flush=True)
def kc(*a):
    p = subprocess.run(["kubectl", *a], capture_output=True, text=True, timeout=60); return p.returncode, (p.stdout + p.stderr).strip()
def hades_avail():
    p = subprocess.run(["ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=8", "vivaan@100.93.7.62", "awk '/MemAvailable/{print int($2/1024)}' /proc/meminfo"], capture_output=True, text=True)
    return int(p.stdout.strip() or 0)

rc, out = kc("get", "node", NODE, "--no-headers")
check("no node of that name exists yet in the real cluster", rc != 0, out)

vm = TcpSerial("127.0.0.1", 4555); vm.start()
try:
    vm.login(120)
    vm.sh("stty -echo")
    vm.sh("rm -f /var/lib/edge/bootstrap.env; mkdir -p /var/lib/edge && umask 077 && cat > /var/lib/edge/bootstrap.env <<'EOF'\n"
          f"NODE_NAME={NODE}\nTAILSCALE_AUTHKEY={KEY}\nK3S_TOKEN={TOK}\nK3S_SERVER_URL=https://{CP}:6443\nEOF\ntrue")
    vm.sh("stty echo")
    rc, out = vm.sh("ls -la /var/lib/edge/bootstrap.env | cut -c1-12; grep -c . /var/lib/edge/bootstrap.env")
    check("bootstrap file written (0600, 4 lines)", out.split()[0].startswith("-rw-------") and out.split()[-1] == "4", out)
    vm.send("systemctl reboot\n")   # exercise the natural boot chain, nothing else typed into the machine
finally:
    vm.stop()

print("rebooted; waiting for the node to appear in the real cluster", flush=True)
t0 = time.time(); seen = None; ready = None; low = 10**9
while time.time() - t0 < 900:
    time.sleep(8)
    a = hades_avail(); low = min(low, a)
    if a < 400:
        print(f"ABORT: hades-02 available memory {a} MiB"); break
    rc, out = kc("get", "node", NODE, "--no-headers")
    if rc == 0 and seen is None:
        seen = time.time() - t0; print(f"  node appeared after {seen:.0f}s: {out}", flush=True)
    if rc == 0 and " Ready" in out:
        ready = time.time() - t0; break
check("the node appeared in the real cluster on its own after the reboot", seen is not None, "")
check("and went Ready", ready is not None, "")
print(f"  hades-02 lowest available memory during the join: {low} MiB")
if ready:
    print(f"  time to Ready after reboot: {ready:.0f}s")
    rc, out = kc("get", "node", NODE, "-o", "jsonpath={.metadata.labels} | {.spec.taints} | {.status.addresses}")
    print("  " + out[:700])
    check("taint node-role=ingress:NoSchedule present from the first heartbeat", "NoSchedule" in out and "ingress" in out, out[:200])
    check("node-type=ingress label present", '"node-type":"ingress"' in out, out[:200])
    check("tailnet internal IP and public external IP both reported", "InternalIP" in out and "ExternalIP" in out and "100." in out, out[:300])
    time.sleep(20)
    rc, out = kc("get", "node", NODE, "-o", "jsonpath={.metadata.annotations}")
    check("image-version annotation set by the node itself", "node.vmd1.dev/image-version" in out, out[:200])
