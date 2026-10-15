//go:build !linux

package cli

import (
	"os"
	"runtime"
	"syscall"
	"testing"
)

func init() { runtime.LockOSThread() } // TestMain's goroutine keeps the main thread

var onMain = make(chan func())

// mainLoop runs run while the main thread runs what onMain sends it.
func mainLoop(run func() int) int {
	code := make(chan int)
	go func() { code <- run() }()
	for {
		select {
		case f := <-onMain:
			f()
		case c := <-code:
			return c
		}
	}
}

// interruptSelf raises SIGINT from the main thread, with no tgkill here. The kernel gives a process its
// signal on the first thread that takes it, the main thread, which runs the handler before kill returns,
// so the runtime decides between a notified channel and the default action while the caller still waits.
// Raised from another thread, it may land after the CLI's signal.Stop and kill the test binary.
func interruptSelf(*testing.T) {
	done := make(chan struct{})
	onMain <- func() { syscall.Kill(os.Getpid(), syscall.SIGINT); close(done) }
	<-done
}
