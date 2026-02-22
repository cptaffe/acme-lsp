//go:build !plan9

package main

import (
	"context"
	"log"

	acmelsp "9fans.net/acme-lsp/internal/lsp/acmelsp"
	"9fans.net/acme-lsp/internal/p9service"
)

// runP9FS listens on a unix socket at srvPath and serves the 9P filesystem.
// Each client connection gets its own 9P conversation.  Blocks until ctx is
// cancelled or the listener fails.
func runP9FS(ctx context.Context, srvPath string, ss *acmelsp.ServerSet, fm *acmelsp.FileManager) {
	ln, err := p9service.Listen(ctx, "unix", srvPath)
	if err != nil {
		log.Printf("acme-lsp: 9P filesystem unavailable: %v", err)
		return
	}
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	acmelsp.ServeP9FS(ctx, ln, ss, fm)
}
