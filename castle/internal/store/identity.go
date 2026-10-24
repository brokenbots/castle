package store

import (
	"encoding/json"
	"fmt"
	"sort"
)

// IdentityJSON renders an agent identity block as canonical JSON for
// persistence: SDK versions are ordered by name and version, and
// encoding/json sorts the extra-map keys, so the same block serialises to
// identical bytes wherever castle stamps it (overseers.identity,
// runs.agent_identity). A zero block renders as empty text — the engines
// treat that as "no block".
func IdentityJSON(i *AgentIdentity) (string, error) {
	if i == nil || i.IsZero() {
		return "", nil
	}
	canon := AgentIdentity{Name: i.Name, Version: i.Version, RepoURL: i.RepoURL, BuildDate: i.BuildDate}
	if len(i.SDKVersions) > 0 {
		canon.SDKVersions = make([]AgentSDKVersion, len(i.SDKVersions))
		copy(canon.SDKVersions, i.SDKVersions)
		sort.Slice(canon.SDKVersions, func(a, b int) bool {
			if canon.SDKVersions[a].Name != canon.SDKVersions[b].Name {
				return canon.SDKVersions[a].Name < canon.SDKVersions[b].Name
			}
			return canon.SDKVersions[a].Version < canon.SDKVersions[b].Version
		})
	}
	if len(i.Extra) > 0 {
		canon.Extra = i.Extra
	}
	b, err := json.Marshal(canon)
	if err != nil {
		return "", fmt.Errorf("marshal identity block: %w", err)
	}
	return string(b), nil
}

// IdentityFromJSON parses a persisted identity block; empty, malformed, or
// field-free text yields nil (an unparsable block must never break agent
// reads, and a block with no fields carries no block at all).
func IdentityFromJSON(s string) *AgentIdentity {
	if s == "" {
		return nil
	}
	var i AgentIdentity
	if err := json.Unmarshal([]byte(s), &i); err != nil || i.IsZero() {
		return nil
	}
	return &i
}
