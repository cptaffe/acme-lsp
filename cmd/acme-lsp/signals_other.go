//go:build !plan9

package main

import (
	"os"
	"syscall"
)

// shutdownSignals are the OS signals that trigger a clean exit.
// SIGTERM is included for launchd/systemd service managers.
var shutdownSignals = []os.Signal{os.Interrupt, syscall.SIGTERM}
