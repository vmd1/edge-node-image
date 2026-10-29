#!/usr/bin/env python3
"""Check that the kernel image really embeds the initrd drop-in that makes a failed root mount reboot (not sit in emergency)."""
import struct, subprocess, sys
d = open(sys.argv[1], "rb").read()
pe = struct.unpack_from("<I", d, 0x3c)[0]
n = struct.unpack_from("<H", d, pe + 6)[0]; opt = struct.unpack_from("<H", d, pe + 20)[0]
off = pe + 24 + opt
for i in range(n):
    name = d[off + i * 40: off + i * 40 + 8].rstrip(b"\0").decode()
    vsize, _, rsize, rptr = struct.unpack_from("<IIII", d, off + i * 40 + 8)
    if name == ".initrd":
        raw = d[rptr: rptr + min(vsize, rsize)]
        out = subprocess.run(["zstd", "-dc"], input=raw, capture_output=True).stdout
        ok = b"emergency.service.d/10-reboot.conf" in out
        print("initrd embeds the emergency-reboot drop-in:", ok)
        sys.exit(0 if ok else 1)
print("no .initrd section"); sys.exit(2)
