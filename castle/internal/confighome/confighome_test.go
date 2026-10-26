package confighome

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read testdata/%s: %v", name, err)
	}
	return data
}

func partitionFixture(t *testing.T) (map[string]*Record, []byte) {
	t.Helper()
	payload := readFixture(t, "routes.json")
	records, err := Partition("Criteria K8s Workflow Runner", payload)
	if err != nil {
		t.Fatalf("Partition: %v", err)
	}
	return records, payload
}

func testAssembly(t *testing.T, records map[string]*Record) *Assembly {
	t.Helper()
	names := make([]string, 0, len(records))
	for name := range records {
		names = append(names, name)
	}
	slices.Sort(names)
	var prs []projectRecord
	for _, name := range names {
		prs = append(prs, projectRecord{projectID: name, record: records[name]})
	}
	return NewAssembly(prs)
}

// TestPartitionRenderRoundTrip proves the import path's no-silent-shape-
// change guarantee: partitioning the live routes payload into per-project
// records and rendering the records back must reproduce the live payload
// with checksum equality.
func TestPartitionRenderRoundTrip(t *testing.T) {
	records, live := partitionFixture(t)

	if len(records) != 4 {
		t.Fatalf("Partition produced %d records, want 4: %v", len(records), names(records))
	}

	// The routes payload's array order is rebuilt from the records'
	// first-route ordinals.
	a := testAssembly(t, records)
	rendered, err := a.RenderRoutesPayload()
	if err != nil {
		t.Fatalf("RenderRoutesPayload: %v", err)
	}
	equal, checksum, err := EqualPayload(live, rendered)
	if err != nil {
		t.Fatalf("EqualPayload: %v", err)
	}
	if !equal {
		t.Fatalf("rendered routes payload diverges from live payload (checksum %s)", checksum)
	}

	// Deterministic: rendering again reproduces the same checksum.
	rendered2, err := a.RenderRoutesPayload()
	if err != nil {
		t.Fatalf("RenderRoutesPayload again: %v", err)
	}
	if _, checksum2, err := EqualPayload(live, rendered2); err != nil || checksum != checksum2 {
		t.Fatalf("rendering is not deterministic: %q vs %q (%v)", checksum, checksum2, err)
	}

	// Route order: the payload array's first-match-wins order is rebuilt.
	doc, err := a.Assemble()
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	want := []string{"kanboard-triage", "kanboard-develop", "kanboard-develop-go", "kanboard-develop-ts",
		"harness-contracts", "harness-composition", "harness-failures", "harness-fleet-smoke", "harness-wire"}
	got := make([]string, 0, len(doc.Routes))
	for _, rt := range doc.Routes {
		got = append(got, rt.Name)
	}
	if !slices.Equal(want, got) {
		t.Fatalf("rendered route order is %v, want the live payload order %v", got, want)
	}

	// The platform overlay configLibrary entries land on the platform
	// record; the routed records carry only their own data.
	platform := records["Criteria K8s Workflow Runner"]
	if _, ok := platform.ConfigLibrary["castle"]; !ok {
		t.Fatalf("platform record must own the 'castle' configLibrary overlay, owns %v", keys(platform.ConfigLibrary))
	}
	if _, ok := platform.ConfigLibrary["workflow-example"]; !ok {
		t.Fatalf("platform record must own the 'workflow-example' configLibrary overlay")
	}
	if got := records["Kanboard Tickets"].ConfigLibrary; len(got) != 0 {
		t.Fatalf("Kanboard Tickets owns configLibrary entries %v, want none", keys(got))
	}

	// Each non-platform record catalogs exactly what its routes reference.
	goDev := records["Criteria Go Dev"]
	if _, ok := goDev.Catalog["kanboard-develop-url"]; !ok {
		t.Fatalf("go-dev record must catalog kanboard-develop-url")
	}
	if len(goDev.Catalog) != 1 {
		t.Fatalf("go-dev record catalogs %v", keys(goDev.Catalog))
	}
}

func names(records map[string]*Record) []string {
	n := make([]string, 0, len(records))
	for name := range records {
		n = append(n, name)
	}
	slices.Sort(n)
	return n
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for name := range m {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

// TestBindingImportRoundTrip proves the namespace binding payload round
// trips through the record with checksum equality — binding-derived
// execution detail (images/env/volumes/secrets/config) is project data
// the record owns.
func TestBindingImportRoundTrip(t *testing.T) {
	for _, fixture := range []string{"binding-go-dev.json", "binding-typescript-sdk.json"} {
		t.Run(fixture, func(t *testing.T) {
			live := readFixture(t, fixture)
			binding, err := ParseBinding(live)
			if err != nil {
				t.Fatalf("ParseBinding: %v", err)
			}
			raw, err := json.Marshal(binding)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			rendered, err := Canonicalize(raw)
			if err != nil {
				t.Fatalf("canonicalize rendered binding: %v", err)
			}
			equal, checksum, err := EqualPayload(live, rendered)
			if err != nil {
				t.Fatalf("EqualPayload: %v", err)
			}
			if !equal {
				t.Fatalf("rendered binding diverges from live payload (checksum %s)", checksum)
			}
			if len(checksum) != 64 {
				t.Fatalf("checksum is not sha256 hex: %q", checksum)
			}
		})
	}
}

// TestRecordStoreRoundTrip proves the store doc form round trips:
// Marshal (canonical bytes) -> ParseRecord -> Marshal is the identity,
// and validation rides both directions.
func TestRecordStoreRoundTrip(t *testing.T) {
	records, _ := partitionFixture(t)
	for name, record := range records {
		t.Run(name, func(t *testing.T) {
			doc, err := record.Marshal()
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			parsed, err := ParseRecord(doc)
			if err != nil {
				t.Fatalf("ParseRecord: %v", err)
			}
			doc2, err := parsed.Marshal()
			if err != nil {
				t.Fatalf("re-Marshal: %v", err)
			}
			if string(doc) != string(doc2) {
				t.Fatalf("store doc round trip is not the identity:\n%s\n%s", doc, string(doc2))
			}
		})
	}
}

// TestParseRecordRejectsUnknownFields enforces the fail-closed shape
// discipline: a record doc with an unknown field is rejected, so schema
// drift cannot enter the store silently.
func TestParseRecordRejectsUnknownFields(t *testing.T) {
	records, _ := partitionFixture(t)
	record := records["Criteria Go Dev"]
	doc, err := record.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	probe := make([]byte, 0, len(doc)+16)
	probe = append(probe, doc[:len(doc)-1]...)
	probe = append(probe, []byte(`,"stray":"oops"}`)...)
	if _, err := ParseRecord(probe); err == nil {
		t.Fatalf("unknown top-level field accepted")
	}
	if _, err := ParseRecord(doc); err != nil {
		t.Fatalf("control record rejected: %v", err)
	}
}

// TestPartitionMaterializesDefaultStates proves the CRI-218 states
// default materializes at import: a route that omits its states list
// stores [DefaultState].
func TestPartitionMaterializesDefaultStates(t *testing.T) {
	payload := []byte(`{
		"apiVersion": "criteria.brokenbots.dev/v1",
		"kind": "Routes",
		"workflowLibrary": {
			"wf": {"type": "image", "image": "registry.local/wf:v1"}
		},
		"configLibrary": {},
		"projectRegistry": {"P": {"namespace": "proj-ns"}},
		"routes": [
			{"name": "r1", "workflow": "wf", "project": "P"},
			{"name": "r2", "workflow": "wf", "project": "P", "states": ["Ready"]}
		]
	}`)
	records, err := Partition("P", payload)
	if err != nil {
		t.Fatalf("Partition: %v", err)
	}
	if got := records["P"].Routes[0].States; !slices.Equal(got, []string{DefaultState}) {
		t.Fatalf("absent states must materialize %v, got %v", []string{DefaultState}, got)
	}
	if got := records["P"].Routes[1].States; !slices.Equal(got, []string{"Ready"}) {
		t.Fatalf("declared states must stay, got %v", got)
	}
}

// TestPartitionRejectsEmptyStatesList: an explicitly empty states list
// is a config error, not a default — records reject it fail-closed.
func TestPartitionRejectsEmptyStatesList(t *testing.T) {
	payload := []byte(`{
		"apiVersion": "criteria.brokenbots.dev/v1",
		"kind": "Routes",
		"workflowLibrary": {
			"wf": {"type": "image", "image": "registry.local/wf:v1"}
		},
		"configLibrary": {},
		"projectRegistry": {"P": {"namespace": "proj-ns"}},
		"routes": [{"name": "r1", "workflow": "wf", "project": "P", "states": []}]
	}`)
	if _, err := Partition("P", payload); err == nil || !strings.Contains(err.Error(), "states must not be empty") {
		t.Fatalf("empty states list must reject the import, got %v", err)
	}
}

// TestPartitionRejectsUnknownFields: the imported payload's decode is
// DisallowUnknownFields everywhere — a drifted live payload imports
// nowhere.
func TestPartitionRejectsUnknownFields(t *testing.T) {
	payload := []byte(`{
		"apiVersion": "criteria.brokenbots.dev/v1",
		"kind": "Routes",
		"workflowLibrary": {"wf": {"type": "image", "image": "registry.local/wf:v1", "stray": 1}},
		"configLibrary": {},
		"projectRegistry": {"P": {"namespace": "proj-ns"}},
		"routes": [{"name": "r1", "workflow": "wf", "project": "P", "states": ["Backlog"]}]
	}`)
	if _, err := Partition("P", payload); err == nil {
		t.Fatalf("unknown workflowLibrary field accepted")
	}
	drifted := strings.Replace(string(payload), `"projectRegistry":`, `"projectRegistrations":`, 1)
	if _, err := Partition("P", []byte(drifted)); err == nil {
		t.Fatalf("renamed top-level field accepted")
	}
}

// TestPartitionPreservesUnreferencedCatalog: workflowLibrary entries no
// route references are still part of the payload — they land on the
// platform record's catalog and must render back, or the import would
// silently drop platform data.
func TestPartitionPreservesUnreferencedCatalog(t *testing.T) {
	records, live := partitionFixture(t)
	if _, ok := records["Criteria K8s Workflow Runner"].Catalog["decision-demo-url"]; !ok {
		t.Fatalf("unreferenced workflowLibrary entry must land on the platform record's catalog")
	}
	a := testAssembly(t, records)
	rendered, err := a.RenderRoutesPayload()
	if err != nil {
		t.Fatalf("RenderRoutesPayload: %v", err)
	}
	if equal, _, err := EqualPayload(live, rendered); err != nil || !equal {
		t.Fatalf("rendered routes payload diverges from live payload: %v", err)
	}
}

// shapeProbe builds a valid minimal record with a url workflow, an image
// workflow, a binding-less route pointing at the url workflow, and a
// config entry the url workflow's interface fully declares.
func shapeProbe() *Record {
	return &Record{
		APIVersion:   APIVersion,
		Kind:         KindRecord,
		RegistryName: "shape-proj",
		Namespace:    "criteria-jobs",
		Catalog: map[string]Workflow{
			"url-wf": {
				Type: TypeURL,
				URL:  "https://example.com/tree",
				Ref:  "refs/heads/main",
				Interface: &Interface{
					APIVersion: APIVersion,
					Kind:       KindInterface,
					Workflow:   "url-wf",
					Adapters:   []string{},
					Mounts:     []Mount{},
					SecretEnv:  []string{},
					Env:        []string{},
					ConfigSlots: []ConfigSlot{
						{Slot: "buildCmd", Variable: "build_cmd", Target: "step:build"},
					},
				},
			},
			"image-wf": {
				Type:    TypeImage,
				Image:   "registry.example.com/runner:v1",
				Volumes: []Volume{},
				Secrets: []Secret{},
				Env:     map[string]string{},
			},
		},
		ConfigLibrary: map[string]ConfigEntry{
			"build-config": {
				BuildCmd: "go build ./...",
				Tags:     []string{},
			},
		},
		Routes: []Route{
			{Name: "shape-route", Project: "shape-proj", Workflow: "url-wf", ConfigRef: "build-config", States: []string{DefaultState}},
			{Name: "image-route", Project: "shape-proj", Workflow: "image-wf", States: []string{DefaultState}},
		},
	}
}

// TestRecordValidateShapeRejects pins the fail-closed shape negatives the
// live fixture exercises only implicitly: one mutation per case against an
// otherwise valid record must reject.
func TestRecordValidateShapeRejects(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(p *Record)
	}{
		{"bad namespace label", func(p *Record) { p.Namespace = "Bad_NS!" }},
		{"unknown volume kind", func(p *Record) {
			c := shapeProbe()
			wf := c.Catalog["image-wf"]
			wf.Volumes = []Volume{{Name: "scratch", MountPath: "/scratch", Kind: "ebs", SizeLimit: "1Gi"}}
			c.Catalog["image-wf"] = wf
			*p = *c
		}},
		{"duplicate route names", func(p *Record) {
			p.Routes[1] = p.Routes[0]
		}},
		{"url workflow missing interface", func(p *Record) {
			wf := p.Catalog["url-wf"]
			wf.Interface = nil
			p.Catalog["url-wf"] = wf
		}},
		{"image workflow declaring url", func(p *Record) {
			wf := p.Catalog["image-wf"]
			wf.URL = "https://example.com/tree"
			p.Catalog["image-wf"] = wf
		}},
		{"image workflow declaring interface", func(p *Record) {
			wf := p.Catalog["image-wf"]
			wf.Interface = &Interface{APIVersion: APIVersion, Kind: KindInterface, Workflow: "image-wf"}
			p.Catalog["image-wf"] = wf
		}},
		{"non-platform config slot", func(p *Record) {
			p.Catalog["url-wf"].Interface.ConfigSlots = append(
				p.Catalog["url-wf"].Interface.ConfigSlots,
				ConfigSlot{Slot: "customSlot", Variable: "custom_slot", Target: "step:build"})
		}},
		{"slot variable mismatch", func(p *Record) {
			p.Catalog["url-wf"].Interface.ConfigSlots[0].Variable = "test_cmd"
		}},
		{"duplicate config slot", func(p *Record) {
			p.Catalog["url-wf"].Interface.ConfigSlots = append(
				p.Catalog["url-wf"].Interface.ConfigSlots,
				ConfigSlot{Slot: "buildCmd", Variable: "build_cmd", Target: "step:test"})
		}},
		{"route config outside declared slots", func(p *Record) {
			// The route's configRef declares a command the workflow's
			// interface does not declare (KB-339 item 3).
			p.Catalog["url-wf"].Interface.ConfigSlots = []ConfigSlot{
				{Slot: "testCmd", Variable: "test_cmd", Target: "step:test"},
			}
		}},
		{"route pointing outside catalog", func(p *Record) {
			p.Catalog["gone"] = p.Catalog["url-wf"]
			delete(p.Catalog, "url-wf")
		}},
		{"configRef outside configLibrary", func(p *Record) {
			delete(p.ConfigLibrary, "build-config")
		}},
		{"route project mismatch", func(p *Record) {
			p.Routes[0].Project = "other-project"
		}},
		{"unknown image conflict policy", func(p *Record) {
			p.ImageConflictPolicy = "prefer-magic"
		}},
		{"unknown cutover", func(p *Record) {
			p.Cutover = "binding-second"
		}},
		{"unknown digest", func(p *Record) {
			p.Digests = map[string]string{"url-wf": "nothex"}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			probe := shapeProbe()
			tc.mutate(probe)
			if err := probe.Validate(); err == nil {
				t.Fatalf("mutated record accepted")
			}
		})
	}
	// The untouched probe stays valid, so negatives fail because of the
	// mutation and not the baseline.
	if err := shapeProbe().Validate(); err != nil {
		t.Fatalf("baseline probe rejected: %v", err)
	}
}

// TestPartitionConfigRefOwnership: a configLibrary entry a route
// references owns that route's record, entries no route references are
// platform overlay data, and a configRef shared across projects'
// records fails the import closed — the non-owner record's validation
// rejects it instead of silently rendering a record that cannot
// resolve its own reference.
func TestPartitionConfigRefOwnership(t *testing.T) {
	payload := []byte(`{
		"apiVersion": "criteria.brokenbots.dev/v1",
		"kind": "Routes",
		"workflowLibrary": {
			"wf-i": {"type": "image", "image": "registry.local/wf-i:v1"},
			"wf-p": {"type": "image", "image": "registry.local/wf-p:v1"}
		},
		"configLibrary": {"cfg-x": {"buildCmd": "make build"}},
		"projectRegistry": {"P1": {"namespace": "proj-ns-1"}, "P2": {"namespace": "proj-ns-2"}},
		"routes": [
			{"name": "r-p1", "workflow": "wf-i", "project": "P1", "states": ["Ready"], "configRef": "cfg-x"}
		]
	}`)
	records, err := Partition("P2", payload)
	if err != nil {
		t.Fatalf("Partition: %v", err)
	}
	if got := records["P1"].ConfigLibrary; len(got) != 1 || got["cfg-x"].BuildCmd != "make build" {
		t.Fatalf("route-owned configLibrary entry must live on the referencing record, P1 owns %v", keys(got))
	}
	if got := records["P2"].ConfigLibrary; len(got) != 0 {
		t.Fatalf("referenced configLibrary entry must not land on the platform record, P2 owns %v", keys(got))
	}
	rendered, err := testAssembly(t, records).RenderRoutesPayload()
	if err != nil {
		t.Fatalf("RenderRoutesPayload: %v", err)
	}
	if equal, _, err := EqualPayload(payload, rendered); err != nil || !equal {
		t.Fatalf("configRef route does not round-trip: %v (equal %v)", err, equal)
	}

	// Cross-project sharing: the first referencing route owns the
	// entry, so a second project's record cannot resolve it — refused
	// at partition time instead of a silent divergence.
	shared := []byte(`{
		"apiVersion": "criteria.brokenbots.dev/v1",
		"kind": "Routes",
		"workflowLibrary": {
			"wf-i": {"type": "image", "image": "registry.local/wf-i:v1"},
			"wf-p": {"type": "image", "image": "registry.local/wf-p:v1"}
		},
		"configLibrary": {"cfg-x": {"buildCmd": "make build"}},
		"projectRegistry": {"P1": {"namespace": "proj-ns-1"}, "P2": {"namespace": "proj-ns-2"}},
		"routes": [
			{"name": "r-p1", "workflow": "wf-i", "project": "P1", "states": ["Ready"], "configRef": "cfg-x"},
			{"name": "r-p2", "workflow": "wf-p", "project": "P2", "states": ["Ready"], "configRef": "cfg-x"}
		]
	}`)
	if _, err := Partition("P2", shared); err == nil || !strings.Contains(err.Error(), "cfg-x") {
		t.Fatalf("cross-project shared configRef must fail closed, got %v", err)
	}
}

// TestRecordMarkCutoverDone pins the structural castle-authoritative
// flip (KB-313, ADR 307): the flip is binding-first — a record with no
// namespace binding refuses — and a flipped record loses the deprecated
// global run-config surfaces so the flipped records re-assemble and
// render post-cutover.
func TestRecordMarkCutoverDone(t *testing.T) {
	records, _ := partitionFixture(t)
	kanboard := records["Kanboard Tickets"]
	if err := kanboard.MarkCutoverDone(); err == nil || !strings.Contains(err.Error(), "binding-first") {
		t.Fatalf("unbound record must refuse the flip, got %v", err)
	}
	godev := records["Criteria Go Dev"]
	stale, ok := godev.Catalog["kanboard-develop-url"]
	if !ok || len(stale.Volumes) == 0 || len(stale.AdapterImages) == 0 {
		t.Fatalf("fixture's go-dev catalog entry must carry the stale global surfaces pre-flip")
	}
	// The fixture's registry carries some pre-cut entries; reset the
	// probe so the flip itself moves the state.
	for _, rec := range records {
		if rec.Cutover == CutoverDone {
			rec.Cutover = ""
		}
		rec.Binding = &Binding{APIVersion: APIVersion, Kind: KindBinding}
		if err := rec.Validate(); err != nil {
			t.Fatalf("binding attachment must not break the record: %v", err)
		}
	}
	// Every record flips its own copies of shared entries too, so the
	// stripped copies stay equal (workflowsEqual) and the assembly is a
	// valid post-cutover payload with no stale surfaces. (A PARTIAL
	// flip over shared trees cannot render — the import tool refuses
	// that state pre-write; see the cmd tests.)
	for _, rec := range records {
		if err := rec.MarkCutoverDone(); err != nil {
			t.Fatalf("MarkCutoverDone %q: %v", rec.RegistryName, err)
		}
	}
	flipped := godev.Catalog["kanboard-develop-url"]
	if godev.Cutover != CutoverDone || len(flipped.Volumes) != 0 || len(flipped.Secrets) != 0 ||
		len(flipped.Env) != 0 || len(flipped.AdapterImages) != 0 {
		t.Fatalf("flipped record must be done and stripped, got %q %+v", godev.Cutover, flipped)
	}
	if err := godev.Validate(); err != nil {
		t.Fatalf("flipped record must still validate: %v", err)
	}
	if _, err := testAssembly(t, records).RenderRoutesPayload(); err != nil {
		t.Fatalf("flipped assembly must render post-cutover: %v", err)
	}

	// A non-conflicting MIXED state stays valid mid-migration: a
	// flipped record whose trees no un-flipped record shares renders
	// alongside pre-cut stale entries (allow-with-warning window).
	mixedGodev := godev
	mixedKanboard := &Record{
		APIVersion: APIVersion, Kind: KindRecord,
		RegistryName: "Kanboard Tickets", Namespace: "criteria-jobs",
		Catalog: map[string]Workflow{"kb-only": {Type: TypeImage, Image: "registry.local/kb-only:v1"}},
		Routes:  []Route{{Name: "kb-route", Workflow: "kb-only", Project: "Kanboard Tickets", States: []string{"Ready"}}},
	}
	if _, err := NewAssemblyFor([]ProjectRecord{
		{ProjectID: "criteria-go-dev", Record: mixedGodev},
		{ProjectID: "kanboard-tickets", Record: mixedKanboard},
	}).RenderRoutesPayload(); err != nil {
		t.Fatalf("mixed pre/post-cutover assembly must render in the migration window: %v", err)
	}
}
