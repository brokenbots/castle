package store

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"
)

// TestDecodeArchiveCursor_RoundTrip pins the cursor contract: empty token
// means "no cursor" (fresh listing), well-formed tokens round-trip exactly,
// and a payload that decodes but carries no key can never advance paging.
func TestDecodeArchiveCursor_RoundTrip(t *testing.T) {
	if key, err := DecodeArchiveCursor(""); err != nil || key != "" {
		t.Fatalf("empty cursor must stay a fresh listing: key=%q err=%v", key, err)
	}

	if got, err := DecodeArchiveCursor(EncodeArchiveCursor("run-9")); err != nil || got != "run-9" {
		t.Fatalf("round trip: got=%q err=%v", got, err)
	}

	emptyJSON, err := json.Marshal(map[string]string{"key": ""})
	if err != nil {
		t.Fatal(err)
	}
	tok := base64.RawURLEncoding.EncodeToString(emptyJSON)
	if _, err := DecodeArchiveCursor(tok); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("empty-key cursor: expected ErrInvalidCursor, got %v", err)
	}
}

func TestPrefixUpperBound(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"abc", "abd"},
		{"run", "ruo"},
		{"run-1", "run-2"},
		{"a\x7f", "a\x80"},
		{"\xff", ""},
		{"a\xff\xff", "b"},
		{"", ""}, // empty prefix never reaches this helper
	}
	for _, tc := range cases {
		if got := PrefixUpperBound(tc.in); got != tc.want {
			t.Errorf("PrefixUpperBound(%q)=%q want %q", tc.in, got, tc.want)
		}
	}
}
