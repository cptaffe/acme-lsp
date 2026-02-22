//go:build plan9

package main

import "os"

// shutdownSignals are the OS signals that trigger a clean exit.
var shutdownSignals = []os.Signal{os.Interrupt}
