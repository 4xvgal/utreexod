//go:build linux

package eh

import (
	"os"
	"syscall"
)

func adviseDontNeed(f *os.File) {
	if f == nil {
		return
	}
	const fadvDontNeed = 4
	syscall.Syscall6(syscall.SYS_FADVISE64, f.Fd(), 0, 0, fadvDontNeed, 0, 0)
}

// adviseWillNeed asks the kernel to start reading [off, off+n) now, so a later
// mmap fault finds the page resident instead of stalling on it.
func adviseWillNeed(f *os.File, off int64, n int) {
	if f == nil {
		return
	}
	const fadvWillNeed = 3
	syscall.Syscall6(syscall.SYS_FADVISE64, f.Fd(), uintptr(off), uintptr(n), fadvWillNeed, 0, 0)
}
