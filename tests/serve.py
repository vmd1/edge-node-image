#!/usr/bin/env python3
"""Serve a release directory over HTTP for the local update tests. Optional throttle so a download can be interrupted."""
import http.server, os, sys, time

root, port = sys.argv[1], int(sys.argv[2])
rate = int(os.environ.get("RATE_KBPS", "0"))  # 0 = unlimited


class H(http.server.SimpleHTTPRequestHandler):
    def __init__(self, *a, **k):
        super().__init__(*a, directory=root, **k)

    def log_message(self, *a):
        pass

    def copyfile(self, src, dst):
        chunk = 64 * 1024
        while True:
            b = src.read(chunk)
            if not b:
                break
            dst.write(b)
            if rate:
                time.sleep(len(b) / (rate * 1024))


http.server.ThreadingHTTPServer(("127.0.0.1", port), H).serve_forever()
