package pg

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/brokenbots/castle/castle/internal/store"
)

const (
	// staleReaderGrace tolerates clock skew between the caller-supplied
	// overseer creation timestamps and the reader's view (KB-317 parity with
	// the sqlite profile's WAL snapshot detection).
	staleReaderGrace = time.Minute
	// staleReaderWarnInterval rate-limits the staleness warning so a long
	// outage cannot flood the log.
	staleReaderWarnInterval = 30 * time.Second
)

// readerFreshness tracks the newest overseer creation timestamp written
// through the writer pool so reader-backed reads (ListOverseers, the auth
// token resolution path) can be audited against what the writer has
// committed. On Postgres both handles share one pool and MVCC reads always
// see committed writes, so the audit is expected to stay quiet; the
// machinery is kept so the KB-10/KB-223 telemetry contract — and every
// engine-parity test around it — behaves identically on both engines.
type readerFreshness struct {
	mu       sync.Mutex
	newest   time.Time // newest creation timestamp written via CreateOverseer
	warned   time.Time // last time the staleness warning was emitted
	lastHeal time.Time // last time the heal notice was emitted (KB-223)
	// behind records that the most recently audited reader view was missing a
	// committed overseer write, or proved fresh after a gap. It gates the
	// OverseerTokenHashPresent writer probe (KB-223).
	behind bool
	// narrated records that the current divergence episode already produced
	// its heal notice (KB-343): a gap that persists across reads — or a
	// false-positive one — narrates once and then stays quiet, and a
	// converged read ends the episode so the next gap narrates anew.
	narrated bool
}

// recordWrite notes a creation timestamp observed on the writer pool.
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
			"pg: reader view is serving a stale snapshot; recently registered agents may fail CreateRun authentication",
			"newest_writer_overseer", f.newest.Format(store.TimestampLayout),
			"newest_reader_overseer", readerNewest.Format(store.TimestampLayout))
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

// converged records that the reader view caught up with committed writes,
// ending the current divergence episode so a later gap narrates anew (KB-343).
func (f *readerFreshness) converged() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.narrated = false
}

// healed clears the behind flag and narrates the recovery once the
// authoritative read confirmed the previously-missing rows (KB-223). A
// divergence episode narrates exactly once (KB-343): ListOverseers calls
// healed on every read while a gap persists, and the latch keeps repeat
// calls — including rate-limiter-window ones — from re-flooding the log.
func (f *readerFreshness) healed(ctx context.Context, log *slog.Logger) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.behind = false
	if f.narrated {
		return
	}
	if log == nil {
		log = slog.Default()
	}
	if f.lastHeal.IsZero() || time.Since(f.lastHeal) >= staleReaderWarnInterval {
		f.lastHeal = time.Now()
		f.narrated = true
		log.WarnContext(ctx, "pg: reader view healed; authoritative overseer rows are visible again")
	}
}
