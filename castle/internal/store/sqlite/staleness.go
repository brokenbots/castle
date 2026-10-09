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
	mu       sync.Mutex
	newest   time.Time // newest creation timestamp written via CreateOverseer
	warned   time.Time // last time the staleness warning was emitted
	lastHeal time.Time // last time the heal notice was emitted (KB-223)
	// behind records that the most recently audited reader view was missing a
	// committed overseer write, or proved fresh after a gap. It gates the
	// OverseerTokenHashPresent writer probe so failed-token traffic only ever
	// reaches the writer connection while a visibility gap is actually
	// suspected (KB-223).
	behind bool
}

// recordWrite notes a creation timestamp observed on the writer handle.
func (f *readerFreshness) recordWrite(at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if at.After(f.newest) {
		f.newest = at
	}
}

// newerThan reports whether the writer has committed an overseer creation
// strictly newer than at. A zero at means the reader saw no overseers at all,
// so any recorded write counts.
func (f *readerFreshness) newerThan(at time.Time) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.newest.IsZero() {
		return false
	}
	if at.IsZero() {
		return true
	}
	return f.newest.After(at)
}

// noteBehind records whether the last audited reader view was behind the
// writer's committed state.
func (f *readerFreshness) noteBehind(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.behind = v
}

// behind reports whether the last audited reader view was behind the writer's
// committed state. While false, token-resolution misses cannot be explained
// by reader lag.
func (f *readerFreshness) readerBehind() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.behind
}

// audit compares the newest creation timestamp visible to the reader view
// against the writer's newest. It returns true and logs (rate-limited) when
// the reader view is stale. readerNewest is zero when the view has no
// overseers at all.
func (f *readerFreshness) audit(ctx context.Context, readerNewest time.Time, log *slog.Logger) bool {
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

// check audits the reader view against the writer's newest and records the
// outcome as the behind flag that gates the OverseerTokenHashPresent probe.
func (f *readerFreshness) check(ctx context.Context, readerNewest time.Time, log *slog.Logger) bool {
	stale := f.audit(ctx, readerNewest, log)
	f.noteBehind(stale)
	return stale
}

// healed logs (rate-limited) that a ListOverseers call served the writer view
// because the reader view was behind committed writes (KB-223). Heal events
// are bounded by the registration rate, so each is logged; the limiter only
// exists so a rapidly repeating gap cannot flood the log.
func (f *readerFreshness) healed(ctx context.Context, log *slog.Logger) {
	if log == nil {
		log = slog.Default()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.behind = true
	if f.lastHeal.IsZero() || time.Since(f.lastHeal) >= staleReaderWarnInterval {
		f.lastHeal = time.Now()
		log.WarnContext(ctx,
			"sqlite: reader view was behind the writer; served the authoritative writer view for overseers (token resolution is unaffected, but investigate reader staleness)",
			"newest_writer_overseer", f.newest.Format(tsLayout))
	}
}
