//go:build !linux

package eh

import "os"

func adviseDontNeed(*os.File) {}

func adviseWillNeed(*os.File, int64, int) {}
