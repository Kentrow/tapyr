// Package state is the persistent state store (see docs/spec/02-architecture.md,
// "State" and ADR 0005): quota ledger, run history and metadata. Access goes
// through the Store interface so that another backend can be added later.
package state

import (
	"context"
	"time"
)

// RunsKept is the number of runs kept per job instance.
const RunsKept = 50

// Store persists Tapyr's state.
type Store interface {
	// Meta returns a metadata value.
	Meta(ctx context.Context, key string) (string, bool, error)
	// SetMeta stores a metadata value.
	SetMeta(ctx context.Context, key, value string) error
	// LoadLedger returns every quota ledger entry.
	LoadLedger(ctx context.Context) ([]LedgerEntry, error)
	// SaveLedger replaces the given entries (by limit and scope).
	SaveLedger(ctx context.Context, entries []LedgerEntry) error
	// AddRun records a run and prunes the history of its job instance.
	AddRun(ctx context.Context, run RunRecord) error
	// Runs returns the most recent runs of a job instance, newest first.
	Runs(ctx context.Context, job, instance string, limit int) ([]RunRecord, error)
	// RunsOfJob returns the most recent runs of every instance of a job.
	RunsOfJob(ctx context.Context, job string, limit int) ([]RunRecord, error)
	// SaveInventory replaces the inventory items of a job instance.
	SaveInventory(ctx context.Context, instance string, items []InventoryItem, at time.Time) error
	// LoadInventory returns the inventory items of a job instance and when
	// they were saved; ok is false when there are none.
	LoadInventory(ctx context.Context, instance string) (items []InventoryItem, at time.Time, ok bool, err error)
	// DeleteInventory removes the inventory items of a job instance.
	DeleteInventory(ctx context.Context, instance string) error
	// SaveSeries replaces the series registry state of the given instances
	// and deletes the removed ones.
	SaveSeries(ctx context.Context, instances []SeriesSnapshot, removed []string) error
	// LoadSeries returns the series registry state of every instance.
	LoadSeries(ctx context.Context) ([]SeriesSnapshot, error)
	// SaveToken stores an auth token (sealed by the caller) under a session key.
	SaveToken(ctx context.Context, key string, sealed []byte, expiry time.Time) error
	// LoadToken returns the token of a session key; ok is false when none.
	LoadToken(ctx context.Context, key string) (sealed []byte, expiry time.Time, ok bool, err error)
	// DeleteToken removes the token of a session key.
	DeleteToken(ctx context.Context, key string) error
	// SaveAccumulators replaces the reconstructed counters of the given
	// instances and deletes those of the removed ones.
	SaveAccumulators(ctx context.Context, instances []AccumulatorSnapshot, removed []string) error
	// LoadAccumulators returns the reconstructed counters of every instance.
	LoadAccumulators(ctx context.Context) ([]AccumulatorSnapshot, error)
	// Ping checks that the store answers.
	Ping(ctx context.Context) error
	// Close releases the store.
	Close() error
}

// LedgerEntry is the persisted state of one quota limit for one scope key.
type LedgerEntry struct {
	// Limit is provider/limit.
	Limit string
	Scope string
	// WindowStart identifies the fixed window; zero for rolling windows.
	WindowStart       time.Time
	Used              int64
	Observed          bool
	ObservedRemaining int64
	ObservedMax       int64
	UsedAtObservation int64
	ExhaustedUntil    time.Time
	// Accounts contributing to the scope (for the budget share).
	Accounts []string
	// Minutes are the per-minute buckets of rolling windows (minute index
	// since the epoch -> count).
	Minutes   map[int64]int64
	UpdatedAt time.Time
}

// AccumulatorSnapshot holds the reconstructed counters of a job instance.
type AccumulatorSnapshot struct {
	Instance string
	Series   []Accumulated
}

// Accumulated is one reconstructed counter: its series key, running total
// and the last window counted.
type Accumulated struct {
	Series string
	Total  float64
	Window string
}

// SeriesSnapshot is the series registry state of a job instance.
type SeriesSnapshot struct {
	Instance    string
	LastSuccess time.Time
	// Series are the series keys of the last successful run.
	Series []string
	// Sent is the last source timestamp sent, by output and series key.
	Sent map[string]map[string]time.Time
}

// InventoryItem is a cached inventory item: its key and its fields as JSON.
type InventoryItem struct {
	Key    string
	Fields []byte
}

// RunRecord is the outcome of one run.
type RunRecord struct {
	Job      string
	Instance string
	RunID    string
	Started  time.Time
	Ended    time.Time
	// Result is success, partial, failed, skipped_budget, skipped_overlap
	// or skipped_circuit.
	Result   string
	Requests int
	Retries  int
	Pages    int
	Items    int
	Samples  int
	// Error is a redacted summary, empty on success.
	Error string
	// ErrorClass classifies the error: the class of the failed request
	// (auth, rate_limited, transient, client, decode), mapping for a
	// response that does not map, or the reason of a budget refusal
	// (quota_exhausted, penalized, circuit_open, deadline). ErrorStatus is
	// the HTTP status, ErrorSubject the templated endpoint or the limit
	// that refused, ErrorUntil when a refusal is expected to end.
	ErrorClass   string
	ErrorStatus  int
	ErrorSubject string
	ErrorUntil   time.Time
}
