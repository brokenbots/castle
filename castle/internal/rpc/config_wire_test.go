package rpc

import (
	"context"
	"errors"
	"strings"
	"testing"

	"connectrpc.com/connect"

	castlev1 "github.com/brokenbots/castle/castle/gen/castle/v1"
	"github.com/brokenbots/castle/castle/internal/confighome"
)

// wireConfigRecord builds the valid two-workflow record the wire contracts
// share: a URL tree with an interface declaration, an image workflow, one
// config library entry, and a route per workflow.
func wireConfigRecord() *confighome.Record {
	return &confighome.Record{
		APIVersion:   confighome.APIVersion,
		Kind:         confighome.KindRecord,
		RegistryName: "wire-proj",
		Namespace:    "criteria-wire",
		Catalog: map[string]confighome.Workflow{
			"url-wf": {
				Type: confighome.TypeURL,
				URL:  "https://example.com/tree",
				Ref:  "refs/heads/main",
				Interface: &confighome.Interface{
					APIVersion:  confighome.APIVersion,
					Kind:        confighome.KindInterface,
					Workflow:    "url-wf",
					Adapters:    []string{},
					Mounts:      []confighome.Mount{},
					SecretEnv:   []string{},
					Env:         []string{},
					ConfigSlots: []confighome.ConfigSlot{{Slot: "buildCmd", Variable: "build_cmd", Target: "step:build"}},
				},
			},
			"image-wf": {
				Type:    confighome.TypeImage,
				Image:   "registry.example.com/runner:v1",
				Volumes: []confighome.Volume{},
				Secrets: []confighome.Secret{},
				Env:     map[string]string{},
			},
		},
		Digests: map[string]string{"url-wf": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		ConfigLibrary: map[string]confighome.ConfigEntry{
			"build-config": {BuildCmd: "go build ./...", Tags: []string{}},
		},
		Routes: []confighome.Route{
			{Name: "url-route", Project: "wire-proj", Workflow: "url-wf", ConfigRef: "build-config", States: []string{confighome.DefaultState}},
			{Name: "image-route", Project: "wire-proj", Workflow: "image-wf", States: []string{confighome.DefaultState}},
		},
	}
}

// TestProjectConfigWireEtagContract pins the KB-339 config swap contract
// over the wire: only console sessions may write, the unset record reads
// empty with the empty etag, the whole-record swap is content-addressed and
// idempotent under the current etag, stale expectations fail with
// FailedPrecondition, and agent members read the result but cannot write.
func TestProjectConfigWireEtagContract(t *testing.T) {
	f := newSettingsFixture(t)
	ctx := context.Background()
	createProject(t, ctx, f.pClient, f.agentToken, f.projectID, castlev1.ProjectMembershipPolicy_PROJECT_MEMBERSHIP_POLICY_OPEN)

	get := func(token string) (*castlev1.GetProjectConfigResponse, error) {
		t.Helper()
		resp, err := f.pClient.GetProjectConfig(ctx, bearer(&castlev1.GetProjectConfigRequest{ProjectId: f.projectID}, token))
		if err != nil {
			return nil, err
		}
		return resp.Msg, nil
	}
	set := func(token, expectedEtag string, doc []byte) (*castlev1.SetProjectConfigResponse, error) {
		t.Helper()
		resp, err := f.pClient.SetProjectConfig(ctx, bearer(&castlev1.SetProjectConfigRequest{
			ProjectId: f.projectID, ExpectedEtag: expectedEtag, Document: doc,
		}, token))
		if err != nil {
			return nil, err
		}
		return resp.Msg, nil
	}

	// A fresh project reads the empty record with the empty (unset) etag —
	// an agent member's read is fine.
	fresh, err := get(f.agentToken)
	if err != nil {
		t.Fatalf("agent GetProjectConfig: %v", err)
	}
	if len(fresh.Document) != 0 || fresh.Etag != "" {
		t.Fatalf("fresh config = %q etag %q, want empty/empty", fresh.Document, fresh.Etag)
	}

	// Wrong credential class: protocol tokens (agent identities) do not
	// carry config writes (KB-324 write shape).
	first := wireConfigRecord()
	firstDoc, err := first.Marshal()
	if err != nil {
		t.Fatalf("marshal record: %v", err)
	}
	if _, err := set(f.agentToken, "", firstDoc); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("agent config write: got %v, want PermissionDenied", err)
	}

	// A stale expectation loses against the empty etag too.
	if _, err := set(f.consoleToken, "stale-expectation", firstDoc); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("stale etag: got %v, want FailedPrecondition", err)
	}

	// The console write lands: etag is the returned handle, and the stored
	// doc round-trips a validated record.
	wrote, err := set(f.consoleToken, "", firstDoc)
	if err != nil {
		t.Fatalf("console config write: %v", err)
	}
	if wrote.Etag == "" {
		t.Fatal("first config set returned empty etag")
	}
	stored, err := get(f.consoleToken)
	if err != nil {
		t.Fatalf("console GetProjectConfig: %v", err)
	}
	if stored.Etag != wrote.Etag {
		t.Fatalf("stored etag %q, want %q", stored.Etag, wrote.Etag)
	}
	parsed, err := confighome.ParseRecord(stored.Document)
	if err != nil {
		t.Fatalf("stored doc does not parse as a record: %v", err)
	}
	if err := parsed.Validate(); err != nil {
		t.Fatalf("stored doc is not a valid record: %v", err)
	}
	if len(parsed.Routes) != 2 || parsed.Routes[0].Name != "url-route" {
		t.Fatalf("stored routes = %+v, want the two wire routes", parsed.Routes)
	}

	// Identical content under the current etag is a no-op that does not
	// move the etag.
	again, err := set(f.consoleToken, wrote.Etag, firstDoc)
	if err != nil {
		t.Fatalf("identical re-set: %v", err)
	}
	if again.Etag != wrote.Etag {
		t.Fatalf("identical re-set etag %q, want unchanged %q", again.Etag, wrote.Etag)
	}

	// A real swap moves the etag; the previous expectation loses.
	second := wireConfigRecord()
	wf := second.Catalog["url-wf"]
	wf.Ref = "refs/heads/release"
	second.Catalog["url-wf"] = wf
	secondDoc, err := second.Marshal()
	if err != nil {
		t.Fatalf("marshal second record: %v", err)
	}
	if _, err := set(f.consoleToken, wrote.Etag, secondDoc); err != nil {
		t.Fatalf("console config swap: %v", err)
	}
	swapped, err := get(f.agentToken)
	if err != nil {
		t.Fatalf("agent read after swap: %v", err)
	}
	if swapped.Etag == wrote.Etag {
		t.Fatal("swapped record kept the first etag")
	}
}

// TestProjectConfigWireRejectsInvalidShape pins the fail-closed record
// validation on the wire: garbage JSON and syntactically-valid-but-
// semantically-invalid records are rejected with InvalidArgument, and the
// empty document clears the record under etag control.
func TestProjectConfigWireRejectsInvalidShape(t *testing.T) {
	f := newSettingsFixture(t)
	ctx := context.Background()
	createProject(t, ctx, f.pClient, f.agentToken, f.projectID, castlev1.ProjectMembershipPolicy_PROJECT_MEMBERSHIP_POLICY_OPEN)
	set := func(expectedEtag string, doc []byte) (*castlev1.SetProjectConfigResponse, error) {
		t.Helper()
		resp, err := f.pClient.SetProjectConfig(ctx, bearer(&castlev1.SetProjectConfigRequest{
			ProjectId: f.projectID, ExpectedEtag: expectedEtag, Document: doc,
		}, f.consoleToken))
		if err != nil {
			return nil, err
		}
		return resp.Msg, nil
	}

	if _, err := set("", []byte(`{"kind": "garbage"`)); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("garbage json: got %v, want InvalidArgument", err)
	}
	bad := wireConfigRecord()
	bad.Namespace = "Bad_NS!"
	badDoc, err := bad.Marshal()
	if err != nil {
		t.Fatalf("marshal bad record: %v", err)
	}
	if _, err := set("", badDoc); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("invalid record shape: got %v, want InvalidArgument", err)
	}
	current, err := f.pClient.GetProjectConfig(ctx, bearer(&castlev1.GetProjectConfigRequest{ProjectId: f.projectID}, f.consoleToken))
	if err != nil {
		t.Fatalf("get after rejected writes: %v", err)
	}
	if len(current.Msg.Document) != 0 || current.Msg.Etag != "" {
		t.Fatalf("rejected writes leaked a record: %q etag %q", current.Msg.Document, current.Msg.Etag)
	}

	// Clearing under the current etag returns to the empty/empty state.
	if _, err := set("", nil); err != nil {
		t.Fatalf("clear empty record: %v", err)
	}
	basis := wireConfigRecord()
	basisDoc, err := basis.Marshal()
	if err != nil {
		t.Fatalf("marshal basis record: %v", err)
	}
	basisWrite, err := set("", basisDoc)
	if err != nil {
		t.Fatalf("basis write: %v", err)
	}
	if _, err := set(basisWrite.Etag, nil); err != nil {
		t.Fatalf("clear under etag: %v", err)
	}
	cleared, err := f.pClient.GetProjectConfig(ctx, bearer(&castlev1.GetProjectConfigRequest{ProjectId: f.projectID}, f.consoleToken))
	if err != nil {
		t.Fatalf("get after clear: %v", err)
	}
	if len(cleared.Msg.Document) != 0 || cleared.Msg.Etag != "" {
		t.Fatalf("cleared config = %q etag %q, want empty/empty", cleared.Msg.Document, cleared.Msg.Etag)
	}
	// Clearing is NOT idempotent across etag churn: a stale expectation
	// against the (now unset) empty etag loses.
	if _, err := set("dangling-etag", nil); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("stale clear: got %v, want FailedPrecondition", err)
	}
}

// TestListWorkflowCatalogWire pins the catalog listing over the wire: the
// unset record lists empty with the empty etag, an imported record lists
// its entries name-sorted with tree refs and learned digests, and a config
// edit moves both the etag and the listed digests.
func TestListWorkflowCatalogWire(t *testing.T) {
	f := newSettingsFixture(t)
	ctx := context.Background()
	createProject(t, ctx, f.pClient, f.agentToken, f.projectID, castlev1.ProjectMembershipPolicy_PROJECT_MEMBERSHIP_POLICY_OPEN)

	list := func(token string) (*castlev1.ListWorkflowCatalogResponse, error) {
		t.Helper()
		resp, err := f.pClient.ListWorkflowCatalog(ctx, bearer(&castlev1.ListWorkflowCatalogRequest{ProjectId: f.projectID}, token))
		if err != nil {
			return nil, err
		}
		return resp.Msg, nil
	}
	unset, err := list(f.agentToken)
	if err != nil {
		t.Fatalf("unset catalog list: %v", err)
	}
	if len(unset.Entries) != 0 || unset.Etag != "" {
		t.Fatalf("unset catalog = %+v etag %q, want empty/empty", unset.Entries, unset.Etag)
	}
	// The catalog read is also open to the console credential class.
	if _, err := list(f.consoleToken); err != nil {
		t.Fatalf("console catalog list: %v", err)
	}

	record := wireConfigRecord()
	doc, err := record.Marshal()
	if err != nil {
		t.Fatalf("marshal record: %v", err)
	}
	wrote, err := f.pClient.SetProjectConfig(ctx, bearer(&castlev1.SetProjectConfigRequest{
		ProjectId: f.projectID, Document: doc,
	}, f.consoleToken))
	if err != nil {
		t.Fatalf("config write: %v", err)
	}
	listed, err := list(f.agentToken)
	if err != nil {
		t.Fatalf("catalog list: %v", err)
	}
	if listed.Etag != wrote.Msg.Etag {
		t.Fatalf("list etag %q, want config etag %q", listed.Etag, wrote.Msg.Etag)
	}
	if len(listed.Entries) != 2 || listed.Entries[0].Name != "image-wf" || listed.Entries[1].Name != "url-wf" {
		t.Fatalf("entries = %+v, want image-wf then url-wf (name-sorted)", listed.Entries)
	}
	imageEntry, urlEntry := listed.Entries[0], listed.Entries[1]
	if imageEntry.Type != "image" || imageEntry.Image != "registry.example.com/runner:v1" {
		t.Fatalf("image entry = %+v", imageEntry)
	}
	if urlEntry.Type != "url" || urlEntry.Url != "https://example.com/tree" || urlEntry.Ref != "refs/heads/main" {
		t.Fatalf("url entry = %+v", urlEntry)
	}
	if urlEntry.Digest != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" || imageEntry.Digest != "" {
		t.Fatalf("digests = %q/%q, want url-wf aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa / image-wf empty", urlEntry.Digest, imageEntry.Digest)
	}
	if urlEntry.InterfaceName != "url-wf" || imageEntry.InterfaceName != "" {
		t.Fatalf("interface names = %q/%q", imageEntry.InterfaceName, urlEntry.InterfaceName)
	}

	// A config edit moves the digest through the swap and the list follows.
	edit := wireConfigRecord()
	edit.Digests["url-wf"] = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	editDoc, err := edit.Marshal()
	if err != nil {
		t.Fatalf("marshal edit: %v", err)
	}
	if _, err := f.pClient.SetProjectConfig(ctx, bearer(&castlev1.SetProjectConfigRequest{
		ProjectId: f.projectID, ExpectedEtag: wrote.Msg.Etag, Document: editDoc,
	}, f.consoleToken)); err != nil {
		t.Fatalf("digest swap: %v", err)
	}
	after, err := list(f.agentToken)
	if err != nil {
		t.Fatalf("list after swap: %v", err)
	}
	if after.Etag == wrote.Msg.Etag {
		t.Fatal("list kept the pre-swap etag")
	}
	if after.Etag == "" {
		t.Fatal("empty etag after swap")
	}
	var got string
	for _, entry := range after.Entries {
		if entry.Name == "url-wf" {
			got = entry.Digest
		}
	}
	if got != "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" {
		t.Fatalf("digest after swap = %q, want bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", got)
	}
	if !strings.Contains(after.Etag, "") || len(after.Etag) == 0 {
		t.Fatal("empty etag after swap")
	}
}

// TestSubmitProjectAssignmentRespectsProjectRoutes pins KB-339 item 3 over
// the wire: once a project imports a record with routes, its trigger
// surface is closed — assignments naming other workflow names reject with
// InvalidArgument, assignments naming carried routes queue, and projects
// without a route-bearing record keep the legacy open surface.
func TestSubmitProjectAssignmentRespectsProjectRoutes(t *testing.T) {
	f := newSettingsFixture(t)
	ctx := context.Background()
	createProject(t, ctx, f.pClient, f.agentToken, f.projectID, castlev1.ProjectMembershipPolicy_PROJECT_MEMBERSHIP_POLICY_OPEN)

	submit := func(token, workflowName string) error {
		t.Helper()
		_, err := f.pClient.SubmitProjectAssignment(ctx, bearer(&castlev1.SubmitProjectAssignmentRequest{
			ProjectId:      f.projectID,
			WorkflowName:   workflowName,
			WorkflowSource: "hcl-source",
			IdempotencyKey: "key-" + workflowName,
		}, token))
		return err
	}

	// No imported record: the legacy surface accepts anything.
	if err := submit(f.agentToken, "anything-goes"); err != nil {
		t.Fatalf("unrecorded project submit: %v", err)
	}

	record := wireConfigRecord()
	doc, err := record.Marshal()
	if err != nil {
		t.Fatalf("marshal record: %v", err)
	}
	if _, err := f.pClient.SetProjectConfig(ctx, bearer(&castlev1.SetProjectConfigRequest{
		ProjectId: f.projectID, Document: doc,
	}, f.consoleToken)); err != nil {
		t.Fatalf("config write: %v", err)
	}

	// A name the record carries queues (the route is also a valid
	// workflow_name shape the trigger lanes submit).
	if err := submit(f.agentToken, "url-route"); err != nil {
		t.Fatalf("routed submit rejected: %v", err)
	}
	// A foreign name fails closed AT the route gate.
	err = submit(f.agentToken, "not-a-route")
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("unrouted submit: got %v, want InvalidArgument", err)
	}
	var routeErr *connect.Error
	if !errors.As(err, &routeErr) || routeErr.Code() != connect.CodeInvalidArgument {
		t.Fatalf("unrouted submit error type %T, want *connect.Error", err)
	}
	if got, want := routeErr.Message(), "not a route in project"; !strings.Contains(got, want) {
		t.Fatalf("unrouted message %q, want it to name the gate (%s)", got, want)
	}

	// A record without routes keeps the legacy surface (nothing to gate).
	routeless := wireConfigRecord()
	routeless.Routes = []confighome.Route{}
	routelessDoc, err := routeless.Marshal()
	if err != nil {
		t.Fatalf("marshal routeless: %v", err)
	}
	current, err := f.pClient.GetProjectConfig(ctx, bearer(&castlev1.GetProjectConfigRequest{ProjectId: f.projectID}, f.consoleToken))
	if err != nil {
		t.Fatalf("get current record: %v", err)
	}
	_, err = f.pClient.SetProjectConfig(ctx, bearer(&castlev1.SetProjectConfigRequest{
		ProjectId: f.projectID, ExpectedEtag: current.Msg.Etag, Document: routelessDoc,
	}, f.consoleToken))
	if err != nil {
		t.Fatalf("swap to routeless: %v", err)
	}
	if err := submit(f.agentToken, "anything-goes"); err != nil {
		t.Fatalf("routeless record submit: %v", err)
	}
}
