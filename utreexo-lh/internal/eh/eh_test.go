package eh

import (
	"encoding/binary"
	"fmt"
	"testing"
)

// testKey mirrors the benchmark harness's splitmix64 key derivation.
func testKey(seed, i uint64) [32]byte {
	var h [32]byte
	x := seed ^ (i + 0x9E3779B97F4A7C15)
	for j := 0; j < 4; j++ {
		x += 0x9E3779B97F4A7C15
		z := x
		z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
		z = (z ^ (z >> 27)) * 0x94D049BB133111EB
		z ^= z >> 31
		binary.LittleEndian.PutUint64(h[j*8:], z)
	}
	return h
}

func checkInvariants(t *testing.T, m *EHMap) {
	t.Helper()
	D := m.Depth()
	if m.DirSlots() != uint64(1)<<D {
		t.Fatalf("dirSlots=%d D=%d", m.DirSlots(), D)
	}
	perBucket := make(map[uint64]int)
	seen := make(map[uint64]bool)
	for i := uint64(0); i < m.DirSlots(); i++ {
		b := m.dirSlot(i)
		if b >= m.Buckets() {
			t.Fatalf("slot %d -> bucket %d >= buckets %d", i, b, m.Buckets())
		}
		d := m.bucketDepth(b)
		if d > D {
			t.Fatalf("bucket %d local depth %d > D %d", b, d, D)
		}
		if seen[b] {
			continue
		}
		seen[b] = true
		ctrl := m.ctrl(b)
		for s := 0; s < SlotCount; s++ {
			if ctrl[s]&ctrlFull == 0 {
				continue
			}
			perBucket[b]++
			if !m.prefix {
				continue
			}
			pfx := m.slotPrefix(b, uint64(s))
			h1 := binary.LittleEndian.Uint64(pfx[:8])
			got := h1 >> (64 - d)
			want := i >> (D - d)
			if got != want {
				t.Fatalf("slot %d bucket %d depth %d: key prefix %d != slot prefix %d",
					i, b, d, got, want)
			}
		}
	}
	for b, n := range perBucket {
		if n > SlotCount {
			t.Fatalf("bucket %d holds %d > %d", b, n, SlotCount)
		}
	}
	if m.PlaceFull() != 0 {
		t.Fatalf("placeFull=%d", m.PlaceFull())
	}
	iter := uint64(0)
	m.ForEach(func(uint64) { iter++ })
	if iter != m.Count() {
		t.Fatalf("ForEach=%d Count=%d", iter, m.Count())
	}
}

func TestEHInvariants(t *testing.T) {
	const N = uint64(1_000_000)
	for _, load := range []float64{0.4, 0.7, 0.8, 0.9} {
		t.Run(fmt.Sprintf("load%.1f", load), func(t *testing.T) {
			m, err := NewEHMap(t.TempDir(), load, true)
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			if err := m.Presize(N); err != nil {
				t.Fatal(err)
			}
			for i := uint64(0); i < N; i++ {
				if err := m.Set(testKey(1, i), i); err != nil {
					t.Fatalf("Set %d: %v", i, err)
				}
			}
			if m.Count() != N {
				t.Fatalf("Count=%d want %d", m.Count(), N)
			}
			checkInvariants(t, m)
			for j := uint64(0); j < 1000; j++ {
				i := (j*2654435761 + 17) % N
				v, ok, err := m.Get(testKey(1, i))
				if err != nil || !ok || v != i {
					t.Fatalf("Get hit %d: v=%d ok=%v err=%v", i, v, ok, err)
				}
			}
			for j := uint64(0); j < 1000; j++ {
				if _, ok, err := m.Get(testKey(1, N+j)); err != nil || ok {
					t.Fatalf("Get miss %d: ok=%v err=%v", N+j, ok, err)
				}
			}
			// Delete every other key.
			for i := uint64(0); i < N; i += 2 {
				ok, err := m.Delete(testKey(1, i))
				if err != nil || !ok {
					t.Fatalf("Delete %d: ok=%v err=%v", i, ok, err)
				}
			}
			if m.Count() != N/2 {
				t.Fatalf("after delete Count=%d want %d", m.Count(), N/2)
			}
			for i := uint64(0); i < N; i += 2 {
				if _, ok, _ := m.Get(testKey(1, i)); ok {
					t.Fatalf("deleted key %d still found", i)
				}
				if _, ok, _ := m.Get(testKey(1, i+1)); !ok {
					t.Fatalf("live key %d missing", i+1)
				}
			}
			checkInvariants(t, m)
			t.Logf("D=%d buckets=%d mapBytes=%d dirBytes=%d load=%.4f splits=%d dirDoubles=%d",
				m.Depth(), m.Buckets(), m.MapBytes(), m.DirBytes(),
				float64(m.Count())/float64(m.NumSlots()), m.Splits(), m.DirDoubles())
		})
	}
}

type heapSource struct{ data []byte }

func (s heapSource) HashAt(off int64) ([32]byte, error) {
	return *(*[32]byte)(s.data[off : off+32]), nil
}

func TestEHBare(t *testing.T) {
	const N = uint64(200_000)
	dir := t.TempDir()
	data := make([]byte, N*32)
	for i := uint64(0); i < N; i++ {
		h := testKey(7, i)
		copy(data[i*32:], h[:])
	}
	m, err := NewEHMap(dir, 0.8, false)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	m.SetVerifySource(heapSource{data: data}, (1<<47)-1)
	if err := m.Presize(N); err != nil {
		t.Fatal(err)
	}
	for i := uint64(0); i < N; i++ {
		if err := m.Set(testKey(7, i), i); err != nil {
			t.Fatal(err)
		}
	}
	if m.Count() != N {
		t.Fatalf("Count=%d", m.Count())
	}
	for i := uint64(0); i < N; i += 97 {
		if v, ok, err := m.Get(testKey(7, i)); err != nil || !ok || v != i {
			t.Fatalf("Get %d: %d %v %v", i, v, ok, err)
		}
	}
	checkInvariants(t, m)
	for i := uint64(0); i < N/2; i++ {
		if _, err := m.Delete(testKey(7, i)); err != nil {
			t.Fatal(err)
		}
	}
	if m.Count() != N/2 {
		t.Fatalf("after delete Count=%d", m.Count())
	}
}
