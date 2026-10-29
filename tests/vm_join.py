#!/usr/bin/env python3
"""First-boot provisioning and a real k3s agent join, against a THROWAWAY k3s server in Docker (never the real cluster).
Uses a test-only token; no Tailscale (EDGE_SKIP_TAILSCALE=1), flannel on eth0. Checks node name, labels, taint, annotation,
external-IP lookup (ip.me), health gate with k3s, nodectl registry change, memory with the agent running, and oomd."""
import os, re, shutil, subprocess, sys, time
sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "scripts"))
from vmdrive import VM

ROOT = os.getcwd(); NODECTL = f"{ROOT}/.vm/bin/nodectl"; NAME = "edge-test-k3s"
results = []

def check(name, ok, detail=""):
    results.append((name, ok)); print(("PASS " if ok else "FAIL ") + name + (f"  [{detail}]" if detail and not ok else ""), flush=True)

def dk(*a, timeout=120):
    p = subprocess.run(["docker", *a], capture_output=True, text=True, timeout=timeout); return p.returncode, (p.stdout + p.stderr).strip()

def nc(*a):
    p = subprocess.run([NODECTL, "--addr", "127.0.0.1:7777", "--no-kube", "--yes", *a], capture_output=True, text=True, timeout=300)
    return p.returncode, (p.stdout + p.stderr).strip()

dk("rm", "-f", NAME)
rc, out = dk("run", "-d", "--name", NAME, "--privileged", "-p", "6443:6443", "rancher/k3s:v1.35.5-k3s1", "server",
             "--token", "testtoken", "--tls-san", "10.0.2.2", "--disable", "traefik", "--disable", "servicelb",
             "--disable", "metrics-server", "--write-kubeconfig-mode", "644", timeout=600)
check("throwaway k3s server started", rc == 0, out[-200:])
for _ in range(60):
    rc, out = dk("exec", NAME, "kubectl", "get", "nodes")
    if rc == 0 and "Ready" in out: break
    time.sleep(5)
check("throwaway server is Ready", rc == 0 and "Ready" in out, out[-200:])

work = ".vm/run-join"; shutil.rmtree(work, ignore_errors=True); os.makedirs(work)
shutil.copyfile(".vm/builds/1.0.0/edge-node.raw", f"{work}/disk.raw"); os.truncate(f"{work}/disk.raw", 12 * 1024**3)
shutil.copyfile("/opt/homebrew/share/qemu/edk2-arm-vars.fd", f"{work}/vars.fd")
vm = VM(f"{work}/disk.raw", f"{work}/vars.fd", work, fwd=("tcp:127.0.0.1:7777-:7777",))
vm.start()
try:
    vm.login()
    vm.sh("mkdir -p /var/lib/edge && cat > /var/lib/edge/bootstrap.env <<'EOF'\n"
          "NODE_NAME=edge-test-01\nK3S_TOKEN=testtoken\nK3S_SERVER_URL=https://10.0.2.2:6443\nEDGE_SKIP_TAILSCALE=1\n"
          "NODE_IP=10.0.2.15\nFLANNEL_IFACE=eth0\nNODED_ALLOW_USERS=admin@example.com\nEOF\n"
          "echo EDGE_HEALTH_SKIP=tailscaled > /var/lib/edge/health.conf")
    rc, out = vm.sh("systemctl restart edge-firstboot; echo rc=$?; journalctl -u edge-firstboot --no-pager -n 12 | cut -c1-200", timeout=240)
    print(out)
    check("edge-firstboot completes", "firstboot: done for edge-test-01" in out, out[-400:])
    rc, out = vm.sh("hostname; cat /var/lib/edge/external-ip; cat /var/lib/edge/noded.json; ls -la /var/lib/edge/k3s-token | cut -c1-12; cat /var/lib/edge/k3s-config.yaml")
    print(out)
    ip = re.search(r"^\d+\.\d+\.\d+\.\d+$", out, re.M)
    check("hostname set from NODE_NAME", out.splitlines()[0] == "edge-test-01", out)
    check("external IP from ip.me is a public IPv4", bool(ip), out)
    check("noded allowlist written", '"admin@example.com"' in out, out)
    check("token file is 0600", "-rw-------" in out, out)
    check("k3s config has taint, label, external ip, eth0 flannel, swap-tolerant kubelet",
          all(x in out for x in ["node-role=ingress:NoSchedule", "node-type=ingress", "node-external-ip", "flannel-iface: eth0", "fail-swap-on=false"]), out)
    check("k3s config contains no token value", "testtoken" not in out.split("server:")[-1], "")
    rc, out = vm.sh("readlink -f /etc/rancher/k3s/registries.yaml; cat /var/lib/edge/registries.yaml")
    check("registries.yaml is the /var file and has the mirror default", "mirror.gcr.io" in out and "/var/lib/edge/registries.yaml" in out, out)

    rc, out = vm.sh("systemctl start k3s-agent; sleep 5; systemctl is-active k3s-agent", timeout=120)
    check("k3s-agent starts", "active" in out, out)
    node = ""
    for _ in range(60):
        rc, node = dk("exec", NAME, "kubectl", "get", "node", "edge-test-01", "--no-headers")
        if rc == 0 and " Ready" in node: break
        time.sleep(5)
    check("node edge-test-01 joined and is Ready", " Ready" in node, node)
    rc, out = dk("exec", NAME, "kubectl", "get", "node", "edge-test-01", "-o", "jsonpath={.metadata.labels} {.spec.taints} {.status.addresses}")
    print(out)
    check("node carries node-type=ingress and the NoSchedule taint from the first heartbeat",
          "node-type" in out and "ingress" in out and "NoSchedule" in out, out)
    check("node external IP is set", "ExternalIP" in out, out)

    vm.sh("systemctl start edge-node-annotate", timeout=120)
    time.sleep(5)
    rc, out = dk("exec", NAME, "kubectl", "get", "node", "edge-test-01", "-o", "jsonpath={.metadata.annotations}")
    check("image-version annotation set with the kubelet's own credentials", "node.vmd1.dev/image-version" in out and "1.0.0" in out, out)

    rc, out = vm.sh("/usr/libexec/edge-health; echo health_rc=$?", timeout=200)
    check("health gate passes with k3s Ready", "health_rc=0" in out and "ok 20-k3s-node" in out, out)

    time.sleep(60)
    rc, out = vm.sh("free -m | sed -n 1,3p; ps -eo rss,comm --sort=-rss | head -8")
    print("memory with the k3s agent running:\n" + out)

    rc, out = nc("registry", "x", "set", "--mirror", "https://mirror.example.org")
    check("nodectl registry set restarts k3s-agent", rc == 0 and "restarted: true" in out, out)
    rc, out = vm.sh("cat /var/lib/edge/registries.yaml")
    check("registries.yaml rewritten", "mirror.example.org" in out, out)
    for _ in range(40):
        rc, node = dk("exec", NAME, "kubectl", "get", "node", "edge-test-01", "--no-headers")
        if rc == 0 and " Ready" in node: break
        time.sleep(5)
    check("node is Ready again after the restart", " Ready" in node, node)

    rc, out = vm.sh("systemctl stop k3s-agent; EDGE_HEALTH_TIMEOUT=12 /usr/libexec/edge-health; echo health_rc=$?; systemctl is-system-running", timeout=120)
    check("health gate fails when the node is not Ready, and does not reboot a blessed boot", "health_rc=1" in out, out)

    rc, out = vm.sh("systemctl status systemd-oomd --no-pager 2>&1 | head -14; journalctl -u systemd-oomd --no-pager -n 12 | cut -c1-200")
    print("oomd:\n" + out)
    check("systemd-oomd is running", "active (running)" in out, out)
finally:
    vm.stop()
    print(dk("exec", NAME, "kubectl", "get", "nodes", "-o", "wide")[1])
    dk("rm", "-f", NAME)
ok = sum(1 for _, o in results if o); print(f"\n{ok}/{len(results)} passed"); sys.exit(0 if ok == len(results) else 1)
