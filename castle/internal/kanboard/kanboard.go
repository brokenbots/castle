// Package kanboard provides a minimal JSON-RPC client for polling Kanboard
// tasks and moving them across board columns. It is the ticket source of
// record for the kanboard trigger adapter (KB-320), with the same
// fail-closed conventions and consumer discipline as the outgoing
// criteria-k8s watcher (one listing read per poll, no per-ticket reads on
// the steady-state path except tag hydration).
package kanboard

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"sync"
	"time"
)

// Client calls a Kanboard JSON-RPC 2.0 endpoint (jsonrpc.php).
type Client struct {
	// BaseURL is the full JSON-RPC endpoint, including the subpath when the
	// instance is served under a path prefix (e.g.
	// http://host/kanboard/jsonrpc.php).
	BaseURL string
	// AppToken authenticates as the app-level user "jsonrpc".
	AppToken string
	HTTP     *http.Client

	// mu guards the lazily populated project/column caches. Both are
	// immutable lookups (names -> ids) resolved once per process; Kanboard
	// project and column ids are stable for the life of the instance.
	mu         sync.Mutex
	projectIDs map[string]int
	columnIDs  map[int]map[string]int // projectID -> column title -> column id
}

// Task is a Kanboard task relevant to the adapter.
type Task struct {
	ID          int    `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	ProjectID   int    `json:"project_id"`
	ProjectName string `json:"project_name"`
	ColumnID    int    `json:"column_id"`
	ColumnName  string `json:"column_name"`
	SwimlaneID  int    `json:"swimlane_id"`
	// Tags holds the task's tag names. Lanes gate on them (trigger tag) and
	// the repo binding is a repo:<owner>/<name> tag.
	Tags []string `json:"tags,omitempty"`
	// Creator/Owner ids are reserved for future per-identity mapping; the
	// adapter does not use them.
	OwnerID int `json:"owner_id"`
}

// Identifier is the ticket identity the rest of the platform keys on. The
// KB- prefix namespaces Kanboard tickets from other ticket sources so
// multiple adapters can coexist in one namespace without TicketID
// collisions.
func (t Task) Identifier() string {
	return fmt.Sprintf("KB-%d", t.ID)
}

// New returns a Kanboard JSON-RPC client. baseURL must be the full endpoint
// URL (jsonrpc.php included).
func New(baseURL, appToken string) *Client {
	return &Client{
		BaseURL:  baseURL,
		AppToken: appToken,
		HTTP:     &http.Client{Timeout: 30 * time.Second},
	}
}

// fetchTagNames returns a task's tag names. Kanboard's getTaskTags takes
// only task_id and returns a map keyed by tag-link id with tag names as
// values; the names are extracted in the JSON-decoded order of the map (Go
// sorts map keys when encoding JSON, so the order is deterministic).
func (c *Client) fetchTagNames(ctx context.Context, taskID int, out *[]string) error {
	var raw map[string]string
	if err := c.call(ctx, "getTaskTags", map[string]any{"task_id": taskID}, &raw); err != nil {
		return err
	}
	names := make([]string, 0, len(raw))
	for _, name := range raw {
		names = append(names, name)
	}
	sort.Strings(names)
	*out = names
	return nil
}

// RPCRequest is the JSON-RPC 2.0 request envelope.
type RPCRequest struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	ID      int    `json:"id"`
	Params  any    `json:"params,omitempty"`
}

// RPCResponse is the JSON-RPC 2.0 response envelope. Kanboard reports
// application errors both in-band (error member) and, for auth failures, via
// HTTP 401.
type RPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *RPCError       `json:"error"`
}

// RPCError is a JSON-RPC error object (in-band or decoded from a 401 body).
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	if e.Data != nil {
		return fmt.Sprintf("kanboard rpc error %d: %s (%v)", e.Code, e.Message, e.Data)
	}
	return fmt.Sprintf("kanboard rpc error %d: %s", e.Code, e.Message)
}

// MarshalRPC encodes a response envelope for one request id (used by tests
// and fakes standing in for a real Kanboard).
func MarshalRPC(id int, result any, rpcErr *RPCError) string {
	resp := RPCResponse{JSONRPC: "2.0", ID: id}
	if rpcErr != nil {
		resp.Error = rpcErr
	} else if raw, err := json.Marshal(result); err == nil {
		resp.Result = raw
	} else {
		resp.Error = &RPCError{Code: -32603, Message: err.Error()}
	}
	out, _ := json.Marshal(resp)
	return string(out)
}

// call invokes one JSON-RPC method and decodes the result into target.
func (c *Client) call(ctx context.Context, method string, params any, target any) error {
	body, err := json.Marshal(RPCRequest{JSONRPC: "2.0", Method: method, ID: 1, Params: params})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	// App auth: username "jsonrpc", password = the app token, HTTP basic.
	req.SetBasicAuth("jsonrpc", c.AppToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("kanboard request: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading kanboard response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("kanboard returned HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	var envelope RPCResponse
	if err := json.Unmarshal(respBody, &envelope); err != nil {
		return fmt.Errorf("parsing kanboard response: %w", err)
	}
	if envelope.Error != nil {
		return envelope.Error
	}
	if target != nil && len(envelope.Result) > 0 {
		if err := json.Unmarshal(envelope.Result, target); err != nil {
			return fmt.Errorf("decoding kanboard result for %s: %w", method, err)
		}
	}
	return nil
}

// taskResult is the task shape getAllTasks returns. Kanboard returns
// snake_case fields; column_name and project_name are resolved client-side
// (see GetAllTasks) because getAllTasks does not join them.
type taskResult struct {
	ID          int    `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	ProjectID   int    `json:"project_id"`
	ColumnID    int    `json:"column_id"`
	SwimlaneID  int    `json:"swimlane_id"`
	OwnerID     int    `json:"owner_id"`
}

// GetAllTasks returns every task in the given project, with tags hydrated.
// Kanboard's getAllTasks does not hydrate tags (verified against 1.2.54:
// the tags field is absent from the listing payload), so each task's tags
// are fetched with getTaskTags. The per-project name/column lookups are
// cached (immutable after first resolution), so the per-poll request budget
// is 1 listing + N tag reads for N tasks — bounded by the watched project's
// open-task count (the steady-state request-frugality contract).
func (c *Client) GetAllTasks(ctx context.Context, projectID int) ([]Task, error) {
	var raw []taskResult
	if err := c.call(ctx, "getAllTasks", map[string]any{"project_id": projectID}, &raw); err != nil {
		return nil, err
	}
	projName, err := c.projectName(ctx, projectID)
	if err != nil {
		return nil, err
	}
	colNames, err := c.columnNames(ctx, projectID)
	if err != nil {
		return nil, err
	}
	out := make([]Task, 0, len(raw))
	for _, t := range raw {
		task := Task{
			ID:          t.ID,
			Title:       t.Title,
			Description: t.Description,
			ProjectID:   t.ProjectID,
			ProjectName: projName,
			ColumnID:    t.ColumnID,
			ColumnName:  colNames[t.ColumnID],
			SwimlaneID:  t.SwimlaneID,
			OwnerID:     t.OwnerID,
		}
		// Kanboard's getTaskTags takes ONLY task_id and returns a map of
		// tag-link-id -> tag name (verified against 1.2.54: passing
		// project_id too fails with -32602 "Too many arguments").
		var tagNames []string
		if err := c.fetchTagNames(ctx, task.ID, &tagNames); err != nil {
			return nil, err
		}
		task.Tags = tagNames
		out = append(out, task)
	}
	return out, nil
}

// FindProjectID resolves a project name to its Kanboard project id.
// Projects are an immutable lookup resolved once and cached.
func (c *Client) FindProjectID(ctx context.Context, name string) (int, error) {
	c.mu.Lock()
	if c.projectIDs != nil {
		id, ok := c.projectIDs[name]
		c.mu.Unlock()
		if ok {
			return id, nil
		}
	}
	c.mu.Unlock()

	var all []struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
	}
	if err := c.call(ctx, "getAllProjects", nil, &all); err != nil {
		return 0, err
	}
	ids := make(map[string]int, len(all))
	found := 0
	for _, p := range all {
		ids[p.Name] = p.ID
		if p.Name == name {
			found = p.ID
		}
	}
	c.mu.Lock()
	c.projectIDs = ids
	c.mu.Unlock()
	if found == 0 {
		return 0, fmt.Errorf("kanboard project %q not found", name)
	}
	return found, nil
}

// GetColumns returns the board columns of a project (title -> id), cached.
func (c *Client) GetColumns(ctx context.Context, projectID int) (map[string]int, error) {
	c.mu.Lock()
	if c.columnIDs != nil {
		cols, ok := c.columnIDs[projectID]
		c.mu.Unlock()
		if ok {
			return cols, nil
		}
	}
	c.mu.Unlock()

	var raw []struct {
		ID    int    `json:"id"`
		Title string `json:"title"`
	}
	if err := c.call(ctx, "getColumns", map[string]any{"project_id": projectID}, &raw); err != nil {
		return nil, err
	}
	cols := make(map[string]int, len(raw))
	for _, col := range raw {
		cols[col.Title] = col.ID
	}
	c.mu.Lock()
	if c.columnIDs == nil {
		c.columnIDs = make(map[int]map[string]int)
	}
	c.columnIDs[projectID] = cols
	c.mu.Unlock()
	return cols, nil
}

// columnName resolves a column id to its title via the cached column map.
func (c *Client) columnName(ctx context.Context, projectID, columnID int) string {
	cols, err := c.GetColumns(ctx, projectID)
	if err != nil {
		return ""
	}
	for title, id := range cols {
		if id == columnID {
			return title
		}
	}
	return ""
}

// projectName resolves a project id to its name.
func (c *Client) projectName(ctx context.Context, projectID int) (string, error) {
	var p struct {
		Name string `json:"name"`
	}
	if err := c.call(ctx, "getProjectById", map[string]any{"project_id": projectID}, &p); err != nil {
		return "", err
	}
	return p.Name, nil
}

// columnNames resolves every column id of a project to its title.
func (c *Client) columnNames(ctx context.Context, projectID int) (map[int]string, error) {
	cols, err := c.GetColumns(ctx, projectID)
	if err != nil {
		return nil, err
	}
	out := make(map[int]string, len(cols))
	for title, id := range cols {
		out[id] = title
	}
	return out, nil
}

// GetTask returns one task with its tags. It backs per-ticket verification.
func (c *Client) GetTask(ctx context.Context, taskID int) (*Task, error) {
	var t taskResult
	if err := c.call(ctx, "getTask", map[string]any{"task_id": taskID}, &t); err != nil {
		return nil, err
	}
	task := Task{
		ID:         t.ID,
		Title:      t.Title,
		ProjectID:  t.ProjectID,
		ColumnID:   t.ColumnID,
		SwimlaneID: t.SwimlaneID,
		OwnerID:    t.OwnerID,
	}
	// Same API shape as GetAllTasks hydration (map of tag-link-id -> name);
	// tag lookup failure leaves tags empty rather than failing the read.
	var tagNames []string
	if err := c.fetchTagNames(ctx, task.ID, &tagNames); err == nil {
		task.Tags = tagNames
	}
	return &task, nil
}

// TaskColumnName resolves a task's column id to a title (cached lookup).
func (c *Client) TaskColumnName(ctx context.Context, task *Task) string {
	return c.columnName(ctx, task.ProjectID, task.ColumnID)
}

// MoveTaskToColumn moves a task to the named column of its project. It backs
// the mirror's state moves (the ticket bookkeeping contract).
func (c *Client) MoveTaskToColumn(ctx context.Context, taskID int, columnName string) (int, error) {
	var t struct {
		ProjectID  int `json:"project_id"`
		ColumnID   int `json:"column_id"`
		SwimlaneID int `json:"swimlane_id"`
	}
	if err := c.call(ctx, "getTask", map[string]any{"task_id": taskID}, &t); err != nil {
		return 0, err
	}
	cols, err := c.GetColumns(ctx, t.ProjectID)
	if err != nil {
		return 0, err
	}
	colID, ok := cols[columnName]
	if !ok {
		return 0, fmt.Errorf("kanboard column %q not found on project %d", columnName, t.ProjectID)
	}
	if colID == t.ColumnID {
		return t.ColumnID, nil // already there; idempotent
	}
	// moveTaskPosition requires swimlane_id on 1.2.54 (omitting it fails
	// with -32602 "Wrong number of arguments"). The task's current swimlane
	// is preserved.
	if err := c.call(ctx, "moveTaskPosition", map[string]any{
		"project_id":  t.ProjectID,
		"task_id":     taskID,
		"column_id":   colID,
		"position":    1,
		"swimlane_id": t.SwimlaneID,
	}, nil); err != nil {
		return 0, err
	}
	return colID, nil
}

// AddComment posts a comment on a task under the app user.
func (c *Client) AddComment(ctx context.Context, taskID int, body string) error {
	return c.call(ctx, "createComment", map[string]any{
		"task_id": taskID,
		"user_id": 0,
		"content": body,
	}, nil)
}

// Ping verifies the endpoint is reachable and credentials work.
func (c *Client) Ping(ctx context.Context) error {
	var version string
	return c.call(ctx, "getVersion", nil, &version)
}
