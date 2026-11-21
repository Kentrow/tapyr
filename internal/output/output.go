// Package output defines the interface of the outputs collected samples are
// sent to (see docs/spec/06-mapping-and-outputs.md, "Outputs"). The
// implementations live in subpackages.
package output

import (
	"context"
	"time"

	"github.com/kentrow/tapyr/internal/model"
)

// Output receives the batches of job runs.
type Output interface {
	// Name is the name of the Output document.
	Name() string
	// Enqueue hands a batch over without blocking; when the queue is full
	// the output drops samples with accounting.
	Enqueue(batch model.Batch)
	// Retire withdraws the series of a job instance (removed, or without a
	// successful run within staleAfter): removed from a pull exposition,
	// marked stale by push outputs.
	Retire(instance string, stale []model.Series)
	// Run is the sender loop; it returns when ctx is done.
	Run(ctx context.Context) error
	// Flush sends what is queued, on shutdown.
	Flush(ctx context.Context) error
	// State describes the queue and the sends.
	State() State
}

// State describes the queue and the sends of an output.
type State struct {
	// Queue is the number of samples waiting; Capacity bounds it (0 for
	// the pull output).
	Queue, Capacity int
	// Sent and Dropped count samples since the output started.
	Sent, Dropped int64
	// LastSuccess is the last successful send, zero before the first.
	LastSuccess time.Time
	// LastError is the last failed send, redacted, with its time.
	LastError   string
	LastErrorAt time.Time
	// Buffer is the disk buffer of the queue (ADR 0015), nil without one.
	Buffer *BufferState
}

// BufferState describes the disk buffer of a push output.
type BufferState struct {
	// Bytes is the size of its files, MaxBytes its maxSize.
	Bytes, MaxBytes int64
	// Points have not been acknowledged by the backend yet; Oldest is the
	// time the oldest of them was queued, zero when none.
	Points int
	Oldest time.Time
	// MaxAge is its maxAge: older points are dropped.
	MaxAge time.Duration
	// Writable is false while writes fail and the queue keeps its points
	// in memory only.
	Writable bool
}
