//go:build linux

package media

import (
	"syscall"
	"time"
)

// threadCPU is the CPU time, user and system, the calling thread has used:
// for a test that has locked its goroutine to its thread
// (runtime.LockOSThread), what it has done itself, and none of what the rest
// of the process, the garbage collector's workers among it, has.
func threadCPU() time.Duration {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_THREAD, &ru); err != nil {
		panic(err)
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}
