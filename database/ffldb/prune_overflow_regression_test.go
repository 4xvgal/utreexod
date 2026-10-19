// Copyright (c) 2026 The utreexod developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package ffldb

import (
	"testing"

	"github.com/utreexo/utreexod/database"
)

// TestPruneTotalsExceed4GiB reproduces the flat-file prune collapse: the
// per-file size accumulator was uint32, so a store holding more than 4 GiB
// across its .fdb files wrapped to a small number and PruneBlocks believed it
// was already under the target. The store here has multiple real flat files
// whose reported sizes are faked to 3 GiB each, so the true total is far above
// 4 GiB while the old accumulator wrapped to something tiny.
func TestPruneTotalsExceed4GiB(t *testing.T) {
	dbPath := t.TempDir()
	idb, err := database.Create(dbType, dbPath, blockDataNet)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer idb.Close()

	pdb := idb.(*db)
	// Small flat files so the test blocks span several file numbers.
	pdb.blkStore.maxBlockFileSize = 1024
	pdb.sjStore.maxBlockFileSize = 1024

	blocks, err := loadBlocks(t, blockDataFile, blockDataNet)
	if err != nil {
		t.Fatalf("loadBlocks: %v", err)
	}
	err = idb.Update(func(tx database.Tx) error {
		for _, block := range blocks {
			if err := tx.StoreBlock(block); err != nil {
				return err
			}
			blockBytes, err := block.Bytes()
			if err != nil {
				return err
			}
			if err := tx.StoreSpendJournal(block.Hash(), blockBytes); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("store blocks: %v", err)
	}

	// Fake every flat file at 3 GiB. Two files alone exceed 4 GiB, and the
	// 5 GiB target is above the wrapped value but below the real total.
	const fakeSize = uint32(3) << 30
	pdb.blkStore.fileSizeFunc = func(uint32) (uint32, error) { return fakeSize, nil }
	pdb.sjStore.fileSizeFunc = func(uint32) (uint32, error) { return fakeSize, nil }

	keepHeight := blocks[len(blocks)-1].Height()
	var earliest int32
	err = idb.Update(func(tx database.Tx) error {
		var e error
		earliest, e = tx.PruneBlocks(uint64(5)<<30, keepHeight)
		return e
	})
	if err != nil {
		t.Fatalf("PruneBlocks: %v", err)
	}
	if earliest == -1 {
		t.Fatal("PruneBlocks made no progress: total size wrapped below the target")
	}
}
