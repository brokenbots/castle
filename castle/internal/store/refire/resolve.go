package refire

import (
	"github.com/brokenbots/castle/castle/internal/store"
)

// ResolvePolicy resolves the policy for one (project, route) assignment from
// the policy rows (KB-321). Rows are full specifications — no field-level
// merging between layers — so the resolution order is simply the specificity
// ladder: per-route, per-project default, global route, global default row,
// built-in default.
//
// A policy row that no longer parses (an operator edit between the row load
// and this evaluation) is skipped: the resolution falls through to the next
// layer, so one bad row cannot take the engine down or disable it. Returns
// the policy, the layer's key ("built-in" for the fallback), and whether any
// row matched.
func ResolvePolicy(rows []store.RefirePolicyRow, projectID, route string) (Policy, string) {
	type key struct{ project, route string }
	ladder := []key{
		{projectID, route},
		{projectID, ""},
		{"", route},
		{"", ""},
	}
	byKey := make(map[key]string, len(rows))
	for _, r := range rows {
		byKey[key{r.ProjectID, r.Route}] = r.Policy
	}
	for _, k := range ladder {
		if doc, ok := byKey[k]; ok {
			if p, err := ParsePolicy(doc); err == nil {
				return p, "(" + k.project + "," + k.route + ")"
			}
		}
	}
	return DefaultPolicyDoc(), "built-in"
}
