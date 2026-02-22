//go:build plan9

package main

import (
	"context"
	"log"
	"path/filepath"

	acmelsp "9fans.net/acme-lsp/internal/lsp/acmelsp"
	"9fans.net/go/plan9/srv9p"
)

// runP9FS posts the 9P service to /srv/<base(srvPath)> and serves it on the
// resulting pipe.  The Plan 9 kernel multiplexes client connections for us.
// Blocks until ctx is cancelled or the pipe fails.
func runP9FS(ctx context.Context, srvPath string, ss *acmelsp.ServerSet, fm *acmelsp.FileManager) {
	rw, err := srv9p.Post(filepath.Base(srvPath))
	if err != nil {
		log.Printf("acme-lsp: 9P filesystem unavailable: %v", err)
		return
	}
	go func() {
		<-ctx.Done()
		rw.Close()
	}()
	acmelsp.ServeP9FSConn(ctx, rw, ss, fm)
}
