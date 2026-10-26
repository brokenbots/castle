package suite

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	confighome "github.com/brokenbots/castle/castle/internal/confighome"
	"github.com/brokenbots/castle/castle/internal/store"
)

// minimalRecord returns a valid record doc pair distinct only in
// registryName, for swap tests.
func minimalRecord(name string) ([]byte, error) {
	rec := &confighome.Record{
		APIVersion:   confighome.APIVersion,
		Kind:         confighome.KindRecord,
		RegistryName: name,
		Namespace:    "criteria-jobs",
	}
	return rec.Marshal()
}

// TestProjectConfigETagSwap (KB-339) covers the ONE project workflow config
// record: reads before first write are the empty doc with the empty etag; the
// first swap may pass expected ""; mismatched expectations fail with
// ErrProjectConfigConflict; a read-modify-write cycle lands and re-reads
// verbatim; clearing the record returns to the empty etag; equivalent
// byte-wise variants collapse to the same etag; and the whole surface is
// ErrNotFound for missing projects.
func TestProjectConfigETagSwap(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		now := microUTC(time.Date(2026, 2, 18, 9, 15, 0, 0, time.UTC))
		newProject(t, ctx, s, "config-proj", store.ProjectMembershipOpen, now)

		got, etag, err := s.GetProjectConfig(ctx, "config-proj")
		if err != nil {
			t.Fatalf("GetProjectConfig fresh: %v", err)
		}
		if len(got) != 0 || etag != "" {
			t.Fatalf("fresh read: doc %q etag %q, want empty/empty", got, etag)
		}

		doc, err := minimalRecord("config-proj")
		if err != nil {
			t.Fatalf("minimal record: %v", err)
		}
		newETag, err := s.SetProjectConfig(ctx, "config-proj", doc, "")
		if err != nil {
			t.Fatalf("SetProjectConfig first write: %v", err)
		}
		if newETag == "" || newETag != store.ProjectConfigETag(doc) {
			t.Fatalf("first write etag %q, want computed %q", newETag, store.ProjectConfigETag(doc))
		}

		got, etag, err = s.GetProjectConfig(ctx, "config-proj")
		if err != nil {
			t.Fatalf("GetProjectConfig after write: %v", err)
		}
		// The store canonicalizes on write: a re-read is stable across
		// swap cycles and its etag matches its own bytes.
		storedETag, err := s.SetProjectConfig(ctx, "config-proj", got, etag)
		if err != nil || storedETag != etag {
			t.Fatalf("re-stored form: got %q err %v, want %q nil", storedETag, err, etag)
		}

		// Stale writer (saw the empty record) loses against the landed one.
		if _, err := s.SetProjectConfig(ctx, "config-proj", doc, ""); err != store.ErrProjectConfigConflict {
			t.Fatalf("stale empty-expected swap: got %v, want ErrProjectConfigConflict", err)
		}
		// A plain wrong etag conflicts too.
		if _, err := s.SetProjectConfig(ctx, "config-proj", doc, "deadbeef"); err != store.ErrProjectConfigConflict {
			t.Fatalf("wrong etag swap: got %v, want ErrProjectConfigConflict", err)
		}

		// Read-modify-write against the current etag succeeds and moves the
		// record forward.
		next, err := minimalRecord("config-proj-next")
		if err != nil {
			t.Fatalf("next record: %v", err)
		}
		nextETag, err := s.SetProjectConfig(ctx, "config-proj", next, newETag)
		if err != nil {
			t.Fatalf("SetProjectConfig rmw: %v", err)
		}
		if nextETag != store.ProjectConfigETag(next) {
			t.Fatalf("rmw etag %q, want %q", nextETag, store.ProjectConfigETag(next))
		}

		// A second writer with the first etag now loses even though it
		// observed a real (older) record.
		if _, err := s.SetProjectConfig(ctx, "config-proj", doc, newETag); err != store.ErrProjectConfigConflict {
			t.Fatalf("second stale writer: got %v, want ErrProjectConfigConflict", err)
		}

		// Identical content re-write from the winning etag is a no-op that
		// returns the same etag.
		sameETag, err := s.SetProjectConfig(ctx, "config-proj", next, nextETag)
		if err != nil || sameETag != nextETag {
			t.Fatalf("identical rewrite: got %q err %v, want %q nil", sameETag, err, nextETag)
		}

		// clearing the record (nil doc) is only possible from the current
		// etag and returns the empty etag again.
		clearedETag, err := s.SetProjectConfig(ctx, "config-proj", nil, nextETag)
		if err != nil {
			t.Fatalf("clear: %v", err)
		}
		if clearedETag != "" {
			t.Fatalf("cleared etag %q, want empty", clearedETag)
		}
		got, etag, err = s.GetProjectConfig(ctx, "config-proj")
		if err != nil || len(got) != 0 || etag != "" {
			t.Fatalf("after clear: doc %q etag %q err %v, want empty", got, etag, err)
		}
		// And the record, once cleared, accepts a fresh empty-expected swap.
		again, err := s.SetProjectConfig(ctx, "config-proj", doc, "")
		if err != nil || again != store.ProjectConfigETag(doc) {
			t.Fatalf("re-first write: got %q err %v", again, err)
		}

		if _, _, err := s.GetProjectConfig(ctx, "no-such-project"); err != store.ErrNotFound {
			t.Fatalf("GetProjectConfig missing: got %v, want ErrNotFound", err)
		}
		if _, err := s.SetProjectConfig(ctx, "no-such-project", doc, ""); err != store.ErrNotFound {
			t.Fatalf("SetProjectConfig missing: got %v, want ErrNotFound", err)
		}
	})
}

// TestProjectConfigClosesByteVariants pins the canonical-form invariant:
// two byte-wise variants of the same record (re-formatted whitespace) carry
// the SAME etag, and a swap written in one variant lands the canonical
// storage form — the rendered ConfigMap payloads derive from deterministic
// bytes.
func TestProjectConfigClosesByteVariants(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		now := microUTC(time.Date(2026, 2, 18, 9, 40, 0, 0, time.UTC))
		newProject(t, ctx, s, "variant-proj", store.ProjectMembershipOpen, now)

		canonical, err := minimalRecord("variant-proj")
		if err != nil {
			t.Fatalf("canonical record: %v", err)
		}

		// A byte-wise variant twin (pretty-printed) of the same record.
		pretty, err := json.MarshalIndent(&confighome.Record{
			APIVersion:   confighome.APIVersion,
			Kind:         confighome.KindRecord,
			RegistryName: "variant-proj",
			Namespace:    "criteria-jobs",
		}, "", "  ")
		if err != nil {
			t.Fatalf("pretty record: %v", err)
		}
		if string(pretty) == string(canonical) {
			t.Fatalf("test bug: twins identical")
		}

		if _, err := s.SetProjectConfig(ctx, "variant-proj", canonical, ""); err != nil {
			t.Fatalf("canonical write: %v", err)
		}
		etag, err := s.SetProjectConfig(ctx, "variant-proj", pretty, store.ProjectConfigETag(canonical))
		if err != nil {
			t.Fatalf("variant swap: %v", err)
		}
		if etag != store.ProjectConfigETag(canonical) {
			t.Fatalf("variant etag %q, want canonical %q", etag, store.ProjectConfigETag(canonical))
		}
	})
}

// TestProjectConfigRejectsNonJSON pins the store's storage-form guards: a
// doc that is not JSON fails with ErrInvalidWorkflowConfig and the project's
// record stays untouched.
func TestProjectConfigRejectsNonJSON(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		now := microUTC(time.Date(2026, 2, 18, 9, 42, 0, 0, time.UTC))
		newProject(t, ctx, s, "bad-json-proj", store.ProjectMembershipOpen, now)

		if _, err := s.SetProjectConfig(ctx, "bad-json-proj", []byte("not json{"), ""); err != store.ErrInvalidWorkflowConfig {
			t.Fatalf("non-JSON write: got %v, want ErrInvalidWorkflowConfig", err)
		}
		if _, etag, err := s.GetProjectConfig(ctx, "bad-json-proj"); err != nil || len(etag) != 0 {
			t.Fatalf("record after failed write: etag %q err %v, want empty/nil", etag, err)
		}
	})
}
