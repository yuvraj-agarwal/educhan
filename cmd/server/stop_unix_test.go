//go:build unix

package main

import (
	"os"
	"syscall"
	"testing"
)

// stopSignal is what a test stops a sweep with: SIGTERM, which Cloud Run sends
// a Job it stops. This file is built where os.Process.Signal can send it, on
// Unix, stop_other_test.go everywhere else.
func stopSignal(t *testing.T) os.Signal {
	t.Helper()
	return syscall.SIGTERM
}
