package sqlite

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/brokenbots/castle/castle/internal/store"
)

// archiveRec builds a store.RunArchiveRecord for tests: seq varies the
// record body and archived_at so paging and overwrite tests can observe
// which version landed. The key is the run id (run-backed archive).
func archiveRec(key string, seq int) *store.RunArchiveRecord {
	rec := archiveSyntheticRec(key, seq)
	rec.RunID = key
	return rec
}

// archiveSyntheticRec builds a record keyed by an explicit archive key with
// no castle run (run_id empty) — the run-id-less terminal-CR shape (KB-308
// archive_key contract).
func archiveSyntheticRec(key string, seq int) *store.RunArchiveRecord {
	return &store.RunArchiveRecord{
		Key:        key,
		JobName:    "job-" + key,
		FinalPhase: "succeeded",
		Ticket:     "ticket-42",
		Record:     fmt.Sprintf(`{"runId":%q,"schemaVersion":1,"seq":%d}`, key, seq),
		OperatorID: "criteria/agent-1",
		ArchivedAt: time.Unix(1_700_000_000+int64(seq)*10, 0).UTC(),
	}
}

func TestPutRunArchiveRecord_RoundTrip(t *testing.T) {
	s := tempStore(t)
	ctx := context.Background()

	rec := archiveRec("run-abc", 1)
	created, err := s.PutRunArchiveRecord(ctx, rec)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if !created {
		t.Fatal("first archive of a run must report created=true")
	}

	got, err := s.GetRunArchiveRecord(ctx, rec.Key)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Key != rec.Key || got.RunID != rec.RunID || got.JobName != rec.JobName ||
		got.FinalPhase != rec.FinalPhase || got.Ticket != rec.Ticket ||
		got.OperatorID != rec.OperatorID {
		t.Fatalf("column round-trip mismatch: got %+v", got)
	}
	if got.Record != rec.Record {
		t.Fatalf("record body round-trip mismatch:\nwant %s\ngot  %s", rec.Record, got.Record)
	}
	if !got.ArchivedAt.Equal(rec.ArchivedAt) {
		t.Fatalf("archived_at round-trip mismatch: want %v got %v", rec.ArchivedAt, got.ArchivedAt)
	}

	if got := store.ArchiveID(rec.RunID); got != "run-archive/"+rec.RunID {
		t.Fatalf("unexpected archive id: %q", got)
	}
}

func TestPutRunArchiveRecord_ReplayOverwritesLatestWins(t *testing.T) {
	s := tempStore(t)
	ctx := context.Background()

	if _, err := s.PutRunArchiveRecord(ctx, archiveRec("run-abc", 1)); err != nil {
		t.Fatalf("first put: %v", err)
	}
	created, err := s.PutRunArchiveRecord(ctx, archiveRec("run-abc", 2))
	if err != nil {
		t.Fatalf("replay put: %v", err)
	}
	if created {
		t.Fatal("second archive of the same run must report created=false")
	}

	// Exactly one store row — the retry leg can never duplicate history.
	recs, _, err := s.ListRunArchives(ctx, "", "", 0, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("expected single record after replay, got %d", len(recs))
	}
	want := archiveRec("run-abc", 2)
	got := recs[0]
	if got.Record != want.Record {
		t.Fatalf("replay must overwrite the record body: want %s got %s", want.Record, got.Record)
	}
	if !got.ArchivedAt.Equal(want.ArchivedAt) {
		t.Fatalf("replay must update archived_at: want %v got %v", want.ArchivedAt, got.ArchivedAt)
	}
}

func TestGetRunArchiveRecord_Missing(t *testing.T) {
	s := tempStore(t)
	_, err := s.GetRunArchiveRecord(context.Background(), "run-nope")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound for unknown run, got %v", err)
	}
}

func TestPutRunArchiveRecord_Validation(t *testing.T) {
	s := tempStore(t)
	ctx := context.Background()

	for _, rec := range []*store.RunArchiveRecord{
		{Record: "{}"}, // no archive key
		{Key: "run-x"}, // no record body
	} {
		if _, err := s.PutRunArchiveRecord(ctx, rec); err == nil {
			t.Errorf("expected error for %+v", rec)
		}
	}
	if _, err := s.GetRunArchiveRecord(ctx, "run-x"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("rejected records must not be stored, got %v", err)
	}
}

func TestPutRunArchiveRecord_SyntheticKeyRoundTrip(t *testing.T) {
	// Run-id-less terminal CRs (no castle overseer run) archive under their
	// explicit CR-derived archive_key (KB-308 archive_key contract) and
	// round-trip by that key alone.
	s := tempStore(t)
	ctx := context.Background()

	rec := archiveSyntheticRec("cr-uid/abc-123", 7)
	created, err := s.PutRunArchiveRecord(ctx, rec)
	if err != nil {
		t.Fatalf("put synthetic-key record: %v", err)
	}
	if !created {
		t.Fatal("first archive of a key must report created=true")
	}

	got, err := s.GetRunArchiveRecord(ctx, rec.Key)
	if err != nil {
		t.Fatalf("get by synthetic key: %v", err)
	}
	if got.Key != rec.Key || got.RunID != "" {
		t.Fatalf("synthetic-key row mismatch: got %+v", got)
	}
	if got.Record != rec.Record {
		t.Fatalf("record body round-trip mismatch:\nwant %s\ngot  %s", rec.Record, got.Record)
	}

	// Replay of the same key overwrites in place: created=false, one row.
	created, err = s.PutRunArchiveRecord(ctx, archiveSyntheticRec(rec.Key, 8))
	if err != nil {
		t.Fatalf("replay put: %v", err)
	}
	if created {
		t.Fatal("second archive of the same key must report created=false")
	}
	recs, _, err := s.ListRunArchives(ctx, "", "", 0, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(recs) != 1 || recs[0].Record != archiveSyntheticRec(rec.Key, 8).Record {
		t.Fatalf("replay must overwrite in place, got %d records", len(recs))
	}
}

func TestPutRunArchiveRecord_CrossIdentityOverwriteRefused(t *testing.T) {
	// Archive history attribution is first-writer-wins per identity
	// (KB-308): a different identity than the one that first archived a key
	// may not overwrite it — history forgery/corruption must fail.
	s := tempStore(t)
	ctx := context.Background()

	first := archiveRec("run-abc", 1)
	if _, err := s.PutRunArchiveRecord(ctx, first); err != nil {
		t.Fatalf("first put: %v", err)
	}

	imposter := archiveRec("run-abc", 2)
	imposter.OperatorID = "criteria/agent-2"
	imposter.ArchivedAt = time.Unix(1_700_001_111, 0).UTC()
	_, err := s.PutRunArchiveRecord(ctx, imposter)
	if !errors.Is(err, store.ErrArchiveOperatorConflict) {
		t.Fatalf("expected ErrArchiveOperatorConflict for cross-identity overwrite, got %v", err)
	}

	// The refused write must not have touched the stored history.
	got, err := s.GetRunArchiveRecord(ctx, imposter.Key)
	if err != nil {
		t.Fatalf("get after refused overwrite: %v", err)
	}
	if got.Record != first.Record || got.OperatorID != first.OperatorID || got.RunID != first.RunID {
		t.Fatalf("refused overwrite mutated history: got %+v", got)
	}
}

func TestPutRunArchiveRecord_UnstampedRowAdoptsNextIdentity(t *testing.T) {
	// Rows archived without any identity (direct-store writers) can be
	// adopted — stamped — by the first identity-bearing re-archive; after
	// that the provenance is locked against other identities.
	s := tempStore(t)
	ctx := context.Background()

	unstamped := archiveRec("run-def", 1)
	unstamped.OperatorID = ""
	if _, err := s.PutRunArchiveRecord(ctx, unstamped); err != nil {
		t.Fatalf("first put: %v", err)
	}

	claimed := archiveRec("run-def", 2) // same agent-1 identity
	created, err := s.PutRunArchiveRecord(ctx, claimed)
	if err != nil {
		t.Fatalf("identity-bearing replay on unstamped row: %v", err)
	}
	if created {
		t.Fatal("adoption of an unstamped row is an overwrite")
	}
	got, err := s.GetRunArchiveRecord(ctx, claimed.Key)
	if err != nil {
		t.Fatalf("get after adoption: %v", err)
	}
	if got.OperatorID != claimed.OperatorID {
		t.Fatalf("adoption must stamp the new writer identity: got %q want %q", got.OperatorID, claimed.OperatorID)
	}

	locked := archiveRec("run-def", 3)
	locked.OperatorID = "criteria/agent-2"
	if _, err := s.PutRunArchiveRecord(ctx, locked); !errors.Is(err, store.ErrArchiveOperatorConflict) {
		t.Fatalf("adopted row must lock provenance, got %v", err)
	}
}

func TestListRunArchives_KeysetPaging(t *testing.T) {
	s := tempStore(t)
	ctx := context.Background()

	const total = 5
	for i := 0; i < total; i++ {
		if _, err := s.PutRunArchiveRecord(ctx, archiveRec(fmt.Sprintf("run-%02d", i), i)); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}

	// Default page: everything fits.
	all, next, err := s.ListRunArchives(ctx, "", "", 0, "")
	if err != nil {
		t.Fatalf("default list: %v", err)
	}
	if len(all) != total || next != "" {
		t.Fatalf("default page: len=%d next=%q", len(all), next)
	}
	for i, rec := range all {
		want := fmt.Sprintf("run-%02d", i)
		if rec.RunID != want {
			t.Fatalf("ascending order broken at %d: got %q want %q", i, rec.RunID, want)
		}
	}

	// limit=2 walks three pages; the last page has no cursor.
	page, next, err := s.ListRunArchives(ctx, "", "", 2, "")
	if err != nil {
		t.Fatalf("page1: %v", err)
	}
	if len(page) != 2 || next == "" {
		t.Fatalf("page1: len=%d next=%q", len(page), next)
	}
	seen := map[string]int{page[0].RunID: 0, page[1].RunID: 1}
	cursor := next
	for pagesWalked := 2; ; pagesWalked++ {
		page, next, err = s.ListRunArchives(ctx, "", "", 2, cursor)
		if err != nil {
			t.Fatalf("walk page %d: %v", pagesWalked, err)
		}
		for _, rec := range page {
			if _, dup := seen[rec.RunID]; dup {
				t.Fatalf("duplicate id %q across pages", rec.RunID)
			}
			seen[rec.RunID] = pagesWalked
		}
		if len(page) == 0 || next == "" {
			break
		}
		cursor = next
	}
	if len(seen) != total {
		t.Fatalf("paged walk covered %d ids, want %d", len(seen), total)
	}

	// afterRunID is the direct-store cursor: same keyset semantics.
	page, next, err = s.ListRunArchives(ctx, "", "run-02", 2, "")
	if err != nil {
		t.Fatalf("afterRunID list: %v", err)
	}
	if len(page) != 2 || next != "" || page[0].RunID != "run-03" {
		t.Fatalf("afterRunID page: ids=%q,%q next=%q", page[0].RunID, page[1].RunID, next)
	}

	// Cursor beyond the last row terminates without error.
	page, next, err = s.ListRunArchives(ctx, "", "", 2, encodeArchiveCursor("zzz"))
	if err != nil || len(page) != 0 || next != "" {
		t.Fatalf("cursor past end: err=%v len=%d next=%q", err, len(page), next)
	}
}

func TestListRunArchives_Limits(t *testing.T) {
	s := tempStore(t)
	ctx := context.Background()

	const total = 110 // low cost; 100 caps the default page, MaxLimit clamps
	for i := 0; i < total; i++ {
		if _, err := s.PutRunArchiveRecord(ctx, archiveRec(fmt.Sprintf("run-%04d", i), i)); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}

	recs, next, err := s.ListRunArchives(ctx, "", "", 0, "")
	if err != nil || len(recs) != ListRunArchivesDefaultLimit || next == "" {
		t.Fatalf("default limit page: err=%v len=%d next=%q", err, len(recs), next)
	}

	recs, next, err = s.ListRunArchives(ctx, "", "", ListRunArchivesMaxLimit*2, "")
	if err != nil {
		t.Fatalf("clamped list: %v", err)
	}
	// 110 < 1000, so the clamp is not observable here; but the request must
	// still return everything and no cursor.
	if len(recs) != total || next != "" {
		t.Fatalf("limit beyond row count: len=%d next=%q", len(recs), next)
	}

	recs, _, err = s.ListRunArchives(ctx, "", "", -5, "")
	if err != nil || len(recs) != ListRunArchivesDefaultLimit {
		t.Fatalf("negative limit falls back to default: err=%v len=%d", err, len(recs))
	}
}

func TestListRunArchives_PrefixBoundary(t *testing.T) {
	s := tempStore(t)
	ctx := context.Background()

	for _, id := range []string{"run", "run-1", "run-2", "runs-x", "other-1", "runs"} {
		if _, err := s.PutRunArchiveRecord(ctx, archiveRec(id, 1)); err != nil {
			t.Fatalf("put %q: %v", id, err)
		}
	}

	cases := []struct {
		prefix string
		want   []string
	}{
		// HasPrefix semantics, including the row whose id equals the
		// prefix itself; ids sharing only a shorter stem ("runs-x",
		// "runs") join the "run" space and ids outside are excluded.
		{"run", []string{"run", "run-1", "run-2", "runs", "runs-x"}},
		// Narrowing past the stem excludes "run" and "runs-x"/"runs".
		{"run-", []string{"run-1", "run-2"}},
		{"other", []string{"other-1"}},
		{"zzz", nil},
	}
	for _, tc := range cases {
		recs, next, err := s.ListRunArchives(ctx, tc.prefix, "", 0, "")
		if err != nil {
			t.Fatalf("list prefix %q: %v", tc.prefix, err)
		}
		got := make([]string, 0, len(recs))
		for _, rec := range recs {
			got = append(got, rec.RunID)
		}
		if next != "" {
			t.Fatalf("prefix %q page must be complete when it fits, got next=%q", tc.prefix, next)
		}
		if len(got) != len(tc.want) {
			t.Fatalf("prefix %q: got %q want %q", tc.prefix, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("prefix %q: got %q want %q", tc.prefix, got, tc.want)
			}
		}
	}
}
func TestListRunArchives_InvalidCursor(t *testing.T) {
	s := tempStore(t)
	if _, err := s.PutRunArchiveRecord(context.Background(), archiveRec("run-1", 1)); err != nil {
		t.Fatalf("put: %v", err)
	}
	for _, c := range []struct{ name, token string }{
		{"not-base64", "!!not-base64!!"},
		{"not-json", "aGVsbG8"},
		{"missing-run-id", encodeArchiveCursor("")},
	} {
		_, _, err := s.ListRunArchives(context.Background(), "", "", 10, c.token)
		if !errors.Is(err, store.ErrInvalidCursor) {
			t.Errorf("%s: expected ErrInvalidCursor, got %v", c.name, err)
		}
	}
}

func TestDecodeArchiveCursor_RoundTrip(t *testing.T) {
	// Empty token means "no cursor": a fresh listing, not an error.
	if id, err := decodeArchiveCursor(""); err != nil || id != "" {
		t.Fatalf("empty cursor must stay a fresh listing: id=%q err=%v", id, err)
	}

	if got, err := decodeArchiveCursor(encodeArchiveCursor("run-9")); err != nil || got != "run-9" {
		t.Fatalf("round trip: got=%q err=%v", got, err)
	}

	// A token whose payload has an empty run_id cannot advance paging.
	emptyJSON, err := json.Marshal(archiveCursor{})
	if err != nil {
		t.Fatal(err)
	}
	tok := base64.RawURLEncoding.EncodeToString(emptyJSON)
	if _, err := decodeArchiveCursor(tok); !errors.Is(err, store.ErrInvalidCursor) {
		t.Fatalf("empty-run_id cursor: expected ErrInvalidCursor, got %v", err)
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
		if got := prefixUpperBound(tc.in); got != tc.want {
			t.Errorf("prefixUpperBound(%q)=%q want %q", tc.in, got, tc.want)
		}
	}
}
