package pg

import (
	"time"

	"github.com/brokenbots/castle/castle/internal/store"
)

// InstallEventAppendHook swaps the AppendEvent out-of-band-writer seam and
// returns its restore function. Exported shim (export_test.go idiom) so the
// external pg_test package can reach the unexported seam — pgtest imports
// this package, so the external test package is where the server-backed
// regression test lives.
func InstallEventAppendHook(hook func(ev *store.Event, seq uint64, ts time.Time)) (restore func()) {
	prev := eventAppendHook
	eventAppendHook = hook
	return func() { eventAppendHook = prev }
}
