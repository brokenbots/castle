package sqlite

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/brokenbots/castle/castle/internal/store"
)

func TestConsoleUser_UpsertAndGet(t *testing.T) {
	s := tempStore(t)
	ctx := context.Background()

	created := time.Date(2026, 9, 15, 8, 0, 0, 0, time.UTC)
	if err := s.UpsertConsoleUser(ctx, &store.ConsoleUser{
		ID: "console-default", Username: "operator", PasswordHash: "$2a$10$hash", CreatedAt: created, UpdatedAt: created,
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	got, err := s.GetConsoleUser(ctx, "operator")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ID != "console-default" || got.Username != "operator" || got.PasswordHash != "$2a$10$hash" {
		t.Fatalf("unexpected user: %+v", got)
	}
	if !got.CreatedAt.Equal(created) || !got.UpdatedAt.Equal(created) {
		t.Fatalf("timestamps round-trip mismatch: created=%v updated=%v", got.CreatedAt, got.UpdatedAt)
	}
}

func TestConsoleUser_GetUnknownUsername(t *testing.T) {
	s := tempStore(t)
	_, err := s.GetConsoleUser(context.Background(), "nobody")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestConsoleUser_UpsertRotatesCredentialsAndPreservesCreatedAt(t *testing.T) {
	s := tempStore(t)
	ctx := context.Background()

	created := time.Date(2026, 9, 15, 8, 0, 0, 0, time.UTC)
	if err := s.UpsertConsoleUser(ctx, &store.ConsoleUser{
		ID: "console-default", Username: "operator", PasswordHash: "$2a$10$old", CreatedAt: created, UpdatedAt: created,
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// Rotate username + password on the same fixed ID: the old username must
	// stop resolving and created_at must be preserved.
	if err := s.UpsertConsoleUser(ctx, &store.ConsoleUser{
		ID: "console-default", Username: "admin", PasswordHash: "$2a$10$new", CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("rotate upsert: %v", err)
	}

	if _, err := s.GetConsoleUser(ctx, "operator"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("old username still resolves: %v", err)
	}
	got, err := s.GetConsoleUser(ctx, "admin")
	if err != nil {
		t.Fatalf("get rotated user: %v", err)
	}
	if got.PasswordHash != "$2a$10$new" {
		t.Fatalf("password hash not rotated: %q", got.PasswordHash)
	}
	if !got.CreatedAt.Equal(created) {
		t.Fatalf("created_at not preserved: want %v got %v", created, got.CreatedAt)
	}
}

func TestConsoleUser_DuplicateUsernameRejected(t *testing.T) {
	s := tempStore(t)
	ctx := context.Background()

	now := time.Now().UTC()
	if err := s.UpsertConsoleUser(ctx, &store.ConsoleUser{
		ID: "u1", Username: "operator", PasswordHash: "h1", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("upsert first: %v", err)
	}
	err := s.UpsertConsoleUser(ctx, &store.ConsoleUser{
		ID: "u2", Username: "operator", PasswordHash: "h2", CreatedAt: now, UpdatedAt: now,
	})
	if err == nil || !strings.Contains(err.Error(), "UNIQUE") {
		t.Fatalf("expected UNIQUE violation, got %v", err)
	}
}

func TestConsoleSession_CreateListAndCascadeDelete(t *testing.T) {
	s := tempStore(t)
	ctx := context.Background()

	now := time.Now().UTC()
	if err := s.UpsertConsoleUser(ctx, &store.ConsoleUser{
		ID: "console-default", Username: "operator", PasswordHash: "h", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("upsert user: %v", err)
	}
	for _, tok := range []string{"hash-a", "hash-b"} {
		if err := s.CreateConsoleSession(ctx, &store.ConsoleSession{
			ID: "sess-" + tok, UserID: "console-default", TokenHash: tok, CreatedAt: now,
		}); err != nil {
			t.Fatalf("create session %s: %v", tok, err)
		}
	}

	got, err := s.ListConsoleSessions(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 sessions, got %d", len(got))
	}

	// Revoking the user cascades to its sessions.
	if err := s.DeleteConsoleUsers(ctx); err != nil {
		t.Fatalf("delete users: %v", err)
	}
	got, err = s.ListConsoleSessions(ctx)
	if err != nil {
		t.Fatalf("list after user delete: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("sessions survived user deletion: %d", len(got))
	}
}

func TestConsoleSession_DeleteByUser(t *testing.T) {
	s := tempStore(t)
	ctx := context.Background()

	now := time.Now().UTC()
	for _, u := range []string{"u1", "u2"} {
		if err := s.UpsertConsoleUser(ctx, &store.ConsoleUser{
			ID: u, Username: u, PasswordHash: "h", CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("upsert %s: %v", u, err)
		}
	}
	if err := s.CreateConsoleSession(ctx, &store.ConsoleSession{ID: "s1", UserID: "u1", TokenHash: "h1", CreatedAt: now}); err != nil {
		t.Fatalf("create s1: %v", err)
	}
	if err := s.CreateConsoleSession(ctx, &store.ConsoleSession{ID: "s2", UserID: "u2", TokenHash: "h2", CreatedAt: now}); err != nil {
		t.Fatalf("create s2: %v", err)
	}

	if err := s.DeleteConsoleSessionsByUser(ctx, "u1"); err != nil {
		t.Fatalf("delete by user: %v", err)
	}
	got, err := s.ListConsoleSessions(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 || got[0].ID != "s2" {
		t.Fatalf("expected only u2's session to remain, got %+v", got)
	}
}

func TestConsoleSession_DeleteAll(t *testing.T) {
	s := tempStore(t)
	ctx := context.Background()

	now := time.Now().UTC()
	if err := s.UpsertConsoleUser(ctx, &store.ConsoleUser{
		ID: "u1", Username: "u1", PasswordHash: "h", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := s.CreateConsoleSession(ctx, &store.ConsoleSession{ID: "s1", UserID: "u1", TokenHash: "h1", CreatedAt: now}); err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := s.DeleteConsoleSessions(ctx); err != nil {
		t.Fatalf("delete sessions: %v", err)
	}
	got, err := s.ListConsoleSessions(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected no sessions, got %d", len(got))
	}
	// The user itself must survive a session-only revoke.
	if _, err := s.GetConsoleUser(ctx, "u1"); err != nil {
		t.Fatalf("user should survive session revoke: %v", err)
	}
}