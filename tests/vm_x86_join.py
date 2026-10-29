#!/usr/bin/env python3
"""The x86-64 trial image (running under KVM elsewhere, serial through a forwarded TCP port) joins the user's tailnet with the
one-off test key and a THROWAWAY k3s server on this Mac, over Tailscale: the production path (flannel on tailscale0, node IP from
Tailscale, real health gate). Reports memory with the k3s agent running. The real cluster is never involved."""
import os, re, subprocess, sys, time
sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "scripts"))
from vmdrive import TcpSerial

NODECTL = f"{os.getcwd()}/.vm/bin/nodectl"; NAME = "edge-test-k3s"
KEY = open(os.path.expanduser("~/.config/edge-image-test/tailscale-authkey")).read().strip()
MAC_TS = subprocess.run(["tailscale", "ip", "-4"], capture_output=True, text=True).stdout.split()[0]
results = []
def check(name, ok, detail=""):
    results.append((name, ok)); print(("PASS " if ok else "FAIL ") + name + (f"  [{detail}]" if detail and not ok else ""), flush=True)
def dk(*a, timeout=120):
    p = subprocess.run(["docker", *a], capture_output=True, text=True, timeout=timeout); return p.returncode, (p.stdout + p.stderr).strip()

dk("rm", "-f", NAME)
rc, out = dk("run", "-d", "--name", NAME, "--privileged", "-p", "6443:6443", "rancher/k3s:v1.35.5-k3s1", "server", "--token", "testtoken",
             "--tls-san", MAC_TS, "--disable", "traefik", "--disable", "servicelb", "--disable", "metrics-server", "--write-kubeconfig-mode", "644", timeout=600)
check("throwaway k3s server started", rc == 0, out[-200:])
for _ in range(60):
    rc, out = dk("exec", NAME, "kubectl", "get", "nodes")
    if rc == 0 and "Ready" in out: break
    time.sleep(5)
check("throwaway server Ready", "Ready" in out, out[-200:])

vm = TcpSerial("127.0.0.1", 4555); vm.start()
ip = None
try:
    vm.login(120)
    vm.sh("stty -echo")
    vm.sh("mkdir -p /var/lib/edge && umask 077 && cat > /var/lib/edge/bootstrap.env <<'EOF'\n"
          f"NODE_NAME={NAME.replace('-k3s','-x86')}\nTAILSCALE_AUTHKEY={KEY}\nK3S_TOKEN=testtoken\nK3S_SERVER_URL=https://{MAC_TS}:6443\nEOF\ntrue")
    vm.sh("stty echo")
    # IMDS does not exist in this local VM, so the file we just wrote is used as is
    rc, out = vm.sh("systemctl restart edge-firstboot; echo fb=$?; journalctl -u edge-firstboot --no-pager -n 6 | cut -c1-190 | sed 's/tskey-[A-Za-z0-9-]*/<key>/g'", timeout=300)
    print(out)
    check("first boot: tailnet joined, external IP found, k3s config rendered", "firstboot: done" in out, out[-300:])
    rc, out = vm.sh("tailscale ip -4"); ip = out.strip().splitlines()[-1]
    check("tailscale address assigned", bool(re.fullmatch(r"100\.\d+\.\d+\.\d+", ip)), out)
    rc, out = vm.sh("head -12 /var/lib/edge/k3s-config.yaml | grep -v token")
    print(out)
    check("k3s config uses flannel on tailscale0 and the tailnet node IP", "flannel-iface: tailscale0" in out and f"node-ip: \"{ip}\"" in out, out)

    vm.sh("systemctl start --no-block k3s-agent", timeout=60)
    node = ""
    for _ in range(90):
        rc, node = dk("exec", NAME, "kubectl", "get", "node", "edge-test-x86", "--no-headers")
        if rc == 0 and " Ready" in node: break
        time.sleep(5)
    print(node)
    check("x86 node joined the throwaway server over the tailnet and is Ready", " Ready" in node, node)
    rc, out = dk("exec", NAME, "kubectl", "get", "node", "edge-test-x86", "-o", "jsonpath={.spec.taints} {.status.addresses}")
    check("taint and tailnet internal IP present from the first heartbeat", "NoSchedule" in out and ip in out, out[:300])

    # real health gate (nothing skipped): tailscaled + k3s node Ready
    rc, out = vm.sh("/usr/libexec/edge-health; echo health_rc=$?", timeout=300)
    print(out)
    check("real health gate passes (system, tailscaled, k3s node Ready)", "health_rc=0" in out, out[-300:])

    time.sleep(60)
    rc, out = vm.sh("free -m | sed -n 1,3p; echo; ps -eo rss,comm --sort=-rss | head -8")
    print("MEMORY with the k3s agent running on x86 (954 MB VM):\n" + out)

    # noded on the tailnet: reachable from this Mac if the ACL allows, and the node itself is refused
    rc, out = vm.sh("systemctl is-active noded; ss -tlnp | grep 7777 | awk '{print $4}'")
    print("noded:", out)
    check("noded is listening on the Tailscale address only", out.splitlines()[-1].strip() == f"{ip}:7777", out)
    p = subprocess.run([NODECTL, "--addr", f"{ip}:7777", "--no-kube", "status", "x"], capture_output=True, text=True, timeout=30)
    print((p.stdout + p.stderr).strip()[:600])
    check("this Mac can query noded over the tailnet", p.returncode == 0 and "image version" in p.stdout, (p.stdout + p.stderr)[-300:])
    rc, out = vm.sh(f"/usr/bin/nodectl --addr {ip}:7777 --no-kube status x 2>&1 | tail -1")
    check("a call from the node itself is refused", "PermissionDenied" in out, out)

    rc, out = dk("exec", NAME, "kubectl", "get", "nodes", "-o", "wide")
    print(out)
finally:
    try: vm.sh("tailscale logout", timeout=30)
    except Exception: pass
    vm.stop(); dk("rm", "-f", NAME)
ok = sum(1 for _, o in results if o); print(f"\n{ok}/{len(results)} passed")
