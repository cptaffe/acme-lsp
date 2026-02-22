//go:build !plan9

package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"

	"golang.org/x/sys/unix"
)

// listenP9FS removes any stale socket at srvPath, forks 9pserve to announce
// a unix socket there, and returns the server end of the socketpair.
// 9pserve multiplexes client connections onto the pipe; we serve 9P on our
// end.  The returned cleanup function kills 9pserve and closes the pipe; call
// it when ctx is cancelled.
func listenP9FS(srvPath string) (io.ReadWriteCloser, func(), error) {
	os.Remove(srvPath)

	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("socketpair: %w", err)
	}
	// Set FD_CLOEXEC on both ends so they are closed on exec.  The child
	// end's copies (dup2'd onto stdin/stdout by exec.Cmd) survive because
	// dup2 clears FD_CLOEXEC on the destination.
	unix.CloseOnExec(fds[0])
	unix.CloseOnExec(fds[1])

	parent := os.NewFile(uintptr(fds[0]), "acme-lsp-srv")
	child := os.NewFile(uintptr(fds[1]), "acme-lsp-9pserve")

	cmd := exec.Command("9pserve", "unix!"+srvPath)
	cmd.Stdin = child
	cmd.Stdout = child
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		parent.Close()
		child.Close()
		return nil, nil, fmt.Errorf("9pserve: %w", err)
	}
	child.Close()

	cleanup := func() {
		parent.Close()
		cmd.Wait() //nolint:errcheck
	}
	return parent, cleanup, nil
}
