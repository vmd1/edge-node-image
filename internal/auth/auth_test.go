package auth

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

func ctxFrom(addr string) context.Context {
	return peer.NewContext(context.Background(), &peer.Peer{Addr: &net.TCPAddr{IP: net.ParseIP(addr), Port: 1234}})
}

func lookup(w WhoIs, err error) Lookup {
	return func(context.Context, string) (WhoIs, error) { return w, err }
}

func TestEmptyConfigRefusesEveryone(t *testing.T) {
	a := NewTailscale(Config{}, lookup(WhoIs{LoginName: "anyone@example.com", Tags: []string{"tag:admin"}}, nil))
	if err := a.Authorize(ctxFrom("100.64.0.1")); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("empty allowlist must deny: %v", err)
	}
}

func TestAllowUserCaseInsensitive(t *testing.T) {
	a := NewTailscale(Config{AllowUsers: []string{"Admin@Example.com"}}, lookup(WhoIs{LoginName: "admin@example.com"}, nil))
	if err := a.Authorize(ctxFrom("100.64.0.1")); err != nil {
		t.Fatal(err)
	}
}

func TestDenyOtherUser(t *testing.T) {
	a := NewTailscale(Config{AllowUsers: []string{"admin@example.com"}}, lookup(WhoIs{LoginName: "other@example.com"}, nil))
	if err := a.Authorize(ctxFrom("100.64.0.1")); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("%v", err)
	}
}

func TestEmptyLoginNeverMatchesEmptyAllowEntry(t *testing.T) {
	a := NewTailscale(Config{AllowUsers: []string{""}}, lookup(WhoIs{LoginName: ""}, nil))
	if err := a.Authorize(ctxFrom("100.64.0.1")); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("a tagged device with no login must not match an empty allow entry: %v", err)
	}
}

func TestAllowTag(t *testing.T) {
	a := NewTailscale(Config{AllowTags: []string{"tag:admin"}}, lookup(WhoIs{Tags: []string{"tag:k3s", "tag:admin"}}, nil))
	if err := a.Authorize(ctxFrom("100.64.0.1")); err != nil {
		t.Fatal(err)
	}
	b := NewTailscale(Config{AllowTags: []string{"tag:admin"}}, lookup(WhoIs{Tags: []string{"tag:k3s"}}, nil))
	if err := b.Authorize(ctxFrom("100.64.0.1")); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("%v", err)
	}
}

func TestLookupFailureDenies(t *testing.T) {
	a := NewTailscale(Config{AllowUsers: []string{"admin@example.com"}}, lookup(WhoIs{}, errors.New("tailscaled down")))
	if err := a.Authorize(ctxFrom("100.64.0.1")); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("fail closed expected: %v", err)
	}
}

func TestNoPeerDenies(t *testing.T) {
	a := NewTailscale(Config{AllowUsers: []string{"admin@example.com"}}, lookup(WhoIs{LoginName: "admin@example.com"}, nil))
	if err := a.Authorize(context.Background()); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("%v", err)
	}
}

func TestLoadConfig(t *testing.T) {
	d := t.TempDir()
	c, err := LoadConfig(filepath.Join(d, "missing.json"))
	if err != nil || len(c.AllowUsers) != 0 || len(c.AllowTags) != 0 {
		t.Fatalf("missing file must yield an empty (deny-all) config: %v %v", c, err)
	}
	p := filepath.Join(d, "noded.json")
	_ = os.WriteFile(p, []byte(`{"allow_users":["a@b.c"],"allow_tags":["tag:x"]}`), 0o600)
	c, err = LoadConfig(p)
	if err != nil || c.AllowUsers[0] != "a@b.c" || c.AllowTags[0] != "tag:x" {
		t.Fatalf("%v %v", c, err)
	}
	_ = os.WriteFile(p, []byte(`{not json`), 0o600)
	if _, err := LoadConfig(p); err == nil {
		t.Fatal("corrupt config must be an error, not silently allow")
	}
}
