package auth

import (
	"net/http"
	"testing"

	"github.com/brokenbots/castle/castle/internal/store"
)

// TestParseAgentRegistration covers the registration metadata parser: valid
// blocks, absent blocks, malformed pairs, size caps, repeated single-value
// headers, and project slug validation.
func TestParseAgentRegistration(t *testing.T) {
	cases := []struct {
		name        string
		headers     func() http.Header
		wantErr     bool
		wantProject string
		want        *store.AgentIdentity
	}{
		{
			name: "full block",
			headers: func() http.Header {
				h := http.Header{}
				h.Set(HeaderAgentVersion, "1.2.3")
				h.Set(HeaderAgentRepoURL, "https://example.test/repo")
				h.Set(HeaderAgentBuildDate, "2026-01-01")
				h.Add(HeaderAgentSDKVersion, "criteria:v9")
				h.Add(HeaderAgentSDKVersion, "adapter:v2")
				h.Add(HeaderAgentExtra, "foo:bar")
				h.Set(HeaderAgentProject, "my-proj")
				return h
			},
			wantProject: "my-proj",
			want: &store.AgentIdentity{
				Version:     "1.2.3",
				RepoURL:     "https://example.test/repo",
				BuildDate:   "2026-01-01",
				SDKVersions: []store.AgentSDKVersion{{Name: "criteria", Version: "v9"}, {Name: "adapter", Version: "v2"}},
				Extra:       map[string]string{"foo": "bar"},
			},
		},
		{
			name:    "absent headers",
			headers: func() http.Header { return http.Header{} },
			want:    &store.AgentIdentity{},
		},
		{
			name: "malformed sdk pair",
			headers: func() http.Header {
				h := http.Header{}
				h.Set(HeaderAgentSDKVersion, "criteria")
				return h
			},
			wantErr: true,
		},
		{
			name: "empty sdk name",
			headers: func() http.Header {
				h := http.Header{}
				h.Set(HeaderAgentSDKVersion, ":v9")
				return h
			},
			wantErr: true,
		},
		{
			name: "oversized version",
			headers: func() http.Header {
				h := http.Header{}
				h.Set(HeaderAgentVersion, string(make([]byte, identityHeaderMax+1)))
				return h
			},
			wantErr: true,
		},
		{
			name: "sdk version cap",
			headers: func() http.Header {
				h := http.Header{}
				for i := 0; i <= identitySDKVersionMax; i++ {
					h.Add(HeaderAgentSDKVersion, "comp:v"+string(rune('a'+i)))
				}
				return h
			},
			wantErr: true,
		},
		{
			name: "extra cap",
			headers: func() http.Header {
				h := http.Header{}
				for i := 0; i <= identityExtraMax; i++ {
					h.Add(HeaderAgentExtra, string(rune('a'+i/26))+string(rune('a'+i%26))+":v")
				}
				return h
			},
			wantErr: true,
		},
		{
			name: "repeated single-value header",
			headers: func() http.Header {
				h := http.Header{}
				h.Add(HeaderAgentVersion, "1")
				h.Add(HeaderAgentVersion, "2")
				return h
			},
			wantErr: true,
		},
		{
			name: "invalid project slug",
			headers: func() http.Header {
				h := http.Header{}
				h.Set(HeaderAgentProject, "Bad Slug!")
				return h
			},
			wantErr: true,
		},
		{
			name: "extra value may contain colons",
			headers: func() http.Header {
				h := http.Header{}
				h.Add(HeaderAgentExtra, "image:v1:a:b")
				return h
			},
			want: &store.AgentIdentity{Extra: map[string]string{"image": "v1:a:b"}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			identity, project, err := ParseAgentRegistration(tc.headers())
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got identity=%+v project=%q", identity, project)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if project != tc.wantProject {
				t.Fatalf("project = %q, want %q", project, tc.wantProject)
			}
			if identity.Version != tc.want.Version || identity.RepoURL != tc.want.RepoURL || identity.BuildDate != tc.want.BuildDate {
				t.Fatalf("scalars = %+v, want %+v", identity, tc.want)
			}
			if len(identity.SDKVersions) != len(tc.want.SDKVersions) {
				t.Fatalf("sdk = %+v, want %+v", identity.SDKVersions, tc.want.SDKVersions)
			}
			for i, sdk := range tc.want.SDKVersions {
				if identity.SDKVersions[i] != sdk {
					t.Fatalf("sdk[%d] = %+v, want %+v", i, identity.SDKVersions[i], sdk)
				}
			}
			if len(identity.Extra) != len(tc.want.Extra) {
				t.Fatalf("extra = %+v, want %+v", identity.Extra, tc.want.Extra)
			}
			for k, v := range tc.want.Extra {
				if identity.Extra[k] != v {
					t.Fatalf("extra[%s] = %q, want %q", k, identity.Extra[k], v)
				}
			}
		})
	}
}

// TestParseAgentRegistrationHeaderCase pins the canonical-header handling:
// agents (and go-http clients) may send MIME-canonical spellings; the parser
// must find them.
func TestParseAgentRegistrationHeaderCase(t *testing.T) {
	h := http.Header{}
	h.Set("X-Criteria-Agent-Version", "9.9.9")
	identity, project, err := ParseAgentRegistration(h)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if project != "" {
		t.Fatalf("project = %q, want empty", project)
	}
	if identity.Version != "9.9.9" {
		t.Fatalf("version = %q", identity.Version)
	}
}
