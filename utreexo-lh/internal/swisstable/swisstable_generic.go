//go:build unix && !amd64

package swisstable

// matchH2 is the generic fallback (no SIMD).
func matchH2(ctrl []byte, h2 byte) uint16 {
	var mask uint16
	for i, c := range ctrl {
		if c == h2 {
			mask |= 1 << i
		}
	}
	return mask
}

// matchEmpty is the generic fallback.
func matchEmpty(ctrl []byte) uint16 {
	var mask uint16
	for i, c := range ctrl {
		if c == ctrlEmpty {
			mask |= 1 << i
		}
	}
	return mask
}

// matchEmptyOrDeleted is the generic fallback.
func matchEmptyOrDeleted(ctrl []byte) uint16 {
	var mask uint16
	for i, c := range ctrl {
		if c&ctrlFull == 0 { // empty and deleted both have high bit = 0
			mask |= 1 << i
		}
	}
	return mask
}
