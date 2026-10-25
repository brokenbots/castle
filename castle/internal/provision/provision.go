// Package provision holds castle's boot-time identity provisioning helpers
// (CRI-133 orchestrator, KB-320 trigger, and KB-337 per-project agent
// credentials). Every helper is idempotent at boot: provision/rotate via
// upsert, revoke via delete, and no secret material persists in plaintext —
// only SHA-256 token hashes.
package provision

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/brokenbots/castle/castle/internal/auth"
	"github.com/brokenbots/castle/castle/internal/store"
)

// The identity shape for one project's operator credential (KB-337): each
// project's openbao-resolved castle_criteria_token resolves to a DISTINCT
// operator row named operator-<project> carrying the agent role and the
// project scope attribute. The shared platform/console 'operator' identity
// stays the unscoped row. No new schema: the operators and operator_tokens
// tables (migration 0022) carry per-project credentials already.
const (
	ProjectOperatorIDPrefix = "operator-"
	ProjectTokenIDPrefix    = "agent-project-"
)

// ProjectOperatorID is the operator row id/name for one project's agent
// credential.
func ProjectOperatorID(project string) string {
	return ProjectOperatorIDPrefix + project
}

// ProjectTokenID is the operator_tokens row id for one project's credential.
// Upserting by this id rotates the hash, killing previously issued tokens.
func ProjectTokenID(project string) string {
	return ProjectTokenIDPrefix + project
}

// ProjectAgentAttributes returns the attributes a per-project agent
// credential's operator row carries: the agent role plus the project scope
// the registration binding enforces (fail closed).
func ProjectAgentAttributes(project string) map[string]string {
	return map[string]string{
		store.OperatorAttributeRole:    store.OperatorRoleAgent,
		store.OperatorAttributeProject: project,
		store.OperatorAttributeDisplay: project,
	}
}

// AgentProjectToken is one parsed CASTLE_AGENT_PROJECT_TOKENS entry: a
// project slug and its bearer token. An empty Token revokes the project's
// credential.
type AgentProjectToken struct {
	Project string
	Token   string
}

// Revoke reports whether the entry revokes instead of provisions.
func (e AgentProjectToken) Revoke() bool { return e.Token == "" }

// ParseAgentProjectTokens parses the per-project credential configuration
// (KB-337): entries "project:token", separated by commas or newlines, split
// at the FIRST colon (project slugs never contain ':') with surrounding
// whitespace trimmed. An entry with an empty token ("project" or "project:")
// revokes that project's credential. An unknown or malformed project slug is
// a configuration error, and the same project may appear at most once so a
// split flag/env configuration cannot silently rotate twice.
func ParseAgentProjectTokens(spec string) ([]AgentProjectToken, error) {
	var entries []AgentProjectToken
	seen := map[string]bool{}
	for _, raw := range strings.FieldsFunc(spec, func(r rune) bool { return r == ',' || r == '\n' || r == '\r' }) {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			continue
		}
		project, token, _ := strings.Cut(entry, ":")
		project = strings.TrimSpace(project)
		if err := store.ValidateProjectID(project); err != nil {
			return nil, fmt.Errorf("agent project credential %q: %w", strings.SplitN(entry, ":", 2)[0], err)
		}
		if seen[project] {
			return nil, fmt.Errorf("agent project credential %q configured more than once", project)
		}
		seen[project] = true
		entries = append(entries, AgentProjectToken{Project: project, Token: strings.TrimSpace(token)})
	}
	return entries, nil
}

// ProvisionProjectAgentCredential provisions or rotates one project's agent
// credential (KB-337): the operator row (operator-<project>, attributes
// {role: agent, project: <project>}) and its operator token keyed by the
// project. Rotating with a changed token invalidates previously issued
// tokens. The provisioning does not require the project row to exist — the
// secret keyspace is upstream of the project ledger; a credential whose
// project never materialises fails closed at registration instead.
func ProvisionProjectAgentCredential(ctx context.Context, st store.Store, project, token string, now time.Time) error {
	if err := store.ValidateProjectID(project); err != nil {
		return fmt.Errorf("provision agent project credential: %w", err)
	}
	if token == "" {
		return fmt.Errorf("provision agent project credential %q: token required", project)
	}
	op := &store.Operator{
		ID:         ProjectOperatorID(project),
		Name:       ProjectOperatorID(project),
		Attributes: ProjectAgentAttributes(project),
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if err := st.UpsertOperator(ctx, op); err != nil {
		return fmt.Errorf("provision agent project credential %q: upsert operator: %w", project, err)
	}
	tok := &store.OperatorToken{
		ID:         ProjectTokenID(project),
		OperatorID: ProjectOperatorID(project),
		TokenHash:  auth.HashToken(token),
		CreatedAt:  now,
	}
	if err := st.UpsertOperatorToken(ctx, tok); err != nil {
		return fmt.Errorf("provision agent project credential %q: upsert token: %w", project, err)
	}
	return nil
}

// RevokeProjectAgentCredential revokes one project's agent credential:
// deleting the operator row cascades its operator tokens (and any login
// sessions), so the credential stops authenticating. Revoking an
// unprovisioned project is a no-op.
func RevokeProjectAgentCredential(ctx context.Context, st store.Store, project string) error {
	if err := store.ValidateProjectID(project); err != nil {
		return fmt.Errorf("revoke agent project credential: %w", err)
	}
	if err := st.DeleteOperator(ctx, ProjectOperatorID(project)); err != nil {
		return fmt.Errorf("revoke agent project credential %q: delete operator: %w", project, err)
	}
	return nil
}

// ApplyAgentProjectTokens provisions/revokes every parsed entry. It exists so
// boot wiring and tests share one apply loop; each entry is independent and
// the returned error is the first failure.
func ApplyAgentProjectTokens(ctx context.Context, st store.Store, entries []AgentProjectToken, now time.Time) error {
	for _, e := range entries {
		if e.Revoke() {
			if err := RevokeProjectAgentCredential(ctx, st, e.Project); err != nil {
				return err
			}
			continue
		}
		if err := ProvisionProjectAgentCredential(ctx, st, e.Project, e.Token, now); err != nil {
			return err
		}
	}
	return nil
}