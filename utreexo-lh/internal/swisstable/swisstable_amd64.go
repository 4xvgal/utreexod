//go:build unix && amd64

package swisstable

// matchH2 finds slots matching h2 using SIMD (SSE2).
// Returns a 16-bit mask where bit i is set if ctrl[i] == h2.
//
//go:noescape
func matchH2(ctrl []byte, h2 byte) uint16

// matchEmpty finds empty slots using SIMD (SSE2).
// Returns a 16-bit mask where bit i is set if ctrl[i] == 0.
//
//go:noescape
func matchEmpty(ctrl []byte) uint16

// matchEmptyOrDeleted finds empty or deleted slots using SIMD (SSE2).
// Returns a 16-bit mask where bit i is set if ctrl[i] has high bit = 0.
//
//go:noescape
func matchEmptyOrDeleted(ctrl []byte) uint16
