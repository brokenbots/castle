package main

import (
	"testing"

	"github.com/brokenbots/castle/castle/internal/provision"
)

// provisionEntry aliases the parsed credential shape so the table stays
// aligned with the provision package's grammar.
type provisionEntry = provision.AgentProjectToken

func TestParseAgentProjectConfig(t *testing.T) {
	cases := []struct {
		name    string
		env     string
		flags   []string
		want    []provisionEntry
		wantErr bool
	}{
		{name: "nothing configured", env: "", flags: nil, want: nil},
		{name: "env only", env: " alpha:tok-a , beta: ", want: []provisionEntry{
			{Project: "alpha", Token: "tok-a"}, {Project: "beta"},
		}},
		{name: "flags only", flags: []string{"alpha:tok-a", "beta:tok-b"}, want: []provisionEntry{
			{Project: "alpha", Token: "tok-a"}, {Project: "beta", Token: "tok-b"},
		}},
		{name: "env extends flags", env: "alpha:tok-a", flags: []string{" beta:tok-b "}, want: []provisionEntry{
			{Project: "alpha", Token: "tok-a"}, {Project: "beta", Token: "tok-b"},
		}},
		{name: "same project twice rejected", env: "alpha:tok-a", flags: []string{"alpha:tok-b"}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseAgentProjectConfig(tc.env, tc.flags)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parse(%q, %v) = %+v, want error", tc.env, tc.flags, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parse(%q, %v): %v", tc.env, tc.flags, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("parse = %+v, want %+v", got, tc.want)
			}
			for i := range tc.want {
				if got[i].Project != tc.want[i].Project || got[i].Token != tc.want[i].Token {
					t.Fatalf("parse[%d] = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}