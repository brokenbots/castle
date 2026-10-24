package suite

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/brokenbots/castle/castle/internal/store"
)

// archiveRec builds a store.RunArchiveRecord for tests: seq varies the record
// body and archived_at so paging and overwrite tests can observe which
// version landed. The key is the run id (run-backed archive).
func archiveRec(key string, seq int) *store.RunArchiveRecord {
	rec := archiveSyntheticRec(key, seq)
	rec.RunID = key
	return rec
}

// archiveSyntheticRec builds a record keyed by an explicit archive key with no
// castle run (run_id empty) — the run-id-less terminal-CR shape (KB-308
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

// TestRunArchiveRoundTrip covers the run-archive contract: first archive
// reports created, the full record round-trips by key, and ArchiveID renders
// the canonical archive id.
func TestRunArchiveRoundTrip(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
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
			t.Fatalf("archived_at round-trip mismatch: want %v got %v", got.ArchivedAt, rec.ArchivedAt)
		}

		if got := store.ArchiveID(rec.RunID); got != "run-archive/"+rec.RunID {
			t.Fatalf("unexpected archive id: %q", got)
		}
	})
}

// TestRunArchiveReplayOverwritesLatestWins pins replay idempotency: the
// second archive of the same key reports created=false, overwrites the record
// body and archived_at in place, and never duplicates the history row.
func TestRunArchiveReplayOverwritesLatestWins(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
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
			t.Fatalf("replay must overwrite the record body: want %s got %s", recs[0].Record, want.Record)
		}
		if !got.ArchivedAt.Equal(want.ArchivedAt) {
			t.Fatalf("replay must update archived_at: want %v got %v", want.ArchivedAt, got.ArchivedAt)
		}
	})
}

// TestRunArchiveMissingIsNotFound pins the unknown-key read path.
func TestRunArchiveMissingIsNotFound(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		_, err := s.GetRunArchiveRecord(context.Background(), "run-nope")
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("expected ErrNotFound for unknown run, got %v", err)
		}
	})
}

// TestRunArchiveValidationRejectsEmptyFields: neither a keyless record nor a
// bodyless record is stored.
func TestRunArchiveValidationRejectsEmptyFields(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
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
	})
}

// TestRunArchiveSyntheticKeyRoundTrip: run-id-less terminal CRs (no castle
// overseer run) archive under their explicit CR-derived archive_key (KB-308)
// and replay-overwrite in place by that key alone.
func TestRunArchiveSyntheticKeyRoundTrip(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
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
	})
}

// TestRunArchiveCrossIdentityOverwriteRefused pins the KB-308 provenance
// rule: first writer wins per identity. A different identity may not
// overwrite a key another identity first archived — history forgery fails,
// and the refused write touches nothing.
func TestRunArchiveCrossIdentityOverwriteRefused(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		first := archiveRec("run-abc", 1)
		if _, err := s.PutRunArchiveRecord(ctx, first); err != nil {
			t.Fatalf("first put: %v", err)
		}

		imposter := archiveRec("run-abc", 2)
		imposter.OperatorID = "criteria/agent-2"
		imposter.ArchivedAt = time.Unix(1_700_001_111, 0).UTC()
		if _, err := s.PutRunArchiveRecord(ctx, imposter); !errors.Is(err, store.ErrArchiveOperatorConflict) {
			t.Fatalf("expected ErrArchiveOperatorConflict for cross-identity overwrite, got %v", err)
		}

		got, err := s.GetRunArchiveRecord(ctx, imposter.Key)
		if err != nil {
			t.Fatalf("get after refused overwrite: %v", err)
		}
		if got.Record != first.Record || got.OperatorID != first.OperatorID || got.RunID != first.RunID {
			t.Fatalf("refused overwrite mutated history: got %+v", got)
		}
	})
}

// TestRunArchiveUnstampedRowAdoptsNextIdentity pins adoption semantics: rows
// archived without an identity (direct-store writers) are stamped by the
// first identity-bearing re-archive, after which provenance is locked.
func TestRunArchiveUnstampedRowAdoptsNextIdentity(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
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
	})
}

// TestRunArchivesKeysetPaging covers ListRunArchives paging: ascending key
// order, a continuation token that resumes exactly where the last page
// stopped, no duplicate rows across pages, the direct-store afterKey cursor,
// and a token beyond the last row terminating cleanly.
func TestRunArchivesKeysetPaging(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
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
			if want := fmt.Sprintf("run-%02d", i); rec.Key != want {
				t.Fatalf("ascending order broken at %d: got %q want %q", i, rec.Key, want)
			}
		}

		// limit=2 walks the pages; the last page has no cursor.
		page, next, err := s.ListRunArchives(ctx, "", "", 2, "")
		if err != nil {
			t.Fatalf("page1: %v", err)
		}
		if len(page) != 2 || next == "" {
			t.Fatalf("page1: len=%d next=%q", len(page), next)
		}
		seen := map[string]int{page[0].Key: 0, page[1].Key: 1}
		cursor := next
		for pagesWalked := 2; ; pagesWalked++ {
			page, next, err = s.ListRunArchives(ctx, "", "", 2, cursor)
			if err != nil {
				t.Fatalf("walk page %d: %v", pagesWalked, err)
			}
			for _, rec := range page {
				if _, dup := seen[rec.Key]; dup {
					t.Fatalf("duplicate key %q across pages", rec.Key)
				}
				seen[rec.Key] = pagesWalked
			}
			if len(page) == 0 || next == "" {
				break
			}
			cursor = next
		}
		if len(seen) != total {
			t.Fatalf("paged walk covered %d keys, want %d", len(seen), total)
		}

		// afterKey is the direct-store cursor: same keyset semantics.
		page, next, err = s.ListRunArchives(ctx, "", "run-02", 2, "")
		if err != nil {
			t.Fatalf("afterKey list: %v", err)
		}
		if len(page) != 2 || next != "" || page[0].Key != "run-03" {
			t.Fatalf("afterKey page: keys=%q,%q next=%q", page[0].Key, page[1].Key, next)
		}

		// Cursor beyond the last row terminates without error.
		page, next, err = s.ListRunArchives(ctx, "", "", 2, store.EncodeArchiveCursor("zzz"))
		if err != nil || len(page) != 0 || next != "" {
			t.Fatalf("cursor past end: err=%v len=%d next=%q", err, len(page), next)
		}
	})
}

// TestRunArchivesLimits pins the limit contract: 0 / negative fall back to
// the default page, values above ListRunArchivesMaxLimit clamp, and the
// default page size returns a cursor when more rows remain.
func TestRunArchivesLimits(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		const total = 110 // 100 caps the default page, MaxLimit clamps above
		for i := 0; i < total; i++ {
			if _, err := s.PutRunArchiveRecord(ctx, archiveRec(fmt.Sprintf("run-%04d", i), i)); err != nil {
				t.Fatalf("put %d: %v", i, err)
			}
		}

		recs, next, err := s.ListRunArchives(ctx, "", "", 0, "")
		if err != nil || len(recs) != store.ListRunArchivesDefaultLimit || next == "" {
			t.Fatalf("default limit page: err=%v len=%d next=%q", err, len(recs), next)
		}

		recs, next, err = s.ListRunArchives(ctx, "", "", store.ListRunArchivesMaxLimit*2, "")
		if err != nil {
			t.Fatalf("clamped list: %v", err)
		}
		// 110 < 1000, so the clamp is not observable here; but the request
		// must still return everything and no cursor.
		if len(recs) != total || next != "" {
			t.Fatalf("limit beyond row count: len=%d next=%q", len(recs), next)
		}

		recs, _, err = s.ListRunArchives(ctx, "", "", -5, "")
		if err != nil || len(recs) != store.ListRunArchivesDefaultLimit {
			t.Fatalf("negative limit falls back to default: err=%v len=%d", err, len(recs))
		}
	})
}

// TestRunArchivesPrefixBoundary pins prefix filtering: HasPrefix semantics
// including the row whose key equals the prefix itself, keys sharing only a
// shorter stem joining the wider prefix space, and narrowing excluding them.
func TestRunArchivesPrefixBoundary(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
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
				got = append(got, rec.Key)
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
	})
}

// TestRunArchivesInvalidCursor pins the malformed page-token rejection.
func TestRunArchivesInvalidCursor(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		if _, err := s.PutRunArchiveRecord(ctx, archiveRec("run-1", 1)); err != nil {
			t.Fatalf("put: %v", err)
		}
		for _, c := range []struct{ name, token string }{
			{"not-base64", "!!not-base64!!"},
			{"not-json", "aGVsbG8"},
			{"missing-run-id", store.EncodeArchiveCursor("")},
		} {
			_, _, err := s.ListRunArchives(ctx, "", "", 10, c.token)
			if !errors.Is(err, store.ErrInvalidCursor) {
				t.Errorf("%s: expected ErrInvalidCursor, got %v", c.name, err)
			}
		}
	})
}

// TestRunArchiveConcurrentCrossIdentity pins the first-writer-wins
// attribution under a race: two different identities archiving the same key
// concurrently must yield exactly one created result and exactly one
// documented conflict (never a raw unique violation), with the winner's row
// stored — whichever write lands first.
func TestRunArchiveConcurrentCrossIdentity(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		for i := 0; i < 20; i++ {
			key := fmt.Sprintf("cr-uid/race-%d", i)
			recA := archiveSyntheticRec(key, i)
			recA.OperatorID = "criteria/operator-a"
			recB := &store.RunArchiveRecord{
				Key:        key,
				JobName:    recA.JobName,
				FinalPhase: recA.FinalPhase,
				Ticket:     recA.Ticket,
				Record:     fmt.Sprintf(`{"writer":"b","seq":%d}`, i),
				OperatorID: "criteria/operator-b",
				ArchivedAt: recA.ArchivedAt,
			}
			recs := []*store.RunArchiveRecord{recA, recB}

			type result struct {
				created bool
				err     error
			}
			results := make([]result, 2)
			var wg sync.WaitGroup
			start := make(chan struct{})
			for j := 0; j < 2; j++ {
				wg.Add(1)
				go func(j int) {
					defer wg.Done()
					<-start
					created, err := s.PutRunArchiveRecord(ctx, recs[j])
					results[j] = result{created: created, err: err}
				}(j)
			}
			close(start)
			wg.Wait()

			var winner *store.RunArchiveRecord
			conflicts := 0
			for j, r := range results {
				switch {
				case r.err == nil && r.created:
					if winner != nil {
						t.Fatalf("iteration %d: both writers reported created for %s", i, key)
					}
					winner = recs[j]
				case errors.Is(r.err, store.ErrArchiveOperatorConflict):
					conflicts++
				default:
					t.Fatalf("iteration %d: writer %d got unexpected error (unique violation escaping?): %v", i, j, r.err)
				}
			}
			if winner == nil || conflicts != 1 {
				t.Fatalf("iteration %d: want exactly one created and one conflict, got winner=%v conflicts=%d", i, winner != nil, conflicts)
			}
			got, err := s.GetRunArchiveRecord(ctx, key)
			if err != nil {
				t.Fatalf("iteration %d: GetRunArchiveRecord: %v", i, err)
			}
			if got.Record != winner.Record || got.OperatorID != winner.OperatorID {
				t.Fatalf("iteration %d: stored row is not the winner's: got operator %q record %s, want %q %s", i, got.OperatorID, got.Record, winner.OperatorID, winner.Record)
			}
		}
	})
}
