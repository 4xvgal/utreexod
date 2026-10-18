//go:build unix

package utreexo

import (
	"fmt"

	"github.com/utreexo/utreexo/internal/swisstable"
)

// PositionMap is the position-map abstraction used by Forest. The production
// implementation is the Swiss Table in internal/swisstable. The bench/l2-forest
// branch adds alternatives (linear hashing) so they can be plugged into the
// real Forest without changing Forest's logic.
type PositionMap interface {
	Get(hash [32]byte) (uint64, bool, error)
	Set(hash [32]byte, packed uint64) error
	Delete(hash [32]byte) (bool, error)
	Count() uint64
	ForEach(fn func(packed uint64))
	Close() error
	SetConsistencyHash(hash [32]byte) error
	BeginBatch(maxEntries uint64) (PositionMapBatch, error)
}

// PositionMapBatch is the bulk-insert handle returned by BeginBatch.
type PositionMapBatch interface {
	Insert(hash [32]byte, packed uint64) error
}

// swissPM adapts swisstable.SwissPositionMap to PositionMap without touching
// the swisstable package.
type swissPM struct{ m *swisstable.SwissPositionMap }

func (s *swissPM) Get(h [32]byte) (uint64, bool, error) { return s.m.Get(h) }
func (s *swissPM) Set(h [32]byte, p uint64) error       { return s.m.Set(h, p) }
func (s *swissPM) Delete(h [32]byte) (bool, error)      { return s.m.Delete(h) }
func (s *swissPM) Count() uint64                        { return s.m.Count() }
func (s *swissPM) ForEach(fn func(uint64))              { s.m.ForEach(fn) }
func (s *swissPM) Close() error                         { return s.m.Close() }
func (s *swissPM) SetConsistencyHash(h [32]byte) error  { return s.m.SetConsistencyHash(h) }
func (s *swissPM) BeginBatch(n uint64) (PositionMapBatch, error) {
	b, err := s.m.BeginBatch(n)
	if err != nil {
		return nil, err
	}
	return &swissPMBatch{b: b}, nil
}

type swissPMBatch struct{ b swisstable.Batch }

func (s *swissPMBatch) Insert(h [32]byte, p uint64) error { return s.b.Insert(h, p) }

// lhFactory is set by positionmap_lh.go (linux). It is nil on platforms where
// the linear-hashing implementation is not compiled in.
var lhFactory func(ctrlPath string, expectedEntries uint64, consistencyHash [32]byte, dataFile swisstable.HashSource, posMask uint64) (PositionMap, bool, error)

// newPositionMap builds the position map selected by kind. needsRebuild is true
// when the on-disk table could not be reused and must be rebuilt from the data
// file.
func newPositionMap(kind, ctrlPath, slotsPath string, expectedEntries uint64, consistencyHash [32]byte, dataFile swisstable.HashSource, posMask uint64) (PositionMap, bool, error) {
	switch kind {
	case "", "swiss":
		m, needsRebuild, err := swisstable.NewSwissPositionMap(ctrlPath, slotsPath, expectedEntries, consistencyHash, dataFile, posMask)
		if err != nil {
			return nil, false, err
		}
		return &swissPM{m: m}, needsRebuild, nil
	case "lh":
		if lhFactory == nil {
			return nil, false, fmt.Errorf("linear hashing not compiled in on this platform")
		}
		return lhFactory(ctrlPath, expectedEntries, consistencyHash, dataFile, posMask)
	default:
		return nil, false, fmt.Errorf("unknown position map kind %q", kind)
	}
}
