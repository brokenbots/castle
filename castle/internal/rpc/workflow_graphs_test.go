package rpc

import (
	"testing"

	criteria "github.com/brokenbots/criteria/sdk"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

// TestOrchestratorWorkflowGraphsIngest verifies the server accepts
// workflow.graphs envelopes (CRI-257): the compiled subworkflow layers are
// stored and re-served through the event history with every field intact,
// and the discriminator round-trips as "workflow.graphs".
func TestOrchestratorWorkflowGraphsIngest(t *testing.T) {
	h := newOrchestratorHarness(t)
	runID := h.newAgentRun(t, "wf-graphs")

	graphs := criteria.NewEnvelope(runID, &pb.WorkflowGraphs{
		Subworkflows: []*pb.SubworkflowGraph{
			{Name: "qa_triage", SourcePath: "../qa_triage_v1", Body: "step \"triage\" {\n}\n"},
			{Name: "enrich", SourcePath: "../enrich_v2", Body: "step \"enrich\" {\n}\n"},
		},
	})
	graphs.CorrelationId = "wg-1"
	started := criteria.NewEnvelope(runID, &pb.RunStarted{WorkflowName: "wf-graphs", InitialStep: "step-1"})
	started.CorrelationId = "wg-started"
	completed := criteria.NewEnvelope(runID, &pb.RunCompleted{Success: true})
	completed.CorrelationId = "wg-completed"
	h.submitEvents(t, []*pb.Envelope{started, graphs, completed})

	events := pollRunEvents(t, h, runID, 0)
	if len(events) != 3 {
		t.Fatalf("want 3 events, got %d", len(events))
	}
	got, ok := events[1].Payload.(*pb.Envelope_WorkflowGraphs)
	if !ok {
		t.Fatalf("expected workflow.graphs second, got %T", events[1].Payload)
	}
	wf := got.WorkflowGraphs
	if len(wf.Subworkflows) != 2 {
		t.Fatalf("want 2 subworkflows, got %d", len(wf.Subworkflows))
	}
	if wf.Subworkflows[0].Name != "qa_triage" ||
		wf.Subworkflows[0].SourcePath != "../qa_triage_v1" ||
		wf.Subworkflows[0].Body != "step \"triage\" {\n}\n" {
		t.Errorf("subworkflow[0] not preserved: %+v", wf.Subworkflows[0])
	}
	if wf.Subworkflows[1].Name != "enrich" {
		t.Errorf("subworkflow[1] name not preserved: %+v", wf.Subworkflows[1])
	}
}