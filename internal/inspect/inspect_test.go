package inspect

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func w(t *testing.T, root, rel, s string) {
	t.Helper()
	p := filepath.Join(root, rel)
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestMemoryPSIModulesSysctl(t *testing.T) {
	d := t.TempDir()
	r := Roots{Proc: d + "/proc", Sys: d + "/sys", Root: d}
	w(t, d, "proc/meminfo", "MemTotal: 975000 kB\nMemAvailable: 400000 kB\nHugePages_Total: 0\n")
	w(t, d, "proc/pressure/memory", "some avg10=0.50 avg60=0.10 avg300=0.00 total=1\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=0\n")
	w(t, d, "proc/modules", "veth 1 0 - Live\nbr_netfilter 2 0 - Live\n")
	w(t, d, "proc/sys/vm/swappiness", "100\n")
	m, _ := r.Memory()
	if m["MemTotalMiB"] != 975000/1024 || m["MemAvailableMiB"] != 390 {
		t.Fatalf("%v", m)
	}
	if p, err := r.PSI(); err != nil || !strings.Contains(p["memory"], "avg10=0.50") {
		t.Fatalf("%v %v", p, err)
	}
	if mods, _ := r.Modules(); len(mods) != 2 || mods[0] != "br_netfilter" {
		t.Fatalf("%v", mods)
	}
	for _, kv := range r.Sysctl() {
		if kv.Key == "vm.swappiness" && kv.Value != "100" {
			t.Fatal(kv)
		}
	}
}

func TestProcessesSortAndLimit(t *testing.T) {
	d := t.TempDir()
	r := Roots{Proc: d + "/proc"}
	// fields after ")" start at state (field 3); utime=idx11, stime=idx12, rss=idx21 in that slice
	stat := func(comm string, ut, rss int) string {
		f := make([]string, 30)
		for i := range f {
			f[i] = "0"
		}
		f[0] = "S"
		f[11] = itoa(ut)
		f[21] = itoa(rss)
		return "1 (" + comm + ") " + strings.Join(f, " ")
	}
	w(t, d, "proc/10/stat", stat("big", 5, 256*10))
	w(t, d, "proc/11/stat", stat("busy", 900, 256))
	w(t, d, "proc/12/stat", stat("my proc)", 1, 256*2))
	w(t, d, "proc/notapid/stat", "x")
	p, _ := r.Processes("rss", 2)
	if len(p) != 2 || p[0].Comm != "big" || p[0].RSSMiB != 10 {
		t.Fatalf("%+v", p)
	}
	p, _ = r.Processes("cpu", 1)
	if p[0].Comm != "busy" {
		t.Fatalf("%+v", p)
	}
	all, _ := r.Processes("rss", 0)
	found := false
	for _, x := range all {
		found = found || x.Comm == "my proc)"
	}
	if !found {
		t.Fatalf("comm with a parenthesis not parsed: %+v", all)
	}
}

func itoa(i int) string {
	return strings.TrimSpace(strings.Replace(string(rune('0'+0)), "0", "", 1) + fmtInt(i))
}
func fmtInt(i int) string {
	if i == 0 {
		return "0"
	}
	s := ""
	for ; i > 0; i /= 10 {
		s = string(rune('0'+i%10)) + s
	}
	return s
}

func TestNetstat(t *testing.T) {
	d := t.TempDir()
	r := Roots{Proc: d + "/proc"}
	// 100.64.0.1:7777 listening -> 0100 40 64 = 0x640040 01 in little endian words: 0100 4064 => "0100 4064" bytes 01 00 40 64? use 0100400A style below
	w(t, d, "proc/net/tcp", "  sl  local_address rem_address   st\n   0: 0100007F:1E61 00000000:0000 0A 0:0 00:0 0 0 0\n   1: 0100007F:0050 0100007F:9999 01 0:0 00:0 0 0 0\n")
	l, _ := r.Netstat()
	if len(l) != 1 || l[0].Addr != "127.0.0.1:7777" {
		t.Fatalf("%+v", l)
	}
}

func TestFileAllowlist(t *testing.T) {
	d := t.TempDir()
	r := Roots{Root: d}
	w(t, d, "var/log/a.log", strings.Repeat("x", 100)+"END")
	w(t, d, "var/lib/edge/k3s-token", "SECRET")
	w(t, d, "var/lib/edge/external-ip", "1.2.3.4\n")
	w(t, d, "etc/shadow", "root:x")
	os.MkdirAll(d+"/etc", 0o755)
	os.Symlink("/etc/shadow", d+"/var/log/sneaky")
	if _, err := r.Read("/etc/shadow", 0); err == nil {
		t.Fatal("outside the allowlist must be refused")
	}
	if _, err := r.Read("/var/lib/edge/k3s-token", 0); err == nil {
		t.Fatal("secret must be refused")
	}
	if _, err := r.Read("/var/log/../../etc/shadow", 0); err == nil {
		t.Fatal("traversal must be refused")
	}
	if _, err := r.Read("relative", 0); err == nil {
		t.Fatal("relative path must be refused")
	}
	if b, err := r.Read("/var/lib/edge/external-ip", 0); err != nil || string(b) != "1.2.3.4\n" {
		t.Fatalf("%q %v", b, err)
	}
	if b, _ := r.Read("/var/log/a.log", 10); string(b) != strings.Repeat("x", 7)+"END" {
		t.Fatalf("tail read: %q", b)
	}
	ls, _ := r.List("/var/lib/edge")
	for _, f := range ls {
		if f.Name == "k3s-token" {
			t.Fatal("secret listed")
		}
	}
	if _, err := r.Read("/var/log/sneaky", 0); err == nil {
		t.Fatal("symlink out of the allowlist must be refused")
	}
}
