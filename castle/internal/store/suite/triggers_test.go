package suite

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/brokenbots/castle/castle/internal/store"
)

// Trigger identity contracts (KB-320) enforced on both engines: upsert
// semantics, token rotation preserving created_at, unique token-hash
// rejection, and the revoke lifecycle. The token material in this suite is a
// synthetic stand-in for the SHA-256 hex digests auth.HashToken produces.

func TestTriggerUpsertAndList(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		created := time.Date(2026, 12, 1, 9, 0, 0, 0, time.UTC)
		if err := s.UpsertTrigger(ctx, &store.Trigger{
			ID: "kanboard-trigger", TokenHash: "hash-kanboard", CreatedAt: created,
		}); err != nil {
			t.Fatalf("upsert: %v", err)
		}

		got, err := s.ListTriggers(ctx)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("want 1 trigger, got %d", len(got))
		}
		if got[0].ID != "kanboard-trigger" || got[0].TokenHash != "hash-kanboard" {
			t.Fatalf("unexpected trigger: %+v", got[0])
		}
		if !got[0].CreatedAt.Equal(created) {
			t.Fatalf("created_at round-trip mismatch: %v", got[0].CreatedAt)
		}
	})
}

func TestTriggerUpsertRotatesTokenAndPreservesCreatedAt(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		created := time.Date(2026, 12, 1, 9, 0, 0, 0, time.UTC)
		if err := s.UpsertTrigger(ctx, &store.Trigger{
			ID: "kanboard-trigger", TokenHash: "hash-old", CreatedAt: created,
		}); err != nil {
			t.Fatalf("upsert: %v", err)
		}
		if err := s.UpsertTrigger(ctx, &store.Trigger{
			ID: "kanboard-trigger", TokenHash: "hash-new", CreatedAt: created,
		}); err != nil {
			t.Fatalf("rotate upsert: %v", err)
		}

		got, err := s.ListTriggers(ctx)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(got) != 1 || got[0].TokenHash != "hash-new" {
			t.Fatalf("want rotated token, got %+v", got)
		}
		if !got[0].CreatedAt.Equal(created) {
			t.Fatalf("created_at not preserved: %v", got[0].CreatedAt)
		}
	})
}

func TestTriggerDuplicateTokenHashRejected(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		if err := s.UpsertTrigger(ctx, &store.Trigger{
			ID: "kanboard-trigger", TokenHash: "hash-shared", CreatedAt: time.Now(),
		}); err != nil {
			t.Fatalf("upsert first: %v", err)
		}
		err := s.UpsertTrigger(ctx, &store.Trigger{
			ID: "trigger-secondary", TokenHash: "hash-shared", CreatedAt: time.Now(),
		})
		if err == nil {
			t.Fatal("expected duplicate token hash to be rejected by the unique index")
		}
		if !strings.Contains(strings.ToLower(err.Error()), "unique") {
			t.Fatalf("expected UNIQUE constraint violation, got: %v", err)
		}

		// The first identity must be untouched by the failed insert.
		got, listErr := s.ListTriggers(ctx)
		if listErr != nil {
			t.Fatalf("list: %v", listErr)
		}
		if len(got) != 1 || got[0].ID != "kanboard-trigger" {
			t.Fatalf("expected original identity intact, got %+v", got)
		}
	})
}

func TestTriggerListEmpty(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		got, err := s.ListTriggers(context.Background())
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("want empty list, got %d", len(got))
		}
	})
}

func TestTriggerDelete(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		for _, id := range []string{"kanboard-trigger", "trigger-secondary"} {
			if err := s.UpsertTrigger(ctx, &store.Trigger{
				ID: id, TokenHash: "hash-" + id, CreatedAt: time.Now(),
			}); err != nil {
				t.Fatalf("upsert %s: %v", id, err)
			}
		}

		// Deleting one identity leaves the others and revokes its token.
		if err := s.DeleteTrigger(ctx, "kanboard-trigger"); err != nil {
			t.Fatalf("delete: %v", err)
		}
		got, err := s.ListTriggers(ctx)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(got) != 1 || got[0].ID != "trigger-secondary" {
			t.Fatalf("want only trigger-secondary, got %+v", got)
		}

		// Deleting an unknown ID is a no-op.
		if err := s.DeleteTrigger(ctx, "trigger-unknown"); err != nil {
			t.Fatalf("delete unknown: %v", err)
		}
		if got, err = s.ListTriggers(ctx); err != nil || len(got) != 1 {
			t.Fatalf("list after unknown delete: %v %v", got, err)
		}
	})
}