//go:build linux

package utreexo

import (
	"encoding/binary"
	"fmt"
	"os"
	"syscall"

	"github.com/utreexo/utreexo/internal/swisstable"
)

// Linear hashing position map.
//
// The table lives in one mmap'd file: a 64-byte header followed by fixed-size
// buckets. Entries store only the 7-bit H2 fingerprint plus the packed value;
// verifyHash reads the key back from the data file, same as the Swiss map.
//
// Growth is incremental: when the global load exceeds 0.5, one bucket is split
// into two. There is no O(N) rehash and no presize cliff. Bucket capacity 128
// at load 0.5 makes per-bucket overflow statistically impossible.
//
// This is a bench/l2-forest addition; it does not replace the production map.

const (
	lhBucketSlots = 128
	lhHeaderSize  = 64
	lhBucketSize  = lhBucketSlots + lhBucketSlots*8

	lhOffMagic = 0
	lhOffLevel = 8
	lhOffSplit = 16
	lhOffCount = 24
	lhOffHash  = 32
)

var lhMagic = [8]byte{'L', 'H', '1'}

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
	src     swisstable.HashSource
	posMask uint64
}

func newLHPositionMap(ctrlPath string, expectedEntries uint64, consistencyHash [32]byte, dataFile swisstable.HashSource, posMask uint64) (PositionMap, bool, error) {
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
			if hdr[0] == lhMagic[0] && hdr[1] == lhMagic[1] && hdr[2] == lhMagic[2] {
				var stored [32]byte
				copy(stored[:], hdr[lhOffHash:lhOffHash+32])
				reuse = stored == consistencyHash
			}
		}
	}

	m := &lhPositionMap{file: f, src: dataFile, posMask: posMask}

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

func (m *lhPositionMap) slots(b uint64) []byte {
	o := lhHeaderSize + int(b)*lhBucketSize + lhBucketSlots
	return m.mmap[o : o+lhBucketSlots*8]
}

func (m *lhPositionMap) getSlot(b, i uint64) uint64 {
	return binary.LittleEndian.Uint64(m.slots(b)[i*8:])
}

func (m *lhPositionMap) setSlot(b, i, v uint64) {
	binary.LittleEndian.PutUint64(m.slots(b)[i*8:], v)
}

func (m *lhPositionMap) clearBucket(b uint64) {
	c := m.ctrl(b)
	for i := range c {
		c[i] = 0
	}
	s := m.slots(b)
	for i := range s {
		s[i] = 0
	}
}

func (m *lhPositionMap) verify(packed uint64, expected [32]byte) (bool, error) {
	pos := packed & m.posMask
	h, err := m.src.HashAt(int64(pos * 32))
	if err != nil {
		return false, err
	}
	return h == expected, nil
}

// Set inserts or updates hash. Growth happens BEFORE the insert (like the
// Swiss map's pre-insert resize): split() re-reads each displaced key from the
// data file, and Forest calls positionMap.Set before writeHash, so a key not
// yet in the table must never be read back mid-split.
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
	for {
		b := m.bucketFor(h1)
		ctrl := m.ctrl(b)
		for i := 0; i < lhBucketSlots; i++ {
			if ctrl[i] == h2 {
				ok, err := m.verify(m.getSlot(b, uint64(i)), hash)
				if err != nil {
					return err
				}
				if ok {
					m.setSlot(b, uint64(i), packed)
					return nil
				}
			}
		}
		for i := 0; i < lhBucketSlots; i++ {
			if ctrl[i] == ctrlEmptyByte || ctrl[i] == ctrlDeletedByte {
				ctrl[i] = h2
				m.setSlot(b, uint64(i), packed)
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

func (m *lhPositionMap) place(b uint64, hash [32]byte, packed uint64) error {
	_, h2 := swissSplitHash(hash)
	ctrl := m.ctrl(b)
	for i := 0; i < lhBucketSlots; i++ {
		if ctrl[i] == ctrlEmptyByte || ctrl[i] == ctrlDeletedByte {
			ctrl[i] = h2
			m.setSlot(b, uint64(i), packed)
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
	packed := make([]uint64, 0, lhBucketSlots)
	for i := 0; i < lhBucketSlots; i++ {
		if ctrl[i]&ctrlFullByte != 0 {
			packed = append(packed, m.getSlot(oldB, uint64(i)))
		}
	}
	m.clearBucket(oldB)
	for _, p := range packed {
		h, err := m.src.HashAt(int64((p & m.posMask) * 32))
		if err != nil {
			return err
		}
		h1 := binary.LittleEndian.Uint64(h[:8])
		if err := m.place(m.bucketFor(h1), h, p); err != nil {
			return err
		}
	}
	return nil
}

func (m *lhPositionMap) Get(hash [32]byte) (uint64, bool, error) {
	h1, h2 := swissSplitHash(hash)
	b := m.bucketFor(h1)
	ctrl := m.ctrl(b)
	for i := 0; i < lhBucketSlots; i++ {
		if ctrl[i] == h2 {
			p := m.getSlot(b, uint64(i))
			ok, err := m.verify(p, hash)
			if err != nil {
				return 0, false, err
			}
			if ok {
				return p, true, nil
			}
		}
	}
	return 0, false, nil
}

func (m *lhPositionMap) Delete(hash [32]byte) (bool, error) {
	h1, h2 := swissSplitHash(hash)
	b := m.bucketFor(h1)
	ctrl := m.ctrl(b)
	for i := 0; i < lhBucketSlots; i++ {
		if ctrl[i] == h2 {
			ok, err := m.verify(m.getSlot(b, uint64(i)), hash)
			if err != nil {
				return false, err
			}
			if ok {
				ctrl[i] = ctrlDeletedByte
				m.setSlot(b, uint64(i), 0)
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
				fn(m.getSlot(b, uint64(i)))
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
	// Pre-grow so no split (which re-reads the data file) happens during
	// Record-mode inserts, where the leaf hashes may not be written yet.
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
