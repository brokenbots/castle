package refire

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// policyEqual compares policies field-wise (slices are not comparable with
// ==; field comparison keeps the failure messages concrete).
func policyEqual(a, b Policy) bool {
	if a.MaxAttempts != b.MaxAttempts || a.Backoff != b.Backoff {
		return false
	}
	if !stringsEqual(a.Transient, b.Transient) || !stringsEqual(a.Parks, b.Parks) || !stringsEqual(a.Cosmetics, b.Cosmetics) {
		return false
	}
	return true
}

func stringsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestPolicyRoundTrip(t *testing.T) {
	in := Policy{
		MaxAttempts: 5,
		Backoff:     90 * time.Second,
		Transient:   []string{"stall", "timeout"},
		Parks:       []string{"need_help"},
		Cosmetics:   []string{"emit race"},
	}
	doc, err := MarshalPolicy(in)
	if err != nil {
		t.Fatalf("MarshalPolicy: %v", err)
	}
	for _, want := range []string{
		`"max_attempts":5`, `"backoff_seconds":90`, `"transient":["stall","timeout"]`,
		`"parks":["need_help"]`, `"cosmetics":["emit race"]`,
	} {
		if !strings.Contains(doc, want) {
			t.Fatalf("marshal policy = %s, want to contain %s", doc, want)
		}
	}
	out, err := ParsePolicy(doc)
	if err != nil {
		t.Fatalf("ParsePolicy(%s): %v", doc, err)
	}
	if !policyEqual(out, in) {
		t.Fatalf("policy round-trip = %+v, want %+v", out, in)
	}
}

func TestDefaultPolicyDocStable(t *testing.T) {
	in := DefaultPolicyDoc()
	out, err := ParsePolicy(MustMarshalPolicy(in))
	if err != nil {
		t.Fatalf("ParsePolicy(default): %v", err)
	}
	if !policyEqual(out, in) {
		t.Fatalf("default policy round-trip = %+v, want %+v", out, in)
	}
	if in.MaxAttempts != 3 || in.Backoff != 30*time.Second {
		t.Fatalf("default policy budget = %d/%s", in.MaxAttempts, in.Backoff)
	}
	if len(in.Transient) == 0 || len(in.Parks) == 0 || len(in.Cosmetics) == 0 {
		t.Fatalf("default policy vocabularies missing: %+v", in)
	}
}

func TestParsePolicyStrict(t *testing.T) {
	valid := MustMarshalPolicy(DefaultPolicyDoc())

	if _, err := ParsePolicy(`{"max_attempts":1,"wat":true}`); err == nil {
		t.Fatalf("unknown key accepted")
	}
	if _, err := ParsePolicy(`{"max_attempts":-1}`); err == nil {
		t.Fatalf("negative max_attempts accepted")
	}
	if _, err := ParsePolicy(`{"backoff_seconds":-5}`); err == nil {
		t.Fatalf("negative backoff_seconds accepted")
	}
	if _, err := ParsePolicy(valid + ` {"max_attempts":2}`); err == nil {
		t.Fatalf("trailing document accepted")
	}
	if _, err := ParsePolicy(`{}` + "\n\n\t"); err != nil {
		t.Fatalf("trailing whitespace rejected: %v", err)
	}
	if _, err := ParsePolicy(valid); err != nil {
		t.Fatalf("default doc rejected: %v", err)
	}

	// max_attempts=0 means disabled, not error.
	p, err := ParsePolicy(`{"max_attempts":0}`)
	if err != nil || p.MaxAttempts != 0 {
		t.Fatalf("max_attempts 0 = %v %v", p, err)
	}
}

func TestParsePolicyFile(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.json")
	body := `[
	 {"project_id":"p1","route":"kanboardtrigger","policy":{"max_attempts":9,"backoff_seconds":0}},
	 {"route":"other","policy":{"max_attempts":1}},
	 {"policy":{"max_attempts":2,"parks":["need_help"]}}
	]`
	if err := os.WriteFile(good, []byte(body), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	specs, err := LoadPolicyFile(good)
	if err != nil {
		t.Fatalf("LoadPolicyFile: %v", err)
	}
	if len(specs) != 3 {
		t.Fatalf("specs = %d, want 3", len(specs))
	}
	if specs[0].ProjectID != "p1" || specs[0].Route != "kanboardtrigger" || specs[0].Policy.MaxAttempts != 9 {
		t.Fatalf("spec 0 = %+v", specs[0])
	}
	if specs[1].ProjectID != "" || specs[1].Route != "other" || specs[1].Policy.MaxAttempts != 1 {
		t.Fatalf("spec 1 = %+v", specs[1])
	}
	if specs[2].Route != "" || specs[2].Policy.Parks == nil {
		t.Fatalf("spec 2 = %+v", specs[2])
	}

	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte(`[{"route":"x","policy":{"wat":1}}]`), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if _, err := LoadPolicyFile(bad); err == nil {
		t.Fatalf("bad policy key accepted")
	}

	missing := filepath.Join(dir, "missing.json")
	if _, err := LoadPolicyFile(missing); err == nil {
		t.Fatalf("missing file accepted")
	}

	nopolicy := filepath.Join(dir, "nopolicy.json")
	if err := os.WriteFile(nopolicy, []byte(`[{"route":"x"}]`), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if _, err := LoadPolicyFile(nopolicy); err == nil || !strings.Contains(err.Error(), "missing policy") {
		t.Fatalf("entry without policy = %v", err)
	}
}
