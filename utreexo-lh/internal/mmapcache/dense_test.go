//go:build linux

package mmapcache

import (
	"encoding/binary"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/require"
)

func newDenseTestStore(t *testing.T, entrySize int, maxBytes int64) *Dense {
	t.Helper()
	s, err := NewDense(entrySize, maxBytes)
	require.NoError(t, err)
	t.Cleanup(s.Close)
	return s
}

func TestNewDense(t *testing.T) {
	_, err := NewDense(0, 1<<20)
	require.Error(t, err)
	_, err = NewDense(-1, 1<<20)
	require.Error(t, err)

	s := newDenseTestStore(t, 32, 1<<20)
	require.Equal(t, 32, s.EntrySize())
	require.Equal(t, 0, s.Count())
	require.False(t, s.Overflowed())
}

// TestDensePutGet mirrors the sparse store's offset shapes so a dense store
// can be swapped in without behavior changes.
func TestDensePutGet(t *testing.T) {
	const entrySize = 8
	tests := []struct {
		name    string
		offsets []int64
	}{
		{name: "single", offsets: []int64{0}},
		{name: "adjacent", offsets: []int64{0, entrySize, 2 * entrySize}},
		{name: "sparse", offsets: []int64{0, 100 * entrySize, 1000 * entrySize}},
		{name: "large", offsets: []int64{1_000_000 * entrySize}},
		{name: "reverse", offsets: []int64{3 * entrySize, 2 * entrySize, entrySize, 0}},
		{name: "across chunk boundary", offsets: func() []int64 {
			var out []int64
			for _, slot := range []int64{
				denseChunkEntries - 2, denseChunkEntries - 1,
				denseChunkEntries, denseChunkEntries + 1,
			} {
				out = append(out, slot*entrySize)
			}
			return out
		}()},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newDenseTestStore(t, entrySize, 1<<25)
			for i, off := range tt.offsets {
				var buf [entrySize]byte
				binary.LittleEndian.PutUint64(buf[:], uint64(i+1))
				require.NoError(t, s.Put(off, buf[:]))
			}
			for i, off := range tt.offsets {
				got, ok := s.Get(off)
				require.True(t, ok, "Get(%d)", off)
				require.Equal(t, uint64(i+1), binary.LittleEndian.Uint64(got))
			}
			_, ok := s.Get(-entrySize)
			require.False(t, ok, "Get on unwritten offset must miss")
			require.Equal(t, len(tt.offsets), s.Count())
		})
	}
}

func TestDensePutWrongSize(t *testing.T) {
	s := newDenseTestStore(t, 8, 1<<20)
	for _, n := range []int{0, 4, 16} {
		require.Error(t, s.Put(0, make([]byte, n)))
	}
	require.Equal(t, 0, s.Count())
}

// TestDensePutOverwrite is the journal-dedupe contract: a second Put at a
// cached offset replaces the value and does not grow Count().
func TestDensePutOverwrite(t *testing.T) {
	const entrySize = 8
	s := newDenseTestStore(t, entrySize, 1<<20)

	buf := make([]byte, entrySize)
	buf[0] = 0xAA
	require.NoError(t, s.Put(0, buf))
	require.Equal(t, 1, s.Count())

	buf[0] = 0xBB
	require.NoError(t, s.Put(0, buf))
	require.Equal(t, 1, s.Count())

	got, ok := s.Get(0)
	require.True(t, ok)
	require.Equal(t, byte(0xBB), got[0])
}

// TestDenseForEachOnce checks every cached offset is visited exactly once,
// with the value that was written last.
func TestDenseForEachOnce(t *testing.T) {
	const entrySize = 32
	s := newDenseTestStore(t, entrySize, 1<<25)

	want := map[int64]uint64{}
	for i := 0; i < 5000; i++ {
		off := int64((i*7919)%5000) * entrySize // duplicates on purpose
		var buf [entrySize]byte
		binary.LittleEndian.PutUint64(buf[:], uint64(i))
		require.NoError(t, s.Put(off, buf[:]))
		want[off] = uint64(i)
	}
	require.Equal(t, len(want), s.Count())

	seen := map[int64]uint64{}
	s.ForEach(func(off int64, data []byte) {
		require.Len(t, data, entrySize)
		_, dup := seen[off]
		require.False(t, dup, "offset %d visited twice", off)
		seen[off] = binary.LittleEndian.Uint64(data)
	})
	require.Equal(t, want, seen)
}

func TestDenseClear(t *testing.T) {
	const entrySize = 8
	s := newDenseTestStore(t, entrySize, 1<<20)

	for i := 0; i < 100; i++ {
		s.Put(int64(i*entrySize), make([]byte, entrySize))
	}
	s.Clear()

	require.Equal(t, 0, s.Count())
	s.ForEach(func(off int64, _ []byte) {
		t.Fatalf("ForEach visited %d after Clear", off)
	})
	_, ok := s.Get(0)
	require.False(t, ok)

	// Reuse after Clear must work (the index pages were dropped, not freed).
	var buf [entrySize]byte
	binary.LittleEndian.PutUint64(buf[:], 42)
	require.NoError(t, s.Put(0, buf[:]))
	got, ok := s.Get(0)
	require.True(t, ok)
	require.Equal(t, uint64(42), binary.LittleEndian.Uint64(got))
	require.Equal(t, 1, s.Count())
}

// TestDenseBudgetChargesIndex pins the documented budget change: the entry
// budget is maxBytes/(entrySize+16), not maxBytes/entrySize.
func TestDenseBudgetChargesIndex(t *testing.T) {
	const (
		entrySize = 32
		maxBytes  = int64(48 * 1000)
	)
	s := newDenseTestStore(t, entrySize, maxBytes)
	require.Equal(t, 1000, s.maxEntries)

	for i := 0; i < 1000; i++ {
		require.NoError(t, s.Put(int64(i*entrySize), make([]byte, entrySize)))
	}
	require.False(t, s.Overflowed())
	require.NoError(t, s.Put(int64(1000*entrySize), make([]byte, entrySize)))
	require.True(t, s.Overflowed())
}

// TestDenseGrowsPastBudget covers a burst inside one block: the cache must
// keep every entry (no silent drop) even past the budget, and Count() must
// stay exact so the flush sees all of them.
func TestDenseGrowsPastBudget(t *testing.T) {
	const entrySize = 8
	s := newDenseTestStore(t, entrySize, int64(8*10)) // 10 entries budget

	const n = 2 * denseChunkEntries * denseShards // >1 chunk per shard
	for i := 0; i < n; i++ {
		var buf [entrySize]byte
		binary.LittleEndian.PutUint64(buf[:], uint64(i))
		require.NoError(t, s.Put(int64(i)*entrySize, buf[:]))
	}
	require.Equal(t, n, s.Count())
	require.True(t, s.Overflowed())

	for i := 0; i < n; i++ {
		got, ok := s.Get(int64(i) * entrySize)
		require.True(t, ok, "offset %d", i*entrySize)
		require.Equal(t, uint64(i), binary.LittleEndian.Uint64(got))
	}
}

// TestDenseConcurrentShards runs Put and Get from many goroutines on
// disjoint offsets. Run with -race: the per-shard mutex must cover the index,
// the offset table and the data chunks.
func TestDenseConcurrentShards(t *testing.T) {
	const (
		entrySize     = 32
		numGoroutines = 16
		perGoroutine  = 4096
	)

	s := newDenseTestStore(t, entrySize, 1<<30)
	offset := func(g, i int) int64 {
		// Interleave goroutines across offsets so shards are shared.
		return int64(g+i*numGoroutines) * entrySize
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	for g := 0; g < numGoroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < perGoroutine; i++ {
				var buf [entrySize]byte
				binary.LittleEndian.PutUint64(buf[:], uint64(g))
				binary.LittleEndian.PutUint64(buf[8:], uint64(i))
				if err := s.Put(offset(g, i), buf[:]); err != nil {
					t.Error(err)
					return
				}
				// Rewrite the same offset: must not bump Count().
				if err := s.Put(offset(g, i), buf[:]); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	close(start)
	wg.Wait()

	if t.Failed() {
		return
	}
	require.Equal(t, numGoroutines*perGoroutine, s.Count())
	for g := 0; g < numGoroutines; g++ {
		for i := 0; i < perGoroutine; i++ {
			got, ok := s.Get(offset(g, i))
			require.True(t, ok)
			require.Equal(t, uint64(g), binary.LittleEndian.Uint64(got))
			require.Equal(t, uint64(i), binary.LittleEndian.Uint64(got[8:]))
		}
	}
}

// TestDenseRssDensity is the CI-scale guard for dense packing: with
// mainnet-shaped (row-spread) offsets the resident cost must stay at the
// model 32 B data + <=16 B index per entry. It cannot detect the sparse
// store's mainnet regression (at container scale the sparse store is also
// dense, ~1.0x) -- dense_bigmem_test.go does that with 33.5M entries.
func TestDenseRssDensity(t *testing.T) {
	const (
		entrySize = 32
		entries   = 200_000
	)

	s := newDenseTestStore(t, entrySize, 1<<30)
	var data [entrySize]byte
	// Mainnet shape: 1/8 sequential leaves, the rest interior nodes scattered
	// over 24 rows that sit 512 GiB, 256 GiB, ... apart.
	var seed uint64 = 0x5EED
	next := func() uint64 {
		seed += 0x9E3779B97F4A7C15
		z := seed
		z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
		z = (z ^ (z >> 27)) * 0x94D049BB133111EB
		return z ^ (z >> 31)
	}
	for i := 0; i < entries; i++ {
		var off int64
		if i%8 == 0 {
			off = int64(i/8) * entrySize
		} else {
			row := next() % 24
			base := ((int64(1) << 35) - (int64(1) << (34 - row + 1))) * 32
			slots := int64(1) << (34 - row)
			off = base + int64(next()%uint64(slots))*int64(entrySize)
		}
		binary.LittleEndian.PutUint64(data[:], uint64(off))
		require.NoError(t, s.Put(off, data[:]))
	}

	rss := sumMappingRss(t, s.chunkAddrs())
	if rss == 0 {
		t.Skip("smaps unavailable")
	}
	perEntry := float64(rss) / float64(s.Count())
	t.Logf("entries=%d Rss=%.1f MiB bytes/entry=%.1f (model %d)",
		s.Count(), float64(rss)/(1<<20), perEntry, entrySize+denseIndexOverhead)
	require.LessOrEqual(t, perEntry, float64(entrySize+denseIndexOverhead)+16)
	require.Greater(t, perEntry, float64(entrySize)/2)
}

var smapsHeaderRe = regexp.MustCompile(`^([0-9a-f]+)-([0-9a-f]+) \S+ .*$`)

// sumMappingRss returns the total resident bytes of the /proc/self/smaps
// mappings that contain at least one of the given addresses. Each mapping is
// counted once even when several chunks live in the same VMA. Returns 0 when
// smaps is unavailable.
func sumMappingRss(t *testing.T, addrs []unsafe.Pointer) int64 {
	t.Helper()
	raw, err := os.ReadFile("/proc/self/smaps")
	if err != nil {
		return 0
	}

	type mapping struct {
		lo, hi uint64
		rss    int64
	}
	var mappings []mapping
	for _, line := range strings.Split(string(raw), "\n") {
		if m := smapsHeaderRe.FindStringSubmatch(line); m != nil {
			lo, _ := strconv.ParseUint(m[1], 16, 64)
			hi, _ := strconv.ParseUint(m[2], 16, 64)
			mappings = append(mappings, mapping{lo: lo, hi: hi})
			continue
		}
		f := strings.Fields(line)
		if len(f) != 3 || f[0] != "Rss:" || len(mappings) == 0 {
			continue
		}
		n, _ := strconv.ParseInt(f[1], 10, 64)
		mappings[len(mappings)-1].rss += n * 1024
	}

	var total int64
	for _, m := range mappings {
		for _, p := range addrs {
			addr := uint64(uintptr(p))
			if addr >= m.lo && addr < m.hi {
				total += m.rss
				break
			}
		}
	}
	return total
}

// chunkAddrs returns one address per allocated dense chunk/index mapping.
func (d *Dense) chunkAddrs() []unsafe.Pointer {
	var out []unsafe.Pointer
	for i := range d.shards {
		sh := &d.shards[i]
		for _, chunk := range sh.data {
			out = append(out, unsafe.Pointer(&chunk[0]))
		}
		for _, chunk := range sh.offs {
			out = append(out, unsafe.Pointer(&chunk[0]))
		}
		if len(sh.idx) > 0 {
			out = append(out, unsafe.Pointer(&sh.idx[0]))
		}
	}
	return out
}

// The benchmarks below compare the two stores on the two operations the WAL
// cache spends its time in. "seq" offsets keep page-fault cost equal so the
// numbers show pure per-op overhead (hash index + shard mutex vs lock-free
// bitmap CAS); "scatter" uses mainnet-shaped interior offsets, where the
// sparse store additionally pays one fault per touched page.

func benchOffsets(n int, scatter bool, entrySize int) []int64 {
	offsets := make([]int64, n)
	var seed uint64 = 0x5EED
	for i := range offsets {
		if !scatter {
			offsets[i] = int64(i) * int64(entrySize)
			continue
		}
		seed += 0x9E3779B97F4A7C15
		z := seed
		z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
		z = (z ^ (z >> 27)) * 0x94D049BB133111EB
		z ^= z >> 31
		// forest.go:rowBaseOffset shape (capBits=34): rows shrink geometrically
		// so the whole forest spans just under 1 TiB.
		row := z % 24
		base := ((int64(1) << 35) - (int64(1) << (34 - row + 1))) * 32
		slots := int64(1) << (34 - row) // row r spans 2^(34-r) slots
		offsets[i] = base + int64((z>>8)%uint64(slots))*int64(entrySize)
	}
	return offsets
}

func benchPut(b *testing.B, put func(int64, []byte) error, offsets []int64, entrySize int) {
	data := make([]byte, entrySize)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := put(offsets[i%len(offsets)], data); err != nil {
			b.Fatal(err)
		}
	}
}

func benchGet(b *testing.B, get func(int64) ([]byte, bool), offsets []int64) {
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, ok := get(offsets[i%len(offsets)]); !ok {
			b.Fatal("miss")
		}
	}
}

const benchEntrySize = 32
const benchSlots = 1 << 16

func newBenchDense(b *testing.B) *Dense {
	b.Helper()
	s, err := NewDense(benchEntrySize, 1<<38)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(s.Close)
	return s
}

func newBenchStore(b *testing.B) *Store {
	b.Helper()
	s, err := New(benchEntrySize, 1<<38)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(s.Close)
	return s
}

func BenchmarkDensePutSeq(b *testing.B) {
	s := newBenchDense(b)
	benchPut(b, s.Put, benchOffsets(benchSlots, false, benchEntrySize), benchEntrySize)
}

func BenchmarkStorePutSeq(b *testing.B) {
	s := newBenchStore(b)
	benchPut(b, s.Put, benchOffsets(benchSlots, false, benchEntrySize), benchEntrySize)
}

func BenchmarkDensePutScatter(b *testing.B) {
	s := newBenchDense(b)
	benchPut(b, s.Put, benchOffsets(benchSlots, true, benchEntrySize), benchEntrySize)
}

func BenchmarkStorePutScatter(b *testing.B) {
	s := newBenchStore(b)
	benchPut(b, s.Put, benchOffsets(benchSlots, true, benchEntrySize), benchEntrySize)
}

func benchGetPrepare(b *testing.B, prepare func(int64), offsets []int64) {
	b.Helper()
	for _, off := range offsets {
		prepare(off)
	}
}

func BenchmarkDenseGetSeq(b *testing.B) {
	s := newBenchDense(b)
	offsets := benchOffsets(benchSlots, false, benchEntrySize)
	benchGetPrepare(b, func(off int64) { _ = s.Put(off, make([]byte, benchEntrySize)) }, offsets)
	benchGet(b, s.Get, offsets)
}

func BenchmarkStoreGetSeq(b *testing.B) {
	s := newBenchStore(b)
	offsets := benchOffsets(benchSlots, false, benchEntrySize)
	benchGetPrepare(b, func(off int64) { _ = s.Put(off, make([]byte, benchEntrySize)) }, offsets)
	benchGet(b, s.Get, offsets)
}
