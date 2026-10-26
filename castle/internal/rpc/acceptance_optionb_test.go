package rpc

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	castlev1 "github.com/brokenbots/castle/castle/gen/castle/v1"
	castlev1connect "github.com/brokenbots/castle/castle/gen/castle/v1/castlev1connect"
	"github.com/brokenbots/castle/castle/internal/auth"
	"github.com/brokenbots/castle/castle/internal/confighome"
	"github.com/brokenbots/castle/castle/internal/kanboard"
	"github.com/brokenbots/castle/castle/internal/kanboardtrigger"
	"github.com/brokenbots/castle/castle/internal/provision"
	"github.com/brokenbots/castle/castle/internal/store"
	critsdk "github.com/brokenbots/criteria/sdk"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
	criteriav1connect "github.com/brokenbots/criteria/sdk/pb/criteria/v1/criteriav1connect"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// The Option B acceptance suite (KB-341): the project's goal demonstrated
// live through castle + parapet's console wire with zero kubectl for
// workflow config — SEED projects with imported config records, FIRE a
// real work card through the kanboard board adapter, OBSERVE the decision
// trail, EDIT the project's ci gate config and refire, prove project
// ISOLATION against a wrong-project agent, and CLEANUP the legacy dispatch
// CM surfaces behind the cutover flag flip.
//
// The "live" element is wire-level: every step runs against real castle
// handlers (StartServer with the production auth posture — no anonymous
// register) and the real kanboardtrigger adapter against an in-memory
// Kanboard JSON-RPC endpoint. The executing agent is played by the test the
// way the deployed criteria agent is: it is driven by the same Control
// delivery and event envelopes the real agent consumes.

const (
	optionbBoardProject = "Dev Board"
	optionbFireProject  = "kanboard-tickets"
	optionbTicketID     = 101

	// optionbPlatformName is the live payload's platform record: the
	// projectRegistry entry that owns unreferenced overlay data.
	optionbPlatformName = "Criteria K8s Workflow Runner"
)

// optionbProjects maps the live payload's projectRegistry entries to the
// castle project ids the Option B program seeds (the import's --map).
var optionbProjects = map[string]string{
	optionbPlatformName: "criteria-k8s-workflow-runner",
	"Kanboard Tickets":  "kanboard-tickets",
	"Criteria Go Dev":   "criteria-go-dev",
	"TypeScript SDK":    "typescript-sdk",
}

// optionbAgent is one project's per-namespace operator identity: the agent
// registered under the project's per-project credential (KB-337), whose
// membership makes the lease scan offer it the project's runs.
type optionbAgent struct {
	CriteriaID string
	Token      string
	Control    *connect.ServerStreamForClient[pb.ControlMessage]
}

// optionbFixture is the seeded Option B world: the wire stack, the console
// operator driving parapet-level writes, the trigger identity the board
// adapter authenticates with, and each project's operator identity.
type optionbFixture struct {
	t            *testing.T
	ts           *testStack
	tsrv         *httptest.Server
	oClient      criteriav1connect.CriteriaServiceClient
	sClient      criteriav1connect.ServerServiceClient
	console      castlev1connect.ConsoleServiceClient
	projects     castlev1connect.ProjectServiceClient
	consoleToken string
	triggerToken string
	agents       map[string]optionbAgent
	board        *optionbFakeKanboard
	boardCancel  context.CancelFunc
	boardDone    chan error
}

// optionbReadLivePayload reads the confighome routes payload fixture — the
// live dispatch state the KB-339 import models.
func optionbReadLivePayload(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile("../confighome/testdata/routes.json")
	if err != nil {
		t.Fatalf("read live routes payload: %v", err)
	}
	return raw
}

// optionbImportSeeds performs the KB-339 import in-process against the
// fixture payloads: partition, binding attach, per-record validation, and
// the canonical round-trip verifications the import tool gates writes on.
// Returns the records to write (registry name order) and each namespace's
// binding source for round-trip checks.
func optionbImportSeeds(t *testing.T) ([]confighome.ProjectRecord, map[string][]byte) {
	t.Helper()
	liveRoutes := optionbReadLivePayload(t)

	bindingBytes := map[string][]byte{}
	bindings := map[string]*confighome.Binding{}
	// Binding payloads pair by namespace; the fixture files carry their
	// own names.
	pairs := []struct{ file, namespace string }{
		{"binding-go-dev.json", "criteria-go-dev"},
		{"binding-typescript-sdk.json", "typescript-sdk"},
	}
	for _, pair := range pairs {
		raw, err := os.ReadFile("../confighome/testdata/" + pair.file)
		if err != nil {
			t.Fatalf("read binding payload for %s: %v", pair.namespace, err)
		}
		parsed, err := confighome.ParseBinding(raw)
		if err != nil {
			t.Fatalf("binding payload %s: %v", pair.namespace, err)
		}
		bindingBytes[pair.namespace] = raw
		bindings[pair.namespace] = parsed
	}

	records, err := confighome.Partition(optionbPlatformName, liveRoutes)
	if err != nil {
		t.Fatalf("partition routes payload: %v", err)
	}

	names := make([]string, 0, len(records))
	for name := range records {
		names = append(names, name)
	}
	slices.Sort(names)
	projRecords := make([]confighome.ProjectRecord, 0, len(records))
	for _, name := range names {
		record := records[name]
		if binding, ok := bindings[record.Namespace]; ok {
			record.Binding = binding
		}
		if err := record.Validate(); err != nil {
			t.Fatalf("record %s: %v", name, err)
		}
		projRecords = append(projRecords, confighome.ProjectRecord{
			ProjectID: optionbProjects[name],
			Record:    record,
		})
	}

	// The verified round-trip before any write (KB-339): the records'
	// assembly must render a routes payload canonically equal to the live
	// file.
	rendered, err := confighome.NewAssemblyFor(projRecords).RenderRoutesPayload()
	if err != nil {
		t.Fatalf("render routes payload from records: %v", err)
	}
	equal, checksum, err := confighome.EqualPayload(rendered, liveRoutes)
	if err != nil {
		t.Fatalf("compare rendered routes payload: %v", err)
	}
	if !equal {
		t.Fatalf("round-trip mismatch: rendered checksum %s differs from the live payload", checksum)
	}
	for _, pr := range projRecords {
		if pr.Record.Binding == nil {
			continue
		}
		renderedBinding, err := pr.Record.RenderBinding()
		if err != nil {
			t.Fatalf("render binding for %s: %v", pr.ProjectID, err)
		}
		equal, _, err := confighome.EqualPayload(renderedBinding, bindingBytes[pr.Record.Namespace])
		if err != nil {
			t.Fatalf("compare rendered binding for %s: %v", pr.ProjectID, err)
		}
		if !equal {
			t.Fatalf("binding round-trip mismatch for %s", pr.ProjectID)
		}
	}
	return projRecords, bindingBytes
}

// optionbNewFixture boots the acceptance world: production auth posture,
// a console operator, the trigger identity, and the wire clients.
func optionbNewFixture(t *testing.T) *optionbFixture {
	t.Helper()
	ctx := context.Background()
	fx := &optionbFixture{
		t:      t,
		ts:     newTestStack(t),
		agents: map[string]optionbAgent{},
	}
	tsrv, oClient, sClient := fx.ts.startServer(t,
		connect.WithInterceptors(auth.NewInterceptor(fx.ts.store, false)),
	)
	fx.tsrv = tsrv
	fx.oClient = oClient
	fx.sClient = sClient
	fx.console = consoleClient(tsrv)
	fx.projects = projectClient(tsrv)

	// Console operator (parapet's operator identity): provisions projects
	// and writes config records through the console wire.
	fx.consoleToken = consoleOperatorSession(t, fx.ts, tsrv, "copilot", "copilot-passphrase")

	// Board trigger identity: the credential the kanboard adapter
	// authenticates with (the same provisioning shape cmd/castle boots).
	token := optionbSecretToken(t, "trigger-kanboard")
	if err := fx.ts.store.UpsertTrigger(ctx, &store.Trigger{
		ID:        "trigger-kanboard",
		TokenHash: auth.HashToken(token),
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("provision trigger: %v", err)
	}
	fx.triggerToken = token
	return fx
}

var optionbTokensUsed sync.Map

// optionbSecretToken mints a unique token value for identity provisioning.
func optionbSecretToken(t *testing.T, name string) string {
	t.Helper()
	n := 0
	if v, ok := optionbTokensUsed.Load(name); ok {
		n = v.(int) + 1
	}
	optionbTokensUsed.Store(name, n)
	return fmt.Sprintf("tok-optionb-%s-%d", name, n)
}

func (fx *optionbFixture) ctx() context.Context {
	return context.Background()
}

// seedConfigImport writes one record through the console wire with an
// expected etag, returns the etag the write produced.
func (fx *optionbFixture) seedConfigImport(projectID string, record *confighome.Record, expectedEtag string) string {
	fx.t.Helper()
	doc, err := record.Marshal()
	if err != nil {
		fx.t.Fatalf("marshal record for %s: %v", projectID, err)
	}
	resp, err := fx.projects.SetProjectConfig(fx.ctx(), bearer(&castlev1.SetProjectConfigRequest{
		ProjectId:    projectID,
		Document:     doc,
		ExpectedEtag: expectedEtag,
	}, fx.consoleToken))
	if err != nil {
		fx.t.Fatalf("console SetProjectConfig for %s: %v", projectID, err)
	}
	return resp.Msg.Etag
}

// getConfigRecord reads the project's stored record with its etag. An
// unset record returns (nil, "").
func (fx *optionbFixture) getConfigRecord(projectID string) (*confighome.Record, string) {
	fx.t.Helper()
	resp, err := fx.projects.GetProjectConfig(fx.ctx(), bearer(&castlev1.GetProjectConfigRequest{ProjectId: projectID}, fx.consoleToken))
	if err != nil {
		fx.t.Fatalf("console GetProjectConfig for %s: %v", projectID, err)
	}
	if len(resp.Msg.Document) == 0 {
		if resp.Msg.Etag != "" {
			fx.t.Fatalf("console GetProjectConfig for %s: etag %q with no document", projectID, resp.Msg.Etag)
		}
		return nil, ""
	}
	record, err := confighome.ParseRecord(resp.Msg.Document)
	if err != nil {
		fx.t.Fatalf("parse stored record for %s (len=%d, etag=%q): %v", projectID, len(resp.Msg.Document), resp.Msg.Etag, err)
	}
	return record, resp.Msg.Etag
}

// seedProject creates the project and provisions its per-project agent
// credential, then registers the project's operator identity — the member
// set the lease scan offers project work to.
func (fx *optionbFixture) seedProject(id string, policy castlev1.ProjectMembershipPolicy) optionbAgent {
	fx.t.Helper()
	ctx := fx.ctx()
	createProject(fx.t, ctx, fx.projects, fx.consoleToken, id, policy)

	agentToken := optionbSecretToken(fx.t, "agent-"+id)
	if err := provision.ProvisionProjectAgentCredential(ctx, fx.ts.store, id, agentToken, time.Now().UTC()); err != nil {
		fx.t.Fatalf("provision agent credential for %s: %v", id, err)
	}
	criteriaID, token := registerAgentCredential(fx.t, ctx, fx.oClient, id, agentToken, "", true)
	agent := optionbAgent{CriteriaID: criteriaID, Token: token}
	fx.agents[id] = agent
	return agent
}

// optionbSeededStore is the SEED result: the fixture with every record
// written and the members registered.
func optionbSeed(t *testing.T) *optionbFixture {
	t.Helper()
	fx := optionbNewFixture(t)
	projRecords, _ := optionbImportSeeds(t)

	// Every record's project must exist before its config write: the
	// console creates the projects (parapet's registry write), the import
	// writes each record under etag control.
	order := []string{"criteria-k8s-workflow-runner", "criteria-go-dev", "kanboard-tickets", "typescript-sdk"}
	for _, slug := range order {
		fx.seedProject(slug, castlev1.ProjectMembershipPolicy_PROJECT_MEMBERSHIP_POLICY_CLOSED)
	}
	for _, pr := range projRecords {
		if record, etag := fx.getConfigRecord(pr.ProjectID); record != nil || etag != "" {
			t.Fatalf("project %s already carries config (etag %q) before the import write", pr.ProjectID, etag)
		}
		fx.seedConfigImport(pr.ProjectID, pr.Record, "")
	}

	// Verify the imports through the console read surface.
	optionbVerifyImports(t, fx, projRecords)
	return fx
}

// optionbVerifyImports checks step 1's config assertions through the wire:
// every project carries its record (canonically equal to what was written),
// the cutover state matches the payload, and each bound record's binding
// renders its live payload back.
func optionbVerifyImports(t *testing.T, fx *optionbFixture, projRecords []confighome.ProjectRecord) {
	t.Helper()
	liveRoutes := optionbReadLivePayload(t)

	written := make([]confighome.ProjectRecord, 0, len(projRecords))
	for _, pr := range projRecords {
		record, etag := fx.getConfigRecord(pr.ProjectID)
		if record == nil {
			t.Fatalf("project %s: import write left the record unset", pr.ProjectID)
		}
		if etag == "" {
			t.Fatalf("project %s: import write left an empty etag", pr.ProjectID)
		}
		if record.Cutover != pr.Record.Cutover {
			t.Fatalf("project %s: cutover = %q, imported %q", pr.ProjectID, record.Cutover, pr.Record.Cutover)
		}
		if (record.Binding == nil) != (pr.Record.Binding == nil) {
			t.Fatalf("project %s: binding presence = %v, imported %v", pr.ProjectID, record.Binding != nil, pr.Record.Binding != nil)
		}
		written = append(written, confighome.ProjectRecord{ProjectID: pr.ProjectID, Record: record})
	}

	// The written records re-assemble into the live payload byte-for-byte.
	rendered, err := confighome.NewAssemblyFor(written).RenderRoutesPayload()
	if err != nil {
		t.Fatalf("re-render stored records: %v", err)
	}
	equal, checksum, err := confighome.EqualPayload(rendered, liveRoutes)
	if err != nil {
		t.Fatalf("compare stored records render: %v", err)
	}
	if !equal {
		t.Fatalf("stored records render to checksum %s, live payload differs", checksum)
	}

	// Bound namespaces still render their binding payloads through the
	// wire-read records (the namespace run-config surface is live).
	for _, pr := range written {
		if pr.Record.Binding == nil {
			continue
		}
		if _, err := pr.Record.RenderBinding(); err != nil {
			t.Fatalf("render binding for %s after the wire round-trip: %v", pr.ProjectID, err)
		}
	}
}

// seedAgentMembers verifies each project's member set carries its
// per-namespace operator identity (step 1's membership assertion).
func verifyAgentMembers(t *testing.T, fx *optionbFixture) {
	t.Helper()
	for _, slug := range []string{"criteria-k8s-workflow-runner", "criteria-go-dev", "kanboard-tickets", "typescript-sdk"} {
		agent := fx.agents[slug]
		if agent.CriteriaID == "" {
			t.Fatalf("project %s: no operator identity registered", slug)
		}
		members := listProjectMembers(t, fx.ctx(), fx.projects, fx.consoleToken, slug)
		if len(members) != 1 {
			t.Fatalf("project %s: members = %d entries, want exactly the operator", slug, len(members))
		}
		m := members[0]
		if m.MemberId != agent.CriteriaID || m.Kind != castlev1.ProjectMemberKind_PROJECT_MEMBER_KIND_AI {
			t.Fatalf("project %s: member = %s/%v, want %s/ai", slug, m.MemberId, m.Kind, agent.CriteriaID)
		}
	}
}

// optionbFetchAllRecords reads every project's stored record back through
// the wire read surface.
func optionbFetchAllRecords(t *testing.T, fx *optionbFixture) []confighome.ProjectRecord {
	t.Helper()
	slugs := make([]string, 0, len(optionbProjects))
	for _, slug := range optionbProjects {
		slugs = append(slugs, slug)
	}
	slices.Sort(slugs)
	records := make([]confighome.ProjectRecord, 0, len(slugs))
	for _, slug := range slugs {
		record, _ := fx.getConfigRecord(slug)
		records = append(records, confighome.ProjectRecord{ProjectID: slug, Record: record})
	}
	return records
}

// TestOptionBSeedImport is step 1's acceptance: projects seeded with their
// per-ns operator identities, the KB-339 config import written through the
// console wire, and the rendered payload verified canonically equal to the
// live fixture.
func TestOptionBSeedImport(t *testing.T) {
	fx := optionbSeed(t)
	verifyAgentMembers(t, fx)

	// The transition window is real: the written tree carries pre-cutover
	// namespaces, so the payload renders with deprecation warnings (the
	// allowed window) — the same render the live import reported.
	assembled := confighome.NewAssemblyFor(optionbFetchAllRecords(t, fx))
	if _, err := assembled.RenderRoutesPayload(); err != nil {
		t.Fatalf("render the written tree: %v", err)
	}
}

// optionbLaneConfig is the board adapter's lane configuration for the demo:
// one develop lane pointing at the kanboard-tickets record's
// "kanboard-develop" route, triggered by cards entering "Ready", with the
// lane's default repo binding (the card carries no repo tag — the static
// lane config is the production shape).
func optionbLaneConfig() *kanboardtrigger.Config {
	return &kanboardtrigger.Config{
		BoardProject:    optionbBoardProject,
		CastleProjectID: optionbFireProject,
		TriggerTag:      "castle-run",
		ReviewColumn:    "Review",
		DoneColumn:      "Done",
		Lanes: []kanboardtrigger.Lane{
			{
				RouteName:      "kanboard-develop",
				Class:          kanboardtrigger.ClassDevelop,
				TriggerColumns: []string{"Ready"},
				WorkflowSource: "hcl-source",
				RepoDefault:    "https://github.com/brokenbots/workflow-example",
			},
		},
	}
}

// optionbCastleClients adapts the real castle wire clients to the adapter's
// three engine interfaces, attaching the trigger identity's bearer token to
// every request — the same shim cmd/kanboard-trigger-adapter boots in
// production.
type optionbCastleClients struct {
	project castlev1connect.ProjectServiceClient
	server  criteriav1connect.ServerServiceClient
	token   string
}

func newOptionbCastleClients(tsrv *httptest.Server, token string) *optionbCastleClients {
	return &optionbCastleClients{
		project: castlev1connect.NewProjectServiceClient(http.DefaultClient, tsrv.URL),
		server:  criteriav1connect.NewServerServiceClient(http.DefaultClient, tsrv.URL),
		token:   token,
	}
}

func (c *optionbCastleClients) auth(h http.Header) { h.Set("Authorization", "Bearer "+c.token) }

func (c *optionbCastleClients) SubmitProjectAssignment(ctx context.Context, req *connect.Request[castlev1.SubmitProjectAssignmentRequest]) (*connect.Response[castlev1.SubmitProjectAssignmentResponse], error) {
	c.auth(req.Header())
	return c.project.SubmitProjectAssignment(ctx, req)
}

func (c *optionbCastleClients) GetAssignmentDisposition(ctx context.Context, req *connect.Request[pb.GetAssignmentDispositionRequest]) (*connect.Response[pb.GetAssignmentDispositionResponse], error) {
	c.auth(req.Header())
	return c.server.GetAssignmentDisposition(ctx, req)
}

func (c *optionbCastleClients) WatchRun(ctx context.Context, req *connect.Request[pb.WatchRunRequest]) (kanboardtrigger.RunEventStream, error) {
	c.auth(req.Header())
	return c.server.WatchRun(ctx, req)
}

// startBoard boots the real kanboardtrigger adapter against the fake board
// and the real castle wire. The adapter polls immediately on boot; the
// fixture records the run context for cleanup ordering (the board must be
// cancelled before the wire server closes so the mirror's open streams do
// not block shutdown).
func (fx *optionbFixture) startBoard(t *testing.T, cfg *kanboardtrigger.Config) {
	t.Helper()
	if fx.board == nil {
		fx.board = newOptionbFakeKanboard(t, optionbBoardProject, "Backlog", "Ready", "Review", "Done")
	}
	kb := kanboard.New(fx.board.srv.URL+"/jsonrpc.php", "board-app-token")
	clients := newOptionbCastleClients(fx.tsrv, fx.triggerToken)
	adapter, err := kanboardtrigger.New(cfg, kb, clients, clients, clients, discardLogger(), kanboardtrigger.Options{
		StatePath:    filepath.Join(t.TempDir(), "adapter-state.json"),
		PollInterval: 15 * time.Millisecond,
		Instance:     "optionb",
	})
	if err != nil {
		t.Fatalf("boot board adapter: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	fx.boardCancel = cancel
	fx.boardDone = make(chan error, 1)
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-fx.boardDone:
			if err != nil && ctx.Err() == nil {
				t.Fatalf("board adapter stopped with error: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("board adapter did not stop after cancel")
		}
	})
	go func() {
		err := adapter.Run(ctx)
		fx.boardDone <- err
	}()
}

// addFiredCard seeds the demo card in the lane's trigger column: the fire
// gate's entry memo treats the first sighting as entry, so the next poll
// fires it.
func (fx *optionbFixture) addFiredCard(t *testing.T, id int, title string) {
	t.Helper()
	fx.board.addCard(t, id, "Ready", title, "castle-run")
}

// optionbRunAssigned waits for the member operator's WorkflowAssignment
// control message and returns the run id it carries.
func optionbRunAssigned(t *testing.T, ctrl *connect.ServerStreamForClient[pb.ControlMessage], route string) string {
	t.Helper()
	msg := receiveControl(t, ctrl, 5*time.Second)
	wa := msg.GetWorkflowAssignment()
	if wa == nil {
		t.Fatalf("expected WorkflowAssignment, got %T", msg.Command)
	}
	if wa.GetWorkflowName() != route {
		t.Fatalf("assignment route = %q, want %q", wa.GetWorkflowName(), route)
	}
	return wa.GetRunId()
}

// optionbAwaitColumn polls the fake board until the card sits in the named
// column (the mirror's move happens on the poll after the settle).
func (fx *optionbFixture) optionbAwaitColumn(t *testing.T, taskID int, column string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if fx.board.columnOf(taskID) == column {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("card %d never reached column %q (now %q)",
				taskID, column, fx.board.columnOf(taskID))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// optionbRunEnvelopes plays the agent side of the run: RunStarted executes
// the run in its project, StepEntered records the attempt (decision-trail
// layer), RunCompleted settles it. success=false parks the mirror on the
// failure path. Returns the envelopes' acks' absence as an error.
func optionbRunEnvelopes(t *testing.T, oClient criteriav1connect.CriteriaServiceClient, token, runID, workflowName string, finalState string, success bool) {
	t.Helper()
	submitRunStarted(t, oClient, token, runID, workflowName, "s1")
	stream := oClient.SubmitEvents(context.Background())
	stream.RequestHeader().Set("Authorization", "Bearer "+token)
	send := func(env *pb.Envelope) {
		t.Helper()
		env.SchemaVersion = 1
		env.RunId = runID
		env.Ts = timestamppb.Now()
		if err := stream.Send(env); err != nil {
			t.Fatalf("send %T: %v", env.GetPayload(), err)
		}
		if _, err := stream.Receive(); err != nil {
			t.Fatalf("receive ack for %T: %v", env.GetPayload(), err)
		}
	}
	send(&pb.Envelope{CorrelationId: "step-" + runID,
		Payload: &pb.Envelope_StepEntered{StepEntered: &pb.StepEntered{Step: "s1", Adapter: "shell", Attempt: 1}}})
	send(critsdk.NewEnvelope(runID, &pb.RunCompleted{FinalState: finalState, Success: success}))
}

// TestOptionBFireRunObserve is steps 2 AND 3's acceptance: a real work card
// fires through the kanboard adapter (castle mode: submit + own disposition
// read + run watch), the project's operator identity leases and executes it
// THROUGH the control wire, the run lands in the project, the decision
// trail is readable (trigger disposition read, assignment, attempt, run
// events), and the mirror settles the card onto Done.
func TestOptionBFireRunObserve(t *testing.T) {
	fx := optionbSeed(t)
	agent := fx.agents[optionbFireProject]

	// The project's operator identity opens its Control stream first: the
	// dispatch only offers work to registered members.
	ctrl := openControl(t, fx.oClient, agent.CriteriaID, agent.Token)
	expectControlReady(t, ctrl)
	t.Cleanup(func() { _ = ctrl.Close() })

	// The board goes live with one demo card sitting in the lane's trigger
	// column.
	fx.startBoard(t, optionbLaneConfig())
	fx.addFiredCard(t, optionbTicketID, "Option B demo card")

	// FIRE: the adapter submits the assignment from the card's entry.
	runID := optionbRunAssigned(t, ctrl, "kanboard-develop")

	// The trigger reads its own assignment's disposition (the assignment's
	// owner IS the trigger: castle resolves it through the trigger identity).
	clients := newOptionbCastleClients(fx.tsrv, fx.triggerToken)
	ctx := context.Background()
	disp, err := clients.GetAssignmentDisposition(ctx, connect.NewRequest(&pb.GetAssignmentDispositionRequest{RunId: runID}))
	if err != nil {
		t.Fatalf("trigger reads assignment disposition: %v", err)
	}
	if got := disp.Msg.GetState(); got != pb.WorkflowAssignmentState_WORKFLOW_ASSIGNMENT_STATE_LEASED {
		t.Fatalf("disposition state = %v, want LEASED", got)
	}
	if got := disp.Msg.GetLeasedCriteriaId(); got != agent.CriteriaID {
		t.Fatalf("leased to %q, want the project's operator identity %q", got, agent.CriteriaID)
	}

	// The run executes in ITS namespace through the server wire: the
	// member reads the run by id (membership scoped) with the route's
	// workflow name.
	getRun, err := fx.sClient.GetRun(ctx, bearer(&pb.GetRunRequest{RunId: runID}, agent.Token))
	if err != nil {
		t.Fatalf("operator reads its run: %v", err)
	}
	if got := getRun.Msg.GetWorkflowName(); got != "kanboard-develop" {
		t.Fatalf("run workflow = %q, want kanboard-develop", got)
	}

	// The member executes the run through the event wire: RunStarted, the
	// step attempt, and completion.
	optionbRunEnvelopes(t, fx.oClient, agent.Token, runID, "kanboard-develop", "handler_complete", true)

	// OBSERVE: the run is visible in the project's run list (the console's
	// scoped surface), and the decision trail is complete: assignment,
	// attempts, run events.
	listRuns, err := fx.projects.ListProjectRuns(ctx, bearer(&castlev1.ListProjectRunsRequest{ProjectId: optionbFireProject}, fx.consoleToken))
	if err != nil {
		t.Fatalf("console lists project runs: %v", err)
	}
	var listed *pb.Run
	for _, r := range listRuns.Msg.GetRuns() {
		if r.GetRunId() == runID {
			listed = r
			break
		}
	}
	if listed == nil {
		t.Fatalf("run %s missing from the project's run list", runID)
	}

	events, err := fx.sClient.ListRunEvents(ctx, bearer(&pb.ListRunEventsRequest{RunId: runID}, agent.Token))
	if err != nil {
		t.Fatalf("listen run events: %v", err)
	}
	sawRunStarted, sawStepEntered := false, false
	for _, ev := range events.Msg.GetEvents() {
		switch p := ev.GetPayload().(type) {
		case *pb.Envelope_RunStarted:
			sawRunStarted = true
		case *pb.Envelope_StepEntered:
			if p.StepEntered.GetAttempt() != 1 {
				t.Fatalf("step attempt = %d, want 1 in the run trail", p.StepEntered.GetAttempt())
			}
			sawStepEntered = true
		}
	}
	if !sawRunStarted || !sawStepEntered {
		t.Fatalf("run trail incomplete for %s: runStarted=%v stepEntered=%v", runID, sawRunStarted, sawStepEntered)
	}

	// The trigger's disposition settles terminal once the run completed.
	deadline := time.Now().Add(5 * time.Second)
	dispState := func() pb.WorkflowAssignmentState {
		d, err := clients.GetAssignmentDisposition(ctx, connect.NewRequest(&pb.GetAssignmentDispositionRequest{RunId: runID}))
		if err != nil {
			t.Fatalf("trigger disposition after completion: %v", err)
		}
		return d.Msg.GetState()
	}
	for dispState() != pb.WorkflowAssignmentState_WORKFLOW_ASSIGNMENT_STATE_TERMINAL {
		if time.Now().After(deadline) {
			t.Fatalf("assignment never settled terminal (state %v)", dispState())
		}
		time.Sleep(5 * time.Millisecond)
	}

	// The board mirror settles the develop success: the demo card moves to
	// Done, exactly once.
	fx.optionbAwaitColumn(t, optionbTicketID, "Done")
	moves := fx.board.moveTargets()
	if len(moves) != 1 {
		t.Fatalf("mirror moved the card %d times (%v), want exactly one Done move", len(moves), moves)
	}
}
