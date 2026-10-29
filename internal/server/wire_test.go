package server

import (
	"context"
	"net"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	pb "github.com/vmd1/edge-node-image/gen/nodedv1"
	"github.com/vmd1/edge-node-image/internal/auth"
)

// serve starts the real gRPC server with the real interceptors on an in-memory listener.
func serve(t *testing.T, a auth.Authorizer) (pb.NodedClient, *fakeRunner) {
	t.Helper()
	s, r, _ := fixture(t)
	lis := bufconn.Listen(1 << 20)
	g := grpc.NewServer(Interceptors(a)...)
	s.Register(g)
	go func() { _ = g.Serve(lis) }()
	t.Cleanup(g.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return pb.NewNodedClient(conn), r
}

type denyAll struct{}

func (denyAll) Authorize(context.Context) error { return status.Error(codes.PermissionDenied, "nope") }

type allowAll struct{}

func (allowAll) Authorize(context.Context) error { return nil }

func TestWireDeniedCallsNeverRunAnything(t *testing.T) {
	c, r := serve(t, denyAll{})
	ctx := context.Background()
	if _, err := c.Status(ctx, &pb.StatusRequest{}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("Status: %v", err)
	}
	if _, err := c.Reboot(ctx, &pb.RebootRequest{}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("Reboot: %v", err)
	}
	if _, err := c.StageUpdate(ctx, &pb.StageUpdateRequest{}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("StageUpdate: %v", err)
	}
	if _, err := c.SetBootSlot(ctx, &pb.SetBootSlotRequest{Version: "1.0.0"}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("SetBootSlot: %v", err)
	}
	if _, err := c.SetRegistryConfig(ctx, &pb.RegistryConfig{}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("SetRegistryConfig: %v", err)
	}
	st, err := c.Logs(ctx, &pb.LogsRequest{Unit: "tailscaled.service"})
	if err == nil {
		_, err = st.Recv()
	}
	if status.Code(err) != codes.PermissionDenied {
		t.Errorf("Logs stream must be refused too: %v", err)
	}
	if len(r.calls) != 0 {
		t.Fatalf("a denied caller must not cause any command to run: %v", r.calls)
	}
}

func TestWireAllowedCallWorksAndStreams(t *testing.T) {
	c, r := serve(t, allowAll{})
	r.stream = []string{"line one", "line two"}
	st, err := c.Status(context.Background(), &pb.StatusRequest{})
	if err != nil || st.ImageVersion != "1.2.3" {
		t.Fatalf("%v %v", st, err)
	}
	ls, err := c.Logs(context.Background(), &pb.LogsRequest{Unit: "noded.service"})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for {
		m, err := ls.Recv()
		if err != nil {
			break
		}
		got = append(got, m.Text)
	}
	if len(got) != 2 || got[0] != "line one" {
		t.Errorf("%v", got)
	}
}

func TestAPIHasNoExecRPC(t *testing.T) {
	for _, m := range pb.Noded_ServiceDesc.Methods {
		switch m.MethodName {
		case "Exec", "Shell", "Run", "Command":
			t.Errorf("API must not contain %s", m.MethodName)
		}
	}
	for _, m := range pb.Noded_ServiceDesc.Streams {
		switch m.StreamName {
		case "Exec", "Shell", "Run", "Command":
			t.Errorf("API must not contain %s", m.StreamName)
		}
	}
}
