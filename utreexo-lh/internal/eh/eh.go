// Package eh implements an extendible-hashing position map for benchmarking.
//
// File layout (matches the LH v5.1 bucket layout so the two can be compared
// in isolation):
//
//	<crtl>: bucket[b] = ctrl[128] + slot[128]*(8B packed [+ 12B key prefix])
//	<dir >: header[64] + directory slot[2^D]*8 + local-depth[bucketCount]
//
// Directory index is the top D bits of h1 (h1 = LE(hash[0:8])), so the
// directory doubles by interleaving: old[i] -> new[2i], new[2i+1]. (The
// directive's "new[i], new[i+2^D]" is the LSB-indexed variant; the MSB index
// in 4.2 and the "top d bits" invariant in 6.1 require the interleaved form.)
//
// Reads go through a read-only MAP_SHARED mmap; mutations use pwrite, the
// v5.1 convention (mmap dirty writeback amplifies random 4 KiB writes).
package eh

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"math/bits"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

const (
	SlotCount    = 128
	KeyPrefixLen = 12
	SlotSizePfx  = 8 + KeyPrefixLen
	SlotSizeBare = 8
	HeaderSize   = 64

	ctrlEmpty   = 0x00
	ctrlDeleted = 0x7F
	ctrlFull    = 0x80

	bucketGrow  = 256  // buckets per bucket-file growth step (size slack)
	depthGrow   = 4096 // bytes per local-depth array growth step
	statDefault = 1 << 22
	ehMaxDepth  = 48 // guard against degenerate key prefixes (see split)
)

// HashSource is the forest data file: HashAt(pos*32) returns the 32-byte key.
// Only the no-prefix ("eh8") variant uses it (verifyHash).
type HashSource interface {
	HashAt(off int64) ([32]byte, error)
}

// EHMap is a persistent, single-writer extendible hash map.
type EHMap struct {
	dirPath  string
	ctrlPath string
	dirFile  *os.File
	ctrlFile *os.File

	dirMM  []byte // read-only mapping of dir.dat
	ctrlMM []byte // read-only mapping of ctrl.dat

	depth       uint64 // global depth D
	dirSlots    uint64 // 1 << D
	bucketCount uint64
	bucketCap   uint64 // buckets addressable by ctrlMM
	depthCap    uint64 // bytes reserved for the local-depth array

	slotSize int
	prefix   bool
	count    uint64
	tomb     uint64
	load     float64
	src      HashSource
	posMask  uint64
	finished bool
	consHash [32]byte

	// counters, EH_STATS
	ops        uint64
	splits     uint64
	dirDoubles uint64
	placeFull  uint64
	statEvery  uint64
	statOn     bool
}

// NewEHMap creates a fresh map. load is the target occupancy used by Presize
// (splitting itself only happens when a bucket is full); prefix selects the
// 20 B (packed+prefix, no verifyHash) or 8 B (packed only, verifyHash) slot.
func NewEHMap(dir string, load float64, prefix bool) (*EHMap, error) {
	m, _, err := NewEHMapWithHash(dir, load, prefix, [32]byte{})
	return m, err
}

// NewEHMapWithHash opens the map and reuses the on-disk table when its header
// magic matches and the stored consistency hash is non-zero and equal. On
// reuse the caller must not Presize again. reused=false means a fresh table
// was created (the old files, if any, were truncated).
func NewEHMapWithHash(dir string, load float64, prefix bool, consistencyHash [32]byte) (*EHMap, bool, error) {
	if load <= 0 || load > 1 {
		load = 0.80
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, false, err
	}
	m := &EHMap{
		dirPath:   filepath.Join(dir, "eh-dir.dat"),
		ctrlPath:  filepath.Join(dir, "eh-ctrl.dat"),
		load:      load,
		prefix:    prefix,
		statEvery: statDefault,
		statOn:    true,
	}
	if prefix {
		m.slotSize = SlotSizePfx
	} else {
		m.slotSize = SlotSizeBare
	}
	if v := os.Getenv("EH_STAT_EVERY"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil && n > 0 {
			m.statEvery = n
		}
	}
	if os.Getenv("EH_STATS") == "0" {
		m.statOn = false
	}
	cf, err := os.OpenFile(m.ctrlPath, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, false, err
	}
	df, err := os.OpenFile(m.dirPath, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		cf.Close()
		return nil, false, err
	}
	m.ctrlFile, m.dirFile = cf, df

	// Reuse path: header magic present and consistency hash matches.
	if fi, err := df.Stat(); err == nil && fi.Size() >= HeaderSize {
		var h [HeaderSize]byte
		if _, err := df.ReadAt(h[:], 0); err == nil && string(h[0:4]) == "EH01" {
			var stored [32]byte
			copy(stored[:], h[32:64])
			if consistencyHash != ([32]byte{}) && stored == consistencyHash {
				m.depth = binary.LittleEndian.Uint64(h[8:])
				m.bucketCount = binary.LittleEndian.Uint64(h[16:])
				m.count = binary.LittleEndian.Uint64(h[24:])
				m.consHash = consistencyHash
				m.dirSlots = uint64(1) << m.depth
				if err := m.mapExisting(); err != nil {
					m.Close()
					return nil, false, err
				}
				return m, true, nil
			}
		}
	}

	// Fresh path.
	if err := cf.Truncate(0); err != nil {
		m.Close()
		return nil, false, err
	}
	if err := df.Truncate(0); err != nil {
		m.Close()
		return nil, false, err
	}
	if err := m.resize(1, 1, 0); err != nil {
		m.Close()
		return nil, false, err
	}
	m.depth = 0
	m.dirSlots = 1
	m.bucketCount = 1
	m.writeHeader()
	return m, false, nil
}

func (m *EHMap) mapExisting() error {
	dfi, err := m.dirFile.Stat()
	if err != nil {
		return err
	}
	cfi, err := m.ctrlFile.Stat()
	if err != nil {
		return err
	}
	dirSize := dfi.Size()
	base := int64(HeaderSize + m.dirSlots*8)
	if dirSize < base {
		return fmt.Errorf("eh: dir file too small (%d < %d)", dirSize, base)
	}
	m.depthCap = uint64(dirSize - base)
	m.bucketCap = uint64(cfi.Size()) / m.bucketSize()
	mm, err := syscall.Mmap(int(m.dirFile.Fd()), 0, int(dirSize), syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		return err
	}
	m.dirMM = mm
	if cfi.Size() > 0 {
		mm, err = syscall.Mmap(int(m.ctrlFile.Fd()), 0, int(cfi.Size()), syscall.PROT_READ, syscall.MAP_SHARED)
		if err != nil {
			return err
		}
		m.ctrlMM = mm
	}
	// tombstones are not persisted; recount them so stats and split's tomb
	// bookkeeping start consistent after a reopen.
	m.tomb = 0
	for b := uint64(0); b < m.bucketCount; b++ {
		c := m.ctrl(b)
		for i := 0; i < SlotCount; i++ {
			if c[i] == ctrlDeleted {
				m.tomb++
			}
		}
	}
	return nil
}

// SetConsistencyHash persists the forest consistency hash so a later open can
// reuse the on-disk table instead of rebuilding it.
func (m *EHMap) SetConsistencyHash(h [32]byte) error {
	m.consHash = h
	_, err := m.dirFile.WriteAt(h[:], 32)
	return err
}

// SetVerifySource supplies the data file used by the no-prefix variant.
func (m *EHMap) SetVerifySource(src HashSource, posMask uint64) {
	m.src = src
	m.posMask = posMask
}

func (m *EHMap) bucketSize() uint64 { return SlotCount + uint64(SlotCount*m.slotSize) }

func roundUp(v, step uint64) uint64 {
	return (v + step - 1) / step * step
}

// resize lays out both files for dirSlots directory entries and bucketCap
// physical buckets, then maps them read-only. Contents are left untouched.
func (m *EHMap) resize(dirSlots, bucketCap, depthCap uint64) error {
	if m.dirMM != nil {
		syscall.Munmap(m.dirMM)
		m.dirMM = nil
	}
	if m.ctrlMM != nil {
		syscall.Munmap(m.ctrlMM)
		m.ctrlMM = nil
	}
	if depthCap == 0 {
		depthCap = depthGrow
	}
	dirSize := int64(HeaderSize + dirSlots*8 + depthCap)
	if err := m.dirFile.Truncate(dirSize); err != nil {
		return err
	}
	if _, err := m.dirFile.WriteAt(make([]byte, HeaderSize), 0); err != nil {
		return err
	}
	ctrlSize := int64(bucketCap * m.bucketSize())
	if err := m.ctrlFile.Truncate(ctrlSize); err != nil {
		return err
	}
	mm, err := syscall.Mmap(int(m.dirFile.Fd()), 0, int(dirSize), syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		return err
	}
	m.dirMM = mm
	if ctrlSize > 0 {
		mm, err = syscall.Mmap(int(m.ctrlFile.Fd()), 0, int(ctrlSize), syscall.PROT_READ, syscall.MAP_SHARED)
		if err != nil {
			return err
		}
		m.ctrlMM = mm
	}
	m.bucketCap = bucketCap
	m.depthCap = depthCap
	return nil
}

// Presize builds a fresh table near the target load.
//
// Directory index = top D bits. A bucket whose local depth is D-1 covers two
// adjacent slots, so it receives twice the key flow of a depth-D bucket; a
// presize that mixes depths is therefore only stable at load <= 0.5. For
// higher loads we presize to 2^(D-1) buckets all at local depth D-1 (initial
// load up to ~0.95) and let full-bucket splits reach EH's natural equilibrium
// (load ~0.69). Either way splits only happen when a bucket is full.
func (m *EHMap) Presize(n uint64) error {
	if n == 0 {
		return nil
	}
	target := uint64(math.Ceil(float64(n) / (float64(SlotCount) * m.load)))
	if target < 1 {
		target = 1
	}
	var D, dirSlots, nb uint64
	if m.load <= 0.5 {
		// Mixed-depth layout reaches the target load exactly.
		D = 0
		if target > 1 {
			D = uint64(bits.Len64(target - 1))
		}
		dirSlots = uint64(1) << D
		nb = target
	} else {
		D = 0
		if target > 1 {
			D = uint64(bits.Len64(target - 1))
		}
		if D == 0 {
			dirSlots, nb = 1, 1
		} else {
			dirSlots = uint64(1) << D
			nb = dirSlots / 2
		}
	}
	cap := roundUp(nb, bucketGrow)
	depthCap := roundUp(nb, depthGrow)
	if err := m.resize(dirSlots, cap, depthCap); err != nil {
		return err
	}
	m.depth = D
	m.dirSlots = dirSlots
	m.bucketCount = nb
	m.count = 0
	m.tomb = 0

	slots := make([]byte, dirSlots*8)
	depth := make([]byte, depthCap)
	setSlot := func(i, b uint64) {
		binary.LittleEndian.PutUint64(slots[i*8:], b)
	}
	if D == 0 {
		setSlot(0, 0)
	} else if m.load <= 0.5 {
		base := uint64(1) << (D - 1)
		pairsSplit := nb - base
		for j := uint64(0); j < base; j++ {
			if j < pairsSplit {
				b0, b1 := 2*j, 2*j+1
				setSlot(2*j, b0)
				setSlot(2*j+1, b1)
				depth[b0] = byte(D)
				depth[b1] = byte(D)
			} else {
				b := pairsSplit + j
				setSlot(2*j, b)
				setSlot(2*j+1, b)
				depth[b] = byte(D - 1)
			}
		}
	} else {
		for j := uint64(0); j < nb; j++ {
			setSlot(2*j, j)
			setSlot(2*j+1, j)
			depth[j] = byte(D - 1)
		}
	}
	if _, err := m.dirFile.WriteAt(slots, HeaderSize); err != nil {
		return err
	}
	if _, err := m.dirFile.WriteAt(depth, int64(HeaderSize+dirSlots*8)); err != nil {
		return err
	}
	m.writeHeader()
	return nil
}

// ---------------------------------------------------------------------------
// raw access (read via mmap, write via pwrite)
// ---------------------------------------------------------------------------

func (m *EHMap) bucketOff(b uint64) int64 { return int64(b * m.bucketSize()) }

func (m *EHMap) ctrl(b uint64) []byte {
	o := m.bucketOff(b)
	return m.ctrlMM[o : o+SlotCount]
}

func (m *EHMap) slotOff(b, i uint64) int64 {
	return m.bucketOff(b) + SlotCount + int64(i)*int64(m.slotSize)
}

func (m *EHMap) slotPacked(b, i uint64) uint64 {
	o := m.slotOff(b, i)
	return binary.LittleEndian.Uint64(m.ctrlMM[o:])
}

func (m *EHMap) slotPrefix(b, i uint64) []byte {
	o := m.slotOff(b, i)
	return m.ctrlMM[o+8 : o+8+KeyPrefixLen]
}

func (m *EHMap) dirSlot(i uint64) uint64 {
	return binary.LittleEndian.Uint64(m.dirMM[HeaderSize+i*8:])
}

func (m *EHMap) setDirSlot(i, b uint64) error {
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], b)
	_, err := m.dirFile.WriteAt(buf[:], int64(HeaderSize+i*8))
	return err
}

func (m *EHMap) depthOff(b uint64) int64 {
	return int64(HeaderSize + m.dirSlots*8 + b)
}

func (m *EHMap) bucketDepth(b uint64) uint64 {
	return uint64(m.dirMM[m.depthOff(b)])
}

func (m *EHMap) setDepth(b, d uint64) error {
	_, err := m.dirFile.WriteAt([]byte{byte(d)}, m.depthOff(b))
	return err
}

func (m *EHMap) setCtrl(b, i uint64, v byte) error {
	_, err := m.ctrlFile.WriteAt([]byte{v}, m.bucketOff(b)+int64(i))
	return err
}

func (m *EHMap) writeSlot(b, i, packed uint64, prefix []byte) error {
	var buf [SlotSizePfx]byte
	binary.LittleEndian.PutUint64(buf[:], packed)
	if m.prefix {
		copy(buf[8:], prefix)
	}
	_, err := m.ctrlFile.WriteAt(buf[:m.slotSize], m.slotOff(b, i))
	return err
}

func (m *EHMap) clearBucket(b uint64) error {
	_, err := m.ctrlFile.WriteAt(make([]byte, m.bucketSize()), m.bucketOff(b))
	return err
}

func (m *EHMap) writeHeader() {
	var h [HeaderSize]byte
	copy(h[0:8], []byte("EH01\x00\x00\x00\x00"))
	binary.LittleEndian.PutUint64(h[8:], m.depth)
	binary.LittleEndian.PutUint64(h[16:], m.bucketCount)
	binary.LittleEndian.PutUint64(h[24:], m.count)
	copy(h[32:64], m.consHash[:])
	m.dirFile.WriteAt(h[:], 0)
}

// ---------------------------------------------------------------------------
// hashing helpers
// ---------------------------------------------------------------------------

func splitHash(hash [32]byte) (h1 uint64, h2 byte) {
	h1 = binary.LittleEndian.Uint64(hash[:8])
	h2 = (hash[8] & 0x7F) | ctrlFull
	return
}

func (m *EHMap) bucketFor(h1 uint64) uint64 {
	if m.depth == 0 {
		return m.dirSlot(0)
	}
	return m.dirSlot(h1 >> (64 - m.depth))
}

func (m *EHMap) verify(packed uint64, hash [32]byte) (bool, error) {
	if m.src == nil {
		return false, fmt.Errorf("eh: no-prefix map without HashSource")
	}
	h, err := m.src.HashAt(int64((packed & m.posMask) * 32))
	if err != nil {
		return false, err
	}
	return h == hash, nil
}

func (m *EHMap) match(b, i uint64, hash [32]byte, h2 byte, prefix []byte) (bool, error) {
	if m.prefix {
		return bytes.Equal(m.slotPrefix(b, i), prefix), nil
	}
	return m.verify(m.slotPacked(b, i), hash)
}

// ---------------------------------------------------------------------------
// public operations
// ---------------------------------------------------------------------------

func (m *EHMap) Set(hash [32]byte, packed uint64) error {
	m.bumpOps()
	h1, h2 := splitHash(hash)
	prefix := hash[:KeyPrefixLen]
	for tries := 0; tries < 4096; tries++ {
		b := m.bucketFor(h1)
		ctrl := m.ctrl(b)
		for i := 0; i < SlotCount; i++ {
			if ctrl[i] == h2 {
				ok, err := m.match(b, uint64(i), hash, h2, prefix)
				if err != nil {
					return err
				}
				if ok {
					return m.writeSlot(b, uint64(i), packed, prefix)
				}
			}
		}
		for i := 0; i < SlotCount; i++ {
			if ctrl[i] == ctrlEmpty || ctrl[i] == ctrlDeleted {
				if ctrl[i] == ctrlDeleted && m.tomb > 0 {
					m.tomb--
				}
				if err := m.setCtrl(b, uint64(i), h2); err != nil {
					return err
				}
				if err := m.writeSlot(b, uint64(i), packed, prefix); err != nil {
					return err
				}
				m.count++
				return nil
			}
		}
		if err := m.split(b, h1); err != nil {
			return err
		}
	}
	return fmt.Errorf("eh: too many splits for one insert")
}

func (m *EHMap) Get(hash [32]byte) (uint64, bool, error) {
	m.bumpOps()
	h1, h2 := splitHash(hash)
	prefix := hash[:KeyPrefixLen]
	b := m.bucketFor(h1)
	ctrl := m.ctrl(b)
	for i := 0; i < SlotCount; i++ {
		if ctrl[i] == h2 {
			ok, err := m.match(b, uint64(i), hash, h2, prefix)
			if err != nil {
				return 0, false, err
			}
			if ok {
				return m.slotPacked(b, uint64(i)), true, nil
			}
		}
	}
	return 0, false, nil
}

func (m *EHMap) Delete(hash [32]byte) (bool, error) {
	m.bumpOps()
	h1, h2 := splitHash(hash)
	prefix := hash[:KeyPrefixLen]
	b := m.bucketFor(h1)
	ctrl := m.ctrl(b)
	for i := 0; i < SlotCount; i++ {
		if ctrl[i] == h2 {
			ok, err := m.match(b, uint64(i), hash, h2, prefix)
			if err != nil {
				return false, err
			}
			if ok {
				if err := m.setCtrl(b, uint64(i), ctrlDeleted); err != nil {
					return false, err
				}
				if err := m.writeSlot(b, uint64(i), 0, make([]byte, KeyPrefixLen)); err != nil {
					return false, err
				}
				m.count--
				m.tomb++
				return true, nil
			}
		}
	}
	return false, nil
}

func (m *EHMap) Count() uint64 { return m.count }
func (m *EHMap) Ops() uint64   { return m.ops }

func (m *EHMap) ForEach(fn func(packed uint64)) {
	for b := uint64(0); b < m.bucketCount; b++ {
		ctrl := m.ctrl(b)
		for i := 0; i < SlotCount; i++ {
			if ctrl[i]&ctrlFull != 0 {
				fn(m.slotPacked(b, uint64(i)))
			}
		}
	}
}

func (m *EHMap) NumSlots() uint64 { return m.bucketCount * SlotCount }

func (m *EHMap) MapBytes() uint64 {
	fi, err := m.ctrlFile.Stat()
	if err != nil {
		return 0
	}
	n := uint64(fi.Size())
	fi, err = m.dirFile.Stat()
	if err != nil {
		return n
	}
	return n + uint64(fi.Size())
}

// DirBytes is the directory file (header + slots + local depths).
func (m *EHMap) DirBytes() uint64 {
	fi, err := m.dirFile.Stat()
	if err != nil {
		return 0
	}
	return uint64(fi.Size())
}

func (m *EHMap) Depth() uint64      { return m.depth }
func (m *EHMap) DirSlots() uint64   { return m.dirSlots }
func (m *EHMap) Buckets() uint64    { return m.bucketCount }
func (m *EHMap) Tomb() uint64       { return m.tomb }
func (m *EHMap) Splits() uint64     { return m.splits }
func (m *EHMap) DirDoubles() uint64 { return m.dirDoubles }
func (m *EHMap) PlaceFull() uint64  { return m.placeFull }

func (m *EHMap) DropCache() {
	adviseDontNeed(m.ctrlFile)
	adviseDontNeed(m.dirFile)
}

func (m *EHMap) Close() error {
	if m.finished {
		return nil
	}
	m.finished = true
	m.logStats("close")
	m.writeHeader()
	if m.ctrlMM != nil {
		syscall.Munmap(m.ctrlMM)
		m.ctrlMM = nil
	}
	if m.dirMM != nil {
		syscall.Munmap(m.dirMM)
		m.dirMM = nil
	}
	err := m.ctrlFile.Close()
	if e := m.dirFile.Close(); err == nil {
		err = e
	}
	return err
}

// ---------------------------------------------------------------------------
// split
// ---------------------------------------------------------------------------

func (m *EHMap) ensureBucketCap(nb uint64) error {
	if nb <= m.bucketCap {
		return nil
	}
	newCap := roundUp(nb, bucketGrow)
	if m.ctrlMM != nil {
		syscall.Munmap(m.ctrlMM)
		m.ctrlMM = nil
	}
	if err := m.ctrlFile.Truncate(int64(newCap * m.bucketSize())); err != nil {
		return err
	}
	mm, err := syscall.Mmap(int(m.ctrlFile.Fd()), 0, int(newCap*m.bucketSize()),
		syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		return err
	}
	m.ctrlMM = mm
	m.bucketCap = newCap
	return nil
}

func (m *EHMap) ensureDepthCap(nb uint64) error {
	if nb <= m.depthCap {
		return nil
	}
	newCap := roundUp(nb+1, depthGrow)
	old := m.dirMM
	if old != nil {
		syscall.Munmap(old)
		m.dirMM = nil
	}
	size := int64(HeaderSize + m.dirSlots*8 + newCap)
	if err := m.dirFile.Truncate(size); err != nil {
		return err
	}
	mm, err := syscall.Mmap(int(m.dirFile.Fd()), 0, int(size), syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		return err
	}
	m.dirMM = mm
	m.depthCap = newCap
	return nil
}

// doubleDir interleaves the directory. Directory index = top D bits, so the
// old index i becomes new indices 2i and 2i+1.
func (m *EHMap) doubleDir() error {
	oldSlots := m.dirSlots
	oldVals := make([]uint64, oldSlots)
	for i := uint64(0); i < oldSlots; i++ {
		oldVals[i] = m.dirSlot(i)
	}
	depths := make([]byte, m.bucketCount)
	copy(depths, m.dirMM[m.depthOff(0):m.depthOff(0)+int64(m.bucketCount)])

	newD := m.depth + 1
	newSlots := oldSlots * 2
	size := int64(HeaderSize + newSlots*8 + m.depthCap)
	if m.dirMM != nil {
		syscall.Munmap(m.dirMM)
		m.dirMM = nil
	}
	if err := m.dirFile.Truncate(size); err != nil {
		return err
	}
	mm, err := syscall.Mmap(int(m.dirFile.Fd()), 0, int(size), syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		return err
	}
	m.dirMM = mm
	m.depth = newD
	m.dirSlots = newSlots

	slots := make([]byte, newSlots*8)
	for i, v := range oldVals {
		binary.LittleEndian.PutUint64(slots[(2*i)*8:], v)
		binary.LittleEndian.PutUint64(slots[(2*i+1)*8:], v)
	}
	if _, err := m.dirFile.WriteAt(slots, HeaderSize); err != nil {
		return err
	}
	if _, err := m.dirFile.WriteAt(depths, int64(HeaderSize+newSlots*8)); err != nil {
		return err
	}
	m.writeHeader()
	return nil
}

type splitEntry struct {
	packed uint64
	prefix [KeyPrefixLen]byte
	h1     uint64
	h2     byte
}

func (m *EHMap) split(b, h1 uint64) error {
	d := m.bucketDepth(b)
	idxOld := uint64(0)
	if m.depth > 0 {
		idxOld = h1 >> (64 - m.depth)
	}

	// Collect entries and drop this bucket's tombstones.
	var ents []splitEntry
	ctrl := m.ctrl(b)
	tombInB := uint64(0)
	for i := 0; i < SlotCount; i++ {
		switch {
		case ctrl[i]&ctrlFull != 0:
			e := splitEntry{packed: m.slotPacked(b, uint64(i)), h2: ctrl[i]}
			if m.prefix {
				copy(e.prefix[:], m.slotPrefix(b, uint64(i)))
				e.h1 = binary.LittleEndian.Uint64(e.prefix[:8])
			} else {
				if m.src == nil {
					return fmt.Errorf("eh: split without HashSource")
				}
				h, err := m.src.HashAt(int64((e.packed & m.posMask) * 32))
				if err != nil {
					return err
				}
				copy(e.prefix[:], h[:KeyPrefixLen])
				e.h1 = binary.LittleEndian.Uint64(h[:8])
			}
			ents = append(ents, e)
		case ctrl[i] == ctrlDeleted:
			tombInB++
		}
	}
	if tombInB >= m.tomb {
		m.tomb = 0
	} else {
		m.tomb -= tombInB
	}

	doubled := false
	if d == m.depth {
		if m.depth >= ehMaxDepth {
			return fmt.Errorf("eh: local depth %d exceeds %d (degenerate key distribution?)", m.depth, ehMaxDepth)
		}
		if err := m.doubleDir(); err != nil {
			return err
		}
		m.dirDoubles++
		doubled = true
	}

	nb := m.bucketCount
	if err := m.ensureBucketCap(nb + 1); err != nil {
		return err
	}
	if err := m.ensureDepthCap(nb + 1); err != nil {
		return err
	}
	m.bucketCount = nb + 1
	if err := m.setDepth(b, d+1); err != nil {
		return err
	}
	if err := m.setDepth(nb, d+1); err != nil {
		return err
	}

	// Slot range of the old depth-d bucket, then its upper half (bit d = 1).
	var base, upperStart, end uint64
	if doubled {
		base = 2 * idxOld
		upperStart = base + 1
		end = base + 2
	} else {
		shift := m.depth - d
		p := idxOld >> shift
		base = p << shift
		upperStart = base + (uint64(1) << (shift - 1))
		end = base + (uint64(1) << shift)
	}

	if err := m.clearBucket(b); err != nil {
		return err
	}
	for _, e := range ents {
		target := b
		if (e.h1>>(63-d))&1 != 0 {
			target = nb
		}
		if err := m.place(target, e); err != nil {
			return err
		}
	}
	for i := upperStart; i < end; i++ {
		if err := m.setDirSlot(i, nb); err != nil {
			return err
		}
	}
	m.splits++
	return nil
}

func (m *EHMap) place(b uint64, e splitEntry) error {
	ctrl := m.ctrl(b)
	for i := 0; i < SlotCount; i++ {
		if ctrl[i] == ctrlEmpty {
			if err := m.setCtrl(b, uint64(i), e.h2); err != nil {
				return err
			}
			return m.writeSlot(b, uint64(i), e.packed, e.prefix[:])
		}
	}
	m.placeFull++
	return fmt.Errorf("eh place: bucket %d full", b)
}

// ---------------------------------------------------------------------------
// stats / page cache
// ---------------------------------------------------------------------------

func (m *EHMap) bumpOps() {
	if !m.statOn {
		return
	}
	m.ops++
	if m.statEvery > 0 && m.ops%m.statEvery == 0 {
		m.logStats("periodic")
	}
}

func (m *EHMap) logStats(tag string) {
	if !m.statOn {
		return
	}
	slots := m.bucketCount * SlotCount
	load, usedRatio, tombRatio := 0.0, 0.0, 0.0
	if slots > 0 {
		load = float64(m.count) / float64(slots)
		usedRatio = float64(m.count+m.tomb) / float64(slots)
	}
	if used := m.count + m.tomb; used > 0 {
		tombRatio = float64(m.tomb) / float64(used)
	}
	fmt.Fprintf(os.Stderr,
		"EH_STATS %s ops=%d count=%d tomb=%d buckets=%d D=%d dirSlots=%d load=%.4f usedRatio=%.4f tombRatio=%.4f splits=%d dirDoubles=%d placeFull=%d\n",
		tag, m.ops, m.count, m.tomb, m.bucketCount, m.depth, m.dirSlots,
		load, usedRatio, tombRatio, m.splits, m.dirDoubles, m.placeFull)
}
