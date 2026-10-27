//go:build !unix

package main

import (
	"os"
	"testing"
)

// stopSignal skips t where the sweep cannot be sent the SIGTERM Cloud Run
// sends a Job it stops: on windows os.Process.Signal sends only os.Kill, js and
// wasip1 send no signal to another process, and on plan9 syscall.SIGTERM is the
// "interrupt" note, as SIGINT is, not that signal. A test asks for it before it
// starts the sweep, so none starts one it cannot stop. CI, which fails on a
// skip, runs on Linux.
func stopSignal(t *testing.T) os.Signal {
	t.Helper()
	t.Skip("needs the SIGTERM Cloud Run sends, which the sweep can be sent only on Unix")
	return nil
}
