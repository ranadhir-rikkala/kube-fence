package ledger

import (
	"testing"
	"time"
)

func TestMemoryLookupRespectsWindow(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	m := NewMemory(time.Hour)
	if err := m.Record(Entry{Key: "a", At: t0}); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name  string
		key   string
		since time.Time
		want  bool
	}{
		{"inside window", "a", t0.Add(-time.Minute), true},
		{"boundary is inclusive", "a", t0, true},
		{"outside window", "a", t0.Add(time.Second), false},
		{"unknown key", "b", t0.Add(-time.Hour), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, got := m.Lookup(tt.key, tt.since); got != tt.want {
				t.Fatalf("Lookup(%q) = %v, want %v", tt.key, got, tt.want)
			}
		})
	}
}

func TestMemoryCountFilters(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	m := NewMemory(time.Hour)
	for i, e := range []Entry{
		{Key: "1", Namespace: "prod", Operation: "DELETE"},
		{Key: "2", Namespace: "prod", Operation: "DELETE"},
		{Key: "3", Namespace: "prod", Operation: "UPDATE"},
		{Key: "4", Namespace: "dev", Operation: "DELETE"},
	} {
		e.At = t0.Add(time.Duration(i) * time.Minute)
		_ = m.Record(e)
	}

	tests := []struct {
		name  string
		f     Filter
		since time.Time
		want  int
	}{
		{"all", Filter{}, t0, 4},
		{"prod deletes", Filter{Namespace: "prod", Operation: "DELETE"}, t0, 2},
		{"prod deletes after first", Filter{Namespace: "prod", Operation: "DELETE"}, t0.Add(time.Minute), 1},
		{"any namespace deletes", Filter{Operation: "DELETE"}, t0, 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := m.Count(tt.f, tt.since); got != tt.want {
				t.Fatalf("Count = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestMemoryPrunesOldEntries(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	m := NewMemory(10 * time.Minute)
	_ = m.Record(Entry{Key: "old", At: t0})
	_ = m.Record(Entry{Key: "new", At: t0.Add(11 * time.Minute)})

	if got := len(m.Entries()); got != 1 {
		t.Fatalf("entries after prune = %d, want 1", got)
	}
	if _, ok := m.Lookup("old", time.Time{}); ok {
		t.Fatal("pruned entry still visible")
	}
	if _, ok := m.Lookup("new", time.Time{}); !ok {
		t.Fatal("fresh entry missing")
	}
}
