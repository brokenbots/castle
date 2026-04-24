package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/brokenbots/overlord/castle/internal/store"
	"github.com/brokenbots/overlord/shared/events"
)

func tempStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestOverseerCRUD(t *testing.T) {
	s := tempStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	o := &store.Overseer{ID: "o1", Name: "alice", TokenHash: "x", Status: "online", CreatedAt: now, LastSeenAt: now}
	if err := s.CreateOverseer(ctx, o); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := s.GetOverseer(ctx, "o1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "alice" {
		t.Errorf("name: %s", got.Name)
	}
	list, _ := s.ListOverseers(ctx)
	if len(list) != 1 {
		t.Errorf("list len: %d", len(list))
	}
}

func TestEventAppendAssignsMonotonicSeq(t *testing.T) {
	s := tempStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := s.CreateOverseer(ctx, &store.Overseer{ID: "o1", Name: "x", TokenHash: "t", Status: "online", CreatedAt: now, LastSeenAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateRun(ctx, &store.Run{ID: "r1", OverseerID: "o1", WorkflowName: "w", WorkflowHCL: "x", Status: "pending", CurrentStep: "a", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		env, _ := events.New("r1", events.TypeStepEntered, events.StepEntered{Step: "a", Adapter: "shell", Attempt: 1})
		seq, inserted, err := s.AppendEvent(ctx, env)
		if err != nil {
			t.Fatal(err)
		}
		if !inserted {
			t.Errorf("append %d: expected inserted=true", i)
		}
		if seq != uint64(i+1) {
			t.Errorf("expected seq %d got %d", i+1, seq)
		}
	}
	list, err := s.ListEvents(ctx, "r1", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Errorf("list len: %d", len(list))
	}
	since, _ := s.ListEvents(ctx, "r1", 1, 100)
	if len(since) != 2 {
		t.Errorf("since=1 len: %d", len(since))
	}
}

func TestEventAppendIdempotentOnCorrelationID(t *testing.T) {
	s := tempStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := s.CreateOverseer(ctx, &store.Overseer{ID: "o1", Name: "x", TokenHash: "t", Status: "online", CreatedAt: now, LastSeenAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateRun(ctx, &store.Run{ID: "r1", OverseerID: "o1", WorkflowName: "w", WorkflowHCL: "x", Status: "pending", CurrentStep: "a", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	env, _ := events.New("r1", events.TypeStepEntered, events.StepEntered{Step: "a", Adapter: "shell", Attempt: 1})
	env.CorrelationID = "corr-xyz"

	seq1, inserted1, err := s.AppendEvent(ctx, env)
	if err != nil {
		t.Fatal(err)
	}
	if !inserted1 || seq1 != 1 {
		t.Fatalf("first append: inserted=%v seq=%d", inserted1, seq1)
	}

	// Second append with the same (run_id, correlation_id) must not insert
	// a new row; it returns the existing seq and inserted=false.
	seq2, inserted2, err := s.AppendEvent(ctx, env)
	if err != nil {
		t.Fatal(err)
	}
	if inserted2 {
		t.Fatalf("second append should be dedup; got inserted=true seq=%d", seq2)
	}
	if seq2 != seq1 {
		t.Fatalf("dedup should return existing seq %d; got %d", seq1, seq2)
	}

	list, _ := s.ListEvents(ctx, "r1", 0, 100)
	if len(list) != 1 {
		t.Fatalf("expected exactly 1 persisted row, got %d", len(list))
	}

	// Different correlation id on the same run inserts a new row.
	env2 := env
	env2.CorrelationID = "corr-abc"
	seq3, inserted3, err := s.AppendEvent(ctx, env2)
	if err != nil {
		t.Fatal(err)
	}
	if !inserted3 || seq3 != 2 {
		t.Fatalf("distinct corr id: inserted=%v seq=%d", inserted3, seq3)
	}
}

func TestMigrationsAreRecordedAndIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "m.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// schema_migrations should record each migration exactly once.
	rows, err := s.db.Query(`SELECT version, name FROM schema_migrations ORDER BY version`)
	if err != nil {
		t.Fatal(err)
	}
	var got []int
	for rows.Next() {
		var v int
		var n string
		if err := rows.Scan(&v, &n); err != nil {
			t.Fatal(err)
		}
		got = append(got, v)
	}
	rows.Close()
	if len(got) < 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("schema_migrations: want [1 2 ...], got %v", got)
	}
	s.Close()

	// Reopening the same DB must be a no-op (idempotent) — no duplicate
	// rows, no errors from re-applying CREATE TABLE/INDEX.
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	var count int
	if err := s2.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != len(got) {
		t.Fatalf("reopen added rows: before=%d after=%d", len(got), count)
	}
}
