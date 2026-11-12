package ledger

import (
	"sync"
	"time"
)

// Memory is an in-process Ledger. Entries older than the retention period are
// pruned on write.
type Memory struct {
	mu        sync.RWMutex
	retention time.Duration
	entries   []Entry
	byKey     map[string]int
}

func NewMemory(retention time.Duration) *Memory {
	return &Memory{retention: retention, byKey: map[string]int{}}
}

func (m *Memory) Lookup(key string, since time.Time) (Entry, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	i, ok := m.byKey[key]
	if !ok || m.entries[i].At.Before(since) {
		return Entry{}, false
	}
	return m.entries[i], true
}

func (m *Memory) Count(f Filter, since time.Time) int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	n := 0
	for _, e := range m.entries {
		if !e.At.Before(since) && f.Matches(e) {
			n++
		}
	}
	return n
}

func (m *Memory) Record(e Entry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.prune(e.At.Add(-m.retention))
	m.entries = append(m.entries, e)
	m.byKey[e.Key] = len(m.entries) - 1
	return nil
}

func (m *Memory) Entries() []Entry {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]Entry(nil), m.entries...)
}

func (m *Memory) prune(cutoff time.Time) {
	if m.retention <= 0 {
		return
	}
	kept := m.entries[:0]
	for _, e := range m.entries {
		if !e.At.Before(cutoff) {
			kept = append(kept, e)
		}
	}
	m.entries = kept
	clear(m.byKey)
	for i, e := range m.entries {
		m.byKey[e.Key] = i
	}
}
