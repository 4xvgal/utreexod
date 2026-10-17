// Copyright (c) 2026 The utreexod developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package ffldb

import (
	"bytes"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/utreexo/utreexod/database"
)

// TestProofRolloverDoesNotOverwriteExistingProof ensures proof storage never
// rolls to a different file number independently of its corresponding block.
// It also ensures returning to an earlier proof file cannot make a subsequent
// write overwrite a proof in a later file.
func TestProofRolloverDoesNotOverwriteExistingProof(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ffldb-proof-rollover")
	idb, err := database.Create(dbType, dbPath, blockDataNet)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer idb.Close()

	blocks, err := loadBlocks(t, blockDataFile, blockDataNet)
	if err != nil {
		t.Fatalf("loadBlocks: %v", err)
	}

	// Keep all four blocks in block file 0 while setting a proof file limit
	// that would independently roll over after a single 60-byte proof.  Each
	// proof record has 12 bytes of framing, so the records below occupy 72,
	// 72, 22, and 72 bytes.  Proof partitioning must follow the block files
	// despite that nominal proof file limit.
	pdb := idb.(*db)
	pdb.proofStore.maxBlockFileSize = 100
	proofs := [][]byte{
		bytes.Repeat([]byte{0x11}, 60),
		bytes.Repeat([]byte{0x22}, 60),
		bytes.Repeat([]byte{0x33}, 10),
		bytes.Repeat([]byte{0x44}, 60),
	}

	err = idb.Update(func(tx database.Tx) error {
		for i, proof := range proofs {
			block := blocks[i+1]
			if err := tx.StoreBlock(block); err != nil {
				return err
			}
			if err := tx.StoreUtreexoProof(block.Hash(), proof); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("store blocks and proofs: %v", err)
	}

	err = idb.View(func(tx database.Tx) error {
		blockIdx := tx.Metadata().Bucket(blockIdxBucketName)
		if blockIdx == nil {
			return fmt.Errorf("block index bucket missing")
		}
		proofIdx := tx.Metadata().Bucket(proofIdxBucketName)
		if proofIdx == nil {
			return fmt.Errorf("proof index bucket missing")
		}

		for i, want := range proofs {
			hash := blocks[i+1].Hash()
			blockRow := blockIdx.Get(hash[:])
			if blockRow == nil {
				return fmt.Errorf("block index row %d missing", i)
			}
			proofRow := proofIdx.Get(hash[:])
			if proofRow == nil {
				return fmt.Errorf("proof index row %d missing", i)
			}

			blockLoc := deserializeBlockLoc(blockRow)
			proofLoc := deserializeBlockLoc(proofRow)
			if proofLoc.blockFileNum != blockLoc.blockFileNum {
				t.Errorf("proof %d stored in file %d, want corresponding block file %d",
					i, proofLoc.blockFileNum, blockLoc.blockFileNum)
			}

			got, err := tx.FetchUtreexoProof(hash)
			if err != nil {
				return err
			}
			if !bytes.Equal(got, want) {
				t.Errorf("proof %d was overwritten: got %x, want %x",
					i, got, want)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("fetch proofs: %v", err)
	}
}

// TestProofStoreAllowsBackwardFileWrite ensures a delayed proof is appended to
// its block's file without moving the current proof write cursor backward.
func TestProofStoreAllowsBackwardFileWrite(t *testing.T) {
	require := require.New(t)

	dbPath := filepath.Join(t.TempDir(), "ffldb-proof-backward")
	idb, err := database.Create(dbType, dbPath, blockDataNet)
	require.NoError(err)
	defer idb.Close()

	blocks, err := loadBlocks(t, blockDataFile, blockDataNet)
	require.NoError(err)
	firstBlockBytes, err := blocks[1].Bytes()
	require.NoError(err)
	secondBlockBytes, err := blocks[2].Bytes()
	require.NoError(err)

	// Make the first two blocks exactly fill block file 0, putting the third
	// block in file 1.
	pdb := idb.(*db)
	firstBlockSize := len(pdb.blkStore.serializeBlockRecord(firstBlockBytes))
	secondBlockSize := len(pdb.blkStore.serializeBlockRecord(secondBlockBytes))
	pdb.blkStore.maxBlockFileSize = uint32(firstBlockSize + secondBlockSize)
	err = idb.Update(func(tx database.Tx) error {
		if err := tx.StoreBlock(blocks[1]); err != nil {
			return err
		}
		if err := tx.StoreBlock(blocks[2]); err != nil {
			return err
		}
		return tx.StoreBlock(blocks[3])
	})
	require.NoError(err)

	// Verify the third block is in file 1 before storing its proof.
	err = idb.View(func(tx database.Tx) error {
		blockIdx := tx.Metadata().Bucket(blockIdxBucketName)
		require.NotNil(blockIdx)
		hash := blocks[3].Hash()
		row := blockIdx.Get(hash[:])
		require.NotNil(row)
		loc := deserializeBlockLoc(row)
		require.Equal(uint32(1), loc.blockFileNum)
		return nil
	})
	require.NoError(err)

	// Put an existing proof in file 0 so the delayed proof must append after
	// it rather than create an empty historical file.
	existingProof := []byte("existing proof in block file zero")
	err = idb.Update(func(tx database.Tx) error {
		return tx.StoreUtreexoProof(blocks[1].Hash(), existingProof)
	})
	require.NoError(err)

	// Establish proof file 1 as the current proof file.
	forwardProof := []byte("proof in block file one")
	err = idb.Update(func(tx database.Tx) error {
		return tx.StoreUtreexoProof(blocks[3].Hash(), forwardProof)
	})
	require.NoError(err)

	proofWC := pdb.proofStore.writeCursor
	proofWC.RLock()
	fileNum, offset := proofWC.curFileNum, proofWC.curOffset
	proofWC.RUnlock()

	// Store a delayed proof for a block in file 0.
	delayedProof := []byte("late proof")
	err = idb.Update(func(tx database.Tx) error {
		return tx.StoreUtreexoProof(blocks[2].Hash(), delayedProof)
	})
	require.NoError(err)

	// The delayed write must not change the current proof file or offset.
	proofWC.RLock()
	gotFileNum, gotOffset := proofWC.curFileNum, proofWC.curOffset
	proofWC.RUnlock()
	require.Equal(fileNum, gotFileNum)
	require.Equal(offset, gotOffset)

	// All proofs must remain in the files matching their blocks.
	err = idb.View(func(tx database.Tx) error {
		proofIdx := tx.Metadata().Bucket(proofIdxBucketName)
		require.NotNil(proofIdx)
		proofs := []struct {
			block int
			file  uint32
			want  []byte
		}{
			{block: 1, file: 0, want: existingProof},
			{block: 2, file: 0, want: delayedProof},
			{block: 3, file: 1, want: forwardProof},
		}
		for _, proof := range proofs {
			hash := blocks[proof.block].Hash()
			row := proofIdx.Get(hash[:])
			require.NotNil(row)
			loc := deserializeBlockLoc(row)
			require.Equal(proof.file, loc.blockFileNum)

			got, err := tx.FetchUtreexoProof(hash)
			require.NoError(err)
			require.Equal(proof.want, got)
		}
		return nil
	})
	require.NoError(err)

	// Retrying identical bytes is a no-op, while different bytes conflict.
	size, err := pdb.proofStore.fileSizeFunc(0)
	require.NoError(err)
	err = idb.Update(func(tx database.Tx) error {
		return tx.StoreUtreexoProof(blocks[2].Hash(), delayedProof)
	})
	require.NoError(err)
	retrySize, err := pdb.proofStore.fileSizeFunc(0)
	require.NoError(err)
	require.Equal(size, retrySize)

	err = idb.Update(func(tx database.Tx) error {
		return tx.StoreUtreexoProof(blocks[2].Hash(), []byte("different"))
	})
	require.Error(err)
}
