// Package inspect reads node state for the introspection API (nodectl get/ps/netstat/ls/read). Everything is read-only,
// typed and allowlisted; there is no generic exec or arbitrary file read. Roots are parameters so tests use fixtures.
package inspect

import (
	"bufio"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

type Roots struct {
	Proc string // /proc
	Sys  string // /sys
	Root string // "" in production (the allowlisted file paths are absolute below it)
}

func Default() Roots { return Roots{Proc: "/proc", Sys: "/sys"} }

// KV is a key/value pair in a stable order.
type KV struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// Memory returns /proc/meminfo in MiB for the interesting keys.
func (r Roots) Memory() (map[string]uint64, error) {
	f, err := os.Open(filepath.Join(r.Proc, "meminfo"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	want := map[string]bool{"MemTotal": true, "MemFree": true, "MemAvailable": true, "Buffers": true, "Cached": true, "SwapTotal": true,
		"SwapFree": true, "Shmem": true, "Slab": true, "SReclaimable": true, "AnonPages": true, "Dirty": true}
	out := map[string]uint64{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok || !want[k] {
			continue
		}
		n, _ := strconv.ParseUint(strings.Fields(v)[0], 10, 64)
		out[k+"MiB"] = n / 1024
	}
	return out, sc.Err()
}

// PSI returns the pressure lines of cpu, memory and io.
func (r Roots) PSI() (map[string]string, error) {
	out := map[string]string{}
	for _, n := range []string{"cpu", "memory", "io"} {
		b, err := os.ReadFile(filepath.Join(r.Proc, "pressure", n))
		if err != nil {
			continue
		}
		out[n] = strings.TrimSpace(strings.ReplaceAll(string(b), "\n", " | "))
	}
	if len(out) == 0 {
		return nil, errors.New("no pressure files (PSI off?)")
	}
	return out, nil
}

type Mount struct{ Device, Point, FS, Options string }

func (r Roots) Mounts() ([]Mount, error) {
	b, err := os.ReadFile(filepath.Join(r.Proc, "self", "mounts"))
	if err != nil {
		b, err = os.ReadFile(filepath.Join(r.Proc, "mounts"))
		if err != nil {
			return nil, err
		}
	}
	var out []Mount
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Fields(l)
		if len(f) >= 4 {
			out = append(out, Mount{f[0], f[1], f[2], f[3]})
		}
	}
	return out, nil
}

// SysctlKeys are the keys reported by the sysctl resource (the hardening baseline plus the tunables the config can set).
var SysctlKeys = []string{"kernel.kptr_restrict", "kernel.kexec_load_disabled", "kernel.yama.ptrace_scope", "kernel.dmesg_restrict",
	"kernel.unprivileged_bpf_disabled", "kernel.modules_disabled", "net.core.bpf_jit_harden", "vm.swappiness", "vm.vfs_cache_pressure",
	"vm.min_free_kbytes", "vm.page-cluster", "fs.inotify.max_user_instances", "fs.inotify.max_user_watches", "net.core.somaxconn",
	"net.ipv4.ip_forward", "net.netfilter.nf_conntrack_max"}

func (r Roots) Sysctl() []KV {
	var out []KV
	for _, k := range SysctlKeys {
		b, err := os.ReadFile(filepath.Join(r.Proc, "sys", strings.ReplaceAll(k, ".", "/")))
		v := "?"
		if err == nil {
			v = strings.TrimSpace(string(b))
		}
		out = append(out, KV{k, v})
	}
	return out
}

func (r Roots) Modules() ([]string, error) {
	f, err := os.Open(filepath.Join(r.Proc, "modules"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if f := strings.Fields(sc.Text()); len(f) > 0 {
			out = append(out, f[0])
		}
	}
	sort.Strings(out)
	return out, sc.Err()
}

type Process struct {
	PID     int    `json:"pid"`
	Comm    string `json:"comm"`
	RSSMiB  uint64 `json:"rssMiB"`
	CPUTick uint64 `json:"cpuTicks"`
}

// Processes lists processes sorted by "rss" or "cpu", at most limit (default 20, max 200).
func (r Roots) Processes(sortBy string, limit int) ([]Process, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 200 {
		limit = 200
	}
	if sortBy != "rss" && sortBy != "cpu" {
		sortBy = "rss"
	}
	ents, err := os.ReadDir(r.Proc)
	if err != nil {
		return nil, err
	}
	var out []Process
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile(filepath.Join(r.Proc, e.Name(), "stat"))
		if err != nil {
			continue
		}
		s := string(b)
		lp, rp := strings.IndexByte(s, '('), strings.LastIndexByte(s, ')')
		if lp < 0 || rp < lp {
			continue
		}
		f := strings.Fields(s[rp+1:])
		if len(f) < 22 {
			continue
		}
		ut, _ := strconv.ParseUint(f[11], 10, 64)
		st, _ := strconv.ParseUint(f[12], 10, 64)
		rssPages, _ := strconv.ParseInt(f[21], 10, 64)
		if rssPages < 0 {
			rssPages = 0
		}
		out = append(out, Process{PID: pid, Comm: s[lp+1 : rp], RSSMiB: uint64(rssPages) * 4096 / (1 << 20), CPUTick: ut + st})
	}
	sort.Slice(out, func(i, j int) bool {
		if sortBy == "cpu" {
			return out[i].CPUTick > out[j].CPUTick
		}
		return out[i].RSSMiB > out[j].RSSMiB
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

type Listener struct {
	Proto string `json:"proto"`
	Addr  string `json:"addr"`
}

// Netstat returns listening TCP sockets and bound UDP sockets (IPv4 and IPv6).
func (r Roots) Netstat() ([]Listener, error) {
	var out []Listener
	for _, t := range []struct{ file, proto, state string }{{"tcp", "tcp", "0A"}, {"tcp6", "tcp6", "0A"}, {"udp", "udp", "07"}, {"udp6", "udp6", "07"}} {
		f, err := os.Open(filepath.Join(r.Proc, "net", t.file))
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Scan() // header
		for sc.Scan() {
			fl := strings.Fields(sc.Text())
			if len(fl) < 4 || fl[3] != t.state {
				continue
			}
			if a, err := hexAddr(fl[1]); err == nil {
				out = append(out, Listener{t.proto, a})
			}
		}
		f.Close()
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Proto+out[i].Addr < out[j].Proto+out[j].Addr })
	return out, nil
}

func hexAddr(s string) (string, error) {
	ah, ph, ok := strings.Cut(s, ":")
	if !ok {
		return "", errors.New("bad address")
	}
	b, err := hex.DecodeString(ah)
	if err != nil {
		return "", err
	}
	port, err := strconv.ParseUint(ph, 16, 16)
	if err != nil {
		return "", err
	}
	// /proc/net stores each 32-bit word in host (little endian) order
	for i := 0; i+3 < len(b); i += 4 {
		b[i], b[i+1], b[i+2], b[i+3] = b[i+3], b[i+2], b[i+1], b[i]
	}
	return net.JoinHostPort(net.IP(b).String(), strconv.FormatUint(port, 10)), nil
}

// Cgroups reports a preset ("memory", "psi" or "io") for the cgroups two levels below the root, largest first.
func (r Roots) Cgroups(preset string) ([]KV, error) {
	file := map[string]string{"memory": "memory.current", "psi": "memory.pressure", "io": "io.stat"}[preset]
	if file == "" {
		return nil, fmt.Errorf("preset %q: want memory, psi or io", preset)
	}
	base := filepath.Join(r.Sys, "fs", "cgroup")
	var out []KV
	var walk func(dir string, depth int)
	walk = func(dir string, depth int) {
		if b, err := os.ReadFile(filepath.Join(dir, file)); err == nil {
			rel, _ := filepath.Rel(base, dir)
			out = append(out, KV{"/" + strings.TrimPrefix(rel, "."), strings.TrimSpace(strings.ReplaceAll(string(b), "\n", " | "))})
		}
		if depth == 0 {
			return
		}
		ents, _ := os.ReadDir(dir)
		for _, e := range ents {
			if e.IsDir() {
				walk(filepath.Join(dir, e.Name()), depth-1)
			}
		}
	}
	walk(base, 2)
	if preset == "memory" {
		sort.Slice(out, func(i, j int) bool {
			a, _ := strconv.ParseUint(out[i].Value, 10, 64)
			b, _ := strconv.ParseUint(out[j].Value, 10, 64)
			return a > b
		})
		for i := range out {
			n, _ := strconv.ParseUint(out[i].Value, 10, 64)
			out[i].Value = fmt.Sprintf("%d MiB", n/(1<<20))
		}
	}
	return out, nil
}

// ---- files -------------------------------------------------------------------------------------------------------------

// AllowedDirs are the only places ListFiles and ReadFile may touch (prefix match on the resolved path).
var AllowedDirs = []string{"/var/log", "/run/edge", "/var/lib/edge", "/etc/os-release", "/proc/pressure", "/etc/rancher"}

// secretNames can never be read, anywhere in the allowlist.
var secretNames = []string{"k3s-token", "token", "bootstrap.env", "authkey", "tailscale_authkey", ".key", ".pem", "node-token", "passwd", "secret", "kubeconfig"}

func (r Roots) resolve(p string) (string, error) {
	p = filepath.Clean(p)
	if !filepath.IsAbs(p) {
		return "", errors.New("path must be absolute")
	}
	real, err := filepath.EvalSymlinks(filepath.Join(r.Root, p))
	if err != nil {
		return "", err
	}
	rootReal := r.Root
	if rootReal != "" {
		rootReal, _ = filepath.EvalSymlinks(rootReal)
	}
	logical := strings.TrimPrefix(real, rootReal)
	ok := false
	for _, a := range AllowedDirs {
		if logical == a || strings.HasPrefix(logical, a+"/") {
			ok = true
		}
	}
	if !ok {
		return "", fmt.Errorf("%s is not readable through the API", p)
	}
	base := strings.ToLower(filepath.Base(logical))
	for _, s := range secretNames {
		if strings.Contains(base, s) || strings.Contains(strings.ToLower(logical), "/secret") {
			return "", fmt.Errorf("%s may contain secrets and is never served", p)
		}
	}
	return real, nil
}

type FileInfo struct {
	Name  string `json:"name"`
	Size  int64  `json:"size"`
	IsDir bool   `json:"isDir"`
}

func (r Roots) List(p string) ([]FileInfo, error) {
	real, err := r.resolve(p)
	if err != nil {
		return nil, err
	}
	ents, err := os.ReadDir(real)
	if err != nil {
		return nil, err
	}
	var out []FileInfo
	for _, e := range ents {
		n := strings.ToLower(e.Name())
		hide := false
		for _, s := range secretNames {
			hide = hide || strings.Contains(n, s)
		}
		if hide {
			continue
		}
		fi, err := e.Info()
		if err == nil {
			out = append(out, FileInfo{e.Name(), fi.Size(), e.IsDir()})
		}
	}
	return out, nil
}

// MaxRead bounds ReadFile.
const MaxRead = 1 << 20

func (r Roots) Read(p string, max int64) ([]byte, error) {
	real, err := r.resolve(p)
	if err != nil {
		return nil, err
	}
	fi, err := os.Stat(real)
	if err != nil {
		return nil, err
	}
	if fi.IsDir() {
		return nil, errors.New("is a directory")
	}
	if max <= 0 || max > MaxRead {
		max = MaxRead
	}
	f, err := os.Open(real)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	// a file larger than the cap is read from its end (the interesting part of a log)
	if fi.Size() > max {
		f.Seek(fi.Size()-max, io.SeekStart)
	}
	return io.ReadAll(io.LimitReader(f, max))
}
