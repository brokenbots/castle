package main

import (
	"bytes"
	"context"
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

// TestConfigImportCutoverFlip pins the castle-authoritative flip: with
// --cutover every verified record marks cutover=done after the round-trip
// checks, and the flip itself re-validates.
func TestConfigImportCutoverFlip(t *testing.T) {
	st, _ := tempImport(t, "--create-projects", "--cutover")
	ctx := context.Background()
	for _, id := range projectSlugs {
		doc, etag, err := st.GetProjectConfig(ctx, id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		record, err := confighome.ParseRecord(doc)
		if err != nil {
			t.Fatalf("parse %s: %v", id, err)
		}
		if record.Cutover != confighome.CutoverDone {
			t.Fatalf("%s cutover = %q, want done", id, record.Cutover)
		}
		if etag == "" {
			t.Fatalf("%s stored an empty etag", id)
		}
	}
}

// TestConfigImportFailsClosed pins the operational guards: usage errors
// exit 2, missing mappings and missing projects fail before any write,
// foreign binding namespaces name the namespace, and a failed import
// leaves NO config records on the store.
func TestConfigImportFailsClosed(t *testing.T) {
	run := func(args []string) (code int, stderr string) {
		t.Helper()
		var errOut bytes.Buffer
		return run(args, &bytes.Buffer{}, &errOut), errOut.String()
	}

	// Usage errors: a missing routes source and a malformed pair shape.
	db := filepath.Join(t.TempDir(), "castle.db")
	code, msg := run([]string{"--platform", "Criteria K8s Workflow Runner", "--db", db})
	if code != 2 || !strings.Contains(msg, "--routes-file") {
		t.Fatalf("missing routes: exit %d msg %s, want 2 naming --routes-file", code, msg)
	}
	code, msg = run(importArgs(db)[:0])
	if code != 2 {
		t.Fatalf("empty args: exit %d, want 2", code)
	}

	// Mapping completeness failure: only one of four records mapped.
	partial := []string{
		"--routes-file", filepath.Join("testdata", "routes.json"),
		"--platform", "Criteria K8s Workflow Runner",
		"--db", filepath.Join(t.TempDir(), "castle.db"),
		"--map", "Criteria K8s Workflow Runner=criteria-k8s-workflow-runner",
	}
	code, msg = run(partial)
	if code != 1 || !strings.Contains(msg, "no --map entry for") || !strings.Contains(msg, "Kanboard Tickets") {
		t.Fatalf("partial map: exit %d msg %s, want 1 naming the missing records", code, msg)
	}

	// A missing project without --create-projects fails and stores nothing.
	db = filepath.Join(t.TempDir(), "castle.db")
	code, msg = run(importArgs(db))
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
	code, msg = run(unknownBinding)
	if code != 1 || !strings.Contains(msg, "unknown-ns") {
		t.Fatalf("unknown binding namespace: exit %d msg %s, want 1 naming ns", code, msg)
	}
}

// TestConfigImportConflictsOnSecondImport pins the operator-resolution
// contract: a project that ALREADY holds a record conflicts on a
// re-import with the empty expected etag, and the failure names the
// project (import never overwrites; the operator resolves conflicts).
func TestConfigImportConflictsOnSecondImport(t *testing.T) {
	db := filepath.Join(t.TempDir(), "castle.db")
	var stdout, stderr bytes.Buffer
	if code := run(importArgs(db, "--create-projects"), &stdout, &stderr); code != 0 {
		t.Fatalf("first import: exit %d stderr %s", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	code := run(importArgs(db, "--create-projects"), &stdout, &stderr)
	// projRecords iterate in registry-name-sorted order, so the first
	// conflicting write is Criteria Go Dev's project.
	if code != 1 || !strings.Contains(stderr.String(), "criteria-go-dev") {
		t.Fatalf("second import: exit %d stderr %s, want conflict failure naming the project", code, stderr.String())
	}
}
