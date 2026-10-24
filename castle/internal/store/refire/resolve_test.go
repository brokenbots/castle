package refire

import (
	"testing"

	"github.com/brokenbots/castle/castle/internal/store"
)

func TestNextAttemptKey(t *testing.T) {
	if got := NextAttemptKey("kb-5-deploy", 1); got != "kb-5-deploy#attempt-2" {
		t.Fatalf("NextAttemptKey = %s", got)
	}
	// Chains strip the last marker only and count from the recorded attempt.
	if got := NextAttemptKey("kb-5-deploy#attempt-2", 2); got != "kb-5-deploy#attempt-3" {
		t.Fatalf("chained key = %s", got)
	}
	// A key without a numeric trailing marker keeps its whole spelling.
	if got := NextAttemptKey("kb-6-needhelp-attempt-x", 1); got != "kb-6-needhelp-attempt-x#attempt-2" {
		t.Fatalf("non-numeric suffix key = %s", got)
	}
}

func TestTrimAttemptSuffix(t *testing.T) {
	if got := TrimAttemptSuffix("kb-5-deploy#attempt-3"); got != "kb-5-deploy" {
		t.Fatalf("TrimAttemptSuffix = %s", got)
	}
	if got := TrimAttemptSuffix("kb-5-deploy"); got != "kb-5-deploy" {
		t.Fatalf("plain key = %s", got)
	}
	if got := TrimAttemptSuffix("kb-5-deploy#attempt-later"); got != "kb-5-deploy#attempt-later" {
		t.Fatalf("non-numeric suffix = %s", got)
	}
	if got := TrimAttemptSuffix("kb-5-deploy#attempt-"); got != "kb-5-deploy#attempt-" {
		t.Fatalf("empty suffix = %s", got)
	}
}

func TestResolvePolicyLadder(t *testing.T) {
	rows := []store.RefirePolicyRow{
		{ProjectID: "p1", Route: "kanboardtrigger", Policy: MustMarshalPolicy(Policy{MaxAttempts: 9, Backoff: 5})},
		{ProjectID: "p1", Route: "", Policy: MustMarshalPolicy(Policy{MaxAttempts: 7})},
		{ProjectID: "", Route: "kanboardtrigger", Policy: MustMarshalPolicy(Policy{MaxAttempts: 6})},
		{ProjectID: "", Route: "", Policy: MustMarshalPolicy(Policy{MaxAttempts: 5})},
	}

	cases := []struct {
		project, route     string
		wantMax            int
		wantSourceContains string
	}{
		{"p1", "kanboardtrigger", 9, "p1,kanboardtrigger"},
		{"p1", "other", 7, "p1,"},
		{"p2", "kanboardtrigger", 6, ",kanboardtrigger"},
		{"p2", "other", 5, ","},
	}
	for _, c := range cases {
		p, src := ResolvePolicy(rows, c.project, c.route)
		if p.MaxAttempts != c.wantMax {
			t.Fatalf("resolve(%s,%s) = %d (source %s), want %d", c.project, c.route, p.MaxAttempts, src, c.wantMax)
		}
		if !contains(src, c.wantSourceContains) {
			t.Fatalf("resolve source = %s, want to contain %s", src, c.wantSourceContains)
		}
	}
}

func TestResolvePolicyFallsThroughBadRow(t *testing.T) {
	rows := []store.RefirePolicyRow{
		{ProjectID: "p1", Route: "wf", Policy: `{"max_attempts":9,"junk":1}`},
		{ProjectID: "p1", Route: "", Policy: MustMarshalPolicy(Policy{MaxAttempts: 7})},
	}
	p, src := ResolvePolicy(rows, "p1", "wf")
	if p.MaxAttempts != 7 {
		t.Fatalf("bad row not skipped: max %d (source %s)", p.MaxAttempts, src)
	}

	// Nothing valid below: the built-in default takes over.
	p, src = ResolvePolicy([]store.RefirePolicyRow{
		{ProjectID: "p1", Route: "wf", Policy: `not json`},
	}, "p1", "wf")
	if src != "built-in" || p.MaxAttempts != DefaultPolicyDoc().MaxAttempts {
		t.Fatalf("fallback = %d/%s, want built-in", p.MaxAttempts, src)
	}
}

func TestResolvePolicyEmptyRowsBuiltIn(t *testing.T) {
	p, src := ResolvePolicy(nil, "p1", "wf")
	if src != "built-in" {
		t.Fatalf("source = %s", src)
	}
	if p.MaxAttempts != 3 || p.Backoff != DefaultPolicyDoc().Backoff {
		t.Fatalf("built-in policy = %+v", p)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
