package suite

import (
	"context"
	"testing"
	"time"

	"github.com/brokenbots/castle/castle/internal/store"
)

// TestProjectSettingsETagSwap (KB-324 S1) covers the ONE project settings
// doc: reads before first write are the empty doc with the empty etag; the
// first swap may pass expected ""; mismatched expectations fail with
// ErrSettingsConflict; a full read-modify-write cycle lands and re-reads
// verbatim; clearing the doc returns to the empty etag; and the whole
// surface is ErrNotFound for missing projects.
func TestProjectSettingsETagSwap(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		now := microUTC(time.Date(2026, 2, 18, 9, 15, 0, 0, time.UTC))
		newProject(t, ctx, s, "settings-proj", store.ProjectMembershipOpen, now)

		got, etag, err := s.GetProjectSettings(ctx, "settings-proj")
		if err != nil {
			t.Fatalf("GetProjectSettings fresh: %v", err)
		}
		if len(got) != 0 || etag != "" {
			t.Fatalf("fresh read: doc %q etag %q, want empty/empty", got, etag)
		}

		doc := map[string]string{
			"notifications.slack": `{"channel":"#castle"}`,
			"retention.runs":      `{"days":30}`,
		}
		newETag, err := s.SetProjectSettings(ctx, "settings-proj", doc, "")
		if err != nil {
			t.Fatalf("SetProjectSettings first write: %v", err)
		}
		if newETag == "" || newETag != store.SettingsETag(doc) {
			t.Fatalf("first write etag %q, want computed %q", newETag, store.SettingsETag(doc))
		}

		got, etag, err = s.GetProjectSettings(ctx, "settings-proj")
		if err != nil {
			t.Fatalf("GetProjectSettings after write: %v", err)
		}
		if len(got) != 2 || got["retention.runs"] != doc["retention.runs"] || etag != newETag {
			t.Fatalf("round trip: doc %q etag %q, want %q etag %q", got, etag, doc, newETag)
		}

		// Stale writer (saw the empty doc) loses against the landed one.
		if _, err := s.SetProjectSettings(ctx, "settings-proj", map[string]string{"x": "1"}, ""); err != store.ErrSettingsConflict {
			t.Fatalf("stale empty-expected swap: got %v, want ErrSettingsConflict", err)
		}
		// A plain wrong etag conflicts too.
		if _, err := s.SetProjectSettings(ctx, "settings-proj", doc, "deadbeef"); err != store.ErrSettingsConflict {
			t.Fatalf("wrong etag swap: got %v, want ErrSettingsConflict", err)
		}

		// Read-modify-write against the current etag succeeds and moves the
		// doc forward.
		next := map[string]string{"notifications.slack": `{"channel":"#ops"}`}
		nextETag, err := s.SetProjectSettings(ctx, "settings-proj", next, newETag)
		if err != nil {
			t.Fatalf("SetProjectSettings rmw: %v", err)
		}
		if nextETag != store.SettingsETag(next) {
			t.Fatalf("rmw etag %q, want %q", nextETag, store.SettingsETag(next))
		}

		// A second writer with the first etag now loses even though it
		// observed a real (older) doc.
		if _, err := s.SetProjectSettings(ctx, "settings-proj", doc, newETag); err != store.ErrSettingsConflict {
			t.Fatalf("second stale writer: got %v, want ErrSettingsConflict", err)
		}

		// Identical content re-write from the winning etag is a no-op that
		// returns the same etag.
		sameETag, err := s.SetProjectSettings(ctx, "settings-proj", next, nextETag)
		if err != nil || sameETag != nextETag {
			t.Fatalf("identical rewrite: got %q err %v, want %q nil", sameETag, err, nextETag)
		}

		// Clearing the doc (empty map) is only possible from the current
		// etag and returns the empty etag again.
		clearedETag, err := s.SetProjectSettings(ctx, "settings-proj", map[string]string{}, nextETag)
		if err != nil {
			t.Fatalf("clear: %v", err)
		}
		if clearedETag != "" {
			t.Fatalf("cleared etag %q, want empty", clearedETag)
		}
		got, etag, err = s.GetProjectSettings(ctx, "settings-proj")
		if err != nil || len(got) != 0 || etag != "" {
			t.Fatalf("after clear: doc %q etag %q err %v, want empty", got, etag, err)
		}
		// And the doc, once cleared, accepts a fresh empty-expected swap.
		again, err := s.SetProjectSettings(ctx, "settings-proj", map[string]string{"k": "2"}, "")
		if err != nil || again != store.SettingsETag(map[string]string{"k": "2"}) {
			t.Fatalf("re-first write: got %q err %v", again, err)
		}

		if _, _, err := s.GetProjectSettings(ctx, "no-such-project"); err != store.ErrNotFound {
			t.Fatalf("GetProjectSettings missing: got %v, want ErrNotFound", err)
		}
		if _, err := s.SetProjectSettings(ctx, "no-such-project", doc, ""); err != store.ErrNotFound {
			t.Fatalf("SetProjectSettings missing: got %v, want ErrNotFound", err)
		}
	})
}
