package main

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestBufferHolds(t *testing.T) {
	// 512 MiB at 64 bytes per sample and 1000 samples per minute.
	if got := bufferHolds(512<<20, 1000); got.Round(time.Minute) != 139*time.Hour+49*time.Minute {
		t.Errorf("bufferHolds = %v", got)
	}
	if got := bufferHolds(512<<20, 0); got != 0 {
		t.Errorf("unknown rate: %v", got)
	}
}

func TestPrintBufferEstimates(t *testing.T) {
	var b bytes.Buffer
	printBufferEstimates(&b, []outputEstimate{
		{Name: "memory"},
		{Name: "hosted", SamplesPerMinute: 250, BufferMaxBytes: 512 << 20, BufferMaxAgeSeconds: 7200, BufferHoldsSeconds: bufferHolds(512<<20, 250).Seconds()},
		{Name: "small", SamplesPerMinute: 100000, BufferMaxBytes: 64 << 20, BufferMaxAgeSeconds: 7200, BufferHoldsSeconds: bufferHolds(64<<20, 100000).Seconds()},
		{Name: "new", BufferMaxBytes: 512 << 20, BufferMaxAgeSeconds: 7200},
	})
	want := `
Disk buffers (about 64 bytes per sample on disk):
  hosted: 512MiB holds about 559h of outage at 250 samples/min; samples older than 2h are dropped first
  small: 64MiB holds about 10m of outage at 100000 samples/min, less than its maxAge of 2h: raise maxSize to keep more
  new: 512MiB, kept up to 2h; the outage it holds is known once its jobs have run
`
	if got := b.String(); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	b.Reset()
	printBufferEstimates(&b, []outputEstimate{{Name: "memory"}})
	if strings.TrimSpace(b.String()) != "" {
		t.Errorf("no buffer, no section: %q", b.String())
	}
}
