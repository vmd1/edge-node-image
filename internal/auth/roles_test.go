package auth

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

type fakeWhois struct {
	id  Identity
	err error
}

func (f fakeWhois) Lookup(context.Context, string) (Identity, error) { return f.id, f.err }

func ctxFrom(addr string) context.Context {
	return peer.NewContext(context.Background(), &peer.Peer{Addr: net.TCPAddrFromAddrPort(netip.MustParseAddrPort(addr))})
}

func policy(w Whois, mode Mode) (*Policy, *[]string) {
	var logs []string
	return &Policy{Whois: w, Mode: func() Mode { return mode }, Logf: func(f string, a ...any) { logs = append(logs, strings.TrimSpace(f)) }}, &logs
}

func TestParseWhoisHighestRoleWins(t *testing.T) {
	j := `{"Node":{"Name":"laptop.tail.ts.net."},"UserProfile":{"LoginName":"me@vmd1.dev"},"CapMap":{"vmd1.dev/cap/noded":[{"role":"reader"},{"role":"operator"}],"other":[{"role":"admin"}]}}`
	id, err := ParseWhois(strings.NewReader(j))
	if err != nil || id.Role != RoleOperator || id.Device != "laptop.tail.ts.net" || id.User != "me@vmd1.dev" {
		t.Fatalf("%+v %v", id, err)
	}
	id, _ = ParseWhois(strings.NewReader(`{"Node":{"Name":"x"},"CapMap":{}}`))
	if id.Role != RoleNone {
		t.Fatal("no grant means no role")
	}
}

func TestEnforceDeniesAboveRole(t *testing.T) {
	p, logs := policy(fakeWhois{id: Identity{Device: "laptop", Role: RoleReader}}, ModeEnforce)
	if _, _, err := p.Check(ctxFrom("100.64.0.9:1"), "Status", RoleReader); err != nil {
		t.Fatalf("reader may read: %v", err)
	}
	_, done, err := p.Check(ctxFrom("100.64.0.9:1"), "Reboot", RoleOperator)
	if status.Code(err) != codes.PermissionDenied || !strings.Contains(err.Error(), "operator") {
		t.Fatalf("reader must not reboot: %v", err)
	}
	_ = done
	if len(*logs) == 0 {
		t.Fatal("denials must be audited")
	}
}

func TestEnforceNoGrantDenied(t *testing.T) {
	p, _ := policy(fakeWhois{id: Identity{Device: "stranger"}}, ModeEnforce)
	if _, _, err := p.Check(ctxFrom("100.64.0.9:1"), "Status", RoleReader); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("no grant must be denied: %v", err)
	}
	p, _ = policy(fakeWhois{err: errors.New("tailscaled down")}, ModeEnforce)
	if _, _, err := p.Check(ctxFrom("100.64.0.9:1"), "Status", RoleReader); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("unidentifiable caller must be denied in enforce mode: %v", err)
	}
}

func TestAuditModeNeverDenies(t *testing.T) {
	p, logs := policy(fakeWhois{id: Identity{Device: "stranger"}}, ModeAudit)
	ctx, done, err := p.Check(ctxFrom("100.64.0.9:1"), "Reboot", RoleOperator)
	if err != nil {
		t.Fatalf("audit mode must not deny: %v", err)
	}
	done(nil)
	if id, _ := FromContext(ctx); id.Device != "stranger" || len(*logs) != 1 || !strings.Contains((*logs)[0], "audit") {
		t.Fatalf("%v %v", id, *logs)
	}
	p, _ = policy(fakeWhois{err: errors.New("down")}, ModeAudit)
	if _, _, err := p.Check(ctxFrom("100.64.0.9:1"), "Status", RoleReader); err != nil {
		t.Fatalf("audit mode tolerates a whois failure: %v", err)
	}
}

func TestRateLimit(t *testing.T) {
	p, _ := policy(fakeWhois{id: Identity{Device: "d", Role: RoleAdmin}}, ModeEnforce)
	now := time.Now()
	p.Now = func() time.Time { return now }
	var denied int
	for i := 0; i < 10; i++ {
		if _, _, err := p.Check(ctxFrom("100.64.0.9:1"), "Reboot", RoleOperator); status.Code(err) == codes.ResourceExhausted {
			denied++
		}
	}
	if denied != 5 { // burst 5 for mutating calls
		t.Fatalf("denied %d of 10", denied)
	}
	now = now.Add(3 * time.Second)
	if _, _, err := p.Check(ctxFrom("100.64.0.9:1"), "Reboot", RoleOperator); err != nil {
		t.Fatalf("bucket should have refilled: %v", err)
	}
	// reads have their own bucket
	if _, _, err := p.Check(ctxFrom("100.64.0.9:1"), "Status", RoleReader); err != nil {
		t.Fatal(err)
	}
}
