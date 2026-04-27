package rpc

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/brokenbots/overlord/castle/internal/store"
	pb "github.com/brokenbots/overlord/shared/pb/overlord/v1" // import-lint:allow castle service bindings (W08: move to castle-proto)
	overseer "github.com/brokenbots/overlord/shared/sdk/overseer"
)

// applyAndFlushScope calls applyRunStatus with the given envelope and then
// synchronously flushes the scope coalescer so the value is persisted to the
// store before we read it back.
func applyAndFlushScope(t *testing.T, ts *testStack, runID string, env *pb.Envelope) {
	t.Helper()
	ts.overseer.applyRunStatus(context.Background(), env)
	ts.overseer.scope.FlushNow(context.Background(), runID)
}

// getScopeIter reads back the stored variable_scope and returns the "iter"
// sub-object, or nil if absent.
func getScopeIter(t *testing.T, ts *testStack, runID string) map[string]interface{} {
	t.Helper()
	run, err := ts.store.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.VariableScope == "" {
		return nil
	}
	var scope map[string]interface{}
	if err := json.Unmarshal([]byte(run.VariableScope), &scope); err != nil {
		t.Fatalf("unmarshal scope: %v", err)
	}
	if iter, ok := scope["iter"]; ok {
		if m, ok2 := iter.(map[string]interface{}); ok2 {
			return m
		}
	}
	return nil
}

func mustRegisterAndCreateRun(t *testing.T, ts *testStack, overseerName string) string {
	t.Helper()
	ctx := context.Background()
	reg, err := ts.overseer.Register(ctx, connect.NewRequest(&pb.RegisterRequest{Name: overseerName}))
	if err != nil {
		t.Fatal(err)
	}
	r := &store.Run{
		ID:           "run-fe-" + reg.Msg.OverseerId[:8],
		OverseerID:   reg.Msg.OverseerId,
		WorkflowName: "test-wf",
		Status:       "running",
		CreatedAt:    time.Now().UTC(),
	}
	if err := ts.store.CreateRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	return r.ID
}

func makeEnvForTest(runID string, payload *pb.Envelope) *pb.Envelope {
	payload.SchemaVersion = int32(overseer.SchemaVersion)
	payload.RunId = runID
	return payload
}

// TestApplyRunStatus_ScopeIterCursorSet_StoresVerbatim verifies that Castle
// stores cursor_json verbatim into scope["iter"] without interpreting field
// names, honouring the 1.6 split-readiness constraint (W07).
func TestApplyRunStatus_ScopeIterCursorSet_StoresVerbatim(t *testing.T) {
	ts := newTestStack(t)
	runID := mustRegisterAndCreateRun(t, ts, "o-fe-set")

	cursorJSON := `{"node":"each_item","index":1,"any_failed":false,"in_progress":true}`
	applyAndFlushScope(t, ts, runID, makeEnvForTest(runID, &pb.Envelope{
		Payload: &pb.Envelope_ScopeIterCursorSet{ScopeIterCursorSet: &pb.ScopeIterCursorSet{CursorJson: cursorJSON}},
	}))

	iter := getScopeIter(t, ts, runID)
	if iter == nil {
		t.Fatal("expected scope[\"iter\"] to be set, got nil")
	}
	if got, _ := iter["node"].(string); got != "each_item" {
		t.Errorf("iter.node = %q; want \"each_item\"", got)
	}
	if got, _ := iter["index"].(float64); got != 1 {
		t.Errorf("iter.index = %v; want 1", got)
	}
	if got, _ := iter["in_progress"].(bool); !got {
		t.Errorf("iter.in_progress = %v; want true", got)
	}
	if got, _ := iter["any_failed"].(bool); got {
		t.Errorf("iter.any_failed = %v; want false", got)
	}
}

// TestApplyRunStatus_ScopeIterCursorSet_EmptyClears verifies that an empty
// cursor_json removes the "iter" field from the stored scope, signalling that
// the for_each loop has completed or aborted (W07).
func TestApplyRunStatus_ScopeIterCursorSet_EmptyClears(t *testing.T) {
	ts := newTestStack(t)
	runID := mustRegisterAndCreateRun(t, ts, "o-fe-clr")

	// First set a cursor.
	applyAndFlushScope(t, ts, runID, makeEnvForTest(runID, &pb.Envelope{
		Payload: &pb.Envelope_ScopeIterCursorSet{ScopeIterCursorSet: &pb.ScopeIterCursorSet{CursorJson: `{"node":"n","index":0,"any_failed":false,"in_progress":false}`}},
	}))
	if getScopeIter(t, ts, runID) == nil {
		t.Fatal("pre-condition: iter should be set after ScopeIterCursorSet")
	}

	// Clear it with an empty cursor_json.
	applyAndFlushScope(t, ts, runID, makeEnvForTest(runID, &pb.Envelope{
		Payload: &pb.Envelope_ScopeIterCursorSet{ScopeIterCursorSet: &pb.ScopeIterCursorSet{CursorJson: ""}},
	}))
	if iter := getScopeIter(t, ts, runID); iter != nil {
		t.Errorf("expected scope[\"iter\"] to be cleared, got %v", iter)
	}
}

// TestApplyRunStatus_ForEachEvents_AreNoOps verifies that the
// ForEachEntered / ForEachIteration / ForEachOutcome events are informational
// only and do NOT write any state into scope["iter"] (W07).
func TestApplyRunStatus_ForEachEvents_AreNoOps(t *testing.T) {
	ts := newTestStack(t)
	runID := mustRegisterAndCreateRun(t, ts, "o-fe-noop")

	for _, tc := range []struct {
		name string
		env  *pb.Envelope
	}{
		{
			"ForEachEntered",
			makeEnvForTest(runID, &pb.Envelope{Payload: &pb.Envelope_ForEachEntered{ForEachEntered: &pb.ForEachEntered{Node: "n", Count: 3}}}),
		},
		{
			"ForEachIteration",
			makeEnvForTest(runID, &pb.Envelope{Payload: &pb.Envelope_ForEachIteration{ForEachIteration: &pb.ForEachIteration{Node: "n", Index: 0, Value: "a"}}}),
		},
		{
			"ForEachOutcome",
			makeEnvForTest(runID, &pb.Envelope{Payload: &pb.Envelope_ForEachOutcome{ForEachOutcome: &pb.ForEachOutcome{Node: "n", Outcome: "all_succeeded", Target: "done"}}}),
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			applyAndFlushScope(t, ts, runID, tc.env)
			if iter := getScopeIter(t, ts, runID); iter != nil {
				t.Errorf("%s: expected scope[\"iter\"] to remain nil (no-op), got %v", tc.name, iter)
			}
		})
	}
}
