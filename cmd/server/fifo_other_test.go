//go:build !(linux || darwin || freebsd || netbsd || openbsd || dragonfly)

package main

import (
	"errors"
	"os"
	"testing"
)

// makeFIFO skips t where Go's syscall package has no Mkfifo to make the FIFO
// with: aix, illumos, solaris, plan9, windows, js and wasip1. CI, which fails
// on a skip, runs on Linux.
func makeFIFO(t *testing.T, _ string) {
	t.Helper()
	t.Skip("needs syscall.Mkfifo, which Go has only on Linux, Darwin and the BSDs")
}

// openFIFOWriter is never reached: makeFIFO has skipped the test first.
func openFIFOWriter(string) (*os.File, error) {
	return nil, errors.New("no syscall.Mkfifo, so no FIFO to open")
}
