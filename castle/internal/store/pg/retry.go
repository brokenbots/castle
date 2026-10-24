package pg

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
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

// reapAttemptTimeout bounds a single reaping attempt (scan + short write) so
// contention fails the tick instead of pinning the reaper goroutine. Generous
// relative to the 15s tick; each retry attempt gets a fresh budget.
const reapAttemptTimeout = 5 * time.Second

// isTransient reports whether err is a transient Postgres fault worth
// retrying: serialization failures and deadlocks (the analogues of sqlite's
// BUSY/LOCKED) plus pool/connection-level faults. Statement timeouts surfaced
// as SQLSTATE 57014 are NOT retried — on pg there is no driver-level interrupt
// (KB-222 does not apply), and a canceled statement means its own context
// bounded it.
func isTransient(err error) bool {
	if err == nil {
		return false
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "40001": // serialization_failure
			return true
		case "40P01": // deadlock_detected
			return true
		}
		return false
	}
	return containsAny(err.Error(),
		"unexpected EOF",
		"connection reset",
		"broken pipe",
		"conn busy",
		"connection refused",
		"closed pool",
	)
}

// containsAny reports whether msg contains any of the needle substrings.
func containsAny(msg string, needles ...string) bool {
	for _, n := range needles {
		if n != "" && strings.Contains(msg, n) {
			return true
		}
	}
	return false
}

// isInterrupt has no Postgres analogue — there is no driver-level
// sqlite3_interrupt-style whole-connection disturbance (KB-222 stays a sqlite
// concept). The reaper's recoverWriter hook is retained for parity but never
// fires on pg.
func isInterrupt(err error) bool {
	return false
}

// retryOnTransient runs fn until it succeeds, returns a non-transient error,
// or the policy or context is exhausted. Transient faults are retried with
// exponential backoff bounded by p.max; fn is re-invoked in full, so it must
// be safe to re-run (reads are idempotent by nature and the reaper's writes
// re-validate their criteria on every attempt).
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
