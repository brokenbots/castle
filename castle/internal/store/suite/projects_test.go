package suite

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/brokenbots/castle/castle/internal/store"
)

func newProject(t *testing.T, ctx context.Context, s store.Store, id string, policy string, now time.Time) {
	t.Helper()
	p := &store.Project{ID: id, MembershipPolicy: policy, CreatedAt: now, UpdatedAt: now}
	if err := s.CreateProject(ctx, p); err != nil {
		t.Fatalf("CreateProject(%s): %v", id, err)
	}
}

// TestProjectCRUDRoundTrip covers the first-class entity: a project persists
// with its membership policy, queue list, and opaque settings/info maps;
// GET/LIST read it back; duplicates fail with ErrProjectExists; an unknown ID
// is ErrNotFound.
func TestProjectCRUDRoundTrip(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		now := microUTC(time.Now())
		newProject(t, ctx, s, "proj-a", store.ProjectMembershipOpen, now)
		newProject(t, ctx, s, "proj-b", store.ProjectMembershipClosed, now)

		got, err := s.GetProject(ctx, "proj-a")
		if err != nil {
			t.Fatalf("GetProject: %v", err)
		}
		if got.MembershipPolicy != store.ProjectMembershipOpen {
			t.Fatalf("membership policy = %q", got.MembershipPolicy)
		}
		if len(got.Queues) != 0 || got.Settings != nil || got.Info != nil {
			t.Fatalf("unset fields should read back empty: %+v", got)
		}

		closed, err := s.GetProject(ctx, "proj-b")
		if err != nil || closed.MembershipPolicy != store.ProjectMembershipClosed {
			t.Fatalf("closed project = %+v err %v", closed, err)
		}

		list, err := s.ListProjects(ctx)
		if err != nil {
			t.Fatalf("ListProjects: %v", err)
		}
		if len(list) != 2 || list[0].ID != "proj-a" || list[1].ID != "proj-b" {
			t.Fatalf("ListProjects = %+v", list)
		}

		if _, err := s.GetProject(ctx, "proj-missing"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("GetProject(unknown) = %v, want ErrNotFound", err)
		}
		if err := s.CreateProject(ctx, &store.Project{ID: "proj-a", MembershipPolicy: store.ProjectMembershipOpen}); !errors.Is(err, store.ErrProjectExists) {
			t.Fatalf("duplicate create = %v, want ErrProjectExists", err)
		}
	})
}

// TestProjectFieldsRoundTrip pins the optional project fields: queues,
// settings and info round-trip verbatim (the store persists, never
// interprets).
func TestProjectFieldsRoundTrip(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		now := microUTC(time.Now())
		p := &store.Project{
			ID:                  "proj-full",
			MembershipPolicy:    store.ProjectMembershipOpen,
			DefaultRefirePolicy: "retry-3x",
			Queues:              []string{"lane-1", "lane-2"},
			Settings:            map[string]string{"region": "eu", "profile": "prod"},
			Info:                map[string]string{"owner": "platform"},
			CreatedAt:           now,
			UpdatedAt:           now,
		}
		if err := s.CreateProject(ctx, p); err != nil {
			t.Fatalf("CreateProject: %v", err)
		}
		got, err := s.GetProject(ctx, "proj-full")
		if err != nil {
			t.Fatalf("GetProject: %v", err)
		}
		if got.DefaultRefirePolicy != "retry-3x" {
			t.Fatalf("default_refire_policy = %q", got.DefaultRefirePolicy)
		}
		if len(got.Queues) != 2 || got.Queues[0] != "lane-1" || got.Queues[1] != "lane-2" {
			t.Fatalf("queues round-trip = %v", got.Queues)
		}
		if got.Settings["region"] != "eu" || got.Settings["profile"] != "prod" {
			t.Fatalf("settings round-trip = %v", got.Settings)
		}
		if got.Info["owner"] != "platform" {
			t.Fatalf("info round-trip = %v", got.Info)
		}
	})
}

// TestProjectCreateValidation pins the store boundary: project IDs must be
// slugs, membership policies must be known values, and queue names must be
// non-empty. Unknown policies are rejected so a typo cannot silently create
// an unjoinable pool.
func TestProjectCreateValidation(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		now := microUTC(time.Now())
		bad := []*store.Project{
			{ID: "", MembershipPolicy: store.ProjectMembershipOpen},
			{ID: "Bad_Upper", MembershipPolicy: store.ProjectMembershipOpen},
			{ID: "-leading-dash", MembershipPolicy: store.ProjectMembershipOpen},
			{ID: "has spaces", MembershipPolicy: store.ProjectMembershipOpen},
			{ID: "ok-slug", MembershipPolicy: "open-unknown"},
			{ID: "ok-slug", MembershipPolicy: ""},
			{ID: "ok-slug", MembershipPolicy: store.ProjectMembershipOpen, Queues: []string{"lane", ""}},
		}
		for i, p := range bad {
			p.CreatedAt = now
			p.UpdatedAt = now
			if err := s.CreateProject(ctx, p); err == nil {
				t.Fatalf("case %d (%+v): expected rejection", i, p)
			}
		}
		// A closed policy is a valid creation value; slugs may contain dots
		// and underscores after the leading character.
		if err := s.CreateProject(ctx, &store.Project{ID: "ok.slug_1", MembershipPolicy: store.ProjectMembershipClosed, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("closed policy create: %v", err)
		}
	})
}

// TestProjectMembership covers the three membership legs: agent self-join
// (open policy only, idempotent, kind-updating), operator-managed add
// (any policy), and remove (no-op when absent). List and membership probes
// close the loop.
func TestProjectMembership(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		now := microUTC(time.Now())
		newProject(t, ctx, s, "open-p", store.ProjectMembershipOpen, now)
		newProject(t, ctx, s, "closed-p", store.ProjectMembershipClosed, now)

		// Unknown project: both legs report ErrNotFound.
		if err := s.JoinProject(ctx, "unknown", "agent-a", store.ProjectMemberKindAI, now); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("JoinProject(unknown) = %v, want ErrNotFound", err)
		}
		if err := s.AddProjectMember(ctx, &store.ProjectMember{ProjectID: "unknown", MemberID: "agent-a", Kind: store.ProjectMemberKindAI, CreatedAt: now}); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("AddProjectMember(unknown) = %v, want ErrNotFound", err)
		}

		// Closed policy refuses the self-join but admits the operator.
		if err := s.JoinProject(ctx, "closed-p", "agent-a", store.ProjectMemberKindAI, now); !errors.Is(err, store.ErrProjectClosed) {
			t.Fatalf("JoinProject(closed) = %v, want ErrProjectClosed", err)
		}
		if err := s.AddProjectMember(ctx, &store.ProjectMember{ProjectID: "closed-p", MemberID: "agent-a", Kind: store.ProjectMemberKindAI, CreatedAt: now}); err != nil {
			t.Fatalf("AddProjectMember(closed): %v", err)
		}
		if ok, err := s.IsProjectMember(ctx, "closed-p", "agent-a"); err != nil || !ok {
			t.Fatalf("IsProjectMember = %v, %v; want true, nil", ok, err)
		}

		// Open policy self-join.
		if err := s.JoinProject(ctx, "open-p", "agent-b", store.ProjectMemberKindAI, now); err != nil {
			t.Fatalf("JoinProject(open): %v", err)
		}
		// Idempotent re-join with the same kind; created_at preserved.
		if err := s.JoinProject(ctx, "open-p", "agent-b", store.ProjectMemberKindAI, now.Add(time.Hour)); err != nil {
			t.Fatalf("JoinProject(re-join): %v", err)
		}
		members, err := s.ListProjectMembers(ctx, "open-p")
		if err != nil {
			t.Fatalf("ListProjectMembers: %v", err)
		}
		if len(members) != 1 || members[0].MemberID != "agent-b" || members[0].Kind != store.ProjectMemberKindAI {
			t.Fatalf("members = %+v", members)
		}
		if !members[0].CreatedAt.Equal(microUTC(now)) {
			t.Fatalf("re-join rewrote created_at: %v, want %v", members[0].CreatedAt, microUTC(now))
		}

		// Re-join with a different kind updates the kind in place.
		if err := s.JoinProject(ctx, "open-p", "agent-b", store.ProjectMemberKindHuman, now.Add(2*time.Hour)); err != nil {
			t.Fatalf("JoinProject(kind switch): %v", err)
		}
		members, err = s.ListProjectMembers(ctx, "open-p")
		if err != nil || len(members) != 1 || members[0].Kind != store.ProjectMemberKindHuman {
			t.Fatalf("members after kind switch = %+v err %v", members, err)
		}

		// Mixed member sets (ai operators and humans) list in member-id
		// order.
		if err := s.JoinProject(ctx, "open-p", "agent-a", store.ProjectMemberKindAI, now); err != nil {
			t.Fatalf("JoinProject(agent-a): %v", err)
		}
		if err := s.AddProjectMember(ctx, &store.ProjectMember{ProjectID: "open-p", MemberID: "human-op", Kind: store.ProjectMemberKindHuman, CreatedAt: now}); err != nil {
			t.Fatalf("AddProjectMember(human-op): %v", err)
		}
		members, err = s.ListProjectMembers(ctx, "open-p")
		if err != nil {
			t.Fatalf("ListProjectMembers: %v", err)
		}
		if len(members) != 3 || members[0].MemberID != "agent-a" || members[1].MemberID != "agent-b" || members[2].MemberID != "human-op" {
			t.Fatalf("member list = %+v", members)
		}

		// Removal is idempotent.
		if err := s.RemoveProjectMember(ctx, "open-p", "agent-a"); err != nil {
			t.Fatalf("RemoveProjectMember: %v", err)
		}
		if err := s.RemoveProjectMember(ctx, "open-p", "agent-never-joined"); err != nil {
			t.Fatalf("RemoveProjectMember(unknown) = %v, want no-op nil", err)
		}
		if ok, err := s.IsProjectMember(ctx, "open-p", "agent-a"); err != nil || ok {
			t.Fatalf("IsProjectMember after removal = %v, %v; want false, nil", ok, err)
		}

		// A project that does not exist reports membership false.
		if ok, err := s.IsProjectMember(ctx, "unknown", "agent-b"); err != nil || ok {
			t.Fatalf("IsProjectMember(unknown project) = %v, %v; want false, nil", ok, err)
		}

		// Kind validation rejects unknown kinds on both legs.
		if err := s.JoinProject(ctx, "open-p", "agent-c", "kind-unknown", now); err == nil {
			t.Fatalf("JoinProject(unknown kind): expected rejection")
		}
		if err := s.AddProjectMember(ctx, &store.ProjectMember{ProjectID: "open-p", MemberID: "agent-c", Kind: "kind-unknown", CreatedAt: now}); err == nil {
			t.Fatalf("AddProjectMember(unknown kind): expected rejection")
		}
	})
}

// TestLeaseProjectScoping is the membership-enforcement acceptance leg
// (KB-318): dispatch = lease restricted to the project's member set. A
// non-member never leases a project assignment, a member does (with the
// project surfaced on the record and the agent's identity block stamped on
// the run), unscoped work stays global, and capability labels partition
// WITHIN the project pool.
func TestLeaseProjectScoping(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		now := microUTC(time.Now())
		newProject(t, ctx, s, "proj-a", store.ProjectMembershipOpen, now)
		newProject(t, ctx, s, "proj-b", store.ProjectMembershipOpen, now)
		if err := s.JoinProject(ctx, "proj-a", "agent-a", store.ProjectMemberKindAI, now); err != nil {
			t.Fatalf("join agent-a: %v", err)
		}
		if err := s.JoinProject(ctx, "proj-b", "agent-b", store.ProjectMemberKindAI, now); err != nil {
			t.Fatalf("join agent-b: %v", err)
		}

		// Both agents carry identical capability labels: routing to work is
		// via project membership; capability (labels) only partitions within
		// the pool. agent-a presents an identity block for the stamp check.
		gpuOn := map[string]string{"gpu": "true"}
		if err := s.CreateOverseer(ctx, &store.Overseer{ID: "agent-a", Name: "agent-a", TokenHash: "hash-agent-a", Status: "online",
			Labels: gpuOn, CreatedAt: now, LastSeenAt: now,
			Identity: &store.AgentIdentity{Version: "9.9.9", RepoURL: "https://git.example/agents/x", BuildDate: "2026-01-02T03:04:05Z"},
		}); err != nil {
			t.Fatalf("CreateOverseer(agent-a): %v", err)
		}
		if err := s.CreateOverseer(ctx, &store.Overseer{ID: "agent-b", Name: "agent-b", TokenHash: "hash-agent-b", Status: "online",
			Labels: gpuOn, CreatedAt: now, LastSeenAt: now}); err != nil {
			t.Fatalf("CreateOverseer(agent-b): %v", err)
		}

		a, created, err := s.CreateWorkflowAssignment(ctx, &store.WorkflowAssignment{
			OwnerCriteriaID: "owner-1",
			WorkflowName:    "wf",
			WorkflowSource:  "source",
			IdempotencyKey:  "key-a",
			Labels:          gpuOn,
			ProjectID:       "proj-a",
		})
		if err != nil || !created {
			t.Fatalf("create proj-a assignment: created=%v err=%v", created, err)
		}

		// A member of a different project (matching labels) must not lease
		// proj-a's work.
		if _, err := s.LeaseWorkflowAssignment(ctx, "agent-b", gpuOn, now, time.Minute); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("non-member lease = %v, want ErrNotFound", err)
		}

		// The member leases it; the record carries the project scope and
		// the leased run is stamped with the executing agent's identity.
		leased, err := s.LeaseWorkflowAssignment(ctx, "agent-a", gpuOn, now, time.Minute)
		if err != nil {
			t.Fatalf("member lease: %v", err)
		}
		if leased.ID != a.ID || leased.ProjectID != "proj-a" {
			t.Fatalf("leased = %+v, want assignment %s in proj-a", leased, a.ID)
		}
		run, err := s.GetRun(ctx, leased.RunID)
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		if run.OverseerID != "agent-a" {
			t.Fatalf("run overseer = %q, want agent-a", run.OverseerID)
		}
		stamped := store.IdentityFromJSON(run.AgentIdentity)
		if stamped == nil || stamped.Version != "9.9.9" {
			t.Fatalf("identity stamp round-trip = %q -> %+v", run.AgentIdentity, stamped)
		}

		// Membership is necessary but not sufficient: a member whose labels
		// do not satisfy the assignment still gets nothing.
		if _, created, err := s.CreateWorkflowAssignment(ctx, &store.WorkflowAssignment{
			OwnerCriteriaID: "owner-1",
			WorkflowName:    "wf",
			WorkflowSource:  "source",
			IdempotencyKey:  "key-zone",
			Labels:          map[string]string{"zone": "b"},
			ProjectID:       "proj-a",
		}); err != nil || !created {
			t.Fatalf("create zone assignment: created=%v err=%v", created, err)
		}
		if _, err := s.LeaseWorkflowAssignment(ctx, "agent-a", gpuOn, now, time.Minute); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("label-mismatched member lease = %v, want ErrNotFound", err)
		}
	})
}

// TestLeaseUnscopedStaysGlobal pins the pre-project behavior: unscoped
// assignments (project_id NULL) remain visible to every eligible agent, the
// leased record carries an empty project, and an agent row without an
// identity block leases normally with an empty identity stamp (pre-KB-318
// rows keep working).
func TestLeaseUnscopedStaysGlobal(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		now := microUTC(time.Now())
		newOverseer(t, s, "plain-agent", now, nil)
		if err := s.CreateOverseer(ctx, &store.Overseer{ID: "proj-member", Name: "pm", TokenHash: "hash-pm", Status: "online",
			CreatedAt: now, LastSeenAt: now}); err != nil {
			t.Fatalf("CreateOverseer(pm): %v", err)
		}
		newProject(t, ctx, s, "proj-x", store.ProjectMembershipOpen, now)
		if err := s.JoinProject(ctx, "proj-x", "proj-member", store.ProjectMemberKindAI, now); err != nil {
			t.Fatalf("join: %v", err)
		}

		unscoped, created, err := s.CreateWorkflowAssignment(ctx, &store.WorkflowAssignment{
			OwnerCriteriaID: "owner-1",
			WorkflowName:    "wf",
			WorkflowSource:  "source",
			IdempotencyKey:  "global-1",
		})
		if err != nil || !created {
			t.Fatalf("create: created=%v err=%v", created, err)
		}

		globalLease, err := s.LeaseWorkflowAssignment(ctx, "plain-agent", nil, now, time.Minute)
		if err != nil {
			t.Fatalf("unscoped lease: %v", err)
		}
		if globalLease.ID != unscoped.ID || globalLease.ProjectID != "" {
			t.Fatalf("unscoped lease = %+v, want %s with empty project", globalLease, unscoped.ID)
		}
		run, err := s.GetRun(ctx, globalLease.RunID)
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		if run.AgentIdentity != "" {
			t.Fatalf("agent without identity got a stamp %q", run.AgentIdentity)
		}
		if run.OverseerID != "plain-agent" {
			t.Fatalf("run overseer = %q, want plain-agent", run.OverseerID)
		}
	})
}

// TestAssignmentConventionsRoundTrip pins the workflow-generic assignment
// conventions (KB-318): input_values and target_metadata round-trip through
// the assignment record untouched; empty maps read back as nil; the store
// never interprets the values. Idempotency stays composed per domain: the
// (owner, idempotency) key match returns the original record.
func TestAssignmentConventionsRoundTrip(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		a, created, err := s.CreateWorkflowAssignment(ctx, &store.WorkflowAssignment{
			OwnerCriteriaID: "owner-1",
			WorkflowName:    "wf",
			WorkflowSource:  "source",
			IdempotencyKey:  "key-conv",
			InputValues: map[string]string{
				"endpoint": "https://api.example.internal",
				"model":    "gpt-x",
			},
			TargetMetadata: map[string]string{
				"adapter.docker.image":   "repo/runner:1.4",
				"adapter.docker.network": "safenet",
			},
		})
		if err != nil || !created {
			t.Fatalf("create: created=%v err=%v", created, err)
		}

		got, err := s.GetWorkflowAssignment(ctx, a.ID)
		if err != nil {
			t.Fatalf("GetWorkflowAssignment: %v", err)
		}
		if got.InputValues["endpoint"] != "https://api.example.internal" || got.InputValues["model"] != "gpt-x" {
			t.Fatalf("input_values round-trip = %v", got.InputValues)
		}
		if got.TargetMetadata["adapter.docker.image"] != "repo/runner:1.4" || got.TargetMetadata["adapter.docker.network"] != "safenet" {
			t.Fatalf("target_metadata round-trip = %v", got.TargetMetadata)
		}

		// Idempotent retry returns the original record, conventions intact.
		dup, created, err := s.CreateWorkflowAssignment(ctx, &store.WorkflowAssignment{
			OwnerCriteriaID: "owner-1",
			WorkflowName:    "other",
			WorkflowSource:  "other",
			IdempotencyKey:  "key-conv",
			InputValues:     map[string]string{"overwritten": "no"},
		})
		if err != nil || created {
			t.Fatalf("idempotent retry: created=%v err=%v", created, err)
		}
		if dup.ID != a.ID || dup.InputValues["endpoint"] != "https://api.example.internal" {
			t.Fatalf("idempotent retry = %+v", dup)
		}

		// An assignment without conventions reads back nil maps.
		plain, created, err := s.CreateWorkflowAssignment(ctx, &store.WorkflowAssignment{
			OwnerCriteriaID: "owner-2",
			WorkflowName:    "wf",
			WorkflowSource:  "source",
			IdempotencyKey:  "key-plain",
		})
		if err != nil || !created {
			t.Fatalf("plain create: created=%v err=%v", created, err)
		}
		if got, err := s.GetWorkflowAssignment(ctx, plain.ID); err != nil || got.InputValues != nil || got.TargetMetadata != nil {
			t.Fatalf("plain conventions = %v / %v err %v, want nil/nil", got.InputValues, got.TargetMetadata, err)
		}
	})
}

// TestLeasedPendingListCarriesProjectAndConventions checks the redelivery
// list surfaces project scoping and the conventions so the control path can
// merge them into the delivered control message.
func TestLeasedPendingListCarriesProjectAndConventions(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		now := microUTC(time.Now())
		newProject(t, ctx, s, "proj-a", store.ProjectMembershipOpen, now)
		if err := s.JoinProject(ctx, "proj-a", "agent-a", store.ProjectMemberKindAI, now); err != nil {
			t.Fatalf("join: %v", err)
		}
		newOverseer(t, s, "agent-a", now, nil)
		if _, created, err := s.CreateWorkflowAssignment(ctx, &store.WorkflowAssignment{
			OwnerCriteriaID: "owner-1",
			WorkflowName:    "wf",
			WorkflowSource:  "source",
			IdempotencyKey:  "key-proj",
			ProjectID:       "proj-a",
			InputValues:     map[string]string{"k": "v"},
			TargetMetadata:  map[string]string{"adapter.docker.image": "repo/runner:2.0"},
		}); err != nil || !created {
			t.Fatalf("create: created=%v err=%v", created, err)
		}

		leased, err := s.LeaseWorkflowAssignment(ctx, "agent-a", nil, now, time.Minute)
		if err != nil {
			t.Fatalf("lease: %v", err)
		}
		if leased.ProjectID != "proj-a" {
			t.Fatalf("leased project = %q, want proj-a", leased.ProjectID)
		}

		pending, err := s.ListLeasedPendingAssignmentsByCriteriaID(ctx, "agent-a")
		if err != nil {
			t.Fatalf("ListLeasedPendingAssignments: %v", err)
		}
		if len(pending) != 1 {
			t.Fatalf("pending = %d, want 1", len(pending))
		}
		got := pending[0]
		if got.ID != leased.ID || got.ProjectID != "proj-a" {
			t.Fatalf("pending = %+v", got)
		}
		if got.InputValues["k"] != "v" || got.TargetMetadata["adapter.docker.image"] != "repo/runner:2.0" {
			t.Fatalf("conventions redelivery: %+v / %+v", got.InputValues, got.TargetMetadata)
		}
	})
}

// TestOverseerIdentityRoundTrip pins the identity block persistence on the
// overseer row: CreateOverseer stores it, GetOverseer and ListOverseers both
// echo it, and a nil block reads back nil (pre-KB-318 rows keep working).
func TestOverseerIdentityRoundTrip(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		now := microUTC(time.Now())
		block := &store.AgentIdentity{
			Version:     "2.7.1",
			RepoURL:     "https://git.example/agents/parapet",
			BuildDate:   "2026-01-02T03:04:05Z",
			SDKVersions: []store.AgentSDKVersion{{Name: "criteria", Version: "4.1.0"}, {Name: "adapter", Version: "0.3.9"}},
			Extra:       map[string]string{"os": "linux", "arch": "amd64"},
		}
		if err := s.CreateOverseer(ctx, &store.Overseer{ID: "ov-id", Name: "n", TokenHash: "t", Status: "online", CreatedAt: now, LastSeenAt: now, Identity: block}); err != nil {
			t.Fatalf("CreateOverseer: %v", err)
		}
		got, err := s.GetOverseer(ctx, "ov-id")
		if err != nil {
			t.Fatalf("GetOverseer: %v", err)
		}
		if got.Identity == nil {
			t.Fatalf("GetOverseer lost the identity block")
		}
		if got.Identity.Version != "2.7.1" || got.Identity.RepoURL != "https://git.example/agents/parapet" || got.Identity.BuildDate != "2026-01-02T03:04:05Z" {
			t.Fatalf("identity = %+v", got.Identity)
		}
		if len(got.Identity.SDKVersions) != 2 || got.Identity.SDKVersions[0].Name != "adapter" || got.Identity.SDKVersions[1].Version != "4.1.0" {
			t.Fatalf("sdk versions = %+v", got.Identity.SDKVersions)
		}
		if got.Identity.Extra["os"] != "linux" || got.Identity.Extra["arch"] != "amd64" {
			t.Fatalf("extra = %+v", got.Identity.Extra)
		}

		list, err := s.ListOverseers(ctx)
		if err != nil {
			t.Fatalf("ListOverseers: %v", err)
		}
		var echoed *store.AgentIdentity
		for _, o := range list {
			if o.ID == "ov-id" {
				echoed = o.Identity
			}
		}
		if echoed == nil || echoed.Version != "2.7.1" || len(echoed.SDKVersions) != 2 || echoed.SDKVersions[0].Name != "adapter" {
			t.Fatalf("ListOverseers identity = %+v", echoed)
		}

		// A nil identity stays nil on read back.
		if err := s.CreateOverseer(ctx, &store.Overseer{ID: "ov-plain", Name: "n2", TokenHash: "t2", Status: "online", CreatedAt: now, LastSeenAt: now}); err != nil {
			t.Fatalf("CreateOverseer(plain): %v", err)
		}
		plain, err := s.GetOverseer(ctx, "ov-plain")
		if err != nil || plain.Identity != nil {
			t.Fatalf("plain overseer identity = %+v err %v, want nil", plain.Identity, err)
		}
	})
}

// TestRunIdentityStampOnCreate pins the CreateRun path: agent-owned runs
// persist the identity block passed in, a run created without one (queued
// run born outside CreateRun) carries empty text, and the stored text parses
// back to the block that was presented.
func TestRunIdentityStampOnCreate(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		now := microUTC(time.Now())
		newOverseer(t, s, "ov-id", now, nil)
		block := &store.AgentIdentity{
			Version:     "1.0.0",
			RepoURL:     "https://git.example/a",
			BuildDate:   "2026-01-01T00:00:00Z",
			SDKVersions: []store.AgentSDKVersion{{Name: "criteria", Version: "1.0.0"}},
		}
		stamp, err := store.IdentityJSON(block)
		if err != nil {
			t.Fatalf("IdentityJSON: %v", err)
		}
		if err := s.CreateRun(ctx, &store.Run{ID: "run-stamped", OverseerID: "ov-id", WorkflowName: "wf", WorkflowHCL: "hcl",
			Status: "pending", CreatedAt: now, AgentIdentity: stamp}); err != nil {
			t.Fatalf("CreateRun(stamped): %v", err)
		}
		got, err := s.GetRun(ctx, "run-stamped")
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		parsed := store.IdentityFromJSON(got.AgentIdentity)
		if parsed == nil || parsed.Version != "1.0.0" || parsed.RepoURL != "https://git.example/a" || len(parsed.SDKVersions) != 1 || parsed.SDKVersions[0].Name != "criteria" {
			t.Fatalf("identity stamp round-trip = %q -> %+v", got.AgentIdentity, parsed)
		}

		if err := s.CreateRun(ctx, &store.Run{ID: "run-unstamped", OverseerID: "ov-id", WorkflowName: "wf", WorkflowHCL: "hcl",
			Status: "pending", CreatedAt: now}); err != nil {
			t.Fatalf("CreateRun(plain): %v", err)
		}
		plain, err := s.GetRun(ctx, "run-unstamped")
		if err != nil {
			t.Fatalf("GetRun(plain): %v", err)
		}
		if plain.AgentIdentity != "" {
			t.Fatalf("plain run identity = %q", plain.AgentIdentity)
		}
	})
}
