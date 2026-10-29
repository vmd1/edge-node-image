#!/usr/bin/env python3
"""Replacement race: a labels-only Node object (what Flux creates from node-labels/<name>.yaml) already exists when the new node's
kubelet registers. Does the taint (--register-with-taints) and the node-type label still get applied? Touches only the test node."""
import os, subprocess, sys, time
sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "scripts"))
from vmdrive import TcpSerial

NODE = "edge-test-x86"
def kc(*a, inp=None):
    p = subprocess.run(["kubectl", *a], capture_output=True, text=True, input=inp, timeout=90); return p.returncode, (p.stdout + p.stderr).strip()

vm = TcpSerial("127.0.0.1", 4555); vm.start()
try:
    vm.login(60)
    rc, out = vm.sh("systemctl stop k3s-agent; systemctl is-active k3s-agent", timeout=120); print("agent stopped:", out)
    print(kc("delete", "node", NODE)[1]); print(kc("-n", "kube-system", "delete", "secret", f"{NODE}.node-password.k3s", "--ignore-not-found")[1])
    stub = f"apiVersion: v1\nkind: Node\nmetadata:\n  name: {NODE}\n  annotations:\n    kustomize.toolkit.fluxcd.io/prune: disabled\n  labels:\n    node-role.kubernetes.io/ingress: \"true\"\n    node.vmd1.dev/stub-test: \"true\"\n"
    print("stub:", kc("create", "-f", "-", inp=stub)[1])
    rc, out = kc("get", "node", NODE, "-o", "jsonpath={.spec.taints} | {.metadata.labels}")
    print("stub before the node registers:", out[:300])
    vm.sh("rm -rf /var/lib/rancher/k3s /var/lib/edge/rancher-node/*; systemctl start --no-block k3s-agent", timeout=120)
    t0 = time.time(); ready = False
    while time.time() - t0 < 300:
        time.sleep(8)
        rc, out = kc("get", "node", NODE, "--no-headers")
        if rc == 0 and " Ready" in out and "NotReady" not in out:
            ready = True; break
    print("Ready:", ready, f"({time.time()-t0:.0f}s)")
    rc, out = kc("get", "node", NODE, "-o", "jsonpath={.spec.taints}")
    print("taints after registration:", out or "(none)")
    rc, out = kc("get", "node", NODE, "-o", "jsonpath={.metadata.labels}")
    print("labels after registration:", out[:600])
    print("RESULT taint applied:", "NoSchedule" in kc("get", "node", NODE, "-o", "jsonpath={.spec.taints}")[1],
          "| node-type label applied:", '"node-type":"ingress"' in out)
finally:
    vm.stop()
