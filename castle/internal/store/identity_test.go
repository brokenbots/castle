package store

import (
	"errors"
	"fmt"
	"testing"
)

func TestIdentityJSONCanonical(t *testing.T) {
	base := &AgentIdentity{
		Version:     "1.2.3",
		RepoURL:     "https://git.example/agents/x",
		BuildDate:   "2026-01-02T03:04:05Z",
		SDKVersions: []AgentSDKVersion{{Name: "criteria", Version: "4.1.0"}, {Name: "adapter", Version: "0.3.9"}},
		Extra:       map[string]string{"os": "linux", "arch": "amd64"},
	}
	canon, err := IdentityJSON(base)
	if err != nil {
		t.Fatalf("IdentityJSON: %v", err)
	}
	want := `{"version":"1.2.3","repo_url":"https://git.example/agents/x","build_date":"2026-01-02T03:04:05Z",` +
		`"sdk_versions":[{"name":"adapter","version":"0.3.9"},{"name":"criteria","version":"4.1.0"}],` +
		`"extra":{"arch":"amd64","os":"linux"}}`
	if canon != want {
		t.Fatalf("canonical JSON = %s, want %s", canon, want)
	}

	// A shuffled copy serialises to identical bytes: canonical stable output.
	shuffled := &AgentIdentity{
		Extra:       map[string]string{"arch": "amd64", "os": "linux"},
		SDKVersions: []AgentSDKVersion{{Name: "adapter", Version: "0.3.9"}, {Name: "criteria", Version: "4.1.0"}},
		RepoURL:     base.RepoURL,
		Version:     "1.2.3",
		BuildDate:   base.BuildDate,
	}
	again, err := IdentityJSON(shuffled)
	if err != nil || again != canon {
		t.Fatalf("shuffled IdentityJSON = %v %q, want nil %q", err, again, canon)
	}

	// Zero and nil blocks render as empty text ("no block").
	for i, zero := range []*AgentIdentity{nil, {}, {Extra: map[string]string{}}, {Version: "", RepoURL: ""}} {
		got, err := IdentityJSON(zero)
		if err != nil || got != "" {
			t.Fatalf("case %d: IdentityJSON = %q, %v; want empty, nil", i, got, err)
		}
	}

	// A stored block parses back equal, and empty/undecodable text is nil.
	parsed := IdentityFromJSON(canon)
	if parsed == nil || parsed.Version != "1.2.3" || parsed.RepoURL != base.RepoURL || parsed.BuildDate != base.BuildDate ||
		len(parsed.SDKVersions) != 2 || parsed.SDKVersions[0].Name != "adapter" ||
		parsed.Extra["os"] != "linux" {
		t.Fatalf("IdentityFromJSON round-trip = %+v", parsed)
	}
	for _, blank := range []string{"", "not json", "{}"} {
		if got := IdentityFromJSON(blank); got != nil {
			t.Fatalf("IdentityFromJSON(%q) = %+v, want nil", blank, got)
		}
	}
}

func TestValidateProjectID(t *testing.T) {
	for _, ok := range []string{"a", "proj-1", "ok.slug_1", "a0123456789abcdef"} {
		if err := ValidateProjectID(ok); err != nil {
			t.Fatalf("ValidateProjectID(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "-lead", "UPPER", "has space", "a/b"} {
		if err := ValidateProjectID(bad); !errors.Is(err, ErrInvalidProjectID) {
			t.Fatalf("ValidateProjectID(%q) = %v, want ErrInvalidProjectID", bad, err)
		}
	}
}

func TestProjectQueueSerialization(t *testing.T) {
	// Empty list serialises as empty text and reads back nil.
	if q, err := MarshalQueues(nil); err != nil || q != "" {
		t.Fatalf("MarshalQueues(nil) = %q, %v; want empty, nil", q, err)
	}
	if got := UnmarshalQueues(""); got != nil {
		t.Fatalf("UnmarshalQueues(empty) = %v, want nil", got)
	}

	// Round-trip preserves the queue set.
	q, err := MarshalQueues([]string{"alpha", "beta"})
	if err != nil {
		t.Fatalf("MarshalQueues: %v", err)
	}
	if got := UnmarshalQueues(q); len(got) != 2 || got[0] != "alpha" || got[1] != "beta" {
		t.Fatalf("UnmarshalQueues(%q) = %v", q, got)
	}

	// The opaque settings/info maps follow the same convention.
	if m := MarshalJSONMap(nil); m != "" {
		t.Fatalf("MarshalJSONMap(nil) = %q", m)
	}
	if got := UnmarshalJSONMap(`{"a":"b"}`); got == nil || got["a"] != "b" {
		t.Fatalf("UnmarshalJSONMap = %v", got)
	}
}

func TestIsUniqueViolation(t *testing.T) {
	for _, text := range []string{
		"UNIQUE constraint failed: projects.id",
		`ERROR: duplicate key value violates unique constraint "projects_pkey" (SQLSTATE 23505)`,
	} {
		if !IsUniqueViolation(errors.New(text)) {
			t.Fatalf("IsUniqueViolation(%q) = false, want true", text)
		}
	}
	if IsUniqueViolation(nil) {
		t.Fatal("IsUniqueViolation(nil) = true, want false")
	}
	if IsUniqueViolation(errors.New("FOREIGN KEY constraint failed")) {
		t.Fatal("IsUniqueViolation(fk) = true, want false")
	}
	if IsUniqueViolation(fmt.Errorf("wrapped: %w", errors.New("duplicate key value violates unique constraint \"projects_pkey\""))) {
		// wrapped dialect text still detected
	} else {
		t.Fatal("IsUniqueViolation(wrapped duplicate) = false, want true")
	}
}
