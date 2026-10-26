// Package confighome holds castle's per-project workflow configuration
// records (KB-339, OPTION B P0.5a): the project config home where
// config lives IN castle and the ConfigMaps keep rendering FROM castle.
// The record re-shapes the routes ConfigMap payload's
// workflowLibrary/configLibrary/projectRegistry sections and each
// namespace's criteria-binding payload as project data; the
// criteria-k8s payload shapes (k8s/routes.schema.json,
// k8s/criteria-binding.schema.json, k8s/workflow-interface.schema.json)
// are the schema of record, so the Go validation here mirrors those
// rules exactly (one schema, one gate). Castle stores and validates
// SHAPE only (dave 10-23 rule): values — commands, images, secret lane
// KEY names, env names — are never interpreted.
//
// Round-trip discipline: importing the live payloads must equal
// rendering them back (ParseRecord <- Partition -> Assembly ->
// RenderRoutesPayload with checksum equality), so no silent shape
// change sneaks through the migration. Canonicalization decodes and
// re-marshals through generic maps (sorted keys), which is the one
// rendering both struct-shaped records and hand-written payloads
// normalize through.
package confighome

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// Wire constants of record (the criteria-k8s payload vocabulary).
const (
	// APIVersion is the declared apiVersion of every payload here.
	APIVersion = "criteria.brokenbots.dev/v1"
	// KindRoutes is the routes payload's kind.
	KindRoutes = "Routes"
	// KindBinding is the binding payload's kind.
	KindBinding = "CriteriaBinding"
	// KindInterface is the tree interface's kind.
	KindInterface = "WorkflowInterface"
	// KindRecord is the per-project config record's kind.
	KindRecord = "ProjectWorkflowConfig"

	// TypeImage and TypeURL are the ADR-0005 D1/D2 workflow source modes
	// (declared, never inferred; image-only is fail-closed against
	// fetched-content fields).
	TypeImage = "image"
	TypeURL   = "url"

	// Volume kinds: pvc | nfs | tmp | k8s-secret (KB-7).
	VolumePVC       = "pvc"
	VolumeNFS       = "nfs"
	VolumeTmp       = "tmp"
	VolumeK8sSecret = "k8s-secret"

	// TagMatch semantics for route and config-entry tag subsets.
	TagMatchAll = "all"
	TagMatchAny = "any"

	// CutoverDone is the registry cutover marker (KB-311, ADR 307):
	// the namespace serves its own criteria-binding ConfigMap.
	CutoverDone = "done"

	// DefaultState is the workflow state a route triggers on when it
	// omits its states list (CRI-218).
	DefaultState = "Triage"

	// Admission queue classes (CRI-242): dev serializes runs per repoURL,
	// triage admits concurrently (read-only against the repo). An absent
	// class resolves to ClassDefault.
	ClassDefault = "dev"
	ClassDev     = "dev"
	ClassTriage  = "triage"

	// Adapter-image conflict policy vocabulary (KB-319). Absent resolves
	// to ImageConflictPolicyFailClosed.
	ImageConflictPolicyFailClosed     = "fail-closed"
	ImageConflictPolicyPreferEvent    = "prefer-event"
	ImageConflictPolicyPreferMetadata = "prefer-metadata"

	// InterfaceLockKeyRepoURL is the interface's declared admission-lock
	// key (KB-319): the exact one value the vocabulary admits.
	InterfaceLockKeyRepoURL = "repoUrl"
)

// KB310ConfigSlotVariables is the platform config-slot surface (KB-310):
// the only slot names a tree interface may declare and the tree variable
// each value renders as through the KB-103 per-run config rendering.
var KB310ConfigSlotVariables = map[string]string{
	"buildCmd":   "build_cmd",
	"testCmd":    "test_cmd",
	"ciGateCmd":  "ci_gate_cmd",
	"baseBranch": "base_branch",
}

// Record is one project's workflow configuration (KB-339): the routes
// the project owns, the workflowLibrary objects its routes reference,
// the configLibrary entries it owns, the projectRegistry entry that
// routes tickets to it, and the project namespace's binding payload.
// The record is the PROJECT SIDE of the ConfigMap data: assemble records
// to render the platform routes payload, render one record's Binding to
// produce the project's binding.json.
//
// RoutesOrdinal is import bookkeeping (the record's first route's
// ordinal in the imported payload); the routes payload's array order is
// semantic (first-match-wins resolution), so rendering sorts records by
// RoutesOrdinal — then project id — to rebuild the imported order
// byte-for-byte. Records created after the import append at the end.
type Record struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	// RegistryName is the projectRegistry key (the Kanboard project
	// name): the ONLY ticket->project routing table's key for this
	// record's routes.
	RegistryName string `json:"registryName"`
	// Namespace is the registry entry's namespace: where the project's
	// runs are admitted and where its criteria-binding ConfigMap lives.
	Namespace string `json:"namespace"`
	// Repos are the advisory owner/name repo members (never routed).
	Repos []string `json:"repos,omitempty"`
	// Cutover holds "done" once the project's namespace serves its own
	// binding; absent keeps the pre-cutover behavior. The flip goes
	// through MarkCutoverDone (binding-first: no binding, no flip) so
	// the record's post-cutover state is always a valid assembly state.
	Cutover string `json:"cutover,omitempty"`
	// ImageConflictPolicy is the project's KB-319 per-project conflict
	// policy — the FINAL authority over its catalog entries' policies.
	ImageConflictPolicy string `json:"imageConflictPolicy,omitempty"`
	// Catalog holds the workflowLibrary objects this record's routes
	// reference ("workflow catalog refs, trees by url+ref+digest").
	Catalog map[string]Workflow `json:"catalog"`
	// Routes are this project's route rows in payload order.
	Routes []Route `json:"routes"`
	// ConfigLibrary holds the configLibrary entries this record owns.
	// Unreferenced platform overlay entries land on the record owning
	// the routes ConfigMap's namespace (the platform record).
	ConfigLibrary map[string]ConfigEntry `json:"configLibrary,omitempty"`
	// Binding is the project namespace's criteria-binding payload;
	// nil until the project imports one.
	Binding *Binding `json:"binding,omitempty"`
	// Digests holds castle-only catalog tree digests (name -> digest):
	// learned bookkeeping by url+ref, never rendered into the routes
	// payload (the operator stamps the run's tree digest itself).
	Digests map[string]string `json:"digests,omitempty"`

	// RoutesOrdinal is import bookkeeping: this record's first route's
	// ordinal in the imported payload. It rides the record doc (imported
	// projects keep their assembly order across etag swaps and re-reads)
	// but never renders — the rendered payload's route order comes from
	// the record's Routes slice, ordered by these ordinals for the
	// array's first-match-wins semantics.
	RoutesOrdinal int `json:"routesOrdinal,omitempty"`
}

// Workflow is a workflowLibrary object: how a run obtains its workflow
// and where/how it executes. Shape mirrors the routes payload entry.
type Workflow struct {
	Type                string            `json:"type"`
	Namespace           string            `json:"namespace,omitempty"`
	Class               string            `json:"class,omitempty"`
	Image               string            `json:"image,omitempty"`
	URL                 string            `json:"url,omitempty"`
	Ref                 string            `json:"ref,omitempty"`
	Volumes             []Volume          `json:"volumes,omitempty"`
	Secrets             []Secret          `json:"secrets,omitempty"`
	Env                 map[string]string `json:"env,omitempty"`
	AdapterImages       map[string]string `json:"adapterImages,omitempty"`
	ImageConflictPolicy string            `json:"imageConflictPolicy,omitempty"`
	Interface           *Interface        `json:"interface,omitempty"`
}

// Interface is the workflow tree's machine-readable interface
// declaration (KB-310, ADR 307): what a run needs from a project
// binding and the config slots the tree honors. Empty slices render
// (unlike the payload's optional entry fields) because the live
// interface objects always carry them — round-trip fidelity.
type Interface struct {
	APIVersion  string       `json:"apiVersion"`
	Kind        string       `json:"kind"`
	Workflow    string       `json:"workflow"`
	Description string       `json:"description,omitempty"`
	Adapters    []string     `json:"adapters"`
	Mounts      []Mount      `json:"mounts"`
	SecretEnv   []string     `json:"secretEnv"`
	Env         []string     `json:"env"`
	ConfigSlots []ConfigSlot `json:"configSlots"`
	LockKey     string       `json:"lockKey,omitempty"`
}

// Mount is one required mount path of an interface declaration.
type Mount struct {
	Name      string `json:"name"`
	MountPath string `json:"mountPath"`
	Kind      string `json:"kind"`
}

// ConfigSlot is one config slot of an interface declaration: the config
// key the tree honors, the tree variable the value renders as, and the
// step/agent that reads it.
type ConfigSlot struct {
	Slot        string `json:"slot"`
	Variable    string `json:"variable"`
	Target      string `json:"target"`
	Description string `json:"description,omitempty"`
}

// Volume is a storage volume of kind pvc, nfs, tmp, or k8s-secret.
type Volume struct {
	Name       string            `json:"name"`
	Kind       string            `json:"kind"`
	MountPath  string            `json:"mountPath"`
	SubPath    string            `json:"subPath,omitempty"`
	ReadOnly   bool              `json:"readOnly,omitempty"`
	Claim      string            `json:"claim,omitempty"`
	Server     string            `json:"server,omitempty"`
	Path       string            `json:"path,omitempty"`
	SizeLimit  string            `json:"sizeLimit,omitempty"`
	SecretName string            `json:"secretName,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
}

// Secret is a secret reference (SecretProviderClass via the Secrets
// Store CSI driver). Values are key NAMES rendered inside the mount,
// never credential material.
type Secret struct {
	Name                string            `json:"name"`
	SecretProviderClass string            `json:"secretProviderClass"`
	MountPath           string            `json:"mountPath"`
	Env                 map[string]string `json:"env,omitempty"`
}

// Route assigns tickets (by workflow state names and a tag subset) to a
// workflowLibrary object. The record's Route.Project must name the
// record's own RegistryName — assembly keeps the payload shape intact.
type Route struct {
	Name     string   `json:"name"`
	Workflow string   `json:"workflow"`
	Project  string   `json:"project"`
	Tags     []string `json:"tags,omitempty"`
	TagMatch string   `json:"tagMatch,omitempty"`
	// States are the workflow state names the route triggers on;
	// omitted parses to [DefaultState] and an empty list rejects.
	States []string `json:"states"`
	// ConfigRef names the configLibrary entry the route's runs run
	// under; the reference resolves in the assembled payload's
	// configLibrary.
	ConfigRef string `json:"configRef,omitempty"`
}

// ConfigEntry is a per-repo configuration entry (KB-103, KB-154): the
// command overrides a run stamps and the ticket tags the entry serves.
type ConfigEntry struct {
	BuildCmd   string   `json:"buildCmd,omitempty"`
	TestCmd    string   `json:"testCmd,omitempty"`
	CIGateCmd  string   `json:"ciGateCmd,omitempty"`
	BaseBranch string   `json:"baseBranch,omitempty"`
	Tags       []string `json:"tags,omitempty"`
	TagMatch   string   `json:"tagMatch,omitempty"`
}

// Binding is one project's execution binding (KB-311, ADR 307): the
// project's COMPLETE execution config — adapter images, env, volumes,
// secrets (its own, namespace-local), and the config slot values the
// tree's interface declares. This maps the criteria-binding payload
// 1:1; Validate enforces the same fail-closed surface.
type Binding struct {
	APIVersion string            `json:"apiVersion"`
	Kind       string            `json:"kind"`
	Images     map[string]string `json:"images"`
	Env        map[string]string `json:"env"`
	Volumes    []Volume          `json:"volumes"`
	Secrets    []Secret          `json:"secrets"`
	Config     map[string]string `json:"config"`
}

// ParseRecord decodes a record doc, rejecting unknown fields at every
// level (fail-closed shape discipline) and applying validation.
func ParseRecord(data []byte) (*Record, error) {
	var r Record
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return nil, fmt.Errorf("project config record: %w", err)
	}
	if err := r.Validate(); err != nil {
		return nil, fmt.Errorf("project config record: %w", err)
	}
	return &r, nil
}

// ParseBinding decodes a criteria-binding payload, rejecting unknown
// fields and applying the binding validator: the import path for a
// live namespace binding.
func ParseBinding(data []byte) (*Binding, error) {
	var b Binding
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&b); err != nil {
		return nil, fmt.Errorf("binding payload: %w", err)
	}
	if err := ValidateBinding(&b); err != nil {
		return nil, fmt.Errorf("binding payload: %w", err)
	}
	return &b, nil
}

// Marshal renders the record to canonical JSON bytes (sorted maps, the
// store doc form).
func (r *Record) Marshal() ([]byte, error) {
	data, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	return Canonicalize(data)
}
