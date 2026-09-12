package sqlite

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/brokenbots/castle/castle/internal/store"
)

func TestUpsertOrchestrator_InsertAndList(t *testing.T) {
	s := tempStore(t)
	ctx := context.Background()

	created := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	if err := s.UpsertOrchestrator(ctx, &store.Orchestrator{
		ID:        "orchestrator-operator",
		Name:      "operator",
		TokenHash: "hash-a",
		CreatedAt: created,
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	got, err := s.ListOrchestrators(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 orchestrator, got %d", len(got))
	}
	o := got[0]
	if o.ID != "orchestrator-operator" || o.Name != "operator" || o.TokenHash != "hash-a" {
		t.Fatalf("unexpected orchestrator: %+v", o)
	}
	if !o.CreatedAt.Equal(created) {
		t.Fatalf("created_at round-trip mismatch: want %v got %v", created, o.CreatedAt)
	}
}

func TestUpsertOrchestrator_RotatesTokenAndPreservesCreatedAt(t *testing.T) {
	s := tempStore(t)
	ctx := context.Background()

	created := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	if err := s.UpsertOrchestrator(ctx, &store.Orchestrator{
		ID: "orchestrator-operator", Name: "operator", TokenHash: "hash-old", CreatedAt: created,
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// Rotating the token must invalidate the old hash and keep created_at.
	if err := s.UpsertOrchestrator(ctx, &store.Orchestrator{
		ID: "orchestrator-operator", Name: "operator", TokenHash: "hash-new", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("rotate upsert: %v", err)
	}

	got, err := s.ListOrchestrators(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 orchestrator after rotation, got %d", len(got))
	}
	if got[0].TokenHash != "hash-new" {
		t.Fatalf("token not rotated: %q", got[0].TokenHash)
	}
	if !got[0].CreatedAt.Equal(created) {
		t.Fatalf("created_at not preserved: want %v got %v", created, got[0].CreatedAt)
	}

	// The rotated token must be the only valid one: the old hash must not
	// resolve anymore (hashes are unique per identity row).
	if got[0].TokenHash == "hash-old" {
		t.Fatal("old token hash still present")
	}
}

func TestUpsertOrchestrator_DuplicateTokenHashRejected(t *testing.T) {
	s := tempStore(t)
	ctx := context.Background()

	if err := s.UpsertOrchestrator(ctx, &store.Orchestrator{
		ID: "orchestrator-operator", Name: "operator", TokenHash: "hash-shared", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("upsert first: %v", err)
	}
	err := s.UpsertOrchestrator(ctx, &store.Orchestrator{
		ID: "orchestrator-secondary", Name: "secondary", TokenHash: "hash-shared", CreatedAt: time.Now(),
	})
	if err == nil {
		t.Fatal("expected duplicate token hash to be rejected by the unique index")
	}
	if !strings.Contains(err.Error(), "UNIQUE") {
		t.Fatalf("expected UNIQUE constraint violation, got: %v", err)
	}

	// The first identity must be untouched by the failed insert.
	got, listErr := s.ListOrchestrators(ctx)
	if listErr != nil {
		t.Fatalf("list: %v", listErr)
	}
	if len(got) != 1 || got[0].ID != "orchestrator-operator" {
		t.Fatalf("expected original identity intact, got %+v", got)
	}
}

func TestListOrchestrators_Empty(t *testing.T) {
	s := tempStore(t)
	got, err := s.ListOrchestrators(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("want empty list, got %d", len(got))
	}
}
