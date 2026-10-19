//go:build linux

package utreexo

import (
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// VerifyHash cost benchmark (linux only, test-only).
//
// Runs the same insert + lookup trace against two position maps that differ
// ONLY in how a lookup confirms the key:
//   - lhV1Map (v1): 7-bit H2 + packed, confirms via src.HashAt(pos*32) -> reads the data file
//   - lhPositionMap (v2): embeds a 16B key prefix, never reads the data file
//
// The data file is a synthetic forest_data: leaf p is splitmix64(4p..4p+3).
// Entries point at random leaves spread over the whole file, like a real
// forest where live leaves << total positions.
//
// Run (cold page cache, cgroup-limited so the file exceeds RAM):
//
//	docker run --rm --privileged --memory=2g -v $PWD:/src -w /src \
//	  golang:1.25-bookworm go test -run TestVerifyHashBench -v -timeout 60m .
//
// Env knobs: LHBENCH_FULL=1 (4GiB file), LHBENCH_OPS (lookups), LHBENCH_KEEP=1
// (reuse an existing data file).

const lhbPositionMask = (uint64(1) << 47) - 1

func lhbSplitmix(x uint64) uint64 {
	x += 0x9E3779B97F4A7C15
	x = (x ^ (x >> 30)) * 0xBF58476D1CE4E5B9
	x = (x ^ (x >> 27)) * 0x94D049BB133111EB
	return x ^ (x >> 31)
}

// lhbLeafAt is the synthetic leaf hash for position p; it must match what the
// generated data file holds at offset p*32.
func lhbLeafAt(p uint64) [32]byte {
	var h [32]byte
	for j := 0; j < 4; j++ {
		binary.LittleEndian.PutUint64(h[j*8:], lhbSplitmix(4*p+uint64(j)))
	}
	return h
}

type lhbFileSource struct {
	f     *os.File
	calls uint64
	bytes uint64
}

func (s *lhbFileSource) HashAt(off int64) ([32]byte, error) {
	var h [32]byte
	s.calls++
	s.bytes += 32
	_, err := s.f.ReadAt(h[:], off)
	return h, err
}

func genDataFile(tb testing.TB, path string, leaves uint64) {
	want := int64(leaves * 32)
	if fi, err := os.Stat(path); err == nil && fi.Size() == want && os.Getenv("LHBENCH_KEEP") != "" {
		return
	}
	f, err := os.Create(path)
	if err != nil {
		tb.Fatal(err)
	}
	defer f.Close()
	const bufWords = 1 << 20 // 8MiB
	buf := make([]byte, bufWords*8)
	word := uint64(0)
	remaining := leaves * 4
	for remaining > 0 {
		n := uint64(bufWords)
		if n > remaining {
			n = remaining
		}
		for i := uint64(0); i < n; i++ {
			binary.LittleEndian.PutUint64(buf[i*8:], lhbSplitmix(word))
			word++
		}
		if _, err := f.Write(buf[:n*8]); err != nil {
			tb.Fatal(err)
		}
		remaining -= n
		if written := (leaves*4 - remaining) * 8; written%(1<<28) < n*8 {
			tb.Logf("      gen data: %.1fGiB / %.1fGiB", float64(written)/(1<<30), float64(leaves*32)/(1<<30))
		}
	}
}

type lhbStats struct {
	readBytes, majflt uint64
}

func lhbReadStats(tb testing.TB) lhbStats {
	var s lhbStats
	if io, err := os.ReadFile("/proc/self/io"); err == nil {
		for _, line := range strings.Split(string(io), "\n") {
			f := strings.Fields(line)
			if len(f) == 2 && f[0] == "read_bytes:" {
				s.readBytes, _ = strconv.ParseUint(f[1], 10, 64)
			}
		}
	}
	if stat, err := os.ReadFile("/proc/self/stat"); err == nil {
		f := strings.Fields(string(stat))
		if len(f) > 12 {
			s.majflt, _ = strconv.ParseUint(f[11], 10, 64) // field 12 = majflt
		}
	}
	return s
}

func lhbDropCaches(tb testing.TB) {
	out, err := exec.Command("sh", "-c", "sync; echo 3 > /proc/sys/vm/drop_caches").CombinedOutput()
	if err != nil {
		tb.Logf("drop_caches failed (need --privileged): %v %s", err, out)
		return
	}
	time.Sleep(500 * time.Millisecond)
}

type lhbPhase struct {
	name     string
	d        time.Duration
	ops      uint64
	reads    uint64
	logical  uint64
	diskRead uint64
	majflt   uint64
}

func (p lhbPhase) report(tb testing.TB) {
	tb.Logf("    %-22s %8.3fs  %9.0f ops/s  reads/op=%6.3f  HashAt=%.2fGB  disk_read=%.3fGB  majflt=%d",
		p.name, p.d.Seconds(), float64(p.ops)/p.d.Seconds(),
		float64(p.reads)/float64(p.ops),
		float64(p.logical)/(1<<30),
		float64(p.diskRead)/(1<<30), p.majflt)
}

// runTraceProg logs the phase that is about to start, then runs it.
func runTraceProg(tb testing.TB, impl, ctrl string, src *lhbFileSource, leaves, n, ops, expected uint64, mode string) (lhbPhase, lhbPhase) {
	tb.Logf("  ==> %s %s: build %d entries, then %d lookups", impl, mode, n, ops)
	return runTrace(tb, impl, ctrl, src, leaves, n, ops, expected)
}

// runTrace inserts n entries then does ops random lookups, returning the
// build and lookup phases plus the resulting ctrl file size.
func runTrace(tb testing.TB, impl, ctrl string, src *lhbFileSource, leaves, n, ops, expected uint64) (lhbPhase, lhbPhase) {
	var pm PositionMap
	switch impl {
	case "v1":
		m, _, err := newLHV1Map(ctrl, expected, [32]byte{9}, src, lhbPositionMask)
		if err != nil {
			tb.Fatal(err)
		}
		pm = m
	case "v2":
		m, _, err := newLHPositionMap(ctrl, expected, [32]byte{9}, nil, 0)
		if err != nil {
			tb.Fatal(err)
		}
		pm = m
	}
	defer pm.Close()

	pos := make([]uint32, n)
	rng := uint64(0x123456789abcdef)
	next := func() uint64 { rng = lhbSplitmix(rng); return rng }

	phase := func(name string, opCount uint64, fn func(prog func(uint64))) lhbPhase {
		src.calls, src.bytes = 0, 0
		a := lhbReadStats(tb)
		start := time.Now()
		last := start
		prog := func(done uint64) {
			if time.Since(last) >= 3*time.Second {
				tb.Logf("      ... %s %d/%d ops, %.1fs elapsed", name, done, opCount, time.Since(start).Seconds())
				last = time.Now()
			}
		}
		fn(prog)
		d := time.Since(start)
		b := lhbReadStats(tb)
		return lhbPhase{name: name, d: d, ops: opCount,
			reads: src.calls, logical: src.bytes,
			diskRead: b.readBytes - a.readBytes, majflt: b.majflt - a.majflt}
	}

	build := phase("build", n, func(prog func(uint64)) {
		for i := uint64(0); i < n; i++ {
			p := next() % leaves
			pos[i] = uint32(p)
			if err := pm.Set(lhbLeafAt(p), p); err != nil {
				tb.Fatalf("set %d: %v", i, err)
			}
			if i&0xfff == 0 {
				prog(i)
			}
		}
	})
	lookup := phase("lookup", ops, func(prog func(uint64)) {
		for i := uint64(0); i < ops; i++ {
			p := uint64(pos[next()%n])
			_, ok, err := pm.Get(lhbLeafAt(p))
			if err != nil {
				tb.Fatal(err)
			}
			if !ok {
				tb.Fatalf("missing key at pos %d", p)
			}
			if i&0xfff == 0 {
				prog(i)
			}
		}
	})
	return build, lookup
}

func TestVerifyHashBench(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Log("warning: not root, drop_caches will fail and page cache is not cold")
	}
	dir := t.TempDir()
	ops := uint64(2_000_000)
	if v := os.Getenv("LHBENCH_OPS"); v != "" {
		ops, _ = strconv.ParseUint(v, 10, 64)
	}

	type cfg struct {
		name   string
		leaves uint64
		n      uint64
	}
	cfgs := []cfg{{"small(64MiB file, fits page cache)", 2 << 20, 1 << 20}}
	if os.Getenv("LHBENCH_FULL") != "" {
		bigLeaves, bigN := uint64(1<<27), uint64(1<<20)
		if v := os.Getenv("LHBENCH_LEAVES"); v != "" {
			bigLeaves, _ = strconv.ParseUint(v, 10, 64)
		}
		if v := os.Getenv("LHBENCH_N"); v != "" {
			bigN, _ = strconv.ParseUint(v, 10, 64)
		}
		cfgs = append(cfgs, cfg{"large(4GiB file, > 2g cgroup)", bigLeaves, bigN})
	}

	for _, c := range cfgs {
		dataPath := filepath.Join(dir, fmt.Sprintf("forest_data_%d.dat", c.leaves))
		t.Logf("=== %s: leaves=%d entries=%d lookups=%d ===", c.name, c.leaves, c.n, ops)
		genDataFile(t, dataPath, c.leaves)
		f, err := os.Open(dataPath)
		if err != nil {
			t.Fatal(err)
		}
		src := &lhbFileSource{f: f}

		for _, impl := range []string{"v1", "v2"} {
			for _, expected := range []uint64{c.n, 1} {
				ctrl := filepath.Join(dir, fmt.Sprintf("ctrl_%s_%d.dat", impl, expected))
				os.Remove(ctrl)
				lhbDropCaches(t)
				mode := "presized"
				if expected == 1 {
					mode = "grow(tiny)"
				}
				build, lookup := runTraceProg(t, impl, ctrl, src, c.leaves, c.n, ops, expected, mode)
				build.report(t)
				lookup.report(t)
				if fi, err := os.Stat(ctrl); err == nil {
					t.Logf("    ctrl file: %.1fMB", float64(fi.Size())/(1<<20))
				}
			}
		}
		f.Close()
	}
}
