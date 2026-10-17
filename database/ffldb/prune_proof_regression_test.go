// Copyright (c) 2026 The utreexod developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package ffldb

import (
	"errors"
	"os"
	"syscall"
	"testing"

	"github.com/utreexo/utreexod/database"
)

// pruneWithProofFileStatError stages a block-file prune, then injects the
// provided proof-file stat error during commit.
func pruneWithProofFileStatError(t *testing.T, statErr error) error {
	t.Helper()

	dbPath := t.TempDir()
	idb, err := database.Create(dbType, dbPath, blockDataNet)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer idb.Close()

	pdb := idb.(*db)
	pdb.blkStore.deleteFileFunc = func(uint32) error { return nil }
	pdb.sjStore.deleteFileFunc = func(uint32) error { return nil }

	proofStatErr := &os.PathError{
		Op:   "stat",
		Path: proofFilePath(dbPath, 0),
		Err:  statErr,
	}
	pdb.proofStore.fileSizeFunc = func(uint32) (uint32, error) {
		return 0, proofStatErr
	}
	pdb.proofStore.deleteFileFunc = func(uint32) error {
		t.Fatal("proof deletion must not be attempted after its stat fails")
		return nil
	}

	return idb.Update(func(dbTx database.Tx) error {
		dbTx.(*transaction).pendingDelFileNums = []uint32{0}
		return nil
	})
}

// TestPruneCommitPropagatesProofFileStatError ensures a proof file stat error
// other than nonexistence aborts pruning instead of being treated as though the
// optional proof file were absent.
func TestPruneCommitPropagatesProofFileStatError(t *testing.T) {
	err := pruneWithProofFileStatError(t, syscall.EACCES)
	if !errors.Is(err, syscall.EACCES) {
		t.Fatalf("prune commit error: got %v, want permission denied", err)
	}
}

// TestPruneCommitIgnoresMissingProofFile ensures databases without a proof
// file corresponding to every pruned block file remain prunable.
func TestPruneCommitIgnoresMissingProofFile(t *testing.T) {
	err := pruneWithProofFileStatError(t, os.ErrNotExist)
	if err != nil {
		t.Fatalf("prune commit with missing proof file: %v", err)
	}
}
