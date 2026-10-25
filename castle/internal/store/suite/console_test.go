package suite

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/brokenbots/castle/castle/internal/store"
)

// Console identity + orchestrator token contracts enforced on both engines:
// upsert/rotation semantics, unique-username rejection, session cascade
// deletes, and operator-credential lifecycle.

// TestConsoleUserUpsertAndGet covers the first-writer-created identity shape
// with fixed IDs and timestamp round-trip.
func TestConsoleUserUpsertAndGet(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
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
	})
}

// TestConsoleUserUnknownIsNotFound pins the unknown-username login path.
func TestConsoleUserUnknownIsNotFound(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		_, err := s.GetConsoleUser(context.Background(), "nobody")
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("want ErrNotFound, got %v", err)
		}
	})
}

// TestConsoleUserRotationPreservesCreatedAt pins credential rotation: the
// password hash is replaced, the old username stops resolving, and created_at
// is preserved from the original creation.
func TestConsoleUserRotationPreservesCreatedAt(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		created := time.Date(2026, 9, 15, 8, 0, 0, 0, time.UTC)
		if err := s.UpsertConsoleUser(ctx, &store.ConsoleUser{
			ID: "console-default", Username: "operator", PasswordHash: "$2a$10$old", CreatedAt: created, UpdatedAt: created,
		}); err != nil {
			t.Fatalf("upsert: %v", err)
		}

		// Rotate username + password on the same fixed ID: the old username
		// must stop resolving and created_at must be preserved.
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
			t.Fatalf("created_at not preserved: want %v got %v", got.CreatedAt, created)
		}
	})
}

// TestConsoleUserDuplicateUsernameRejected pins the unique-username
// constraint (the exact error text is dialect-specific): the second insert
// must fail and the first identity must remain intact.
func TestConsoleUserDuplicateUsernameRejected(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()

		now := microUTC(time.Now())
		if err := s.UpsertConsoleUser(ctx, &store.ConsoleUser{
			ID: "u1", Username: "operator", PasswordHash: "h1", CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("upsert first: %v", err)
		}
		err := s.UpsertConsoleUser(ctx, &store.ConsoleUser{
			ID: "u2", Username: "operator", PasswordHash: "h2", CreatedAt: now, UpdatedAt: now,
		})
		if err == nil {
			t.Fatal("expected duplicate username rejection, got nil error")
		}
		got, gerr := s.GetConsoleUser(ctx, "operator")
		if gerr != nil || got.ID != "u1" {
			t.Fatalf("first identity must be intact after rejected duplicate: %+v err=%v", got, gerr)
		}
	})
}

// TestConsoleSessionCreateListAndCascadeDelete covers the session lifecycle:
// sessions list unsorted-but-complete, and revoking a user cascades to its
// sessions.
func TestConsoleSessionCreateListAndCascadeDelete(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()

		now := microUTC(time.Now())
		if err := s.UpsertConsoleUser(ctx, &store.ConsoleUser{
			ID: "console-default", Username: "operator", PasswordHash: "h", CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("upsert user: %v", err)
		}
		for _, tok := range []string{"hash-a", "hash-b"} {
			if err := s.CreateConsoleSession(ctx, &store.ConsoleSession{
				ID: "sess-" + tok, OperatorID: "console-default", TokenHash: tok, CreatedAt: now,
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
	})
}

// TestConsoleSessionDeleteByUser covers scoping a session revoke to one user.
func TestConsoleSessionDeleteByUser(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()

		now := microUTC(time.Now())
		for _, u := range []string{"u1", "u2"} {
			if err := s.UpsertConsoleUser(ctx, &store.ConsoleUser{
				ID: u, Username: u, PasswordHash: "h", CreatedAt: now, UpdatedAt: now,
			}); err != nil {
				t.Fatalf("upsert %s: %v", u, err)
			}
		}
		if err := s.CreateConsoleSession(ctx, &store.ConsoleSession{ID: "s1", OperatorID: "u1", TokenHash: "h1", CreatedAt: now}); err != nil {
			t.Fatalf("create s1: %v", err)
		}
		if err := s.CreateConsoleSession(ctx, &store.ConsoleSession{ID: "s2", OperatorID: "u2", TokenHash: "h2", CreatedAt: now}); err != nil {
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
	})
}

// TestConsoleSessionDeleteAll pins the session-only revoke: all sessions go,
// the user survives.
func TestConsoleSessionDeleteAll(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()

		now := microUTC(time.Now())
		if err := s.UpsertConsoleUser(ctx, &store.ConsoleUser{
			ID: "u1", Username: "u1", PasswordHash: "h", CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("upsert: %v", err)
		}
		if err := s.CreateConsoleSession(ctx, &store.ConsoleSession{ID: "s1", OperatorID: "u1", TokenHash: "h1", CreatedAt: now}); err != nil {
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
	})
}

// TestConsoleSessionRevokedAtKeepsFirstStamp covers the KB-325 revocation
// tombstone: the first revoked_at stamp wins (Logout is idempotent), the
// revoked row stays in the ledger (ListConsoleSessions returns it), and the
// issue-time membership snapshot round-trips through both engines.
func TestConsoleSessionRevokedAtKeepsFirstStamp(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		now := microUTC(time.Now())
		if err := s.UpsertConsoleUser(ctx, &store.ConsoleUser{
			ID: "u1", Username: "u1", PasswordHash: "h", CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("upsert: %v", err)
		}
		created := microUTC(time.Now())
		membership := []string{"proj-a", "proj-b"}
		if err := s.CreateConsoleSession(ctx, &store.ConsoleSession{
			ID: "s1", OperatorID: "u1", TokenHash: "h1", CreatedAt: created, Membership: membership,
		}); err != nil {
			t.Fatalf("create: %v", err)
		}

		got, err := s.ListConsoleSessions(ctx)
		if err != nil || len(got) != 1 {
			t.Fatalf("list: %v entries=%d", err, len(got))
		}
		if got[0].RevokedAt != nil {
			t.Fatalf("fresh session must not be revoked: %v", got[0].RevokedAt)
		}
		if !got[0].CreatedAt.Equal(created) {
			t.Fatalf("created_at round-trip mismatch: want %v got %v", created, got[0].CreatedAt)
		}
		if len(got[0].Membership) != 2 || got[0].Membership[0] != "proj-a" || got[0].Membership[1] != "proj-b" {
			t.Fatalf("membership snapshot not round-tripped: %+v", got[0].Membership)
		}

		revoked := microUTC(time.Now())
		if err := s.RevokeConsoleSession(ctx, "s1", revoked); err != nil {
			t.Fatalf("revoke: %v", err)
		}
		// A later stamp must not overwrite the first (first stamp wins).
		if err := s.RevokeConsoleSession(ctx, "s1", revoked.Add(time.Hour)); err != nil {
			t.Fatalf("re-revoke: %v", err)
		}

		got, err = s.ListConsoleSessions(ctx)
		if err != nil || len(got) != 1 {
			t.Fatalf("list after revoke: %v entries=%d", err, len(got))
		}
		if got[0].RevokedAt == nil || !got[0].RevokedAt.Equal(revoked) {
			t.Fatalf("revoked_at mismatch: want first stamp %v, got %v", revoked, got[0].RevokedAt)
		}
		if len(got[0].Membership) != 2 {
			t.Fatalf("membership snapshot must survive revocation: %+v", got[0].Membership)
		}
	})
}

// TestConsoleSessionRevocationEmptyMembershipAndUnknownID covers the edges:
// a session created without a membership snapshot reads back empty, and
// revoking an unknown session id is a no-op.
func TestConsoleSessionRevocationEmptyMembershipAndUnknownID(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		now := microUTC(time.Now())
		if err := s.UpsertConsoleUser(ctx, &store.ConsoleUser{
			ID: "u1", Username: "u1", PasswordHash: "h", CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("upsert: %v", err)
		}
		if err := s.CreateConsoleSession(ctx, &store.ConsoleSession{
			ID: "s1", OperatorID: "u1", TokenHash: "h1", CreatedAt: now, Membership: []string{},
		}); err != nil {
			t.Fatalf("create: %v", err)
		}
		got, err := s.ListConsoleSessions(ctx)
		if err != nil || len(got) != 1 {
			t.Fatalf("list: %v entries=%d", err, len(got))
		}
		if len(got[0].Membership) != 0 {
			t.Fatalf("explicit empty membership must read back empty, got %+v", got[0].Membership)
		}

		if err := s.RevokeConsoleSession(ctx, "sess-unknown", now); err != nil {
			t.Fatalf("revoking an unknown session id must be a no-op, got %v", err)
		}
	})
}

// TestListMemberProjectsSharesMembershipSet pins the KB-325 contract:
// ListMemberProjects reads the same project_members rows the lease scan and
// run read scope consult, ordered by project id; membership removal is
// reflected; an unknown member yields nil (not an error).
func TestListMemberProjectsSharesMembershipSet(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		now := microUTC(time.Now())
		for _, id := range []string{"proj-x", "proj-b"} {
			newProject(t, ctx, s, id, store.ProjectMembershipOpen, now)
		}
		for _, m := range []*store.ProjectMember{
			{ProjectID: "proj-x", MemberID: "member-1", Kind: store.ProjectMemberKindHuman, CreatedAt: now},
			{ProjectID: "proj-b", MemberID: "member-1", Kind: store.ProjectMemberKindAI, CreatedAt: now},
		} {
			if err := s.AddProjectMember(ctx, m); err != nil {
				t.Fatalf("AddProjectMember(%s/%s): %v", m.ProjectID, m.MemberID, err)
			}
		}

		got, err := s.ListMemberProjects(ctx, "member-1")
		if err != nil {
			t.Fatalf("ListMemberProjects: %v", err)
		}
		if len(got) != 2 || got[0] != "proj-b" || got[1] != "proj-x" {
			t.Fatalf("member projects must come back ordered by project id, got %v", got)
		}

		if err := s.RemoveProjectMember(ctx, "proj-b", "member-1"); err != nil {
			t.Fatalf("RemoveProjectMember: %v", err)
		}
		got, err = s.ListMemberProjects(ctx, "member-1")
		if err != nil || len(got) != 1 || got[0] != "proj-x" {
			t.Fatalf("membership removal must be reflected: %v %v", err, got)
		}

		// An unknown member has no projects and yields nil, not an error.
		unknown, err := s.ListMemberProjects(ctx, "member-nobody")
		if err != nil || len(unknown) != 0 {
			t.Fatalf("unknown member must yield nothing: %v %v", err, unknown)
		}
	})
}

// TestOrchestratorInsertAndList covers the orchestrator identity list shape
// with created_at round-trip.
func TestOrchestratorInsertAndList(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()

		created := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
		if err := s.UpsertOrchestrator(ctx, &store.Orchestrator{
			ID: "orchestrator-operator", Name: "operator", TokenHash: "hash-a", CreatedAt: created,
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
	})
}

// TestOrchestratorRotationPreservesCreatedAt pins token rotation: the new
// hash is the only one present and created_at survives.
func TestOrchestratorRotationPreservesCreatedAt(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
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
			t.Fatalf("created_at not preserved: want %v got %v", got[0].CreatedAt, created)
		}
	})
}

// TestOrchestratorDuplicateTokenHashRejected pins the unique token-hash
// index: a second identity presenting a taken hash fails; the exact error
// text is dialect-specific.
func TestOrchestratorDuplicateTokenHashRejected(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()

		if err := s.UpsertOrchestrator(ctx, &store.Orchestrator{
			ID: "orchestrator-operator", Name: "operator", TokenHash: "hash-shared", CreatedAt: microUTC(time.Now()),
		}); err != nil {
			t.Fatalf("upsert first: %v", err)
		}
		err := s.UpsertOrchestrator(ctx, &store.Orchestrator{
			ID: "orchestrator-secondary", Name: "secondary", TokenHash: "hash-shared", CreatedAt: microUTC(time.Now()),
		})
		if err == nil {
			t.Fatal("expected duplicate token hash to be rejected by the unique index")
		}

		// The first identity must be untouched by the failed insert.
		got, listErr := s.ListOrchestrators(ctx)
		if listErr != nil {
			t.Fatalf("list: %v", listErr)
		}
		if len(got) != 1 || got[0].ID != "orchestrator-operator" {
			t.Fatalf("expected original identity intact, got %+v", got)
		}
	})
}

// TestOrchestratorsEmpty pins the empty-store list shape.
func TestOrchestratorsEmpty(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		got, err := s.ListOrchestrators(context.Background())
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("want empty list, got %d", len(got))
		}
	})
}
