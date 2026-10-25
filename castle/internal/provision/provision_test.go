package provision

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/brokenbots/castle/castle/internal/auth"
	"github.com/brokenbots/castle/castle/internal/store"
	"github.com/brokenbots/castle/castle/internal/store/sqlite"
)

func testStore(t *testing.T) store.Store {
	t.Helper()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "castle.db"))
	if err != nil {
		t.Fatalf("open sqlite store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	st.SetMaxOpenConns(1)
	return st
}

func TestParseAgentProjectTokens(t *testing.T) {
	cases := []struct {
		name    string
		spec    string
		want    []AgentProjectToken
		wantErr bool
	}{
		{name: "empty spec", spec: "", want: nil},
		{name: "whitespace only", spec: "  \n\t ", want: nil},
		{name: "comma separated", spec: " alpha:tok-a ,beta:tok-b", want: []AgentProjectToken{
			{Project: "alpha", Token: "tok-a"}, {Project: "beta", Token: "tok-b"},
		}},
		{name: "newline separated", spec: "alpha:tok-a\nbeta:\n gamma", want: []AgentProjectToken{
			{Project: "alpha", Token: "tok-a"}, {Project: "beta"}, {Project: "gamma"},
		}},
		{name: "token may contain colons", spec: "alpha:v1:secret-part", want: []AgentProjectToken{
			{Project: "alpha", Token: "v1:secret-part"},
		}},
		{name: "slug charset accepted", spec: "proj_x-1.2:tok", want: []AgentProjectToken{
			{Project: "proj_x-1.2", Token: "tok"},
		}},
		{name: "invalid slug rejected", spec: "Bad Slug:tok", wantErr: true},
		{name: "empty project rejected", spec: ":tok", wantErr: true},
		{name: "duplicate project rejected", spec: "alpha:tok-a\nalpha:tok-b", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseAgentProjectTokens(tc.spec)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Parse(%q) = %v, want error", tc.spec, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.spec, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("Parse(%q) = %+v, want %+v", tc.spec, got, tc.want)
			}
			for i := range tc.want {
				if got[i].Project != tc.want[i].Project || got[i].Token != tc.want[i].Token {
					t.Fatalf("Parse(%q)[%d] = %+v, want %+v", tc.spec, i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestProvisionProjectCredentialResolvesAndRotates(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	now := time.Now().UTC()

	if err := ProvisionProjectAgentCredential(ctx, st, "build-pool", "secret-alpha", now); err != nil {
		t.Fatalf("provision: %v", err)
	}

	// The credential resolves onto a DISTINCT operator row scoped to the
	// project: operator-<project> with {role: agent, project: <project>}.
	otok, err := auth.ResolveOperatorToken(ctx, st, "secret-alpha")
	if err != nil || otok == nil {
		t.Fatalf("resolve fresh credential: tok=%+v err=%v", otok, err)
	}
	if otok.OperatorID != ProjectOperatorID("build-pool") || otok.ID != ProjectTokenID("build-pool") {
		t.Fatalf("operator token = %+v, want ids %s/%s", otok, ProjectOperatorID("build-pool"), ProjectTokenID("build-pool"))
	}
	op, err := st.GetOperator(ctx, ProjectOperatorID("build-pool"))
	if err != nil {
		t.Fatalf("get operator: %v", err)
	}
	if op.Role() != store.OperatorRoleAgent || op.ProjectAttribute() != "build-pool" {
		t.Fatalf("operator attributes = %v, want role=agent project=build-pool", op.Attributes)
	}

	// Re-provisioning with the same token is idempotent (same row, same
	// material). Rotation with a new token kills the old material.
	if err := ProvisionProjectAgentCredential(ctx, st, "build-pool", "secret-alpha", now); err != nil {
		t.Fatalf("idempotent re-provision: %v", err)
	}
	if err := ProvisionProjectAgentCredential(ctx, st, "build-pool", "secret-alpha-rotated", now); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if got, err := auth.ResolveOperatorToken(ctx, st, "secret-alpha"); err != nil || got != nil {
		t.Fatalf("pre-rotation token still resolving: tok=%v err=%v", got, err)
	}
	if got, err := auth.ResolveOperatorToken(ctx, st, "secret-alpha-rotated"); err != nil || got == nil {
		t.Fatalf("rotated token does not resolve: tok=%v err=%v", got, err)
	}
}

func TestRevokeProjectCredential(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	now := time.Now().UTC()

	if err := ProvisionProjectAgentCredential(ctx, st, "build-pool", "secret-alpha", now); err != nil {
		t.Fatalf("provision: %v", err)
	}
	if err := RevokeProjectAgentCredential(ctx, st, "build-pool"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if got, err := auth.ResolveOperatorToken(ctx, st, "secret-alpha"); err != nil || got != nil {
		t.Fatalf("revoked credential still resolving: tok=%v err=%v", got, err)
	}
	if _, err := st.GetOperator(ctx, ProjectOperatorID("build-pool")); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("operator after revoke = %v, want ErrNotFound", err)
	}
	tokens, err := st.ListOperatorTokens(ctx)
	if err != nil {
		t.Fatalf("list tokens: %v", err)
	}
	if len(tokens) != 0 {
		t.Fatalf("operator tokens after revoke = %v, want empty (cascade)", tokens)
	}
	// Revoking an unprovisioned project is a no-op.
	if err := RevokeProjectAgentCredential(ctx, st, "never-provisioned"); err != nil {
		t.Fatalf("revoke unknown project: %v", err)
	}
}

func TestProvisionRejectsInvalidInput(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	now := time.Now().UTC()

	if err := ProvisionProjectAgentCredential(ctx, st, "Bad Slug!", "tok", now); err == nil {
		t.Fatal("invalid slug accepted")
	}
	if err := ProvisionProjectAgentCredential(ctx, st, "build-pool", "", now); err == nil {
		t.Fatal("empty token accepted")
	}
	if err := RevokeProjectAgentCredential(ctx, st, "Bad Slug!"); err == nil {
		t.Fatal("revoke with invalid slug accepted")
	}
}