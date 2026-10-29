package server

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf16"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/vmd1/edge-node-image/gen/nodedv1"
)

type call struct {
	name string
	args []string
}

type fakeRunner struct {
	calls  []call
	out    map[string]string // "name arg0 arg1..." -> output
	fail   map[string]bool
	stream []string
}

func (f *fakeRunner) key(name string, args []string) string {
	return strings.TrimSpace(name + " " + strings.Join(args, " "))
}
func (f *fakeRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	f.calls = append(f.calls, call{name, args})
	k := f.key(name, args)
	for prefix, out := range f.out {
		if strings.HasPrefix(k, prefix) {
			if f.fail[prefix] {
				return out, fmt.Errorf("exit 1")
			}
			return out, nil
		}
	}
	if f.fail[name] {
		return "", fmt.Errorf("exit 1")
	}
	return "", nil
}
func (f *fakeRunner) Stream(_ context.Context, emit func(string) error, name string, args ...string) error {
	f.calls = append(f.calls, call{name, args})
	for _, l := range f.stream {
		if err := emit(l); err != nil {
			return err
		}
	}
	return nil
}

func fixture(t *testing.T) (*Server, *fakeRunner, Paths) {
	t.Helper()
	d := t.TempDir()
	p := Paths{OSRelease: filepath.Join(d, "os-release"), Proc: filepath.Join(d, "proc"), Run: filepath.Join(d, "run"),
		Var: filepath.Join(d, "var"), EFIVars: filepath.Join(d, "efivars"), K3sUnit: "k3s-agent.service"}
	for _, dir := range []string{filepath.Join(p.Proc, "pressure"), filepath.Join(p.Proc, "sys", "kernel", "random"), p.Run, p.Var, p.EFIVars} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	w := func(path, s string) {
		if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	w(p.OSRelease, "ID=debian\nIMAGE_ID=edge-node\nIMAGE_VERSION=\"1.2.3\"\n")
	w(filepath.Join(p.Proc, "pressure", "memory"), "some avg10=12.50 avg60=3.00 avg300=1.00 total=1\nfull avg10=4.00 avg60=1.00 avg300=0.50 total=1\n")
	w(filepath.Join(p.Proc, "pressure", "io"), "some avg10=0.25 avg60=0.00 avg300=0.00 total=1\n")
	w(filepath.Join(p.Proc, "meminfo"), "MemTotal: 977000 kB\nMemAvailable:  424242 kB\n")
	w(filepath.Join(p.Proc, "uptime"), "3600.55 7000.00\n")
	w(filepath.Join(p.Proc, "sys", "kernel", "random", "boot_id"), "11111111-2222-3333-4444-555555555555\n")
	w(filepath.Join(p.Run, "external-ip"), "203.0.113.7\n")
	w(filepath.Join(p.Run, "available-version"), "1.3.0\n")
	r := &fakeRunner{out: map[string]string{}, fail: map[string]bool{}}
	return New(p, r), r, p
}

func writeEFIString(t *testing.T, path, s string) {
	t.Helper()
	u := utf16.Encode([]rune(s))
	b := []byte{7, 0, 0, 0}
	for _, c := range u {
		b = binary.LittleEndian.AppendUint16(b, c)
	}
	b = append(b, 0, 0)
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestStatus(t *testing.T) {
	s, r, p := fixture(t)
	r.out["systemctl show -p ActiveState --value tailscaled.service"] = "active\n"
	r.out["systemctl show -p ActiveState --value k3s-agent.service"] = "inactive\n"
	writeEFIString(t, filepath.Join(p.EFIVars, "LoaderEntrySelected-"+loaderGUID), "edge-node_1.2.3+2-1.efi")
	st, err := s.Status(context.Background(), &pb.StatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if st.ImageVersion != "1.2.3" {
		t.Errorf("version %q", st.ImageVersion)
	}
	if st.BootedEntry != "edge-node_1.2.3+2-1.efi" || st.BootAssessment != "pending" {
		t.Errorf("entry %q assess %q", st.BootedEntry, st.BootAssessment)
	}
	if st.MemPressureSomeAvg10 != 12.5 || st.IoPressureSomeAvg10 != 0.25 {
		t.Errorf("psi %v %v", st.MemPressureSomeAvg10, st.IoPressureSomeAvg10)
	}
	if st.MemAvailableBytes != 424242*1024 {
		t.Errorf("mem %d", st.MemAvailableBytes)
	}
	if st.BootId != "11111111-2222-3333-4444-555555555555" {
		t.Errorf("boot id %q", st.BootId)
	}
	if st.UptimeSeconds != 3600 || st.ExternalIp != "203.0.113.7" || st.AvailableVersion != "1.3.0" {
		t.Errorf("uptime/ip/available: %d %q %q", st.UptimeSeconds, st.ExternalIp, st.AvailableVersion)
	}
	if st.Units["tailscaled.service"] != "active" || st.Units["k3s-agent.service"] != "inactive" {
		t.Errorf("units %v", st.Units)
	}
}

func TestStatusGoodWhenNoCounter(t *testing.T) {
	s, _, p := fixture(t)
	writeEFIString(t, filepath.Join(p.EFIVars, "LoaderEntrySelected-"+loaderGUID), "edge-node_1.2.3.efi")
	st, _ := s.Status(context.Background(), &pb.StatusRequest{})
	if st.BootAssessment != "good" {
		t.Errorf("assessment %q", st.BootAssessment)
	}
}

func TestStatusWithoutEFI(t *testing.T) {
	s, _, _ := fixture(t)
	st, _ := s.Status(context.Background(), &pb.StatusRequest{})
	if st.BootAssessment != "unknown" || st.BootedEntry != "" {
		t.Errorf("%q %q", st.BootAssessment, st.BootedEntry)
	}
}

type logStream struct {
	pb.Noded_LogsServer
	ctx   context.Context
	lines []string
}

func (l *logStream) Context() context.Context { return l.ctx }
func (l *logStream) Send(m *pb.LogLine) error { l.lines = append(l.lines, m.Text); return nil }

func TestLogs(t *testing.T) {
	s, r, _ := fixture(t)
	r.stream = []string{"a", "b"}
	ls := &logStream{ctx: context.Background()}
	if err := s.Logs(&pb.LogsRequest{Unit: "sshd.service"}, ls); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("sshd must be refused, got %v", err)
	}
	if err := s.Logs(&pb.LogsRequest{Unit: "../../etc/passwd"}, ls); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("path must be refused, got %v", err)
	}
	if len(r.calls) != 0 {
		t.Fatalf("refused requests must not run anything: %v", r.calls)
	}
	if err := s.Logs(&pb.LogsRequest{Unit: "tailscaled.service", TailLines: 999999, Follow: true}, ls); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(r.calls[0].args, " ")
	if !strings.Contains(got, "-n 2000") || !strings.Contains(got, "-u tailscaled.service") || !strings.Contains(got, "-f") {
		t.Errorf("args %q", got)
	}
	if len(ls.lines) != 2 {
		t.Errorf("lines %v", ls.lines)
	}
	r.calls = nil
	if err := s.Logs(&pb.LogsRequest{Unit: "kernel"}, ls); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(r.calls[0].args, " "); !strings.Contains(got, "-k") || strings.Contains(got, "-u") || !strings.Contains(got, "-n 100") {
		t.Errorf("kernel args %q", got)
	}
}

func TestStageUpdate(t *testing.T) {
	s, r, _ := fixture(t)
	for _, bad := range []string{"1.0;reboot", "$(id)", "../x", "a b", "-force", strings.Repeat("a", 100)} {
		if _, err := s.StageUpdate(context.Background(), &pb.StageUpdateRequest{TargetVersion: bad}); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%q should be refused, got %v", bad, err)
		}
	}
	if len(r.calls) != 0 {
		t.Fatalf("nothing may run for refused input: %v", r.calls)
	}
	r.out["/usr/libexec/edge-sysupdate"] = "✓ Successfully installed update '2.0.0'.\n"
	r.out["bootctl list"] = "         id: edge-node_1.0.0.efi\n         id: edge-node_2.0.0+3-0.efi\n"
	resp, err := s.StageUpdate(context.Background(), &pb.StageUpdateRequest{})
	if err != nil || resp.StagedVersion != "2.0.0" {
		t.Fatalf("%v %v", resp, err)
	}
	if got := strings.Join(r.calls[0].args, " "); got != "--no-pager update" {
		t.Errorf("args %q", got)
	}
	r.calls = nil
	if _, err := s.StageUpdate(context.Background(), &pb.StageUpdateRequest{TargetVersion: "2.0.0"}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(r.calls[0].args, " "); got != "--no-pager update 2.0.0" {
		t.Errorf("args %q", got)
	}
	r.fail["/usr/libexec/edge-sysupdate"] = true
	if _, err := s.StageUpdate(context.Background(), &pb.StageUpdateRequest{}); status.Code(err) != codes.Internal {
		t.Errorf("failure must surface: %v", err)
	}
}

func TestStageRefusesAnEntryTheBootloaderCannotUse(t *testing.T) {
	s, r, _ := fixture(t)
	r.out["/usr/libexec/edge-sysupdate"] = "✓ Successfully installed update '2.0.0'.\n"
	// the real failure: the file is on the ESP but unparsable, so bootctl lists only the old entry
	r.out["bootctl list"] = "Failed to parse PE file '/boot/EFI/Linux/edge-node_2.0.0+3-0.efi': Bad message\n         id: edge-node_1.0.0.efi\n"
	if _, err := s.StageUpdate(context.Background(), &pb.StageUpdateRequest{}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("an unusable staged entry must be refused: %v", err)
	}
	// a listed entry for a different version is not good enough
	r.out["bootctl list"] = "         id: edge-node_2.0.0x.efi\n"
	if _, err := s.StageUpdate(context.Background(), &pb.StageUpdateRequest{TargetVersion: "2.0.0"}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("a look-alike entry must not satisfy the check: %v", err)
	}
}

func TestCheckUpdateWritesAvailable(t *testing.T) {
	s, r, p := fixture(t)
	r.out["/usr/libexec/edge-sysupdate"] = "2.0.0\n"
	resp, err := s.CheckUpdate(context.Background(), &pb.CheckUpdateRequest{})
	if err != nil || resp.NewestVersion != "2.0.0" || resp.CurrentVersion != "1.2.3" {
		t.Fatalf("%v %v", resp, err)
	}
	if b, _ := os.ReadFile(filepath.Join(p.Run, "available-version")); strings.TrimSpace(string(b)) != "2.0.0" {
		t.Errorf("available-version %q", b)
	}
	r.fail["/usr/libexec/edge-sysupdate"] = true
	resp, _ = s.CheckUpdate(context.Background(), &pb.CheckUpdateRequest{})
	if resp.NewestVersion != "" {
		t.Errorf("no update must report empty, got %q", resp.NewestVersion)
	}
}

func TestSetBootSlot(t *testing.T) {
	s, r, _ := fixture(t)
	for _, bad := range []string{"", "1.0/../x", "1 2", ";", "-h"} {
		if _, err := s.SetBootSlot(context.Background(), &pb.SetBootSlotRequest{Version: bad}); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%q should be refused", bad)
		}
	}
	if len(r.calls) != 0 {
		t.Fatal("nothing may run for refused input")
	}
	if _, err := s.SetBootSlot(context.Background(), &pb.SetBootSlotRequest{Version: "1.0.0", OneShot: true}); err != nil {
		t.Fatal(err)
	}
	if got := r.calls[0].name + " " + strings.Join(r.calls[0].args, " "); got != "bootctl set-oneshot edge-node_1.0.0.efi" {
		t.Errorf("%q", got)
	}
	r.calls = nil
	_, _ = s.SetBootSlot(context.Background(), &pb.SetBootSlotRequest{Version: "1.0.0"})
	if got := r.calls[0].args[0]; got != "set-default" {
		t.Errorf("%q", got)
	}
}

func TestReboot(t *testing.T) {
	s, r, _ := fixture(t)
	if _, err := s.Reboot(context.Background(), &pb.RebootRequest{}); err != nil {
		t.Fatal(err)
	}
	got := r.calls[0].name + " " + strings.Join(r.calls[0].args, " ")
	if !strings.Contains(got, "systemd-run --on-active=3") || !strings.HasSuffix(got, "systemctl reboot") {
		t.Errorf("%q", got)
	}
}

func TestRegistry(t *testing.T) {
	s, r, p := fixture(t)
	def, _ := s.GetRegistryConfig(context.Background(), &pb.GetRegistryConfigRequest{})
	if def.DockerIoMirror != "https://mirror.gcr.io" || def.DockerIoFallback != "https://registry-1.docker.io" {
		t.Errorf("defaults %v", def)
	}
	for _, bad := range []string{"http://m.example", "file:///etc", "https://u:p@m.example", "https://m.example/?a=b", "mirror.gcr.io"} {
		if _, err := s.SetRegistryConfig(context.Background(), &pb.RegistryConfig{DockerIoMirror: bad}); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%q should be refused: %v", bad, err)
		}
	}
	// k3s unit absent: no restart
	r.fail["systemctl cat"] = true
	r.out["systemctl cat"] = ""
	resp, err := s.SetRegistryConfig(context.Background(), &pb.RegistryConfig{DockerIoMirror: "https://m.example", DockerIoFallback: "https://registry-1.docker.io"})
	if err != nil || resp.K3SAgentRestarted {
		t.Fatalf("%v %v", resp, err)
	}
	y, _ := os.ReadFile(filepath.Join(p.Var, "registries.yaml"))
	want := "mirrors:\n  docker.io:\n    endpoint:\n      - \"https://m.example\"\n      - \"https://registry-1.docker.io\"\n"
	if string(y) != want {
		t.Errorf("yaml %q", y)
	}
	// k3s unit present: restart
	r.fail["systemctl cat"] = false
	r.calls = nil
	resp, err = s.SetRegistryConfig(context.Background(), &pb.RegistryConfig{})
	if err != nil || !resp.K3SAgentRestarted {
		t.Fatalf("%v %v", resp, err)
	}
	if y, _ := os.ReadFile(filepath.Join(p.Var, "registries.yaml")); !strings.HasPrefix(string(y), "# no mirror") {
		t.Errorf("empty mirror must render a comment-only file: %q", y)
	}
	last := r.calls[len(r.calls)-1]
	if last.name != "systemctl" || last.args[0] != "restart" || last.args[1] != "k3s-agent.service" {
		t.Errorf("restart call %v", last)
	}
	got, _ := s.GetRegistryConfig(context.Background(), &pb.GetRegistryConfigRequest{})
	if got.DockerIoMirror != "" {
		t.Errorf("cleared mirror must persist, got %q", got.DockerIoMirror)
	}
}

func TestUpdateConfig(t *testing.T) {
	s, _, p := fixture(t)
	for _, bad := range []string{"ftp://x.example/", "https://u:p@x.example/", "https://x.example/?a=b", "https://x.example/#f", "x.example"} {
		if _, err := s.SetUpdateConfig(context.Background(), &pb.UpdateConfig{UpdateUrl: bad}); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%q should be refused", bad)
		}
	}
	if _, err := s.SetUpdateConfig(context.Background(), &pb.UpdateConfig{UpdateUrl: "https://example.org/releases/stable"}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetUpdateConfig(context.Background(), &pb.GetUpdateConfigRequest{})
	if got.UpdateUrl != "https://example.org/releases/stable/" {
		t.Errorf("trailing slash expected: %q", got.UpdateUrl)
	}
	if _, err := s.SetUpdateConfig(context.Background(), &pb.UpdateConfig{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(p.Var, "update-url")); !os.IsNotExist(err) {
		t.Error("empty URL must remove the override")
	}
	// clearing twice is fine
	if _, err := s.SetUpdateConfig(context.Background(), &pb.UpdateConfig{}); err != nil {
		t.Fatal(err)
	}
}

func TestHTTPUpdateURLOnlyInDebugBuild(t *testing.T) {
	s, _, _ := fixture(t)
	_, err := s.SetUpdateConfig(context.Background(), &pb.UpdateConfig{UpdateUrl: "http://10.0.2.2:8000/"})
	if allowHTTP && err != nil {
		t.Errorf("debug build must allow http: %v", err)
	}
	if !allowHTTP && status.Code(err) != codes.InvalidArgument {
		t.Errorf("release build must refuse http: %v", err)
	}
}

func TestRenderRegistries(t *testing.T) {
	if got := RenderRegistries("https://a.example", ""); got != "mirrors:\n  docker.io:\n    endpoint:\n      - \"https://a.example\"\n" {
		t.Errorf("%q", got)
	}
}
