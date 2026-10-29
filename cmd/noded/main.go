// noded is the node agent: a typed gRPC API on the Tailscale address, authorized by Tailscale identity.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc"

	"github.com/vmd1/edge-node-image/internal/auth"
	"github.com/vmd1/edge-node-image/internal/server"
)

func main() {
	listen := flag.String("listen", "tailscale:7777", `"tailscale:PORT" (bind to this node's Tailscale IPv4 address)`)
	sock := flag.String("tailscale-socket", "/var/run/tailscale/tailscaled.sock", "tailscaled local API socket (used to learn this node's Tailscale address)")
	flag.Parse()

	var a auth.Authorizer = auth.NewTailnetOnly()
	if d := debugAuthorizer(); d != nil && os.Getenv("NODED_DEBUG_ALLOW_ALL") == "1" {
		log.Print("WARNING: debug build, authorization is disabled")
		a = d
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	addr, err := resolveListen(ctx, *listen, *sock)
	if err != nil {
		log.Fatal(err)
	}
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatal(err)
	}
	g := grpc.NewServer(server.Interceptors(a)...)
	server.New(server.DefaultPaths(), server.ExecRunner{}).Register(g)
	go func() { <-ctx.Done(); g.GracefulStop() }()
	log.Printf("listening on %s", addr)
	if err := g.Serve(lis); err != nil {
		log.Fatal(err)
	}
}

// resolveListen turns "tailscale:PORT" into the node's Tailscale IPv4 address, waiting for tailscaled to be up.
// Any other form is accepted only by the debug build, so a release binary can never listen on a public interface.
func resolveListen(ctx context.Context, spec, sock string) (string, error) {
	port, ok := strings.CutPrefix(spec, "tailscale:")
	if !ok {
		if !debugBuild {
			return "", fmt.Errorf("release build only listens on tailscale:PORT, got %q", spec)
		}
		return spec, nil
	}
	for {
		if ip, err := tailscaleIPv4(ctx, sock); err == nil {
			return net.JoinHostPort(ip, port), nil
		} else {
			log.Printf("waiting for tailscale: %v", err)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

func tailscaleIPv4(ctx context.Context, sock string) (string, error) {
	c := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		}}}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://local-tailscaled.sock/localapi/v0/status?peers=false", nil)
	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var st struct {
		BackendState string
		TailscaleIPs []string
	}
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return "", err
	}
	if st.BackendState != "Running" {
		return "", fmt.Errorf("tailscale state %q", st.BackendState)
	}
	for _, ip := range st.TailscaleIPs {
		if p := net.ParseIP(ip); p != nil && p.To4() != nil {
			return ip, nil
		}
	}
	return "", fmt.Errorf("no tailscale IPv4 address")
}
