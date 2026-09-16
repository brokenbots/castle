package sqlite

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/brokenbots/castle/castle/internal/store"
	criteria "github.com/brokenbots/criteria/sdk"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
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

// TestOpenRejectsMemoryDSN pins the Open contract for in-memory DSNs (CRI-143
// review): the dedicated reader handle only works for file-backed databases —
// each pooled connection on a :memory: DSN gets its own private, empty
// database, so reads would fail with "no such table" — so Open must reject
// the path up front with the documented error instead of returning a broken
// store.
func TestOpenRejectsMemoryDSN(t *testing.T) {
	s, err := Open(":memory:")
	if err == nil {
		_ = s.Close()
		t.Fatal("Open(:memory:) succeeded, want the documented rejection error")
	}
	if !strings.Contains(err.Error(), ":memory: stores are not supported") {
		t.Fatalf("err = %v, want the documented :memory: rejection", err)
	}
}

// TestReaderHandleSeesWrites is the reader-split regression for the path Open
// accepts (CRI-143 review): reader-backed methods — ListOverseers and friends
// query the dedicated reader pool — must observe rows written through the
// serialized writer pool on a file-backed store.
func TestReaderHandleSeesWrites(t *testing.T) {
	s := tempStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	o := &store.Overseer{ID: "ov-reader", Name: "alice", TokenHash: "x", Status: "online", CreatedAt: now, LastSeenAt: now}
	if err := s.CreateOverseer(ctx, o); err != nil {
		t.Fatalf("CreateOverseer: %v", err)
	}
	list, err := s.ListOverseers(ctx)
	if err != nil {
		t.Fatalf("ListOverseers (reader-backed): %v", err)
	}
	if len(list) != 1 || list[0].ID != "ov-reader" {
		t.Fatalf("ListOverseers = %+v, want the seeded overseer", list)
	}
}

// eventFromProto converts a wire envelope into the storage-neutral
// store.Event representation used by the persistence layer. It is intentionally
// local to the SQLite test package so storage tests can seed events without
// depending on the RPC codec (which imports sqlite).
func eventFromProto(env *criteria.Envelope) (*store.Event, error) {
	payloadOO := env.ProtoReflect().Descriptor().Oneofs().ByName("payload")
	fd := env.ProtoReflect().WhichOneof(payloadOO)
	if fd == nil {
		return nil, fmt.Errorf("envelope has no payload")
	}
	payload := env.ProtoReflect().Get(fd).Message().Interface()
	payloadJSON, err := protojson.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return &store.Event{
		SchemaVersion: int32(criteria.SchemaVersion),
		RunID:         env.RunId,
		Seq:           env.Seq,
		Type:          criteria.TypeString(env),
		Ts:            env.Ts.AsTime(),
		CorrelationID: env.CorrelationId,
		Payload:       payloadJSON,
	}, nil
}

// mustEventFromProto is the test-failing variant of eventFromProto.
func mustEventFromProto(t *testing.T, env *criteria.Envelope) *store.Event {
	t.Helper()
	ev, err := eventFromProto(env)
	if err != nil {
		t.Fatalf("convert envelope to store event: %v", err)
	}
	return ev
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
	list, err := s.ListOverseers(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Errorf("list len: %d", len(list))
	}
}

// TestListRunsPagesByKeysetCursor covers server-side paging (CRI-187): with
// more runs than the page limit, the continuation token resumes exactly where
// the previous page stopped, pages neither overlap nor skip rows, an
// exactly-full final page yields no dead token, and a remainder that fits in
// one page returns no token. Tied created_at values are disambiguated by the
// id tiebreaker, and a malformed token fails with ErrInvalidCursor.
func TestListRunsPagesByKeysetCursor(t *testing.T) {
	s := tempStore(t)
	ctx := context.Background()

	now := time.Now().UTC()
	if err := s.CreateOverseer(ctx, &store.Overseer{ID: "ov-1", Name: "paging", TokenHash: "x", Status: "online", CreatedAt: now, LastSeenAt: now}); err != nil {
		t.Fatalf("create overseer: %v", err)
	}

	// Alternating timestamps exercise both ORDER BY branches: pairs share a
	// created_at so the id tiebreaker decides their relative order.
	base := time.Date(2026, 2, 5, 8, 30, 0, 0, time.UTC)
	wantIDs := make([]string, 0, 7)
	for i := 0; i < 7; i++ {
		id := fmt.Sprintf("run-%02d", i)
		wantIDs = append(wantIDs, id)
		status := "succeeded"
		if i%2 == 1 {
			status = "running"
		}
		r := &store.Run{
			ID:           id,
			OverseerID:   "ov-1",
			WorkflowName: "wf",
			Status:       status,
			CreatedAt:    base.Add(time.Duration(i%2) * time.Minute),
		}
		if err := s.CreateRun(ctx, r); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}

	t.Run("no limit returns every run with no token", func(t *testing.T) {
		all, next, err := s.ListRuns(ctx, "", "", 0, "")
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if next != "" {
			t.Fatalf("next token %q, want empty without a limit", next)
		}
		if len(all) != len(wantIDs) {
			t.Fatalf("len=%d want %d", len(all), len(wantIDs))
		}
	})

	t.Run("pages resume at the cursor and cover every run once", func(t *testing.T) {
		seen := map[string]bool{}
		var order []string
		pageToken := ""
		for page := 0; ; page++ {
			rows, next, err := s.ListRuns(ctx, "", "", 3, pageToken)
			if err != nil {
				t.Fatalf("page %d: %v", page, err)
			}
			if page > 0 && len(rows) == 0 {
				t.Fatalf("page %d returned no rows for a live token; dead cursor page", page)
			}
			for _, r := range rows {
				if seen[r.ID] {
					t.Fatalf("run %s returned on two pages", r.ID)
				}
				seen[r.ID] = true
				order = append(order, r.ID)
			}
			if next == "" {
				break
			}
			if page > 100 {
				t.Fatal("paging did not terminate")
			}
			pageToken = next
		}
		if len(seen) != len(wantIDs) {
			t.Fatalf("paged traversal saw %d runs, want %d", len(seen), len(wantIDs))
		}
		// Newest first: later timestamps, then higher id within a tie.
		wantOrder := []string{"run-05", "run-03", "run-01", "run-06", "run-04", "run-02", "run-00"}
		if strings.Join(order, ",") != strings.Join(wantOrder, ",") {
			t.Fatalf("order = %v, want %v", order, wantOrder)
		}
	})

	t.Run("status filter pages independently", func(t *testing.T) {
		rows, next, err := s.ListRuns(ctx, "", "running", 2, "")
		if err != nil {
			t.Fatalf("first page: %v", err)
		}
		if len(rows) != 2 || rows[0].ID != "run-05" || rows[1].ID != "run-03" {
			t.Fatalf("first page = %v/%v, want run-05,run-03", ids(rows), "run-05,run-03")
		}
		rows, next2, err := s.ListRuns(ctx, "", "running", 2, next)
		if err != nil {
			t.Fatalf("second page: %v", err)
		}
		if next2 != "" {
			t.Fatalf("second page returned token %q, want empty when the remainder fits", next2)
		}
		if len(rows) != 1 || rows[0].ID != "run-01" {
			t.Fatalf("second page = %v, want run-01", ids(rows))
		}
	})

	t.Run("malformed token is rejected", func(t *testing.T) {
		if _, _, err := s.ListRuns(ctx, "", "", 3, "not-a-cursor"); !errors.Is(err, store.ErrInvalidCursor) {
			t.Fatalf("err = %v, want ErrInvalidCursor", err)
		}
	})
}

func ids(runs []*store.Run) []string {
	out := make([]string, 0, len(runs))
	for _, r := range runs {
		out = append(out, r.ID)
	}
	return out
}

// TestRunMetadataCRUD covers the CRI-131 k8s-native run metadata columns:
// create-time ticket/repo_url persistence, list/get visibility, non-empty-only
// promotion via SetRunMetadata, and the status-update path leaving metadata
// intact.
func TestRunMetadataCRUD(t *testing.T) {
	s := tempStore(t)
	ctx := context.Background()

	now := time.Now().UTC()
	if err := s.CreateOverseer(ctx, &store.Overseer{ID: "ov-1", Name: "k8s-operator", TokenHash: "x", Status: "online", CreatedAt: now, LastSeenAt: now}); err != nil {
		t.Fatalf("create overseer: %v", err)
	}

	r := &store.Run{
		ID: "r-meta", OverseerID: "ov-1", WorkflowName: "wf", Status: "pending",
		CreatedAt: now,
		Ticket:    "CRI-131", RepoURL: "brokenbots/castle",
	}
	if err := s.CreateRun(ctx, r); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := s.GetRun(ctx, "r-meta")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Ticket != "CRI-131" || got.RepoURL != "brokenbots/castle" || got.PRURL != "" {
		t.Errorf("after create: ticket=%q repo=%q pr=%q", got.Ticket, got.RepoURL, got.PRURL)
	}

	runs, _, err := s.ListRuns(ctx, "", "", 0, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(runs) != 1 || runs[0].Ticket != "CRI-131" {
		t.Errorf("list visibility: len=%d ticket=%q", len(runs), runs[0].Ticket)
	}

	// SetRunMetadata promotes only non-empty values; a later metadata event
	// that omits ticket/repo_url must not clear them.
	if err := s.SetRunMetadata(ctx, "r-meta", "", "", "https://github.com/brokenbots/castle/pull/42"); err != nil {
		t.Fatalf("set metadata: %v", err)
	}
	got, err = s.GetRun(ctx, "r-meta")
	if err != nil {
		t.Fatalf("get after metadata: %v", err)
	}
	if got.PRURL != "https://github.com/brokenbots/castle/pull/42" || got.Ticket != "CRI-131" || got.RepoURL != "brokenbots/castle" {
		t.Errorf("after metadata: ticket=%q repo=%q pr=%q", got.Ticket, got.RepoURL, got.PRURL)
	}

	// Run status transitions do not clobber published metadata.
	got.Status = "succeeded"
	if err := s.UpdateRun(ctx, got); err != nil {
		t.Fatalf("update run: %v", err)
	}
	got, err = s.GetRun(ctx, "r-meta")
	if err != nil {
		t.Fatalf("get after status: %v", err)
	}
	if got.Status != "succeeded" || got.Ticket != "CRI-131" || got.PRURL != "https://github.com/brokenbots/castle/pull/42" {
		t.Errorf("after status update: status=%q ticket=%q pr=%q", got.Status, got.Ticket, got.PRURL)
	}

	// Unknown run ids are a no-op, not an error.
	if err := s.SetRunMetadata(ctx, "missing-run", "t", "r", "p"); err != nil {
		t.Errorf("metadata for unknown run: %v", err)
	}

	// Agent-initiated runs keep NULL metadata until an orchestrator publishes.
	if err := s.CreateRun(ctx, &store.Run{ID: "r-agent", OverseerID: "ov-1", WorkflowName: "wf", Status: "pending", CreatedAt: now}); err != nil {
		t.Fatalf("create agent run: %v", err)
	}
	got, err = s.GetRun(ctx, "r-agent")
	if err != nil {
		t.Fatalf("get agent run: %v", err)
	}
	if got.Ticket != "" || got.RepoURL != "" || got.PRURL != "" {
		t.Errorf("agent run metadata should be empty: %q/%q/%q", got.Ticket, got.RepoURL, got.PRURL)
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
		env := criteria.NewEnvelope("r1", &pb.StepEntered{Step: "a", Adapter: "shell", Attempt: 1})
		seq, inserted, err := s.AppendEvent(ctx, mustEventFromProto(t, env))
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
	env := criteria.NewEnvelope("r1", &pb.StepEntered{Step: "a", Adapter: "shell", Attempt: 1})
	env.CorrelationId = "corr-xyz"

	seq1, inserted1, err := s.AppendEvent(ctx, mustEventFromProto(t, env))
	if err != nil {
		t.Fatal(err)
	}
	if !inserted1 || seq1 != 1 {
		t.Fatalf("first append: inserted=%v seq=%d", inserted1, seq1)
	}

	// Second append with the same (run_id, correlation_id) must not insert
	// a new row; it returns the existing seq and inserted=false.
	seq2, inserted2, err := s.AppendEvent(ctx, mustEventFromProto(t, env))
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
	env2 := criteria.NewEnvelope("r1", &pb.StepEntered{Step: "a", Adapter: "shell", Attempt: 1})
	env2.CorrelationId = "corr-abc"
	seq3, inserted3, err := s.AppendEvent(ctx, mustEventFromProto(t, env2))
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

			env := criteria.NewEnvelope("r1", msg)
			if env.Payload == nil {
				t.Fatalf("NewEnvelope produced nil payload for arm %q", armName)
			}
			// Use armName as correlation id to avoid dedup across subtests.
			env.CorrelationId = armName

			seq, inserted, err := s.AppendEvent(ctx, mustEventFromProto(t, env))
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

			wantType := criteria.TypeString(env)
			if back.Type != wantType {
				t.Fatalf("type drift for arm %q: want %q got %q", armName, wantType, back.Type)
			}
			if back.SchemaVersion != int32(criteria.SchemaVersion) {
				t.Fatalf("schema version drift for arm %q: want %d got %d", armName, criteria.SchemaVersion, back.SchemaVersion)
			}
			if back.RunID != env.RunId {
				t.Fatalf("run id drift for arm %q", armName)
			}
			if back.Seq != seq {
				t.Fatalf("seq drift for arm %q: want %d got %d", armName, seq, back.Seq)
			}
			if back.CorrelationID != env.CorrelationId {
				t.Fatalf("correlation id drift for arm %q", armName)
			}
			if !back.Ts.Equal(env.Ts.AsTime()) {
				t.Fatalf("timestamp drift for arm %q", armName)
			}

			// Validate the JSON payload round-trips back into the same message.
			roundtrip := mt.New().Interface()
			if err := protojson.Unmarshal(back.Payload, roundtrip); err != nil {
				t.Fatalf("payload unmarshal for arm %q: %v", armName, err)
			}
			if !proto.Equal(msg, roundtrip) {
				t.Fatalf("payload round-trip mismatch for arm %q:\nwant: %v\ngot:  %v", armName, msg, roundtrip)
			}
		})
	}
}

// sqlitePopulateMessage sets every field in m to a deterministic non-zero
// value. Persistence and RPC layers are separate modules and cannot share
// test helpers directly, so this helper is duplicated where needed.
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
			v := sqliteDeterministicValue(fd.MapValue(), depth)
			mp.Set(k, v)
		case fd.IsList():
			ls := m.Mutable(fd).List()
			if fd.Kind() == protoreflect.MessageKind || fd.Kind() == protoreflect.GroupKind {
				sub := sqliteNewMessage(fd.Message())
				sqlitePopulateMessage(sub, depth+1)
				ls.Append(protoreflect.ValueOfMessage(sub))
			} else {
				ls.Append(sqliteDeterministicScalar(fd))
			}
		case fd.Kind() == protoreflect.MessageKind || fd.Kind() == protoreflect.GroupKind:
			sub := m.Mutable(fd).Message()
			sqlitePopulateMessage(sub, depth+1)
		default:
			m.Set(fd, sqliteDeterministicScalar(fd))
		}
	}
}

func sqliteNewMessage(desc protoreflect.MessageDescriptor) protoreflect.Message {
	mt, err := protoregistry.GlobalTypes.FindMessageByName(desc.FullName())
	if err != nil {
		panic(fmt.Sprintf("find message %q: %v", desc.FullName(), err))
	}
	return mt.New().Interface().ProtoReflect()
}

func sqliteDeterministicValue(fd protoreflect.FieldDescriptor, depth int) protoreflect.Value {
	if fd.Kind() == protoreflect.MessageKind || fd.Kind() == protoreflect.GroupKind {
		if sqliteIsWellKnown(fd.Message().FullName()) {
			return protoreflect.ValueOfMessage(sqliteNewMessage(fd.Message()))
		}
		sub := sqliteNewMessage(fd.Message())
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
		env := criteria.NewEnvelope("r-limit", &pb.StepEntered{Step: "a", Adapter: "shell", Attempt: 1})
		env.CorrelationId = fmt.Sprintf("limit-%d", i)
		if _, _, err := s.AppendEvent(ctx, mustEventFromProto(t, env)); err != nil {
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
		env := criteria.NewEnvelope("r-default", &pb.StepEntered{Step: "a", Adapter: "shell", Attempt: 1})
		env.CorrelationId = fmt.Sprintf("default-%d", i)
		if _, _, err := s.AppendEvent(ctx, mustEventFromProto(t, env)); err != nil {
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
		env := criteria.NewEnvelope("r-page", &pb.StepEntered{Step: "a", Adapter: "shell", Attempt: 1})
		env.CorrelationId = fmt.Sprintf("page-%d", i)
		if _, _, err := s.AppendEvent(ctx, mustEventFromProto(t, env)); err != nil {
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

func TestGetLatestEvent_ReturnsMostRecentBySeq(t *testing.T) {
	s := tempStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := s.CreateOverseer(ctx, &store.Overseer{ID: "o1", Name: "x", TokenHash: "t", Status: "online", CreatedAt: now, LastSeenAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateRun(ctx, &store.Run{ID: "r-latest", OverseerID: "o1", WorkflowName: "w", WorkflowHCL: "x", Status: "pending", CurrentStep: "a", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}

	base := now.Truncate(time.Second)
	for i := 0; i < 5; i++ {
		env := criteria.NewEnvelope("r-latest", &pb.StepLog{Step: "a", Stream: pb.LogStream_LOG_STREAM_STDOUT, Chunk: fmt.Sprintf("line %d", i)})
		env.Ts = timestamppb.New(base.Add(time.Duration(i) * time.Second))
		env.CorrelationId = fmt.Sprintf("log-%d", i)
		if _, _, err := s.AppendEvent(ctx, mustEventFromProto(t, env)); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.GetLatestEvent(ctx, "r-latest")
	if err != nil {
		t.Fatalf("GetLatestEvent: %v", err)
	}
	if got.Seq != 5 {
		t.Fatalf("seq=%d want 5", got.Seq)
	}
	if got.Ts != base.Add(4*time.Second) {
		t.Fatalf("ts=%v want %v", got.Ts, base.Add(4*time.Second))
	}
}

func TestGetLatestEvent_NotFound(t *testing.T) {
	s := tempStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := s.CreateOverseer(ctx, &store.Overseer{ID: "o1", Name: "x", TokenHash: "t", Status: "online", CreatedAt: now, LastSeenAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateRun(ctx, &store.Run{ID: "r-empty", OverseerID: "o1", WorkflowName: "w", WorkflowHCL: "x", Status: "pending", CurrentStep: "a", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetLatestEvent(ctx, "r-empty"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestGetLatestStepEnteredEvent_ReturnsMostRecentAdapter(t *testing.T) {
	s := tempStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := s.CreateOverseer(ctx, &store.Overseer{ID: "o1", Name: "x", TokenHash: "t", Status: "online", CreatedAt: now, LastSeenAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateRun(ctx, &store.Run{ID: "r-adapters", OverseerID: "o1", WorkflowName: "w", WorkflowHCL: "x", Status: "pending", CurrentStep: "a", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}

	base := now.Truncate(time.Second)
	adapters := []string{"docker", "shell", "kubernetes"}
	for i, adapter := range adapters {
		env := criteria.NewEnvelope("r-adapters", &pb.StepEntered{Step: fmt.Sprintf("s%d", i), Adapter: adapter, Attempt: 1})
		env.Ts = timestamppb.New(base.Add(time.Duration(i) * time.Second))
		env.CorrelationId = fmt.Sprintf("enter-%d", i)
		if _, _, err := s.AppendEvent(ctx, mustEventFromProto(t, env)); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.GetLatestStepEnteredEvent(ctx, "r-adapters")
	if err != nil {
		t.Fatalf("GetLatestStepEnteredEvent: %v", err)
	}
	var payload pb.StepEntered
	if err := protojson.Unmarshal(got.Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if payload.Adapter != "kubernetes" {
		t.Fatalf("adapter=%q want kubernetes", payload.Adapter)
	}
}

func TestGetLatestStepEnteredEvent_NotFound(t *testing.T) {
	s := tempStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := s.CreateOverseer(ctx, &store.Overseer{ID: "o1", Name: "x", TokenHash: "t", Status: "online", CreatedAt: now, LastSeenAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateRun(ctx, &store.Run{ID: "r-no-steps", OverseerID: "o1", WorkflowName: "w", WorkflowHCL: "x", Status: "pending", CurrentStep: "a", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetLatestStepEnteredEvent(ctx, "r-no-steps"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
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
		env := criteria.NewEnvelope("r-logs", &pb.StepLog{Step: "build", Stream: pb.LogStream_LOG_STREAM_STDOUT, Chunk: fmt.Sprintf("line-%d", i)})
		env.CorrelationId = fmt.Sprintf("build-%d", i)
		if _, _, err := s.AppendEvent(ctx, mustEventFromProto(t, env)); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 50; i++ {
		env := criteria.NewEnvelope("r-logs", &pb.StepLog{Step: "test", Stream: pb.LogStream_LOG_STREAM_STDOUT, Chunk: fmt.Sprintf("other-%d", i)})
		env.CorrelationId = fmt.Sprintf("test-%d", i)
		if _, _, err := s.AppendEvent(ctx, mustEventFromProto(t, env)); err != nil {
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
			if env.Type != "step.log" {
				t.Fatalf("expected step.log event, got %q", env.Type)
			}
			var logPayload pb.StepLog
			if err := protojson.Unmarshal(env.Payload, &logPayload); err != nil {
				t.Fatalf("unmarshal step log payload: %v", err)
			}
			if logPayload.Step != "build" {
				t.Fatalf("unexpected step %q", logPayload.Step)
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
