package confighome

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// routesPayloadDoc mirrors the assembled routes payload: the shape the
// criteria-k8s operator parses. Records contribute their slices; the
// assembly union is idempotent on equal content and fails closed on
// conflicts.
type routesPayloadDoc struct {
	APIVersion      string                   `json:"apiVersion"`
	Kind            string                   `json:"kind"`
	WorkflowLibrary map[string]Workflow      `json:"workflowLibrary"`
	ConfigLibrary   map[string]ConfigEntry   `json:"configLibrary"`
	ProjectRegistry map[string]RegistryEntry `json:"projectRegistry"`
	Routes          []Route                  `json:"routes"`
}

// RegistryEntry is the payload's projectRegistry entry shape (KB-311,
// KB-313).
type RegistryEntry struct {
	Namespace           string   `json:"namespace"`
	Repos               []string `json:"repos,omitempty"`
	Cutover             string   `json:"cutover,omitempty"`
	ImageConflictPolicy string   `json:"imageConflictPolicy,omitempty"`
}

// Assembly is the ordered collection of project records that renders
// the platform routes payload (and each record's binding payload):
// castle is the single writer, ConfigMaps render FROM castle records.
type Assembly struct {
	// records sorted by RoutesOrdinal then project id.
	order []ordered
}

type ordered struct {
	projectID string
	record    *Record
}

// ProjectRecord pairs a castle project id with its config record — the
// exported bridge the KB-339 config import path uses to construct an
// assembly outside the package.
type ProjectRecord struct {
	ProjectID string
	Record    *Record
}

// NewAssemblyFor orders the paired records the way NewAssembly orders
// plain records (RoutesOrdinal, then project id).
func NewAssemblyFor(records []ProjectRecord) *Assembly {
	inner := make([]projectRecord, len(records))
	for i, pr := range records {
		inner[i] = projectRecord{projectID: pr.ProjectID, record: pr.Record}
	}
	return NewAssembly(inner)
}

// NewAssembly orders the records for rendering: by RoutesOrdinal (the
// imported payload position) then project id (records post-import
// append deterministically at the end).
func NewAssembly(records []projectRecord) *Assembly {
	a := &Assembly{}
	for _, pr := range records {
		a.order = append(a.order, ordered{projectID: pr.projectID, record: pr.record})
	}
	slices.SortStableFunc(a.order, func(x, y ordered) int {
		if c := x.record.RoutesOrdinal - y.record.RoutesOrdinal; c != 0 {
			return c
		}
		return strings.Compare(x.projectID, y.projectID)
	})
	return a
}

type projectRecord struct {
	projectID string
	record    *Record
}

// validateProjectRecord applies the record validator with the project
// id as context.
func validateProjectRecord(pr ordered) error {
	if pr.record == nil {
		return fmt.Errorf("project %q: nil record", pr.projectID)
	}
	if pr.projectID == "" {
		return fmt.Errorf("project %q: empty project id", pr.record.RegistryName)
	}
	return pr.record.Validate()
}

// RenderRoutesPayload assembles the records into the routes payload
// bytes (canonical form). Conflicting duplicate content across records
// fails closed; identical duplicate content (catalog objects shared
// between projects that route the same tree) merges idempotently.
func (a *Assembly) RenderRoutesPayload() ([]byte, error) {
	doc, err := a.Assemble()
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	return Canonicalize(raw)
}

// Assemble builds the payload document and enforces the assembled
// payload's integrity: workflow/config library reference resolution,
// route-name uniqueness, and project resolution.
func (a *Assembly) Assemble() (*routesPayloadDoc, error) {
	for _, pr := range a.order {
		if err := validateProjectRecord(pr); err != nil {
			return nil, fmt.Errorf("project %q: %w", pr.projectID, err)
		}
	}
	doc := &routesPayloadDoc{
		APIVersion:      APIVersion,
		Kind:            KindRoutes,
		WorkflowLibrary: map[string]Workflow{},
		ConfigLibrary:   map[string]ConfigEntry{},
		ProjectRegistry: map[string]RegistryEntry{},
	}
	var routes []Route
	for _, pr := range a.order {
		r := pr.record
		if entry, ok := doc.ProjectRegistry[r.RegistryName]; ok {
			if err := requireEqualRegistry(entry, r); err != nil {
				return nil, fmt.Errorf("project %q registryName %q: %w", pr.projectID, r.RegistryName, err)
			}
		} else {
			doc.ProjectRegistry[r.RegistryName] = RegistryEntry{
				Namespace:           r.Namespace,
				Repos:               append([]string(nil), r.Repos...),
				Cutover:             r.Cutover,
				ImageConflictPolicy: r.ImageConflictPolicy,
			}
		}
		for name, wf := range r.Catalog {
			if existing, ok := doc.WorkflowLibrary[name]; ok {
				if !workflowsEqual(existing, wf) {
					return nil, fmt.Errorf("project %q: catalog %q conflicts with another project's entry of the same name", pr.projectID, name)
				}
				continue
			}
			doc.WorkflowLibrary[name] = wf
		}
		for name, entry := range r.ConfigLibrary {
			if existing, ok := doc.ConfigLibrary[name]; ok {
				if !configEntriesEqual(existing, entry) {
					return nil, fmt.Errorf("project %q: configLibrary %q conflicts with another project's entry of the same name", pr.projectID, name)
				}
				continue
			}
			doc.ConfigLibrary[name] = entry
		}
		routes = append(routes, r.Routes...)
	}
	doc.Routes = routes
	if err := validateAssembledPayload(doc); err != nil {
		return nil, err
	}
	return doc, nil
}

// MarkCutoverDone flips a record to its post-cutover state (KB-313,
// ADR 307): the namespace serves its run config from its own binding
// payload from here on. The flip is binding-first — a record with no
// namespace binding cannot go post-cutover (its namespace would lose
// the only run config it ever had) — and the full record re-validates.
// The catalog no longer carries the global run-config surfaces at all
// (KB-313 promise executed), so the flip is a marker flip, not a
// content edit.
func (r *Record) MarkCutoverDone() error {
	if r.Binding == nil {
		return fmt.Errorf("record %q: binding-first cutover requires a namespace binding (KB-313, ADR 307)", r.RegistryName)
	}
	r.Cutover = CutoverDone
	return r.Validate()
}

// validateAssembledPayload enforces the assembled payload's integrity —
// the shape rules the criteria-k8s operator applies to the ConfigMap:
// non-empty libraries, route-workflow/configRef/project resolution, and
// global route-name uniqueness.
func validateAssembledPayload(doc *routesPayloadDoc) error {
	if len(doc.WorkflowLibrary) == 0 {
		return fmt.Errorf("workflowLibrary must not be empty")
	}
	if len(doc.Routes) == 0 {
		return fmt.Errorf("routes must not be empty")
	}
	seenRoutes := make(map[string]bool, len(doc.Routes))
	for i, rt := range doc.Routes {
		if seenRoutes[rt.Name] {
			return fmt.Errorf("routes[%d].name %q is not unique across projects", i, rt.Name)
		}
		seenRoutes[rt.Name] = true
		if _, ok := doc.WorkflowLibrary[rt.Workflow]; !ok {
			return fmt.Errorf("routes[%d] (%s): workflow %q is not present in workflowLibrary", i, rt.Name, rt.Workflow)
		}
		if rt.ConfigRef != "" {
			if _, ok := doc.ConfigLibrary[rt.ConfigRef]; !ok {
				return fmt.Errorf("routes[%d] (%s): configRef %q is not present in configLibrary", i, rt.Name, rt.ConfigRef)
			}
		}
		if rt.Project == "" {
			return fmt.Errorf("routes[%d] (%s): project is required", i, rt.Name)
		}
		if _, ok := doc.ProjectRegistry[rt.Project]; !ok {
			return fmt.Errorf("routes[%d] (%s): project %q is not present in projectRegistry", i, rt.Name, rt.Project)
		}
	}
	return nil
}

func requireEqualRegistry(entry RegistryEntry, r *Record) error {
	want := RegistryEntry{Namespace: r.Namespace, Repos: append([]string(nil), r.Repos...), Cutover: r.Cutover, ImageConflictPolicy: r.ImageConflictPolicy}
	if entry.Namespace != want.Namespace || entry.Cutover != want.Cutover || entry.ImageConflictPolicy != want.ImageConflictPolicy || !slices.Equal(entry.Repos, want.Repos) {
		return fmt.Errorf("registry entry conflicts with another project's entry (one registry per project name)")
	}
	return nil
}

func workflowsEqual(a, b Workflow) bool {
	if a.Type != b.Type || a.Namespace != b.Namespace || a.Class != b.Class || a.Image != b.Image ||
		a.URL != b.URL || a.Ref != b.Ref || a.ImageConflictPolicy != b.ImageConflictPolicy || a.Interface != nil != (b.Interface != nil) {
		return false
	}
	if a.Interface == nil {
		return true
	}
	return interfacesEqual(a.Interface, b.Interface)
}

func interfacesEqual(a, b *Interface) bool {
	if a.APIVersion != b.APIVersion || a.Kind != b.Kind || a.Workflow != b.Workflow || a.Description != b.Description || a.LockKey != b.LockKey {
		return false
	}
	if !slices.Equal(a.Adapters, b.Adapters) || !slices.Equal(a.SecretEnv, b.SecretEnv) || !slices.Equal(a.Env, b.Env) {
		return false
	}
	if len(a.Mounts) != len(b.Mounts) || len(a.ConfigSlots) != len(b.ConfigSlots) {
		return false
	}
	for i := range a.Mounts {
		if a.Mounts[i] != b.Mounts[i] {
			return false
		}
	}
	for i := range a.ConfigSlots {
		if a.ConfigSlots[i].Slot != b.ConfigSlots[i].Slot || a.ConfigSlots[i].Variable != b.ConfigSlots[i].Variable ||
			a.ConfigSlots[i].Target != b.ConfigSlots[i].Target || a.ConfigSlots[i].Description != b.ConfigSlots[i].Description {
			return false
		}
	}
	return true
}

func configEntriesEqual(a, b ConfigEntry) bool {
	return a.BuildCmd == b.BuildCmd && a.TestCmd == b.TestCmd && a.CIGateCmd == b.CIGateCmd &&
		a.BaseBranch == b.BaseBranch && a.TagMatch == b.TagMatch && slices.Equal(a.Tags, b.Tags)
}

// RenderBinding renders the record's binding payload (the project
// namespace's binding.json content) in canonical form.
func (r *Record) RenderBinding() ([]byte, error) {
	if r.Binding == nil {
		return nil, fmt.Errorf("project %q has no binding record", r.RegistryName)
	}
	raw, err := json.Marshal(r.Binding)
	if err != nil {
		return nil, err
	}
	return Canonicalize(raw)
}

// RenderBinding renders one project's binding.json content.
func (a *Assembly) RenderBinding(projectID string) ([]byte, error) {
	for _, pr := range a.order {
		if pr.projectID == projectID {
			return pr.record.RenderBinding()
		}
	}
	return nil, fmt.Errorf("project %q: no config record", projectID)
}
