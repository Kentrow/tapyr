// Package buffer keeps the queue of a push output on disk (ADR 0015): a
// directory of append-only segment files holding checksummed, compressed
// records, and a checkpoint of the last record the backend acknowledged.
//
// The package knows records only as opaque payloads with a time and a
// count of points; the push queue encodes its points into them. It is
// passive: the caller appends, reads, acknowledges, syncs every second and
// enforces the bounds with the time of its own clock.
package buffer

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/snappy"
)

// DefaultSegmentSize is the size at which a segment is closed and the next
// one started.
const DefaultSegmentSize = 4 << 20

const (
	segmentSuffix  = ".seg"
	checkpointName = "checkpoint"
	lockName       = "lock"
)

// Options configures a buffer.
type Options struct {
	// Dir is the directory of the buffer, created if needed.
	Dir string
	// MaxSize bounds the segment files; Enforce drops the oldest segments
	// beyond it.
	MaxSize int64
	// MaxAge bounds the age of a segment's newest record; Enforce drops the
	// segments older than it.
	MaxAge time.Duration
	// SegmentSize rotates segments (DefaultSegmentSize when 0), at most half
	// of MaxSize so that the bound keeps room for a closed segment.
	SegmentSize int64
}

// Entry is a record read back: its payload, decompressed.
type Entry struct {
	Seq     uint64
	Time    time.Time
	Count   int
	Payload []byte
}

// Dropped counts what Enforce removed before the backend acknowledged it.
type Dropped struct {
	// Full counts the points of the segments dropped for MaxSize.
	Full int
	// TooOld counts the points of the segments dropped for MaxAge.
	TooOld int
}

// Stats describes the buffer.
type Stats struct {
	// Bytes is the size of the segment files.
	Bytes int64
	// Records and Points are not acknowledged yet.
	Records, Points int
	// Oldest is the time of the oldest record not acknowledged (zero when
	// none).
	Oldest time.Time
	// Corrupt counts the records found corrupt since Open (skipped).
	Corrupt int
}

// record is the index entry of a record not acknowledged.
type record struct {
	seq   uint64
	seg   *segment
	off   int64
	time  int64
	count int
}

// segment is one segment file.
type segment struct {
	path   string
	first  uint64 // sequence in its name
	size   int64
	newest int64 // newest record time, Unix nanoseconds
	// sealed segments take no more records: a corrupt record in them would
	// hide the records written after it.
	sealed bool
}

// Buffer is a disk buffer. Its methods are safe for concurrent use.
type Buffer struct {
	opts Options

	mu       sync.Mutex
	lock     *os.File
	segments []*segment // oldest first; the last one is the active one
	active   *os.File   // the last segment, open for appends (nil until one is needed)
	records  []record   // not acknowledged, in sequence order
	next     uint64     // sequence of the next record
	acked    uint64     // last sequence acknowledged
	dirty    bool       // acked moved since the last checkpoint
	bytes    int64
	corrupt  int
	readers  map[string]*os.File
	closed   bool
}

// Open opens the buffer of a directory: it takes the lock, reads the
// checkpoint and indexes the records after it. A record cut short at the
// end of the last segment (a crash during a write) is truncated; a corrupt
// record elsewhere ends the reading of its segment, counted in Stats.
func Open(opts Options) (*Buffer, error) {
	if opts.SegmentSize <= 0 {
		opts.SegmentSize = DefaultSegmentSize
	}
	if opts.MaxSize > 0 && opts.SegmentSize > opts.MaxSize/2 {
		opts.SegmentSize = max(opts.MaxSize/2, 1)
	}
	if err := os.MkdirAll(opts.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("creating the buffer directory: %w", err)
	}
	lock, err := os.OpenFile(filepath.Join(opts.Dir, lockName), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening the buffer lock: %w", err)
	}
	if err := lockFile(lock); err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("the buffer %s is used by another process: %w", opts.Dir, err)
	}
	b := &Buffer{opts: opts, lock: lock, readers: map[string]*os.File{}}
	if err := b.load(); err != nil {
		_ = b.closeFiles()
		return nil, err
	}
	return b, nil
}

// load reads the checkpoint and indexes the segments.
func (b *Buffer) load() error {
	acked, err := readCheckpoint(filepath.Join(b.opts.Dir, checkpointName))
	if err != nil {
		return err
	}
	b.acked = acked
	names, err := filepath.Glob(filepath.Join(b.opts.Dir, "*"+segmentSuffix))
	if err != nil {
		return err
	}
	type named struct {
		path  string
		first uint64
	}
	var files []named
	for _, p := range names {
		first, err := strconv.ParseUint(strings.TrimSuffix(filepath.Base(p), segmentSuffix), 10, 64)
		if err != nil {
			continue // not a segment of ours
		}
		files = append(files, named{p, first})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].first < files[j].first })
	last := b.acked
	for i, f := range files {
		seg := &segment{path: f.path, first: f.first}
		end, lastSeq, err := b.scan(seg, i == len(files)-1)
		if err != nil {
			return err
		}
		seg.size = end
		last = max(last, lastSeq)
		if seg.sealed {
			// The records after the corruption are unknown: no sequence
			// beyond what the file could hold may be reused (every record
			// is at least a header).
			last = max(last, seg.first+uint64(end/headerSize)) //nolint:gosec // G115: end is a file offset, never negative.
		}
		b.segments = append(b.segments, seg)
		b.bytes += seg.size
	}
	b.next = last + 1
	// Segments every record of which was acknowledged are gone.
	b.dropAcked()
	return nil
}

// scan indexes the records of a segment after the checkpoint. It returns
// the offset after the last good record and the last sequence seen.
func (b *Buffer) scan(seg *segment, isLast bool) (int64, uint64, error) {
	f, err := os.Open(seg.path)
	if err != nil {
		return 0, 0, fmt.Errorf("opening segment %s: %w", seg.path, err)
	}
	defer f.Close()
	r := bufio.NewReader(f)
	var off int64
	var lastSeq uint64
	for {
		h, payload, err := readRecord(r, maxRecordLength)
		if errors.Is(err, io.EOF) {
			return off, lastSeq, nil
		}
		if err != nil {
			if isLast && errors.Is(err, io.ErrUnexpectedEOF) {
				// A write cut by a crash: drop the tail.
				if terr := os.Truncate(seg.path, off); terr != nil {
					return 0, 0, fmt.Errorf("truncating segment %s: %w", seg.path, terr)
				}
				return off, lastSeq, nil
			}
			// Corrupt: the rest of the segment cannot be trusted, and no
			// record is appended after it.
			b.corrupt++
			seg.sealed = true
			info, serr := f.Stat()
			if serr != nil {
				return off, lastSeq, nil
			}
			return info.Size(), lastSeq, nil
		}
		size := int64(headerSize + len(payload))
		if h.seq > b.acked {
			b.records = append(b.records, record{seq: h.seq, seg: seg, off: off, time: h.time, count: int(h.count)})
		}
		seg.newest = max(seg.newest, h.time)
		lastSeq = max(lastSeq, h.seq)
		off += size
	}
}

// maxRecordLength bounds the payload a header may announce: a corrupt
// length must not make Open allocate gigabytes.
const maxRecordLength = 256 << 20

// Append writes a record: its payload, the time it stands for (the age
// MaxAge is measured from) and the number of points it holds. It returns
// the sequence of the record. The record is written at once, and on disk
// for sure after the next Sync.
func (b *Buffer) Append(at time.Time, count int, payload []byte) (uint64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return 0, errors.New("buffer closed")
	}
	if count < 0 || count > math.MaxUint32 {
		return 0, fmt.Errorf("record of %d points out of range", count)
	}
	data := snappy.Encode(nil, payload)
	if len(data) > maxRecordLength {
		return 0, fmt.Errorf("record of %d bytes over the limit of %d", len(data), maxRecordLength)
	}
	seq := b.next
	rec := encodeRecord(seq, at.UnixNano(), uint32(count), data)
	if err := b.ensureActive(seq); err != nil {
		return 0, err
	}
	seg := b.segments[len(b.segments)-1]
	if _, err := b.active.Write(rec); err != nil {
		// Leave the segment as it was before the write.
		_ = b.active.Truncate(seg.size)
		_, _ = b.active.Seek(seg.size, io.SeekStart)
		return 0, fmt.Errorf("writing to the buffer: %w", err)
	}
	b.records = append(b.records, record{seq: seq, seg: seg, off: seg.size, time: at.UnixNano(), count: count})
	seg.size += int64(len(rec))
	seg.newest = max(seg.newest, at.UnixNano())
	b.bytes += int64(len(rec))
	b.next++
	return seq, nil
}

// ensureActive opens the active segment, or starts a new one when there is
// none or the active one is full.
func (b *Buffer) ensureActive(seq uint64) error {
	if n := len(b.segments); n > 0 && !b.segments[n-1].sealed && b.segments[n-1].size < b.opts.SegmentSize {
		if b.active != nil {
			return nil
		}
		f, err := os.OpenFile(b.segments[n-1].path, os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return fmt.Errorf("opening segment: %w", err)
		}
		b.active = f
		return nil
	}
	if b.active != nil {
		if err := b.active.Sync(); err != nil {
			return fmt.Errorf("syncing segment: %w", err)
		}
		if err := b.active.Close(); err != nil {
			return fmt.Errorf("closing segment: %w", err)
		}
		b.active = nil
	}
	path := filepath.Join(b.opts.Dir, fmt.Sprintf("%020d%s", seq, segmentSuffix))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("creating segment: %w", err)
	}
	b.active = f
	b.segments = append(b.segments, &segment{path: path, first: seq})
	return nil
}

// Read returns up to limit records after the sequence after, oldest first,
// skipping the acknowledged ones. A record found corrupt is skipped and
// counted in Stats.
func (b *Buffer) Read(after uint64, limit int) ([]Entry, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	i := sort.Search(len(b.records), func(i int) bool { return b.records[i].seq > after })
	var out []Entry
	for ; i < len(b.records) && len(out) < limit; i++ {
		rec := b.records[i]
		payload, err := b.readAt(rec)
		if errors.Is(err, errCorrupt) {
			b.corrupt++
			continue
		}
		if err != nil {
			return out, err
		}
		out = append(out, Entry{Seq: rec.seq, Time: time.Unix(0, rec.time), Count: rec.count, Payload: payload})
	}
	return out, nil
}

// readAt reads and decompresses the payload of a record.
func (b *Buffer) readAt(rec record) ([]byte, error) {
	f, ok := b.readers[rec.seg.path]
	if !ok {
		var err error
		if f, err = os.Open(rec.seg.path); err != nil {
			return nil, fmt.Errorf("opening segment: %w", err)
		}
		b.readers[rec.seg.path] = f
	}
	_, data, err := readRecord(io.NewSectionReader(f, rec.off, rec.seg.size-rec.off), maxRecordLength)
	if err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, errCorrupt
		}
		return nil, err
	}
	payload, err := snappy.Decode(nil, data)
	if err != nil {
		return nil, errCorrupt
	}
	return payload, nil
}

// Ack records that the backend acknowledged every record up to seq. The
// segments left without a record to send are deleted; the checkpoint is
// written at the next Sync.
func (b *Buffer) Ack(seq uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if seq <= b.acked {
		return
	}
	b.acked = seq
	b.dirty = true
	b.dropAcked()
}

// dropAcked forgets the acknowledged records and deletes the segments
// that hold no other, except the active one.
func (b *Buffer) dropAcked() {
	i := sort.Search(len(b.records), func(i int) bool { return b.records[i].seq > b.acked })
	b.records = slices.Delete(b.records, 0, i)
	for len(b.segments) > 1 {
		seg := b.segments[0]
		if len(b.records) > 0 && b.records[0].seg == seg {
			break
		}
		b.removeOldest()
	}
}

// removeOldest deletes the oldest segment, which is not the active one.
func (b *Buffer) removeOldest() {
	seg := b.segments[0]
	if f, ok := b.readers[seg.path]; ok {
		_ = f.Close()
		delete(b.readers, seg.path)
	}
	_ = os.Remove(seg.path)
	b.bytes -= seg.size
	b.segments = b.segments[1:]
}

// Enforce drops the segments beyond MaxSize (oldest first) and the
// segments whose newest record is older than MaxAge at now, and returns
// the points dropped that the backend had not acknowledged.
func (b *Buffer) Enforce(now time.Time) Dropped {
	b.mu.Lock()
	defer b.mu.Unlock()
	var d Dropped
	drop := func() int {
		seg := b.segments[0]
		points := 0
		n := 0
		for n < len(b.records) && b.records[n].seg == seg {
			points += b.records[n].count
			n++
		}
		b.records = slices.Delete(b.records, 0, n)
		if len(b.segments) == 1 && b.active != nil {
			_ = b.active.Close()
			b.active = nil
		}
		b.removeOldest()
		return points
	}
	if b.opts.MaxAge > 0 {
		cutoff := now.Add(-b.opts.MaxAge).UnixNano()
		for len(b.segments) > 0 && b.segments[0].newest < cutoff && b.segments[0].size > 0 {
			d.TooOld += drop()
		}
	}
	if b.opts.MaxSize > 0 {
		for len(b.segments) > 1 && b.bytes > b.opts.MaxSize {
			d.Full += drop()
		}
	}
	return d
}

// Sync writes the active segment to disk and, when records were
// acknowledged since, the checkpoint.
func (b *Buffer) Sync() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sync()
}

func (b *Buffer) sync() error {
	if b.active != nil {
		if err := b.active.Sync(); err != nil {
			return fmt.Errorf("syncing segment: %w", err)
		}
	}
	if !b.dirty {
		return nil
	}
	if err := writeCheckpoint(b.opts.Dir, b.acked); err != nil {
		return err
	}
	b.dirty = false
	return nil
}

// Stats returns the state of the buffer.
func (b *Buffer) Stats() Stats {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := Stats{Bytes: b.bytes, Records: len(b.records), Corrupt: b.corrupt}
	for _, r := range b.records {
		s.Points += r.count
	}
	if len(b.records) > 0 {
		s.Oldest = time.Unix(0, b.records[0].time)
	}
	return s
}

// Close syncs the buffer and releases its files and lock.
func (b *Buffer) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil
	}
	err := b.sync()
	b.closed = true
	return errors.Join(err, b.closeFiles())
}

func (b *Buffer) closeFiles() error {
	var errs []error
	if b.active != nil {
		errs = append(errs, b.active.Close())
		b.active = nil
	}
	for path, f := range b.readers {
		errs = append(errs, f.Close())
		delete(b.readers, path)
	}
	if b.lock != nil {
		errs = append(errs, b.lock.Close())
	}
	return errors.Join(errs...)
}

// The checkpoint is the last acknowledged sequence and its CRC-32C, 12
// bytes, replaced atomically.

func readCheckpoint(path string) (uint64, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("reading the buffer checkpoint: %w", err)
	}
	if len(data) != 12 || crc32.Checksum(data[:8], castagnoli) != binary.LittleEndian.Uint32(data[8:]) {
		// A checkpoint that cannot be trusted: send everything again
		// rather than lose records; the backend sees duplicates at worst.
		return 0, nil
	}
	return binary.LittleEndian.Uint64(data), nil
}

func writeCheckpoint(dir string, seq uint64) error {
	var data [12]byte
	binary.LittleEndian.PutUint64(data[:], seq)
	binary.LittleEndian.PutUint32(data[8:], crc32.Checksum(data[:8], castagnoli))
	tmp := filepath.Join(dir, checkpointName+".tmp")
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("writing the buffer checkpoint: %w", err)
	}
	if _, err := f.Write(data[:]); err != nil {
		_ = f.Close()
		return fmt.Errorf("writing the buffer checkpoint: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("syncing the buffer checkpoint: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("closing the buffer checkpoint: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, checkpointName)); err != nil {
		return fmt.Errorf("replacing the buffer checkpoint: %w", err)
	}
	syncDir(dir)
	return nil
}

// syncDir makes a rename durable where the platform allows it (not on
// Windows, where opening a directory for a sync is not possible).
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}
