//go:build linux

package utreexo

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"syscall"

	"github.com/utreexo/utreexo/internal/swisstable"
)

// Linear hashing position map, v3 (key-prefix embedded).
//
// The table lives in one mmap'd file: a 64-byte header followed by fixed-size
// buckets. Each entry stores the packed value plus a 12-byte prefix of the key,
// so the map confirms a hit by itself and never reads the data file. This
// removes verifyHash (a random forest_data read per lookup), at the cost of
// 20 bytes/entry instead of ~13.
//
// The prefix also lets split() re-place entries using the stored key bytes, so
// the data file is not touched during growth either. A 12-byte (96-bit) prefix
// is enough: 8 bytes would collide ~16 times at 2^34 live entries, while 96
// bits keeps the table-wide collision expectation at 2^-17 for 2^40 live
// entries and 2^-33 for the 2^31.5 of the assume-utreexod set.
//
// Growth is incremental: when the global load exceeds 0.4, one bucket is split
// into two. There is no O(N) rehash and no presize cliff. Bucket capacity 128
// at load 0.4 leaves headroom for the 2x key-inflow bias of unsplit buckets.
//
// This is a bench/l2-forest addition; it does not replace the production map.

const (
	lhBucketSlots  = 128
	lhKeyPrefixLen = 12
	lhSlotSize     = 8 + lhKeyPrefixLen // packed value + key prefix
	lhHeaderSize   = 64
	lhBucketSize   = lhBucketSlots + lhBucketSlots*lhSlotSize

	lhOffMagic = 0
	lhOffLevel = 8
	lhOffSplit = 16
	lhOffCount = 24
	lhOffHash  = 32
)

// lhMagic is bumped to "LH4" for the live-only map: spent leaves are now
// removed, so a v3 file holds cumulative entries and must be rebuilt once.
var lhMagic = [8]byte{'L', 'H', '4'}

var lhZeroPrefix [lhKeyPrefixLen]byte

// swisstable control-byte encoding, mirrored locally (the constants are
// unexported in the internal package).
const (
	ctrlEmptyByte   = 0b00000000
	ctrlDeletedByte = 0b01111111
	ctrlFullByte    = 0b10000000
)

// swissSplitHash mirrors swisstable.splitHash: h1 = first 8 bytes, h2 = 7-bit
// fingerprint of byte 8 with the full-slot high bit set.
func swissSplitHash(hash [32]byte) (h1 uint64, h2 byte) {
	h1 = binary.LittleEndian.Uint64(hash[:8])
	h2 = (hash[8] & 0x7F) | ctrlFullByte
	return
}

func init() { lhFactory = newLHPositionMap }

type lhPositionMap struct {
	mmap    []byte
	file    *os.File
	level   uint64
	splitAt uint64
	count   uint64
	buckets uint64 // allocated capacity

	// Measurement only, enabled by UTREEXOD_POSMAP_STATS. Counters are
	// process-lifetime (not persisted); a reused table starts with
	// tombSlots=0, so absolute tombstone numbers are only exact on a fresh
	// build, which is what the measurement runs use.
	statsOn    bool
	statEvery  uint64
	ops        uint64
	splitsLoad uint64 // splits triggered by the global load threshold
	splitsFull uint64 // splits triggered because a bucket was full (tombstones)
	placeFull  uint64 // place() could not fit during a split (should stay 0)
	tombSlots  uint64 // deleted control bytes across the table
}

// newLHPositionMap keeps the shared factory signature. dataFile and posMask
// are unused in v3: keys are confirmed by the embedded prefix, so neither the
// forest data file nor a position mask is needed.
func newLHPositionMap(ctrlPath string, expectedEntries uint64, consistencyHash [32]byte, _ swisstable.HashSource, _ uint64) (PositionMap, bool, error) {
	f, err := os.OpenFile(ctrlPath, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, false, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, false, err
	}

	reuse := false
	if fi.Size() >= lhHeaderSize && consistencyHash != ([32]byte{}) {
		var hdr [lhHeaderSize]byte
		if _, err := f.ReadAt(hdr[:], 0); err == nil {
			if bytes.Equal(hdr[lhOffMagic:lhOffMagic+8], lhMagic[:]) {
				var stored [32]byte
				copy(stored[:], hdr[lhOffHash:lhOffHash+32])
				reuse = stored == consistencyHash
			}
		}
	}

	m := &lhPositionMap{file: f, statsOn: os.Getenv("UTREEXOD_POSMAP_STATS") != "", statEvery: 1 << 22}

	if reuse {
		var hdr [lhHeaderSize]byte
		if _, err := f.ReadAt(hdr[:], 0); err != nil {
			f.Close()
			return nil, false, err
		}
		m.level = binary.LittleEndian.Uint64(hdr[lhOffLevel:])
		m.splitAt = binary.LittleEndian.Uint64(hdr[lhOffSplit:])
		m.count = binary.LittleEndian.Uint64(hdr[lhOffCount:])
		m.buckets = uint64((fi.Size() - lhHeaderSize) / lhBucketSize)
	} else {
		need := uint64(1)
		if expectedEntries > 0 {
			need = expectedEntries*2/lhBucketSlots + 1
		}
		maxB := uint64(2)
		for maxB < need {
			maxB <<= 1
		}
		m.buckets = maxB
		size := int64(lhHeaderSize + maxB*lhBucketSize)
		if err := f.Truncate(0); err != nil {
			f.Close()
			return nil, false, err
		}
		if err := f.Truncate(size); err != nil {
			f.Close()
			return nil, false, err
		}
		var hdr [lhHeaderSize]byte
		copy(hdr[lhOffMagic:], lhMagic[:])
		if _, err := f.WriteAt(hdr[:], 0); err != nil {
			f.Close()
			return nil, false, err
		}
	}

	size := int64(lhHeaderSize + m.buckets*lhBucketSize)
	mm, err := syscall.Mmap(int(f.Fd()), 0, int(size), syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		f.Close()
		return nil, false, err
	}
	m.mmap = mm
	if m.statsOn {
		fmt.Fprintf(os.Stderr, "LH_STATS open reuse=%v count=%d tomb=unknown numBuckets=%d capBuckets=%d expectedEntries=%d\n",
			reuse, m.count, m.numBuckets(), m.buckets, expectedEntries)
	}
	return m, !reuse, nil
}

func (m *lhPositionMap) numBuckets() uint64 { return (uint64(1) << m.level) + m.splitAt }

func (m *lhPositionMap) bucketFor(h1 uint64) uint64 {
	base := uint64(1) << m.level
	b := h1 % (base << 1)
	if b >= base+m.splitAt {
		b = h1 % base
	}
	return b
}

func (m *lhPositionMap) ctrl(b uint64) []byte {
	o := lhHeaderSize + int(b)*lhBucketSize
	return m.mmap[o : o+lhBucketSlots]
}

// slot returns the packed value (first 8 bytes) and the stored key prefix
// (next 12 bytes) of slot i in bucket b.
func (m *lhPositionMap) slot(b, i uint64) (uint64, []byte) {
	o := lhHeaderSize + int(b)*lhBucketSize + lhBucketSlots + int(i)*lhSlotSize
	return binary.LittleEndian.Uint64(m.mmap[o:]), m.mmap[o+8 : o+8+lhKeyPrefixLen]
}

// bucketStart is the byte offset of bucket b's control bytes.
func (m *lhPositionMap) bucketStart(b uint64) int64 {
	return int64(lhHeaderSize + int(b)*lhBucketSize)
}

// writeAt writes directly to the file. Position-map mutations go through
// pwrite instead of stores into the shared mmap: on btrfs the dirty-page
// writeback path turns each ~4 KiB random mmap write into tens of times the
// device writes (prof/mmaptest7), while the buffered write path does not
// (prof/mmaptest9). Reads keep using the read-only mmap.
func (m *lhPositionMap) writeAt(off int64, p []byte) error {
	_, err := m.file.WriteAt(p, off)
	return err
}

// setCtrl stores control byte i of bucket b.
func (m *lhPositionMap) setCtrl(b, i uint64, v byte) error {
	return m.writeAt(m.bucketStart(b)+int64(i), []byte{v})
}

func (m *lhPositionMap) setSlot(b, i, packed uint64, prefix []byte) error {
	var buf [lhSlotSize]byte
	binary.LittleEndian.PutUint64(buf[:], packed)
	copy(buf[8:8+lhKeyPrefixLen], prefix)
	return m.writeAt(m.bucketStart(b)+int64(lhBucketSlots+i*lhSlotSize), buf[:])
}

func (m *lhPositionMap) setPacked(b, i, packed uint64) error {
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], packed)
	return m.writeAt(m.bucketStart(b)+int64(lhBucketSlots+i*lhSlotSize), buf[:])
}

// clearBucket zeroes the control bytes and every slot of bucket b, so stale
// keys vanish.
func (m *lhPositionMap) clearBucket(b uint64) error {
	var zero [lhBucketSize]byte
	return m.writeAt(m.bucketStart(b), zero[:])
}

// Set inserts or updates hash. Growth happens BEFORE the insert; split() now
// re-places entries from their stored prefixes, so the data file is not read
// and a key need not be resolvable mid-split.
func (m *lhPositionMap) Set(hash [32]byte, packed uint64) error {
	if err := m.growFor(1); err != nil {
		return err
	}
	return m.insert(hash, packed)
}

// growFor splits until count+add fits at load <= 0.4.
func (m *lhPositionMap) growFor(add uint64) error {
	for (m.count+add)*5 > m.numBuckets()*lhBucketSlots*2 {
		m.splitsLoad++
		if err := m.split(); err != nil {
			return err
		}
	}
	return nil
}

// insert places hash without growing.
func (m *lhPositionMap) insert(hash [32]byte, packed uint64) error {
	m.bumpStats()
	h1, h2 := swissSplitHash(hash)
	prefix := hash[:lhKeyPrefixLen]
	for {
		b := m.bucketFor(h1)
		ctrl := m.ctrl(b)
		for i := 0; i < lhBucketSlots; i++ {
			if ctrl[i] == h2 {
				if _, p := m.slot(b, uint64(i)); bytes.Equal(p, prefix) {
					return m.setPacked(b, uint64(i), packed)
				}
			}
		}
		for i := 0; i < lhBucketSlots; i++ {
			if ctrl[i] == ctrlEmptyByte || ctrl[i] == ctrlDeletedByte {
				if ctrl[i] == ctrlDeletedByte {
					m.tombSlots--
				}
				if err := m.setCtrl(b, uint64(i), h2); err != nil {
					return err
				}
				if err := m.setSlot(b, uint64(i), packed, prefix); err != nil {
					return err
				}
				m.count++
				return nil
			}
		}
		// Bucket full although the global load is low: tombstone pressure.
		m.splitsFull++
		if err := m.split(); err != nil {
			return err
		}
	}
}

// place stores an entry in bucket b without growing or counting; used by split.
func (m *lhPositionMap) place(b uint64, h2 byte, packed uint64, prefix []byte) error {
	ctrl := m.ctrl(b)
	for i := 0; i < lhBucketSlots; i++ {
		if ctrl[i] == ctrlEmptyByte || ctrl[i] == ctrlDeletedByte {
			if err := m.setCtrl(b, uint64(i), h2); err != nil {
				return err
			}
			return m.setSlot(b, uint64(i), packed, prefix)
		}
	}
	m.placeFull++
	return fmt.Errorf("lh place: bucket %d full", b)
}

// ensureBuckets grows and remaps the file until bucket idx fits. The table
// starts small and doubles, so no presize hint is needed for unbounded growth.
func (m *lhPositionMap) ensureBuckets(idx uint64) error {
	if idx < m.buckets {
		return nil
	}
	newB := m.buckets
	for newB <= idx {
		newB <<= 1
	}
	newSize := int64(lhHeaderSize + newB*lhBucketSize)
	if err := syscall.Munmap(m.mmap); err != nil {
		return err
	}
	m.mmap = nil
	if err := m.file.Truncate(newSize); err != nil {
		return err
	}
	mm, err := syscall.Mmap(int(m.file.Fd()), 0, int(newSize),
		syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		return err
	}
	m.mmap = mm
	m.buckets = newB
	return nil
}

func (m *lhPositionMap) split() error {
	oldB := m.splitAt
	newB := (uint64(1) << m.level) + m.splitAt
	if err := m.ensureBuckets(newB); err != nil {
		return err
	}
	if err := m.clearBucket(newB); err != nil {
		return err
	}
	m.splitAt++
	if m.splitAt == (uint64(1) << m.level) {
		m.level++
		m.splitAt = 0
	}

	ctrl := m.ctrl(oldB)
	type entry struct {
		h1, packed uint64
		prefix     [lhKeyPrefixLen]byte
	}
	ents := make([]entry, 0, lhBucketSlots)
	tomb := uint64(0)
	for i := 0; i < lhBucketSlots; i++ {
		if ctrl[i]&ctrlFullByte != 0 {
			p, prefix := m.slot(oldB, uint64(i))
			var e entry
			e.packed = p
			copy(e.prefix[:], prefix)
			// The prefix's first 8 bytes ARE the key's first 8 bytes (h1),
			// so the data file is never consulted here.
			e.h1 = binary.LittleEndian.Uint64(e.prefix[:8])
			ents = append(ents, e)
		} else if ctrl[i] == ctrlDeletedByte {
			tomb++
		}
	}
	if tomb >= m.tombSlots {
		m.tombSlots = 0
	} else {
		m.tombSlots -= tomb
	}
	if err := m.clearBucket(oldB); err != nil {
		return err
	}
	for _, e := range ents {
		h2 := (e.prefix[8] & 0x7F) | ctrlFullByte
		if err := m.place(m.bucketFor(e.h1), h2, e.packed, e.prefix[:]); err != nil {
			return err
		}
	}
	return nil
}

func (m *lhPositionMap) Get(hash [32]byte) (uint64, bool, error) {
	h1, h2 := swissSplitHash(hash)
	prefix := hash[:lhKeyPrefixLen]
	b := m.bucketFor(h1)
	ctrl := m.ctrl(b)
	for i := 0; i < lhBucketSlots; i++ {
		if ctrl[i] == h2 {
			p, stored := m.slot(b, uint64(i))
			if bytes.Equal(stored, prefix) {
				return p, true, nil
			}
		}
	}
	return 0, false, nil
}

func (m *lhPositionMap) Delete(hash [32]byte) (bool, error) {
	m.bumpStats()
	h1, h2 := swissSplitHash(hash)
	prefix := hash[:lhKeyPrefixLen]
	b := m.bucketFor(h1)
	ctrl := m.ctrl(b)
	for i := 0; i < lhBucketSlots; i++ {
		if ctrl[i] == h2 {
			_, stored := m.slot(b, uint64(i))
			if bytes.Equal(stored, prefix) {
				if err := m.setCtrl(b, uint64(i), ctrlDeletedByte); err != nil {
					return false, err
				}
				if err := m.setSlot(b, uint64(i), 0, lhZeroPrefix[:]); err != nil {
					return false, err
				}
				m.count--
				m.tombSlots++
				return true, nil
			}
		}
	}
	return false, nil
}

func (m *lhPositionMap) Count() uint64 { return m.count }

func (m *lhPositionMap) bumpStats() {
	if !m.statsOn {
		return
	}
	m.ops++
	if m.ops%m.statEvery == 0 {
		m.logStats("periodic")
	}
}

func (m *lhPositionMap) logStats(tag string) {
	if !m.statsOn {
		return
	}
	slots := m.numBuckets() * lhBucketSlots
	load, usedRatio, tombRatio := 0.0, 0.0, 0.0
	if slots > 0 {
		load = float64(m.count) / float64(slots)
		usedRatio = float64(m.count+m.tombSlots) / float64(slots)
	}
	if used := m.count + m.tombSlots; used > 0 {
		tombRatio = float64(m.tombSlots) / float64(used)
	}
	fmt.Fprintf(os.Stderr,
		"LH_STATS %s ops=%d count=%d tomb=%d numBuckets=%d capBuckets=%d load=%.4f usedRatio=%.4f tombRatio=%.4f splitsLoad=%d splitsFull=%d placeFull=%d\n",
		tag, m.ops, m.count, m.tombSlots, m.numBuckets(), m.buckets, load, usedRatio, tombRatio,
		m.splitsLoad, m.splitsFull, m.placeFull)
}

func (m *lhPositionMap) ForEach(fn func(packed uint64)) {
	for b := uint64(0); b < m.numBuckets(); b++ {
		ctrl := m.ctrl(b)
		for i := 0; i < lhBucketSlots; i++ {
			if ctrl[i]&ctrlFullByte != 0 {
				p, _ := m.slot(b, uint64(i))
				fn(p)
			}
		}
	}
}

func (m *lhPositionMap) syncHeader() error {
	var buf [24]byte
	binary.LittleEndian.PutUint64(buf[0:], m.level)
	binary.LittleEndian.PutUint64(buf[8:], m.splitAt)
	binary.LittleEndian.PutUint64(buf[16:], m.count)
	return m.writeAt(lhOffLevel, buf[:])
}

func (m *lhPositionMap) SetConsistencyHash(hash [32]byte) error {
	if err := m.syncHeader(); err != nil {
		return err
	}
	if _, err := m.file.WriteAt(hash[:], lhOffHash); err != nil {
		return err
	}
	return m.file.Sync()
}

func (m *lhPositionMap) BeginBatch(maxEntries uint64) (PositionMapBatch, error) {
	// Pre-grow once so Record-mode inserts do not split mid-batch.
	if err := m.growFor(maxEntries); err != nil {
		return nil, err
	}
	return &lhBatch{m: m}, nil
}

type lhBatch struct{ m *lhPositionMap }

func (b *lhBatch) Insert(h [32]byte, p uint64) error { return b.m.insert(h, p) }

func (m *lhPositionMap) Close() error {
	werr := m.syncHeader()
	if m.statsOn {
		m.logStats("close")
	}
	err := syscall.Munmap(m.mmap)
	if e := m.file.Close(); err == nil {
		err = e
	}
	if werr != nil && err == nil {
		err = werr
	}
	return err
}
