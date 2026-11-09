#!/usr/bin/env python3
"""End-to-end check of the roadmap features on the x86-64 debug image (KVM elsewhere, serial through a forwarded TCP port 4555).

The VM joins a THROWAWAY k3s server running in Docker on this Mac over the tailnet (the production path). The real cluster is
never touched: every nodectl call that uses kubectl gets KUBECONFIG pointed at the throwaway server.

  env: CRD_DIR=<dir with edgenodeconfig-crd.yaml firewallrule-crd.yaml rbac.yaml>   NODECTL=<path to a darwin nodectl>
"""
import json, os, re, subprocess, sys, time
sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "scripts"))
from vmdrive import TcpSerial

NODECTL = os.environ.get("NODECTL", f"{os.getcwd()}/.vm/bin/nodectl"); CRD_DIR = os.environ["CRD_DIR"]
CT = "edge-test-k3s"; NODE = "edge-test-x86"
KEY = open(os.path.expanduser("~/.config/edge-image-test/tailscale-authkey")).read().strip()
MAC_TS = subprocess.run(["tailscale", "ip", "-4"], capture_output=True, text=True).stdout.split()[0]
KC = "/tmp/edge-throwaway.kubeconfig"
results = []
def check(name, ok, detail=""):
    results.append((name, ok)); print(("PASS " if ok else "FAIL ") + name + (f"  [{str(detail)[-300:]}]" if detail and not ok else ""), flush=True)
def dk(*a, timeout=180):
    p = subprocess.run(["docker", *a], capture_output=True, text=True, timeout=timeout); return p.returncode, (p.stdout + p.stderr).strip()
def kc(*a, timeout=60): return dk("exec", CT, "kubectl", *a, timeout=timeout)
def kapply(yaml): 
    p = subprocess.run(["docker", "exec", "-i", CT, "kubectl", "apply", "-f", "-"], input=yaml, capture_output=True, text=True, timeout=60); return p.returncode, p.stdout + p.stderr
def ann(key): return kc("get", "node", NODE, "-o", "jsonpath={.metadata.annotations.node\\.vmd1\\.dev/" + key + "}")[1]
def wait(fn, timeout, every=5):
    end = time.time() + timeout
    while time.time() < end:
        v = fn()
        if v: return v
        time.sleep(every)
    return None
def ctl(*a, kube=False, timeout=120, stdin=None):
    env = dict(os.environ, KUBECONFIG=KC) if kube else dict(os.environ)
    base = [NODECTL] + ([] if kube else ["--no-kube"]) + ["--addr", f"{IP}:7777"]
    p = subprocess.run(base + list(a), capture_output=True, text=True, timeout=timeout, env=env, input=stdin); return p.returncode, (p.stdout + p.stderr).strip()

dk("rm", "-f", CT)
rc, out = dk("run", "-d", "--name", CT, "--privileged", "-p", "6443:6443", "rancher/k3s:v1.35.5-k3s1", "server", "--token", "testtoken",
             "--tls-san", MAC_TS, "--disable", "traefik", "--disable", "servicelb", "--disable", "metrics-server", "--write-kubeconfig-mode", "644", timeout=900)
check("throwaway k3s server started", rc == 0, out)
check("throwaway server Ready", wait(lambda: kc("get", "nodes")[0] == 0 and "Ready" in kc("get", "nodes")[1], 300), "")
for f in ["edgenodeconfig-crd.yaml", "firewallrule-crd.yaml", "rbac.yaml"]:
    dk("cp", f"{CRD_DIR}/{f}", f"{CT}:/tmp/{f}")
    rc, out = kc("apply", "-f", f"/tmp/{f}"); check(f"applied {f}", rc == 0, out)
rc, kcfg = dk("exec", CT, "cat", "/etc/rancher/k3s/k3s.yaml")
open(KC, "w").write(kcfg.replace("127.0.0.1", MAC_TS))

vm = TcpSerial("127.0.0.1", 4555, logfile=".vm/full-console.log"); vm.start()
IP = None
try:
    vm.login(300)
    def sh(c, t=60): return vm.sh(c, timeout=t)[1]
    vm.sh("stty -echo")
    vm.sh("mkdir -p /var/lib/edge && umask 077 && cat > /var/lib/edge/bootstrap.env <<'EOF'\n"
          f"NODE_NAME={NODE}\nTAILSCALE_AUTHKEY={KEY}\nK3S_TOKEN=testtoken\nK3S_SERVER_URL=https://{MAC_TS}:6443\nEOF\ntrue")
    vm.sh("stty echo")
    out = sh("systemctl restart edge-firstboot; journalctl -u edge-firstboot --no-pager -n 4 | cut -c1-150 | sed 's/tskey-[A-Za-z0-9-]*/<key>/g'", 300)
    check("first boot done (tailnet joined, k3s config rendered)", "firstboot: done" in out, out)
    IP = sh("tailscale ip -4").strip().splitlines()[-1]
    sh("systemctl start --no-block k3s-agent", 60)
    check("node joined and Ready", wait(lambda: " Ready" in kc("get", "node", NODE, "--no-headers")[1], 450), kc("get", "nodes")[1])
    time.sleep(20)

    # ---- module lockdown (1.A): lock, then everything below must still work -------------------------------------------------
    out = sh("/usr/libexec/edge-health; echo rc=$?", 300)
    check("boot health gate passes", "rc=0" in out, out)
    print("loaded modules before lock:", sh("wc -l < /proc/modules").strip())
    sh("systemctl start --no-block edge-lock-modules", 10)
    check("kernel.modules_disabled becomes 1", wait(lambda: sh("cat /proc/sys/kernel/modules_disabled").strip().splitlines()[-1] == "1", 200, 10), sh("systemctl status edge-lock-modules --no-pager | tail -5"))

    # ---- noded reachable, introspection (2.C) ------------------------------------------------------------------------------
    rc, out = ctl("status", "x"); check("nodectl status over the tailnet", rc == 0 and "image version" in out, out)
    rc, out = ctl("whoami", "x"); print(out); check("whoami reports a mode (audit by default)", "audit" in out, out)
    for kind in ["image", "memory", "psi", "mounts", "sysctl", "modules", "netstat", "services", "tailscale", "kubelet", "time", "config", "firewall"]:
        rc, out = ctl("get", "x", kind); check(f"get {kind}", rc == 0 and len(out) > 10, out)
    rc, out = ctl("get", "x", "sysctl"); check("sysctl resource shows modules_disabled=1", re.search(r'"kernel.modules_disabled",\s*"value": "1"', out) is not None, out[-200:])
    rc, out = ctl("get", "x", "cgroups", "memory"); check("cgroups memory preset", rc == 0 and "MiB" in out, out)
    rc, out = ctl("ps", "x", "rss", "-n", "5"); check("ps by rss", rc == 0 and "comm" in out, out)
    rc, out = ctl("netstat", "x"); check("netstat lists noded on the tailnet address", f"{IP}:7777" in out, out)
    rc, out = ctl("ls", "x", "/var/lib/edge"); check("ls an allowlisted dir hides secrets", rc == 0 and "k3s-token" not in out and "bootstrap.env" not in out, out)
    rc, out = ctl("read", "x", "/var/lib/edge/k3s-token"); check("read refuses a secret", rc != 0 and ("secret" in out.lower()), out)
    rc, out = ctl("read", "x", "/etc/shadow"); check("read refuses a path outside the allowlist", rc != 0, out)
    rc, out = ctl("read", "x", "/var/lib/edge/external-ip"); check("read an allowlisted file", rc == 0 and re.search(r"\d+\.\d+\.\d+\.\d+", out), out)
    rc, out = ctl("service", "x", "status", "k3s-agent.service"); check("service status", rc == 0 and "k3s-agent" in out, out)
    rc, out = ctl("service", "x", "restart", "sshd.service"); check("service refuses non-allowlisted units", rc != 0, out)
    rc, out = ctl("dmesg", "x"); check("dmesg", rc == 0 and len(out) > 100, out)
    rc, out = ctl("events", "x", "-n", "20"); check("events", rc == 0 and len(out) > 20, out)
    rc, out = ctl("health", "x"); check("health", "ok=true" in out, out)
    rc, out = ctl("support", "x", "-o", "/tmp"); check("support bundle written", rc == 0 and "support-" in out, out)
    p = subprocess.run(f"{NODECTL} --no-kube --addr {IP}:7777 pcap x --seconds 4 --proto tcp > /tmp/edge.pcap; xxd -l 4 -p /tmp/edge.pcap; wc -c < /tmp/edge.pcap", shell=True, capture_output=True, text=True, timeout=60)
    check("pcap writes a valid pcap header", p.stdout.startswith("d4c3b2a1"), p.stdout + p.stderr)
    rc, out = ctl("audit", "x"); check("audit log has the calls above", "audit id=" in out and "method=Status" in out, out[-300:])

    # ---- declarative config (1.B) ------------------------------------------------------------------------------------------
    cap0 = kc("get", "node", NODE, "-o", "jsonpath={.status.capacity.pods}")[1]
    rc, out = kapply("""apiVersion: edge.vmd1.dev/v1alpha1
kind: EdgeNodeConfig
metadata: {name: t1}
spec:
  kubelet: {maxPods: 77, evictionHard: {memory.available: 90Mi}}
  nodeLabels: {team: edge-test}
  sysctls: {vm.swappiness: "77"}
""")
    check("EdgeNodeConfig t1 created", rc == 0, out)
    check("config-state applied within 3 minutes", wait(lambda: ann("config-state") == "applied" and ann("config-hash"), 240, 10), ann("config-error"))
    rc, out = ctl("config", "x", "show"); print(out[:600])
    check("config show has the rendered drop-in", "max-pods=77" in out and "vm.swappiness = 77" in out, out)
    check("kubelet capacity pods == 77 after the restart", wait(lambda: kc("get", "node", NODE, "-o", "jsonpath={.status.capacity.pods}")[1] == "77", 180, 10), kc("get", "node", NODE, "-o", "jsonpath={.status.capacity.pods}")[1] + " (was " + cap0 + ")")
    check("node label from the config", wait(lambda: kc("get", "node", NODE, "--show-labels")[1].find("team=edge-test") >= 0, 120, 10), "")
    check("sysctl applied", sh("sysctl -n vm.swappiness").strip().splitlines()[-1] == "77", sh("sysctl -n vm.swappiness"))
    check("node Ready after the config restart", wait(lambda: " Ready" in kc("get", "node", NODE, "--no-headers")[1], 200), "")

    kapply("""apiVersion: edge.vmd1.dev/v1alpha1
kind: EdgeNodeConfig
metadata: {name: bad}
spec:
  nodeSelector: {kubernetes.io/hostname: %s}
  sysctls: {kernel.core_pattern: "1"}
""" % NODE)
    check("invalid object -> config-state rejected with an error", wait(lambda: ann("config-state") == "rejected" and ann("config-error"), 180, 10), ann("config-state"))
    check("running config kept while rejected", kc("get", "node", NODE, "-o", "jsonpath={.status.capacity.pods}")[1] == "77", "")
    kc("delete", "edgenodeconfig", "bad")
    check("state returns to applied after removing the bad object", wait(lambda: ann("config-state") == "applied", 180, 10), ann("config-state"))

    # a config that cannot start the kubelet in try mode must revert on its own
    kapply("""apiVersion: edge.vmd1.dev/v1alpha1
kind: EdgeNodeConfig
metadata: {name: breaker}
spec:
  nodeSelector: {kubernetes.io/hostname: %s}
  mode: try
  tryTimeoutSeconds: 75
  kubelet: {kubeReserved: {memory: 100Gi}}
""" % NODE)
    check("unhealthy try config reverts: state rolled-back", wait(lambda: ann("config-state") == "rolled-back", 420, 10), ann("config-state") + " " + ann("config-error"))
    check("node Ready again after the revert", wait(lambda: " Ready" in kc("get", "node", NODE, "--no-headers")[1], 300), kc("get", "nodes")[1])
    check("pods capacity back at 77 (last-known-good)", kc("get", "node", NODE, "-o", "jsonpath={.status.capacity.pods}")[1] == "77", "")
    kc("delete", "edgenodeconfig", "breaker")

    rc, out = ctl("config", "x", "validate", "-f", "-", stdin='{"kubelet":{"maxPods":3}}'); check("config validate refuses maxPods 3", rc != 0 and "maxPods" in out, out)
    rc, out = ctl("config", "x", "set", "-f", "-", "--ttl", "10m", stdin='{"sysctls":{"vm.swappiness":"55"}}'); check("config set override", rc == 0 and "expires" in out, out)
    check("override beats the cluster config (swappiness 55)", wait(lambda: sh("sysctl -n vm.swappiness").strip().splitlines()[-1] == "55", 200, 10), "")
    rc, out = ctl("config", "x", "clear"); check("config clear", rc == 0, out)
    check("cluster config returns (swappiness 77)", wait(lambda: sh("sysctl -n vm.swappiness").strip().splitlines()[-1] == "77", 200, 10), "")

    # ---- host firewall (2.F) -----------------------------------------------------------------------------------------------
    rc, out = kapply("""apiVersion: network.vmd1.dev/v1alpha1
kind: FirewallRule
metadata: {name: t-host}
spec: {type: Host, ports: [{port: 5555, protocol: TCP}]}
""")
    check("FirewallRule Host created", rc == 0, out)
    check("firewall-state applied", wait(lambda: ann("firewall-state") == "applied", 180, 10), ann("firewall-error"))
    rc, out = ctl("get", "x", "firewall"); print(out[:500])
    check("nft table has default drop, baseline, trusted interfaces and the new port", all(s in out for s in ["policy drop", "tcp dport 5555 accept", "tcp dport 443 accept", "tailscale0"]), out)
    rc, out = ctl("status", "x"); check("noded still reachable with the firewall on", rc == 0, out)
    check("node still Ready with the firewall on", " Ready" in kc("get", "node", NODE, "--no-headers")[1], "")
    out = sh("nft list ruleset 2>&1 | grep -c 'table inet edge'")
    check("host rules loaded in the kernel", out.strip().splitlines()[-1] == "1", out)
    kc("delete", "firewallrule", "t-host")
    check("rule removed -> port closed again", wait(lambda: "5555" not in ctl("get", "x", "firewall")[1], 150, 10), "")

    # ---- roles (2.E): enforce with no grant must deny; removing the object restores access --------------------------------
    kapply("""apiVersion: edge.vmd1.dev/v1alpha1
kind: EdgeNodeConfig
metadata: {name: roles}
spec: {apiRoles: enforce}
""")
    check("apiRoles enforce takes effect", wait(lambda: "PermissionDenied" in ctl("status", "x")[1] or "denied" in ctl("status", "x")[1].lower(), 240, 10), ctl("status", "x")[1])
    rc, out = ctl("reboot", "x"); check("a caller without a grant is refused (no reboot)", rc != 0 and "role" in out.lower(), out)
    kc("delete", "edgenodeconfig", "roles")
    check("access returns when the object is removed (audit mode)", wait(lambda: ctl("status", "x")[0] == 0, 300, 10), ctl("status", "x")[1])

    # ---- fleet commands against the throwaway server only (2.D) ------------------------------------------------------------
    rc, out = ctl("health", "--all", kube=True); check("health --all against the throwaway fleet", rc == 0 and "ok" in out, out)
    rc, out = ctl("rollout", kube=True); check("rollout dry run prints a plan", rc == 0 and ("dry run" in out or "up to date" in out), out)
    rc, out = ctl("reset", "x", "--wipe", "k3s", kube=True); check("reset without --yes only prints the plan", rc == 0 and "plan for" in out, out)
    rc, out = ctl("hard-reset", "x", kube=True); check("hard-reset without --yes only prints the plan", rc == 0 and "plan:" in out, out)

    # reset --wipe k3s: node reboots, rejoins under the same name
    boot0 = sh("cat /proc/sys/kernel/random/boot_id").strip().splitlines()[-1]
    p = subprocess.run([NODECTL, "--addr", f"{IP}:7777", "--yes", "reset", NODE, "--wipe", "k3s", "--force"], capture_output=True, text=True, timeout=1500, env=dict(os.environ, KUBECONFIG=KC))
    print((p.stdout + p.stderr)[-700:])
    check("reset --wipe k3s completes and the node is Ready again", p.returncode == 0 and "Ready and schedulable" in p.stdout, p.stdout + p.stderr)
    out = sh("cat /proc/sys/kernel/random/boot_id; tailscale ip -4 | head -1; cat /sys/kernel/security/lockdown; sysctl -n kernel.modules_disabled")
    check("after the reset: new boot, Tailscale identity kept", boot0 not in out and IP in out, out)
    rc, out = ctl("health", "x"); print(out[:300])
finally:
    try: vm.sh("tailscale logout", timeout=30)
    except Exception: pass
    vm.stop(); dk("rm", "-f", CT)
ok = sum(1 for _, o in results if o); print(f"\n{ok}/{len(results)} passed")
sys.exit(0 if ok == len(results) else 1)
