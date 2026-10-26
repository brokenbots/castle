package confighome

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
)

// Partition splits the routes payload into per-project records: each
// projectRegistry entry becomes a record carrying its routes, the
// catalog objects those routes reference, and the configLibrary entries
// its routes' configRefs name. Unreferenced platform overlay entries land
// on the platformRegistryName record — the import tool passes the
// registry name that should own the platform overlay data (config
// entries not referenced by any route), which keeps the assembled
// configLibrary byte-faithful without inventing ownership.
//
// RoutesOrdinal records the payload ordinal of each record's first route
// so rendering rebuilds the array order (first-match-wins resolution
// makes order semantic). A route that omits its states list materializes
// the CRI-218 default ([DefaultState]) in its record.
//
// The payload's namespace is ConfigMap metadata, not payload data, so it
// never lands in records; the binding payloads are imported separately
// (ParseBinding).
//
// Round-trip fidelity limits (both fail closed at the import's
// pre-write checksum check, never a silent shape change): a live route
// that omits its states materializes the default in the record, so the
// rendered payload re-emits states and diverges from a live payload
// that truly omits them; and routes of different projects interleaved
// in the live array render as contiguous per-record blocks, so the
// first-match-wins order — and the payload — differ. Both abort the
// import; re-materialize the live payload first (states defaults,
// grouped routes) before importing.
func Partition(platformRegistryName string, payload []byte) (map[string]*Record, error) {
	var doc routesPayloadDoc
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("routes payload: %w", err)
	}
	if err := validateImportedPayload(&doc); err != nil {
		return nil, err
	}

	// Route ordinals: the payload index of each registry name's first
	// route.
	ordinals := map[string]int{}
	for i, rt := range doc.Routes {
		if _, ok := ordinals[rt.Project]; !ok {
			ordinals[rt.Project] = i
		}
	}

	// configLibrary ownership: entries referenced by a route's configRef
	// belong to that route's record; every other entry is platform
	// overlay data owned by the payload's platform record.
	configOwner := map[string]string{}
	for _, rt := range doc.Routes {
		if rt.ConfigRef != "" && configOwner[rt.ConfigRef] == "" {
			configOwner[rt.ConfigRef] = rt.Project
		}
	}
	// Catalog ownership works the same way: a workflowLibrary entry a
	// route references lands on the route's record; unreferenced
	// entries are platform overlay objects owned by the platform
	// record's catalog, so the assembled workflowLibrary stays
	// byte-faithful (a workflow that no route references is still part
	// of the payload).
	referencedWorkflows := map[string]bool{}
	for _, rt := range doc.Routes {
		referencedWorkflows[rt.Workflow] = true
	}

	records := map[string]*Record{}
	names := make([]string, 0, len(doc.ProjectRegistry))
	for name := range doc.ProjectRegistry {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		entry := doc.ProjectRegistry[name]
		records[name] = &Record{
			APIVersion:          APIVersion,
			Kind:                KindRecord,
			RegistryName:        name,
			Namespace:           entry.Namespace,
			Repos:               entry.Repos,
			Cutover:             entry.Cutover,
			ImageConflictPolicy: entry.ImageConflictPolicy,
			Catalog:             map[string]Workflow{},
			Routes:              []Route{},
			ConfigLibrary:       map[string]ConfigEntry{},
			RoutesOrdinal:       ordinals[name],
		}
	}

	for name, entry := range doc.ConfigLibrary {
		if referenced, ok := configOwner[name]; ok {
			records[referenced].ConfigLibrary[name] = entry
			continue
		}
		if records[platformRegistryName] == nil {
			return nil, fmt.Errorf("platform record %q is not a projectRegistry entry (configLibrary entry %q would strand)", platformRegistryName, name)
		}
		records[platformRegistryName].ConfigLibrary[name] = entry
	}
	for name, wf := range doc.WorkflowLibrary {
		if referencedWorkflows[name] {
			continue
		}
		if records[platformRegistryName] == nil {
			return nil, fmt.Errorf("platform record %q is not a projectRegistry entry (workflowLibrary entry %q would strand)", platformRegistryName, name)
		}
		records[platformRegistryName].Catalog[name] = wf
	}

	for _, rt := range doc.Routes {
		rec := records[rt.Project]
		wf, ok := doc.WorkflowLibrary[rt.Workflow]
		if !ok {
			return nil, fmt.Errorf("route %q: workflow %q not in workflowLibrary", rt.Name, rt.Workflow)
		}
		rec.Catalog[rt.Workflow] = wf
		if rt.States == nil {
			rt.States = []string{DefaultState}
		}
		rec.Routes = append(rec.Routes, rt)
	}

	for name, rec := range records {
		if err := rec.Validate(); err != nil {
			return nil, fmt.Errorf("record %q: %w", name, err)
		}
	}
	return records, nil
}

// validateImportedPayload gates the imported payload's envelope and
// integrity before any record is written: the exact checks the
// criteria-k8s operator applies to the ConfigMap, so a live payload that
// would not parse is imported nowhere.
func validateImportedPayload(doc *routesPayloadDoc) error {
	if doc.APIVersion != APIVersion {
		return fmt.Errorf("routes payload: apiVersion must be %q, got %q", APIVersion, doc.APIVersion)
	}
	if doc.Kind != KindRoutes {
		return fmt.Errorf("routes payload: kind must be %q, got %q", KindRoutes, doc.Kind)
	}
	return validateAssembledPayload(doc)
}
