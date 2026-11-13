package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure Go SQLite driver, no CGO
)

const schemaVersion = "1"

var schema = []string{
	`CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS ledger (
		limit_id TEXT NOT NULL,
		scope TEXT NOT NULL,
		window_start INTEGER NOT NULL,
		used INTEGER NOT NULL,
		observed INTEGER NOT NULL,
		observed_remaining INTEGER NOT NULL,
		observed_max INTEGER NOT NULL,
		used_at_observation INTEGER NOT NULL,
		exhausted_until INTEGER NOT NULL,
		accounts TEXT NOT NULL,
		updated_at INTEGER NOT NULL,
		PRIMARY KEY (limit_id, scope))`,
	`CREATE TABLE IF NOT EXISTS ledger_minutes (
		limit_id TEXT NOT NULL,
		scope TEXT NOT NULL,
		minute INTEGER NOT NULL,
		count INTEGER NOT NULL,
		PRIMARY KEY (limit_id, scope, minute))`,
	`CREATE TABLE IF NOT EXISTS runs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		job TEXT NOT NULL,
		instance TEXT NOT NULL,
		run_id TEXT NOT NULL,
		started INTEGER NOT NULL,
		ended INTEGER NOT NULL,
		result TEXT NOT NULL,
		requests INTEGER NOT NULL,
		retries INTEGER NOT NULL,
		pages INTEGER NOT NULL,
		items INTEGER NOT NULL,
		samples INTEGER NOT NULL,
		error TEXT NOT NULL)`,
	`CREATE INDEX IF NOT EXISTS runs_by_instance ON runs (job, instance, id)`,
	`CREATE TABLE IF NOT EXISTS inventory (
		instance TEXT NOT NULL,
		key TEXT NOT NULL,
		fields TEXT NOT NULL,
		updated_at INTEGER NOT NULL,
		PRIMARY KEY (instance, key))`,
	`CREATE TABLE IF NOT EXISTS tokens (
		key TEXT PRIMARY KEY,
		token BLOB NOT NULL,
		expiry INTEGER NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS series_instances (
		instance TEXT PRIMARY KEY,
		last_success INTEGER NOT NULL)`,
	// output is empty for the series of the last successful run; else the
	// row holds the last source timestamp sent to that output.
	`CREATE TABLE IF NOT EXISTS series (
		instance TEXT NOT NULL,
		output TEXT NOT NULL,
		series BLOB NOT NULL,
		last_ts INTEGER NOT NULL,
		PRIMARY KEY (instance, output, series))`,
	// Reconstructed counters (accumulate): running total and last window.
	`CREATE TABLE IF NOT EXISTS accumulators (
		instance TEXT NOT NULL,
		series BLOB NOT NULL,
		total REAL NOT NULL,
		window TEXT NOT NULL,
		PRIMARY KEY (instance, series))`,
}

// SQLite is a Store backed by an embedded SQLite database.
type SQLite struct {
	db *sql.DB

	// ledgerMu serializes SaveLedger; saved holds the entries of the last
	// successful save, so that the next one writes only what changed.
	ledgerMu sync.Mutex
	saved    map[ledgerKey]LedgerEntry
}

// ledgerKey identifies a ledger entry (a limit and a scope key).
type ledgerKey struct{ limit, scope string }

var _ Store = (*SQLite)(nil)

// OpenSQLite opens (and creates if needed) the database at path. A read-only
// store does not create the file and refuses writes.
func OpenSQLite(ctx context.Context, path string, readOnly bool) (*SQLite, error) {
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	if readOnly {
		if _, err := os.Stat(path); err != nil {
			return nil, fmt.Errorf("opening state %s: %w", path, err)
		}
		dsn += "&mode=ro"
	} else {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			return nil, fmt.Errorf("creating the state directory: %w", err)
		}
		dsn += "&_pragma=journal_mode(WAL)"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening state %s: %w", path, err)
	}
	db.SetMaxOpenConns(1) // SQLite allows one writer; serialize access
	s := &SQLite{db: db}
	if !readOnly {
		if err := s.migrate(ctx); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("initializing state %s: %w", path, err)
		}
	}
	return s, nil
}

func (s *SQLite) migrate(ctx context.Context) error {
	for _, stmt := range schema {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	v, ok, err := s.Meta(ctx, "schema_version")
	switch {
	case err != nil:
		return err
	case !ok:
		return s.SetMeta(ctx, "schema_version", schemaVersion)
	case v != schemaVersion:
		return fmt.Errorf("unsupported state schema version %s (want %s)", v, schemaVersion)
	}
	return nil
}

// Close implements Store.
func (s *SQLite) Close() error { return s.db.Close() }

// Ping implements Store.
func (s *SQLite) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// Meta implements Store.
func (s *SQLite) Meta(ctx context.Context, key string) (string, bool, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("reading meta %s: %w", key, err)
	}
	return v, true, nil
}

// SetMeta implements Store.
func (s *SQLite) SetMeta(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO meta (key, value) VALUES (?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, value)
	if err != nil {
		return fmt.Errorf("writing meta %s: %w", key, err)
	}
	return nil
}

func millis(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func fromMillis(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}

// LoadLedger implements Store.
func (s *SQLite) LoadLedger(ctx context.Context) ([]LedgerEntry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT limit_id, scope, window_start, used, observed, observed_remaining,
		observed_max, used_at_observation, exhausted_until, accounts, updated_at FROM ledger ORDER BY limit_id, scope`)
	if err != nil {
		return nil, fmt.Errorf("reading the ledger: %w", err)
	}
	defer rows.Close()
	var out []LedgerEntry
	index := map[[2]string]int{}
	for rows.Next() {
		var e LedgerEntry
		var start, exhausted, updated int64
		var observed int
		var accounts string
		if err := rows.Scan(&e.Limit, &e.Scope, &start, &e.Used, &observed, &e.ObservedRemaining,
			&e.ObservedMax, &e.UsedAtObservation, &exhausted, &accounts, &updated); err != nil {
			return nil, fmt.Errorf("reading the ledger: %w", err)
		}
		e.WindowStart, e.ExhaustedUntil, e.UpdatedAt = fromMillis(start), fromMillis(exhausted), fromMillis(updated)
		e.Observed = observed != 0
		if err := json.Unmarshal([]byte(accounts), &e.Accounts); err != nil {
			return nil, fmt.Errorf("reading the ledger accounts: %w", err)
		}
		index[[2]string{e.Limit, e.Scope}] = len(out)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading the ledger: %w", err)
	}
	minutes, err := s.db.QueryContext(ctx, `SELECT limit_id, scope, minute, count FROM ledger_minutes`)
	if err != nil {
		return nil, fmt.Errorf("reading the ledger minutes: %w", err)
	}
	defer minutes.Close()
	for minutes.Next() {
		var limitID, scope string
		var minute, count int64
		if err := minutes.Scan(&limitID, &scope, &minute, &count); err != nil {
			return nil, fmt.Errorf("reading the ledger minutes: %w", err)
		}
		if i, ok := index[[2]string{limitID, scope}]; ok {
			if out[i].Minutes == nil {
				out[i].Minutes = map[int64]int64{}
			}
			out[i].Minutes[minute] = count
		}
	}
	return out, minutes.Err()
}

// SaveLedger implements Store. It writes only what changed since its last
// successful save: an entry whose fields changed, and the minutes added,
// changed or gone. Every 5 seconds, most entries are unchanged and a
// window moves by one minute; rewriting every minute of a daily window
// (up to 1440 rows per scope) at every flush wore the disk for nothing.
// The first save of a process writes every entry in full, replacing the
// minutes a previous process left.
func (s *SQLite) SaveLedger(ctx context.Context, entries []LedgerEntry) error {
	s.ledgerMu.Lock()
	defer s.ledgerMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("writing the ledger: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	stmts := map[string]*sql.Stmt{}
	exec := func(query string, args ...any) error {
		st, ok := stmts[query]
		if !ok {
			if st, err = tx.PrepareContext(ctx, query); err != nil {
				return err
			}
			stmts[query] = st
		}
		_, err := st.ExecContext(ctx, args...)
		return err
	}
	defer func() {
		for _, st := range stmts {
			_ = st.Close()
		}
	}()
	next := make(map[ledgerKey]LedgerEntry, len(entries))
	for _, e := range entries {
		key := ledgerKey{e.Limit, e.Scope}
		prev, known := s.saved[key]
		if !known || !sameLedgerRow(prev, e) {
			accounts, err := json.Marshal(e.Accounts)
			if err != nil {
				return err
			}
			observed := 0
			if e.Observed {
				observed = 1
			}
			if err := exec(`INSERT INTO ledger (limit_id, scope, window_start, used, observed,
				observed_remaining, observed_max, used_at_observation, exhausted_until, accounts, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT (limit_id, scope) DO UPDATE SET window_start = excluded.window_start, used = excluded.used,
				observed = excluded.observed, observed_remaining = excluded.observed_remaining,
				observed_max = excluded.observed_max, used_at_observation = excluded.used_at_observation,
				exhausted_until = excluded.exhausted_until, accounts = excluded.accounts, updated_at = excluded.updated_at`,
				e.Limit, e.Scope, millis(e.WindowStart), e.Used, observed, e.ObservedRemaining, e.ObservedMax,
				e.UsedAtObservation, millis(e.ExhaustedUntil), string(accounts), millis(e.UpdatedAt)); err != nil {
				return fmt.Errorf("writing the ledger: %w", err)
			}
		}
		if !known {
			if err := exec(`DELETE FROM ledger_minutes WHERE limit_id = ? AND scope = ?`, e.Limit, e.Scope); err != nil {
				return fmt.Errorf("writing the ledger minutes: %w", err)
			}
		}
		for minute := range prev.Minutes {
			if _, ok := e.Minutes[minute]; !ok {
				if err := exec(`DELETE FROM ledger_minutes WHERE limit_id = ? AND scope = ? AND minute = ?`, e.Limit, e.Scope, minute); err != nil {
					return fmt.Errorf("writing the ledger minutes: %w", err)
				}
			}
		}
		for _, minute := range slices.Sorted(maps.Keys(e.Minutes)) {
			count := e.Minutes[minute]
			if old, ok := prev.Minutes[minute]; ok && old == count {
				continue
			}
			if err := exec(`INSERT INTO ledger_minutes (limit_id, scope, minute, count) VALUES (?, ?, ?, ?)
				ON CONFLICT (limit_id, scope, minute) DO UPDATE SET count = excluded.count`,
				e.Limit, e.Scope, minute, count); err != nil {
				return fmt.Errorf("writing the ledger minutes: %w", err)
			}
		}
		// A copy: the caller may change its maps and slices in place.
		e.Accounts = slices.Clone(e.Accounts)
		e.Minutes = maps.Clone(e.Minutes)
		next[key] = e
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("writing the ledger: %w", err)
	}
	// Entries absent from this save keep their rows, as before; they are
	// written in full if they come back.
	s.saved = next
	return nil
}

// sameLedgerRow reports whether two entries have the same row in the
// ledger table (their minutes aside).
func sameLedgerRow(a, b LedgerEntry) bool {
	return a.WindowStart.Equal(b.WindowStart) && a.Used == b.Used && a.Observed == b.Observed &&
		a.ObservedRemaining == b.ObservedRemaining && a.ObservedMax == b.ObservedMax &&
		a.UsedAtObservation == b.UsedAtObservation && a.ExhaustedUntil.Equal(b.ExhaustedUntil) &&
		a.UpdatedAt.Equal(b.UpdatedAt) && slices.Equal(a.Accounts, b.Accounts)
}

// AddRun implements Store.
func (s *SQLite) AddRun(ctx context.Context, r RunRecord) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("recording the run: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `INSERT INTO runs (job, instance, run_id, started, ended, result, requests,
		retries, pages, items, samples, error) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.Job, r.Instance, r.RunID, millis(r.Started), millis(r.Ended), r.Result, r.Requests, r.Retries,
		r.Pages, r.Items, r.Samples, r.Error); err != nil {
		return fmt.Errorf("recording the run: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM runs WHERE job = ? AND instance = ? AND id NOT IN (
		SELECT id FROM runs WHERE job = ? AND instance = ? ORDER BY id DESC LIMIT ?)`,
		r.Job, r.Instance, r.Job, r.Instance, RunsKept); err != nil {
		return fmt.Errorf("pruning the run history: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("recording the run: %w", err)
	}
	return nil
}

// Runs implements Store.
func (s *SQLite) Runs(ctx context.Context, job, instance string, limit int) ([]RunRecord, error) {
	return s.queryRuns(ctx, `SELECT job, instance, run_id, started, ended, result, requests, retries,
		pages, items, samples, error FROM runs WHERE job = ? AND instance = ? ORDER BY id DESC LIMIT ?`, job, instance, limit)
}

// RunsOfJob implements Store.
func (s *SQLite) RunsOfJob(ctx context.Context, job string, limit int) ([]RunRecord, error) {
	return s.queryRuns(ctx, `SELECT job, instance, run_id, started, ended, result, requests, retries,
		pages, items, samples, error FROM runs WHERE job = ? ORDER BY id DESC LIMIT ?`, job, limit)
}

func (s *SQLite) queryRuns(ctx context.Context, query string, args ...any) ([]RunRecord, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("reading the run history: %w", err)
	}
	defer rows.Close()
	var out []RunRecord
	for rows.Next() {
		var r RunRecord
		var started, ended int64
		if err := rows.Scan(&r.Job, &r.Instance, &r.RunID, &started, &ended, &r.Result, &r.Requests, &r.Retries,
			&r.Pages, &r.Items, &r.Samples, &r.Error); err != nil {
			return nil, fmt.Errorf("reading the run history: %w", err)
		}
		r.Started, r.Ended = fromMillis(started), fromMillis(ended)
		out = append(out, r)
	}
	return out, rows.Err()
}

// SaveInventory implements Store.
func (s *SQLite) SaveInventory(ctx context.Context, instance string, items []InventoryItem, at time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("writing the inventory: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM inventory WHERE instance = ?`, instance); err != nil {
		return fmt.Errorf("writing the inventory: %w", err)
	}
	for _, it := range items {
		if _, err := tx.ExecContext(ctx, `INSERT INTO inventory (instance, key, fields, updated_at) VALUES (?, ?, ?, ?)`,
			instance, it.Key, string(it.Fields), millis(at)); err != nil {
			return fmt.Errorf("writing the inventory: %w", err)
		}
	}
	if len(items) == 0 {
		// Keep track of an empty inventory with a sentinel row.
		if _, err := tx.ExecContext(ctx, `INSERT INTO inventory (instance, key, fields, updated_at) VALUES (?, '', '', ?)`,
			instance, millis(at)); err != nil {
			return fmt.Errorf("writing the inventory: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("writing the inventory: %w", err)
	}
	return nil
}

// LoadInventory implements Store.
func (s *SQLite) LoadInventory(ctx context.Context, instance string) ([]InventoryItem, time.Time, bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, fields, updated_at FROM inventory WHERE instance = ? ORDER BY rowid`, instance)
	if err != nil {
		return nil, time.Time{}, false, fmt.Errorf("reading the inventory: %w", err)
	}
	defer rows.Close()
	var items []InventoryItem
	var at time.Time
	found := false
	for rows.Next() {
		var key, fields string
		var updated int64
		if err := rows.Scan(&key, &fields, &updated); err != nil {
			return nil, time.Time{}, false, fmt.Errorf("reading the inventory: %w", err)
		}
		found, at = true, fromMillis(updated)
		if key == "" {
			continue // empty inventory sentinel
		}
		items = append(items, InventoryItem{Key: key, Fields: []byte(fields)})
	}
	return items, at, found, rows.Err()
}

// DeleteInventory implements Store.
func (s *SQLite) DeleteInventory(ctx context.Context, instance string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM inventory WHERE instance = ?`, instance); err != nil {
		return fmt.Errorf("deleting the inventory: %w", err)
	}
	return nil
}

// SaveSeries implements Store.
func (s *SQLite) SaveSeries(ctx context.Context, instances []SeriesSnapshot, removed []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("writing the series: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	del := func(instance string) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM series WHERE instance = ?`, instance); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM series_instances WHERE instance = ?`, instance)
		return err
	}
	for _, instance := range removed {
		if err := del(instance); err != nil {
			return fmt.Errorf("writing the series: %w", err)
		}
	}
	for _, snap := range instances {
		if err := del(snap.Instance); err != nil {
			return fmt.Errorf("writing the series: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO series_instances (instance, last_success) VALUES (?, ?)`,
			snap.Instance, millis(snap.LastSuccess)); err != nil {
			return fmt.Errorf("writing the series: %w", err)
		}
		for _, key := range snap.Series {
			if _, err := tx.ExecContext(ctx, `INSERT INTO series (instance, output, series, last_ts) VALUES (?, '', ?, 0)`,
				snap.Instance, []byte(key)); err != nil {
				return fmt.Errorf("writing the series: %w", err)
			}
		}
		for output, sent := range snap.Sent {
			for key, ts := range sent {
				if _, err := tx.ExecContext(ctx, `INSERT INTO series (instance, output, series, last_ts) VALUES (?, ?, ?, ?)`,
					snap.Instance, output, []byte(key), ts.UnixNano()); err != nil {
					return fmt.Errorf("writing the series: %w", err)
				}
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("writing the series: %w", err)
	}
	return nil
}

// loadSeriesInstances reads the job instances of the series registry, by
// instance.
func (s *SQLite) loadSeriesInstances(ctx context.Context) ([]SeriesSnapshot, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT instance, last_success FROM series_instances ORDER BY instance`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SeriesSnapshot
	for rows.Next() {
		var snap SeriesSnapshot
		var last int64
		if err := rows.Scan(&snap.Instance, &last); err != nil {
			return nil, err
		}
		snap.LastSuccess = time.UnixMilli(last).UTC()
		snap.Sent = map[string]map[string]time.Time{}
		out = append(out, snap)
	}
	return out, rows.Err()
}

// LoadSeries implements Store.
func (s *SQLite) LoadSeries(ctx context.Context) ([]SeriesSnapshot, error) {
	out, err := s.loadSeriesInstances(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading the series: %w", err)
	}
	index := make(map[string]int, len(out))
	for i, snap := range out {
		index[snap.Instance] = i
	}
	rows, err := s.db.QueryContext(ctx, `SELECT instance, output, series, last_ts FROM series ORDER BY instance, output, series`)
	if err != nil {
		return nil, fmt.Errorf("reading the series: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var instance, output string
		var key []byte
		var ts int64
		if err := rows.Scan(&instance, &output, &key, &ts); err != nil {
			return nil, fmt.Errorf("reading the series: %w", err)
		}
		i, ok := index[instance]
		if !ok {
			continue
		}
		snap := &out[i]
		if output == "" {
			snap.Series = append(snap.Series, string(key))
			continue
		}
		if snap.Sent[output] == nil {
			snap.Sent[output] = map[string]time.Time{}
		}
		snap.Sent[output][string(key)] = time.Unix(0, ts).UTC()
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading the series: %w", err)
	}
	return out, nil
}

// SaveAccumulators implements Store.
func (s *SQLite) SaveAccumulators(ctx context.Context, instances []AccumulatorSnapshot, removed []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("writing the accumulators: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	del := func(instance string) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM accumulators WHERE instance = ?`, instance)
		return err
	}
	for _, instance := range removed {
		if err := del(instance); err != nil {
			return fmt.Errorf("writing the accumulators: %w", err)
		}
	}
	for _, snap := range instances {
		if err := del(snap.Instance); err != nil {
			return fmt.Errorf("writing the accumulators: %w", err)
		}
		for _, a := range snap.Series {
			if _, err := tx.ExecContext(ctx, `INSERT INTO accumulators (instance, series, total, window) VALUES (?, ?, ?, ?)`,
				snap.Instance, []byte(a.Series), a.Total, a.Window); err != nil {
				return fmt.Errorf("writing the accumulators: %w", err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("writing the accumulators: %w", err)
	}
	return nil
}

// LoadAccumulators implements Store.
func (s *SQLite) LoadAccumulators(ctx context.Context) ([]AccumulatorSnapshot, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT instance, series, total, window FROM accumulators ORDER BY instance, series`)
	if err != nil {
		return nil, fmt.Errorf("reading the accumulators: %w", err)
	}
	defer rows.Close()
	var out []AccumulatorSnapshot
	for rows.Next() {
		var instance string
		var a Accumulated
		var key []byte
		if err := rows.Scan(&instance, &key, &a.Total, &a.Window); err != nil {
			return nil, fmt.Errorf("reading the accumulators: %w", err)
		}
		a.Series = string(key)
		if len(out) == 0 || out[len(out)-1].Instance != instance {
			out = append(out, AccumulatorSnapshot{Instance: instance})
		}
		out[len(out)-1].Series = append(out[len(out)-1].Series, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading the accumulators: %w", err)
	}
	return out, nil
}

// SaveToken implements Store.
func (s *SQLite) SaveToken(ctx context.Context, key string, sealed []byte, expiry time.Time) error {
	if _, err := s.db.ExecContext(ctx, `INSERT INTO tokens (key, token, expiry) VALUES (?, ?, ?)
		ON CONFLICT (key) DO UPDATE SET token = excluded.token, expiry = excluded.expiry`, key, sealed, millis(expiry)); err != nil {
		return fmt.Errorf("writing a token: %w", err)
	}
	return nil
}

// LoadToken implements Store.
func (s *SQLite) LoadToken(ctx context.Context, key string) ([]byte, time.Time, bool, error) {
	var sealed []byte
	var expiry int64
	err := s.db.QueryRowContext(ctx, `SELECT token, expiry FROM tokens WHERE key = ?`, key).Scan(&sealed, &expiry)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, time.Time{}, false, nil
	}
	if err != nil {
		return nil, time.Time{}, false, fmt.Errorf("reading a token: %w", err)
	}
	return sealed, time.UnixMilli(expiry).UTC(), true, nil
}

// DeleteToken implements Store.
func (s *SQLite) DeleteToken(ctx context.Context, key string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM tokens WHERE key = ?`, key); err != nil {
		return fmt.Errorf("deleting a token: %w", err)
	}
	return nil
}
