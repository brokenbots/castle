package rpc

import (
	"context"
	"errors"
	"strconv"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/brokenbots/castle/castle/internal/auth"
	"github.com/brokenbots/castle/castle/internal/store"

	castlev1 "github.com/brokenbots/castle/castle/gen/castle/v1"
	castlev1connect "github.com/brokenbots/castle/castle/gen/castle/v1/castlev1connect"
)

// ProjectService (KB-318) is implemented by ServerServer, matching the
// ConsoleService precedent: one server struct can satisfy several castle-owned
// handler interfaces, and SubmitProjectAssignment reuses the same dispatch
// machinery as SubmitWorkflowAssignment.
//
// Authorization posture (pre-user-auth): ProjectService procedures accept
// agent identities only. The auth interceptor denies console and orchestrator
// callers for these procedures (they are in neither allow set); the handlers
// below re-check CallerCriteriaID as defense in depth. Full API auth by
// project arrives with users (deferred by the KB-318 design).
//
// Member management is strictly self-service at this layer: a caller may
// add only itself, and only to OPEN projects (closed admission is
// operator-managed and has no self-service join), and may remove only
// itself. Registered callers may create projects and read the project
// registry. Human-kind membership and administrator roles arrive with
// authenticated operators.

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
	if err := requireProjectCaller(ctx); err != nil {
		return nil, err
	}
	// Membership is operator-managed for closed projects and self-service
	// otherwise: a caller may only register itself, never other identities
	// (admission tooling for closed projects and human-kind management
	// arrives with authenticated operators). This is the wire-level mirror
	// of the registration self-join, rejected identically.
	callerID := auth.CallerCriteriaID(ctx)
	memberID := req.Msg.GetMemberId()
	if memberID == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("member_id required"))
	}
	if memberID != callerID {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("members may only join themselves"))
	}
	if req.Msg.GetKind() == castlev1.ProjectMemberKind_PROJECT_MEMBER_KIND_HUMAN {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("human-kind membership is operator-managed"))
	}
	policy, err := s.projectMembershipPolicy(ctx, req.Msg.GetProjectId())
	if err != nil {
		return nil, err
	}
	if policy == store.ProjectMembershipClosed {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("closed project admission is operator-managed"))
	}
	m := &store.ProjectMember{
		ProjectID: req.Msg.GetProjectId(),
		MemberID:  memberID,
		Kind:      store.ProjectMemberKindAI,
		CreatedAt: time.Now().UTC(),
	}
	if err := s.Store.JoinProject(ctx, req.Msg.GetProjectId(), memberID, store.ProjectMemberKindAI, m.CreatedAt); err != nil {
		return nil, projectStoreError(err)
	}
	return connect.NewResponse(&castlev1.AddProjectMemberResponse{Member: mapMember(m)}), nil
}

func (s *ServerServer) RemoveProjectMember(ctx context.Context, req *connect.Request[castlev1.RemoveProjectMemberRequest]) (*connect.Response[castlev1.RemoveProjectMemberResponse], error) {
	if err := requireProjectCaller(ctx); err != nil {
		return nil, err
	}
	// No administrator role exists yet (user auth arrives later), so the
	// only permitted member eviction is self-leave; the deny is decided on
	// caller identity alone, without disclosing whether the target exists.
	memberID := req.Msg.GetMemberId()
	if memberID == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("member_id required"))
	}
	if memberID != auth.CallerCriteriaID(ctx) {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("members may only leave projects themselves"))
	}
	if err := s.Store.RemoveProjectMember(ctx, req.Msg.GetProjectId(), memberID); err != nil {
		return nil, projectStoreError(err)
	}
	return connect.NewResponse(&castlev1.RemoveProjectMemberResponse{}), nil
}

func (s *ServerServer) ListProjectMembers(ctx context.Context, req *connect.Request[castlev1.ListProjectMembersRequest]) (*connect.Response[castlev1.ListProjectMembersResponse], error) {
	if err := requireProjectCaller(ctx); err != nil {
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

// requireProjectCaller enforces the agent-identity-only posture for the
// project surface: console and orchestrator identities reaching a handler
// despite interceptor policy are rejected here (defense in depth). Trigger
// identities are rejected explicitly with PermissionDenied (KB-320): they
// hold exactly one project surface call — SubmitProjectAssignment, which
// resolves its own caller above — and must never administer projects or
// membership.
func requireProjectCaller(ctx context.Context) error {
	if auth.CallerTriggerID(ctx) != "" {
		return connect.NewError(connect.CodePermissionDenied, errors.New("trigger identities cannot invoke this project surface"))
	}
	if auth.CallerCriteriaID(ctx) == "" {
		return connect.NewError(connect.CodeUnauthenticated, errors.New("caller required"))
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

// projectMembershipPolicy resolves a project's policy, mapping store errors
// onto wire codes (unknown project → NotFound).
func (s *ServerServer) projectMembershipPolicy(ctx context.Context, projectID string) (string, error) {
	row, err := s.Store.GetProject(ctx, projectID)
	if err != nil {
		return "", projectStoreError(err)
	}
	return row.MembershipPolicy, nil
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
