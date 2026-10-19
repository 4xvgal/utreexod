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
