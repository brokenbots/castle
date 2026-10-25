package suite

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/brokenbots/castle/castle/internal/store"
)

// Operator identity contracts enforced on both engines (KB-330): the ONE
// operator model behind every credential class, its operator_tokens space,
// the password/token-only identity split, and the login-namespace name
// uniqueness scoped to password rows.

// TestOperatorUpsertAndGet pins the identity shape round-trip: a
// password-carrying operator (the console class) and a token-only operator
// (legacy alias) both round-trip their attributes, and Role() reads the role
// attribute.
func TestOperatorUpsertAndGet(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		created := time.Date(2026, 10, 25, 9, 0, 0, 0, time.UTC)

		admin := &store.Operator{
			ID: "op-admin", Name: "alice", DisplayName: "Alice Admin",
			PasswordHash: strptr("$2a$10$hash"),
			Attributes:   map[string]string{store.OperatorAttributeRole: store.OperatorRoleAdmin},
			CreatedAt:    created, UpdatedAt: created,
		}
		if err := s.UpsertOperator(ctx, admin); err != nil {
			t.Fatalf("upsert admin: %v", err)
		}
		tokenOnly := &store.Operator{
			ID: "op-agent", Name: "worker",
			Attributes: map[string]string{store.OperatorAttributeRole: store.OperatorRoleAgent},
			CreatedAt:  created, UpdatedAt: created,
		}
		if err := s.UpsertOperator(ctx, tokenOnly); err != nil {
			t.Fatalf("upsert token-only: %v", err)
		}

		gotAdmin, err := s.GetOperator(ctx, "op-admin")
		if err != nil {
			t.Fatalf("get admin: %v", err)
		}
		if gotAdmin.Name != "alice" || gotAdmin.DisplayName != "Alice Admin" {
			t.Fatalf("unexpected admin identity: %+v", gotAdmin)
		}
		if gotAdmin.PasswordHash == nil || *gotAdmin.PasswordHash != "$2a$10$hash" {
			t.Fatalf("password hash must round-trip: %+v", gotAdmin.PasswordHash)
		}
		if gotAdmin.Role() != store.OperatorRoleAdmin {
			t.Fatalf("admin role attribute lost: %+v", gotAdmin.Attributes)
		}

		gotAgent, err := s.GetOperator(ctx, "op-agent")
		if err != nil {
			t.Fatalf("get token-only: %v", err)
		}
		if gotAgent.PasswordHash != nil {
			t.Fatalf("token-only operator must carry no password: %+v", gotAgent.PasswordHash)
		}
		if gotAgent.Role() != store.OperatorRoleAgent {
			t.Fatalf("agent role attribute lost: %+v", gotAgent.Attributes)
		}

		if _, err := s.GetOperator(ctx, "nobody"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("unknown operator: want ErrNotFound, got %v", err)
		}
	})
}

// TestOperatorRotationPreservesCreatedAt pins identity rotation: an upserted
// operator keeps its original creation timestamp while carrying the new
// attributes.
func TestOperatorRotationPreservesCreatedAt(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		created := time.Date(2026, 10, 25, 9, 0, 0, 0, time.UTC)
		updated := created.Add(time.Hour)
		if err := s.UpsertOperator(ctx, &store.Operator{
			ID: "op-1", Name: "worker",
			Attributes: map[string]string{store.OperatorAttributeRole: store.OperatorRoleAgent},
			CreatedAt:  created, UpdatedAt: created,
		}); err != nil {
			t.Fatalf("upsert: %v", err)
		}
		if err := s.UpsertOperator(ctx, &store.Operator{
			ID: "op-1", Name: "worker",
			Attributes: map[string]string{store.OperatorAttributeRole: store.OperatorRoleAdmin},
			CreatedAt:  updated, UpdatedAt: updated,
		}); err != nil {
			t.Fatalf("re-upsert: %v", err)
		}
		got, err := s.GetOperator(ctx, "op-1")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if !got.CreatedAt.Equal(created) {
			t.Fatalf("created_at must survive rotation: got %v want %v", got.CreatedAt, created)
		}
		if got.Role() != store.OperatorRoleAdmin {
			t.Fatalf("rotated attributes must win: %+v", got.Attributes)
		}
	})
}

// TestOperatorTokenRotateListAndDelete pins the operator_tokens credential
// space: tokens list, rotate (same id, new hash), and revoke by id.
func TestOperatorTokenRotateListAndDelete(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		created := time.Date(2026, 10, 25, 9, 0, 0, 0, time.UTC)
		if err := s.UpsertOperator(ctx, &store.Operator{
			ID: "op-1", Name: "alice",
			Attributes: map[string]string{store.OperatorAttributeRole: store.OperatorRoleAdmin},
			CreatedAt:  created, UpdatedAt: created,
		}); err != nil {
			t.Fatalf("operator: %v", err)
		}

		tok := &store.OperatorToken{ID: "tok-1", OperatorID: "op-1", TokenHash: "hash-a", CreatedAt: created}
		if err := s.UpsertOperatorToken(ctx, tok); err != nil {
			t.Fatalf("upsert token: %v", err)
		}
		tokens, err := s.ListOperatorTokens(ctx)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(tokens) != 1 || tokens[0].ID != "tok-1" || tokens[0].TokenHash != "hash-a" {
			t.Fatalf("unexpected tokens: %+v", tokens)
		}

		// Rotation: same token id, new hash — the old hash stops resolving.
		if err := s.UpsertOperatorToken(ctx, &store.OperatorToken{ID: "tok-1", OperatorID: "op-1", TokenHash: "hash-b", CreatedAt: created.Add(time.Minute)}); err != nil {
			t.Fatalf("rotate token: %v", err)
		}
		tokens, err = s.ListOperatorTokens(ctx)
		if err != nil {
			t.Fatalf("list after rotation: %v", err)
		}
		if len(tokens) != 1 || tokens[0].TokenHash != "hash-b" {
			t.Fatalf("rotation must replace the hash in place: %+v", tokens)
		}

		// Revocation: the token row disappears; the operator identity stays.
		if err := s.DeleteOperatorToken(ctx, "tok-1"); err != nil {
			t.Fatalf("revoke: %v", err)
		}
		tokens, err = s.ListOperatorTokens(ctx)
		if err != nil {
			t.Fatalf("list after revoke: %v", err)
		}
		if len(tokens) != 0 {
			t.Fatalf("revoked token must vanish: %+v", tokens)
		}
		if _, err := s.GetOperator(ctx, "op-1"); err != nil {
			t.Fatalf("operator must survive token revocation: %v", err)
		}
	})
}

// TestOperatorDeleteCascadesTokens pins that deleting an operator identity
// removes its tokens with it (the migration-level ON DELETE CASCADE).
func TestOperatorDeleteCascadesTokens(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		created := time.Date(2026, 10, 25, 9, 0, 0, 0, time.UTC)
		if err := s.UpsertOperator(ctx, &store.Operator{
			ID: "op-1", Name: "alice",
			Attributes: map[string]string{store.OperatorAttributeRole: store.OperatorRoleAdmin},
			CreatedAt:  created, UpdatedAt: created,
		}); err != nil {
			t.Fatalf("operator: %v", err)
		}
		for _, id := range []string{"tok-1", "tok-2"} {
			if err := s.UpsertOperatorToken(ctx, &store.OperatorToken{ID: id, OperatorID: "op-1", TokenHash: "hash-" + id, CreatedAt: created}); err != nil {
				t.Fatalf("upsert %s: %v", id, err)
			}
		}
		if err := s.DeleteOperator(ctx, "op-1"); err != nil {
			t.Fatalf("delete operator: %v", err)
		}
		tokens, err := s.ListOperatorTokens(ctx)
		if err != nil {
			t.Fatalf("list tokens: %v", err)
		}
		if len(tokens) != 0 {
			t.Fatalf("operator deletion must cascade to tokens: %+v", tokens)
		}
		if _, err := s.GetOperator(ctx, "op-1"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("deleted operator must be ErrNotFound, got %v", err)
		}
	})
}

// TestAliasOperatorMaySharePasswordOperatorName pins the login-namespace
// scoping: a token-only alias operator (e.g. an orchestrator named like a
// console username) may share a password operator's name — the legacy class
// registries never shared a name namespace — while two password operators
// with the same name still collide, because login resolves by name.
func TestAliasOperatorMaySharePasswordOperatorName(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		created := time.Date(2026, 10, 25, 9, 0, 0, 0, time.UTC)
		if err := s.UpsertOperator(ctx, &store.Operator{
			ID: "op-alice", Name: "alice", DisplayName: "alice",
			PasswordHash: strptr("$2a$10$hash"),
			Attributes:   map[string]string{store.OperatorAttributeRole: store.OperatorRoleAdmin},
			CreatedAt:    created, UpdatedAt: created,
		}); err != nil {
			t.Fatalf("password operator: %v", err)
		}
		// Distinct id, same name, no password — allowed alias shape.
		if err := s.UpsertOperator(ctx, &store.Operator{
			ID: "orch-1", Name: "alice",
			Attributes: map[string]string{store.OperatorAttributeRole: store.OperatorRoleOrchestrator},
			CreatedAt:  created, UpdatedAt: created,
		}); err != nil {
			t.Fatalf("alias operator sharing the name: %v", err)
		}
		// Second PASSWORD operator with the same name is a login collision.
		err := s.UpsertOperator(ctx, &store.Operator{
			ID: "op-alice-2", Name: "alice", DisplayName: "alice again",
			PasswordHash: strptr("$2a$10$other"),
			Attributes:   map[string]string{store.OperatorAttributeRole: store.OperatorRoleAdmin},
			CreatedAt:    created, UpdatedAt: created,
		})
		if err == nil {
			t.Fatal("two password operators with the same name must be rejected")
		}

		// Login still resolves the one password row under the shared name…
		user, err := s.GetConsoleUser(ctx, "alice")
		if err != nil {
			t.Fatalf("login namespace must still resolve: %v", err)
		}
		if user.ID != "op-alice" {
			t.Fatalf("login must resolve the password row, got %+v", user)
		}
		// …and the alias row keeps its own identity.
		alias, err := s.GetOperator(ctx, "orch-1")
		if err != nil {
			t.Fatalf("alias operator: %v", err)
		}
		if alias.Role() != store.OperatorRoleOrchestrator {
			t.Fatalf("alias role lost: %+v", alias.Attributes)
		}
	})
}

// strptr is a suite helper for optional string columns.
func strptr(s string) *string { return &s }
