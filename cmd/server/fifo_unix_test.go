//go:build unix

package main

import (
	"syscall"
	"testing"
)

// makeFIFO makes a FIFO at path, which a test reads as a file that is slow to
// come: a read of it waits until something is written.
func makeFIFO(t *testing.T, path string) {
	t.Helper()
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatalf("make the FIFO: %v", err)
	}
}
