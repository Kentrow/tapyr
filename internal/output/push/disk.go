package push

import (
	"bytes"
	"context"
	"encoding/gob"
	"fmt"
	"time"
)

// The disk buffer of a queue (ADR 0015). Every Add is written to the buffer
// as one record before its points join the memory queue; a record is
// acknowledged once every point of it left the queue (delivered, or
// dropped for good). The memory queue is a window over the buffer: when it
// is full, the records wait on disk (the backlog) and are loaded, oldest
// first, as the memory frees, instead of being dropped.

// syncInterval is how often the buffer is synced to disk: a crash loses at
// most this much of the last points.
const syncInterval = time.Second

// diskRecord is a record of the buffer whose points are in the memory
// queue, or were and are not acknowledged yet.
type diskRecord struct {
	seq         uint64 // sequence of the record in the buffer
	first, last uint64 // seqs of its points in the queue
	count       int
}

// encodePoints writes the points of a record.
func encodePoints(points []Point) ([]byte, error) {
	var b bytes.Buffer
	if err := gob.NewEncoder(&b).Encode(points); err != nil {
		return nil, fmt.Errorf("encoding the points of a record: %w", err)
	}
	return b.Bytes(), nil
}

// decodePoints reads the points of a record.
func decodePoints(payload []byte) ([]Point, error) {
	var points []Point
	if err := gob.NewDecoder(bytes.NewReader(payload)).Decode(&points); err != nil {
		return nil, fmt.Errorf("decoding the points of a record: %w", err)
	}
	return points, nil
}

// addToDisk writes points to the buffer and queues them in memory when the
// window has room and no older record waits on disk. It reports false when
// the buffer cannot take them: the caller then queues them in memory only.
// q.mu must be held.
func (q *Queue) addToDisk(points []Point, now time.Time) (bool, int) {
	payload, err := encodePoints(points)
	if err == nil {
		var seq uint64
		if seq, err = q.opts.Buffer.Append(now, len(points), payload); err == nil {
			if q.diskFailing {
				q.diskFailing = false
				q.opts.Logger.Info("output disk buffer writable again", "component", "output", "output", q.opts.Name)
			}
			if !q.backlog && len(q.points)+len(points) <= q.opts.Capacity {
				q.load(seq, points, now)
			} else {
				q.backlog = true
			}
			return true, q.enforce(now)
		}
	}
	q.opts.Observer.OutputBufferError(q.opts.Name, "write", 1)
	if !q.diskFailing {
		q.diskFailing = true
		q.opts.Logger.Warn("output disk buffer not writable: the queue keeps the samples in memory only",
			"component", "output", "output", q.opts.Name, "error", err)
	}
	return false, 0
}

// load puts the points of a record in the memory queue; q.mu must be held.
func (q *Queue) load(seq uint64, points []Point, at time.Time) {
	rec := diskRecord{seq: seq, first: q.seq + 1, count: len(points)}
	for _, p := range points {
		q.seq++
		q.points = append(q.points, entry{Point: p, seq: q.seq, at: at})
	}
	rec.last = q.seq
	q.records = append(q.records, rec)
	q.loadedThrough = seq
	q.state.Queue = len(q.points)
}

// fill loads records of the backlog while the memory queue has room, and
// returns the points dropped for being older than the maximum age. q.mu
// must be held.
func (q *Queue) fill(now time.Time) int {
	tooOld := 0
	for q.backlog {
		room := q.opts.Capacity - len(q.points)
		if room <= 0 {
			return tooOld
		}
		entries, err := q.opts.Buffer.Read(q.loadedThrough, 16)
		if err != nil {
			q.opts.Logger.Warn("reading the output disk buffer", "component", "output", "output", q.opts.Name, "error", err)
			return tooOld
		}
		if len(entries) == 0 {
			q.backlog = false
			return tooOld
		}
		for _, e := range entries {
			if q.opts.MaxAge > 0 && now.Sub(e.Time) > q.opts.MaxAge {
				// Most backends refuse samples this old: drop them rather
				// than send them to be refused.
				tooOld += e.Count
				q.records = append(q.records, diskRecord{seq: e.Seq, first: q.seq + 1, last: q.seq, count: 0})
				q.loadedThrough = e.Seq
				continue
			}
			if e.Count > room && len(q.points) > 0 {
				return tooOld // the next record waits for room
			}
			points, err := decodePoints(e.Payload)
			if err != nil {
				q.opts.Logger.Warn("output disk buffer: record skipped", "component", "output", "output", q.opts.Name, "error", err)
				q.records = append(q.records, diskRecord{seq: e.Seq, first: q.seq + 1, last: q.seq})
				q.loadedThrough = e.Seq
				continue
			}
			q.load(e.Seq, points, e.Time)
			room = q.opts.Capacity - len(q.points)
		}
	}
	return tooOld
}

// acknowledge tells the buffer which records left the queue: the records
// whose points are all at or below seq, in order. q.mu must be held.
func (q *Queue) acknowledge(seq uint64) {
	n := 0
	for n < len(q.records) && q.records[n].last <= seq {
		n++
	}
	if n == 0 {
		return
	}
	q.opts.Buffer.Ack(q.records[n-1].seq)
	q.records = append(q.records[:0], q.records[n:]...)
}

// enforce applies the bounds of the buffer and drops from the memory queue
// the points of the records it dropped, except those of the batch in
// flight, which are being delivered. It returns the points dropped for the
// maximum size; those dropped for age are reported as too_old. q.mu must be
// held.
func (q *Queue) enforce(now time.Time) int {
	d := q.opts.Buffer.Enforce(now)
	if d.Through == 0 {
		return 0
	}
	// Records dropped from disk but loaded in memory were counted by the
	// buffer; their points leave the memory queue here, those in flight
	// stay and are not counted.
	kept := 0
	n := 0
	for n < len(q.records) && q.records[n].seq <= d.Through {
		for _, e := range q.points {
			if e.seq >= q.records[n].first && e.seq <= q.records[n].last && e.seq <= q.sending {
				kept++
			}
		}
		n++
	}
	if n > 0 {
		last := q.records[n-1].last
		q.records = append(q.records[:0], q.records[n:]...)
		i := 0
		for _, e := range q.points {
			if e.seq <= last && e.seq > q.sending {
				continue
			}
			q.points[i] = e
			i++
		}
		q.points = q.points[:i]
		q.state.Queue = len(q.points)
	}
	q.loadedThrough = max(q.loadedThrough, d.Through)
	full, tooOld := d.Full, d.TooOld
	// Points kept in flight are not lost: take them off the counts, the
	// size first, then the age.
	if k := min(kept, full); k > 0 {
		full -= k
		kept -= k
	}
	tooOld -= min(kept, tooOld)
	q.pendingTooOld += tooOld
	return full
}

// maintain syncs the buffer every second, applies its maximum age and
// loads the backlog, until ctx is done; then it closes the buffer.
func (q *Queue) maintain(ctx context.Context) {
	ticker := q.opts.Clock.NewTicker(syncInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			if err := q.opts.Buffer.Close(); err != nil {
				q.opts.Logger.Warn("closing the output disk buffer", "component", "output", "output", q.opts.Name, "error", err)
			}
			return
		case <-ticker.C():
		}
		if err := q.opts.Buffer.Sync(); err != nil {
			q.opts.Observer.OutputBufferError(q.opts.Name, "sync", 1)
			q.opts.Logger.Warn("syncing the output disk buffer", "component", "output", "output", q.opts.Name, "error", err)
		}
		if corrupt := q.opts.Buffer.Stats().Corrupt; corrupt > q.corruptSeen {
			q.opts.Observer.OutputBufferError(q.opts.Name, "corrupt", corrupt-q.corruptSeen)
			q.opts.Logger.Warn("output disk buffer: corrupt records skipped", "component", "output", "output", q.opts.Name,
				"records", corrupt-q.corruptSeen)
			q.corruptSeen = corrupt
		}
		q.mu.Lock()
		now := q.opts.Clock.Now()
		full := q.enforce(now)
		tooOld := q.fill(now) + q.pendingTooOld
		q.pendingTooOld = 0
		q.state.Dropped += int64(full + tooOld)
		q.mu.Unlock()
		q.reportDrops(full, tooOld)
		q.signal()
	}
}

// reportDrops counts the points dropped by the bounds of the buffer.
func (q *Queue) reportDrops(full, tooOld int) {
	if full > 0 {
		q.opts.Observer.OutputDropped(q.opts.Name, "buffer_full", full)
		q.opts.Logger.Warn("output disk buffer full: oldest samples dropped", "component", "output", "output", q.opts.Name, "dropped", full)
	}
	if tooOld > 0 {
		q.opts.Observer.OutputDropped(q.opts.Name, "too_old", tooOld)
		q.opts.Logger.Warn("output disk buffer: samples older than the maximum age dropped", "component", "output", "output", q.opts.Name,
			"dropped", tooOld, "max_age", q.opts.MaxAge.String())
	}
}
