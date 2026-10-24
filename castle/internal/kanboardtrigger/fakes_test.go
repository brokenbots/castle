package kanboardtrigger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"connectrpc.com/connect"

	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"

	castlev1 "github.com/brokenbots/castle/castle/gen/castle/v1"

	"github.com/brokenbots/castle/castle/internal/kanboard"
)

// fakeKanboard is an in-memory Kanboard JSON-RPC endpoint covering the exact
// method set the client uses: getAllProjects, getProjectById, getAllTasks,
// getColumns, getTaskTags, getTask, moveTaskPosition, createComment,
// getVersion.
type fakeKanboard struct {
	mu       sync.Mutex
	project  string
	nextTask int
	tasks    map[int]*kanboard.Task
	tags     map[int][]string
	columns  []struct {
		ID    int
		Title string
	}
	comments   map[int][]string
	moves      []string // description of each move
	srv        *httptest.Server
	columnIDOf map[string]int
}

func newFakeKanboard(t *testing.T, project string, columns ...string) *fakeKanboard {
	fk := &fakeKanboard{
		project:  project,
		nextTask: 0,
		tasks:    map[int]*kanboard.Task{},
		tags:     map[int][]string{},
		comments: map[int][]string{},
	}
	fk.columnIDOf = map[string]int{}
	for i, name := range columns {
		id := i + 1
		fk.columns = append(fk.columns, struct {
			ID    int
			Title string
		}{id, name})
		fk.columnIDOf[name] = id
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/jsonrpc.php" {
			http.NotFound(w, r)
			return
		}
		fk.handle(w, r)
	})
	fk.srv = httptest.NewServer(mux)
	t.Cleanup(fk.srv.Close)
	return fk
}

// addCard seeds a task with tags in the given column name.
func (fk *fakeKanboard) addCard(t *testing.T, id int, column, title string, tags ...string) {
	fk.mu.Lock()
	defer fk.mu.Unlock()
	colID, ok := fk.columnIDOf[column]
	if !ok {
		t.Fatalf("fake kanboard: unknown column %q", column)
	}
	fk.nextTask = max(fk.nextTask, id)
	fk.tasks[id] = &kanboard.Task{ID: id, Title: title, ProjectID: 1, ColumnID: colID, SwimlaneID: 1}
	fk.tags[id] = append([]string(nil), tags...)
}

func (fk *fakeKanboard) mustColumn(t *testing.T, name string) int {
	t.Helper()
	fk.mu.Lock()
	defer fk.mu.Unlock()
	id, ok := fk.columnIDOf[name]
	if !ok {
		t.Fatalf("fake kanboard: unknown column %q", name)
	}
	return id
}

func (fk *fakeKanboard) columnOf(taskID int) string {
	fk.mu.Lock()
	defer fk.mu.Unlock()
	t, ok := fk.tasks[taskID]
	if !ok {
		return ""
	}
	for _, c := range fk.columns {
		if c.ID == t.ColumnID {
			return c.Title
		}
	}
	return ""
}

func (fk *fakeKanboard) columnNames() map[int]string {
	out := map[int]string{}
	for _, c := range fk.columns {
		out[c.ID] = c.Title
	}
	return out
}

func (fk *fakeKanboard) moveTargets() []string {
	fk.mu.Lock()
	defer fk.mu.Unlock()
	return append([]string(nil), fk.moves...)
}

func (fk *fakeKanboard) commentsOn(taskID int) []string {
	fk.mu.Lock()
	defer fk.mu.Unlock()
	return append([]string(nil), fk.comments[taskID]...)
}

func (fk *fakeKanboard) setCardColumn(taskID int, column string) {
	fk.mu.Lock()
	defer fk.mu.Unlock()
	if t, ok := fk.tasks[taskID]; ok {
		if id, ok := fk.columnIDOf[column]; ok {
			t.ColumnID = id
		}
	}
}

// decodeParams re-marshals the generic params value and unmarshals it into
// the concrete target (JSON into `any` yields map[string]interface{}, not
// json.RawMessage).
func decodeParams(req kanboard.RPCRequest, p any) {
	raw, err := json.Marshal(req.Params)
	if err == nil {
		_ = json.Unmarshal(raw, p)
	}
}

// errSubmitDenied is the submit-side failure used in retry tests.
var errSubmitDenied = connect.NewError(connect.CodePermissionDenied, errors.New("trigger identity denied"))

func (fk *fakeKanboard) handle(w http.ResponseWriter, r *http.Request) {
	var req kanboard.RPCRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	fk.mu.Lock()
	w.Header().Set("Content-Type", "application/json")
	var result any
	var rpcErr *kanboard.RPCError
	switch req.Method {
	case "getVersion":
		result = "1.2.54"
	case "getAllProjects":
		type proj struct {
			ID   int    `json:"id"`
			Name string `json:"name"`
		}
		result = []proj{{ID: 1, Name: fk.project}}
	case "getProjectById":
		type proj struct {
			Name string `json:"name"`
		}
		result = proj{Name: fk.project}
	case "getColumns":
		type col struct {
			ID    int    `json:"id"`
			Title string `json:"title"`
		}
		cols := make([]col, 0, len(fk.columns))
		for _, c := range fk.columns {
			cols = append(cols, col{c.ID, c.Title})
		}
		result = cols
	case "getAllTasks":
		type task struct {
			ID         int `json:"id"`
			Title      string
			Desc       string `json:"description"`
			ProjectID  int    `json:"project_id"`
			ColumnID   int    `json:"column_id"`
			SwimlaneID int    `json:"swimlane_id"`
			OwnerID    int    `json:"owner_id"`
		}
		out := []task{}
		for _, t := range fk.tasks {
			out = append(out, task{t.ID, t.Title, t.Description, t.ProjectID, t.ColumnID, t.SwimlaneID, t.OwnerID})
		}
		result = out
	case "getTaskTags":
		var p struct {
			TaskID int `json:"task_id"`
		}
		decodeParams(req, &p)
		tagMap := map[string]string{}
		for i, name := range fk.tags[p.TaskID] {
			tagMap[fmt.Sprintf("%d", i+1)] = name
		}
		result = tagMap
	case "getTask":
		var p struct {
			TaskID int `json:"task_id"`
		}
		decodeParams(req, &p)
		t, ok := fk.tasks[p.TaskID]
		if !ok {
			rpcErr = &kanboard.RPCError{Code: 1, Message: "task not found"}
			break
		}
		type task struct {
			ProjectID  int `json:"project_id"`
			ColumnID   int `json:"column_id"`
			SwimlaneID int `json:"swimlane_id"`
		}
		result = task{t.ProjectID, t.ColumnID, t.SwimlaneID}
	case "moveTaskPosition":
		var p struct {
			TaskID   int `json:"task_id"`
			ColumnID int `json:"column_id"`
		}
		decodeParams(req, &p)
		t, ok := fk.tasks[p.TaskID]
		if !ok {
			rpcErr = &kanboard.RPCError{Code: 1, Message: "task not found"}
			break
		}
		t.ColumnID = p.ColumnID
		name := fmt.Sprintf("task %d -> %d", p.TaskID, p.ColumnID)
		fk.moves = append(fk.moves, name)
		result = true
	case "createComment":
		var p struct {
			TaskID  int    `json:"task_id"`
			Content string `json:"content"`
		}
		decodeParams(req, &p)
		fk.comments[p.TaskID] = append(fk.comments[p.TaskID], p.Content)
		result = float64(len(fk.comments[p.TaskID]))
	default:
		rpcErr = &kanboard.RPCError{Code: 32603, Message: "unknown method " + req.Method}
	}
	w.Write([]byte(kanboard.MarshalRPC(req.ID, result, rpcErr)))
	fk.mu.Unlock()
}

// submitCall records one SubmitProjectAssignment invocation.
type submitCall struct {
	ProjectID      string
	WorkflowName   string
	WorkflowSource string
	LockfileSource string
	InputValues    map[string]string
	IdempotencyKey string
}

type fakeSubmitter struct {
	mu     sync.Mutex
	calls  []submitCall
	nextID int
	err    *connect.Error
	// adopt marks whether an existing assignment is reported instead of a
	// fresh one (created=false).
	adopt           bool
	dispState       pb.WorkflowAssignmentState
	rejectionReason string
}

// GetAssignmentDisposition completes the AssignmentReader half of the
// fake: the disposition source is configurable per test (default QUEUED).
func (f *fakeSubmitter) GetAssignmentDisposition(ctx context.Context, req *connect.Request[pb.GetAssignmentDispositionRequest]) (*connect.Response[pb.GetAssignmentDispositionResponse], error) {
	state := f.dispState
	if state == pb.WorkflowAssignmentState_WORKFLOW_ASSIGNMENT_STATE_UNSPECIFIED {
		state = pb.WorkflowAssignmentState_WORKFLOW_ASSIGNMENT_STATE_QUEUED
	}
	res := &pb.GetAssignmentDispositionResponse{
		RunId:           req.Msg.GetRunId(),
		State:           state,
		RejectionReason: f.rejectionReason,
	}
	return connect.NewResponse(res), nil
}

func (f *fakeSubmitter) SubmitProjectAssignment(ctx context.Context, req *connect.Request[castlev1.SubmitProjectAssignmentRequest]) (*connect.Response[castlev1.SubmitProjectAssignmentResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	msg := req.Msg
	f.calls = append(f.calls, submitCall{
		ProjectID:      msg.GetProjectId(),
		WorkflowName:   msg.GetWorkflowName(),
		WorkflowSource: msg.GetWorkflowSource(),
		LockfileSource: msg.GetLockfileSource(),
		InputValues:    msg.GetInputValues(),
		IdempotencyKey: msg.GetIdempotencyKey(),
	})
	if f.err != nil {
		return nil, f.err
	}
	f.nextID++
	state := pb.WorkflowAssignmentState_WORKFLOW_ASSIGNMENT_STATE_QUEUED
	if f.adopt {
		state = pb.WorkflowAssignmentState_WORKFLOW_ASSIGNMENT_STATE_LEASED
	}
	resp := connect.NewResponse(&castlev1.SubmitProjectAssignmentResponse{
		RunId: fmt.Sprintf("run-%d", f.nextID),
		State: state,
	})
	resp.Header().Set("Castle-Created", map[bool]string{true: "true", false: "false"}[f.adopt])
	return connect.NewResponse(resp.Msg), nil
}

func (f *fakeSubmitter) submitted() []submitCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]submitCall(nil), f.calls...)
}

// fakeRunEvents is a canned RunEventStream.
type fakeRunEvents struct {
	events []*pb.Envelope
	pos    int
}

func (s *fakeRunEvents) Receive() bool { return s.pos < len(s.events) }
func (s *fakeRunEvents) Msg() *pb.Envelope {
	e := s.events[s.pos]
	s.pos++
	return e
}
func (s *fakeRunEvents) Err() error   { return nil }
func (s *fakeRunEvents) Close() error { return nil }

type fakeRunWatcher struct {
	mu      sync.Mutex
	streams map[string][]func() RunEventStream
}

func newFakeRunWatcher() *fakeRunWatcher {
	return &fakeRunWatcher{streams: map[string][]func() RunEventStream{}}
}

func (f *fakeRunWatcher) WatchRun(ctx context.Context, req *connect.Request[pb.WatchRunRequest]) (RunEventStream, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if openers := f.streams[req.Msg.GetRunId()]; len(openers) > 0 {
		s := openers[0]()
		f.streams[req.Msg.GetRunId()] = openers[1:]
		return s, nil
	}
	return &fakeRunEvents{}, nil
}

func (f *fakeRunWatcher) setEvents(runID string, events ...*pb.Envelope) {
	f.mu.Lock()
	defer f.mu.Unlock()
	evs := append([]*pb.Envelope(nil), events...)
	f.streams[runID] = append(f.streams[runID], func() RunEventStream {
		return &fakeRunEvents{events: evs}
	})
}
