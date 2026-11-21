package collector

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kentrow/tapyr/internal/config"
)

// TestOpenBuffer checks where the disk buffer of an output is opened, and
// that a buffer that cannot be opened leaves the output in memory, said in
// the log.
func TestOpenBuffer(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "data", "tapyr.db")
	var logs bytes.Buffer
	c := &Collector{logger: slog.New(slog.NewTextHandler(&logs, nil))}
	out := &config.Output{
		Metadata: config.ObjectMeta{Name: "vm"},
		Spec: config.OutputSpec{Type: config.OutputRemoteWrite, Queue: &config.Queue{
			Disk: &config.DiskBuffer{MaxSize: config.Size(64 << 20), MaxAge: config.Duration(2 * time.Hour)},
		}},
	}
	q := c.openBuffer(out, state)
	if q.Buffer == nil || q.MaxAge != 2*time.Hour {
		t.Fatalf("buffer not opened: %+v\n%s", q, logs.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "data", "buffer", "vm")); err != nil {
		t.Errorf("default directory: %v", err)
	}

	// The directory is held: a second output on it stays in memory.
	again := c.openBuffer(out, state)
	if again.Buffer != nil {
		t.Error("a buffer held by another queue was opened twice")
	}
	if !strings.Contains(logs.String(), "the queue keeps the samples in memory only") {
		t.Errorf("log = %s", logs.String())
	}
	if err := q.Buffer.Close(); err != nil {
		t.Fatal(err)
	}

	// No disk block, no buffer.
	out.Spec.Queue.Disk = nil
	if q := c.openBuffer(out, state); q.Buffer != nil {
		t.Error("a buffer was opened without a disk block")
	}
}
