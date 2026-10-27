//go:build !unix

package main

import "testing"

// makeFIFO skips t: only Unix has FIFOs, and CI, which fails on a skip, runs
// on Linux.
func makeFIFO(t *testing.T, _ string) {
	t.Helper()
	t.Skip("needs a FIFO, which only Unix has")
}
