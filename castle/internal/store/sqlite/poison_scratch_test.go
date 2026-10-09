package sqlite

// Scratch experiment for KB-222 — observe modernc.org/sqlite v1.40.1 behavior
// when a caller context dies while a store operation is in flight on the
// shared serialized writer connection. NOT part of the final commit set.

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/brokenbots/castle/castle/internal/store"
)

func TestScratchPoison(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "store.db")
	s, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	dsn := "file:" + dbPath + "?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"

	// Seed an overseer + run through the public API.
	ctx := context.Background()
	now := time.Now().UTC()
	if err := s.CreateOverseer(ctx, &store.Overseer{ID: "ov1", Name: "a", TokenHash: "x", Status: "online", CreatedAt: now, LastSeenAt: now}); err != nil {
		t.Fatal(err)
	}
	r := &store.Run{ID: "run1", OverseerID: "ov1", WorkflowName: "wf", Status: "pending", CreatedAt: now}
	if err := s.CreateRun(ctx, r); err != nil {
		t.Fatal(err)
	}

	raw, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`PRAGMA busy_timeout=5000`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}

	// Simulate the gRPC stream ctx.
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	start := time.Now()
	errCh := make(chan error, 1)
	blocked := make(chan struct{})
	go func() {
		close(blocked)
		_, _, err := s.AppendEvent(streamCtx, &store.Event{SchemaVersion: 1, RunID: "run1", Type: "step.log", Ts: now, Payload: []byte(`{"x":"y"}`)})
		errCh <- err
	}()
	<-blocked
	time.Sleep(150 * time.Millisecond) // AppendEvent should now be blocked inside the driver's busy wait on the write lock
	cancel()                           // the stream deadline fires mid-operation
	time.Sleep(150 * time.Millisecond) // let the driver watcher deliver sqlite3_interrupt
	if _, err := raw.Exec(`ROLLBACK`); err != nil {
		t.Fatal(err)
	}

	appendErr := <-errCh
	t.Logf("AppendEvent under expiring stream ctx: err=%v elapsed=%v (cancel at ~150ms)", appendErr, time.Since(start))
	seq, inserted, err := s.AppendEvent(context.Background(), &store.Event{SchemaVersion: 1, RunID: "run1", Type: "step.log", CorrelationID: "probe-first", Ts: time.Now().UTC()})
	t.Logf("probe append #1: seq=%d inserted=%v err=%v", seq, inserted, err)

	// The incident: every DB-backed RPC fails with interrupted (9) afterwards.
	for i := 0; i < 15; i++ {
		_, err := s.GetRun(context.Background(), "run1")
		if err != nil {
			t.Logf("post-expiry GetRun #%d: err=%v", i, err)
		}
		_, inserted, err := s.AppendEvent(context.Background(), &store.Event{SchemaVersion: 1, RunID: "run1", Type: "criteria.heartbeat", Ts: time.Now().UTC()})
		if err != nil {
			t.Logf("post-expiry AppendEvent #%d: err=%v", i, err)
		} else {
			t.Logf("post-expiry AppendEvent #%d: ok inserted=%v", i, inserted)
		}
	}
	if _, err := s.GetOverseer(context.Background(), "ov1"); err != nil {
		t.Logf("post-expiry GetOverseer reader: err=%v", err)
	}
}

// Variant 2: interrupt arrives while a multi-statement transaction on the
// SHARED writer is mid-flight (like SubmitEvents' AppendEvent tx).
func TestScratchTxInterrupt(t *testing.T) {
	s := tempStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := s.CreateOverseer(ctx, &store.Overseer{ID: "ov1", Name: "a", TokenHash: "x", Status: "online", CreatedAt: now, LastSeenAt: now}); err != nil {
		t.Fatal(err)
	}
	r := &store.Run{ID: "runX", OverseerID: "ov1", WorkflowName: "wf", Status: "pending", CreatedAt: now}
	if err := s.CreateRun(ctx, r); err != nil {
		t.Fatal(err)
	}

	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	tx, err := s.db.BeginTx(streamCtx, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Log("tx open on shared writer")

	// Statement 1 succeeds under the live ctx.
	if _, err := tx.ExecContext(streamCtx, `INSERT INTO events(run_id, seq, type, ts, payload) VALUES ('runX', 1, 'pre.q', ?, '{}')`, now, now); err != nil {
		t.Fatal(err)
	}
	t.Log("stmt1 ok")

	// The stream deadline dies; every watcher-armed statement from now on
	// fires sqlite3_interrupt on the shared conn.
	cancel()
	time.Sleep(100 * time.Millisecond)

	// Statement 2 under the dead ctx — what does the handler see?
	_, err = tx.ExecContext(streamCtx, `INSERT INTO events(run_id, seq, type, ts, payload) VALUES ('runX', 2, 'post.q', ?, '{}')`, now, now)
	t.Logf("stmt2 under dead ctx: err=%v", err)

	// Rollback through the same dead txCtx (what handler cleanup does).
	rbErr := tx.Rollback()
	t.Logf("tx.Rollback err=%v", rbErr)

	// Post-expiry health of the shared writer.
	for i := 0; i < 10; i++ {
		_, _, aErr := s.AppendEvent(ctx, &store.Event{SchemaVersion: 1, RunID: "runX", Type: "criteria.heartbeat", Ts: time.Now().UTC()})
		if aErr != nil {
			t.Logf("post-expiry AppendEvent #%d: err=%v", i, aErr)
			continue
		}
		t.Logf("post-expiry AppendEvent #%d: ok", i)
	}
}