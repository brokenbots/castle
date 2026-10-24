// smoke_test.go verifies the ephemeral-server boot path end to end: one
// store open, one write, one read. It is a fixture self-check, not part of
// the behavioral suite.
package pgtest

import (
	"context"
	"testing"
	"time"

	"github.com/brokenbots/castle/castle/internal/store"
)

func TestPGTestSmoke(t *testing.T) {
	s := Store(t)
	ctx := context.Background()
	now := time.Now()
	if err := s.CreateOverseer(ctx, &store.Overseer{ID: "ov-1", Name: "n", TokenHash: "h", Status: "online", CreatedAt: now, LastSeenAt: now}); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetOverseer(ctx, "ov-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "ov-1" {
		t.Fatalf("roundtrip got %q", got.ID)
	}
}
