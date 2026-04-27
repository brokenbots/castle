//go:build integration

package integration

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	pb "github.com/brokenbots/overlord/shared/pb/overlord/v1"
)

// TestCastle_DemoTour runs the demo_tour.hcl workflow end-to-end and verifies
// five scenarios:
//  1. ForEach progression events
//  2. Wait auto-elapsed (duration-based)
//  3. Approval signal mismatch then successful approval
//  4. WatchRun replays all events in order
//  5. ListRunEvents paging consistency
func TestCastle_DemoTour(t *testing.T) {
	castle := StartCastle(t)
	overseer := StartOverseer(t, castle.URL, filepath.Join(repoRoot(), "examples", "demo_tour.hcl"))
	_ = overseer

	castleClient := NewCastleClient(castle.URL)
	runID := WaitForRun(t, castleClient, 30*time.Second)
	t.Logf("run ID: %s", runID)

	// Obtain the overseer token from the checkpoint file.
	cp := WaitForCheckpoint(t, overseer.StateDir, runID, 30*time.Second)
	overseerClient := NewOverseerClient(castle.URL, cp.Token)

	// ── Scenario 1: ForEach progression ──────────────────────────────────────

	t.Run("Scenario1_ForEachProgression", func(t *testing.T) {
		// Count for_each items from the workflow file.
		itemCount := countForEachItems(t, filepath.Join(repoRoot(), "examples", "demo_tour.hcl"))
		t.Logf("for_each item count from workflow: %d", itemCount)

		// Poll until the ForEachOutcome event appears (all iterations complete)
		// or the timeout elapses.
		deadline := time.Now().Add(30 * time.Second)
		var events []*pb.Envelope
		for time.Now().Before(deadline) {
			events = ListAllEvents(t, castleClient, runID)
			if hasEventType(events, "for_each.outcome") {
				break
			}
			time.Sleep(300 * time.Millisecond)
		}

		var forEachEntered []*pb.Envelope
		var forEachIteration []*pb.Envelope
		var forEachOutcome []*pb.Envelope
		var scopeCursorSet []*pb.Envelope

		for _, e := range events {
			switch EventTypeString(e) {
			case "for_each.entered":
				forEachEntered = append(forEachEntered, e)
			case "for_each.iteration":
				forEachIteration = append(forEachIteration, e)
			case "for_each.outcome":
				forEachOutcome = append(forEachOutcome, e)
			case "scope.iter_cursor_set":
				scopeCursorSet = append(scopeCursorSet, e)
			}
		}

		// Exactly 1 ForEachEntered with node == "process_each".
		if len(forEachEntered) != 1 {
			t.Errorf("expected 1 for_each.entered, got %d", len(forEachEntered))
		} else if n := forEachEntered[0].GetForEachEntered().GetNode(); n != "process_each" {
			t.Errorf("for_each.entered node: want %q, got %q", "process_each", n)
		}

		// ForEachIteration count == item count.
		if len(forEachIteration) != itemCount {
			t.Errorf("expected %d for_each.iteration events, got %d", itemCount, len(forEachIteration))
		}

		// ScopeIterCursorSet count >= item count.
		// The engine emits cursor events at the start of each iteration,
		// after each iteration completes (to advance the index), and once
		// when the loop ends (cursor cleared). Total = 2*itemCount + 1.
		if len(scopeCursorSet) < itemCount {
			t.Errorf("expected at least %d scope.iter_cursor_set events, got %d", itemCount, len(scopeCursorSet))
		}

		// Exactly 1 ForEachOutcome.
		if len(forEachOutcome) != 1 {
			t.Errorf("expected 1 for_each.outcome, got %d", len(forEachOutcome))
		}

		// Ordering: ForEachEntered seq < all ForEachIteration seqs < ForEachOutcome seq.
		if len(forEachEntered) == 1 && len(forEachIteration) > 0 && len(forEachOutcome) == 1 {
			enteredSeq := forEachEntered[0].GetSeq()
			outcomeSeq := forEachOutcome[0].GetSeq()
			for _, iter := range forEachIteration {
				iterSeq := iter.GetSeq()
				if iterSeq <= enteredSeq {
					t.Errorf("for_each.iteration seq %d <= for_each.entered seq %d", iterSeq, enteredSeq)
				}
				if iterSeq >= outcomeSeq {
					t.Errorf("for_each.iteration seq %d >= for_each.outcome seq %d", iterSeq, outcomeSeq)
				}
			}
		}
	})

	// ── Scenario 2: Wait auto-elapsed ────────────────────────────────────────

	t.Run("Scenario2_WaitAutoElapsed", func(t *testing.T) {
		// Poll until both wait.entered and wait.resumed appear for node "wait_for_window".
		deadline := time.Now().Add(30 * time.Second)
		var waitEntered, waitResumed *pb.Envelope
		for time.Now().Before(deadline) {
			events := ListAllEvents(t, castleClient, runID)
			for _, e := range events {
				switch e.Payload.(type) {
				case *pb.Envelope_WaitEntered:
					if e.GetWaitEntered().GetNode() == "wait_for_window" {
						waitEntered = e
					}
				case *pb.Envelope_WaitResumed:
					if e.GetWaitResumed().GetNode() == "wait_for_window" {
						waitResumed = e
					}
				}
			}
			if waitEntered != nil && waitResumed != nil {
				break
			}
			time.Sleep(300 * time.Millisecond)
		}

		if waitEntered == nil {
			t.Fatal("wait.entered event for wait_for_window not found within 30s")
		}
		if waitResumed == nil {
			t.Fatal("wait.resumed event for wait_for_window not found within 30s")
		}
		if waitEntered.GetSeq() >= waitResumed.GetSeq() {
			t.Errorf("wait.entered seq %d should be < wait.resumed seq %d",
				waitEntered.GetSeq(), waitResumed.GetSeq())
		}
	})

	// ── Scenario 3: Approval signal mismatch then success ────────────────────

	t.Run("Scenario3_ApprovalAndSignalMismatch", func(t *testing.T) {
		WaitForRunStatus(t, castleClient, runID, "paused", 60*time.Second)

		// Wrong signal → rejected.
		mismatchResp, err := overseerClient.Resume(context.Background(), connect.NewRequest(&pb.ResumeRequest{
			RunId:  runID,
			Signal: "wrong_signal",
		}))
		if err != nil {
			t.Fatalf("Resume(wrong_signal): %v", err)
		}
		if mismatchResp.Msg.Accepted {
			t.Error("expected Accepted=false for wrong_signal")
		}
		if mismatchResp.Msg.Reason != "signal_mismatch" {
			t.Errorf("expected reason %q, got %q", "signal_mismatch", mismatchResp.Msg.Reason)
		}

		// Correct signal → accepted.
		approveResp, err := overseerClient.Resume(context.Background(), connect.NewRequest(&pb.ResumeRequest{
			RunId:  runID,
			Signal: "ship_approval",
			Payload: map[string]string{
				"decision": "approved",
			},
		}))
		if err != nil {
			t.Fatalf("Resume(ship_approval): %v", err)
		}
		if !approveResp.Msg.Accepted {
			t.Errorf("expected Accepted=true for ship_approval, got reason %q", approveResp.Msg.Reason)
		}
		if approveResp.Msg.Reason != "ok" {
			t.Errorf("expected reason %q, got %q", "ok", approveResp.Msg.Reason)
		}
	})

	// ── Scenario 4: WatchRun replay ──────────────────────────────────────────

	t.Run("Scenario4_WatchRunReplay", func(t *testing.T) {
		WaitForRunStatus(t, castleClient, runID, "succeeded", 30*time.Second)

		listEvents := ListAllEvents(t, castleClient, runID)

		// Open WatchRun from the beginning.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		stream, err := castleClient.WatchRun(ctx, connect.NewRequest(&pb.WatchRunRequest{
			RunId:    runID,
			SinceSeq: 0,
		}))
		if err != nil {
			t.Fatalf("WatchRun: %v", err)
		}

		var watchEvents []*pb.Envelope
		for stream.Receive() {
			env := stream.Msg()
			// Skip the WatchReady sentinel.
			if _, ok := env.Payload.(*pb.Envelope_WatchReady); ok {
				continue
			}
			watchEvents = append(watchEvents, env)
		}
		if err := stream.Err(); err != nil {
			t.Errorf("WatchRun stream closed with unexpected error: %v", err)
		}

		// Same length.
		if len(watchEvents) != len(listEvents) {
			t.Errorf("watch events count %d != list events count %d", len(watchEvents), len(listEvents))
		}

		// Same seq values in order.
		minLen := len(listEvents)
		if len(watchEvents) < minLen {
			minLen = len(watchEvents)
		}
		for i := 0; i < minLen; i++ {
			if watchEvents[i].GetSeq() != listEvents[i].GetSeq() {
				t.Errorf("event[%d]: watch seq %d != list seq %d",
					i, watchEvents[i].GetSeq(), listEvents[i].GetSeq())
			}
			if EventTypeString(watchEvents[i]) != EventTypeString(listEvents[i]) {
				t.Errorf("event[%d]: watch type %q != list type %q",
					i, EventTypeString(watchEvents[i]), EventTypeString(listEvents[i]))
			}
			if !proto.Equal(watchEvents[i], listEvents[i]) {
				t.Errorf("event[%d] (seq=%d, type=%q): watch and list payloads differ",
					i, listEvents[i].GetSeq(), EventTypeString(listEvents[i]))
			}
		}

		// Assert required event types are present (superset of the workflow's expected emit set).
		requiredTypes := []string{
			"run.started",
			"step.entered",
			"step.outcome",
			"step.output_captured",
			"step.transition",
			"variable.set",
			"for_each.entered",
			"for_each.iteration",
			"for_each.outcome",
			"scope.iter_cursor_set",
			"wait.entered",
			"wait.resumed",
			"approval.requested",
			"approval.decision",
			"branch.evaluated",
			"run.completed",
		}
		seenTypes := make(map[string]bool)
		for _, e := range watchEvents {
			seenTypes[EventTypeString(e)] = true
		}
		for _, want := range requiredTypes {
			if !seenTypes[want] {
				t.Errorf("required event type %q not found in watch events", want)
			}
		}
	})

	// ── Scenario 5: ListRunEvents paging consistency ──────────────────────────

	t.Run("Scenario5_ListRunEventsPaging", func(t *testing.T) {
		// Page with Limit=3 and reconstruct full sequence.
		var pagedEvents []*pb.Envelope
		var sinceSeq uint64
		for {
			resp, err := castleClient.ListRunEvents(context.Background(), connect.NewRequest(&pb.ListRunEventsRequest{
				RunId:    runID,
				SinceSeq: sinceSeq,
				Limit:    3,
			}))
			if err != nil {
				t.Fatalf("ListRunEvents(limit=3): %v", err)
			}
			pagedEvents = append(pagedEvents, resp.Msg.Events...)
			if len(resp.Msg.Events) < 3 {
				break
			}
			sinceSeq = resp.Msg.NextSinceSeq
			if sinceSeq == 0 {
				break
			}
		}

		// No seq gaps: each event's seq > previous event's seq.
		for i := 1; i < len(pagedEvents); i++ {
			if pagedEvents[i].GetSeq() <= pagedEvents[i-1].GetSeq() {
				t.Errorf("seq gap at index %d: seq %d <= seq %d",
					i, pagedEvents[i].GetSeq(), pagedEvents[i-1].GetSeq())
			}
		}

		// Total count matches ListAllEvents result.
		allEvents := ListAllEvents(t, castleClient, runID)
		if len(pagedEvents) != len(allEvents) {
			t.Errorf("paged count %d != ListAllEvents count %d", len(pagedEvents), len(allEvents))
		}
	})
}

// TestCastle_RestartDurability verifies scenario 6: Castle restart preserves
// the durable state of an approved run.
func TestCastle_RestartDurability(t *testing.T) {
	castle := StartCastle(t)
	overseer := StartOverseer(t, castle.URL, filepath.Join(repoRoot(), "examples", "demo_tour.hcl"))
	_ = overseer

	castleClient := NewCastleClient(castle.URL)
	runID := WaitForRun(t, castleClient, 30*time.Second)
	t.Logf("run ID: %s", runID)

	cp := WaitForCheckpoint(t, overseer.StateDir, runID, 30*time.Second)
	overseerClient := NewOverseerClient(castle.URL, cp.Token)

	WaitForRunStatus(t, castleClient, runID, "paused", 60*time.Second)

	// Approve the run.
	approveResp, err := overseerClient.Resume(context.Background(), connect.NewRequest(&pb.ResumeRequest{
		RunId:  runID,
		Signal: "ship_approval",
		Payload: map[string]string{
			"decision": "approved",
		},
	}))
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if !approveResp.Msg.Accepted {
		t.Fatalf("Resume not accepted: %s", approveResp.Msg.Reason)
	}

	// Brief pause to let Castle persist the approval decision before kill.
	time.Sleep(100 * time.Millisecond)

	// SIGKILL Castle.
	if castle.cmd.Process != nil {
		_ = castle.cmd.Process.Kill()
		_ = castle.cmd.Wait()
	}

	// Restart Castle on the same port and DB.
	castle2 := startCastleOnDB(t, castle.DBPath, portFromAddr(castle.URL))
	castleClient2 := NewCastleClient(castle2.URL)

	// Verify the run exists and is no longer paused (Resume was durable).
	runResp, err := castleClient2.GetRun(context.Background(), connect.NewRequest(&pb.GetRunRequest{RunId: runID}))
	if err != nil {
		t.Fatalf("GetRun after restart: %v", err)
	}
	status := runResp.Msg.Status
	if status == "paused" {
		t.Errorf("run is still paused after restart; expected running or succeeded (Resume should be durable)")
	}
	t.Logf("run status after Castle restart: %s", status)

	// ApprovalDecision event is persisted.
	events := ListAllEvents(t, castleClient2, runID)
	if !hasEventType(events, "approval.decision") {
		t.Error("approval.decision event not found after Castle restart; event log not durable")
	}

	// Sending Resume again should be rejected because the run is no longer paused.
	overseerClient2 := NewOverseerClient(castle2.URL, cp.Token)
	retryResp, err := overseerClient2.Resume(context.Background(), connect.NewRequest(&pb.ResumeRequest{
		RunId:  runID,
		Signal: "ship_approval",
		Payload: map[string]string{
			"decision": "approved",
		},
	}))
	if err != nil {
		t.Fatalf("Resume retry after restart: %v", err)
	}
	if retryResp.Msg.Accepted {
		t.Error("expected Resume to be rejected after restart (run is no longer paused)")
	}
	t.Logf("retry Resume reason: %s", retryResp.Msg.Reason)

	t.Log("DOCUMENTED LIMITATION: control message lost on Castle restart; overseer stuck waiting on ResumeCh. " +
		"Re-issuing Resume after restart would require overseer reconnect + forward-pointer: " +
		"overlord post-split cleanup (W06/W08)")
}

// ─── Helpers local to the test file ──────────────────────────────────────────

// hasEventType returns true if any envelope in the slice matches the given type string.
func hasEventType(events []*pb.Envelope, typStr string) bool {
	for _, e := range events {
		if EventTypeString(e) == typStr {
			return true
		}
	}
	return false
}

// countForEachItems reads the workflow file and counts the string items in the
// `items = [...]` line of the for_each block. This avoids hardcoding 3.
func countForEachItems(t *testing.T, workflowPath string) int {
	t.Helper()
	f, err := os.Open(workflowPath)
	if err != nil {
		t.Fatalf("countForEachItems: open %s: %v", workflowPath, err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "items") {
			continue
		}
		// Extract content between [ and ]
		start := strings.Index(trimmed, "[")
		end := strings.LastIndex(trimmed, "]")
		if start < 0 || end <= start {
			continue
		}
		inner := trimmed[start+1 : end]
		// Count quoted string tokens: split by comma and count non-empty.
		count := 0
		for _, part := range strings.Split(inner, ",") {
			part = strings.TrimSpace(part)
			if strings.HasPrefix(part, `"`) || strings.HasPrefix(part, `'`) {
				count++
			}
		}
		if count > 0 {
			return count
		}
	}
	t.Fatalf("countForEachItems: could not find items = [...] line in %s", workflowPath)
	return 0
}
