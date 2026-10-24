// Package kanboardtrigger implements the kanboard trigger adapter (KB-320):
// a thin board adapter that fires Castle workflow assignments from Kanboard
// card entries (no routing logic — each lane carries ONE static route name)
// and mirrors Castle run events back onto board columns, replacing the
// criteria-k8s watcher's reconcile loop.
package kanboardtrigger

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Lane is the adapter's unit of configuration: one route name, its trigger
// columns, and its assignment content. Lanes are declared statically; the
// adapter performs no matching beyond the minimum any trigger needs (trigger
// tag gate + entry memo + in-flight abstention). One process can serve
// several lanes because the board mirror must reconcile a ticket's runs
// across lanes.
type Lane struct {
	// RouteName is the assignment's workflow_name ("route NAME"). Static: no
	// selection, no override, nothing resolves beyond what is written here.
	RouteName string `json:"route_name"`
	// Class is "triage" or "develop", driving the succeed-verdict handling
	// (KB-9) when a run settles.
	Class string `json:"class"`
	// TriggerColumns are the board columns whose ENTRY (KB-302 firing
	// discipline) makes the lane eligible to fire a card.
	TriggerColumns []string `json:"trigger_columns"`
	// WorkflowSource and LockfileSource are pass-through assignment content.
	// Castle requires a non-empty workflow_source (upstream criteria.v1
	// contract), so a lane without source fails closed at boot.
	WorkflowSource string `json:"workflow_source"`
	LockfileSource string `json:"lockfile_source"`
	// Inputs are static pass-through input_values (lane-level defaults),
	// merged under the per-ticket payload keys.
	Inputs map[string]string `json:"inputs"`
	// RepoDefault is the lane's optional default repo binding: a card must
	// carry repo:<owner>/<name> or a default (lane's, else the config's
	// global RepoDefault), else the fire fails closed (watcher parity).
	RepoDefault string `json:"repo_default"`
}

// Config is the adapter's boot configuration (parsed from the lanes JSON
// file plus process flags).
type Config struct {
	// BoardProject is the Kanboard project name the adapter polls.
	BoardProject string `json:"board_project"`
	// CastleProjectID is the castle project id assignments submit into.
	CastleProjectID string `json:"castle_project_id"`
	// TriggerTag gates which cards may fire (the "tag-lane tag"; default
	// "k8s-run"). Empty disables the gate (board-wide lanes).
	TriggerTag string `json:"trigger_tag"`
	// RepoDefault is the config's global default repo binding: the backstop
	// when a card carries no repo:<owner>/<name> tag and its lane declares
	// no repo_default. Never fire without a repo (fail closed, watcher
	// parity).
	RepoDefault string `json:"repo_default"`
	// ReviewColumn and DoneColumn name the mirror's two move targets
	// (defaults "Review"/"Done").
	ReviewColumn string `json:"review_column"`
	DoneColumn   string `json:"done_column"`
	Lanes        []Lane `json:"lanes"`
}

// Validated returns the config after boot-time validation; every lane must
// carry a route name, class, non-empty workflow source, and at least one
// trigger column — the fail-closed posture for content castle would reject.
func (c *Config) Validated() error {
	if c == nil {
		return fmt.Errorf("nil config")
	}
	if strings.TrimSpace(c.CastleProjectID) == "" {
		return fmt.Errorf("castle_project_id required")
	}
	if strings.TrimSpace(c.BoardProject) == "" {
		return fmt.Errorf("board_project required")
	}
	if len(c.Lanes) == 0 {
		return fmt.Errorf("at least one lane required")
	}
	for i, lane := range c.Lanes {
		if strings.TrimSpace(lane.RouteName) == "" {
			return fmt.Errorf("lane %d: route_name required", i)
		}
		if lane.Class != ClassTriage && lane.Class != ClassDevelop {
			return fmt.Errorf("lane %d (%s): class must be %q or %q", i, lane.RouteName, ClassTriage, ClassDevelop)
		}
		if strings.TrimSpace(lane.WorkflowSource) == "" {
			return fmt.Errorf("lane %d (%s): workflow_source required (castle rejects an empty source)", i, lane.RouteName)
		}
		if len(lane.TriggerColumns) == 0 {
			return fmt.Errorf("lane %d (%s): trigger_columns required", i, lane.RouteName)
		}
	}
	return nil
}

const (
	ClassTriage  = "triage"
	ClassDevelop = "develop"
)

// LoadConfigFile reads and validates the lanes config file.
func LoadConfigFile(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading lane config: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parsing lane config: %w", err)
	}
	if cfg.TriggerTag == "" {
		cfg.TriggerTag = "k8s-run"
	}
	if cfg.ReviewColumn == "" {
		cfg.ReviewColumn = "Review"
	}
	if cfg.DoneColumn == "" {
		cfg.DoneColumn = "Done"
	}
	for i := range cfg.Lanes {
		if cfg.Lanes[i].Inputs == nil {
			cfg.Lanes[i].Inputs = map[string]string{}
		}
	}
	if err := cfg.Validated(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// repoTagPrefix is the per-ticket repo binding tag prefix (watcher parity).
const repoTagPrefix = "repo:"

// RepoURL extracts the ticket's repo binding: a repo:<owner>/<name> tag
// wins, then the lane's default, then the config's global default, and
// neither means fail closed. A nil lane skips straight to the global
// default.
func (c *Config) RepoURL(lane *Lane, tags []string) string {
	for _, tag := range tags {
		if binding, ok := strings.CutPrefix(tag, repoTagPrefix); ok && binding != "" {
			return binding
		}
	}
	if lane != nil && lane.RepoDefault != "" {
		return lane.RepoDefault
	}
	return c.RepoDefault
}
