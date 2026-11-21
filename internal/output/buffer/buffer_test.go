package buffer

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"
	"time"
)

var t0 = time.Date(2026, time.November, 21, 10, 0, 0, 0, time.UTC)

func open(t *testing.T, opts Options) *Buffer {
	t.Helper()
	b, err := Open(opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b
}

func appendN(t *testing.T, b *Buffer, from, n int, at time.Time) {
	t.Helper()
	for i := from; i < from+n; i++ {
		if _, err := b.Append(at, 2, payload(i)); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}
}

// payload is a recognizable payload of 150 bytes that does not compress,
// so that the sizes of the tests hold.
func payload(i int) []byte {
	r := rand.New(rand.NewPCG(uint64(i), 7))
	b := fmt.Appendf(nil, "record %04d;", i)
	for len(b) < 150 {
		b = append(b, byte(r.Uint32()))
	}
	return b
}

func seqs(entries []Entry) []uint64 {
	out := make([]uint64, len(entries))
	for i, e := range entries {
		out[i] = e.Seq
	}
	return out
}

// crash releases the files of a buffer without a sync, as a killed
// process would.
func crash(t *testing.T, b *Buffer) {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.closeFiles(); err != nil {
		t.Fatal(err)
	}
	b.closed = true
}

func segments(t *testing.T, dir string) []string {
	t.Helper()
	names, err := filepath.Glob(filepath.Join(dir, "*"+segmentSuffix))
	if err != nil {
		t.Fatal(err)
	}
	return names
}

func TestAppendReadAck(t *testing.T) {
	dir := t.TempDir()
	b := open(t, Options{Dir: dir})
	appendN(t, b, 1, 5, t0)
	got, err := b.Read(0, 10)
	if err != nil || fmt.Sprint(seqs(got)) != "[1 2 3 4 5]" {
		t.Fatalf("Read = %v, %v", seqs(got), err)
	}
	if !bytes.Equal(got[2].Payload, payload(3)) || got[2].Count != 2 || !got[2].Time.Equal(t0) {
		t.Errorf("entry 3 = %+v", got[2])
	}
	if got, _ := b.Read(3, 1); fmt.Sprint(seqs(got)) != "[4]" {
		t.Errorf("Read(3, 1) = %v", seqs(got))
	}
	b.Ack(2)
	if s := b.Stats(); s.Records != 3 || s.Points != 6 || !s.Oldest.Equal(t0) {
		t.Errorf("stats after ack = %+v", s)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopened: the records after the checkpoint, and the sequence goes on.
	b = open(t, Options{Dir: dir})
	if got, _ := b.Read(0, 10); fmt.Sprint(seqs(got)) != "[3 4 5]" {
		t.Errorf("after reopen = %v", seqs(got))
	}
	if seq, err := b.Append(t0, 1, payload(6)); err != nil || seq != 6 {
		t.Errorf("Append after reopen = %d, %v", seq, err)
	}
}

func TestCrashBeforeSync(t *testing.T) {
	dir := t.TempDir()
	b := open(t, Options{Dir: dir})
	appendN(t, b, 1, 3, t0)
	b.Ack(1)
	if err := b.Sync(); err != nil {
		t.Fatal(err)
	}
	b.Ack(3) // not synced: lost with the crash
	crash(t, b)

	// Records acknowledged after the last sync come back: the backend sees
	// them twice, nothing is lost.
	b = open(t, Options{Dir: dir})
	if got, _ := b.Read(0, 10); fmt.Sprint(seqs(got)) != "[2 3]" {
		t.Errorf("after crash = %v", seqs(got))
	}
}

func TestTornTail(t *testing.T) {
	dir := t.TempDir()
	b := open(t, Options{Dir: dir})
	appendN(t, b, 1, 3, t0)
	crash(t, b)
	seg := segments(t, dir)[0]
	info, err := os.Stat(seg)
	if err != nil {
		t.Fatal(err)
	}
	// The last write was cut by the crash.
	if err := os.Truncate(seg, info.Size()-7); err != nil {
		t.Fatal(err)
	}

	b = open(t, Options{Dir: dir})
	if got, _ := b.Read(0, 10); fmt.Sprint(seqs(got)) != "[1 2]" {
		t.Errorf("after a torn write = %v", seqs(got))
	}
	if s := b.Stats(); s.Corrupt != 0 {
		t.Errorf("a torn tail is not corruption: %+v", s)
	}
	// The tail is gone: appends follow the last good record.
	appendN(t, b, 3, 1, t0)
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	b = open(t, Options{Dir: dir})
	if got, _ := b.Read(0, 10); fmt.Sprint(seqs(got)) != "[1 2 3]" {
		t.Errorf("after the append = %v", seqs(got))
	}
}

func TestCorruptRecord(t *testing.T) {
	dir := t.TempDir()
	// Segments of about two records.
	b := open(t, Options{Dir: dir, SegmentSize: 300})
	appendN(t, b, 1, 6, t0)
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	segs := segments(t, dir)
	if len(segs) != 3 {
		t.Fatalf("segments = %v", segs)
	}
	// A bit flips in the payload of the first record of the second segment.
	data, err := os.ReadFile(segs[1])
	if err != nil {
		t.Fatal(err)
	}
	data[headerSize+3] ^= 0xff
	if err := os.WriteFile(segs[1], data, 0o600); err != nil {
		t.Fatal(err)
	}

	b = open(t, Options{Dir: dir, SegmentSize: 300})
	// The rest of the corrupt segment is skipped; the next one is read.
	if got, _ := b.Read(0, 10); fmt.Sprint(seqs(got)) != "[1 2 5 6]" {
		t.Errorf("records = %v", seqs(got))
	}
	if s := b.Stats(); s.Corrupt != 1 {
		t.Errorf("corrupt = %d", s.Corrupt)
	}
}

func TestCorruptLastSegmentIsSealed(t *testing.T) {
	dir := t.TempDir()
	b := open(t, Options{Dir: dir})
	appendN(t, b, 1, 2, t0)
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	seg := segments(t, dir)[0]
	data, err := os.ReadFile(seg)
	if err != nil {
		t.Fatal(err)
	}
	data[headerSize+3] ^= 0xff // the first record
	if err := os.WriteFile(seg, data, 0o600); err != nil {
		t.Fatal(err)
	}
	b = open(t, Options{Dir: dir})
	appendN(t, b, 3, 1, t0)
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	// The new record went to a new segment, where the corrupt one cannot
	// hide it.
	if n := len(segments(t, dir)); n != 2 {
		t.Errorf("segments = %d, want 2", n)
	}
	b = open(t, Options{Dir: dir})
	// Its sequence is past anything the corrupt segment could hold.
	if got, _ := b.Read(0, 10); len(got) != 1 || got[0].Seq <= 2 || !bytes.Equal(got[0].Payload, payload(3)) {
		t.Errorf("records = %v", seqs(got))
	}
}

func TestCorruptCheckpoint(t *testing.T) {
	dir := t.TempDir()
	b := open(t, Options{Dir: dir})
	appendN(t, b, 1, 3, t0)
	b.Ack(2)
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, checkpointName), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Everything still on disk is sent again rather than lost.
	b = open(t, Options{Dir: dir})
	if got, _ := b.Read(0, 10); len(got) == 0 || got[len(got)-1].Seq != 3 {
		t.Errorf("records = %v", seqs(got))
	}
}

func TestAckDeletesSegments(t *testing.T) {
	dir := t.TempDir()
	b := open(t, Options{Dir: dir, SegmentSize: 300})
	appendN(t, b, 1, 8, t0)
	if n := len(segments(t, dir)); n != 4 {
		t.Fatalf("segments = %d", n)
	}
	before := b.Stats().Bytes
	b.Ack(5)
	// The two segments of records 1 to 4 are gone; the one of 5 and 6
	// stays for 6.
	if n := len(segments(t, dir)); n != 2 {
		t.Errorf("segments after ack = %d, want 2", n)
	}
	if after := b.Stats().Bytes; after >= before {
		t.Errorf("bytes %d after ack, %d before", after, before)
	}
	b.Ack(8)
	// The active segment stays, empty of records to send.
	if n, s := len(segments(t, dir)), b.Stats(); n != 1 || s.Records != 0 || !s.Oldest.IsZero() {
		t.Errorf("after the last ack: %d segments, stats %+v", n, s)
	}
}

func TestEnforceMaxSize(t *testing.T) {
	dir := t.TempDir()
	b := open(t, Options{Dir: dir, MaxSize: 1200, SegmentSize: 300})
	appendN(t, b, 1, 12, t0)
	d := b.Enforce(t0)
	if d.Full == 0 || d.TooOld != 0 {
		t.Errorf("dropped = %+v", d)
	}
	s := b.Stats()
	if s.Bytes > 1200 {
		t.Errorf("bytes = %d over the maximum", s.Bytes)
	}
	// The newest records are kept, the oldest dropped, every point counted.
	got, _ := b.Read(0, 100)
	if len(got) == 0 || got[len(got)-1].Seq != 12 || got[0].Seq == 1 {
		t.Errorf("records kept = %v", seqs(got))
	}
	if d.Full+s.Points != 24 {
		t.Errorf("points dropped %d + kept %d, want 24", d.Full, s.Points)
	}
	// Acknowledged records cost nothing when dropped.
	b.Ack(12)
	appendN(t, b, 13, 12, t0)
	b.Ack(24)
	if d := b.Enforce(t0); d.Full != 0 {
		t.Errorf("dropped acknowledged points: %+v", d)
	}
}

func TestEnforceMaxAge(t *testing.T) {
	dir := t.TempDir()
	b := open(t, Options{Dir: dir, MaxAge: 2 * time.Hour, SegmentSize: 300})
	appendN(t, b, 1, 2, t0)                  // segment 1, an hour old at t0+1h
	appendN(t, b, 3, 2, t0.Add(time.Hour))   // segment 2
	appendN(t, b, 5, 1, t0.Add(2*time.Hour)) // segment 3
	if d := b.Enforce(t0.Add(2 * time.Hour)); d.TooOld != 0 {
		t.Errorf("nothing is older than 2h yet: %+v", d)
	}
	d := b.Enforce(t0.Add(3*time.Hour + time.Minute))
	if d.TooOld != 8 || d.Full != 0 {
		t.Errorf("dropped = %+v, want the 8 points of records 1 to 4", d)
	}
	if got, _ := b.Read(0, 10); fmt.Sprint(seqs(got)) != "[5]" {
		t.Errorf("records = %v", seqs(got))
	}
	// Everything expired: the buffer is empty and takes records again.
	b.Enforce(t0.Add(5 * time.Hour))
	appendN(t, b, 6, 1, t0.Add(5*time.Hour))
	if got, _ := b.Read(0, 10); fmt.Sprint(seqs(got)) != "[6]" {
		t.Errorf("records after expiry = %v", seqs(got))
	}
}

func TestLock(t *testing.T) {
	dir := t.TempDir()
	b := open(t, Options{Dir: dir})
	if _, err := Open(Options{Dir: dir}); err == nil {
		t.Fatal("a second Open of the same directory succeeded")
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	// Released on close.
	open(t, Options{Dir: dir})
}

func TestSegmentSizeFitsMaxSize(t *testing.T) {
	b := open(t, Options{Dir: t.TempDir(), MaxSize: 8 << 20})
	if b.opts.SegmentSize != 4<<20 {
		t.Errorf("segment size = %d", b.opts.SegmentSize)
	}
	b = open(t, Options{Dir: t.TempDir(), MaxSize: 1000})
	if b.opts.SegmentSize != 500 {
		t.Errorf("segment size = %d, want half of MaxSize", b.opts.SegmentSize)
	}
}
