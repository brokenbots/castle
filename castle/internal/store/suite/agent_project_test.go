package suite

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/brokenbots/castle/castle/internal/auth"
	"github.com/brokenbots/castle/castle/internal/provision"
	"github.com/brokenbots/castle/castle/internal/store"
)

// TestProjectAgentCredentialLifecycle drives the per-project agent credential
// model (KB-337 P0.1 deliverable 1) through both store engines: provisioning
// resolves to a DISTINCT operator row (role=agent, project=<project>) — the
// shared platform/console operator row is never touched — and the credential
// round-trips rotate and revoke.
func TestProjectAgentCredentialLifecycle(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		now := microUTC(time.Now())

		// A pre-existing platform operator row: provisioning a project
		// credential must never disturb it.
		if err := s.UpsertOperator(ctx, &store.Operator{
			ID:         "console-admin",
			Name:       "console-admin",
			Attributes: store.ConsoleOperatorAttributes("admin"),
			CreatedAt:  now,
			UpdatedAt:  now,
		}); err != nil {
			t.Fatalf("seed console operator: %v", err)
		}

		tokID := provision.ProjectTokenID("proj-a")
		if err := provision.ApplyAgentProjectTokens(ctx, s, []provision.AgentProjectToken{
			{Project: "proj-a", Token: "agent-proj-a-cred"},
		}, now); err != nil {
			t.Fatalf("provision proj-a credential: %v", err)
		}

		// The credential resolves to its own operator row.
		tok, err := auth.ResolveOperatorToken(ctx, s, "agent-proj-a-cred")
		if err != nil {
			t.Fatalf("resolve provisioned credential: %v", err)
		}
		if tok.ID != tokID || tok.OperatorID != provision.ProjectOperatorID("proj-a") {
			t.Fatalf("credential row = id %q operator %q, want %q / %q", tok.ID, tok.OperatorID, tokID, provision.ProjectOperatorID("proj-a"))
		}
		op, err := s.GetOperator(ctx, provision.ProjectOperatorID("proj-a"))
		if err != nil {
			t.Fatalf("get project operator: %v", err)
		}
		if op.Role() != store.OperatorRoleAgent || op.ProjectAttribute() != "proj-a" {
			t.Fatalf("project operator attributes = role %q project %q, want %q/%q", op.Role(), op.ProjectAttribute(), store.OperatorRoleAgent, "proj-a")
		}

		// The platform operator row is untouched (distinct identity).
		console, err := s.GetOperator(ctx, "console-admin")
		if err != nil {
			t.Fatalf("get console operator: %v", err)
		}
		if console.Role() != store.OperatorRoleAdmin || console.ProjectAttribute() != "" {
			t.Fatalf("console operator mutated: role %q project %q", console.Role(), console.ProjectAttribute())
		}

		// Rotation: provisioning again re-keys the token; the old material
		// stops resolving (ResolveOperatorToken yields nil, nil for an
		// unknown token) while the operator row keeps its scope.
		if err := provision.ProvisionProjectAgentCredential(ctx, s, "proj-a", "agent-proj-a-cred-2", now); err != nil {
			t.Fatalf("rotate proj-a credential: %v", err)
		}
		if old, err := auth.ResolveOperatorToken(ctx, s, "agent-proj-a-cred"); old != nil || err != nil {
			t.Fatalf("old credential resolve = %+v, %v, want nil, nil", old, err)
		}
		if _, err := auth.ResolveOperatorToken(ctx, s, "agent-proj-a-cred-2"); err != nil {
			t.Fatalf("new credential resolve: %v", err)
		}
		op, err = s.GetOperator(ctx, provision.ProjectOperatorID("proj-a"))
		if err != nil {
			t.Fatalf("operator row survives rotation: %v", err)
		}
		if op.ProjectAttribute() != "proj-a" {
			t.Fatalf("rotation changed scope: project %q", op.ProjectAttribute())
		}

		// Revocation deletes the operator row; the delete cascades the
		// token (the credential stops authenticating). Revoking an
		// unprovisioned project is a no-op.
		if err := provision.RevokeProjectAgentCredential(ctx, s, "proj-a"); err != nil {
			t.Fatalf("revoke proj-a credential: %v", err)
		}
		if _, err := s.GetOperator(ctx, provision.ProjectOperatorID("proj-a")); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("operator row after revoke = %v, want not found", err)
		}
		if revoked, err := auth.ResolveOperatorToken(ctx, s, "agent-proj-a-cred-2"); revoked != nil || err != nil {
			t.Fatalf("credential resolve after revoke = %+v, %v, want nil, nil", revoked, err)
		}
		if err := provision.RevokeProjectAgentCredential(ctx, s, "proj-never"); err != nil {
			t.Fatalf("revoke unprovisioned project: %v", err)
		}

		// The console operator still resolves after all project churn.
		if _, err := s.GetOperator(ctx, "console-admin"); err != nil {
			t.Fatalf("console operator vanished: %v", err)
		}
	})
}

// TestAgentCredentialJoinFailsClosed covers deliverable 2's store leg (both
// engines): membership is the binding — a per-project agent identity may only
// join its own project row, which must exist. The lease engine then keeps
// project assignments inside the granted member set.
func TestAgentCredentialJoinFailsClosed(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		now := microUTC(time.Now())

		// No project row exists: joining fails closed even on the
		// registration-bypass path the per-project credential uses.
		if err := s.AddProjectMember(ctx, &store.ProjectMember{
			ProjectID: "ghost-p", MemberID: "agent-x", Kind: store.ProjectMemberKindAI, CreatedAt: now,
		}); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("AddProjectMember(ghost project) = %v, want not found", err)
		}

		// Membership is granted per project; a CLOSED project only admits
		// members through this bypass (the credential path) — and once
		// admitted the agent leases exactly its project's assignments.
		if err := s.CreateProject(ctx, &store.Project{
			ID: "proj-a", MembershipPolicy: store.ProjectMembershipClosed, CreatedAt: now,
		}); err != nil {
			t.Fatalf("create proj-a: %v", err)
		}
		if err := s.CreateProject(ctx, &store.Project{
			ID: "proj-b", MembershipPolicy: store.ProjectMembershipClosed, CreatedAt: now,
		}); err != nil {
			t.Fatalf("create proj-b: %v", err)
		}

		// Stand up the registered agent rows (registration mints a fresh
		// overseer row and joins it — here we replay the resulting state).
		agentA := &store.Overseer{ID: "proj-a-agent", Name: "gate-pool-runner", TokenHash: "hash-a", Status: "online", CreatedAt: now, LastSeenAt: now,
			Identity: &store.AgentIdentity{Version: "9.9.9", RepoURL: "https://git.example/agents/x", BuildDate: "2026-01-02T03:04:05Z"}}
		if err := s.CreateOverseer(ctx, agentA); err != nil {
			t.Fatalf("CreateOverseer(agent-a): %v", err)
		}
		if err := s.CreateOverseer(ctx, &store.Overseer{ID: "proj-b-agent", Name: "beta-runner", TokenHash: "hash-b", Status: "online", CreatedAt: now, LastSeenAt: now}); err != nil {
			t.Fatalf("CreateOverseer(agent-b): %v", err)
		}
		if err := s.AddProjectMember(ctx, &store.ProjectMember{ProjectID: "proj-a", MemberID: "proj-a-agent", Kind: store.ProjectMemberKindAI, CreatedAt: now}); err != nil {
			t.Fatalf("join proj-a (bypass, closed project): %v", err)
		}
		if err := s.AddProjectMember(ctx, &store.ProjectMember{ProjectID: "proj-b", MemberID: "proj-b-agent", Kind: store.ProjectMemberKindAI, CreatedAt: now}); err != nil {
			t.Fatalf("join proj-b (bypass, closed project): %v", err)
		}

		// Deliverable 3/5 acceptance: the proj-a agent leases the proj-a
		// assignment with project_id stamped on the minted run; the proj-b
		// agent can neither lease it nor read the resulting run.
		if _, _, err := s.CreateWorkflowAssignment(ctx, &store.WorkflowAssignment{
			OwnerCriteriaID: "owner-1",
			WorkflowName:    "wf",
			WorkflowSource:  "source",
			IdempotencyKey:  "key-a",
			ProjectID:       "proj-a",
		}); err != nil {
			t.Fatalf("create proj-a assignment: %v", err)
		}

		// A member of another project sees NO work in this project's pool
		// (membership is checked in the scan itself - the assignment is
		// simply not visible): leasing while the assignment is still queued
		// reports the same not-found as an empty pool.
		if _, err := s.LeaseWorkflowAssignment(ctx, "proj-b-agent", nil, now, time.Minute); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("foreign agent leased project assignment: %v", err)
		}

		a, err := s.LeaseWorkflowAssignment(ctx, "proj-a-agent", nil, now, time.Minute)
		if err != nil {
			t.Fatalf("lease as proj-a member: %v", err)
		}
		if a == nil {
			t.Fatalf("lease as proj-a member: nil assignment")
		}
		if a.ProjectID != "proj-a" {
			t.Fatalf("leased assignment project = %q, want proj-a", a.ProjectID)
		}
		run, err := s.GetRun(ctx, a.RunID)
		if err != nil {
			t.Fatalf("GetRun after lease: %v", err)
		}
		if run.OverseerID != "proj-a-agent" {
			t.Fatalf("run overseer = %q, want proj-a-agent", run.OverseerID)
		}
		if stamped := store.IdentityFromJSON(run.AgentIdentity); stamped == nil || stamped.Version != "9.9.9" {
			t.Fatalf("identity stamp round-trip = %q -> %+v", run.AgentIdentity, stamped)
		}
	})
}