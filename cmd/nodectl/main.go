// nodectl is the client for noded (see package nodectl). The vmd1 CLI embeds the same code as `vmd1 node`.
package main

import (
	"context"
	"os"
	"os/signal"

	"github.com/vmd1/edge-node-image/nodectl"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	code := nodectl.Main(ctx, "nodectl", os.Args[1:])
	stop()
	os.Exit(code)
}
