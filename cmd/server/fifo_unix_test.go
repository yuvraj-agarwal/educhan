//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package main

import (
	"os"
	"syscall"
	"testing"
)

// makeFIFO makes a FIFO at path, which a test reads as a file that is slow to
// come: a read of it waits until something is written. This file is built
// where Go's syscall package has Mkfifo, fifo_other_test.go everywhere else.
func makeFIFO(t *testing.T, path string) {
	t.Helper()
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatalf("make the FIFO: %v", err)
	}
}

// openFIFOWriter opens the FIFO at path to write without waiting, so it fails
// until something has the FIFO open to read.
func openFIFOWriter(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0)
}
