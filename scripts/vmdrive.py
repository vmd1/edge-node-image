"""Drive the local QEMU VM over its serial console (autologin root shell, debug profile only)."""
import os, re, socket, subprocess, threading, time

Q = "/opt/homebrew/share/qemu"


class VM:
    def __init__(self, disk, vars_fd, workdir, mem=1024, smp=2, fwd=()):
        self.disk, self.vars_fd, self.dir = disk, vars_fd, workdir
        self.sock = os.path.join(workdir, "serial.sock")
        self.logfile = os.path.join(workdir, "console.log")
        self.mem, self.smp, self.fwd = mem, smp, fwd
        self.buf = ""
        self.lock = threading.Lock()
        self.proc = None

    def start(self):
        for p in (self.sock,):
            if os.path.exists(p):
                os.unlink(p)
        open(self.logfile, "w").close()
        netdev = "user,id=net0" + "".join(f",hostfwd={f}" for f in self.fwd)
        args = ["qemu-system-aarch64", "-machine", "virt", "-accel", "hvf", "-cpu", "host",
                "-smp", str(self.smp), "-m", str(self.mem),
                "-drive", f"if=pflash,format=raw,readonly=on,file={Q}/edk2-aarch64-code.fd",
                "-drive", f"if=pflash,format=raw,file={self.vars_fd}",
                "-drive", f"if=none,id=disk0,format=raw,file={self.disk}",
                "-device", "virtio-blk-pci,drive=disk0",
                "-netdev", netdev, "-device", "virtio-net-pci,netdev=net0,romfile=",
                "-display", "none", "-monitor", "none",
                "-chardev", f"socket,id=ser0,path={self.sock},server=on,wait=off,logfile={self.logfile}",
                "-serial", "chardev:ser0"]
        self.proc = subprocess.Popen(args, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        for _ in range(100):
            if os.path.exists(self.sock):
                break
            time.sleep(0.1)
        self.s = socket.socket(socket.AF_UNIX)
        self.s.connect(self.sock)
        threading.Thread(target=self._read, daemon=True).start()

    def _read(self):
        while True:
            try:
                d = self.s.recv(65536)
            except OSError:
                return
            if not d:
                return
            with self.lock:
                self.buf += d.decode("utf-8", "replace")

    def wait_for(self, regex, timeout=120, consume=True):
        end = time.time() + timeout
        pat = re.compile(regex, re.S)
        while time.time() < end:
            with self.lock:
                m = pat.search(self.buf)
                if m:
                    out = self.buf[:m.end()]
                    if consume:
                        self.buf = self.buf[m.end():]
                    return out
            time.sleep(0.2)
        raise TimeoutError(f"timeout waiting for {regex!r}; tail: {self.buf[-600:]!r}")

    def send(self, text):
        self.s.sendall(text.encode())

    def login(self, timeout=240):
        """Wait until the debug shell answers."""
        end = time.time() + timeout
        while time.time() < end:
            self.send("\necho __UP_$((6*7))__\n")
            try:
                self.wait_for(r"__UP_42__", 6)
                time.sleep(0.5)
                return
            except TimeoutError:
                pass
        raise TimeoutError("debug shell never answered; tail: %r" % self.buf[-600:])

    def sh(self, cmd, timeout=60):
        tag = f"__RC_{int(time.time()*1000)%10**8}_"
        self.send(f"{cmd}; echo {tag}$?__\n")
        out = self.wait_for(tag + r"(\d+)__", timeout)
        rc = int(re.search(tag + r"(\d+)__", out).group(1))
        body = out.split(f"echo {tag}$?__", 1)[-1]
        body = re.sub(tag + r"\d+__.*", "", body, flags=re.S)
        body = re.sub(r"\x1b\[[0-9;?]*[A-Za-z]|\x1b\][^\x07]*\x07|\x1b[=>]|\x1b\[!p", "", body)
        return rc, body.replace("\r", "").strip()

    def stop(self):
        if self.proc:
            self.proc.kill()
            self.proc.wait()
