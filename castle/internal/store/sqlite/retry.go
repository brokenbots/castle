// Transient-fault retry helpers for the sqlite store (CRI-143). When SQLite
// surfaces a transient fault — a query interrupted mid-run, the database or a
// table locked by another writer — the caller must retry with backoff instead
// of propagating an internal error to RPCs or wedging the connection pool.
package sqlite

import (
	"context"
	"errors"
	"strings"
	"time"

	"modernc.org/sqlite"
)

// sqlite result codes surfaced by modernc.org/sqlite's Error.Code(). These are
// stable, documented values from the SQLite C API.
const (
	sqliteBusy      = 5 // SQLITE_BUSY
	sqliteLocked    = 6 // SQLITE_LOCKED
	sqliteInterrupt = 9 // SQLITE_INTERRUPT
)

// retryPolicy bounds the retry of a transiently failing operation.
type retryPolicy struct {
	attempts int           // total attempts, including the first
	base     time.Duration // first backoff wait
	max      time.Duration // backoff ceiling
}

// reapRetryPolicy bounds the run reaper's transient-error retries. The reaper
// ticks every 15s, so a few sub-second backoffs never delay reaping beyond the
// next tick, and a failed tick never holds pool connections while waiting.
var reapRetryPolicy = retryPolicy{attempts: 4, base: 10 * time.Millisecond, max: 250 * time.Millisecond}

// reapAttemptTimeout bounds a single reaping attempt (scan + short write) so a
// connection wedged by other work fails the tick instead of pinning the reaper
// goroutine. Generous relative to the 15s tick; each retry attempt gets a
// fresh budget.
const reapAttemptTimeout = 5 * time.Second

// isTransient reports whether err is a transient sqlite fault worth retrying.
// modernc.org/sqlite surfaces driver faults as *sqlite.Error carrying the
// result code; wrapped forms that lose the type are matched against the
// surfaced message forms (e.g. "interrupted (9)").
func isTransient(err error) bool {
	if err == nil {
		return false
	}
	var serr *sqlite.Error
	if errors.As(err, &serr) {
		return isTransientCode(serr.Code())
	}
	msg := err.Error()
	return strings.Contains(msg, "interrupted (") ||
		strings.Contains(msg, "database is locked") ||
		strings.Contains(msg, "database table is locked")
}

// isTransientCode reports whether a raw sqlite result code is transient.
// modernc.org/sqlite enables extended result codes during connection setup,
// so the code can be an extended one (SQLITE_BUSY_SNAPSHOT 517,
// SQLITE_BUSY_RECOVERY 261, SQLITE_LOCKED_SHAREDCACHE 262) whose low 8 bits
// carry the transient primary code; matching the full code exactly would
// classify those as fatal, so the primary code is masked before the switch.
func isTransientCode(code int) bool {
	switch code & 0xff {
	case sqliteBusy, sqliteLocked, sqliteInterrupt:
		return true
	}
	return false
}

// retryOnTransient runs fn until it succeeds, returns a non-transient error,
// or the policy or context is exhausted. Transient sqlite faults are retried
// with exponential backoff bounded by p.max; fn is re-invoked in full, so it
// must be safe to re-run (reads are idempotent by nature and the reaper's
// writes re-validate their criteria on every attempt).
func retryOnTransient[T any](ctx context.Context, p retryPolicy, fn func() (T, error)) (T, error) {
	var (
		out T
		err error
	)
	wait := p.base
	for attempt := 1; ; attempt++ {
		out, err = fn()
		if err == nil || !isTransient(err) || attempt >= p.attempts {
			return out, err
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return out, err
		case <-timer.C:
		}
		wait = min(2*wait, p.max)
	}
}
