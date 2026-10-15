package cli

import (
	"os"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

// openPTY opens a pseudo-terminal: its master, and the terminal end ccswap would read.
func openPTY(t *testing.T) (master, tty *os.File) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { master.Close() })
	fd := master.Fd()
	var name [128]byte
	for _, req := range []uint{unix.TIOCPTYGRANT, unix.TIOCPTYUNLK} {
		if err := unix.IoctlSetInt(int(fd), req, 0); err != nil {
			t.Fatal(err)
		}
	}
	//lint:ignore SA1019 x/sys has no wrapper for an ioctl that fills a buffer, as TIOCPTYGNAME does
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, fd, unix.TIOCPTYGNAME, uintptr(unsafe.Pointer(&name[0]))); e != 0 {
		t.Fatal(e)
	}
	tty, err = os.OpenFile(unix.ByteSliceToString(name[:]), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tty.Close() })
	return master, tty
}

// queued is how many bytes of whole lines the terminal f holds unread.
func queued(t *testing.T, f *os.File) int {
	t.Helper()
	n, err := unix.IoctlGetInt(int(f.Fd()), 0x4004667f) // FIONREAD; x/sys has no name for it on darwin
	if err != nil {
		t.Fatal(err)
	}
	return n
}
