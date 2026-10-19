package utreexo

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestProcessDeletionsBucketOrderScatter checks that the bucket-order reorder in
// processDeletions does not disturb the caller-visible result alignment: each
// returned deletion position must still belong to the hash at the same input
// index, even though the lookups and live-map deletes run in bucket order.
// Both the serial (< minParallelSize) and the parallel fan-out paths are run.
func TestProcessDeletionsBucketOrderScatter(t *testing.T) {
	t.Run("serial", func(t *testing.T) { checkDeletionScatter(t, 64, false) })
	t.Run("parallel", func(t *testing.T) { checkDeletionScatter(t, minParallelSize+128, true) })
}

// checkDeletionScatter adds n leaves, deletes a reversed subset, and verifies
// every returned position matches the pre-delete position of the hash at that
// input index. deleteAll picks the whole set so the fan-out crosses
// minParallelSize; otherwise only odd indexes are deleted, leaving a mix of
// live and spent entries to check the live-map deletion.
func checkDeletionScatter(t *testing.T, n int, deleteAll bool) {
	t.Helper()
	dir := t.TempDir()
	f, err := newForest(memCached(t, 32), memCached(t, 4), memCached(t, 32),
		newDeletedBitmap(0), dir+"/ctrl", dir+"/slots", 16, 0)
	require.NoError(t, err)

	adds := make([]Leaf, n)
	for i := range adds {
		adds[i] = Leaf{Hash: testHashFromInt(i)}
	}
	require.NoError(t, f.Modify(adds, nil, Proof{}))

	// Expected position per hash, read before the deletions remove them.
	want := make(map[Hash]uint64, n)
	for i := range adds {
		packed, ok, err := f.positionMap.Get(adds[i].Hash)
		require.NoError(t, err)
		require.True(t, ok)
		want[adds[i].Hash] = unpackPos(packed)
	}

	// Reverse order so block order != ascending bucket order.
	dels := make([]Hash, 0, n)
	for i := n - 1; i >= 0; i-- {
		if deleteAll || i%2 == 1 {
			dels = append(dels, adds[i].Hash)
		}
	}
	_, delPositions, err := f.processDeletions(dels)
	require.NoError(t, err)
	require.Len(t, delPositions, len(dels))
	for i, h := range dels {
		require.Equalf(t, want[h], delPositions[i], "delPositions[%d] must belong to dels[%d]", i, i)
	}

	// The spent hashes are gone from the live map; the rest remain.
	for i := range adds {
		_, ok, err := f.positionMap.Get(adds[i].Hash)
		require.NoError(t, err)
		require.Equalf(t, !deleteAll && i%2 == 0, ok, "hash %d live-map presence", i)
	}
}
