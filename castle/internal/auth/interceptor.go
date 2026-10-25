package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"connectrpc.com/connect"

	"github.com/brokenbots/castle/castle/internal/store"
	criteria "github.com/brokenbots/criteria/sdk"
	"github.com/brokenbots/criteria/sdk/pb/criteria/v1/criteriav1connect"

	castlev1connect "github.com/brokenbots/castle/castle/gen/castle/v1/castlev1connect"
)

// callerCriteriaIDKey is the context key for the authenticated caller's criteria agent ID.
type callerCriteriaIDKey struct{}

// CallerCriteriaID returns the criteria agent ID injected by AuthInterceptor,
// or "" if the request was not authenticated (e.g. exempt procedures or
// direct handler calls in tests without the interceptor wired).
func CallerCriteriaID(ctx context.Context) string {
	v, _ := ctx.Value(callerCriteriaIDKey{}).(string)
	return v
}

// WithCallerCriteriaID returns a context with the given criteria agent ID
// injected as the authenticated caller. Use in tests that call handlers
// directly (no HTTP stack) to simulate the identity the interceptor would
// inject.
func WithCallerCriteriaID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, callerCriteriaIDKey{}, id)
}

// readOnlyServerProcedures are the read-only ServerService observation
// surfaces: the calls exempted by dev-mode anonymous reads
// (--allow-anon-reads). InspectRun is included (CRI-194): it is a read-only
// inspection of run state, and omitting it made the Parapet run detail page
// the only page whose data call rejected a token every other page accepted.
// Per-caller authorization on InspectRun (caller-owns-run) stays enforceable
// because presented tokens are validated even on this surface — see
// authenticateAnonReadToken.
var readOnlyServerProcedures = map[string]struct{}{
	criteriav1connect.ServerServiceListAgentsProcedure:    {},
	criteriav1connect.ServerServiceGetAgentProcedure:      {},
	criteriav1connect.ServerServiceListRunsProcedure:      {},
	criteriav1connect.ServerServiceGetRunProcedure:        {},
	criteriav1connect.ServerServiceListRunEventsProcedure: {},
	criteriav1connect.ServerServiceWatchRunProcedure:      {},
	criteriav1connect.ServerServiceInspectRunProcedure:    {},
}

// anonymousReadCallerKey marks the context of a tokenless caller admitted
// by the anonymous-read pass-through (KB-326 S3). The interceptor never
// decides WHICH run an anonymous caller may read — it only marks the
// credential class, so handlers own the per-run grant decision.
type anonymousReadCallerKey struct{}

// WithAnonymousReadCaller marks the context as an anonymous (tokenless)
// caller admitted through the anonymous-read pass-through.
func WithAnonymousReadCaller(ctx context.Context) context.Context {
	return context.WithValue(ctx, anonymousReadCallerKey{}, true)
}

// CallerIsAnonymousRead reports whether the context belongs to a tokenless
// caller the interceptor passed through as anonymous. The direct-call path
// (no interceptor, bare ctx) is never marked: direct callers keep the
// privileged whole-space semantics the unwired tests rely on.
func CallerIsAnonymousRead(ctx context.Context) bool {
	v, _ := ctx.Value(anonymousReadCallerKey{}).(bool)
	return v
}

// runReadServerProcedures are the per-run ServerService read surfaces whose
// anonymous admission (KB-326 S3) is decided per-run by the handler, not by
// the global flag: scoped runs (project-stamped) are never anonymously
// readable, unscoped legacy runs need the flag or an owning project's
// allow_anonymous_reads setting. They are exempt from token enforcement so
// tokenless callers reach the handler's grant check; a presented token is
// still fully validated by authenticateAnonReadToken. ListRuns is NOT here:
// its anonymous surface stays globally flag-gated at the credential gate
// (the handler only filters it to unscoped runs).
var runReadServerProcedures = map[string]struct{}{
	criteriav1connect.ServerServiceGetRunProcedure:        {},
	criteriav1connect.ServerServiceListRunEventsProcedure: {},
	criteriav1connect.ServerServiceWatchRunProcedure:      {},
	criteriav1connect.ServerServiceInspectRunProcedure:    {},
}

// orchestratorProcedures are the OrchestratorService RPCs (CRI-133). They are
// read-only observation surfaces for the operator reconcile loop.
var orchestratorProcedures = map[string]struct{}{
	criteriav1connect.OrchestratorServiceSubscribeRunEventsProcedure: {},
	criteriav1connect.OrchestratorServiceListActiveRunsProcedure:     {},
}

// orchestratorWriteProcedures are OrchestratorService RPCs that mutate run
// state on the operator's behalf (CRI-142 CancelRun). Orchestrator and agent
// identities may invoke them, but — unlike the read surfaces above — they are
// never exempted by dev-mode anonymous reads, so unauthenticated callers
// cannot mutate run state even with --allow-anon-reads.
var orchestratorWriteProcedures = map[string]struct{}{
	criteriav1connect.OrchestratorServiceCancelRunProcedure: {},
}

// runArchiveReadProcedures are the RunArchiveService read RPCs (KB-308): the
// pre-retention archive record store the operator writes before CR retention
// deletes a terminal run. Reads are observation surfaces (console identities
// and dev-mode anonymous reads reach them like the ServerService reads); the
// write (ArchiveRun) is never exempt and stays denied for console identities.
var runArchiveReadProcedures = map[string]struct{}{
	castlev1connect.RunArchiveServiceGetRunArchiveProcedure:   {},
	castlev1connect.RunArchiveServiceListRunArchivesProcedure: {},
}

// isOrchestratorAllowed reports whether an orchestrator identity may invoke
// the procedure (CRI-133). Orchestrators get the read-only ServerService
// surface, OrchestratorService (including CRI-142 CancelRun), and nothing
// else: every agent-owned write procedure (CriteriaService, ServerService
// writes) is denied.
func isOrchestratorAllowed(procedure string) bool {
	if _, ok := orchestratorProcedures[procedure]; ok {
		return true
	}
	if _, ok := orchestratorWriteProcedures[procedure]; ok {
		return true
	}
	if _, ok := readOnlyServerProcedures[procedure]; ok {
		return true
	}
	if _, ok := runArchiveReadProcedures[procedure]; ok {
		return true
	}
	return false
}

// consoleRunControlProcedures are the run-control writes a console identity
// may invoke (CRI-196): the released ServerService run-control RPCs plus the
// castle-owned ConsoleService.ResolveResume, which carries the operator's
// explicit decision payload (approval decisions / signal notes) the released
// criteria sdk deliberately does not ship on ServerService.ResumeRun (KB-102).
// A human operator manages runs from the Parapet console — Stop/Pause/
// Resume, including runs owned by other agents — without being able to act
// like a workflow: ResumeRun carries the operator-supplied signal + payload
// contract (approval decisions), CancelRun (OrchestratorService, CRI-142),
// SubmitWorkflowAssignment, SendPrompt, and every agent-owned
// CriteriaService procedure stay denied. Like every console surface this is
// deterministic map membership, not judgment.
var consoleRunControlProcedures = map[string]struct{}{
	criteriav1connect.ServerServiceStopRunProcedure:   {},
	criteriav1connect.ServerServicePauseRunProcedure:  {},
	criteriav1connect.ServerServiceResumeRunProcedure: {},

	castlev1connect.ConsoleServiceResolveResumeProcedure: {},
}

// consoleProjectAdminProcedures are the KB-324 project-administration
// surfaces a console identity reaches: the membership writes (the KB-324
// write shape moves admission and eviction to console sessions — protocol
// tokens carry assignment/run writes only), the ONE settings doc
// (Get/SetProjectSettings with its etag swap), the refire policy rows
// (Get/SetRefirePolicy over the KB-321 store methods), plus the registry
// reads an admin context needs. CreateProject is admitted for the settings
// seed (a console-session write; the handler gates the seed per caller) and
// SubmitProjectAssignment stays agent-owned.
var consoleProjectAdminProcedures = map[string]struct{}{
	castlev1connect.ProjectServiceGetProjectProcedure:          {},
	castlev1connect.ProjectServiceListProjectsProcedure:        {},
	castlev1connect.ProjectServiceListProjectMembersProcedure:  {},
	castlev1connect.ProjectServiceAddProjectMemberProcedure:    {},
	castlev1connect.ProjectServiceRemoveProjectMemberProcedure: {},
	castlev1connect.ProjectServiceGetProjectSettingsProcedure:  {},
	castlev1connect.ProjectServiceSetProjectSettingsProcedure:  {},
	castlev1connect.ProjectServiceGetRefirePolicyProcedure:     {},
	castlev1connect.ProjectServiceSetRefirePolicyProcedure:     {},
	castlev1connect.ProjectServiceCreateProjectProcedure:       {},
}

// consoleSessionProcedures are the KB-325 session-ledger surfaces: a console
// session may log itself out and list its user's session ledger in-handler.
// Both are self-scoped — the handler denies protocol-token callers and
// filters the ledger to the calling console user — so the credential class,
// not the interceptor map, stays the authority on who can serve them.
var consoleSessionProcedures = map[string]struct{}{
	castlev1connect.ConsoleServiceLogoutProcedure:       {},
	castlev1connect.ConsoleServiceListSessionsProcedure: {},
}

// isConsoleAllowed reports whether a console identity may invoke the
// procedure (CRI-195, extended by CRI-196 and KB-324). Console identities
// are human operators viewing the Parapet console: they get the read-only
// ServerService observation surface across ALL runs and agents (a human
// viewing the console is not subject to caller-owns-run — that boundary is
// for agent identities), the run-control writes a human operator needs
// (CRI-196), and the KB-324 project-administration surfaces (membership,
// settings, refire policies — the console-session write class). Everything
// else — CriteriaService, ServerService SubmitWorkflowAssignment/
// SendPrompt, OrchestratorService writes (CancelRun), Register — is denied.
func isConsoleAllowed(procedure string) bool {
	if _, ok := readOnlyServerProcedures[procedure]; ok {
		return true
	}
	if _, ok := runArchiveReadProcedures[procedure]; ok {
		return true
	}
	if _, ok := consoleRunControlProcedures[procedure]; ok {
		return true
	}
	if _, ok := consoleSessionProcedures[procedure]; ok {
		return true
	}
	_, ok := consoleProjectAdminProcedures[procedure]
	return ok
}

// authorizeConsoleProcedure enforces the console auth boundary (CRI-195,
// CRI-196): a console identity may invoke only the read-only ServerService
// observation procedures and the run-control writes (Stop/Pause/Resume).
// Remaining agent-owned writes must be rejected so a console login can never
// submit or drive workflows like an agent. Agent and orchestrator callers
// pass through (CallerConsoleUserID is empty for them).
func authorizeConsoleProcedure(ctx context.Context, procedure string) error {
	if CallerConsoleUserID(ctx) == "" {
		return nil
	}
	if isConsoleAllowed(procedure) {
		return nil
	}
	return connect.NewError(connect.CodePermissionDenied, errors.New("console identities cannot invoke this procedure"))
}

// triggerProcedures are the submit-only board-adapter surface (KB-320). A
// trigger identity submits board-tagged work into the assignment queue and
// observes the resulting runs to mirror board columns; everything else —
// every mutating agent procedure, project administration, run control,
// credential registration — is denied. GetAssignmentDisposition is read-only
// and ownership-scoped in the handler (assignment owner match), the pair of
// submit RPCs is where board events turn into runs, and WatchRun drives the
// column mirror exactly like the orchestrator's observation loop. The
// adapter consumes run events only through WatchRun, so ListRunEvents stays
// off the surface (even though that procedure is anon-readable anyway).
var triggerProcedures = map[string]struct{}{
	criteriav1connect.ServerServiceSubmitWorkflowAssignmentProcedure: {},
	criteriav1connect.ServerServiceGetAssignmentDispositionProcedure: {},
	criteriav1connect.ServerServiceWatchRunProcedure:                 {},
	castlev1connect.ProjectServiceSubmitProjectAssignmentProcedure:   {},
}

// isTriggerAllowed reports whether a trigger identity may invoke the
// procedure (KB-320): map-deterministic membership in the submit-only
// allowlist above.
func isTriggerAllowed(procedure string) bool {
	_, ok := triggerProcedures[procedure]
	return ok
}

// authorizeTriggerProcedure enforces the trigger auth boundary (KB-320). A
// trigger is a least-privilege board adapter: it may only submit
// assignments and observe the runs they produce. Agent, orchestrator, and
// console callers pass through (CallerTriggerID is empty for them).
func authorizeTriggerProcedure(ctx context.Context, procedure string) error {
	if CallerTriggerID(ctx) == "" {
		return nil
	}
	if isTriggerAllowed(procedure) {
		return nil
	}
	return connect.NewError(connect.CodePermissionDenied, errors.New("trigger identities cannot invoke this procedure"))
}

// InterceptorOption configures an AuthInterceptor.
type InterceptorOption func(*AuthInterceptor)

// WithBootstrapToken configures the raw bootstrap token required for Register.
// The interceptor hashes it and validates incoming X-Server-Bootstrap headers.
// If not set, Register is disabled (returns Unimplemented).
func WithBootstrapToken(token string) InterceptorOption {
	return func(i *AuthInterceptor) {
		if token != "" {
			i.bootstrapTokenHash = HashToken(token)
		}
	}
}

// WithAnonRegister allows Register without a bootstrap token. Use only in dev
// mode (--dev-allow-anon-register flag) or in tests.
func WithAnonRegister() InterceptorOption {
	return func(i *AuthInterceptor) {
		i.allowAnonRegister = true
	}
}

// AuthInterceptor authenticates Connect calls using criteria agent tokens.
type AuthInterceptor struct {
	store              store.Store
	allowAnonReads     bool
	bootstrapTokenHash string // SHA-256 hex of the bootstrap token; empty = Register disabled
	allowAnonRegister  bool   // dev mode: bypass bootstrap check on Register
}

// NewInterceptor creates an AuthInterceptor. Options are applied in order and
// may override defaults. This function is variadic for backward compatibility;
// existing callers that pass only (store, allowAnonReads) continue to work.
func NewInterceptor(st store.Store, allowAnonReads bool, opts ...InterceptorOption) *AuthInterceptor {
	i := &AuthInterceptor{store: st, allowAnonReads: allowAnonReads}
	for _, opt := range opts {
		opt(i)
	}
	return i
}

// consoleLoginProcedure is the castle-owned human console login RPC
// (CRI-195). It lives under the castle.v1 ConsoleService — upstream criteria
// deliberately does not ship a human login, so Castle owns the surface after
// re-pinning the released criteria/sdk (KB-102).
const consoleLoginProcedure = "/castle.v1.ConsoleService/Login"

func (i *AuthInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		// Login is the credential bootstrap: it must reach its handler
		// unauthenticated so the handler can own the feature gate (console
		// login disabled → Unimplemented) and verify credentials (CRI-195).
		if req.Spec().Procedure == consoleLoginProcedure {
			return next(ctx, req)
		}
		if req.Spec().Procedure == criteria.RegisterProcedure {
			return i.handleRegister(ctx, req, next)
		}
		if i.isExempt(req.Spec().Procedure) {
			newCtx, err := i.authenticateAnonReadToken(ctx, req.Header(), req.Spec().Procedure)
			if err != nil {
				return nil, err
			}
			return next(newCtx, req)
		}
		newCtx, err := i.authenticateHeaders(ctx, req.Header())
		if err != nil {
			return nil, err
		}
		if err := authorizeOperatorProcedure(newCtx, req.Spec().Procedure); err != nil {
			return nil, err
		}
		return next(newCtx, req)
	}
}

func (i *AuthInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (i *AuthInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		if i.isExempt(conn.Spec().Procedure) {
			newCtx, err := i.authenticateAnonReadToken(ctx, conn.RequestHeader(), conn.Spec().Procedure)
			if err != nil {
				return err
			}
			return next(newCtx, conn)
		}
		newCtx, err := i.authenticateHeaders(ctx, conn.RequestHeader())
		if err != nil {
			return err
		}
		if err := authorizeOperatorProcedure(newCtx, conn.Spec().Procedure); err != nil {
			return err
		}
		return next(newCtx, conn)
	}
}

// authorizeOrchestratorProcedure enforces the orchestrator auth boundary
// (CRI-133): an orchestrator identity may invoke only the read-only
// observation procedures. Agent-owned writes must be rejected so an operator
// token can never be used to mutate agent state. Agent callers pass through
// (CallerOrchestratorID is empty for them).
func authorizeOrchestratorProcedure(ctx context.Context, procedure string) error {
	if CallerOrchestratorID(ctx) == "" {
		return nil
	}
	if isOrchestratorAllowed(procedure) {
		return nil
	}
	return connect.NewError(connect.CodePermissionDenied, errors.New("orchestrator identities cannot invoke agent-owned procedures"))
}

// authorizeOperatorProcedure enforces the KB-330 attribute-driven boundary:
// a resolved operator's privileges derive from its role attribute, not from
// the credential class the identity arrived through. role=admin reaches the
// full console surface (read-everything, run control, project
// administration) because a human operator and an admin operator token are
// the same privilege class; role=orchestrator reaches the read-only
// orchestrator observation surface; role=trigger reaches the submit-only
// trigger surface; role=agent (the least-privilege default) reaches the
// agent-owned writes and the read-only observation surface — the pre-merge
// agent path injects no boundary at all.
//
// Callers without an operator identity (pre-merge direct-call tests
// injecting only the legacy kind keys, and anonymous contexts from the
// tokenless exempt pass-through) fall back to the legacy class maps, so
// unwired handlers keep their exact pre-operators behavior.
func authorizeOperatorProcedure(ctx context.Context, procedure string) error {
	if CallerOperator(ctx) == nil {
		if err := authorizeOrchestratorProcedure(ctx, procedure); err != nil {
			return err
		}
		if err := authorizeConsoleProcedure(ctx, procedure); err != nil {
			return err
		}
		if err := authorizeTriggerProcedure(ctx, procedure); err != nil {
			return err
		}
		return nil
	}
	switch CallerOperatorRole(ctx) {
	case store.OperatorRoleAdmin:
		if isConsoleAllowed(procedure) {
			return nil
		}
		return connect.NewError(connect.CodePermissionDenied, errors.New("console identities cannot invoke this procedure"))
	case store.OperatorRoleOrchestrator:
		if isOrchestratorAllowed(procedure) {
			return nil
		}
		return connect.NewError(connect.CodePermissionDenied, errors.New("orchestrator identities cannot invoke agent-owned procedures"))
	case store.OperatorRoleTrigger:
		if isTriggerAllowed(procedure) {
			return nil
		}
		return connect.NewError(connect.CodePermissionDenied, errors.New("trigger identities cannot invoke this procedure"))
	default:
		return nil
	}
}

// authenticateAnonReadToken handles the exempt-procedure credential class
// (CRI-194, restructured by KB-326 S3). It has exactly three branches:
//
//   - Probe procedures (health/reflection): pass through untouched, no
//     credential validation — infrastructure surfaces, never anon-readable.
//   - Tokenless callers: pass through MARKED anonymous
//     (WithAnonymousReadCaller). The handler owns the per-run grant on the
//     run-read surface; on the flag-gated surfaces the pass-through can only
//     happen with the dev flag on, because isExempt gates them.
//   - A presented token must be valid: its identity is injected so
//     per-caller authorization stays enforceable (InspectRun requires run
//     ownership), and an invalid token is rejected as unauthenticated
//     instead of silently succeeding. That last property is what lets the
//     Parapet login gate's token probe (a ListAgents call carrying the
//     candidate token) actually detect a bad token rather than accept
//     anything and trap the UI in a login loop on a deep-linked route.
//
// The surface includes OrchestratorService subscription APIs and WatchRun,
// which a console identity must never reach (CRI-195) and which a trigger
// identity may observe only per its submit-only boundary (KB-320), so every
// identity boundary is re-checked after identity injection; a rejected
// identity fails the call like the non-exempt path would.
func (i *AuthInterceptor) authenticateAnonReadToken(ctx context.Context, h http.Header, procedure string) (context.Context, error) {
	if isProbeProcedure(procedure) {
		// Health and reflection exemption probes are not anon-readable
		// surfaces and stay fully unauthenticated: a stray header must not
		// turn a health check into a failed one.
		return ctx, nil
	}
	if _, ok := TokenFromHeaders(h); !ok {
		// KB-326 S3: a tokenless caller on an exempt procedure is not denied
		// here — for the run-read surface the handler owns the per-run grant
		// (scoped runs are never anonymous-readable; unscoped runs need the
		// dev flag or an owning project's grant), and for the flag-gated
		// surfaces the caller only ever reaches this branch when the flag is
		// on. Every pass-through is marked so handlers can distinguish
		// tokenless callers from the unmarked direct-call path.
		return WithAnonymousReadCaller(ctx), nil
	}
	// A presented token must be valid: its identity is injected so
	// per-caller authorization stays enforceable (InspectRun requires run
	// ownership), and an invalid token is rejected as unauthenticated
	// instead of silently succeeding. That last property is what lets the
	// Parapet login gate's token probe (a ListAgents call carrying the
	// candidate token) actually detect a bad token rather than accept
	// anything and trap the UI in a login loop on a deep-linked route.
	newCtx, err := i.authenticateHeaders(ctx, h)
	if err != nil {
		return nil, err
	}
	if err := authorizeOperatorProcedure(newCtx, procedure); err != nil {
		return nil, err
	}
	return newCtx, nil
}

// authenticateHeaders validates the token and returns a context with the
// caller's identity injected as an OPERATOR (KB-330): the operator_tokens
// space (admin-issued operator tokens) resolves first, then the legacy
// per-class spaces — agent, orchestrator (CRI-133), trigger (KB-320),
// console session (CRI-195) — keep resolving and map onto the SAME operator
// model through their alias operator rows (or a synthesized class-role
// identity when the alias row has not landed). Every resolution injects
// CallerOperator so privilege checks are attribute-driven; agent,
// orchestrator, and trigger roles additionally carry their legacy kind key
// so ownership checks stay unchanged. Token precedence still matters only
// for identical material across spaces: the per-table UNIQUE indexes cannot
// detect that collision, and the operator token (or, below it, the agent
// token) registered earlier shadows the identity sharing the material.
func (i *AuthInterceptor) authenticateHeaders(ctx context.Context, h http.Header) (context.Context, error) {
	tok, ok := TokenFromHeaders(h)
	if !ok {
		return ctx, connect.NewError(connect.CodeUnauthenticated, errors.New("missing token"))
	}
	otok, err := ResolveOperatorToken(ctx, i.store, tok)
	if err != nil {
		return ctx, connect.NewError(connect.CodeInternal, err)
	}
	if otok != nil {
		op, err := operatorForIdentity(ctx, i.store, otok.OperatorID, otok.OperatorID, "")
		if err != nil {
			return ctx, connect.NewError(connect.CodeInternal, err)
		}
		return i.operatorContext(ctx, op), nil
	}
	o, err := ResolveToken(ctx, i.store, tok)
	if err != nil {
		return ctx, connect.NewError(connect.CodeInternal, err)
	}
	if o != nil {
		op, err := operatorForIdentity(ctx, i.store, o.ID, o.Name, store.OperatorRoleAgent)
		if err != nil {
			return ctx, connect.NewError(connect.CodeInternal, err)
		}
		return i.operatorContext(ctx, op), nil
	}
	orch, err := ResolveOrchestratorToken(ctx, i.store, tok)
	if err != nil {
		return ctx, connect.NewError(connect.CodeInternal, err)
	}
	if orch != nil {
		op, err := operatorForIdentity(ctx, i.store, orch.ID, orch.ID, store.OperatorRoleOrchestrator)
		if err != nil {
			return ctx, connect.NewError(connect.CodeInternal, err)
		}
		return i.operatorContext(ctx, op), nil
	}
	trigger, err := ResolveTriggerToken(ctx, i.store, tok)
	if err != nil {
		return ctx, connect.NewError(connect.CodeInternal, err)
	}
	if trigger != nil {
		op, err := operatorForIdentity(ctx, i.store, trigger.ID, trigger.ID, store.OperatorRoleTrigger)
		if err != nil {
			return ctx, connect.NewError(connect.CodeInternal, err)
		}
		return i.operatorContext(ctx, op), nil
	}
	sess, err := ResolveConsoleSession(ctx, i.store, tok)
	if err != nil {
		return ctx, connect.NewError(connect.CodeInternal, err)
	}
	if sess == nil {
		return ctx, i.unauthenticatedTokenError(ctx, tok)
	}
	op, err := operatorForIdentity(ctx, i.store, sess.OperatorID, sess.OperatorID, store.OperatorRoleAdmin)
	if err != nil {
		return ctx, connect.NewError(connect.CodeInternal, err)
	}
	if op.Role() == store.OperatorRoleAgent {
		// A console session can only ever key an admin-class operator: a
		// token-only agent never has a password row, so a session pointing
		// at one is seeded data corruption, not an identity.
		return ctx, connect.NewError(connect.CodeInternal, errors.New("console session keys a non-admin operator"))
	}
	ctx = i.operatorContext(ctx, op)
	ctx = context.WithValue(ctx, callerConsoleUserIDKey{}, sess.OperatorID)
	return context.WithValue(ctx, callerConsoleSessionIDKey{}, sess.ID), nil
}

// operatorContext injects the operator identity plus the legacy kind key its
// role implies (KB-330): ownership checks keep keying on the kind keys,
// privilege checks read the attribute-driven role. Admin operators carry no
// kind key — exactly the console-session shape — so an admin operator token
// sees everything without holding agent/orchestrator/trigger ownership.
func (i *AuthInterceptor) operatorContext(ctx context.Context, op *store.Operator) context.Context {
	ctx = WithCallerOperator(ctx, op)
	switch op.Role() {
	case store.OperatorRoleAgent:
		return context.WithValue(ctx, callerCriteriaIDKey{}, op.ID)
	case store.OperatorRoleOrchestrator:
		return context.WithValue(ctx, callerOrchestratorIDKey{}, op.ID)
	case store.OperatorRoleTrigger:
		return context.WithValue(ctx, callerTriggerIDKey{}, op.ID)
	}
	return ctx
}

// overseerTokenPresenceChecker is implemented (optionally) by stores that can
// report overseer token presence from the authoritative writer view.
type overseerTokenPresenceChecker interface {
	OverseerTokenHashPresent(ctx context.Context, tokenHash string) (bool, error)
}

// unauthenticatedTokenError builds the rejection for a presented token that
// resolved to nothing across all token spaces (KB-223): a token the store's
// writer view has registered is surfaced as "token not yet visible" (retryable,
// Unavailable) instead of the misleading generic "invalid token", while a
// token that exists nowhere stays a plain invalid token. The presence probe
// only fires while the store itself suspects reader staleness, so normal
// failed-token traffic never touches the write path.
func (i *AuthInterceptor) unauthenticatedTokenError(ctx context.Context, tok string) error {
	checker, ok := i.store.(overseerTokenPresenceChecker)
	if ok {
		present, err := checker.OverseerTokenHashPresent(ctx, HashToken(tok))
		switch {
		case err != nil:
			return connect.NewError(connect.CodeInternal, err)
		case present:
			return connect.NewError(connect.CodeUnavailable,
				errors.New("token not yet visible: identity is registered in the store write view but token resolution did not observe it yet; retry shortly"))
		}
	}
	return connect.NewError(connect.CodeUnauthenticated, errors.New("invalid token"))
}

// handleRegister enforces the bootstrap-token gate for the Register RPC, and
// denies operator identities whose role attribute forbids minting agent
// credentials (CRI-195 via KB-330): the admin role — which console sessions
// resolve to — and the trigger role are both denied outright, even under
// --dev-allow-anon-register. Legacy console-session and trigger tokens keep
// their denials through the same attribute check plus the legacy resolvers.
//
//   - Operator session/token presented with admin or trigger role:
//     PermissionDenied, checked first so the denial is deterministic
//     regardless of the bootstrap configuration.
//   - allowAnonRegister=true (--dev-allow-anon-register): pass through.
//   - bootstrapTokenHash set: require a matching X-Server-Bootstrap header.
//   - Neither: Register is disabled; Unimplemented is returned.
func (i *AuthInterceptor) handleRegister(ctx context.Context, req connect.AnyRequest, next connect.UnaryFunc) (connect.AnyResponse, error) {
	if tok, present := TokenFromHeaders(req.Header()); present {
		// Operator tokens are denied by role attribute (KB-330): an
		// admin-role operator is the console privilege class and can never
		// bootstrap agent credentials, a trigger-role operator is
		// least-privilege board-adapter.
		if otok, err := ResolveOperatorToken(ctx, i.store, tok); err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		} else if otok != nil {
			op, err := operatorForIdentity(ctx, i.store, otok.OperatorID, otok.OperatorID, "")
			if err != nil {
				return nil, connect.NewError(connect.CodeInternal, err)
			}
			switch op.Role() {
			case store.OperatorRoleAdmin:
				return nil, connect.NewError(connect.CodePermissionDenied, errors.New("console identities cannot register agents"))
			case store.OperatorRoleTrigger:
				return nil, connect.NewError(connect.CodePermissionDenied, errors.New("trigger identities cannot register agents"))
			}
		}
		sess, err := ResolveConsoleSession(ctx, i.store, tok)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		if sess != nil {
			return nil, connect.NewError(connect.CodePermissionDenied, errors.New("console identities cannot register agents"))
		}
		if trigger, err := ResolveTriggerToken(ctx, i.store, tok); err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		} else if trigger != nil {
			return nil, connect.NewError(connect.CodePermissionDenied, errors.New("trigger identities cannot register agents"))
		}
	}
	if i.allowAnonRegister {
		return i.nextRegister(ctx, req, next)
	}
	if i.bootstrapTokenHash == "" {
		return nil, connect.NewError(connect.CodeUnimplemented, errors.New("register is disabled: no bootstrap token configured"))
	}
	tok := strings.TrimSpace(req.Header().Get("X-Server-Bootstrap"))
	if tok == "" {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("X-Server-Bootstrap header required for Register"))
	}
	if !ConstantTimeEqual(tok, i.bootstrapTokenHash) {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("invalid bootstrap token"))
	}
	return i.nextRegister(ctx, req, next)
}

// nextRegister carries the KB-318 registration metadata (agent identity
// block, self-join project id) from Register headers into the handler
// context, after the bootstrap gate has admitted the call. Validation
// failures surface as InvalidArgument so a misconfigured agent fails fast at
// registration instead of persisting a truncated identity block.
func (i *AuthInterceptor) nextRegister(ctx context.Context, req connect.AnyRequest, next connect.UnaryFunc) (connect.AnyResponse, error) {
	identity, projectID, err := ParseAgentRegistration(req.Header())
	if err != nil {
		return nil, err
	}
	if identity != nil {
		ctx = WithAgentIdentity(ctx, identity)
	}
	if projectID != "" {
		ctx = WithRegisterProjectID(ctx, projectID)
	}
	return next(ctx, req)
}

func (i *AuthInterceptor) isExempt(procedure string) bool {
	// Run reads (KB-326 S3) are exempt regardless of the flag: the
	// interceptor no longer decides anonymous admission for them — it marks
	// the credential class and the handler enforces the per-run grant
	// (scoped runs never anonymous-readable; unscoped runs behind the dev
	// flag or an owning project's allow_anonymous_reads grant). Presented
	// tokens are still fully validated by authenticateAnonReadToken.
	if _, ok := runReadServerProcedures[procedure]; ok {
		return true
	}
	if i.allowAnonReads {
		// Read-only observation surfaces (ServerService reads and the
		// OrchestratorService subscription APIs, CRI-133) plus the KB-308
		// archive reads stay anonymously readable in dev mode so local
		// tooling keeps working. Production TLS deployments do not enable
		// this flag. Anonymous ListRuns additionally stays flag-gated: it
		// never receives a tokenless caller when the flag is off.
		if _, ok := readOnlyServerProcedures[procedure]; ok {
			return true
		}
		if _, ok := orchestratorProcedures[procedure]; ok {
			return true
		}
		if _, ok := runArchiveReadProcedures[procedure]; ok {
			return true
		}
	}
	return isProbeProcedure(procedure)
}

// isProbeProcedure identifies the health/reflection exemption probes: they
// are infrastructure surfaces, not anon-readable RPCs, and stay fully
// unauthenticated even when a token header is presented.
func isProbeProcedure(procedure string) bool {
	if strings.HasPrefix(procedure, "/grpc.health.v1.Health/") {
		return true
	}
	if strings.HasPrefix(procedure, "/grpc.reflection.v1.ServerReflection/") {
		return true
	}
	return strings.HasPrefix(procedure, "/grpc.reflection.v1alpha.ServerReflection/")
}
