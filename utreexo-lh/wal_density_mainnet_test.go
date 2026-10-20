//go:build linux

package utreexo

import (
	"encoding/binary"
	"testing"

	"github.com/utreexo/utreexo/internal/mmapcache"
)

// forest layout, mirroring prof/density.py and forest.go:rowBaseOffset.
const (
	densityFR      = 63
	densityCapBits = 34
	densityMask64  = ^uint64(0)
	densityPage    = 4096
)

func densityDetectRow(pos uint64) uint8 {
	marker := uint64(1) << densityFR
	var row uint8
	for pos&marker != 0 {
		row++
		marker >>= 1
	}
	return row
}

func densityRowBase(row uint8) int64 {
	return ((int64(1) << (densityCapBits + 1)) - (int64(1) << (densityCapBits - row + 1))) * 32
}

func densityStartPos(row uint8) uint64 {
	if row == 0 {
		return 0
	}
	// (2<<63) - (2<<(63-row)) taken mod 2^64, i.e. the wraparound constant
	// prof/density.py gets from Python's big integers.
	return (uint64(1)<<row - 1) << (64 - row)
}

func densityOffsetOf(pos uint64) int64 {
	row := densityDetectRow(pos)
	return densityRowBase(row) + int64((pos-densityStartPos(row))&densityMask64)*32
}

func densityParent(pos uint64) uint64 { return (pos >> 1) | (uint64(1) << densityFR) }

// TestWALCacheDensityMainnetShape verifies candidate A on the real mmapcache
// with mainnet-shaped offsets: leaves appended sequentially (row 0) and each
// deletion walking ~24 ancestors whose offsets sit in other rows, 512 GiB
// apart. It measures the resident bytes of the 1 TB anon cache region and
// compares with dense (entry-indexed) packing of the same entry count.
//
//	go test -run TestWALCacheDensityMainnetShape -v .
func TestWALCacheDensityMainnetShape(t *testing.T) {
	const (
		leafPositions = uint64(1) << 28 // 268M leaves: mainnet-ish row depth
		addsPerBlock  = 2000
		delsPerBlock  = 1500
		targetEntries = 1_700_000
		entrySize     = 32
	)

	cache, err := mmapcache.New(entrySize, 64<<30)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()

	var seed uint64 = 0x5EED
	next := func() uint64 {
		seed += 0x9E3779B97F4A7C15
		z := seed
		z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
		z = (z ^ (z >> 27)) * 0x94D049BB133111EB
		return z ^ (z >> 31)
	}

	var data [entrySize]byte
	written := 0
	var minOff, maxOff int64 = 1 << 62, -1
	put := func(off int64) {
		binary.LittleEndian.PutUint64(data[:], uint64(off))
		if err := cache.Put(off, data[:]); err != nil {
			t.Fatalf("put: %v", err)
		}
		written++
		if off < minOff {
			minOff = off
		}
		if off > maxOff {
			maxOff = off
		}
	}

	// Leaf positions arrive in order starting at the forest's row-0 base offset
	// (2<<63 - 2<<63 == 0), so leaf offsets are dense. Deletions pick random
	// older leaves and walk their ancestors, which is what scatters the writes.
	var nextLeaf uint64
	old := make([]uint64, 0, 1<<20)
	for written < targetEntries {
		for i := 0; i < addsPerBlock; i++ {
			put(densityOffsetOf(nextLeaf))
			old = append(old, nextLeaf)
			nextLeaf++
		}
		for i := 0; i < delsPerBlock && len(old) > 0; i++ {
			// Each UTXO is spent at most once, so remove it: re-picking the same
			// leaf would rewrite the same ancestor offsets and hide the scatter.
			k := next() % uint64(len(old))
			p := old[k]
			old[k] = old[len(old)-1]
			old = old[:len(old)-1]
			for row := 0; row < 24; row++ {
				p = densityParent(p)
				put(densityOffsetOf(p))
			}
		}
	}

	t.Logf("writes=%d distinct offsets=%d (each UTXO spent once)", written, cache.Count())
	entries := cache.Count()
	sparseRSS := largestAnonRSS(t)

	pages := make(map[int64]struct{}, entries)
	cache.ForEach(func(off int64, _ []byte) {
		pages[off/densityPage] = struct{}{}
	})

	densePages, denseRSS := denseReplayRSS(t, entries, entrySize)

	t.Logf("entries=%d  logical=%.1f MiB", entries, float64(entries*entrySize)/(1<<20))
	t.Logf("offset span=%.1f GiB", float64(maxOff-minOff)/(1<<30))
	t.Logf("sparse: pages=%d  entries/page=%.2f  bytes/entry=%.0f  Rss=%.1f MiB",
		len(pages), float64(entries)/float64(len(pages)),
		float64(sparseRSS)/float64(entries), float64(sparseRSS)/(1<<20))
	t.Logf("dense : pages=%d  entries/page=%.2f  Rss=%.1f MiB",
		densePages, float64(entries)/float64(densePages), float64(denseRSS)/(1<<20))
	t.Logf("page waste sparse/dense = %.1fx  (candidate A reclaims %.2f GiB)",
		float64(len(pages))/float64(densePages),
		float64(sparseRSS-denseRSS)/(1<<30))
}
