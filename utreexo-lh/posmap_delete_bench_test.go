//go:build linux

package utreexo

import (
	"encoding/binary"
	"os"
	"strconv"
	"syscall"
	"testing"
)

// splitmix is the hash mixer used to spread synthetic keys over the map.
func splitmix(x uint64) uint64 {
	x += 0x9E3779B97F4A7C15
	x = (x ^ (x >> 30)) * 0xBF58476D1CE4E5B9
	x = (x ^ (x >> 27)) * 0x94D049BB133111EB
	return x ^ (x >> 31)
}

// benchHash derives a full-range 32-byte key from an index so the benchmark
// never has to hold the key set in memory (the container's cgroup is far
// smaller than the map it exercises).
func benchHash(i uint64) Hash {
	var h Hash
	binary.LittleEndian.PutUint64(h[:8], splitmix(i+1))
	binary.LittleEndian.PutUint64(h[8:16], splitmix(i^0xA5A5A5A5A5A5A5A5))
	return h
}

// BenchmarkProcessDeletionsReorder measures Forest.processDeletions on a
// position map larger than the container's memory limit, so the map cannot be
// cached and every lookup is a page miss. The same binary runs on the base and
// reordered commits: the code under test, not the benchmark, picks the access
// order. Reported metrics are ns/op plus minor and major faults per op.
//
// Sized so the whole run fits a cgroup: BENCH_MAP entries of EH prefix slots
// are ~BENCH_MAP*25 B of ctrl file, dimensionless in Go heap because the keys
// are derived, not stored.
func BenchmarkProcessDeletionsReorder(b *testing.B) {
	n := envUint("BENCH_MAP", 20_000_000)
	k := int(envUint("BENCH_DELS", 8192))

	// BENCH_DIR persists the built map across runs, so the expensive build
	// happens once without a memory cap and the A/B runs only measure the
	// lookup/delete pass. A fixed non-zero consistency hash makes the reuse
	// path take the table instead of rebuilding it.
	dir := os.Getenv("BENCH_DIR")
	if dir == "" {
		dir = b.TempDir()
	} else if err := os.MkdirAll(dir, 0o755); err != nil {
		b.Fatal(err)
	}
	consistency := [32]byte{0xE, 0x1, 0x2, 0x3}

	pm, needsRebuild, err := newPositionMap("eh", dir+"/ctrl", dir+"/slots", n, consistency, nil, 0)
	if err != nil {
		b.Fatalf("newPositionMap: %v", err)
	}
	defer pm.Close()

	if needsRebuild {
		for i := uint64(0); i < n; i++ {
			if err := pm.Set(benchHash(i), packPosIndex(i, int32(i))); err != nil {
				b.Fatalf("build map at %d: %v", i, err)
			}
		}
		if err := pm.SetConsistencyHash(consistency); err != nil {
			b.Fatalf("set consistency hash: %v", err)
		}
	}

	f := &Forest{positionMap: pm, deletedLeafPositions: newDeletedBitmap(n)}

	rng := uint64(0x12345678)
	next := func() uint64 { rng = splitmix(rng); return rng }

	dels := make([]Hash, k)
	delsIdx := make([]uint64, k)

	var ru syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	startMin, startMaj := ru.Minflt, ru.Majflt

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Key selection is outside the timed region; it must not be charged to
		// the position-map pass.
		b.StopTimer()
		for j := 0; j < k; j++ {
			idx := next() % n
			delsIdx[j] = idx
			dels[j] = benchHash(idx)
		}
		b.StartTimer()

		addIdx, delPos, err := f.processDeletions(dels)
		if err != nil {
			b.Fatalf("processDeletions: %v", err)
		}

		// Restore the spent entries so the next iteration sees the same map.
		b.StopTimer()
		for j := 0; j < k; j++ {
			if delPos[j] != delsIdx[j] {
				b.Fatalf("returned position %d for index %d", delPos[j], delsIdx[j])
			}
			if err := pm.Set(dels[j], packPosIndex(delPos[j], addIdx[j])); err != nil {
				b.Fatalf("restore: %v", err)
			}
		}
	}
	b.StopTimer()

	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	b.ReportMetric(float64(ru.Minflt-startMin)/float64(b.N), "minflt/op")
	b.ReportMetric(float64(ru.Majflt-startMaj)/float64(b.N), "majflt/op")
}

func envUint(name string, def uint64) uint64 {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil && n > 0 {
			return n
		}
	}
	return def
}
