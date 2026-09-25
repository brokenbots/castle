package sqlite

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

const (
	// staleReaderGrace tolerates clock skew between the caller-supplied
	// overseer creation timestamps and the reader's view. In WAL mode a fresh
	// reader connection always sees committed writes, so a gap larger than
	// this means the reader is serving a pinned snapshot (KB-10).
	staleReaderGrace = time.Minute
	// staleReaderWarnInterval rate-limits the staleness warning so a long
	// outage cannot flood the log.
	staleReaderWarnInterval = 30 * time.Second
)

// readerFreshness tracks the newest overseer creation timestamp written
// through the writer handle so reader-backed reads (ListOverseers, the auth
// token resolution path) can be audited against what the writer has
// committed. A reader view older than the writer's newest write is the
// operator-visible symptom of a pinned WAL snapshot: registrations keep
// succeeding while CreateRun rejects the fresh tokens as unauthenticated
// (KB-10, observed 2026-09-25).
type readerFreshness struct {
	mu     sync.Mutex
	newest time.Time // newest creation timestamp written via CreateOverseer
	warned time.Time // last time the staleness warning was emitted
}

// recordWrite notes a creation timestamp observed on the writer handle.
func (f *readerFreshness) recordWrite(at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if at.After(f.newest) {
		f.newest = at
	}
}

// check compares the newest creation timestamp visible to the reader handle
// against the writer's newest. It returns true and logs (rate-limited) when
// the reader view is stale. readerNewest is zero when the reader sees no
// overseers at all.
func (f *readerFreshness) check(ctx context.Context, readerNewest time.Time, log *slog.Logger) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.newest.IsZero() || !f.newest.After(readerNewest.Add(staleReaderGrace)) {
		return false
	}
	if log == nil {
		log = slog.Default()
	}
	if f.warned.IsZero() || time.Since(f.warned) >= staleReaderWarnInterval {
		f.warned = time.Now()
		log.WarnContext(ctx,
			"sqlite: reader handle is serving a stale WAL snapshot; recently registered agents may fail CreateRun authentication until the reader unpins",
			"newest_writer_overseer", f.newest.Format(tsLayout),
			"newest_reader_overseer", readerNewest.Format(tsLayout))
	}
	return true
}