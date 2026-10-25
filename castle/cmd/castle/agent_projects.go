package main

import (
	"strings"

	"github.com/brokenbots/castle/castle/internal/provision"
)

// parseAgentProjectConfig combines CASTLE_AGENT_PROJECT_TOKENS with the
// repeated --agent-project-token flag values into one grammar string for the
// provision package: env entries first, flag entries after, so a deployment
// can bootstrap a base set via env and extend or rotate entries per replica
// with flags. Configuring the SAME project twice is an error — a split
// configuration may never silently rotate a credential.
func parseAgentProjectConfig(envSpec string, flags []string) ([]provision.AgentProjectToken, error) {
	var spec strings.Builder
	if strings.TrimSpace(envSpec) != "" {
		spec.WriteString(envSpec)
		spec.WriteString(",")
	}
	spec.WriteString(strings.Join(flags, ","))
	return provision.ParseAgentProjectTokens(spec.String())
}
