//go:build linux

package utreexo

import (
	"encoding/binary"
	"os"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"unsafe"
)

var smapsHeader = regexp.MustCompile(`^([0-9a-f]+)-([0-9a-f]+) (\S+) `)

// TestWALCacheDensity measures candidate A's claim about the WAL mmap cache:
// entries are addressed by file offset, so a block's log2(N) interior writes
// land on pages that hold only a few of their 128 possible entries.
//
// The waste is NOT row spacing: rowBaseOffset shrinks geometrically (capBits
// 34 puts row 0 at 549 GB and row 34 at 64 B, the whole layout inside ~1.1 TB),
// so rows are adjacent-ish. It is intra-row sparseness: row 1 holds 346M slots
// (~11 GB) at mainnet scale while one flush window touches a few hundred
// thousand of them.
//
//	go test -run TestWALCacheDensity -v .
func TestWALCacheDensity(t *testing.T) {
	const (
		blocks       = 40
		addsPerBlock = 2000
		delsPerBlock = 1500
		entrySize    = 32 // forest data entries are hashes
	)

	f, err := OpenForest(t.TempDir(), MaxCacheMemory(64<<30)) // never flushes
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close([32]byte{})
	if err := f.EnterRecordMode(); err != nil {
		t.Fatal(err)
	}

	seed := uint64(0x5EED)
	randHash := func() Hash {
		var h Hash
		for i := 0; i < 4; i++ {
			seed += 0x9E3779B97F4A7C15
			z := seed
			z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
			z = (z ^ (z >> 27)) * 0x94D049BB133111EB
			binary.LittleEndian.PutUint64(h[i*8:], z^(z>>31))
		}
		return h
	}

	live := make([]Hash, 0, blocks*addsPerBlock)
	for b := 0; b < blocks; b++ {
		adds := make([]Hash, addsPerBlock)
		for i := range adds {
			adds[i] = randHash()
		}

		var dels []Hash
		for i := 0; i < delsPerBlock && len(live) > 0; i++ {
			pick := int(seed>>7) % len(live)
			dels = append(dels, live[pick])
			live[pick] = live[len(live)-1]
			live = live[:len(live)-1]
		}

		if _, _, err := f.Record(adds, dels); err != nil {
			t.Fatalf("block %d record: %v", b, err)
		}
		if _, _, _, err := f.RehashAndProve(nil); err != nil {
			t.Fatalf("block %d rehash: %v", b, err)
		}
		live = append(live, adds...)
	}

	cache := f.wal.cached[0].cache
	entries := cache.count()
	if entries == 0 {
		t.Fatal("no cached entries: the WAL cache was not exercised")
	}

	pages := make(map[int64]struct{}, entries)
	var minOff, maxOff int64 = 1 << 62, -1
	cache.forEach(func(off int64, _ []byte) {
		pages[off/4096] = struct{}{}
		if off < minOff {
			minOff = off
		}
		if off > maxOff {
			maxOff = off
		}
	})

	sparseRSS := largestAnonRSS(t)
	densePages, denseRSS := denseReplayRSS(t, entries, entrySize)

	t.Logf("entries=%d  logical=%.1f MiB", entries, float64(entries*entrySize)/(1<<20))
	t.Logf("offset span=%.1f GiB = %.1f rows of 512 GiB",
		float64(maxOff-minOff)/(1<<30), float64(maxOff-minOff)/float64(uint64(1)<<39))
	t.Logf("sparse: pages=%d  entries/page=%.2f  bytes/entry=%.0f  Rss=%.1f MiB",
		len(pages), float64(entries)/float64(len(pages)),
		float64(sparseRSS)/float64(entries), float64(sparseRSS)/(1<<20))
	t.Logf("dense : pages=%d  entries/page=%.2f  Rss=%.1f MiB",
		densePages, float64(entries)/float64(densePages), float64(denseRSS)/(1<<20))
	t.Logf("page waste sparse/dense = %.1fx", float64(len(pages))/float64(densePages))
}

type smapsBlock struct {
	lo, hi uint64
	rss    int64
	anon   int64
}

func readSmaps(t *testing.T) []smapsBlock {
	t.Helper()
	raw, err := os.ReadFile("/proc/self/smaps")
	if err != nil {
		t.Skipf("smaps unavailable: %v", err)
	}

	var out []smapsBlock
	var cur *smapsBlock
	for _, line := range strings.Split(string(raw), "\n") {
		if m := smapsHeader.FindStringSubmatch(line); m != nil {
			lo, _ := strconv.ParseUint(m[1], 16, 64)
			hi, _ := strconv.ParseUint(m[2], 16, 64)
			out = append(out, smapsBlock{lo: lo, hi: hi})
			cur = &out[len(out)-1]
			continue
		}
		if cur == nil {
			continue
		}
		switch {
		case strings.HasPrefix(line, "Rss:"):
			if n, ok := smapsKB(line); ok {
				cur.rss += n
			}
		case strings.HasPrefix(line, "Anonymous:"):
			if n, ok := smapsKB(line); ok {
				cur.anon = n
			}
		}
	}
	return out
}

func smapsKB(line string) (int64, bool) {
	f := strings.Fields(line)
	if len(f) != 3 {
		return 0, false
	}
	n, err := strconv.ParseInt(f[1], 10, 64)
	return n * 1024, err == nil
}

// largestAnonRSS returns the Rss of the biggest anonymous mapping, which is the
// WAL's 1 TB MAP_NORESERVE cache region.
func largestAnonRSS(t *testing.T) int64 {
	t.Helper()
	var best int64
	for _, b := range readSmaps(t) {
		if b.anon > 0 && b.hi-b.lo > (1<<40) && b.rss > best {
			best = b.rss
		}
	}
	return best
}

// denseReplayRSS writes `entries` records back to back into a fresh anon mmap
// and returns its page count and measured Rss: what entry-indexed packing would
// cost for the same number of entries.
func denseReplayRSS(t *testing.T, entries, entrySize int) (int, int64) {
	t.Helper()
	size := ((entries*entrySize + 4095) / 4096) * 4096
	buf, err := syscall.Mmap(-1, 0, size, syscall.PROT_READ|syscall.PROT_WRITE,
		syscall.MAP_ANON|syscall.MAP_PRIVATE)
	if err != nil {
		t.Fatalf("anon mmap: %v", err)
	}
	defer syscall.Munmap(buf)

	for i := 0; i < entries; i++ {
		binary.LittleEndian.PutUint64(buf[i*entrySize:], uint64(i))
	}

	addr := uint64(uintptr(unsafe.Pointer(&buf[0])))
	var rss int64
	for _, b := range readSmaps(t) {
		if addr >= b.lo && addr < b.hi {
			rss = b.rss
		}
	}
	if rss == 0 {
		t.Logf("warning: dense region not found in smaps")
	}
	return size / 4096, rss
}
