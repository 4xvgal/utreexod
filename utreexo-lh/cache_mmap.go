//go:build linux

package utreexo

import (
	"os"
	"sync"

	"github.com/utreexo/utreexo/internal/mmapcache"
)

// mmapCacheStore adapts the mmapcache stores to the unexported cacheStore
// interface used by cachedRWS. Exactly one of s (legacy file-offset-addressed
// store) and d (dense entry-indexed store) is non-nil.
type mmapCacheStore struct {
	s *mmapcache.Store
	d *mmapcache.Dense
}

// cacheImpl is the concrete cache type cachedRWS stores. Using a concrete
// type (rather than the cacheStore interface) lets escape analysis see
// through cache.put so callers can pass values without heap-allocating.
type cacheImpl = mmapCacheStore

// denseEnabled reports whether the WAL mmap cache packs entries densely.
// Env-gated so the same binary can run both sides of an A/B:
// UTREEXO_WAL_DENSE=1 selects the dense store, anything else the legacy one.
// Overridable in tests.
var denseEnabled = sync.OnceValue(func() bool {
	return os.Getenv("UTREEXO_WAL_DENSE") == "1"
})

func newMmapCacheStore(entrySize int, maxBytes int64) (*mmapCacheStore, error) {
	if denseEnabled() {
		d, err := mmapcache.NewDense(entrySize, maxBytes)
		if err != nil {
			return nil, err
		}
		return &mmapCacheStore{d: d}, nil
	}
	s, err := mmapcache.New(entrySize, maxBytes)
	if err != nil {
		return nil, err
	}
	return &mmapCacheStore{s: s}, nil
}

func (m *mmapCacheStore) get(offset int64) ([]byte, bool) {
	if m.d != nil {
		return m.d.Get(offset)
	}
	return m.s.Get(offset)
}

func (m *mmapCacheStore) put(offset int64, data []byte) error {
	if m.d != nil {
		return m.d.Put(offset, data)
	}
	return m.s.Put(offset, data)
}

func (m *mmapCacheStore) clear() {
	if m.d != nil {
		m.d.Clear()
		return
	}
	m.s.Clear()
}

func (m *mmapCacheStore) forEach(fn func(int64, []byte)) {
	if m.d != nil {
		m.d.ForEach(fn)
		return
	}
	m.s.ForEach(fn)
}

func (m *mmapCacheStore) overflowed() bool {
	if m.d != nil {
		return m.d.Overflowed()
	}
	return m.s.Overflowed()
}

func (m *mmapCacheStore) count() int {
	if m.d != nil {
		return m.d.Count()
	}
	return m.s.Count()
}

func (m *mmapCacheStore) entrySize() int {
	if m.d != nil {
		return m.d.EntrySize()
	}
	return m.s.EntrySize()
}

func (m *mmapCacheStore) close() {
	if m.d != nil {
		m.d.Close()
		return
	}
	m.s.Close()
}
