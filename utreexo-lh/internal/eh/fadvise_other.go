//go:build !linux

package eh

import "os"

func adviseDontNeed(*os.File) {}
