package store

import "testing"

// TestSettingsETagDeterministic (KB-324 S1): the etag is content-addressed,
// not insertion-ordered, doc-specific rather than value-specific, and empty
// when the doc is unset — so an identical doc re-write can be detected as a
// no-op and a swap of the unset doc may pass expected "".
func TestSettingsETagDeterministic(t *testing.T) {
	doc := map[string]string{
		"notifications.slack": `{"channel":"#castle"}`,
		"retention.runs":      `{"days":30}`,
		"auth.write_shape":    `{"console":"settings"}`,
	}
	// Different Go map iteration orders sort to the same etag.
	fordered := map[string]string{}
	for _, k := range []string{"notifications.slack", "retention.runs", "auth.write_shape"} {
		fordered[k] = doc[k]
	}
	if got := SettingsETag(doc); got != SettingsETag(fordered) {
		t.Fatalf("etag not order-insensitive: %q vs %q", SettingsETag(doc), SettingsETag(fordered))
	}
	if got := SettingsETag(doc); len(got) != 32 {
		t.Fatalf("etag %q, want 32 hex chars", got)
	}
	// A changed value changes the etag.
	different := map[string]string{"notifications.slack": `{"channel":"#ops"}`}
	if SettingsETag(different) == SettingsETag(doc) {
		t.Fatalf("different docs share an etag")
	}
	if got := SettingsETag(nil); got != "" {
		t.Fatalf("nil doc: got %q, want empty", got)
	}
}
