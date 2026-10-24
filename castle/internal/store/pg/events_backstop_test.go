package pg_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/brokenbots/castle/castle/internal/store"
	"github.com/brokenbots/castle/castle/internal/store/pg"
	"github.com/brokenbots/castle/castle/internal/store/pg/pgtest"
	"github.com/jackc/pgx/v5/pgconn"
)

// TestAppendEventDedupBackstop pins the out-of-band-writer backstop in
// AppendEvent: when the INSERT loses the (run_id, correlation_id) race to a
// writer that bypassed the advisory lock, the violation aborts the append
// transaction, so the seq must be re-resolved on the pool (rollback +
// re-read) — never on the aborted tx, where every further statement fails
// with SQLSTATE 25P02. The hook seam fires inside that exact window, making
// both constraint outcomes deterministic: an idx_events_run_correlation
// violation resolves idempotently, while a non-dedup 23505 (events_pkey)
// still escapes as a real error.
func TestAppendEventDedupBackstop(t *testing.T) {
	dsn := pgtest.DSN(t)
	s, err := pg.Open(dsn)
	if err != nil {
		t.Fatalf("pg.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	raw, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open out-of-band handle: %v", err)
	}
	t.Cleanup(func() { _ = raw.Close() })

	ctx := context.Background()
	const runID = "backstop-run"
	var now = time.Now().UTC()
	// runs.overseer_id carries a foreign key to overseers(id) even after
	// migrations make it nullable, so seed an overseer for the run row.
	const overseerID = "ov-backstop"
	if err := s.CreateOverseer(ctx, &store.Overseer{ID: overseerID, Name: "backstop", TokenHash: "t", Status: "offline", CreatedAt: now, LastSeenAt: now}); err != nil {
		t.Fatalf("CreateOverseer: %v", err)
	}
	// events carries no FK on run_id, but the runs.last_seq assertions need
	// the run row.
	if err := s.CreateRun(ctx, &store.Run{
		ID:           runID,
		OverseerID:   overseerID,
		WorkflowName: "backstop",
		WorkflowHCL:  "run {}",
		Status:       "pending",
	}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	var hookErr error
	insertOutOfBand := func(ev *store.Event, seq int64, ts time.Time, corr string) error {
		_, err := raw.ExecContext(ctx,
			`INSERT INTO events(run_id,seq,type,ts,correlation_id,payload) VALUES($1,$2,$3,$4,$5,$6)`,
			ev.RunID, seq, ev.Type, ts.UTC().Truncate(time.Microsecond), corr, []byte(`{"hook":true}`))
		return err
	}
	raceTs := time.Date(2026, 3, 4, 5, 0, 0, 123000, time.UTC)
	// Pre-create the target month partition with an autocommit call. If the
	// partition only came into existence inside the append's own transaction
	// (CREATE PARTITION OF holds ACCESS EXCLUSIVE on events until commit),
	// the out-of-band insert below would wait on that lock while the append
	// is synchronously blocked inside the hook — a long self-stall, with the
	// committed race row landing in events_default. Pre-warming keeps
	// castle_ensure_event_partition on its no-DDL fast path inside
	// AppendEvent and keeps both subtests deterministic.
	if _, err := raw.ExecContext(ctx, `SELECT castle_ensure_event_partition($1::timestamptz)`, raceTs); err != nil {
		t.Fatalf("pre-warm event partition: %v", err)
	}

	t.Run("correlation violation resolves idempotently", func(t *testing.T) {
		restore := pg.InstallEventAppendHook(func(ev *store.Event, seq uint64, ts time.Time) {
			// The out-of-band writer wins the race with a different seq, so
			// only idx_events_run_correlation is violated, not the PK.
			hookErr = insertOutOfBand(ev, int64(seq)+1000, ts, ev.CorrelationID)
		})
		defer restore()

		ev := &store.Event{
			RunID:         runID,
			Type:          "step.entered",
			CorrelationID: "dedup-race",
			Ts:            raceTs,
			Payload:       []byte(`{}`),
		}
		seq, created, err := s.AppendEvent(ctx, ev)
		if hookErr != nil {
			t.Fatalf("out-of-band insert: %v", hookErr)
		}
		if err != nil {
			// A regression that re-queries the aborted tx surfaces here as
			// SQLSTATE 25P02.
			t.Fatalf("AppendEvent after dedup violation: %v", err)
		}
		if created {
			t.Fatalf("backstop must report created=false, got true (seq=%d)", seq)
		}
		if seq != 1001 || ev.Seq != 1001 {
			t.Fatalf("AppendEvent = (%d, created=%v), want the out-of-band row's seq 1001; ev.Seq=%d", seq, created, ev.Seq)
		}
		var n int64
		if err := raw.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM events WHERE run_id=$1 AND correlation_id=$2`, runID, ev.CorrelationID).Scan(&n); err != nil {
			t.Fatalf("count events: %v", err)
		}
		if n != 1 {
			t.Fatalf("events for correlation id: got %d rows, want exactly 1", n)
		}
		assertLastSeqUnchanged(t, raw, runID)
	})

	t.Run("non-dedup 23505 still escapes", func(t *testing.T) {
		restore := pg.InstallEventAppendHook(func(ev *store.Event, seq uint64, ts time.Time) {
			// Identical (run_id, seq, ts) but a different correlation id: the
			// duplicate is on the primary key only, which must surface as a
			// real error instead of the idempotent return path.
			hookErr = insertOutOfBand(ev, int64(seq), ts, ev.CorrelationID+"-pkey")
		})
		defer restore()

		ev := &store.Event{
			RunID:         runID,
			Type:          "step.entered",
			CorrelationID: "pkey-race",
			Ts:            raceTs,
			Payload:       []byte(`{}`),
		}
		seq, created, err := s.AppendEvent(ctx, ev)
		if hookErr != nil {
			t.Fatalf("out-of-band insert: %v", hookErr)
		}
		if err == nil {
			t.Fatalf("events_pkey violation must escape as an error, got (seq=%d, created=%v)", seq, created)
		}
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23505" || strings.HasSuffix(pgErr.ConstraintName, "run_id_correlation_id_ts_idx") || !strings.HasSuffix(pgErr.ConstraintName, "_pkey") {
			t.Fatalf("error = %v, want a 23505 violation of the events primary key", err)
		}
		if created {
			t.Fatalf("created=true on a failed append")
		}
		var n, total int64
		if err := raw.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM events WHERE run_id=$1 AND correlation_id=$2`, runID, ev.CorrelationID).Scan(&n); err != nil {
			t.Fatalf("count events: %v", err)
		}
		if err := raw.QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE run_id=$1`, runID).Scan(&total); err != nil {
			t.Fatalf("count events: %v", err)
		}
		if n != 0 {
			t.Fatalf("failed append must not persist a row for its correlation id, got %d", n)
		}
		if total != 2 {
			t.Fatalf("events after pkey violation: got %d rows, want 2 (the earlier dedup row plus the out-of-band row); the failed append's tx must roll back", total)
		}
	})
}

func assertLastSeqUnchanged(t *testing.T, raw *sql.DB, runID string) {
	t.Helper()
	var lastSeq int64
	if err := raw.QueryRowContext(context.Background(), `SELECT last_seq FROM runs WHERE id=$1`, runID).Scan(&lastSeq); err != nil {
		t.Fatalf("read last_seq: %v", err)
	}
	if lastSeq != 0 {
		t.Fatalf("backstop path must not touch runs.last_seq, got %d", lastSeq)
	}
}
