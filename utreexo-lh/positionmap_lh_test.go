//go:build linux

package utreexo

import (
	"encoding/binary"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
)

// TestLHPositionMapKeyPrefix exercises the v2 map end to end without a data
// file: bulk insert, full lookup, delete, and reopen from disk.
func TestLHPositionMapKeyPrefix(t *testing.T) {
	dir := t.TempDir()
	ctrl := filepath.Join(dir, "forest_posmap_ctrl.dat")
	consistency := [32]byte{0xAB}
	const n = 5000

	hashes := make([][32]byte, n)
	rng := rand.New(rand.NewSource(1))
	for i := range hashes {
		rng.Read(hashes[i][:])
	}

	// expectedEntries=1 starts the table tiny so the inserts force many splits
	// (the split path must not need the data file).
	m, rebuild, err := newLHPositionMap(ctrl, 1, consistency, nil, 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if !rebuild {
		t.Fatal("fresh file should report rebuild")
	}
	for i, h := range hashes {
		if err := m.Set(h, uint64(i)); err != nil {
			t.Fatalf("set %d: %v", i, err)
		}
	}
	if got := m.Count(); got != n {
		t.Fatalf("count = %d, want %d", got, n)
	}
	for i, h := range hashes {
		p, ok, err := m.Get(h)
		if err != nil || !ok || p != uint64(i) {
			t.Fatalf("get %d: p=%d ok=%v err=%v", i, p, ok, err)
		}
	}

	// Delete every other key.
	for i := 0; i < n; i += 2 {
		ok, err := m.Delete(hashes[i])
		if err != nil || !ok {
			t.Fatalf("delete %d: ok=%v err=%v", i, ok, err)
		}
	}
	if got := m.Count(); got != n/2 {
		t.Fatalf("count after delete = %d, want %d", got, n/2)
	}
	for i, h := range hashes {
		_, ok, err := m.Get(h)
		if err != nil {
			t.Fatalf("get %d: %v", i, err)
		}
		if want := i%2 == 1; ok != want {
			t.Fatalf("get %d: ok=%v want %v", i, ok, want)
		}
	}

	// The magic must be the v3 marker.
	var magic [8]byte
	f, err := os.Open(ctrl)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.ReadAt(magic[:], 0); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if string(magic[:3]) != "LH4" {
		t.Fatalf("magic = %q, want LH4", magic[:3])
	}

	if err := m.SetConsistencyHash(consistency); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen: must reuse (no rebuild) and recover the surviving entries.
	m2, rebuild, err := newLHPositionMap(ctrl, n, consistency, nil, 0)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if rebuild {
		t.Fatal("reopen should reuse the table")
	}
	if got := m2.Count(); got != n/2 {
		t.Fatalf("reopened count = %d, want %d", got, n/2)
	}
	for i, h := range hashes {
		p, ok, err := m2.Get(h)
		if err != nil {
			t.Fatalf("reopen get %d: %v", i, err)
		}
		if want := i%2 == 1; ok != want {
			t.Fatalf("reopen get %d: ok=%v want %v", i, ok, want)
		}
		if ok && p != uint64(i) {
			t.Fatalf("reopen get %d: p=%d", i, p)
		}
	}
	if err := m2.Close(); err != nil {
		t.Fatal(err)
	}

	// A different consistency hash must force a rebuild.
	m3, rebuild, err := newLHPositionMap(ctrl, n, [32]byte{0xCD}, nil, 0)
	if err != nil {
		t.Fatalf("open 3: %v", err)
	}
	if !rebuild {
		t.Fatal("consistency mismatch should rebuild")
	}
	if got := m3.Count(); got != 0 {
		t.Fatalf("rebuilt count = %d, want 0", got)
	}
	m3.Close()
}

// TestLHPositionMapBatch covers the Record-mode bulk path and that positions
// survive a split without any data file.
func TestLHPositionMapBatch(t *testing.T) {
	dir := t.TempDir()
	m, _, err := newLHPositionMap(filepath.Join(dir, "ctrl.dat"), 1, [32]byte{1}, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	const n = 4000
	b, err := m.BeginBatch(n)
	if err != nil {
		t.Fatal(err)
	}
	hashes := make([][32]byte, n)
	for i := range hashes {
		binary.LittleEndian.PutUint64(hashes[i][:8], uint64(i)*0x9E3779B97F4A7C15)
		hashes[i][8] = byte(i)
		if err := b.Insert(hashes[i], uint64(i)); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	for i, h := range hashes {
		p, ok, err := m.Get(h)
		if err != nil || !ok || p != uint64(i) {
			t.Fatalf("get %d: p=%d ok=%v err=%v", i, p, ok, err)
		}
	}
}
