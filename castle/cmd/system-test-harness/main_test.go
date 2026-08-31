package main

import (
	"testing"
)

func TestEnvOrDefaultHarness(t *testing.T) {
	t.Setenv("TEST_HARNESS_VAR", "override")
	if got := envOrDefault("TEST_HARNESS_VAR", "fallback"); got != "override" {
		t.Errorf("env set: got %q want override", got)
	}
	if got := envOrDefault("TEST_HARNESS_VAR_NOT_SET", "fallback"); got != "fallback" {
		t.Errorf("env unset: got %q want fallback", got)
	}
}

func TestH2CClient(t *testing.T) {
	c := h2cClient()
	if c == nil {
		t.Fatal("h2cClient returned nil")
	}
	if c.Transport == nil {
		t.Fatal("h2cClient transport is nil")
	}
}

func TestFixturesDefined(t *testing.T) {
	if len(fixtures) == 0 {
		t.Fatal("no fixtures defined")
	}
	for _, fx := range fixtures {
		if fx.name == "" || fx.source == "" || fx.wantAgent == "" || fx.wantStatus == "" {
			t.Errorf("incomplete fixture: %+v", fx)
		}
	}
}
