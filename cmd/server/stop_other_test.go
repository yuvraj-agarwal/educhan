//go:build !unix

package main

import (
	"os"
	"testing"
)

// stopSignal skips t where os.Process.Signal cannot send SIGTERM, a Unix
// signal: on windows it sends only os.Kill, and plan9, js and wasip1 have no
// Unix signals. A test asks for it before it starts the sweep, so none starts
// one it cannot stop. CI, which fails on a skip, runs on Linux.
func stopSignal(t *testing.T) os.Signal {
	t.Helper()
	t.Skip("needs SIGTERM sent to the sweep, which os.Process.Signal can send only on Unix")
	return nil
}
