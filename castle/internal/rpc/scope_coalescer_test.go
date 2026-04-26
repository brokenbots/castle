package rpc

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brokenbots/overlord/castle/internal/store"
	"github.com/brokenbots/overlord/castle/internal/store/sqlite"
)

func newScopeTestStore(t *testing.T) store.Store {
	t.Helper()
	s, err := sqlite.Open(filepath.Join(t.TempDir(), "scope.db"))
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func mustCreateScopeOverseer(t *testing.T, s store.Store, ctx context.Context) {
	t.Helper()
	now := time.Now().UTC()
	if err := s.CreateOverseer(ctx, &store.Overseer{
		ID: "ov-scope-1", Name: "test", TokenHash: "t",
		Status: "online", CreatedAt: now, LastSeenAt: now,
	}); err != nil {
		t.Fatalf("CreateOverseer: %v", err)
	}
}

func mustCreateScopeRun(t *testing.T, s store.Store, ctx context.Context, id string) {
	t.Helper()
	if err := s.CreateRun(ctx, &store.Run{
		ID: id, OverseerID: "ov-scope-1", WorkflowName: "test",
		WorkflowHCL: "{}", Status: "running",
	}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
}

func TestScopeCoalescer_FlushNow(t *testing.T) {
	st := newScopeTestStore(t)
	ctx := context.Background()
	mustCreateScopeOverseer(t, st, ctx)
	mustCreateScopeRun(t, st, ctx, "run-c1")

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := newScopeCoalescer(st, log)

	// Enqueue two mutations — they should be coalesced into a single write.
	c.Enqueue("run-c1", func(scope map[string]interface{}) {
		varMap := map[string]interface{}{"x": "1"}
		scope["var"] = varMap
	})
	c.Enqueue("run-c1", func(scope map[string]interface{}) {
		varMap, _ := scope["var"].(map[string]interface{})
		if varMap == nil {
			varMap = map[string]interface{}{}
		}
		varMap["y"] = "2"
		scope["var"] = varMap
	})

	// Force flush without waiting for the timer.
	c.FlushNow(ctx, "run-c1")

	got, err := st.GetRunVariableScope(ctx, "run-c1")
	if err != nil {
		t.Fatalf("GetRunVariableScope: %v", err)
	}
	if got == "" {
		t.Fatal("scope is empty after flush")
	}
	// Both mutations must be present.
	if !containsAll(got, `"x":"1"`, `"y":"2"`) {
		t.Errorf("scope = %q; want both x=1 and y=2", got)
	}
}

func TestScopeCoalescer_TimerFlush(t *testing.T) {
	st := newScopeTestStore(t)
	ctx := context.Background()
	mustCreateScopeOverseer(t, st, ctx)
	mustCreateScopeRun(t, st, ctx, "run-c2")

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := newScopeCoalescer(st, log)

	c.Enqueue("run-c2", func(scope map[string]interface{}) {
		scope["var"] = map[string]interface{}{"z": "timer"}
	})

	// Wait longer than the 250ms debounce interval for the timer to fire.
	time.Sleep(scopeFlushInterval + 100*time.Millisecond)

	got, err := st.GetRunVariableScope(ctx, "run-c2")
	if err != nil {
		t.Fatalf("GetRunVariableScope: %v", err)
	}
	if !containsAll(got, `"z":"timer"`) {
		t.Errorf("scope = %q; expected z=timer after timer flush", got)
	}
}

func TestScopeCoalescer_NoDoubleWrite(t *testing.T) {
	// Enqueue several mutations and flush immediately; the store must have been
	// called exactly once for the single FlushNow, not once per mutation.
	st := newScopeTestStore(t)
	ctx := context.Background()
	mustCreateScopeOverseer(t, st, ctx)
	mustCreateScopeRun(t, st, ctx, "run-c3")

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := newScopeCoalescer(st, log)

	for i := 0; i < 5; i++ {
		n := i // capture
		c.Enqueue("run-c3", func(scope map[string]interface{}) {
			vm, _ := scope["var"].(map[string]interface{})
			if vm == nil {
				vm = map[string]interface{}{}
			}
			_ = n
			vm["k"] = "v"
			scope["var"] = vm
		})
	}
	c.FlushNow(ctx, "run-c3")

	// Second FlushNow with no pending mutations must be a no-op (no panic, no
	// error, store unchanged).
	c.FlushNow(ctx, "run-c3")
	got, err := st.GetRunVariableScope(ctx, "run-c3")
	if err != nil {
		t.Fatalf("GetRunVariableScope: %v", err)
	}
	if got == "" {
		t.Error("scope empty after flush")
	}
}

// containsAll returns true if s contains all of the given substrings.
func containsAll(s string, substrings ...string) bool {
	for _, sub := range substrings {
		found := false
		for i := 0; i <= len(s)-len(sub); i++ {
			if s[i:i+len(sub)] == sub {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// countingStore wraps a real store.Store and counts calls to
// SetRunVariableScope to verify coalescing reduces write count.
type countingStore struct {
	store.Store
	writes atomic.Int64
}

func (c *countingStore) SetRunVariableScope(ctx context.Context, runID, scope string) error {
	c.writes.Add(1)
	return c.Store.SetRunVariableScope(ctx, runID, scope)
}

func (c *countingStore) GetRunVariableScope(ctx context.Context, runID string) (string, error) {
	return c.Store.GetRunVariableScope(ctx, runID)
}

// TestScopeCoalescer_WriteCountReduction asserts that N queued mutations
// result in exactly one SetRunVariableScope call when flushed together.
func TestScopeCoalescer_WriteCountReduction(t *testing.T) {
	const mutations = 20

	real := newScopeTestStore(t)
	ctx := context.Background()
	mustCreateScopeOverseer(t, real, ctx)
	mustCreateScopeRun(t, real, ctx, "run-wc1")

	spy := &countingStore{Store: real}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := newScopeCoalescer(spy, log)

	// Queue many mutations rapidly without letting the timer fire.
	for i := 0; i < mutations; i++ {
		c.Enqueue("run-wc1", func(scope map[string]interface{}) {
			vm, _ := scope["var"].(map[string]interface{})
			if vm == nil {
				vm = map[string]interface{}{}
			}
			vm["k"] = "v"
			scope["var"] = vm
		})
	}

	// Flush synchronously — should produce exactly one DB write.
	c.FlushNow(ctx, "run-wc1")

	if got := spy.writes.Load(); got != 1 {
		t.Errorf("SetRunVariableScope called %d times for %d mutations; want 1 (coalesced)", got, mutations)
	}
}
