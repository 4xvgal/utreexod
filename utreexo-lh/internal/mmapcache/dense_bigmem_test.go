//go:build linux && bigmem

package mmapcache

import (
	"encoding/binary"
	"testing"
)

// TestDenseRssMainnetShape is the RSS guard for dense packing, kept out of the
// normal suite because it needs ~2 GB and a minute of scattered writes.
//
//	go test -tags=bigmem -run TestDenseRssMainnetShape -v ./internal/mmapcache
//
// Offsets are mainnet-shaped: leaves arrive sequentially (row 0) and each
// deletion walks 24 ancestors spread over the higher rows, which is exactly
// what made the offset-addressed store cost ~100 B/entry on mainnet. Dense
// packing must stay at the model 32 B/entry (+16 B index) regardless.
func TestDenseRssMainnetShape(t *testing.T) {
	const (
		entrySize     = 32
		targetEntries = 33_500_000 // mainnet cache size at flush time
		leafCap       = uint64(1) << 28
	)

	s, err := NewDense(entrySize, 1<<38)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	var seed uint64 = 0x5EED
	next := func() uint64 {
		seed += 0x9E3779B97F4A7C15
		z := seed
		z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
		z = (z ^ (z >> 27)) * 0x94D049BB133111EB
		return z ^ (z >> 31)
	}

	// Row bases follow forest.go:rowBaseOffset (capBits=34); consecutive rows
	// are 512 GiB, 256 GiB, ... apart, so the whole forest fits in ~1.1 TB.
	const capBits = 34
	rowBase := func(row uint8) int64 {
		return ((int64(1) << (capBits + 1)) - (int64(1) << (capBits - row + 1))) * 32
	}
	rowStart := func(row uint8) uint64 {
		if row == 0 {
			return 0
		}
		return (uint64(1)<<row - 1) << (64 - row)
	}
	offsetOf := func(pos uint64) int64 {
		var row uint8
		for marker := uint64(1) << 63; pos&marker != 0; marker >>= 1 {
			row++
		}
		return rowBase(row) + int64(pos-rowStart(row))*32
	}
	parent := func(pos uint64) uint64 { return (pos >> 1) | (uint64(1) << 63) }

	var data [entrySize]byte
	writes := 0
	put := func(off int64) {
		binary.LittleEndian.PutUint64(data[:], uint64(off))
		if err := s.Put(off, data[:]); err != nil {
			t.Fatalf("put: %v", err)
		}
		writes++
	}

	// Leaves arrive sequentially (row 0); interior writes walk up from random
	// past leaves, which is what scatters the offsets over the higher rows.
	// Loop on the entry count, not the write count: repeated offsets dedupe.
	var nextLeaf uint64
	for s.Count() < targetEntries && nextLeaf < leafCap {
		for i := 0; i < 2 && s.Count() < targetEntries; i++ {
			put(offsetOf(nextLeaf))
			nextLeaf++
		}
		p := next() % nextLeaf
		for row := 0; row < 8; row++ {
			p = parent(p)
		}
		put(offsetOf(p))
	}

	entries := s.Count()
	rss := sumMappingRss(t, s.chunkAddrs())
	if rss == 0 {
		t.Skip("smaps unavailable")
	}
	perEntry := float64(rss) / float64(entries)
	t.Logf("writes=%d entries=%d Rss=%.2f GiB bytes/entry=%.1f (model %d)",
		writes, entries, float64(rss)/(1<<30), perEntry, entrySize+denseIndexOverhead)

	if perEntry > 64 {
		t.Fatalf("dense cache costs %.1f B/entry, want <= 64", perEntry)
	}
}
