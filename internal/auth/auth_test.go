package auth

import (
	"context"
	"net"
	"net/netip"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

func from(addr string) context.Context {
	ap := netip.MustParseAddrPort(addr)
	return peer.NewContext(context.Background(), &peer.Peer{Addr: net.TCPAddrFromAddrPort(ap)})
}

var self = []netip.Addr{netip.MustParseAddr("100.101.102.103"), netip.MustParseAddr("10.0.2.15"), netip.MustParseAddr("127.0.0.1")}

func denied(err error) bool { return status.Code(err) == codes.PermissionDenied }

func TestAcceptsOtherTailnetPeers(t *testing.T) {
	a := NewTailnetOnlyWithLocal(self)
	for _, addr := range []string{"100.64.0.1:5000", "100.118.170.5:41000", "100.127.255.254:1", "[fd7a:115c:a1e0::1234]:5000"} {
		if err := a.Authorize(from(addr)); err != nil {
			t.Errorf("%s should be accepted: %v", addr, err)
		}
	}
}

func TestRefusesNonTailnetSources(t *testing.T) {
	a := NewTailnetOnlyWithLocal(self)
	for _, addr := range []string{
		"10.42.0.17:5000",     // a pod
		"10.0.2.2:5000",       // a private LAN address
		"192.168.1.10:5000",   // home LAN
		"127.0.0.1:5000",      // loopback
		"8.8.8.8:5000",        // the internet
		"100.63.255.255:5000", // just below the tailnet range
		"100.128.0.0:5000",    // just above it
		"[::1]:5000",
		"[fe80::1]:5000",
	} {
		if err := a.Authorize(from(addr)); !denied(err) {
			t.Errorf("%s must be refused, got %v", addr, err)
		}
	}
}

func TestRefusesTheNodeItself(t *testing.T) {
	a := NewTailnetOnlyWithLocal(self)
	// a host-network pod or local process connecting to the node's own Tailscale address arrives from that same address
	if err := a.Authorize(from("100.101.102.103:44444")); !denied(err) {
		t.Fatalf("the node's own Tailscale address must be refused: %v", err)
	}
}

func TestIPv4MappedIPv6IsHandled(t *testing.T) {
	a := NewTailnetOnlyWithLocal(self)
	if err := a.Authorize(from("[::ffff:100.64.0.9]:5000")); err != nil {
		t.Errorf("a mapped tailnet address is a tailnet address: %v", err)
	}
	if err := a.Authorize(from("[::ffff:10.42.0.9]:5000")); !denied(err) {
		t.Errorf("a mapped pod address must be refused: %v", err)
	}
	if err := a.Authorize(from("[::ffff:100.101.102.103]:5000")); !denied(err) {
		t.Errorf("a mapped own address must be refused: %v", err)
	}
}

func TestNoPeerOrJunkIsRefused(t *testing.T) {
	a := NewTailnetOnlyWithLocal(self)
	if err := a.Authorize(context.Background()); !denied(err) {
		t.Errorf("no peer: %v", err)
	}
	junk := peer.NewContext(context.Background(), &peer.Peer{Addr: &net.UnixAddr{Name: "/tmp/x", Net: "unix"}})
	if err := a.Authorize(junk); !denied(err) {
		t.Errorf("unix socket peer: %v", err)
	}
}

func TestFailsClosedWhenLocalAddressesUnknown(t *testing.T) {
	a := &tailnetOnly{localAddrs: func() ([]netip.Addr, error) { return nil, net.ErrClosed }}
	if err := a.Authorize(from("100.64.0.1:5000")); !denied(err) {
		t.Errorf("must fail closed: %v", err)
	}
}

func TestRealInterfaceAddressesIncludeLoopback(t *testing.T) {
	as, err := interfaceAddrs()
	if err != nil || len(as) == 0 {
		t.Fatalf("%v %v", as, err)
	}
	found := false
	for _, a := range as {
		if a.IsLoopback() {
			found = true
		}
	}
	if !found {
		t.Error("expected a loopback address among the node's own addresses")
	}
}
