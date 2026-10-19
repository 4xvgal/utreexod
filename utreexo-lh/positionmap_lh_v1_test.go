//go:build linux

package utreexo

import (
	"encoding/binary"
	"fmt"
	"os"
	"syscall"

	"github.com/utreexo/utreexo/internal/swisstable"
)

// Linear hashing position map, v1 (test-only copy). VerifyHash reads each
// candidate key back from the data file; kept here so benchmarks can compare
// it against the v2 prefix-embedded layout. DO NOT ship.
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
	v1BucketSlots = 128
	v1HeaderSize  = 64
	v1BucketSize  = v1BucketSlots + v1BucketSlots*8

	v1OffMagic = 0
	v1OffLevel = 8
	v1OffSplit = 16
	v1OffCount = 24
	v1OffHash  = 32
)

var v1Magic = [8]byte{'L', 'H', '1'}

// swisstable control-byte encoding, mirrored locally (the constants are
// unexported in the internal package).
const (
	v1CtrlEmpty   = 0b00000000
	v1CtrlDeleted = 0b01111111
	v1CtrlFull    = 0b10000000
)

// v1SplitHash mirrors swisstable.splitHash: h1 = first 8 bytes, h2 = 7-bit
// fingerprint of byte 8 with the full-slot high bit set.
func v1SplitHash(hash [32]byte) (h1 uint64, h2 byte) {
	h1 = binary.LittleEndian.Uint64(hash[:8])
	h2 = (hash[8] & 0x7F) | v1CtrlFull
	return
}

type lhV1Map struct {
	mmap    []byte
	file    *os.File
	level   uint64
	splitAt uint64
	count   uint64
	buckets uint64 // allocated capacity
	src     swisstable.HashSource
	posMask uint64
}

func newLHV1Map(ctrlPath string, expectedEntries uint64, consistencyHash [32]byte, dataFile swisstable.HashSource, posMask uint64) (PositionMap, bool, error) {
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
	if fi.Size() >= v1HeaderSize && consistencyHash != ([32]byte{}) {
		var hdr [v1HeaderSize]byte
		if _, err := f.ReadAt(hdr[:], 0); err == nil {
			if hdr[0] == v1Magic[0] && hdr[1] == v1Magic[1] && hdr[2] == v1Magic[2] {
				var stored [32]byte
				copy(stored[:], hdr[v1OffHash:v1OffHash+32])
				reuse = stored == consistencyHash
			}
		}
	}

	m := &lhV1Map{file: f, src: dataFile, posMask: posMask}

	if reuse {
		var hdr [v1HeaderSize]byte
		if _, err := f.ReadAt(hdr[:], 0); err != nil {
			f.Close()
			return nil, false, err
		}
		m.level = binary.LittleEndian.Uint64(hdr[v1OffLevel:])
		m.splitAt = binary.LittleEndian.Uint64(hdr[v1OffSplit:])
		m.count = binary.LittleEndian.Uint64(hdr[v1OffCount:])
		m.buckets = uint64((fi.Size() - v1HeaderSize) / v1BucketSize)
	} else {
		need := uint64(1)
		if expectedEntries > 0 {
			need = expectedEntries*2/v1BucketSlots + 1
		}
		maxB := uint64(2)
		for maxB < need*2 {
			maxB <<= 1
		}
		m.buckets = maxB
		size := int64(v1HeaderSize + maxB*v1BucketSize)
		if err := f.Truncate(0); err != nil {
			f.Close()
			return nil, false, err
		}
		if err := f.Truncate(size); err != nil {
			f.Close()
			return nil, false, err
		}
		var hdr [v1HeaderSize]byte
		copy(hdr[v1OffMagic:], v1Magic[:])
		if _, err := f.WriteAt(hdr[:], 0); err != nil {
			f.Close()
			return nil, false, err
		}
	}

	size := int64(v1HeaderSize + m.buckets*v1BucketSize)
	mm, err := syscall.Mmap(int(f.Fd()), 0, int(size), syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		f.Close()
		return nil, false, err
	}
	m.mmap = mm
	return m, !reuse, nil
}

func (m *lhV1Map) numBuckets() uint64 { return (uint64(1) << m.level) + m.splitAt }

func (m *lhV1Map) bucketFor(h1 uint64) uint64 {
	base := uint64(1) << m.level
	b := h1 % (base << 1)
	if b >= base+m.splitAt {
		b = h1 % base
	}
	return b
}

func (m *lhV1Map) ctrl(b uint64) []byte {
	o := v1HeaderSize + int(b)*v1BucketSize
	return m.mmap[o : o+v1BucketSlots]
}

func (m *lhV1Map) slots(b uint64) []byte {
	o := v1HeaderSize + int(b)*v1BucketSize + v1BucketSlots
	return m.mmap[o : o+v1BucketSlots*8]
}

func (m *lhV1Map) getSlot(b, i uint64) uint64 {
	return binary.LittleEndian.Uint64(m.slots(b)[i*8:])
}

func (m *lhV1Map) setSlot(b, i, v uint64) {
	binary.LittleEndian.PutUint64(m.slots(b)[i*8:], v)
}

func (m *lhV1Map) clearBucket(b uint64) {
	c := m.ctrl(b)
	for i := range c {
		c[i] = 0
	}
	s := m.slots(b)
	for i := range s {
		s[i] = 0
	}
}

func (m *lhV1Map) verify(packed uint64, expected [32]byte) (bool, error) {
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
func (m *lhV1Map) Set(hash [32]byte, packed uint64) error {
	if err := m.growFor(1); err != nil {
		return err
	}
	return m.insert(hash, packed)
}

// growFor splits until count+add fits at load <= 0.5.
func (m *lhV1Map) growFor(add uint64) error {
	for (m.count+add)*2 > m.numBuckets()*v1BucketSlots {
		if err := m.split(); err != nil {
			return err
		}
	}
	return nil
}

// insert places hash without growing.
func (m *lhV1Map) insert(hash [32]byte, packed uint64) error {
	h1, h2 := v1SplitHash(hash)
	for {
		b := m.bucketFor(h1)
		ctrl := m.ctrl(b)
		for i := 0; i < v1BucketSlots; i++ {
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
		for i := 0; i < v1BucketSlots; i++ {
			if ctrl[i] == v1CtrlEmpty || ctrl[i] == v1CtrlDeleted {
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

func (m *lhV1Map) place(b uint64, hash [32]byte, packed uint64) error {
	_, h2 := v1SplitHash(hash)
	ctrl := m.ctrl(b)
	for i := 0; i < v1BucketSlots; i++ {
		if ctrl[i] == v1CtrlEmpty || ctrl[i] == v1CtrlDeleted {
			ctrl[i] = h2
			m.setSlot(b, uint64(i), packed)
			return nil
		}
	}
	return fmt.Errorf("lh place: bucket %d full", b)
}

// ensureBuckets grows and remaps the file until bucket idx fits (v1 carry-over
// of the same growth fix as v2, so only the verifyHash path differs).
func (m *lhV1Map) ensureBuckets(idx uint64) error {
	if idx < m.buckets {
		return nil
	}
	newB := m.buckets
	for newB <= idx {
		newB <<= 1
	}
	newSize := int64(v1HeaderSize + newB*v1BucketSize)
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

func (m *lhV1Map) split() error {
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
	packed := make([]uint64, 0, v1BucketSlots)
	for i := 0; i < v1BucketSlots; i++ {
		if ctrl[i]&v1CtrlFull != 0 {
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

func (m *lhV1Map) Get(hash [32]byte) (uint64, bool, error) {
	h1, h2 := v1SplitHash(hash)
	b := m.bucketFor(h1)
	ctrl := m.ctrl(b)
	for i := 0; i < v1BucketSlots; i++ {
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

func (m *lhV1Map) Delete(hash [32]byte) (bool, error) {
	h1, h2 := v1SplitHash(hash)
	b := m.bucketFor(h1)
	ctrl := m.ctrl(b)
	for i := 0; i < v1BucketSlots; i++ {
		if ctrl[i] == h2 {
			ok, err := m.verify(m.getSlot(b, uint64(i)), hash)
			if err != nil {
				return false, err
			}
			if ok {
				ctrl[i] = v1CtrlDeleted
				m.setSlot(b, uint64(i), 0)
				m.count--
				return true, nil
			}
		}
	}
	return false, nil
}

func (m *lhV1Map) Count() uint64 { return m.count }

func (m *lhV1Map) ForEach(fn func(packed uint64)) {
	for b := uint64(0); b < m.numBuckets(); b++ {
		ctrl := m.ctrl(b)
		for i := 0; i < v1BucketSlots; i++ {
			if ctrl[i]&v1CtrlFull != 0 {
				fn(m.getSlot(b, uint64(i)))
			}
		}
	}
}

func (m *lhV1Map) syncHeader() {
	binary.LittleEndian.PutUint64(m.mmap[v1OffLevel:], m.level)
	binary.LittleEndian.PutUint64(m.mmap[v1OffSplit:], m.splitAt)
	binary.LittleEndian.PutUint64(m.mmap[v1OffCount:], m.count)
}

func (m *lhV1Map) SetConsistencyHash(hash [32]byte) error {
	m.syncHeader()
	if _, err := m.file.WriteAt(hash[:], v1OffHash); err != nil {
		return err
	}
	return m.file.Sync()
}

func (m *lhV1Map) BeginBatch(maxEntries uint64) (PositionMapBatch, error) {
	// Pre-grow so no split (which re-reads the data file) happens during
	// Record-mode inserts, where the leaf hashes may not be written yet.
	if err := m.growFor(maxEntries); err != nil {
		return nil, err
	}
	return &lhV1Batch{m: m}, nil
}

type lhV1Batch struct{ m *lhV1Map }

func (b *lhV1Batch) Insert(h [32]byte, p uint64) error { return b.m.insert(h, p) }

func (m *lhV1Map) Close() error {
	m.syncHeader()
	err := syscall.Munmap(m.mmap)
	if e := m.file.Close(); err == nil {
		err = e
	}
	return err
}
