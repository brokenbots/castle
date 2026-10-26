package rpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
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
	t              *testing.T
	ts             *testStack
	tsrv           *httptest.Server
	oClient        criteriav1connect.CriteriaServiceClient
	sClient        criteriav1connect.ServerServiceClient
	console        castlev1connect.ConsoleServiceClient
	projects       castlev1connect.ProjectServiceClient
	consoleToken   string
	triggerToken   string
	agents         map[string]optionbAgent
	board          *optionbFakeKanboard
	boardStatePath string
	boardStop      func()
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

var (
	optionbTokensMu   sync.Mutex
	optionbTokensUsed = map[string]int{}
)

// optionbSecretToken mints a unique token value for identity provisioning.
func optionbSecretToken(t *testing.T, name string) string {
	t.Helper()
	optionbTokensMu.Lock()
	defer optionbTokensMu.Unlock()
	optionbTokensUsed[name]++
	return fmt.Sprintf("tok-optionb-%s-%d", name, optionbTokensUsed[name])
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
	// namespaces, so the assembly reports the KB-313 deprecation warnings
	// (the allowed window) — the two un-cut projects ("Kanboard Tickets"
	// and "Criteria K8s Workflow Runner") still carry workflows with the
	// global run-config surfaces the per-project bindings replace.
	assembled := confighome.NewAssemblyFor(optionbFetchAllRecords(t, fx))
	if warnings := assembled.DeprecationWarnings(); len(warnings) == 0 {
		t.Fatalf("expected deprecation warnings for the pre-cutover registry, got none")
	}
	if _, err := assembled.RenderRoutesPayload(); err != nil {
		t.Fatalf("render the written tree: %v", err)
	}
}

// optionbLaneConfig is the board adapter's lane configuration for the demo:
// one develop lane pointing at the given route of the kanboard-tickets
// record (the first fire uses the imported "kanboard-develop" route; the
// EDIT-FLOW refire re-boots the adapter pointed at the renamed
// "kanboard-develop-edited" route), triggered by cards entering "Ready",
// with the lane's default repo binding (the card carries no repo tag — the
// static lane config is the production shape).
func optionbLaneConfig(routeName string) *kanboardtrigger.Config {
	return &kanboardtrigger.Config{
		BoardProject:    optionbBoardProject,
		CastleProjectID: optionbFireProject,
		TriggerTag:      "castle-run",
		ReviewColumn:    "Review",
		DoneColumn:      "Done",
		Lanes: []kanboardtrigger.Lane{
			{
				RouteName:      routeName,
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

// SetRefirePolicy is the console-side policy write (parapet's operator
// identity owns refire settings): bound with the fixture's console token
// it reaches the server surface's project-admin check.
func (c *optionbCastleClients) SetRefirePolicy(ctx context.Context, req *connect.Request[castlev1.SetRefirePolicyRequest]) (*connect.Response[castlev1.SetRefirePolicyResponse], error) {
	c.auth(req.Header())
	return c.project.SetRefirePolicy(ctx, req)
}

// startBoard boots a kanboardtrigger adapter instance against the fake
// board and the real castle wire, and returns the function that stops it:
// cancel, then wait for adapter.Run to return (context.Canceled is
// success; any other stop error fails the test). It is re-entrant — each
// call owns its cancel func and done channel in local variables and its
// own idempotent cleanup — so a test may stop one instance and boot a
// second (the EDIT-FLOW re-points the lane at the edited route); the fake
// board fx.board is shared across instances, holding the cards and the
// moves log the tests assert on, and so is the adapter's state file: a
// second boot is a RESTART and must resume the first instance's memo and
// fire sequence — a fresh state would re-derive fire key :1 and castle's
// idempotency replay the first fire's (terminal) run instead of firing a
// new one. Cleanup order per instance: the board adapter is cancelled
// before the wire server closes, so the mirror's open streams do not
// block shutdown.
func (fx *optionbFixture) startBoard(t *testing.T, cfg *kanboardtrigger.Config) func() {
	t.Helper()
	if fx.board == nil {
		fx.board = newOptionbFakeKanboard(t, optionbBoardProject, "Backlog", "Ready", "Review", "Done")
	}
	if fx.boardStatePath == "" {
		fx.boardStatePath = filepath.Join(t.TempDir(), "adapter-state.json")
	}
	kb := kanboard.New(fx.board.srv.URL+"/jsonrpc.php", "board-app-token")
	clients := newOptionbCastleClients(fx.tsrv, fx.triggerToken)
	adapter, err := kanboardtrigger.New(cfg, kb, clients, clients, clients, discardLogger(), kanboardtrigger.Options{
		StatePath:    fx.boardStatePath,
		PollInterval: 15 * time.Millisecond,
		Instance:     "optionb",
	})
	if err != nil {
		t.Fatalf("boot board adapter: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- adapter.Run(ctx)
	}()
	var once sync.Once
	// stop is idempotent and safe to call repeatedly: cancel signals the
	// adapter, and the wait runs once — later calls (an explicit stop plus
	// the registered cleanup) return immediately instead of racing for the
	// single buffered done value.
	stop := func() {
		cancel()
		once.Do(func() {
			select {
			case err := <-done:
				if err != nil && !errors.Is(err, context.Canceled) {
					t.Fatalf("board adapter stopped with error: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("board adapter did not stop after cancel")
			}
		})
	}
	t.Cleanup(stop)
	fx.boardStop = stop
	return stop
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

// optionbFireReady opens the demo member's control stream, brings the
// board live, seeds the demo card in the lane's trigger column, and waits
// for the fire: the member receives the assignment on its control stream.
// Returns the member identity, the (caller-owned) control stream, and the
// fired assignment's run id.
func optionbFireReady(t *testing.T, fx *optionbFixture) (optionbAgent, *connect.ServerStreamForClient[pb.ControlMessage], string) {
	t.Helper()
	agent := fx.agents[optionbFireProject]

	// The project's operator identity opens its Control stream first: the
	// dispatch only offers work to registered members.
	ctrl := openControl(t, fx.oClient, agent.CriteriaID, agent.Token)
	expectControlReady(t, ctrl)
	t.Cleanup(func() { _ = ctrl.Close() })

	// The board goes live with one demo card sitting in the lane's trigger
	// column.
	fx.startBoard(t, optionbLaneConfig("kanboard-develop"))
	fx.addFiredCard(t, optionbTicketID, "Option B demo card")

	// FIRE: the adapter submits the assignment from the card's entry.
	runID := optionbRunAssigned(t, ctrl, "kanboard-develop")
	return agent, ctrl, runID
}

// TestOptionBFireRunObserve is steps 2 AND 3's acceptance: a real work card
// fires through the kanboard adapter (castle mode: submit + own disposition
// read + run watch), the project's operator identity leases and executes it
// THROUGH the control wire, the run lands in the project, the decision
// trail is readable (trigger disposition read, assignment, attempt, run
// events), and the mirror settles the card onto Done.
func TestOptionBFireRunObserve(t *testing.T) {
	fx := optionbSeed(t)
	agent, _, runID := optionbFireReady(t, fx)

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

// optionbFailedRunEnvelopes plays a failing run through the event wire:
// RunStarted, the step attempt, and RunFailed carrying the ending reason
// the refire policy classifies.
func optionbFailedRunEnvelopes(t *testing.T, oClient criteriav1connect.CriteriaServiceClient, token, runID, workflowName, reason string) {
	t.Helper()
	submitRunStarted(t, oClient, token, runID, workflowName, "s1")
	stream := oClient.SubmitEvents(context.Background())
	stream.RequestHeader().Set("Authorization", "Bearer "+token)
	for _, env := range []*pb.Envelope{
		{CorrelationId: "step-" + runID,
			Payload: &pb.Envelope_StepEntered{StepEntered: &pb.StepEntered{Step: "s1", Adapter: "shell", Attempt: 1}}},
		critsdk.NewEnvelope(runID, &pb.RunFailed{Reason: reason, Step: "s1"}),
	} {
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
}

// TestOptionBFireFailureRefirePolicy continues the OBSERVE step on the
// failure branch: the run fails, the mirror parks the card on Review with
// the single failure comment, and the decision trail's policy-evaluate
// leg refires the failed run's lineage — the console writes the project's
// refire policy, the pass mints a successor run for the SAME ticket's
// family (new run id, new attempt), the successor reaches the member
// through the same control wire and executes, and the lineage then closes
// (the refired original ages out of the candidate scan).
func TestOptionBFireFailureRefirePolicy(t *testing.T) {
	fx := optionbSeed(t)
	agent, ctrl, runID := optionbFireReady(t, fx)
	ctx := context.Background()

	// The run fails mid-flight.
	optionbFailedRunEnvelopes(t, fx.oClient, agent.Token, runID, "kanboard-develop", "boom: ci gate flaked")

	// The assignment settles failed, and the mirror parks the card on
	// Review with exactly the one failure comment.
	clients := newOptionbCastleClients(fx.tsrv, fx.triggerToken)
	deadline := time.Now().Add(5 * time.Second)
	dispState := func() pb.WorkflowAssignmentState {
		d, err := clients.GetAssignmentDisposition(ctx, connect.NewRequest(&pb.GetAssignmentDispositionRequest{RunId: runID}))
		if err != nil {
			t.Fatalf("trigger disposition after failure: %v", err)
		}
		return d.Msg.GetState()
	}
	for dispState() != pb.WorkflowAssignmentState_WORKFLOW_ASSIGNMENT_STATE_TERMINAL {
		if time.Now().After(deadline) {
			t.Fatalf("failed assignment never settled terminal (state %v)", dispState())
		}
		time.Sleep(5 * time.Millisecond)
	}
	fx.optionbAwaitColumn(t, optionbTicketID, "Review")
	if got := len(fx.board.moveTargets()); got != 1 {
		t.Fatalf("mirror moved the failed card %d times (%v), want the single park", got, fx.board.moveTargets())
	}
	comments := fx.board.commentsOn(optionbTicketID)
	if len(comments) != 1 {
		t.Fatalf("failed card carries %d comments (%v), want the single failure comment", len(comments), comments)
	}
	if !strings.Contains(comments[0], runID) || !strings.Contains(comments[0], "boom") {
		t.Fatalf("failure comment %q does not record the run id and reason", comments[0])
	}

	// POLICY EVALUATE: the console (parapet's operator identity) writes the
	// refire policy for the project's route — a transient vocabulary entry
	// matching the failure reason — and the pass refires the failed run.
	policyClients := newOptionbCastleClients(fx.tsrv, fx.consoleToken)
	if _, err := policyClients.SetRefirePolicy(ctx, connect.NewRequest(&castlev1.SetRefirePolicyRequest{
		ProjectId: optionbFireProject,
		Route:     "kanboard-develop",
		Policy: &castlev1.RefirePolicy{
			MaxAttempts: 2, // the budget counts the original attempt
			Transient:   []string{"boom"},
		},
	})); err != nil {
		t.Fatalf("console writes the refire policy: %v", err)
	}

	outcomes, err := fx.ts.server.RefireFailedRunsNow(ctx)
	if err != nil {
		t.Fatalf("refire pass: %v", err)
	}
	refireAssignmentID := ""
	refires := 0
	for _, o := range outcomes {
		if o.Disposition == store.RefireDispositionRefire {
			refires++
			refireAssignmentID = o.RefireAssignmentID
		}
	}
	if refires != 1 || refireAssignmentID == "" {
		t.Fatalf("refire pass outcomes %v, want exactly one refire", outcomes)
	}

	// The successor assignment reaches the member through the same control
	// wire and executes with the member's project credentials.
	runID2 := optionbRunAssigned(t, ctrl, "kanboard-develop")
	if runID2 == runID {
		t.Fatalf("successor re-used run id %s", runID2)
	}
	disp2, err := clients.GetAssignmentDisposition(ctx, connect.NewRequest(&pb.GetAssignmentDispositionRequest{RunId: runID2}))
	if err != nil {
		t.Fatalf("trigger reads successor disposition: %v", err)
	}
	if got := disp2.Msg.GetState(); got != pb.WorkflowAssignmentState_WORKFLOW_ASSIGNMENT_STATE_LEASED {
		t.Fatalf("successor disposition = %v, want LEASED", got)
	}
	if got := disp2.Msg.GetLeasedCriteriaId(); got != agent.CriteriaID {
		t.Fatalf("successor leased to %q, want %q", got, agent.CriteriaID)
	}
	optionbRunEnvelopes(t, fx.oClient, agent.Token, runID2, "kanboard-develop", "handler_complete", true)

	deadline = time.Now().Add(5 * time.Second)
	for {
		d2, err := clients.GetAssignmentDisposition(ctx, connect.NewRequest(&pb.GetAssignmentDispositionRequest{RunId: runID2}))
		if err != nil {
			t.Fatalf("trigger disposition after successor completion: %v", err)
		}
		if d2.Msg.GetState() == pb.WorkflowAssignmentState_WORKFLOW_ASSIGNMENT_STATE_TERMINAL {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("successor assignment never settled terminal (state %v)", d2.Msg.GetState())
		}
		time.Sleep(5 * time.Millisecond)
	}

	// The parked card stays parked: the successor run is lineage
	// bookkeeping, not a new card event — no extra move, no extra comment.
	if got := fx.board.columnOf(optionbTicketID); got != "Review" {
		t.Fatalf("parked card drifted to %q during the refire", got)
	}
	if got := len(fx.board.moveTargets()); got != 1 {
		t.Fatalf("mirror moved the card %d times (%v), want only the failure park", got, fx.board.moveTargets())
	}

	// The lineage closed: the refired original aged out of the candidate
	// scan and the successor run succeeded, so a fresh pass is a no-op.
	outcomes2, err := fx.ts.server.RefireFailedRunsNow(ctx)
	if err != nil {
		t.Fatalf("second refire pass: %v", err)
	}
	if len(outcomes2) != 0 {
		t.Fatalf("second refire pass outcomes %v, want none (lineage closed)", outcomes2)
	}
}

// optionbAwaitTerminal polls the trigger's disposition until the
// assignment settles terminal.
func optionbAwaitTerminal(t *testing.T, clients *optionbCastleClients, runID string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		disp, err := clients.GetAssignmentDisposition(context.Background(),
			connect.NewRequest(&pb.GetAssignmentDispositionRequest{RunId: runID}))
		if err != nil {
			t.Fatalf("trigger disposition for %s: %v", runID, err)
		}
		if disp.Msg.GetState() == pb.WorkflowAssignmentState_WORKFLOW_ASSIGNMENT_STATE_TERMINAL {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("assignment %s never settled terminal (state %v)", runID, disp.Msg.GetState())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// optionbRenderedRoutes is the small slice of the rendered routes payload
// the edit-flow assertions read: the configLibrary entries and the
// routes' configRef references.
type optionbRenderedRoutes struct {
	ConfigLibrary map[string]struct {
		CIGateCmd string `json:"ciGateCmd"`
	} `json:"configLibrary"`
	Routes []struct {
		Name      string `json:"name"`
		ConfigRef string `json:"configRef"`
	} `json:"routes"`
}

// TestOptionBEditConfigRefire is step 4 (EDIT-FLOW): the console edits the
// project's config through the etag-gated write and the edited record is
// immediately the routing truth. The edit RENAMES the develop route to
// "kanboard-develop-edited" and repoints its configRef at a freshly added
// "edited-gate" config entry, which makes both routing directions
// load-bearing: the pre-edit route name is rejected by the route gate (on
// the pre-edit record that name would still have fired, so a test that
// ignored the record write fails here), and the renamed route is accepted,
// leased to the project's operator on its control stream, and executed.
// The assembled routes payload carries the rename and the edited command,
// and a card refired through the edited route executes a fresh run that
// settles the card on Done.
//
// RESIDUAL (owed by criteria-k8s before the Option B program is declared
// closed): the resolved ciGateCmd never leaves castle/internal/confighome
// — the configSlots assembly that lifts a route's configRef into the
// rendered ConfigMap the platform consumes is outside castle, and
// SubmitProjectAssignment checks route names only — so this suite cannot
// evidence the refired run executing WITH the edited ci gate command. What
// is proven here: the editor flow (etag gate, lost-update conflict), the
// edited record as the routing truth in both directions, the edit's
// carriage in the rendered payload, and the refire routing through the
// edited route.
func TestOptionBEditConfigRefire(t *testing.T) {
	fx := optionbSeed(t)
	ctx := context.Background()

	// First fire runs to completion so the card sits in Done and the
	// adapter fully settled the ticket.
	agent, ctrl, runID1 := optionbFireReady(t, fx)
	clients := newOptionbCastleClients(fx.tsrv, fx.triggerToken)
	optionbRunEnvelopes(t, fx.oClient, agent.Token, runID1, "kanboard-develop", "handler_complete", true)
	optionbAwaitTerminal(t, clients, runID1)
	fx.optionbAwaitColumn(t, optionbTicketID, "Done")

	// EDIT: the console reads the record, renames the develop route and
	// repoints it at the edited ci gate config entry, validates, and
	// writes through the etag-gated surface — parapet's editor flow. The
	// rename (not an added route) is what lets the assertions below
	// discriminate the edit from the pre-edit record.
	record, etag := fx.getConfigRecord(optionbFireProject)
	const (
		editedRoute = "kanboard-develop-edited"
		editEntry   = "edited-gate"
		editCmd     = "edited ci gate v2: make ci && make vuln-scan"
	)
	if record.ConfigLibrary == nil {
		record.ConfigLibrary = map[string]confighome.ConfigEntry{}
	}
	record.ConfigLibrary[editEntry] = confighome.ConfigEntry{CIGateCmd: editCmd}
	for i := range record.Routes {
		if record.Routes[i].Name == "kanboard-develop" {
			record.Routes[i].Name = editedRoute
			record.Routes[i].ConfigRef = editEntry
		}
	}
	if err := record.Validate(); err != nil {
		t.Fatalf("console edit fails validation: %v", err)
	}
	editedDoc, err := record.Marshal()
	if err != nil {
		t.Fatalf("marshal edited record: %v", err)
	}
	newEtag := fx.seedConfigImport(optionbFireProject, record, etag)
	if newEtag == etag {
		t.Fatalf("etag unchanged after the edit (%s)", newEtag)
	}
	if _, etag2 := fx.getConfigRecord(optionbFireProject); etag2 != newEtag {
		t.Fatalf("readback etag, want the edit's etag %q", newEtag)
	}

	// A lost-update write conflicts: a writer holding the pre-edit etag
	// fails against the record the edit just moved (parapet's editor
	// lost-update guard, live on the wire).
	_, err = fx.projects.SetProjectConfig(ctx, bearer(&castlev1.SetProjectConfigRequest{
		ProjectId:    optionbFireProject,
		Document:     editedDoc,
		ExpectedEtag: etag,
	}, fx.consoleToken))
	if err == nil || connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("stale-etag write = %v, want FailedPrecondition conflict", err)
	}

	// REJECT direction (the load-bearing routing-truth assertion): the
	// pre-edit route name is gone from the edited record, so submitting
	// under it fails the route gate — this submit succeeds on the
	// pre-edit record, so a dropped write cannot pass the test.
	_, err = clients.SubmitProjectAssignment(ctx, connect.NewRequest(&castlev1.SubmitProjectAssignmentRequest{
		ProjectId:      optionbFireProject,
		WorkflowName:   "kanboard-develop",
		WorkflowSource: "hcl-source",
		IdempotencyKey: "optionb-edit-reject-probe",
	}))
	if err == nil || connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "is not a route in project") {
		t.Fatalf("pre-edit route name = %v, want InvalidArgument 'is not a route in project'", err)
	}

	// An unknown name fails the same gate (extra negative-path check; the
	// pre-edit-name reject above is the load-bearing one).
	_, err = clients.SubmitProjectAssignment(ctx, connect.NewRequest(&castlev1.SubmitProjectAssignmentRequest{
		ProjectId:      optionbFireProject,
		WorkflowName:   "kanboard-nonexistent",
		WorkflowSource: "hcl-source",
		IdempotencyKey: "optionb-edit-probe",
	}))
	if err == nil || connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("unknown route rejected = %v, want InvalidArgument", err)
	}

	// ACCEPT direction: the renamed route is live on the edited record —
	// the submit succeeds and dispatch leases the run to the project's
	// operator, delivering the assignment on the member's control stream
	// in order: first fire's (already consumed) → probe → later refire.
	acceptProbe, err := clients.SubmitProjectAssignment(ctx, connect.NewRequest(&castlev1.SubmitProjectAssignmentRequest{
		ProjectId:      optionbFireProject,
		WorkflowName:   editedRoute,
		WorkflowSource: "hcl-source",
		IdempotencyKey: "optionb-edit-accept-probe",
	}))
	if err != nil {
		t.Fatalf("renamed route submit: %v", err)
	}
	probeRunID := acceptProbe.Msg.GetRunId()
	if got := optionbRunAssigned(t, ctrl, editedRoute); got != probeRunID {
		t.Fatalf("control stream delivered run %s, want the accept probe's %s", got, probeRunID)
	}

	// The probe executes like any leased run — the adapter did not submit
	// it, so nothing watches it and the card stays untouched in Done;
	// settling it releases the operator's lease before the refire needs
	// the member idle.
	optionbRunEnvelopes(t, fx.oClient, agent.Token, probeRunID, editedRoute, "handler_complete", true)
	optionbAwaitTerminal(t, clients, probeRunID)

	// The assembled payload the platform renders from carries the edit:
	// the renamed route points at the edited config entry and the
	// pre-edit route name is gone from the rendered routes.
	assembled, err := confighome.NewAssemblyFor(optionbFetchAllRecords(t, fx)).RenderRoutesPayload()
	if err != nil {
		t.Fatalf("render edited records: %v", err)
	}
	var rendered optionbRenderedRoutes
	if err := json.Unmarshal(assembled, &rendered); err != nil {
		t.Fatalf("unmarshal rendered routes payload: %v", err)
	}
	if got := rendered.ConfigLibrary[editEntry].CIGateCmd; got != editCmd {
		t.Fatalf("rendered configLibrary[%s] = %q, want %q", editEntry, got, editCmd)
	}
	sawEdited := false
	for _, r := range rendered.Routes {
		if r.Name == "kanboard-develop" {
			t.Fatalf("rendered routes still carry the pre-edit name %q", r.Name)
		}
		if r.Name == editedRoute {
			if r.ConfigRef != editEntry {
				t.Fatalf("rendered route %s configRef = %q, want %q", editedRoute, r.ConfigRef, editEntry)
			}
			sawEdited = true
		}
	}
	if !sawEdited {
		t.Fatalf("rendered payload lost the renamed route %s", editedRoute)
	}

	// REFIRE through the edited route: the lane's RouteName is static at
	// boot, so the first adapter is stopped and a second one is booted
	// pointed at the renamed route. The reboot happens while the card
	// sits in Done — a fresh adapter treats the FIRST sighting of a card
	// in a trigger column as an entry, so booting over a card already in
	// Ready would fire spuriously; after the boot the operator drags the
	// settled card back into Ready and the next poll's column change
	// fires a fresh run whose assignment carries the edited route's
	// workflow name — the refire went through the edited record's route.
	// Stopping the first adapter first matters: the boot is otherwise
	// double-fired, with the stale lane's submit failing the route gate
	// and commenting failure on the card.
	fx.boardStop()
	fx.startBoard(t, optionbLaneConfig(editedRoute))
	fx.board.moveCard(t, optionbTicketID, "Ready")
	runID2 := optionbRunAssigned(t, ctrl, editedRoute)
	if runID2 == runID1 {
		t.Fatalf("refire re-used run id %s", runID2)
	}
	optionbRunEnvelopes(t, fx.oClient, agent.Token, runID2, editedRoute, "handler_complete", true)
	optionbAwaitTerminal(t, clients, runID2)
	fx.optionbAwaitColumn(t, optionbTicketID, "Done")
	doneMove := fmt.Sprintf("task %d -> %d", optionbTicketID, fx.board.columnIDOf["Done"])
	moves := fx.board.moveTargets()
	if len(moves) != 2 || moves[0] != doneMove || moves[1] != doneMove {
		t.Fatalf("mirror moves across both fires = %v (want %q twice), got something else", moves, doneMove)
	}
}

// TestOptionBIsolationRejectsWrongProject is step 5: the wrong-project
// agent identity cannot receive, lease, or even see another project's run.
// The rogue (criteria-go-dev's member) has its control stream open through
// the fire, so a leak would have been visible in dispatch.
func TestOptionBIsolationRejectsWrongProject(t *testing.T) {
	fx := optionbSeed(t)
	ctx := context.Background()

	// The rogue's control stream is open BEFORE the fire: the dispatch
	// scan ran with both identities eligible-in-theory, and only the
	// owning project's member must ever be offered the assignment.
	rogue := fx.agents["criteria-go-dev"]
	rogueCtrl := openControl(t, fx.oClient, rogue.CriteriaID, rogue.Token)
	expectControlReady(t, rogueCtrl)
	t.Cleanup(func() { _ = rogueCtrl.Close() })

	_, _, runID := optionbFireReady(t, fx)

	// The rogue's stream never sees the assignment.
	expectNoWorkflowAssignment(t, rogueCtrl, 800*time.Millisecond)

	// The rogue cannot read the run by id.
	if _, err := fx.sClient.GetRun(ctx, bearer(&pb.GetRunRequest{RunId: runID}, rogue.Token)); err == nil || connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("rogue agent GetRun = %v, want NotFound", err)
	}

	// The rogue cannot read the owning project's run list...
	if _, err := fx.projects.ListProjectRuns(ctx, bearer(&castlev1.ListProjectRunsRequest{ProjectId: optionbFireProject}, rogue.Token)); err == nil || connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("rogue ListProjectRuns on the owning project = %v, want PermissionDenied", err)
	}

	// ...and its own project's list never carries the foreign run.
	own, err := fx.projects.ListProjectRuns(ctx, bearer(&castlev1.ListProjectRunsRequest{ProjectId: "criteria-go-dev"}, rogue.Token))
	if err != nil {
		t.Fatalf("rogue lists its own project runs: %v", err)
	}
	for _, r := range own.Msg.GetRuns() {
		if r.GetRunId() == runID {
			t.Fatalf("foreign run %s leaked into the wrong project's run list", runID)
		}
	}

	// The disposition read is owner-scoped: only the assignment's owner
	// (the firing trigger) reads it.
	if _, err := newOptionbCastleClients(fx.tsrv, rogue.Token).
		GetAssignmentDisposition(ctx, connect.NewRequest(&pb.GetAssignmentDispositionRequest{RunId: runID})); err == nil || connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("rogue disposition read = %v, want PermissionDenied", err)
	}
}

// TestOptionBCleanupCutoverFlip is step 6: the flag-flip decision. Every
// registry record imports its namespace binding, flips post-cutover
// through the console's etag-gated editor, and the assembled CM payload
// ends warning-free — with the stale run-config surfaces hard-rejected
// (no zombie routes) and the post-flip pipeline still firing.
func TestOptionBCleanupCutoverFlip(t *testing.T) {
	fx := optionbSeed(t)

	// The unbound fail-closed check first: the kanboard record imports
	// with no binding of its own yet, and a binding-first cutover flip
	// must refuse it.
	record, _ := fx.getConfigRecord(optionbFireProject)
	if record.Binding != nil {
		t.Fatal("pre-import kanboard record unexpectedly carries a binding")
	}
	if err := record.MarkCutoverDone(); err == nil {
		t.Fatal("MarkCutoverDone accepted an unbound record")
	}

	// Import the criteria-jobs binding into both records of that
	// namespace, then flip every record post-cutover through the
	// console editor.
	bindingRaw, err := os.ReadFile("../confighome/testdata/binding-criteria-jobs.json")
	if err != nil {
		t.Fatalf("read criteria-jobs binding: %v", err)
	}
	binding, err := confighome.ParseBinding(bindingRaw)
	if err != nil {
		t.Fatalf("parse criteria-jobs binding: %v", err)
	}
	for _, slug := range slices.Sorted(maps.Keys(optionbProjects)) {
		projectID := optionbProjects[slug]
		record, etag := fx.getConfigRecord(projectID)
		attachedCriteriaJobs := record.Binding == nil
		if attachedCriteriaJobs {
			record.Binding = binding
			// Round-trip the just-parsed binding through the record's
			// render (mirrors the per-binding check in optionbImportSeeds):
			// a malformed field would otherwise go unnoticed, since the
			// binding is imported but nothing else reads it.
			renderedBinding, err := record.RenderBinding()
			if err != nil {
				t.Fatalf("render criteria-jobs binding for %s: %v", projectID, err)
			}
			equal, _, err := confighome.EqualPayload(renderedBinding, bindingRaw)
			if err != nil {
				t.Fatalf("compare criteria-jobs binding for %s: %v", projectID, err)
			}
			if !equal {
				t.Fatalf("criteria-jobs binding round-trip mismatch for %s", projectID)
			}
		}
		if err := record.MarkCutoverDone(); err != nil {
			t.Fatalf("flip %s post-cutover: %v", projectID, err)
		}
		newEtag := fx.seedConfigImport(projectID, record, etag)
		if newEtag == etag {
			t.Fatalf("%s: flip produced the same etag", projectID)
		}
		readback, _ := fx.getConfigRecord(projectID)
		if readback.Cutover != confighome.CutoverDone {
			t.Fatalf("%s: readback cutover = %q, want done", projectID, readback.Cutover)
		}
		if readback.Binding == nil {
			t.Fatalf("%s: readback lost the namespace binding", projectID)
		}
	}

	// The CM payload is now assembled warning-free, with the registry
	// fully post-cutover.
	all := optionbFetchAllRecords(t, fx)
	asm := confighome.NewAssemblyFor(all)
	if warnings := asm.DeprecationWarnings(); len(warnings) != 0 {
		t.Fatalf("post-cutover assembly still emits deprecation warnings: %v", warnings)
	}
	if _, err := asm.RenderRoutesPayload(); err != nil {
		t.Fatalf("post-cutover render: %v", err)
	}

	// No zombie routes: reintroducing a stale global run-config surface
	// on a post-cutover registry hard-rejects the assembled payload.
	last := -1
	zombie := append([]confighome.ProjectRecord(nil), all...)
	for i, pr := range zombie {
		if pr.ProjectID == optionbFireProject {
			last = i
			break
		}
	}
	if last == -1 {
		t.Fatalf("no %s record in the assembled set", optionbFireProject)
	}
	zombieRecord := *zombie[last].Record
	zombieRecord.Catalog = maps.Clone(zombieRecord.Catalog)
	stale := zombieRecord.Catalog["kanboard-triage-url"]
	stale.Volumes = []confighome.Volume{{
		Name:      "stale-data",
		Kind:      "pvc",
		Claim:     "criteria-data",
		MountPath: "/data",
	}}
	zombieRecord.Catalog["kanboard-triage-url"] = stale
	zombie[last].Record = &zombieRecord
	if _, err := confighome.NewAssemblyFor(zombie).RenderRoutesPayload(); err == nil || !strings.Contains(err.Error(), "post-cutover payload still carries global run-config surfaces") {
		t.Fatalf("zombie render = %v, want the KB-313 hard error", err)
	}

	// The post-flip pipeline still fires: a fresh card routes through the
	// flipped record, the project member leases and executes it, and the
	// card settles on Done.
	member, _, postFlipRun := optionbFireReady(t, fx)
	optionbRunEnvelopes(t, fx.oClient, member.Token, postFlipRun, "kanboard-develop", "handler_complete", true)
	optionbAwaitTerminal(t, newOptionbCastleClients(fx.tsrv, fx.triggerToken), postFlipRun)
	fx.optionbAwaitColumn(t, optionbTicketID, "Done")
}
