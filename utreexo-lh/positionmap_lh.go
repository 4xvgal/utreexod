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

// Linear hashing position map, v2 (key-prefix embedded).
//
// The table lives in one mmap'd file: a 64-byte header followed by fixed-size
// buckets. Each entry stores the packed value plus a 16-byte prefix of the key,
// so the map confirms a hit by itself and never reads the data file. This
// removes verifyHash (a random forest_data read per lookup), at the cost of
// ~36 bytes/entry instead of ~13.
//
// The prefix also lets split() re-place entries using the stored key bytes, so
// the data file is not touched during growth either. A 16-byte prefix is
// required: 8 bytes would collide ~16 times at 2^34 live entries, 16 bytes
// gives a 2^-60 table-wide collision probability.
//
// Growth is incremental: when the global load exceeds 0.5, one bucket is split
// into two. There is no O(N) rehash and no presize cliff. Bucket capacity 128
// at load 0.5 makes per-bucket overflow statistically impossible.
//
// This is a bench/l2-forest addition; it does not replace the production map.

const (
	lhBucketSlots  = 128
	lhKeyPrefixLen = 16
	lhSlotSize     = 8 + lhKeyPrefixLen // packed value + key prefix
	lhHeaderSize   = 64
	lhBucketSize   = lhBucketSlots + lhBucketSlots*lhSlotSize

	lhOffMagic = 0
	lhOffLevel = 8
	lhOffSplit = 16
	lhOffCount = 24
	lhOffHash  = 32
)

// lhMagic is bumped from "LH1" because the on-disk slot layout changed; an
// existing v1 file fails the magic check and is rebuilt from the data file.
var lhMagic = [8]byte{'L', 'H', '2'}

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
}

// newLHPositionMap keeps the shared factory signature. dataFile and posMask
// are unused in v2: keys are confirmed by the embedded prefix, so neither the
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

	m := &lhPositionMap{file: f}

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
		for maxB < need*2 {
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
	mm, err := syscall.Mmap(int(f.Fd()), 0, int(size), syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		f.Close()
		return nil, false, err
	}
	m.mmap = mm
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
// (next 16 bytes) of slot i in bucket b.
func (m *lhPositionMap) slot(b, i uint64) (uint64, []byte) {
	o := lhHeaderSize + int(b)*lhBucketSize + lhBucketSlots + int(i)*lhSlotSize
	return binary.LittleEndian.Uint64(m.mmap[o:]), m.mmap[o+8 : o+8+lhKeyPrefixLen]
}

func (m *lhPositionMap) setSlot(b, i, packed uint64, prefix []byte) {
	o := lhHeaderSize + int(b)*lhBucketSize + lhBucketSlots + int(i)*lhSlotSize
	binary.LittleEndian.PutUint64(m.mmap[o:], packed)
	copy(m.mmap[o+8:o+8+lhKeyPrefixLen], prefix)
}

func (m *lhPositionMap) setPacked(b, i, packed uint64) {
	o := lhHeaderSize + int(b)*lhBucketSize + lhBucketSlots + int(i)*lhSlotSize
	binary.LittleEndian.PutUint64(m.mmap[o:], packed)
}

func (m *lhPositionMap) clearBucket(b uint64) {
	c := m.ctrl(b)
	for i := range c {
		c[i] = 0
	}
	// Zero the whole slot region (packed + prefix) so stale keys vanish.
	o := lhHeaderSize + int(b)*lhBucketSize + lhBucketSlots
	s := m.mmap[o : o+lhBucketSlots*lhSlotSize]
	for i := range s {
		s[i] = 0
	}
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

// growFor splits until count+add fits at load <= 0.5.
func (m *lhPositionMap) growFor(add uint64) error {
	for (m.count+add)*2 > m.numBuckets()*lhBucketSlots {
		if err := m.split(); err != nil {
			return err
		}
	}
	return nil
}

// insert places hash without growing.
func (m *lhPositionMap) insert(hash [32]byte, packed uint64) error {
	h1, h2 := swissSplitHash(hash)
	prefix := hash[:lhKeyPrefixLen]
	for {
		b := m.bucketFor(h1)
		ctrl := m.ctrl(b)
		for i := 0; i < lhBucketSlots; i++ {
			if ctrl[i] == h2 {
				if _, p := m.slot(b, uint64(i)); bytes.Equal(p, prefix) {
					m.setPacked(b, uint64(i), packed)
					return nil
				}
			}
		}
		for i := 0; i < lhBucketSlots; i++ {
			if ctrl[i] == ctrlEmptyByte || ctrl[i] == ctrlDeletedByte {
				ctrl[i] = h2
				m.setSlot(b, uint64(i), packed, prefix)
				m.count++
				return nil
			}
		}
		// Statistically unreachable at load 0.5 with 128 slots, but recover.
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
			ctrl[i] = h2
			m.setSlot(b, uint64(i), packed, prefix)
			return nil
		}
	}
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
		syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
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
	m.clearBucket(newB)
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
		}
	}
	m.clearBucket(oldB)
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
	h1, h2 := swissSplitHash(hash)
	prefix := hash[:lhKeyPrefixLen]
	b := m.bucketFor(h1)
	ctrl := m.ctrl(b)
	for i := 0; i < lhBucketSlots; i++ {
		if ctrl[i] == h2 {
			_, stored := m.slot(b, uint64(i))
			if bytes.Equal(stored, prefix) {
				ctrl[i] = ctrlDeletedByte
				m.setSlot(b, uint64(i), 0, lhZeroPrefix[:])
				m.count--
				return true, nil
			}
		}
	}
	return false, nil
}

func (m *lhPositionMap) Count() uint64 { return m.count }

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

func (m *lhPositionMap) syncHeader() {
	binary.LittleEndian.PutUint64(m.mmap[lhOffLevel:], m.level)
	binary.LittleEndian.PutUint64(m.mmap[lhOffSplit:], m.splitAt)
	binary.LittleEndian.PutUint64(m.mmap[lhOffCount:], m.count)
}

func (m *lhPositionMap) SetConsistencyHash(hash [32]byte) error {
	m.syncHeader()
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
	m.syncHeader()
	err := syscall.Munmap(m.mmap)
	if e := m.file.Close(); err == nil {
		err = e
	}
	return err
}
