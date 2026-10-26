package rpc

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/brokenbots/castle/castle/internal/kanboard"
)

// optionbFakeKanboard is an in-memory Kanboard JSON-RPC endpoint for the
// Option B acceptance suite (KB-341): it covers the exact method set the
// production board client uses — getAllProjects, getProjectById, getAllTasks,
// getColumns, getTaskTags, getTask, moveTaskPosition, createComment, and
// getVersion — so the real kanboardtrigger adapter and kanboard.Client run
// against it unchanged.
type optionbFakeKanboard struct {
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
	moves      []string
	srv        *httptest.Server
	columnIDOf map[string]int
}

func newOptionbFakeKanboard(t *testing.T, project string, columns ...string) *optionbFakeKanboard {
	t.Helper()
	fk := &optionbFakeKanboard{
		project:  project,
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
func (fk *optionbFakeKanboard) addCard(t *testing.T, id int, column, title string, tags ...string) {
	t.Helper()
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

// columnOf returns the card's current column title.
func (fk *optionbFakeKanboard) columnOf(taskID int) string {
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

// commentsOn returns the comments the mirror left on the card.
func (fk *optionbFakeKanboard) commentsOn(taskID int) []string {
	fk.mu.Lock()
	defer fk.mu.Unlock()
	return append([]string(nil), fk.comments[taskID]...)
}

// moveTargets returns one description per column move the mirror performed.
func (fk *optionbFakeKanboard) moveTargets() []string {
	fk.mu.Lock()
	defer fk.mu.Unlock()
	return append([]string(nil), fk.moves...)
}

func (fk *optionbFakeKanboard) handle(w http.ResponseWriter, r *http.Request) {
	var req kanboard.RPCRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	fk.mu.Lock()
	defer fk.mu.Unlock()
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
			ID         int    `json:"id"`
			Title      string `json:"title"`
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
		fk.moves = append(fk.moves, fmt.Sprintf("task %d -> %d", p.TaskID, p.ColumnID))
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
