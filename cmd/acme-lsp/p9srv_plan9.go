//go:build plan9

package main

import (
	"fmt"
	"io"
	"path/filepath"

	"9fans.net/go/plan9/srv9p"
)

// listenP9FS posts the service to /srv/<base(srvPath)> and returns the server
// end of the pipe for use as both directions in fs.Build().Serve.
func listenP9FS(srvPath string) (io.ReadWriteCloser, func(), error) {
	rw, err := srv9p.Post(filepath.Base(srvPath))
	if err != nil {
		return nil, nil, fmt.Errorf("post %s: %w", srvPath, err)
	}
	return rw, func() { rw.Close() }, nil
}
