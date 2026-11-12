// Package ledger records the side effects that agents have been allowed to
// make against the cluster, keyed by an idempotency key.
package ledger

import "time"

type Entry struct {
	Key       string    `json:"key"`
	Agent     string    `json:"agent"`
	Operation string    `json:"operation"`
	Resource  string    `json:"resource"`
	Namespace string    `json:"namespace"`
	Name      string    `json:"name"`
	At        time.Time `json:"at"`
}

type Filter struct {
	Namespace string
	Operation string
}

func (f Filter) Matches(e Entry) bool {
	return (f.Namespace == "" || f.Namespace == e.Namespace) &&
		(f.Operation == "" || f.Operation == e.Operation)
}

// Ledger is the storage contract for recorded effects. Implementations must be
// safe for concurrent use.
type Ledger interface {
	Lookup(key string, since time.Time) (Entry, bool)
	Count(f Filter, since time.Time) int
	Record(e Entry) error
}
