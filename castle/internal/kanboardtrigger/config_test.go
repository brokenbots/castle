package kanboardtrigger

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigDefaultsFillMirrorColumns(t *testing.T) {
	raw := `{"board_project":"KB board","castle_project_id":"kb","lanes":[{"route_name":"r1","class":"develop","trigger_columns":["WIP"],"workflow_source":"hermes"}]}`
	path := filepath.Join(t.TempDir(), "lanes.json")
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfigFile(path)
	if err != nil {
		t.Fatalf("LoadConfigFile: %v", err)
	}
	if cfg.TriggerTag != "k8s-run" {
		t.Errorf("default trigger tag = %q, want k8s-run", cfg.TriggerTag)
	}
	if cfg.ReviewColumn != "Review" || cfg.DoneColumn != "Done" {
		t.Errorf("default mirror columns = %q/%q, want Review/Done", cfg.ReviewColumn, cfg.DoneColumn)
	}
}

func TestConfigValidatedRejectsIncompleteLanes(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{
			name:    "no lanes",
			mutate:  func(c *Config) { c.Lanes = nil },
			wantErr: "at least one lane",
		},
		{
			name:    "empty route name",
			mutate:  func(c *Config) { c.Lanes[0].RouteName = "  " },
			wantErr: "route_name",
		},
		{
			name:    "bad class",
			mutate:  func(c *Config) { c.Lanes[0].Class = "research" },
			wantErr: "class must be",
		},
		{
			name:    "empty workflow source",
			mutate:  func(c *Config) { c.Lanes[0].WorkflowSource = "" },
			wantErr: "workflow_source",
		},
		{
			name:    "no trigger columns",
			mutate:  func(c *Config) { c.Lanes[0].TriggerColumns = nil },
			wantErr: "trigger_columns",
		},
		{
			name:    "no castle project",
			mutate:  func(c *Config) { c.CastleProjectID = "" },
			wantErr: "castle_project_id",
		},
		{
			name:    "no board project",
			mutate:  func(c *Config) { c.BoardProject = "" },
			wantErr: "board_project",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testLaneConfig()
			tc.mutate(cfg)
			err := cfg.Validated()
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validated() err = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestRepoURLPrecedence(t *testing.T) {
	cfg := &Config{
		BoardProject:    "KB board",
		CastleProjectID: "kb",
		Lanes: []Lane{
			{
				RouteName: "kb-develop", Class: ClassDevelop,
				TriggerColumns: []string{"WIP"}, WorkflowSource: "hermes",
				RepoDefault: "lane-owner/lane-repo",
			},
			{
				RouteName: "kb-triage", Class: ClassTriage,
				TriggerColumns: []string{"Backlog"}, WorkflowSource: "hermes",
			},
		},
		RepoDefault: "global-owner/global-repo",
	}
	develop := &cfg.Lanes[0]
	triage := &cfg.Lanes[1]
	if got := cfg.RepoURL(develop, []string{"repo:own/name", "k8s-run"}); got != "own/name" {
		t.Errorf("RepoURL(tag) = %q, want own/name", got)
	}
	if got := cfg.RepoURL(develop, []string{"k8s-run"}); got != "lane-owner/lane-repo" {
		t.Errorf("RepoURL(lane default) = %q, want lane-owner/lane-repo", got)
	}
	if got := cfg.RepoURL(triage, []string{"k8s-run"}); got != "global-owner/global-repo" {
		t.Errorf("RepoURL(lane without default) = %q, want global-owner/global-repo", got)
	}
	if got := cfg.RepoURL(nil, nil); got != "global-owner/global-repo" {
		t.Errorf("RepoURL(no lane, no tags) = %q, want global default", got)
	}
	if got := cfg.RepoURL(triage, []string{"repo:"}); got != "global-owner/global-repo" {
		t.Errorf("RepoURL(empty repo: tag) = %q, want global default (empty tag does not bind)", got)
	}
}
