#!/usr/bin/env python3
"""Boot an image in the local VM, run shell commands in it, print the output.
   scripts/vmcmd.py <image.raw> 'cmd1' 'cmd2' ...     (HTTP site on :8000 is served from .vm/site if it exists)"""
import os, shutil, subprocess, sys
sys.path.insert(0, os.path.dirname(__file__))
from vmdrive import VM
img, cmds = sys.argv[1], sys.argv[2:]
work = ".vm/run-cmd"
shutil.rmtree(work, ignore_errors=True); os.makedirs(work)
shutil.copyfile(img, f"{work}/disk.raw"); os.truncate(f"{work}/disk.raw", 12 * 1024**3)
shutil.copyfile("/opt/homebrew/share/qemu/edk2-arm-vars.fd", f"{work}/vars.fd")
srv = None
if os.path.isdir(".vm/site"):
    srv = subprocess.Popen([sys.executable, "tests/serve.py", ".vm/site", "8000"])
vm = VM(f"{work}/disk.raw", f"{work}/vars.fd", work, fwd=("tcp:127.0.0.1:7777-:7777",))
vm.start()
try:
    vm.login()
    for c in cmds:
        rc, out = vm.sh(c, timeout=300)
        print(f"$ {c}\n[rc={rc}]\n{out}\n")
finally:
    vm.stop()
    if srv: srv.terminate()
