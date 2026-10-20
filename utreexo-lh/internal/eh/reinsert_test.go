//go:build linux

package eh

import "testing"

// TestEHDeleteThenSetReinserts pins the delete -> reinsert round trip. The
// processDeletions bench restores every deleted entry with Set right after
// Delete; if that silently drops a key, the next pass reports "not found".
func TestEHDeleteThenSetReinserts(t *testing.T) {
	const n = 20000

	m, _, err := NewEHMapWithHash(t.TempDir(), 0.8, true, [32]byte{0xC0, 0xFF})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if err := m.Presize(n); err != nil {
		t.Fatal(err)
	}

	keys := make([][32]byte, n)
	for i := range keys {
		keys[i] = testKey(0x1234, uint64(i))
		if err := m.Set(keys[i], uint64(i)); err != nil {
			t.Fatalf("set %d: %v", i, err)
		}
	}

	for i := range keys {
		ok, err := m.Delete(keys[i])
		if err != nil {
			t.Fatalf("delete %d: %v", i, err)
		}
		if !ok {
			t.Fatalf("delete %d: not found", i)
		}
		if err := m.Set(keys[i], uint64(i)); err != nil {
			t.Fatalf("reinsert %d: %v", i, err)
		}
		if _, ok, err := m.Get(keys[i]); err != nil || !ok {
			t.Fatalf("reinsert %d: lost (found=%v err=%v)", i, ok, err)
		}
	}

	for i := range keys {
		if v, ok, _ := m.Get(keys[i]); !ok || v != uint64(i) {
			t.Fatalf("final get %d: found=%v v=%d", i, ok, v)
		}
	}
	if got := m.Count(); got != n {
		t.Fatalf("count=%d want %d", got, n)
	}
}
