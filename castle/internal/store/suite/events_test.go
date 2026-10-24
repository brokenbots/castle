package suite

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/brokenbots/castle/castle/internal/store"
)

func ev(runID, typ string, ts time.Time, correlation, payload string) *store.Event {
	return &store.Event{
		SchemaVersion: int32(store.EventSchemaVersion),
		RunID:         runID,
		Type:          typ,
		Ts:            ts,
		CorrelationID: correlation,
		Payload:       []byte(payload),
	}
}

// TestEventAppendReplayDedup is the submit-events replay acceptance leg:
// sequence numbers assign monotonically per run, a duplicate correlation id
// returns the ORIGINAL seq with inserted=false (reconnect replays must not
// produce duplicate rows), last_seq advances on the run, and the replayed
// event mutates ev.Seq so callers can fan out.
func TestEventAppendReplayDedup(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		now := microUTC(time.Now())
		newOverseer(t, s, "ov-events", now, nil)
		newRun(t, s, &store.Run{ID: "run-events", OverseerID: "ov-events", WorkflowName: "wf", Status: "running", CreatedAt: now})

		ev1 := ev("run-events", "run.started", now, "corr-1", `{"step":"s1"}`)
		seq, inserted, err := s.AppendEvent(ctx, ev1)
		if err != nil || !inserted || seq != 1 {
			t.Fatalf("first append: seq=%d inserted=%v err=%v", seq, inserted, err)
		}
		if ev1.Seq != 1 {
			t.Fatalf("AppendEvent did not mutate ev.Seq: %d", ev1.Seq)
		}

		ev2 := ev("run-events", "step.entered", now.Add(time.Second), "corr-2", `{"step":"s2"}`)
		seq, inserted, err = s.AppendEvent(ctx, ev2)
		if err != nil || !inserted || seq != 2 {
			t.Fatalf("second append: seq=%d inserted=%v err=%v", seq, inserted, err)
		}

		events, err := s.ListEvents(ctx, "run-events", 0, 0)
		if err != nil {
			t.Fatalf("ListEvents: %v", err)
		}
		if len(events) != 2 || events[0].Seq != 1 || events[1].Seq != 2 {
			t.Fatalf("ListEvents = %d rows [%d,%d]", len(events), events[0].Seq, events[len(events)-1].Seq)
		}
		r, err2 := gotRun(t, s, "run-events")
		if err2 != nil {
			t.Fatalf("GetRun: %v", err2)
		}
		if r.LastSeq != 2 || r.Status != "running" {
			t.Fatalf("run last_seq=%d status=%q, want 2/running", r.LastSeq, r.Status)
		}

		// Replay: same correlation id returns the original seq, inserts nothing.
		replay := ev("run-events", "step.entered", now.Add(2*time.Second), "corr-2", `{"step":"s2"}`)
		seq, inserted, err = s.AppendEvent(ctx, replay)
		if err != nil || seq != 2 {
			t.Fatalf("replay: seq=%d err=%v", seq, err)
		}
		if inserted {
			t.Fatal("replay inserted=true, want false (at-most-once correlation)")
		}

		// Correlation ids are keyed per run: the same id on another run is new.
		newRun(t, s, &store.Run{ID: "run-events-2", OverseerID: "ov-events", WorkflowName: "wf", Status: "running", CreatedAt: now})
		cross := ev("run-events-2", "run.started", now, "corr-1", `{"step":"s1"}`)
		seq, inserted, err = s.AppendEvent(ctx, cross)
		if err != nil || !inserted || seq != 1 {
			t.Fatalf("cross-run correlation: seq=%d inserted=%v err=%v", seq, inserted, err)
		}

		// Empty correlation ids never dedup.
		for i := 0; i < 2; i++ {
			seq, inserted, err = s.AppendEvent(ctx, ev("run-events", "step.log", now, "", `{"out":"x"}`))
			if err != nil || !inserted {
				t.Fatalf("empty-correlation append %d: inserted=%v err=%v", i, inserted, err)
			}
		}
	})
}

// TestEventListFiltersSinceAndLimit pins ListEvents continuation semantics:
// since-exclusive reads, page limits, oversize rejects with ErrInvalidLimit,
// nonpositive falls back to the default page.
func TestEventListFiltersSinceAndLimit(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		now := microUTC(time.Now())
		newOverseer(t, s, "ov-filters", now, nil)
		newRun(t, s, &store.Run{ID: "run-filters", OverseerID: "ov-filters", WorkflowName: "wf", Status: "running", CreatedAt: now})

		for i := 1; i <= 6; i++ {
			if _, _, err := s.AppendEvent(ctx, ev("run-filters", "step.log", now.Add(time.Duration(i)*time.Second), "", fmt.Sprintf(`{"n":%d}`, i))); err != nil {
				t.Fatalf("append %d: %v", i, err)
			}
		}

		page, err := s.ListEvents(ctx, "run-filters", 0, 2)
		if err != nil {
			t.Fatalf("first page: %v", err)
		}
		if len(page) != 2 || page[0].Seq != 1 || page[1].Seq != 2 {
			t.Fatalf("first page = %v", pageSeqs(page))
		}
		page, err = s.ListEvents(ctx, "run-filters", 2, 2)
		if err != nil {
			t.Fatalf("second page: %v", err)
		}
		if len(page) != 2 || page[0].Seq != 3 {
			t.Fatalf("second page starts at seq %d, want 3", page[0].Seq)
		}

		if _, err := s.ListEvents(ctx, "run-filters", 0, store.ListEventsMaxLimit+1); !errors.Is(err, store.ErrInvalidLimit) {
			t.Fatalf("oversize limit err = %v, want ErrInvalidLimit", err)
		}
		page, err = s.ListEvents(ctx, "run-filters", 0, 0)
		if err != nil || len(page) != 6 {
			t.Fatalf("default page len=%d err=%v", len(page), err)
		}
	})
}

// TestEventStepLogsAndLatest covers the reads the run watcher serves:
// step-filtered logs, the newest event overall, and the newest
// step.entered event. ErrNotFound when nothing matches.
func TestEventStepLogsAndLatest(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		now := microUTC(time.Now())
		newOverseer(t, s, "ov-logs", now, nil)
		newRun(t, s, &store.Run{ID: "run-logs", OverseerID: "ov-logs", WorkflowName: "wf", Status: "running", CreatedAt: now})

		if _, _, err := s.AppendEvent(ctx, ev("run-logs", "step.entered", now, "c1", `{"step":"build"}`)); err != nil {
			t.Fatalf("seed entered: %v", err)
		}
		for i := 0; i < 3; i++ {
			if _, _, err := s.AppendEvent(ctx, ev("run-logs", "step.log", now.Add(time.Duration(i)*time.Second), "", fmt.Sprintf(`{"step":"build"}`))); err != nil {
				t.Fatalf("seed log: %v", err)
			}
		}
		if _, _, err := s.AppendEvent(ctx, ev("run-logs", "step.log", now.Add(3*time.Second), "", `{"step":"test"}`)); err != nil {
			t.Fatalf("seed other step: %v", err)
		}

		logs, err := s.ListStepLogs(ctx, "run-logs", "build", 0, 0)
		if err != nil {
			t.Fatalf("ListStepLogs: %v", err)
		}
		if len(logs) != 3 {
			t.Fatalf("step logs = %d rows, want 3", len(logs))
		}
		logs, err = s.ListStepLogs(ctx, "run-logs", "build", 2, 10)
		if err != nil || len(logs) != 2 {
			t.Fatalf("step logs since=2: len=%d err=%v", len(logs), err)
		}

		latest, err := s.GetLatestEvent(ctx, "run-logs")
		if err != nil {
			t.Fatalf("GetLatestEvent: %v", err)
		}
		if latest.Type != "step.log" {
			t.Fatalf("latest = %q, want step.log", latest.Type)
		}
		entered, err := s.GetLatestStepEnteredEvent(ctx, "run-logs")
		if err != nil || entered.Type != "step.entered" {
			t.Fatalf("GetLatestStepEnteredEvent = %q err %v", entered.Type, err)
		}

		if _, err := s.GetLatestStepEnteredEvent(ctx, "run-missing"); err != store.ErrNotFound {
			t.Fatalf("latest on missing run = %v, want ErrNotFound", err)
		}
	})
}

// TestSubscriberCursorRewindGuard covers the subscriber-cursor contract:
// cursors persist, and max(existing, last_seq) means a stale reconnect can
// never rewind a cursor. Unknown pairs read (0, false).
func TestSubscriberCursorRewindGuard(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		if seq, found, err := s.GetSubscriberCursor(ctx, "sub", "run-x"); err != nil || found || seq != 0 {
			t.Fatalf("unknown cursor = (%d,%v,%v)", seq, found, err)
		}
		if err := s.UpsertSubscriberCursor(ctx, "sub", "run-x", 7); err != nil {
			t.Fatalf("UpsertSubscriberCursor: %v", err)
		}
		// Advance.
		if err := s.UpsertSubscriberCursor(ctx, "sub", "run-x", 9); err != nil {
			t.Fatalf("Upsert 9: %v", err)
		}
		if seq, found, err := s.GetSubscriberCursor(ctx, "sub", "run-x"); err != nil || !found || seq != 9 {
			t.Fatalf("cursor after advance = (%d,%v,%v)", seq, found, err)
		}
		// Stale reconnect cannot rewind.
		if err := s.UpsertSubscriberCursor(ctx, "sub", "run-x", 4); err != nil {
			t.Fatalf("Upsert stale 4: %v", err)
		}
		if seq, found, err := s.GetSubscriberCursor(ctx, "sub", "run-x"); err != nil || seq != 9 {
			t.Fatalf("rewound cursor = (%d,%v,%v), want 9 preserved", seq, found, err)
		}
	})
}

// TestAttemptLedger covers RecordAttemptStart idempotency, completion
// stamping, and GetLatestAttempt ErrNotFound behavior.
func TestAttemptLedger(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		now := microUTC(time.Now())
		newOverseer(t, s, "ov-attempt", now, nil)
		newRun(t, s, &store.Run{ID: "run-attempt", OverseerID: "ov-attempt", WorkflowName: "wf", Status: "running", CreatedAt: now})

		if _, err := s.GetLatestAttempt(ctx, "run-attempt", "s1"); err != store.ErrNotFound {
			t.Fatalf("no-attempts read = %v, want ErrNotFound", err)
		}

		start := microUTC(now)
		if err := s.RecordAttemptStart(ctx, &store.RunAttempt{RunID: "run-attempt", Step: "s1", Attempt: 1, StartedAt: start, Outcome: ""}); err != nil {
			t.Fatalf("RecordAttemptStart: %v", err)
		}
		// Idempotent: re-recording leaves the row unchanged.
		if err := s.RecordAttemptStart(ctx, &store.RunAttempt{RunID: "run-attempt", Step: "s1", Attempt: 1, StartedAt: start, Outcome: "different"}); err != nil {
			t.Fatalf("RecordAttemptStart replay: %v", err)
		}
		if err := s.RecordAttemptComplete(ctx, "run-attempt", "s1", 1, "ok"); err != nil {
			t.Fatalf("RecordAttemptComplete: %v", err)
		}
		got, err := s.GetLatestAttempt(ctx, "run-attempt", "s1")
		if err != nil {
			t.Fatalf("GetLatestAttempt: %v", err)
		}
		// completed_at is stamped with the store's clock at completion time
		// (not the caller's instant), so the assertion is on presence and
		// ordering against the recorded start.
		if got.Attempt != 1 || got.Outcome != "ok" || got.CompletedAt == nil || !got.StartedAt.Equal(start) {
			t.Fatalf("attempt = %+v, want start %v stamped ok", got, start)
		}
		if got.CompletedAt.Before(got.StartedAt) {
			t.Fatalf("completed %v before started %v", got.CompletedAt, got.StartedAt)
		}

		// A second attempt for the same step supersedes as the latest.
		start2 := microUTC(now.Add(3 * time.Second))
		if err := s.RecordAttemptStart(ctx, &store.RunAttempt{RunID: "run-attempt", Step: "s1", Attempt: 2, StartedAt: start2}); err != nil {
			t.Fatalf("RecordAttemptStart 2: %v", err)
		}
		got, err = s.GetLatestAttempt(ctx, "run-attempt", "s1")
		if err != nil || got.Attempt != 2 {
			t.Fatalf("latest attempt = %d err %v, want 2", got.Attempt, err)
		}
	})
}

func pageSeqs(evs []*store.Event) []uint64 {
	out := make([]uint64, 0, len(evs))
	for _, e := range evs {
		out = append(out, e.Seq)
	}
	return out
}

func gotRun(t *testing.T, s store.Store, id string) (*store.Run, error) {
	t.Helper()
	return s.GetRun(context.Background(), id)
}

// TestEventAppendConcurrentReplayDedup pins the at-most-once guarantee under
// concurrency: writers replaying the same (run, correlation) pair must all
// observe the same single seq, exactly one reports created, exactly one row
// exists, and the run's last_seq never advances past that seq. The pg leg
// serializes appends per run under the advisory lock before the dedup
// check, so the check can no longer miss a concurrent replay; the unique
// index remains only as a backstop, not as the dedup mechanism.
func TestEventAppendConcurrentReplayDedup(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		now := microUTC(time.Now())
		newOverseer(t, s, "ov-events", now, nil)
		newRun(t, s, &store.Run{ID: "run-events-race", OverseerID: "ov-events", WorkflowName: "wf", Status: "running", CreatedAt: now})

		const writers = 8
		type result struct {
			seq     uint64
			created bool
			err     error
		}
		results := make([]result, writers)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < writers; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				e := ev("run-events-race", "step.log", now, "corr-race", `{"writer":true}`)
				seq, created, err := s.AppendEvent(ctx, e)
				results[i] = result{seq: seq, created: created, err: err}
			}(i)
		}
		close(start)
		wg.Wait()

		createdCount := 0
		seqs := map[uint64]int{}
		for i, r := range results {
			if r.err != nil {
				t.Fatalf("writer %d: AppendEvent: %v", i, r.err)
			}
			if r.created {
				createdCount++
			}
			seqs[r.seq]++
		}
		if createdCount != 1 {
			t.Fatalf("created=true reported %d times, want exactly 1", createdCount)
		}
		if len(seqs) != 1 {
			t.Fatalf("writers observed divergent seqs: %v", seqs)
		}
		var seq uint64
		for s := range seqs {
			seq = s
		}
		if seq != 1 {
			t.Fatalf("replayed append allocated seq %d, want 1", seq)
		}

		events, err := s.ListEvents(ctx, "run-events-race", 0, 0)
		if err != nil {
			t.Fatalf("ListEvents: %v", err)
		}
		if len(events) != 1 || events[0].Seq != seq || events[0].CorrelationID != "corr-race" {
			t.Fatalf("ListEvents = %d rows, want exactly the one replayed row: %+v", len(events), events)
		}

		r, err := gotRun(t, s, "run-events-race")
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		if r.LastSeq != seq {
			t.Fatalf("run last_seq = %d, want %d", r.LastSeq, seq)
		}
	})
}
