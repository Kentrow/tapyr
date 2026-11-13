package state

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

func open(t *testing.T, path string) *SQLite {
	t.Helper()
	s, err := OpenSQLite(context.Background(), path, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestMeta(t *testing.T) {
	ctx := context.Background()
	s := open(t, filepath.Join(t.TempDir(), "nested", "tapyr.db"))
	if _, ok, err := s.Meta(ctx, "credential_salt"); ok || err != nil {
		t.Fatalf("Meta() on empty store = %v, %v", ok, err)
	}
	for _, v := range []string{"first", "second"} {
		if err := s.SetMeta(ctx, "credential_salt", v); err != nil {
			t.Fatal(err)
		}
	}
	if v, ok, err := s.Meta(ctx, "credential_salt"); v != "second" || !ok || err != nil {
		t.Fatalf("Meta() = %q, %v, %v", v, ok, err)
	}
	if v, _, _ := s.Meta(ctx, "schema_version"); v != schemaVersion {
		t.Errorf("schema_version = %q", v)
	}
}

func TestLedgerRoundTrip(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "tapyr.db")
	s := open(t, path)
	at := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	entries := []LedgerEntry{
		{
			Limit: "p/daily", Scope: "account=a", WindowStart: at, Used: 42, Observed: true, ObservedRemaining: 900,
			ObservedMax: 1000, UsedAtObservation: 40, ExhaustedUntil: at.Add(time.Hour), Accounts: []string{"a"}, UpdatedAt: at,
		},
		{Limit: "p/rolling", Scope: "instance", Used: 0, Accounts: []string{"a", "b"}, Minutes: map[int64]int64{100: 3, 101: 1}, UpdatedAt: at},
	}
	if err := s.SaveLedger(ctx, entries); err != nil {
		t.Fatal(err)
	}
	// Updates replace entries, minutes included.
	entries[0].Used = 43
	entries[1].Minutes = map[int64]int64{102: 7}
	if err := s.SaveLedger(ctx, entries); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	ro, err := OpenSQLite(ctx, path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	got, err := ro.LoadLedger(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(entries, got); diff != "" {
		t.Errorf("ledger (-want +got):\n%s", diff)
	}
	if err := ro.SetMeta(ctx, "k", "v"); err == nil {
		t.Error("read-only store accepted a write")
	}
}

func TestOpenReadOnlyMissing(t *testing.T) {
	if _, err := OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "missing.db"), true); err == nil {
		t.Fatal("read-only open of a missing file succeeded")
	}
}

func TestRunHistory(t *testing.T) {
	ctx := context.Background()
	s := open(t, filepath.Join(t.TempDir(), "tapyr.db"))
	start := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	for i := range RunsKept + 10 {
		r := RunRecord{
			Job: "devices", Instance: "devices", RunID: fmt.Sprintf("%06d", i), Started: start.Add(time.Duration(i) * time.Minute),
			Ended: start.Add(time.Duration(i)*time.Minute + time.Second), Result: "success", Requests: 3, Pages: 3, Items: 5, Samples: 16,
		}
		if err := s.AddRun(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AddRun(ctx, RunRecord{Job: "sites", Instance: "sites", RunID: "x", Result: "failed", Error: "transient /sites: HTTP 503"}); err != nil {
		t.Fatal(err)
	}
	runs, err := s.Runs(ctx, "devices", "devices", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != RunsKept || runs[0].RunID != fmt.Sprintf("%06d", RunsKept+9) || runs[len(runs)-1].RunID != "000010" {
		t.Fatalf("kept %d runs, newest %s, oldest %s", len(runs), runs[0].RunID, runs[len(runs)-1].RunID)
	}
	if runs[0].Pages != 3 || !runs[0].Started.Equal(start.Add(time.Duration(RunsKept+9)*time.Minute)) {
		t.Errorf("run = %+v", runs[0])
	}
	sites, err := s.Runs(ctx, "sites", "sites", 5)
	if err != nil || len(sites) != 1 || sites[0].Error != "transient /sites: HTTP 503" {
		t.Fatalf("sites = %+v, %v", sites, err)
	}
}

func TestInventory(t *testing.T) {
	ctx := context.Background()
	s := open(t, filepath.Join(t.TempDir(), "tapyr.db"))
	at := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	if _, _, ok, err := s.LoadInventory(ctx, "orgs"); ok || err != nil {
		t.Fatalf("empty store: %v %v", ok, err)
	}
	items := []InventoryItem{{Key: "b", Fields: []byte(`{"id":2}`)}, {Key: "a", Fields: []byte(`{"id":1}`)}}
	if err := s.SaveInventory(ctx, "orgs", items, at); err != nil {
		t.Fatal(err)
	}
	got, gotAt, ok, err := s.LoadInventory(ctx, "orgs")
	if err != nil || !ok || !gotAt.Equal(at) {
		t.Fatalf("LoadInventory = %v %v %v", gotAt, ok, err)
	}
	if diff := cmp.Diff(items, got); diff != "" {
		t.Errorf("items (-want +got):\n%s", diff)
	}
	// An empty inventory is remembered as such.
	if err := s.SaveInventory(ctx, "orgs", nil, at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, gotAt, ok, _ = s.LoadInventory(ctx, "orgs")
	if !ok || len(got) != 0 || !gotAt.Equal(at.Add(time.Hour)) {
		t.Fatalf("empty inventory = %v %v %v", got, gotAt, ok)
	}
	if err := s.DeleteInventory(ctx, "orgs"); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, _ := s.LoadInventory(ctx, "orgs"); ok {
		t.Fatal("inventory not deleted")
	}
}

func TestSeries(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "tapyr.db")
	s := open(t, path)
	at := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	ts := at.Add(-time.Minute + 123*time.Nanosecond)
	key := "up\x00serial\xffSIM-1\xfe"
	snaps := []SeriesSnapshot{
		{Instance: "devices", LastSuccess: at, Series: []string{key}, Sent: map[string]map[string]time.Time{"remote": {key: ts}}},
		{Instance: "sites", LastSuccess: at, Sent: map[string]map[string]time.Time{}},
	}
	if err := s.SaveSeries(ctx, snaps, nil); err != nil {
		t.Fatal(err)
	}
	// Saving again replaces; removed instances are deleted.
	if err := s.SaveSeries(ctx, snaps[:1], []string{"sites"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = open(t, path)
	got, err := s.LoadSeries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(snaps[:1], got); diff != "" {
		t.Errorf("LoadSeries() (-want +got):\n%s", diff)
	}
}

func TestTokens(t *testing.T) {
	ctx := context.Background()
	s := open(t, filepath.Join(t.TempDir(), "tapyr.db"))
	expiry := time.Date(2026, 11, 2, 13, 0, 0, 0, time.UTC)
	if _, _, ok, err := s.LoadToken(ctx, "k"); ok || err != nil {
		t.Fatalf("empty store: %v %v", ok, err)
	}
	for _, tok := range []string{"\x01first", "\x01second"} {
		if err := s.SaveToken(ctx, "k", []byte(tok), expiry); err != nil {
			t.Fatal(err)
		}
	}
	got, exp, ok, err := s.LoadToken(ctx, "k")
	if err != nil || !ok || string(got) != "\x01second" || !exp.Equal(expiry) {
		t.Fatalf("LoadToken() = %q %v %v %v", got, exp, ok, err)
	}
	if err := s.DeleteToken(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, _ := s.LoadToken(ctx, "k"); ok {
		t.Error("token not deleted")
	}
}

func TestAccumulators(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "tapyr.db")
	s := open(t, path)
	key := "bytes_total\x00serial\xffSIM-1\xfe"
	snaps := []AccumulatorSnapshot{
		{Instance: "devices", Series: []Accumulated{{Series: key, Total: 1.5e12, Window: "2026-11-02T12:00:00Z"}}},
		{Instance: "uplinks", Series: []Accumulated{{Series: "a\x00", Total: 1, Window: "w1"}, {Series: "b\x00", Total: 2, Window: "w1"}}},
	}
	if err := s.SaveAccumulators(ctx, snaps, nil); err != nil {
		t.Fatal(err)
	}
	// Replacing an instance and removing another.
	if err := s.SaveAccumulators(ctx, []AccumulatorSnapshot{{Instance: "devices", Series: []Accumulated{{Series: key, Total: 2e12, Window: "w2"}}}},
		[]string{"uplinks"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = open(t, path)
	got, err := s.LoadAccumulators(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []AccumulatorSnapshot{{Instance: "devices", Series: []Accumulated{{Series: key, Total: 2e12, Window: "w2"}}}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("accumulators (-want +got):\n%s", diff)
	}
	if err := s.Ping(ctx); err != nil {
		t.Error(err)
	}
}

// TestRunsOfJob returns the runs of every instance of a job, newest first.
func TestRunsOfJob(t *testing.T) {
	ctx := context.Background()
	s := open(t, filepath.Join(t.TempDir(), "tapyr.db"))
	for i, inst := range []string{"devices[org=1]", "devices[org=2]", "devices[org=1]", "sites"} {
		job := "devices"
		if inst == "sites" {
			job = "sites"
		}
		if err := s.AddRun(ctx, RunRecord{Job: job, Instance: inst, RunID: fmt.Sprintf("%06d", i), Result: "success"}); err != nil {
			t.Fatal(err)
		}
	}
	runs, err := s.RunsOfJob(ctx, "devices", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 || runs[0].RunID != "000002" || runs[1].RunID != "000001" || runs[1].Instance != "devices[org=2]" {
		t.Errorf("runs = %+v", runs)
	}
}

// TestSchemaVersion refuses a state store written by another schema
// version, instead of reading it wrongly.
func TestSchemaVersion(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "tapyr.db")
	s := open(t, path)
	if v, ok, err := s.Meta(ctx, "schema_version"); err != nil || !ok || v != schemaVersion {
		t.Fatalf("schema_version = %q, %v, %v", v, ok, err)
	}
	if err := s.SetMeta(ctx, "schema_version", "99"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenSQLite(ctx, path, false); err == nil || !strings.Contains(err.Error(), "unsupported state schema version 99 (want 1)") {
		t.Errorf("OpenSQLite() = %v", err)
	}
}

// TestLedgerWritesChanges checks that SaveLedger writes only what changed
// since its last save, and stays exact: the first save of a process
// replaces what a previous process left, minutes changed in place are seen,
// and a failed save is written again in full.
func TestLedgerWritesChanges(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "tapyr.db")
	at := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	load := func(s *SQLite) []LedgerEntry {
		t.Helper()
		got, err := s.LoadLedger(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	// A previous process left minutes 1 to 3.
	prev := open(t, path)
	stale := []LedgerEntry{{Limit: "p/daily", Scope: "account=a", Used: 6, Minutes: map[int64]int64{1: 1, 2: 2, 3: 3}, UpdatedAt: at}}
	if err := prev.SaveLedger(ctx, stale); err != nil {
		t.Fatal(err)
	}
	if err := prev.Close(); err != nil {
		t.Fatal(err)
	}

	// The first save of the next process replaces them.
	s := open(t, path)
	defer s.Close()
	minutes := map[int64]int64{3: 5, 4: 1}
	entries := []LedgerEntry{{Limit: "p/daily", Scope: "account=a", Used: 6, Minutes: minutes, UpdatedAt: at}}
	if err := s.SaveLedger(ctx, entries); err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(entries, load(s)); diff != "" {
		t.Errorf("after the first save (-want +got):\n%s", diff)
	}

	// Minutes changed in place, as the engine may hand the same map again.
	minutes[4] = 2
	minutes[5] = 1
	delete(minutes, 3)
	entries[0].Used = 3
	if err := s.SaveLedger(ctx, entries); err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(entries, load(s)); diff != "" {
		t.Errorf("after changes in place (-want +got):\n%s", diff)
	}

	// An unchanged entry is not written again: a row removed behind the
	// store's back stays removed.
	if _, err := s.db.ExecContext(ctx, `DELETE FROM ledger_minutes WHERE minute = 5`); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveLedger(ctx, entries); err != nil {
		t.Fatal(err)
	}
	if got := load(s); len(got) != 1 || got[0].Minutes[5] != 0 {
		t.Errorf("an unchanged entry was written again: %+v", got)
	}
	minutes[5] = 1 // put the store back in line with the database for the rest of the test
	if _, err := s.db.ExecContext(ctx, `INSERT INTO ledger_minutes (limit_id, scope, minute, count) VALUES ('p/daily', 'account=a', 5, 1)`); err != nil {
		t.Fatal(err)
	}

	// A failed save leaves nothing behind in the store's memory: the next
	// one writes the same changes.
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	changed := []LedgerEntry{{Limit: "p/daily", Scope: "account=a", Used: 9, Minutes: map[int64]int64{6: 9}, UpdatedAt: at.Add(time.Minute)}}
	if err := s.SaveLedger(canceled, changed); err == nil {
		t.Fatal("a save with a canceled context succeeded")
	}
	if err := s.SaveLedger(ctx, changed); err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(changed, load(s)); diff != "" {
		t.Errorf("after a failed save (-want +got):\n%s", diff)
	}
}
