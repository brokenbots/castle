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
		seq, err := s.AppendEvent(ctx, env)
		if err != nil {
			t.Fatal(err)
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
