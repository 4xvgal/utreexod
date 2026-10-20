//go:build linux

package utreexo

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// forceDense switches the WAL mmap cache to the dense store for this test.
// The production switch is the UTREEXO_WAL_DENSE env var; tests override the
// cached decision instead of mutating the process environment.
func forceDense(t *testing.T) {
	t.Helper()
	old := denseEnabled
	denseEnabled = func() bool { return true }
	t.Cleanup(func() { denseEnabled = old })
}

// TestDenseCacheBudget pins the documented budget change: the dense store
// charges entrySize+16 per entry, so the same --utreexoproofindexmaxmemory
// holds ~2/3 as many 32-byte entries and flush triggers earlier.
func TestDenseCacheBudget(t *testing.T) {
	forceDense(t)

	const maxCacheBytes = 48 * 1000
	c, err := newCachedRWS(newMemFile(), 32, maxCacheBytes, 0)
	require.NoError(t, err)
	defer c.Close()

	for i := 0; i <= 1000; i++ {
		require.NoError(t, c.PutHashAt(testHashFromInt(i), int64(i)*32))
		if i < 1000 {
			require.False(t, c.FlushNeeded(), "must not flush at %d entries", i+1)
		}
	}
	require.True(t, c.FlushNeeded(), "budget is maxBytes/(32+16)=1000 entries")

	require.NoError(t, c.Flush())
	require.False(t, c.FlushNeeded(), "flush resets the cache")
	require.Equal(t, 0, c.cache.count())
}

// TestDenseWALCrashBeforeCommit is TestWALCrashBeforeCommit with the dense
// store: an incomplete journal must be discarded and the underlying files
// must stay at the previous consistent state.
func TestDenseWALCrashBeforeCommit(t *testing.T) {
	forceDense(t)

	journal := newMemFile()
	delFile := newMemFile()
	mainFile := newMemFile()
	blockCountsFile := newMemFile()
	metaFile := newMemFile()

	w, err := newWAL(journal, delFile,
		walFile{File: mainFile, EntrySize: 32},
		walFile{File: blockCountsFile, EntrySize: 4},
		walFile{File: metaFile, EntrySize: 32},
	)
	require.NoError(t, err)

	tmpDir := t.TempDir()
	forest, err := newForest(w.Cached(0), w.Cached(1), w.Cached(2), w.Bitmap(),
		tmpDir+"/ctrl", tmpDir+"/slots", 10, 0)
	require.NoError(t, err)

	leaves := make([]Leaf, 8)
	for i := range leaves {
		leaves[i] = Leaf{Hash: testHashFromInt(i + 1)}
	}
	require.NoError(t, forest.Modify(leaves, nil, Proof{}))
	require.NoError(t, w.Flush([32]byte{}))
	block1Roots := forest.GetRoots()

	delHashes := []Hash{leaves[0].Hash, leaves[3].Hash}
	proof, err := forest.Prove(delHashes)
	require.NoError(t, err)
	require.NoError(t, forest.Modify(nil, delHashes, proof))

	require.NoError(t, w.crashBeforeCommit())

	w2, err := newWAL(journal, delFile,
		walFile{File: mainFile, EntrySize: 32},
		walFile{File: blockCountsFile, EntrySize: 4},
		walFile{File: metaFile, EntrySize: 32},
	)
	require.NoError(t, err)

	tmpDir2 := t.TempDir()
	forest2, err := newForest(w2.Cached(0), w2.Cached(1), w2.Cached(2), w2.Bitmap(),
		tmpDir2+"/ctrl", tmpDir2+"/slots", 10, 0)
	require.NoError(t, err)

	require.Equal(t, block1Roots, forest2.GetRoots(),
		"roots should match block 1 after discarding the incomplete journal")
}

// TestDenseWALForestRecovery is TestWALForestRecovery with the dense store:
// replay after a crash between journal commit and apply must reproduce the
// post-block roots and the deleted-position bitmap.
func TestDenseWALForestRecovery(t *testing.T) {
	forceDense(t)

	journal := newMemFile()
	delFile := newMemFile()
	mainFile := newMemFile()
	blockCountsFile := newMemFile()
	metaFile := newMemFile()

	w, err := newWAL(journal, delFile,
		walFile{File: mainFile, EntrySize: 32},
		walFile{File: blockCountsFile, EntrySize: 4},
		walFile{File: metaFile, EntrySize: 32},
	)
	require.NoError(t, err)

	tmpDir := t.TempDir()
	forest, err := newForest(w.Cached(0), w.Cached(1), w.Cached(2), w.Bitmap(),
		tmpDir+"/ctrl", tmpDir+"/slots", 10, 0)
	require.NoError(t, err)

	leaves := make([]Leaf, 8)
	for i := range leaves {
		leaves[i] = Leaf{Hash: testHashFromInt(i + 1)}
	}
	require.NoError(t, forest.Modify(leaves, nil, Proof{}))
	require.NoError(t, w.Flush([32]byte{}))

	delHashes := []Hash{leaves[0].Hash, leaves[3].Hash}
	proof, err := forest.Prove(delHashes)
	require.NoError(t, err)
	require.NoError(t, forest.Modify(nil, delHashes, proof))

	expectedRoots := forest.GetRoots()
	expectedDeleted := w.Bitmap().count()
	require.Positive(t, expectedDeleted)

	require.NoError(t, w.crashAfterCommit())

	w2, err := newWAL(journal, delFile,
		walFile{File: mainFile, EntrySize: 32},
		walFile{File: blockCountsFile, EntrySize: 4},
		walFile{File: metaFile, EntrySize: 32},
	)
	require.NoError(t, err)

	tmpDir2 := t.TempDir()
	forest2, err := newForest(w2.Cached(0), w2.Cached(1), w2.Cached(2), w2.Bitmap(),
		tmpDir2+"/ctrl", tmpDir2+"/slots", 10, 0)
	require.NoError(t, err)

	require.Equal(t, expectedRoots, forest2.GetRoots(), "roots after dense replay")
	require.Equal(t, expectedDeleted, w2.Bitmap().count(), "deleted bitmap after dense replay")
}

// TestDenseForestRecordRoundTrip runs the record-mode write path (the one that
// produces the scattered interior offsets) through the dense store with
// flushes in the middle, then verifies the roots against an in-memory
// accumulator and reloads the forest from disk.
func TestDenseForestRecordRoundTrip(t *testing.T) {
	forceDense(t)

	dir := t.TempDir()
	f, err := OpenForest(dir, MaxCacheMemory(256<<10)) // flush every few blocks
	require.NoError(t, err)
	require.NoError(t, f.EnterRecordMode())

	acc := NewAccumulator()
	live := make([]Hash, 0, 4096)

	var seed uint64 = 0x5EED
	randHash := func() Hash {
		var h Hash
		for i := 0; i < 4; i++ {
			seed += 0x9E3779B97F4A7C15
			z := seed
			z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
			z = (z ^ (z >> 27)) * 0x94D049BB133111EB
			binary.LittleEndian.PutUint64(h[i*8:], z^(z>>31))
		}
		return h
	}

	flushes := 0
	for b := 0; b < 8; b++ {
		adds := make([]Leaf, 500)
		addHashes := make([]Hash, len(adds))
		for i := range adds {
			adds[i] = Leaf{Hash: randHash()}
			addHashes[i] = adds[i].Hash
		}
		var dels []Hash
		for i := 0; i < 200 && len(live) > 0; i++ {
			pick := int(seed>>7) % len(live)
			dels = append(dels, live[pick])
			live[pick] = live[len(live)-1]
			live = live[:len(live)-1]
		}

		_, _, err := f.Record(addHashes, dels)
		require.NoError(t, err)
		if _, _, _, err := f.RehashAndProve(nil); err != nil {
			t.Fatalf("block %d rehash: %v", b, err)
		}
		require.NoError(t, acc.Modify(adds, dels, Proof{}))
		live = append(live, addHashes...)

		if f.FlushNeeded() {
			require.NoError(t, f.Flush([32]byte{}))
			flushes++
		}
	}
	require.Positive(t, flushes, "test must exercise mid-run flushes")

	require.Equal(t, acc.GetRoots(), f.GetRoots(), "roots before reopen")
	numLeaves := f.NumLeaves
	require.NoError(t, f.Close([32]byte{}))

	f2, err := OpenForest(dir, MaxCacheMemory(256<<10))
	require.NoError(t, err)
	defer f2.Close([32]byte{})

	require.Equal(t, numLeaves, f2.NumLeaves)
	require.Equal(t, acc.GetRoots(), f2.GetRoots(), "roots after reopen")
}

// TestDenseMatchesSparseOnDisk replays one identical Record/Rehash/Flush
// sequence twice -- once with the sparse store, once with the dense store --
// and requires the same committed state. The stores only change how pending
// writes are buffered, so neither the pending (offset, value) set nor the
// bytes that land on disk may depend on them.
func TestDenseMatchesSparseOnDisk(t *testing.T) {
	dirs := [2]string{t.TempDir(), t.TempDir()}
	pending := [2]map[int64][32]byte{}

	for run := range dirs {
		if run == 1 {
			forceDense(t)
		}
		f, err := OpenForest(dirs[run], MaxCacheMemory(128<<10))
		require.NoError(t, err)
		require.NoError(t, f.EnterRecordMode())

		var seed uint64 = 0xC0FFEE
		randHash := func() Hash {
			var h Hash
			for i := 0; i < 4; i++ {
				seed += 0x9E3779B97F4A7C15
				z := seed
				z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
				z = (z ^ (z >> 27)) * 0x94D049BB133111EB
				binary.LittleEndian.PutUint64(h[i*8:], z^(z>>31))
			}
			return h
		}

		live := make([]Hash, 0, 4096)
		for b := 0; b < 6; b++ {
			adds := make([]Hash, 400)
			for i := range adds {
				adds[i] = randHash()
			}
			var dels []Hash
			for i := 0; i < 150 && len(live) > 0; i++ {
				pick := int(seed>>7) % len(live)
				dels = append(dels, live[pick])
				live[pick] = live[len(live)-1]
				live = live[:len(live)-1]
			}
			_, _, err := f.Record(adds, dels)
			require.NoError(t, err)
			if _, _, _, err := f.RehashAndProve(nil); err != nil {
				t.Fatal(err)
			}
			live = append(live, adds...)
		}

		// Snapshot the forest data writes still buffered in the WAL cache.
		got := make(map[int64][32]byte)
		f.wal.Cached(0).cache.forEach(func(off int64, data []byte) {
			got[off] = [32]byte(data)
		})
		require.Greater(t, len(got), 1000, "test must buffer a meaningful write set")
		pending[run] = got

		require.NoError(t, f.Close([32]byte{}))

		// Every buffered write must be readable back from the data file.
		dataFile, err := os.Open(filepath.Join(dirs[run], forestDataFileName))
		require.NoError(t, err)
		for off, want := range got {
			var buf [32]byte
			n, err := dataFile.ReadAt(buf[:], off)
			require.NoError(t, err, "read back at %d", off)
			require.Equal(t, 32, n)
			require.Equal(t, want, buf, "committed bytes at offset %d", off)
		}
		require.NoError(t, dataFile.Close())
	}

	require.Equal(t, len(pending[0]), len(pending[1]), "pending write set size")
	require.Equal(t, pending[0], pending[1], "pending (offset, value) sets differ")
}
