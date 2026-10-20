//go:build linux

package mmapcache

import (
	"fmt"
	"sync"
	"sync/atomic"
	"unsafe"
)

const (
	// denseShards is the number of independently locked stores. Callers hash
	// the file offset into a shard, so Put/Get on unrelated offsets rarely
	// contend.
	denseShards = 64

	// denseChunkEntries is the growth granularity of the data/offset arrays.
	// 4096 entries = 128 KiB at 32 B/entry, one chunk is one mmap.
	denseChunkEntries = 4096

	// denseIndexMin is the smallest open-addressing table (power of two).
	denseIndexMin = 64

	// denseIndexOverhead is the per-entry cost charged on top of entrySize:
	// 8 B for the stored offset ([]uint64) + 8 B for the open-addressing
	// index kept at <= 50% load ([]int32 of size 2N).
	denseIndexOverhead = 16
)

// Dense is an entry-indexed variant of Store: entries are packed back to back
// in the order they are first written, and a per-shard open-addressing index
// maps the caller's file offset to its dense entry number. Cost is therefore
// entrySize+16 bytes per cached entry, independent of how scattered the file
// offsets are, instead of one resident page per touch pattern.
//
// The observable contract is Store's: offsets are opaque keys, a Put at an
// already-cached offset updates the value without increasing Count(),
// ForEach yields (offset, data) pairs, and Overflowed() signals the caller to
// flush. Ordering of ForEach output is unspecified and may differ from Store.
//
// Concurrency: Put/Get on any offsets may run concurrently (per-shard mutex).
// ForEach, Clear and Close are NOT safe concurrently with any other method.
type Dense struct {
	shards     [denseShards]denseShard
	entrySize  int
	maxEntries int
	count      atomic.Int64
}

type denseShard struct {
	mu        sync.Mutex
	entrySize int
	data      [][]byte   // chunks of denseChunkEntries*entrySize bytes
	offs      [][]uint64 // chunks of denseChunkEntries offsets
	idx       []int32    // open addressing: 0 empty, else entry number+1
	n         int        // entries stored; they occupy slots [0,n)
}

// NewDense creates a Dense store for entries of the given byte size. maxBytes
// is the flush threshold; it is charged per entry as entrySize+denseIndexOverhead
// so the budget covers the offset table and index too.
func NewDense(entrySize int, maxBytes int64) (*Dense, error) {
	if entrySize <= 0 {
		return nil, fmt.Errorf("mmapcache: entrySize must be positive")
	}

	d := &Dense{
		entrySize:  entrySize,
		maxEntries: int(maxBytes / int64(entrySize+denseIndexOverhead)),
	}
	if d.maxEntries < 0 {
		d.maxEntries = 0
	}

	// The index starts minimal and doubles on demand: sizing it from the
	// budget would leave a mostly-empty table whose random probe slots fault
	// in nearly every page (the density problem this store exists to avoid).
	// Rehashing is amortized, and Clear keeps the grown length so steady
	// state pays for it once.
	for i := range d.shards {
		sh := &d.shards[i]
		sh.entrySize = entrySize
		idx, err := mmapSlice[int32](denseIndexMin)
		if err != nil {
			d.Close()
			return nil, fmt.Errorf("mmapcache: dense index: %w", err)
		}
		sh.idx = idx
	}
	return d, nil
}

// EntrySize returns the fixed size of each entry in bytes.
func (d *Dense) EntrySize() int { return d.entrySize }

// Count returns the total number of cached entries.
func (d *Dense) Count() int { return int(d.count.Load()) }

// Overflowed returns true if the cache has exceeded its memory budget.
func (d *Dense) Overflowed() bool { return d.Count() > d.maxEntries }

func (d *Dense) shardFor(offset int64) *denseShard {
	return &d.shards[denseHash(offset)%denseShards]
}

// denseHash mixes a file offset into a shard/index position. Offsets in this
// cache are 32-byte aligned and often stride by powers of two, so a plain
// modulo would cluster; the final mixes avoid that.
func denseHash(offset int64) uint64 {
	h := uint64(offset) * 0x9E3779B97F4A7C15
	h ^= h >> 31
	h *= 0xBF58476D1CE4E5B9
	h ^= h >> 29
	return h
}

// entry returns the slot's data as a slice aliasing the mmap'd chunk.
// Caller must hold sh.mu.
func (sh *denseShard) entry(slot int) []byte {
	start := (slot % denseChunkEntries) * sh.entrySize
	chunk := sh.data[slot/denseChunkEntries]
	return chunk[start : start+sh.entrySize]
}

// lookup returns the entry number holding offset. Caller must hold sh.mu.
func (sh *denseShard) lookup(offset int64) (int, bool) {
	mask := uint64(len(sh.idx) - 1)
	for i := denseHash(offset) & mask; ; i = (i + 1) & mask {
		v := sh.idx[i]
		if v == 0 {
			return 0, false
		}
		slot := int(v) - 1
		if sh.offs[slot/denseChunkEntries][slot%denseChunkEntries] == uint64(offset) {
			return slot, true
		}
	}
}

// insert publishes slot under offset. Caller must hold sh.mu and have
// verified that the table has room.
func (sh *denseShard) insert(offset int64, slot int) {
	mask := uint64(len(sh.idx) - 1)
	i := denseHash(offset) & mask
	for sh.idx[i] != 0 {
		i = (i + 1) & mask
	}
	sh.idx[i] = int32(slot + 1)
}

// appendChunk grows data/offs by one chunk. Caller must hold sh.mu.
func (sh *denseShard) appendChunk() error {
	data, err := mmapSlice[byte](denseChunkEntries * sh.entrySize)
	if err != nil {
		return err
	}
	offs, err := mmapSlice[uint64](denseChunkEntries)
	if err != nil {
		releaseSlice(data)
		return err
	}
	sh.data = append(sh.data, data)
	sh.offs = append(sh.offs, offs)
	return nil
}

// rehash doubles the index and re-inserts every stored entry. Caller must
// hold sh.mu.
func (sh *denseShard) rehash() error {
	idx, err := mmapSlice[int32](2 * len(sh.idx))
	if err != nil {
		return err
	}
	old := sh.idx
	sh.idx = idx
	for slot := 0; slot < sh.n; slot++ {
		off := sh.offs[slot/denseChunkEntries][slot%denseChunkEntries]
		sh.insert(int64(off), slot)
	}
	// The old table is superseded: unmap it (the new one is twice as big,
	// rehash is rare, so this is not a hot path).
	releaseSlice(old)
	return nil
}

// Get retrieves the data at the given byte offset. Returns nil, false if
// the offset has no cached entry. The returned slice aliases internal storage.
func (d *Dense) Get(offset int64) ([]byte, bool) {
	sh := d.shardFor(offset)
	sh.mu.Lock()
	slot, ok := sh.lookup(offset)
	var out []byte
	if ok {
		out = sh.entry(slot)
	}
	sh.mu.Unlock()
	return out, ok
}

// Put stores data at the given byte offset. len(data) must equal EntrySize().
// Writing an offset that is already cached overwrites the value in place and
// does not change Count().
func (d *Dense) Put(offset int64, data []byte) error {
	if len(data) != d.entrySize {
		return fmt.Errorf("mmapcache: Put: len(data)=%d, want %d", len(data), d.entrySize)
	}

	sh := d.shardFor(offset)
	sh.mu.Lock()
	defer sh.mu.Unlock()

	if slot, ok := sh.lookup(offset); ok {
		// Same-offset rewrite: value only, Count() is unchanged. This is what
		// lets the journal dedupe repeated writes to one node.
		copy(sh.entry(slot), data)
		return nil
	}

	if sh.n == len(sh.data)*denseChunkEntries {
		if err := sh.appendChunk(); err != nil {
			return fmt.Errorf("mmapcache: dense grow: %w", err)
		}
	}
	if 2*(sh.n+1) > len(sh.idx) {
		if err := sh.rehash(); err != nil {
			return fmt.Errorf("mmapcache: dense rehash: %w", err)
		}
	}

	slot := sh.n
	copy(sh.entry(slot), data)
	sh.offs[slot/denseChunkEntries][slot%denseChunkEntries] = uint64(offset)
	sh.insert(offset, slot)
	sh.n++
	d.count.Add(1)
	return nil
}

// ForEach iterates over all cached entries, calling fn for each.
// NOT safe for concurrent use with Put/Get. Visit order is unspecified.
func (d *Dense) ForEach(fn func(offset int64, data []byte)) {
	for i := range d.shards {
		sh := &d.shards[i]
		sh.mu.Lock()
		for slot := 0; slot < sh.n; slot++ {
			off := sh.offs[slot/denseChunkEntries][slot%denseChunkEntries]
			fn(int64(off), sh.entry(slot))
		}
		sh.mu.Unlock()
	}
}

// Clear removes all entries. Chunks and index stay mapped for reuse; their
// resident pages are dropped so the kernel can hand them back to the page
// cache until the next batch of writes faults them in again.
func (d *Dense) Clear() {
	for i := range d.shards {
		sh := &d.shards[i]
		sh.mu.Lock()
		for _, chunk := range sh.data {
			dropSlice(chunk)
		}
		for _, chunk := range sh.offs {
			dropSlice(chunk)
		}
		dropSlice(sh.idx)
		sh.n = 0
		sh.mu.Unlock()
	}
	d.count.Store(0)
}

// Close releases all mmap regions back to the OS.
func (d *Dense) Close() {
	for i := range d.shards {
		sh := &d.shards[i]
		for _, chunk := range sh.data {
			releaseSlice(chunk)
		}
		for _, chunk := range sh.offs {
			releaseSlice(chunk)
		}
		releaseSlice(sh.idx)
		sh.data, sh.offs, sh.idx, sh.n = nil, nil, nil, 0
	}
	d.count.Store(0)
}

// mmapSlice allocates n zeroed elements of T in an anonymous mmap region
// (outside the Go heap, demand-paged).
func mmapSlice[T any](n int) ([]T, error) {
	if n <= 0 {
		return nil, nil
	}
	var zero T
	b, err := mmapAnon(n * int(unsafe.Sizeof(zero)))
	if err != nil {
		return nil, err
	}
	return unsafe.Slice((*T)(unsafe.Pointer(&b[0])), n), nil
}

// releaseSlice munmaps a region returned by mmapSlice.
func releaseSlice[T any](s []T) {
	if len(s) == 0 {
		return
	}
	var zero T
	mmapRelease(unsafe.Slice((*byte)(unsafe.Pointer(&s[0])), len(s)*int(unsafe.Sizeof(zero))))
}

// dropSlice releases the physical pages of a region returned by mmapSlice,
// keeping the mapping. Subsequent reads return zeroes.
func dropSlice[T any](s []T) {
	if len(s) == 0 {
		return
	}
	var zero T
	madviseDontNeed(unsafe.Slice((*byte)(unsafe.Pointer(&s[0])), len(s)*int(unsafe.Sizeof(zero))))
}
