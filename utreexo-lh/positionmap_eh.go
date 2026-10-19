//go:build linux

package utreexo

import (
	"os"
	"path/filepath"
	"strconv"

	"github.com/utreexo/utreexo/internal/eh"
	"github.com/utreexo/utreexo/internal/swisstable"
)

// ehFactory is set in init (declared in positionmap.go) so the bench/l2 Forest
// can select the extendible-hashing position map with PositionMapKind("eh")
// without changing the production position map or Forest logic.
func init() { ehFactory = newEHPositionMap }

type ehPM struct{ m *eh.EHMap }

func (e *ehPM) Get(h [32]byte) (uint64, bool, error) { return e.m.Get(h) }
func (e *ehPM) Set(h [32]byte, p uint64) error       { return e.m.Set(h, p) }
func (e *ehPM) Delete(h [32]byte) (bool, error)      { return e.m.Delete(h) }
func (e *ehPM) Count() uint64                        { return e.m.Count() }
func (e *ehPM) ForEach(fn func(uint64))              { e.m.ForEach(fn) }
func (e *ehPM) Close() error                         { return e.m.Close() }
func (e *ehPM) SetConsistencyHash(h [32]byte) error  { return e.m.SetConsistencyHash(h) }

type ehPMBatch struct{ m *eh.EHMap }

func (b *ehPMBatch) Insert(h [32]byte, p uint64) error { return b.m.Set(h, p) }

// BeginBatch returns a batch that inserts through Set. EH does not need a
// pre-resize step (it is presized at open), so this is the whole batch.
func (e *ehPM) BeginBatch(uint64) (PositionMapBatch, error) {
	return &ehPMBatch{m: e.m}, nil
}

func ehLoad() float64 {
	if v := os.Getenv("UTREEXO_EH_LOAD"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 && f <= 1 {
			return f
		}
	}
	return 0.80
}

// newEHPositionMap keeps the newPositionMap factory signature. EH lives in its
// own files under the position-map directory (dir of ctrlPath).
func newEHPositionMap(ctrlPath string, expectedEntries uint64, consistencyHash [32]byte, dataFile swisstable.HashSource, posMask uint64) (PositionMap, bool, error) {
	m, reused, err := eh.NewEHMapWithHash(filepath.Dir(ctrlPath), ehLoad(), true, consistencyHash)
	if err != nil {
		return nil, false, err
	}
	m.SetVerifySource(dataFile, posMask)
	if !reused && expectedEntries > 0 {
		if err := m.Presize(expectedEntries); err != nil {
			m.Close()
			return nil, false, err
		}
	}
	return &ehPM{m: m}, !reused, nil
}
