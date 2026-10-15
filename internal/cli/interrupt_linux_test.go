//go:build linux

package cli

import (
	"runtime"
	"syscall"
	"testing"
)

func mainLoop(run func() int) int { return run() }

// interruptSelf raises SIGINT on the calling thread. The kernel runs the handler before tgkill returns, so
// the runtime decides between a notified channel and the default action now, while the caller still waits.
// A process-wide kill lands on the main thread whenever the scheduler runs it, possibly after the CLI's
// signal.Stop, and then the default action kills the test binary.
func interruptSelf(*testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	syscall.Tgkill(syscall.Getpid(), syscall.Gettid(), syscall.SIGINT)
}
