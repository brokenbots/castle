package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brokenbots/castle/castle/internal/confighome"
	"github.com/brokenbots/castle/castle/internal/store"
	"github.com/brokenbots/castle/castle/internal/store/sqlite"
)

// projectSlugs is the explicit registry-name to castle-project-id mapping
// the live fixture imports with, and its castle-side slugs.
var projectSlugs = []string{
	"criteria-k8s-workflow-runner",
	"kanboard-tickets",
	"criteria-go-dev",
	"typescript-sdk",
}

// importArgs assembles the flags for a fixture import into target db.
func importArgs(db string, extra ...string) []string {
	args := []string{
		"--routes-file", filepath.Join("testdata", "routes.json"),
		"--binding", "criteria-go-dev=" + filepath.Join("testdata", "binding-go-dev.json"),
		"--binding", "typescript-sdk=" + filepath.Join("testdata", "binding-typescript-sdk.json"),
		"--platform", "Criteria K8s Workflow Runner",
		"--db", db,
		"--map", "Criteria K8s Workflow Runner=criteria-k8s-workflow-runner",
		"--map", "Kanboard Tickets=kanboard-tickets",
		"--map", "Criteria Go Dev=criteria-go-dev",
		"--map", "TypeScript SDK=typescript-sdk",
	}
	return append(args, extra...)
}

// tempImport runs a fixture import into a fresh store and reopens it.
func tempImport(t *testing.T, extra ...string) (*sqlite.Store, string) {
	t.Helper()
	db := filepath.Join(t.TempDir(), "castle.db")
	var stdout, stderr bytes.Buffer
	code := run(importArgs(db, extra...), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr.String())
	}
	st, err := sqlite.Open(db)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st, db
}

// loadStoredAssembly re-reads every stored record and assembles them.
func loadStoredAssembly(t *testing.T, st *sqlite.Store) ([]byte, error) {
	t.Helper()
	records := make([]confighome.ProjectRecord, 0, len(projectSlugs))
	for _, id := range projectSlugs {
		doc, _, err := st.GetProjectConfig(context.Background(), id)
		if err != nil {
			return nil, err
		}
		record, err := confighome.ParseRecord(doc)
		if err != nil {
			t.Fatalf("parse stored record %s: %v", id, err)
		}
		if err := record.Validate(); err != nil {
			t.Fatalf("stored record %s fails validation: %v", id, err)
		}
		records = append(records, confighome.ProjectRecord{ProjectID: id, Record: record})
	}
	return confighome.NewAssemblyFor(records).RenderRoutesPayload()
}

// TestConfigImportRoundTripAndWrites pins the import lifecycle over the
// live fixtures: the four records partition out of the routes payload with
// their namespace binding payloads attached, the verified round-trip
// renders canonically back to the live file, the writes land per project
// (creating missing projects only under --create-projects), and the STORED
// records re-assemble to exactly the live payload.
func TestConfigImportRoundTripAndWrites(t *testing.T) {
	// Pre-create exactly one project so the run covers both paths: an
	// existing target project and --create-projects additions.
	db := filepath.Join(t.TempDir(), "castle.db")
	pre, err := sqlite.Open(db)
	if err != nil {
		t.Fatalf("provision store: %v", err)
	}
	if err := pre.CreateProject(context.Background(), &store.Project{
		ID: "kanboard-tickets", MembershipPolicy: store.ProjectMembershipOpen,
	}); err != nil {
		t.Fatalf("pre-create kanboard-tickets: %v", err)
	}
	if err := pre.Close(); err != nil {
		t.Fatalf("close provisioning store: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := run(importArgs(db, "--create-projects"), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "verified") {
		t.Fatalf("stdout missing verification summary: %q", stdout.String())
	}

	st, err := sqlite.Open(db)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer st.Close()
	ctx := context.Background()

	// Every project exists (created or pre-existing), and its record
	// landed with an etag, parses, validates, and carries the expected
	// namespace bindings.
	for _, id := range projectSlugs {
		project, err := st.GetProject(ctx, id)
		if err != nil {
			t.Fatalf("get project %s: %v", id, err)
		}
		if id != "kanboard-tickets" && project.CreatedAt.IsZero() {
			t.Fatalf("project %s was not created by the import", id)
		}
		doc, etag, err := st.GetProjectConfig(ctx, id)
		if err != nil {
			t.Fatalf("get config for %s: %v", id, err)
		}
		if etag == "" {
			t.Fatalf("%s stored an empty etag", id)
		}
		record, err := confighome.ParseRecord(doc)
		if err != nil {
			t.Fatalf("%s doc does not parse: %v", id, err)
		}
		if err := record.Validate(); err != nil {
			t.Fatalf("%s doc fails validation: %v", id, err)
		}
		switch id {
		case "criteria-go-dev", "typescript-sdk":
			if record.Binding == nil {
				t.Fatalf("%s lost its binding payload", id)
			}
		default:
			if record.Binding != nil {
				t.Fatalf("%s imported a binding it never carried", id)
			}
		}
	}

	// The stored records re-assemble into exactly the live payload.
	live, err := os.ReadFile(filepath.Join("testdata", "routes.json"))
	if err != nil {
		t.Fatalf("read live routes: %v", err)
	}
	rendered, err := loadStoredAssembly(t, st)
	if err != nil {
		t.Fatalf("render stored assembly: %v", err)
	}
	equal, checksum, err := confighome.EqualPayload(rendered, live)
	if err != nil {
		t.Fatalf("compare stored assembly: %v", err)
	}
	if !equal || checksum == "" {
		t.Fatalf("stored records re-rendered a DIFFERENT routes payload (equal %v)", equal)
	}

	// The binding payload round-trips per record.
	binding, err := os.ReadFile(filepath.Join("testdata", "binding-go-dev.json"))
	if err != nil {
		t.Fatalf("read live binding: %v", err)
	}
	doc, etag, err := st.GetProjectConfig(ctx, "criteria-go-dev")
	if err != nil || etag == "" {
		t.Fatalf("go-dev config: %v etag %q", err, etag)
	}
	goDev, err := confighome.ParseRecord(doc)
	if err != nil {
		t.Fatalf("parse go-dev record: %v", err)
	}
	renderedBinding, err := goDev.RenderBinding()
	if err != nil {
		t.Fatalf("render binding: %v", err)
	}
	if ok, _, err := confighome.EqualPayload(renderedBinding, binding); err != nil || !ok {
		t.Fatalf("binding round-trip mismatch: ok %v err %v", ok, err)
	}
}

// TestConfigImportCutoverRefusesUnrenderableFlip pins the KB-313 gate
// on the flip itself (KB-339 BL-1): with the live fixtures, --cutover
// flips only the two bound records (binding-first) and the flip's
// post-write check is the pre-ASSEMBLY render — the flipped records
// share the kanboard-develop-url tree with the un-flipped Kanboard
// Tickets record, so the partial flip cannot re-render and the run
// refuses BEFORE any write instead of landing an un-renderable
// castle-authoritative state. Unbound records are named in stderr and
// left untouched.
func TestConfigImportCutoverRefusesUnrenderableFlip(t *testing.T) {
	db := filepath.Join(t.TempDir(), "castle.db")
	var stdout, stderr bytes.Buffer
	code := run(importArgs(db, "--create-projects", "--cutover"), &stdout, &stderr)
	msg := stderr.String()
	if code != 1 || !strings.Contains(msg, "re-render routes payload after the cutover flip") ||
		!strings.Contains(msg, "kanboard-develop-url") || !strings.Contains(msg, "no writes happened") {
		t.Fatalf("partial flip over a shared tree: exit %d msg %s, want 1 naming the refused render before any write", code, msg)
	}
	if !strings.Contains(msg, `"Kanboard Tickets" (kanboard-tickets) stays pre-cutover`) ||
		!strings.Contains(msg, `"Criteria K8s Workflow Runner" (criteria-k8s-workflow-runner) stays pre-cutover`) {
		t.Fatalf("unbound records must be named and left pre-cutover: %s", msg)
	}

	// Nothing landed: no project, no config record.
	st, err := sqlite.Open(db)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer st.Close()
	ctx := context.Background()
	for _, id := range projectSlugs {
		if _, _, err := st.GetProjectConfig(ctx, id); err == nil {
			t.Fatalf("refused import left a config record for %s", id)
		}
		if _, err := st.GetProject(ctx, id); err == nil {
			t.Fatalf("refused import created %s", id)
		}
	}
}

// TestConfigImportCutoverFlipsBoundRecords pins the successful
// castle-authoritative flip end to end: when EVERY record imports a
// namespace binding, --cutover marks them all done, the deprecated
// global run-config surfaces leave every catalog entry, the flipped
// state re-assembles into a valid post-cutover routes payload with no
// stale surfaces and no registry entry left pre-cut, and each flipped
// record's binding still renders.
func TestConfigImportCutoverFlipsBoundRecords(t *testing.T) {
	jobsBinding := filepath.Join(t.TempDir(), "binding-jobs.json")
	// criteria-jobs carries both the Kanboard Tickets and the platform
	// registry entries, so one binding payload binds both records.
	if err := os.WriteFile(jobsBinding, []byte(`{"apiVersion":"criteria.brokenbots.dev/v1","kind":"CriteriaBinding","images":{},"env":{},"volumes":[],"secrets":[],"config":{}}`), 0o644); err != nil {
		t.Fatalf("write criteria-jobs binding: %v", err)
	}
	st, _ := tempImport(t, "--create-projects", "--cutover", "--binding", "criteria-jobs="+jobsBinding)
	ctx := context.Background()

	for _, id := range projectSlugs {
		doc, etag, err := st.GetProjectConfig(ctx, id)
		if err != nil || etag == "" {
			t.Fatalf("get %s: %v etag %q", id, err, etag)
		}
		record, err := confighome.ParseRecord(doc)
		if err != nil {
			t.Fatalf("parse %s: %v", id, err)
		}
		if record.Cutover != confighome.CutoverDone {
			t.Fatalf("%s cutover = %q, want done", id, record.Cutover)
		}
		if record.Binding == nil {
			t.Fatalf("%s must carry its binding payload post-flip", id)
		}
		if _, err := record.RenderBinding(); err != nil {
			t.Fatalf("%s binding render post-flip: %v", id, err)
		}
		for _, wf := range record.Catalog {
			if len(wf.Volumes) != 0 || len(wf.Secrets) != 0 || len(wf.Env) != 0 || len(wf.AdapterImages) != 0 {
				t.Fatalf("%s catalog entry %q (type %s) still carries a stale global surface post-flip", id, wf.Namespace, wf.Type)
			}
		}
	}

	// The stored records re-assemble into a valid post-cutover payload:
	// no stale surfaces anywhere and every registry entry done.
	rendered, err := loadStoredAssembly(t, st)
	if err != nil {
		t.Fatalf("stored records must re-assemble post-cutover: %v", err)
	}
	var doc struct {
		WorkflowLibrary map[string]confighome.Workflow      `json:"workflowLibrary"`
		ProjectRegistry map[string]confighome.RegistryEntry `json:"projectRegistry"`
	}
	if err := json.Unmarshal(rendered, &doc); err != nil {
		t.Fatalf("parse rendered payload: %v", err)
	}
	if len(doc.WorkflowLibrary) == 0 {
		t.Fatalf("rendered payload must carry the workflowLibrary")
	}
	for name, wf := range doc.WorkflowLibrary {
		if len(wf.Volumes) != 0 || len(wf.Secrets) != 0 || len(wf.Env) != 0 || len(wf.AdapterImages) != 0 {
			t.Fatalf("rendered post-cutover payload still carries stale surfaces on %q", name)
		}
	}
	for name, entry := range doc.ProjectRegistry {
		if entry.Cutover != confighome.CutoverDone {
			t.Fatalf("rendered registry entry %s cutover = %q, want all done", name, entry.Cutover)
		}
	}
}

// TestConfigImportCutoverRequiresBindings pins the binding-first
// refusal: with NO namespace bindings imported, no record may flip and
// the run refuses before any write — flipping an unbound project would
// leave its namespace without the global run-config surfaces and with
// no binding of its own.
func TestConfigImportCutoverRequiresBindings(t *testing.T) {
	base := importArgs(filepath.Join(t.TempDir(), "castle.db"), "--create-projects", "--cutover")
	var args []string
	for i := 0; i < len(base); i++ {
		if i < len(base)-1 && base[i] == "--binding" {
			i++
			continue
		}
		args = append(args, base[i])
	}
	var stdout, stderr bytes.Buffer
	if code := run(args, &stdout, &stderr); code != 1 ||
		!strings.Contains(stderr.String(), "cutover refuses to flip: no record carries a namespace binding") {
		t.Fatalf("all-unbound cutover must refuse pre-write, exit %d stderr %s", code, stderr.String())
	}
}

// TestConfigImportFailsClosed pins the operational guards: usage errors
// exit 2, missing mappings and missing projects fail before any write,
// foreign binding namespaces name the namespace, and a failed import
// leaves NO config records on the store.
func TestConfigImportFailsClosed(t *testing.T) {
	try := func(args []string) (code int, stderr string) {
		t.Helper()
		var errOut bytes.Buffer
		return run(args, &bytes.Buffer{}, &errOut), errOut.String()
	}

	// Usage errors: a missing routes source and a malformed pair shape.
	db := filepath.Join(t.TempDir(), "castle.db")
	code, msg := try([]string{"--platform", "Criteria K8s Workflow Runner", "--db", db})
	if code != 2 || !strings.Contains(msg, "--routes-file") {
		t.Fatalf("missing routes: exit %d msg %s, want 2 naming --routes-file", code, msg)
	}
	code, msg = try(importArgs(db)[:0])
	if code != 2 {
		t.Fatalf("empty args: exit %d, want 2", code)
	}
	code, msg = try(append(importArgs(db, "--create-projects"), "--policy", "weird"))
	if code != 2 || !strings.Contains(msg, "unknown --policy") {
		t.Fatalf("bad policy: exit %d msg %s, want 2 unknown policy", code, msg)
	}

	// Mapping completeness failure: only one of four records mapped.
	partial := []string{
		"--routes-file", filepath.Join("testdata", "routes.json"),
		"--platform", "Criteria K8s Workflow Runner",
		"--db", filepath.Join(t.TempDir(), "castle.db"),
		"--map", "Criteria K8s Workflow Runner=criteria-k8s-workflow-runner",
	}
	code, msg = try(partial)
	if code != 1 || !strings.Contains(msg, "no --map entry for") || !strings.Contains(msg, "Kanboard Tickets") {
		t.Fatalf("partial map: exit %d msg %s, want 1 naming the missing records", code, msg)
	}

	// A missing project without --create-projects fails and stores nothing.
	db = filepath.Join(t.TempDir(), "castle.db")
	code, msg = try(importArgs(db))
	if code != 1 || !strings.Contains(msg, "not found") {
		t.Fatalf("missing project: exit %d msg %s, want 1 not-found", code, msg)
	}
	st, err := sqlite.Open(db)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	ctx := context.Background()
	for _, id := range projectSlugs {
		if _, _, err := st.GetProjectConfig(ctx, id); err == nil {
			t.Fatalf("failed import left a config record for %s", id)
		}
	}
	if project, err := st.GetProject(ctx, "typescript-sdk"); err == nil {
		t.Fatalf("failed import created %s: %+v", project.ID, project)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	// A binding payload bound to an unknown namespace fails naming it.
	unknownBinding := importArgs(filepath.Join(t.TempDir(), "castle.db"), "--create-projects")
	for i := 0; i < len(unknownBinding)-1; i++ {
		if unknownBinding[i] == "--binding" && strings.Contains(unknownBinding[i+1], "criteria-go-dev") {
			unknownBinding[i+1] = "unknown-ns=" + filepath.Join("testdata", "binding-go-dev.json")
			break
		}
	}
	code, msg = try(unknownBinding)
	if code != 1 || !strings.Contains(msg, "unknown-ns") {
		t.Fatalf("unknown binding namespace: exit %d msg %s, want 1 naming ns", code, msg)
	}
}

// TestConfigImportConflictsOnSecondImport pins the operator-resolution
// contract: on a re-import, every target that already holds a record
// conflicts in the pre-flight — before any write — and the failure
// names the projects (import never overwrites; the operator clears
// stale records explicitly via SetProjectConfig with the empty
// document to re-import).
func TestConfigImportConflictsOnSecondImport(t *testing.T) {
	db := filepath.Join(t.TempDir(), "castle.db")
	var stdout, stderr bytes.Buffer
	if code := run(importArgs(db, "--create-projects"), &stdout, &stderr); code != 0 {
		t.Fatalf("first import: exit %d stderr %s", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	code := run(importArgs(db, "--create-projects"), &stdout, &stderr)
	// The all-or-nothing pre-flight catches every already-imported
	// target BEFORE any write, listing the project slugs in
	// projRecords' registry-name-sorted order ("Criteria Go Dev" sorts
	// before "Criteria K8s Workflow Runner").
	if code != 1 || !strings.Contains(stderr.String(), "already imported for: criteria-go-dev, criteria-k8s-workflow-runner, kanboard-tickets, typescript-sdk") {
		t.Fatalf("second import: exit %d stderr %s, want pre-flight conflict failure naming the projects", code, stderr.String())
	}
}
