//go:build !linux

package media

import "time"

var testsStarted = time.Now()

// threadCPU stands in, where there is no clock of a thread's CPU time to
// read, with the time on the wall clock since the tests started: what a test
// comparing two pieces of work done one after the other measures as well on
// a machine with nothing else to do.
func threadCPU() time.Duration { return time.Since(testsStarted) }
