package pg

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func stalenessTestLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// TestReaderFreshness_HealNarratesOncePerDivergence pins the KB-343 heal
// latch: a divergence narrates exactly once no matter how many reads run
// while it persists; once a read converges the episode ends and the next
// divergence narrates again.
func TestReaderFreshness_HealNarratesOncePerDivergence(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	var f readerFreshness

	// Episode 1: a committed write the served view cannot show.
	f.recordWrite(now.Add(2 * staleReaderGrace))

	buf := &bytes.Buffer{}
	log := stalenessTestLogger(buf)
	if !f.check(ctx, now, log) {
		t.Fatal("check did not report the divergence")
	}
	if !strings.Contains(buf.String(), "stale snapshot") {
		t.Fatalf("divergence audit did not warn; log = %q", buf.String())
	}

	buf.Reset()
	f.healed(ctx, log)
	if !strings.Contains(buf.String(), "healed") {
		t.Fatalf("first heal of the divergence was not logged; log = %q", buf.String())
	}

	// A persisting (or false-positive) divergence never narrates again.
	for range 5 {
		buf.Reset()
		f.healed(ctx, log)
		if strings.Contains(buf.String(), "healed") {
			t.Fatalf("heal logged again while the same divergence persisted; log = %q", buf.String())
		}
	}

	// The reader view converges: the episode ends and the latch clears.
	f.converged()

	// Episode 2 narrates again (the 30s heal rate limiter is bypassed by
	// resetting its clock, so the latch alone gates this leg).
	f.lastHeal = time.Time{}
	f.recordWrite(now.Add(4 * staleReaderGrace))
	buf.Reset()
	f.healed(ctx, log)
	if !strings.Contains(buf.String(), "healed") {
		t.Fatalf("heal after a converged read was not logged; log = %q", buf.String())
	}
}