package rpc

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/brokenbots/castle/castle/internal/auth"
	"github.com/brokenbots/castle/castle/internal/store"
	"github.com/brokenbots/castle/castle/internal/store/refire"

	castlev1 "github.com/brokenbots/castle/castle/gen/castle/v1"
	castlev1connect "github.com/brokenbots/castle/castle/gen/castle/v1/castlev1connect"
)

// ProjectService (KB-318) is implemented by ServerServer, matching the
// ConsoleService precedent: one server struct can satisfy several castle-owned
// handler interfaces, and SubmitProjectAssignment reuses the same dispatch
// machinery as SubmitWorkflowAssignment.
//
// Authorization posture (KB-324): rights are membership-scoped and the
// write shape is the credential class — protocol tokens (agent identities)
// carry the assignment/run writes, console sessions carry the settings,
// refire-policy and membership writes. Kind gates and label-fallback auth
// are banned: authorization never branches on kind_ai vs kind_human.

var _ castlev1connect.ProjectServiceHandler = (*ServerServer)(nil)

// Assignment convention wire limits: input_values, target_metadata and labels
// are workflow-generic, opaque value carriers. Castle shape-validates — never
// interprets — so the RPC layer only enforces size discipline that keeps rows
// and control messages bounded.
const (
	conventionMaxEntries   = 256
	conventionMaxKeyBytes  = 256
	conventionMaxValueByte = 16 * 1024
)

func (s *ServerServer) CreateProject(ctx context.Context, req *connect.Request[castlev1.CreateProjectRequest]) (*connect.Response[castlev1.CreateProjectResponse], error) {
	if err := requireProjectCaller(ctx); err != nil {
		return nil, err
	}
	p := req.Msg.GetProject()
	if p == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("project required"))
	}
	if p.GetMembershipPolicy() == castlev1.ProjectMembershipPolicy_PROJECT_MEMBERSHIP_POLICY_UNSPECIFIED {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("membership_policy required"))
	}
	settings, err := shapeValidateStringMap("settings", p.GetSettings())
	if err != nil {
		return nil, err
	}
	info, err := shapeValidateStringMap("info", p.GetInfo())
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	policy, err2 := projectPolicyFromPB(p.GetMembershipPolicy())
	if err2 != nil {
		return nil, err2
	}
	row := &store.Project{
		ID:                  p.GetId(),
		MembershipPolicy:    policy,
		DefaultRefirePolicy: p.GetDefaultRefirePolicy(),
		Queues:              p.GetQueues(),
		Settings:            settings,
		Info:                info,
		CreatedAt:           now,
		UpdatedAt:           now,
	}
	if err3 := s.Store.CreateProject(ctx, row); err3 != nil {
		return nil, projectStoreError(err3)
	}
	return connect.NewResponse(&castlev1.CreateProjectResponse{Project: mapProject(row)}), nil
}

func (s *ServerServer) GetProject(ctx context.Context, req *connect.Request[castlev1.GetProjectRequest]) (*connect.Response[castlev1.GetProjectResponse], error) {
	if err := requireProjectCaller(ctx); err != nil {
		return nil, err
	}
	row, err := s.Store.GetProject(ctx, req.Msg.GetProjectId())
	if err != nil {
		return nil, projectStoreError(err)
	}
	return connect.NewResponse(&castlev1.GetProjectResponse{Project: mapProject(row)}), nil
}

func (s *ServerServer) ListProjects(ctx context.Context, req *connect.Request[castlev1.ListProjectsRequest]) (*connect.Response[castlev1.ListProjectsResponse], error) {
	if err := requireProjectCaller(ctx); err != nil {
		return nil, err
	}
	rows, err := s.Store.ListProjects(ctx)
	if err != nil {
		return nil, projectStoreError(err)
	}
	resp := &castlev1.ListProjectsResponse{Projects: make([]*castlev1.Project, 0, len(rows))}
	for _, p := range rows {
		resp.Projects = append(resp.Projects, mapProject(p))
	}
	return connect.NewResponse(resp), nil
}

func (s *ServerServer) AddProjectMember(ctx context.Context, req *connect.Request[castlev1.AddProjectMemberRequest]) (*connect.Response[castlev1.AddProjectMemberResponse], error) {
	// KB-324 write shape: membership is a console-session write — the
	// operator admits AI and human operators into any project regardless of
	// the join policy. Agents self-join at registration time (the
	// registration header path), never over this RPC.
	if err := requireProjectAdminCaller(ctx); err != nil {
		return nil, err
	}
	projectID := req.Msg.GetProjectId()
	if projectID == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("project_id required"))
	}
	if err := store.ValidateProjectID(projectID); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	memberID := req.Msg.GetMemberId()
	if memberID == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("member_id required"))
	}
	if req.Msg.GetKind() == castlev1.ProjectMemberKind_PROJECT_MEMBER_KIND_UNSPECIFIED {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("kind required"))
	}
	m := &store.ProjectMember{
		ProjectID: projectID,
		MemberID:  memberID,
		Kind:      projectKindFromPB(req.Msg.GetKind()),
		CreatedAt: time.Now().UTC(),
	}
	if err := s.Store.AddProjectMember(ctx, m); err != nil {
		return nil, projectStoreError(err)
	}
	return connect.NewResponse(&castlev1.AddProjectMemberResponse{Member: mapMember(m)}), nil
}

func (s *ServerServer) RemoveProjectMember(ctx context.Context, req *connect.Request[castlev1.RemoveProjectMemberRequest]) (*connect.Response[castlev1.RemoveProjectMemberResponse], error) {
	// KB-324 write shape: membership is a console-session write. Eviction is
	// idempotent (removing an unknown member is a no-op) and the project id
	// is validated the same way admits are.
	if err := requireProjectAdminCaller(ctx); err != nil {
		return nil, err
	}
	projectID := req.Msg.GetProjectId()
	if projectID == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("project_id required"))
	}
	if err := store.ValidateProjectID(projectID); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	memberID := req.Msg.GetMemberId()
	if memberID == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("member_id required"))
	}
	if err := s.Store.RemoveProjectMember(ctx, projectID, memberID); err != nil {
		return nil, projectStoreError(err)
	}
	return connect.NewResponse(&castlev1.RemoveProjectMemberResponse{}), nil
}

func (s *ServerServer) ListProjectMembers(ctx context.Context, req *connect.Request[castlev1.ListProjectMembersRequest]) (*connect.Response[castlev1.ListProjectMembersResponse], error) {
	if err := requireProjectReader(ctx); err != nil {
		return nil, err
	}
	rows, err := s.Store.ListProjectMembers(ctx, req.Msg.GetProjectId())
	if err != nil {
		return nil, projectStoreError(err)
	}
	resp := &castlev1.ListProjectMembersResponse{Members: make([]*castlev1.ProjectMember, 0, len(rows))}
	for _, m := range rows {
		resp.Members = append(resp.Members, mapMember(m))
	}
	return connect.NewResponse(resp), nil
}

func (s *ServerServer) SubmitProjectAssignment(ctx context.Context, req *connect.Request[castlev1.SubmitProjectAssignmentRequest]) (*connect.Response[castlev1.SubmitProjectAssignmentResponse], error) {
	callerID := auth.AssignmentOwnerID(ctx)
	if callerID == "" {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("caller required"))
	}
	if req.Msg.GetProjectId() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("project_id required"))
	}
	if err := store.ValidateProjectID(req.Msg.GetProjectId()); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	inputValues, err := shapeValidateStringMap("input_values", req.Msg.GetInputValues())
	if err != nil {
		return nil, err
	}
	targetMetadata, err := shapeValidateStringMap("target_metadata", req.Msg.GetTargetMetadata())
	if err != nil {
		return nil, err
	}
	labels, err := shapeValidateStringMap("labels", req.Msg.GetLabels())
	if err != nil {
		return nil, err
	}
	// The project must exist so a typo cannot silently queue into a void.
	if _, err := s.Store.GetProject(ctx, req.Msg.GetProjectId()); err != nil {
		return nil, projectStoreError(err)
	}
	if req.Msg.GetWorkflowName() == "" || req.Msg.GetWorkflowSource() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("workflow_name and workflow_source required"))
	}
	if req.Msg.GetIdempotencyKey() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("idempotency_key required"))
	}

	a := &store.WorkflowAssignment{
		OwnerCriteriaID: callerID,
		WorkflowName:    req.Msg.GetWorkflowName(),
		WorkflowSource:  req.Msg.GetWorkflowSource(),
		LockfileSource:  req.Msg.GetLockfileSource(),
		IdempotencyKey:  req.Msg.GetIdempotencyKey(),
		Labels:          labels,
		ProjectID:       req.Msg.GetProjectId(),
		InputValues:     inputValues,
		TargetMetadata:  targetMetadata,
	}
	existing, created, err := s.Store.CreateWorkflowAssignment(ctx, a)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	resp := &castlev1.SubmitProjectAssignmentResponse{
		RunId:     existing.RunID,
		State:     mapAssignmentState(existing.State),
		CreatedAt: timestamppb.New(existing.CreatedAt),
	}
	if existing.LeasedCriteriaID != "" {
		resp.LeasedCriteriaId = existing.LeasedCriteriaID
	}

	// Same fire-and-forget dispatch contract as SubmitWorkflowAssignment:
	// errors are logged, never returned to the caller, and the context is
	// detached so dispatch survives RPC completion.
	if created {
		go s.dispatchAssignment(context.WithoutCancel(ctx), existing)
	}
	return connect.NewResponse(resp), nil
}

// requireProjectCaller enforces the agent-identity posture for the project
// READ surface (registry reads: CreateProject/GetProject/ListProjects/
// ListProjectMembers). Console identities reaching a handler despite
// interceptor policy are rejected here (defense in depth) for the surfaces
// that are not console-allowed; the console-allowed reads use
// requireProjectReader instead. Trigger identities are rejected explicitly
// with PermissionDenied (KB-320).
func requireProjectCaller(ctx context.Context) error {
	if auth.CallerTriggerID(ctx) != "" {
		return connect.NewError(connect.CodePermissionDenied, errors.New("trigger identities cannot invoke this project surface"))
	}
	if auth.CallerCriteriaID(ctx) == "" {
		return connect.NewError(connect.CodeUnauthenticated, errors.New("caller required"))
	}
	return nil
}

// requireProjectReader is the KB-324 reader posture shared by the project
// surfaces a console operator administers: agent identities (the registered
// project surface callers) and console-session identities (the operator UI)
// may read; trigger and orchestrator identities may not. With no identity at
// all the auth interceptor is not wired (direct handler calls in tests), so
// the check passes through like requireCaller does.
func requireProjectReader(ctx context.Context) error {
	if auth.CallerTriggerID(ctx) != "" {
		return connect.NewError(connect.CodePermissionDenied, errors.New("trigger identities cannot invoke this project surface"))
	}
	if auth.CallerOrchestratorID(ctx) != "" {
		return connect.NewError(connect.CodePermissionDenied, errors.New("orchestrator identities cannot invoke this project surface"))
	}
	if auth.CallerCriteriaID(ctx) == "" && auth.CallerConsoleUserID(ctx) == "" {
		// Not wired (direct-call path in tests).
		return nil
	}
	return nil
}

// requireProjectAdminCaller is the KB-324 write shape for project
// administration: settings, refire policies and membership are
// console-session writes. Protocol tokens — agent and orchestrator
// identities, and trigger identities (submit-only, KB-320) — are rejected
// regardless of any membership claim. With no identity at all the auth
// interceptor is not wired (direct handler calls in tests), so the check
// passes through like requireCaller does.
func requireProjectAdminCaller(ctx context.Context) error {
	if auth.CallerTriggerID(ctx) != "" {
		return connect.NewError(connect.CodePermissionDenied, errors.New("trigger identities cannot invoke this project admin surface"))
	}
	if auth.CallerCriteriaID(ctx) != "" || auth.CallerOrchestratorID(ctx) != "" {
		return connect.NewError(connect.CodePermissionDenied, errors.New("protocol tokens cannot write project settings, policies or membership; this surface is a console-session write"))
	}
	if auth.CallerConsoleUserID(ctx) == "" {
		// Not wired (direct-call path in tests).
		return nil
	}
	return nil
}

// projectStoreError projects store errors onto wire codes for the read/write
// project surface.
func projectStoreError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, store.ErrNotFound):
		return connect.NewError(connect.CodeNotFound, err)
	case errors.Is(err, store.ErrSettingsConflict):
		// The caller's etag did not match the doc it now races against
		// (KB-324): not a permanent InvalidArgument, only a precondition
		// the writer must satisfy by re-reading first.
		return connect.NewError(connect.CodeFailedPrecondition, err)
	case errors.Is(err, store.ErrProjectExists):
		return connect.NewError(connect.CodeAlreadyExists, err)
	case errors.Is(err, store.ErrInvalidProjectID),
		errors.Is(err, store.ErrInvalidProjectPolicy),
		errors.Is(err, store.ErrInvalidProjectMember):
		return connect.NewError(connect.CodeInvalidArgument, err)
	default:
		return connect.NewError(connect.CodeInternal, err)
	}
}

func projectPolicyFromPB(v castlev1.ProjectMembershipPolicy) (string, error) {
	switch v {
	case castlev1.ProjectMembershipPolicy_PROJECT_MEMBERSHIP_POLICY_OPEN:
		return store.ProjectMembershipOpen, nil
	case castlev1.ProjectMembershipPolicy_PROJECT_MEMBERSHIP_POLICY_CLOSED:
		return store.ProjectMembershipClosed, nil
	default:
		return "", connect.NewError(connect.CodeInvalidArgument, errors.New("membership_policy required"))
	}
}

func projectPolicyToPB(v string) castlev1.ProjectMembershipPolicy {
	switch v {
	case store.ProjectMembershipOpen:
		return castlev1.ProjectMembershipPolicy_PROJECT_MEMBERSHIP_POLICY_OPEN
	case store.ProjectMembershipClosed:
		return castlev1.ProjectMembershipPolicy_PROJECT_MEMBERSHIP_POLICY_CLOSED
	default:
		return castlev1.ProjectMembershipPolicy_PROJECT_MEMBERSHIP_POLICY_UNSPECIFIED
	}
}

func projectKindToPB(v string) castlev1.ProjectMemberKind {
	switch v {
	case store.ProjectMemberKindHuman:
		return castlev1.ProjectMemberKind_PROJECT_MEMBER_KIND_HUMAN
	default:
		return castlev1.ProjectMemberKind_PROJECT_MEMBER_KIND_AI
	}
}

func mapProject(p *store.Project) *castlev1.Project {
	if p == nil {
		return nil
	}
	settings := p.Settings
	if settings == nil {
		settings = map[string]string{}
	}
	info := p.Info
	if info == nil {
		info = map[string]string{}
	}
	queues := p.Queues
	if queues == nil {
		queues = []string{}
	}
	return &castlev1.Project{
		Id:                  p.ID,
		MembershipPolicy:    projectPolicyToPB(p.MembershipPolicy),
		DefaultRefirePolicy: p.DefaultRefirePolicy,
		Queues:              queues,
		Settings:            settings,
		Info:                info,
		CreatedAt:           timestamppb.New(p.CreatedAt),
		UpdatedAt:           timestamppb.New(p.UpdatedAt),
	}
}

func mapMember(m *store.ProjectMember) *castlev1.ProjectMember {
	if m == nil {
		return nil
	}
	return &castlev1.ProjectMember{
		ProjectId: m.ProjectID,
		MemberId:  m.MemberID,
		Kind:      projectKindToPB(m.Kind),
		CreatedAt: timestamppb.New(m.CreatedAt),
	}
}

// shapeValidateStringMap enforces the size discipline for opaque convention
// maps (input_values, target_metadata, labels, project settings/info):
// bounded entry count, key length and value length. Contents are never
// interpreted.
func shapeValidateStringMap(name string, m map[string]string) (map[string]string, error) {
	if len(m) > conventionMaxEntries {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New(name+" exceeds "+strconv.Itoa(conventionMaxEntries)+" entries"))
	}
	for k, v := range m {
		if len(k) == 0 || len(k) > conventionMaxKeyBytes {
			return nil, connect.NewError(connect.CodeInvalidArgument,
				errors.New(name+" key length must be 1.."+strconv.Itoa(conventionMaxKeyBytes)+" bytes"))
		}
		if len(v) > conventionMaxValueByte {
			return nil, connect.NewError(connect.CodeInvalidArgument,
				errors.New(name+" value exceeds "+strconv.Itoa(conventionMaxValueByte)+" bytes"))
		}
	}
	return m, nil
}

// GetProjectSettings is the KB-324 settings read: the project's ONE
// convention-keyed settings doc plus its content-addressed etag — the etag
// a SetProjectSettings swap reconciles against.
func (s *ServerServer) GetProjectSettings(ctx context.Context, req *connect.Request[castlev1.GetProjectSettingsRequest]) (*connect.Response[castlev1.GetProjectSettingsResponse], error) {
	if err := requireProjectReader(ctx); err != nil {
		return nil, err
	}
	projectID := req.Msg.GetProjectId()
	if projectID == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("project_id required"))
	}
	if err := store.ValidateProjectID(projectID); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	settings, etag, err := s.Store.GetProjectSettings(ctx, projectID)
	if err != nil {
		return nil, projectStoreError(err)
	}
	return connect.NewResponse(&castlev1.GetProjectSettingsResponse{
		Settings: settings,
		Etag:     etag,
	}), nil
}

// SetProjectSettings is the KB-324 settings write: the whole doc swaps under
// optimistic etag semantics. Values are convention-keyed JSON: shape-validated
// for bounded entries/keys and parsed for JSON validity, never interpreted.
// A stale expectation fails with FailedPrecondition so the writer re-reads
// and refires; a concurrent writer never clobbers.
func (s *ServerServer) SetProjectSettings(ctx context.Context, req *connect.Request[castlev1.SetProjectSettingsRequest]) (*connect.Response[castlev1.SetProjectSettingsResponse], error) {
	if err := requireProjectAdminCaller(ctx); err != nil {
		return nil, err
	}
	projectID := req.Msg.GetProjectId()
	if projectID == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("project_id required"))
	}
	if err := store.ValidateProjectID(projectID); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	settings, err := shapeValidateStringMap("settings", req.Msg.GetSettings())
	if err != nil {
		return nil, err
	}
	for k, v := range settings {
		if v == "" {
			continue // an empty value is the "entry absent" convention
		}
		if !json.Valid([]byte(v)) {
			return nil, connect.NewError(connect.CodeInvalidArgument,
				errors.New("settings["+strconv.Quote(k)+"] must be valid JSON"))
		}
	}
	etag, err := s.Store.SetProjectSettings(ctx, projectID, settings, req.Msg.GetExpectedEtag())
	if err != nil {
		return nil, projectStoreError(err)
	}
	return connect.NewResponse(&castlev1.SetProjectSettingsResponse{Etag: etag}), nil
}

// GetRefirePolicy reads the exact policy row (project_id, route) — not the
// engine's resolution ladder; an absent row is NotFound.
func (s *ServerServer) GetRefirePolicy(ctx context.Context, req *connect.Request[castlev1.GetRefirePolicyRequest]) (*connect.Response[castlev1.GetRefirePolicyResponse], error) {
	if err := requireProjectReader(ctx); err != nil {
		return nil, err
	}
	projectID, route := req.Msg.GetProjectId(), req.Msg.GetRoute()
	if projectID != "" {
		if err := store.ValidateProjectID(projectID); err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
	}
	route, err := validateRefireRoute(route)
	if err != nil {
		return nil, err
	}
	row, err := s.refirePolicyRow(ctx, projectID, route)
	if err != nil {
		return nil, projectStoreError(err)
	}
	policy, err := refire.ParsePolicy(row.Policy)
	if err != nil {
		// Policy rows only enter the store through MarshalPolicy-shaped
		// documents; a stale/corrupt row surfaces as Internal, not a
		// request failure the caller could retry meaningfully.
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	resp := &castlev1.GetRefirePolicyResponse{
		Policy:    mapRefirePolicy(&policy),
		UpdatedAt: timestamppb.New(row.UpdatedAt),
	}
	if resp.UpdatedAt == nil || row.UpdatedAt.IsZero() {
		resp.UpdatedAt = nil
	}
	return connect.NewResponse(resp), nil
}

// SetRefirePolicy upserts the policy row with the same strict validation the
// policy file loader applies: non-negative budgets and bounded vocabularies,
// the whole row replaced.
func (s *ServerServer) SetRefirePolicy(ctx context.Context, req *connect.Request[castlev1.SetRefirePolicyRequest]) (*connect.Response[castlev1.SetRefirePolicyResponse], error) {
	if err := requireProjectAdminCaller(ctx); err != nil {
		return nil, err
	}
	projectID, route := req.Msg.GetProjectId(), req.Msg.GetRoute()
	if projectID != "" {
		if err := store.ValidateProjectID(projectID); err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
	}
	route, err := validateRefireRoute(route)
	if err != nil {
		return nil, err
	}
	wp := req.Msg.GetPolicy()
	if wp == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("policy required"))
	}
	if wp.GetMaxAttempts() < 0 || wp.GetBackoffSeconds() < 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("max_attempts and backoff_seconds must be non-negative"))
	}
	if err := shapeValidateRefireVocabulary("transient", wp.GetTransient()); err != nil {
		return nil, err
	}
	if err := shapeValidateRefireVocabulary("parks", wp.GetParks()); err != nil {
		return nil, err
	}
	if err := shapeValidateRefireVocabulary("cosmetics", wp.GetCosmetics()); err != nil {
		return nil, err
	}
	p := refire.Policy{
		MaxAttempts: int(wp.GetMaxAttempts()),
		Backoff:     time.Duration(wp.GetBackoffSeconds()) * time.Second,
		Transient:   wp.GetTransient(),
		Parks:       wp.GetParks(),
		Cosmetics:   wp.GetCosmetics(),
	}
	// Round-trip through the store's own parser with MarshalPolicy so the
	// row written is exactly the document ParsePolicy would read back: the
	// same strictness the refire policy file loader runs at boot.
	if _, err := refire.ParsePolicy(refire.MustMarshalPolicy(p)); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	doc, err := refire.MarshalPolicy(p)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	now := time.Now().UTC()
	if err := s.Store.UpsertRefirePolicy(ctx, projectID, route, doc, now); err != nil {
		return nil, projectStoreError(err)
	}
	return connect.NewResponse(&castlev1.SetRefirePolicyResponse{UpdatedAt: timestamppb.New(now)}), nil
}

// refirePolicyRow scans the policy rows for the exact (projectID, route)
// key — the engine-side resolution already covers reads through the store's
// existing methods, so no new store surface is minted for one key.
func (s *ServerServer) refirePolicyRow(ctx context.Context, projectID, route string) (*store.RefirePolicyRow, error) {
	rows, err := s.Store.ListRefirePolicies(ctx)
	if err != nil {
		return nil, err
	}
	for i := range rows {
		if rows[i].ProjectID == projectID && rows[i].Route == route {
			return &rows[i], nil
		}
	}
	return nil, store.ErrNotFound
}

// validateRefireRoute bounds the route key (the vocabulary the engine and
// policy files key on); routes are opaque strings.
func validateRefireRoute(route string) (string, error) {
	if len(route) > conventionMaxKeyBytes {
		return "", connect.NewError(connect.CodeInvalidArgument,
			errors.New("route length exceeds "+strconv.Itoa(conventionMaxKeyBytes)+" bytes"))
	}
	return route, nil
}

// shapeValidateRefireVocabulary bounds one classification vocabulary: the
// entry-count and per-value limits match the convention maps.
func shapeValidateRefireVocabulary(name string, values []string) error {
	if len(values) > conventionMaxEntries {
		return connect.NewError(connect.CodeInvalidArgument,
			errors.New(name+" exceeds "+strconv.Itoa(conventionMaxEntries)+" entries"))
	}
	for _, v := range values {
		if len(v) == 0 || len(v) > conventionMaxValueByte {
			return connect.NewError(connect.CodeInvalidArgument,
				errors.New(name+" value length must be 1.."+strconv.Itoa(conventionMaxValueByte)+" bytes"))
		}
	}
	return nil
}

func mapRefirePolicy(p *refire.Policy) *castlev1.RefirePolicy {
	if p == nil {
		return nil
	}
	return &castlev1.RefirePolicy{
		MaxAttempts:    int32(p.MaxAttempts),
		BackoffSeconds: int32(p.Backoff / time.Second),
		Transient:      p.Transient,
		Parks:          p.Parks,
		Cosmetics:      p.Cosmetics,
	}
}

// projectKindFromPB maps the wire kind onto the store vocabulary; the
// unspecified kind is rejected by the caller before mapping.
func projectKindFromPB(v castlev1.ProjectMemberKind) string {
	if v == castlev1.ProjectMemberKind_PROJECT_MEMBER_KIND_HUMAN {
		return store.ProjectMemberKindHuman
	}
	return store.ProjectMemberKindAI
}
