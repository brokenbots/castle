package sqlite

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	"github.com/brokenbots/overlord/castle/internal/store"
	pb "github.com/brokenbots/overlord/shared/sdk/overseer"
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
		env := pb.NewEnvelope("r1", &pb.StepEntered{Step: "a", Adapter: "shell", Attempt: 1})
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
	env := pb.NewEnvelope("r1", &pb.StepEntered{Step: "a", Adapter: "shell", Attempt: 1})
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
	env2 := pb.NewEnvelope("r1", &pb.StepEntered{Step: "a", Adapter: "shell", Attempt: 1})
	env2.CorrelationId = "corr-abc"
	seq3, inserted3, err := s.AppendEvent(ctx, env2)
	if err != nil {
		t.Fatal(err)
	}
	if !inserted3 || seq3 != 2 {
		t.Fatalf("distinct corr id: inserted=%v seq=%d", inserted3, seq3)
	}
}

func TestUpsertSubscriberCursor_RoundTrip(t *testing.T) {
	s := tempStore(t)
	ctx := context.Background()

	if err := s.UpsertSubscriberCursor(ctx, "sub-1", "run-1", 42); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	seq, found, err := s.GetSubscriberCursor(ctx, "sub-1", "run-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !found {
		t.Fatal("expected cursor row to exist")
	}
	if seq != 42 {
		t.Fatalf("seq=%d want 42", seq)
	}
}

func TestUpsertSubscriberCursor_AdvancesOnly(t *testing.T) {
	s := tempStore(t)
	ctx := context.Background()

	if err := s.UpsertSubscriberCursor(ctx, "sub-1", "run-1", 50); err != nil {
		t.Fatalf("upsert 50: %v", err)
	}
	if err := s.UpsertSubscriberCursor(ctx, "sub-1", "run-1", 40); err != nil {
		t.Fatalf("upsert 40: %v", err)
	}

	seq, found, err := s.GetSubscriberCursor(ctx, "sub-1", "run-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !found {
		t.Fatal("expected cursor row to exist")
	}
	if seq != 50 {
		t.Fatalf("seq=%d want 50", seq)
	}

	if err := s.UpsertSubscriberCursor(ctx, "sub-1", "run-1", 80); err != nil {
		t.Fatalf("upsert 80: %v", err)
	}
	seq, found, err = s.GetSubscriberCursor(ctx, "sub-1", "run-1")
	if err != nil {
		t.Fatalf("get after advance: %v", err)
	}
	if !found || seq != 80 {
		t.Fatalf("seq=%d found=%v want seq=80 found=true", seq, found)
	}
}

// TestExhaustive_PayloadRoundTrip enumerates every Envelope.payload oneof arm
// via protoreflect and verifies that each type round-trips cleanly through
// AppendEvent → ListEvents (the full SQLite persistence path). This is the
// sides-3-and-4 drift gate for Castle: adding a new oneof arm to events.proto
// without updating payloadMessage or unmarshalPayload fails this test.
//
// Each arm is exercised with non-zero field values so a codec regression on
// any field is observable.
func TestExhaustive_PayloadRoundTrip(t *testing.T) {
	s := tempStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := s.CreateOverseer(ctx, &store.Overseer{ID: "o1", Name: "x", TokenHash: "t", Status: "online", CreatedAt: now, LastSeenAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateRun(ctx, &store.Run{ID: "r1", OverseerID: "o1", WorkflowName: "w", WorkflowHCL: "x", Status: "pending", CurrentStep: "a", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}

	oneofs := (&pb.Envelope{}).ProtoReflect().Descriptor().Oneofs()
	var payloadOO protoreflect.OneofDescriptor
	for i := 0; i < oneofs.Len(); i++ {
		if oneofs.Get(i).Name() == "payload" {
			payloadOO = oneofs.Get(i)
			break
		}
	}
	if payloadOO == nil {
		t.Fatal("payload oneof not found in Envelope descriptor")
	}

	fields := payloadOO.Fields()
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		armName := string(fd.Name())

		t.Run(armName, func(t *testing.T) {
			mt, err := protoregistry.GlobalTypes.FindMessageByName(fd.Message().FullName())
			if err != nil {
				t.Fatalf("message type %q not registered: %v", fd.Message().FullName(), err)
			}
			msg := mt.New().Interface()
			sqlitePopulateMessage(msg.ProtoReflect(), 0)

			env := pb.NewEnvelope("r1", msg)
			if env.Payload == nil {
				t.Fatalf("NewEnvelope produced nil payload for arm %q", armName)
			}
			// Use armName as correlation id to avoid dedup across subtests.
			env.CorrelationId = armName

			seq, inserted, err := s.AppendEvent(ctx, env)
			if err != nil {
				t.Fatalf("AppendEvent: %v", err)
			}
			if !inserted {
				t.Fatalf("expected inserted=true")
			}

			got, err := s.ListEvents(ctx, "r1", seq-1, 1)
			if err != nil {
				t.Fatalf("ListEvents: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("ListEvents returned %d events, want 1", len(got))
			}
			back := got[0]

			// Side 3+4: proto.Equal across the full SQLite persistence path.
			if !proto.Equal(env, back) {
				t.Fatalf("round-trip mismatch for arm %q:\nwant: %v\ngot:  %v", armName, env, back)
			}
			if pb.TypeString(back) != pb.TypeString(env) {
				t.Fatalf("TypeString drift for arm %q: want %q got %q",
					armName, pb.TypeString(env), pb.TypeString(back))
			}
		})
	}
}

// sqlitePopulateMessage sets every field in m to a deterministic non-zero
// value. Duplicated from shared/events/exhaustive_test.go because shared/ and
// castle/ are separate Go modules and cannot share test helpers directly.
// depth guards against infinite recursion in self-referential message types.
func sqlitePopulateMessage(m protoreflect.Message, depth int) {
	if depth > 3 || sqliteIsWellKnown(m.Descriptor().FullName()) {
		return
	}
	fds := m.Descriptor().Fields()
	for i := 0; i < fds.Len(); i++ {
		fd := fds.Get(i)
		switch {
		case fd.IsMap():
			mp := m.Mutable(fd).Map()
			k := sqliteMapKey(fd.MapKey().Kind())
			v := sqliteDeterministicValue(fd.MapValue(), m, depth)
			mp.Set(k, v)
		case fd.IsList():
			ls := m.Mutable(fd).List()
			ls.Append(sqliteDeterministicValue(fd, m, depth))
		case fd.Kind() == protoreflect.MessageKind || fd.Kind() == protoreflect.GroupKind:
			sub := m.Mutable(fd).Message()
			sqlitePopulateMessage(sub, depth+1)
		default:
			m.Set(fd, sqliteDeterministicScalar(fd))
		}
	}
}

func sqliteDeterministicValue(fd protoreflect.FieldDescriptor, parent protoreflect.Message, depth int) protoreflect.Value {
	if fd.Kind() == protoreflect.MessageKind || fd.Kind() == protoreflect.GroupKind {
		if sqliteIsWellKnown(fd.Message().FullName()) {
			return parent.NewField(fd)
		}
		sub := parent.NewField(fd).Message()
		sqlitePopulateMessage(sub, depth+1)
		return protoreflect.ValueOfMessage(sub)
	}
	return sqliteDeterministicScalar(fd)
}

func sqliteDeterministicScalar(fd protoreflect.FieldDescriptor) protoreflect.Value {
	switch fd.Kind() {
	case protoreflect.BoolKind:
		return protoreflect.ValueOfBool(true)
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		return protoreflect.ValueOfInt32(1)
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		return protoreflect.ValueOfInt64(1)
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		return protoreflect.ValueOfUint32(1)
	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		return protoreflect.ValueOfUint64(1)
	case protoreflect.FloatKind:
		return protoreflect.ValueOfFloat32(1.0)
	case protoreflect.DoubleKind:
		return protoreflect.ValueOfFloat64(1.0)
	case protoreflect.StringKind:
		return protoreflect.ValueOfString("x")
	case protoreflect.BytesKind:
		return protoreflect.ValueOfBytes([]byte("x"))
	case protoreflect.EnumKind:
		evs := fd.Enum().Values()
		for j := 0; j < evs.Len(); j++ {
			if evs.Get(j).Number() != 0 {
				return protoreflect.ValueOfEnum(evs.Get(j).Number())
			}
		}
		return protoreflect.ValueOfEnum(evs.Get(0).Number())
	default:
		return protoreflect.Value{}
	}
}

func sqliteMapKey(k protoreflect.Kind) protoreflect.MapKey {
	switch k {
	case protoreflect.StringKind:
		return protoreflect.ValueOfString("k").MapKey()
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		return protoreflect.ValueOfInt32(1).MapKey()
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		return protoreflect.ValueOfInt64(1).MapKey()
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		return protoreflect.ValueOfUint32(1).MapKey()
	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		return protoreflect.ValueOfUint64(1).MapKey()
	case protoreflect.BoolKind:
		return protoreflect.ValueOfBool(true).MapKey()
	default:
		return protoreflect.ValueOfString("k").MapKey()
	}
}

func sqliteIsWellKnown(name protoreflect.FullName) bool {
	return strings.HasPrefix(string(name), "google.protobuf.")
}

func TestListEvents_HonorsLimit(t *testing.T) {
	s := tempStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := s.CreateOverseer(ctx, &store.Overseer{ID: "o1", Name: "x", TokenHash: "t", Status: "online", CreatedAt: now, LastSeenAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateRun(ctx, &store.Run{ID: "r-limit", OverseerID: "o1", WorkflowName: "w", WorkflowHCL: "x", Status: "pending", CurrentStep: "a", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 30; i++ {
		env := pb.NewEnvelope("r-limit", &pb.StepEntered{Step: "a", Adapter: "shell", Attempt: 1})
		env.CorrelationId = fmt.Sprintf("limit-%d", i)
		if _, _, err := s.AppendEvent(ctx, env); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.ListEvents(ctx, "r-limit", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 10 {
		t.Fatalf("events=%d want 10", len(got))
	}
	if got[9].Seq != 10 {
		t.Fatalf("last seq=%d want 10", got[9].Seq)
	}
}

func TestListEvents_RejectsOversizedLimit(t *testing.T) {
	s := tempStore(t)
	_, err := s.ListEvents(context.Background(), "r-missing", 0, ListEventsMaxLimit+1)
	if err == nil {
		t.Fatal("expected oversize limit error")
	}
	if !errors.Is(err, store.ErrInvalidLimit) {
		t.Fatalf("expected ErrInvalidLimit, got %v", err)
	}
}

func TestListEvents_DefaultOnZero(t *testing.T) {
	s := tempStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := s.CreateOverseer(ctx, &store.Overseer{ID: "o1", Name: "x", TokenHash: "t", Status: "online", CreatedAt: now, LastSeenAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateRun(ctx, &store.Run{ID: "r-default", OverseerID: "o1", WorkflowName: "w", WorkflowHCL: "x", Status: "pending", CurrentStep: "a", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 700; i++ {
		env := pb.NewEnvelope("r-default", &pb.StepEntered{Step: "a", Adapter: "shell", Attempt: 1})
		env.CorrelationId = fmt.Sprintf("default-%d", i)
		if _, _, err := s.AppendEvent(ctx, env); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.ListEvents(ctx, "r-default", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != ListEventsDefaultLimit {
		t.Fatalf("events=%d want default %d", len(got), ListEventsDefaultLimit)
	}
}

func TestListEvents_Pagination_OrderPreserved(t *testing.T) {
	s := tempStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := s.CreateOverseer(ctx, &store.Overseer{ID: "o1", Name: "x", TokenHash: "t", Status: "online", CreatedAt: now, LastSeenAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateRun(ctx, &store.Run{ID: "r-page", OverseerID: "o1", WorkflowName: "w", WorkflowHCL: "x", Status: "pending", CurrentStep: "a", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 1500; i++ {
		env := pb.NewEnvelope("r-page", &pb.StepEntered{Step: "a", Adapter: "shell", Attempt: 1})
		env.CorrelationId = fmt.Sprintf("page-%d", i)
		if _, _, err := s.AppendEvent(ctx, env); err != nil {
			t.Fatal(err)
		}
	}

	var (
		since   uint64
		seen    int
		lastSeq uint64
	)
	for {
		page, err := s.ListEvents(ctx, "r-page", since, ListEventsMaxLimit)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		for _, env := range page {
			if env.Seq <= lastSeq {
				t.Fatalf("sequence regressed: last=%d current=%d", lastSeq, env.Seq)
			}
			lastSeq = env.Seq
			seen++
		}
		since = page[len(page)-1].Seq
		if len(page) < ListEventsMaxLimit {
			break
		}
	}
	if seen != 1500 {
		t.Fatalf("events seen=%d want 1500", seen)
	}
}

func TestListStepLogs_Pagination(t *testing.T) {
	s := tempStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := s.CreateOverseer(ctx, &store.Overseer{ID: "o1", Name: "x", TokenHash: "t", Status: "online", CreatedAt: now, LastSeenAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateRun(ctx, &store.Run{ID: "r-logs", OverseerID: "o1", WorkflowName: "w", WorkflowHCL: "x", Status: "pending", CurrentStep: "a", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 900; i++ {
		env := pb.NewEnvelope("r-logs", &pb.StepLog{Step: "build", Stream: pb.LogStream_LOG_STREAM_STDOUT, Chunk: fmt.Sprintf("line-%d", i)})
		env.CorrelationId = fmt.Sprintf("build-%d", i)
		if _, _, err := s.AppendEvent(ctx, env); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 50; i++ {
		env := pb.NewEnvelope("r-logs", &pb.StepLog{Step: "test", Stream: pb.LogStream_LOG_STREAM_STDOUT, Chunk: fmt.Sprintf("other-%d", i)})
		env.CorrelationId = fmt.Sprintf("test-%d", i)
		if _, _, err := s.AppendEvent(ctx, env); err != nil {
			t.Fatal(err)
		}
	}

	var (
		since uint64
		seen  int
	)
	for {
		page, err := s.ListStepLogs(ctx, "r-logs", "build", since, 300)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		for _, env := range page {
			logPayload, ok := env.Payload.(*pb.Envelope_StepLog)
			if !ok {
				t.Fatalf("expected step.log payload, got %T", env.Payload)
			}
			if logPayload.StepLog.Step != "build" {
				t.Fatalf("unexpected step %q", logPayload.StepLog.Step)
			}
		}
		seen += len(page)
		since = page[len(page)-1].Seq
		if len(page) < 300 {
			break
		}
	}
	if seen != 900 {
		t.Fatalf("step.log count=%d want 900", seen)
	}
}
