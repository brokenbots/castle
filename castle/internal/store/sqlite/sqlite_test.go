package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/brokenbots/overlord/castle/internal/store"
	"github.com/brokenbots/overlord/shared/events"
	pb "github.com/brokenbots/overlord/shared/pb/overlord/v1"
)

func tempStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestOverseerCRUD(t *testing.T) {
	s := tempStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	o := &store.Overseer{ID: "o1", Name: "alice", TokenHash: "x", Status: "online", CreatedAt: now, LastSeenAt: now}
	if err := s.CreateOverseer(ctx, o); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := s.GetOverseer(ctx, "o1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "alice" {
		t.Errorf("name: %s", got.Name)
	}
	list, _ := s.ListOverseers(ctx)
	if len(list) != 1 {
		t.Errorf("list len: %d", len(list))
	}
}

func TestEventAppendAssignsMonotonicSeq(t *testing.T) {
	s := tempStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := s.CreateOverseer(ctx, &store.Overseer{ID: "o1", Name: "x", TokenHash: "t", Status: "online", CreatedAt: now, LastSeenAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateRun(ctx, &store.Run{ID: "r1", OverseerID: "o1", WorkflowName: "w", WorkflowHCL: "x", Status: "pending", CurrentStep: "a", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		env := events.NewEnvelope("r1", &pb.StepEntered{Step: "a", Adapter: "shell", Attempt: 1})
		seq, inserted, err := s.AppendEvent(ctx, env)
		if err != nil {
			t.Fatal(err)
		}
		if !inserted {
			t.Errorf("append %d: expected inserted=true", i)
		}
		if seq != uint64(i+1) {
			t.Errorf("expected seq %d got %d", i+1, seq)
		}
	}
	list, err := s.ListEvents(ctx, "r1", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Errorf("list len: %d", len(list))
	}
	since, _ := s.ListEvents(ctx, "r1", 1, 100)
	if len(since) != 2 {
		t.Errorf("since=1 len: %d", len(since))
	}
}

func TestEventAppendIdempotentOnCorrelationID(t *testing.T) {
	s := tempStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := s.CreateOverseer(ctx, &store.Overseer{ID: "o1", Name: "x", TokenHash: "t", Status: "online", CreatedAt: now, LastSeenAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateRun(ctx, &store.Run{ID: "r1", OverseerID: "o1", WorkflowName: "w", WorkflowHCL: "x", Status: "pending", CurrentStep: "a", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	env := events.NewEnvelope("r1", &pb.StepEntered{Step: "a", Adapter: "shell", Attempt: 1})
	env.CorrelationId = "corr-xyz"

	seq1, inserted1, err := s.AppendEvent(ctx, env)
	if err != nil {
		t.Fatal(err)
	}
	if !inserted1 || seq1 != 1 {
		t.Fatalf("first append: inserted=%v seq=%d", inserted1, seq1)
	}

	// Second append with the same (run_id, correlation_id) must not insert
	// a new row; it returns the existing seq and inserted=false.
	seq2, inserted2, err := s.AppendEvent(ctx, env)
	if err != nil {
		t.Fatal(err)
	}
	if inserted2 {
		t.Fatalf("second append should be dedup; got inserted=true seq=%d", seq2)
	}
	if seq2 != seq1 {
		t.Fatalf("dedup should return existing seq %d; got %d", seq1, seq2)
	}

	list, _ := s.ListEvents(ctx, "r1", 0, 100)
	if len(list) != 1 {
		t.Fatalf("expected exactly 1 persisted row, got %d", len(list))
	}

	// Different correlation id on the same run inserts a new row.
	env2 := events.NewEnvelope("r1", &pb.StepEntered{Step: "a", Adapter: "shell", Attempt: 1})
	env2.CorrelationId = "corr-abc"
	seq3, inserted3, err := s.AppendEvent(ctx, env2)
	if err != nil {
		t.Fatal(err)
	}
	if !inserted3 || seq3 != 2 {
		t.Fatalf("distinct corr id: inserted=%v seq=%d", inserted3, seq3)
	}
}

// TestEventPayloadRoundTripAllVariants locks in the protojson persistence
// format across every payload variant that SubmitEvents accepts. Each
// envelope is appended, read back, and compared with proto.Equal so that
// a future codec change (e.g. enum naming, struct field emission) is
// caught by CI instead of by the first operator who reloads a dev DB.
func TestEventPayloadRoundTripAllVariants(t *testing.T) {
	s := tempStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := s.CreateOverseer(ctx, &store.Overseer{ID: "o1", Name: "x", TokenHash: "t", Status: "online", CreatedAt: now, LastSeenAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateRun(ctx, &store.Run{ID: "r1", OverseerID: "o1", WorkflowName: "w", WorkflowHCL: "x", Status: "pending", CurrentStep: "a", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}

	adapterData, err := structpb.NewStruct(map[string]any{
		"nested": map[string]any{"count": 3.0, "label": "ok"},
		"list":   []any{"a", "b"},
	})
	if err != nil {
		t.Fatalf("structpb: %v", err)
	}

	cases := []struct {
		name    string
		payload any
	}{
		{"run.started", &pb.RunStarted{WorkflowName: "wf", InitialStep: "build"}},
		{"run.completed", &pb.RunCompleted{FinalState: "done", Success: true}},
		{"run.failed", &pb.RunFailed{Reason: "boom", Step: "build"}},
		{"step.entered", &pb.StepEntered{Step: "test", Adapter: "shell", Attempt: 2}},
		{"step.outcome", &pb.StepOutcome{Step: "test", Outcome: "success", DurationMs: 1234}},
		{"step.transition", &pb.StepTransition{From: "build", To: "test", ViaOutcome: "success"}},
		// Non-default enum value guards against a regression where
		// protojson emits enums by name while legacy code compared
		// against lowercase strings.
		{"step.log.stderr", &pb.StepLog{Step: "test", Stream: pb.LogStream_LOG_STREAM_STDERR, Chunk: "warn: x\n"}},
		{"step.log.agent", &pb.StepLog{Step: "test", Stream: pb.LogStream_LOG_STREAM_AGENT, Chunk: "hi"}},
		// structpb round-trip: nested object, list, and numeric values
		// all need to survive the protojson encode/decode pair.
		{"adapter.event", &pb.AdapterEvent{Step: "test", Kind: "tool_call", Data: adapterData}},
		{"overseer.heartbeat", &pb.OverseerHeartbeat{OverseerId: "o1"}},
		{"overseer.disconnected", &pb.OverseerDisconnected{OverseerId: "o1", Reason: "idle"}},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := events.NewEnvelope("r1", tc.payload)
			if env.Payload == nil {
				t.Fatalf("payload %T was not wrapped by NewEnvelope", tc.payload)
			}
			// Distinct correlation id per case to avoid dedup.
			env.CorrelationId = tc.name
			seq, inserted, err := s.AppendEvent(ctx, env)
			if err != nil {
				t.Fatalf("append: %v", err)
			}
			if !inserted {
				t.Fatalf("expected inserted=true")
			}
			got, err := s.ListEvents(ctx, "r1", seq-1, 1)
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("list len=%d want 1 (case %d)", len(got), i)
			}
			back := got[0]
			// proto.Equal compares the full envelope including the
			// oneof payload, which is the real contract we care
			// about for clients.
			if !proto.Equal(env, back) {
				t.Fatalf("round trip mismatch:\nwant: %+v\nback: %+v", env, back)
			}
			if events.TypeString(back) != events.TypeString(env) {
				t.Fatalf("type string drift: want %q got %q", events.TypeString(env), events.TypeString(back))
			}
		})
	}
}
