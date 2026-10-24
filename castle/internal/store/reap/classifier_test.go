package reap

import "testing"

// TestLastEventParksHumanGate is the classifier's contract table: exactly
// these last-event shapes park a run at a human gate, and everything else —
// including terminal completions that are not human gates and the duration
// variants — stays reapable with the legacy verdict.
func TestLastEventParksHumanGate(t *testing.T) {
	cases := []struct {
		name       string
		evType     string
		payload    string
		wantParked bool
		wantReason string
	}{
		{
			name:       "run.completed awaiting_human camelCase",
			evType:     "run.completed",
			payload:    `{"finalState":"awaiting_human","success":false}`,
			wantParked: true,
			wantReason: AwaitingHumanGateVerdictReason,
		},
		{
			name:       "run.completed awaiting_human snake_case",
			evType:     "run.completed",
			payload:    `{"final_state":"awaiting_human","success":false}`,
			wantParked: true,
			wantReason: AwaitingHumanGateVerdictReason,
		},
		{
			name:       "run.completed awaiting_human only success true",
			evType:     "run.completed",
			payload:    `{"finalState":"awaiting_human","success":true}`,
			wantParked: true,
			wantReason: AwaitingHumanGateVerdictReason,
		},
		{
			name:       "run.completed need_assistance camelCase (KB-270 rename)",
			evType:     "run.completed",
			payload:    `{"finalState":"need_assistance","success":false}`,
			wantParked: true,
			wantReason: AwaitingHumanGateVerdictReason,
		},
		{
			name:       "run.completed need_assistance snake_case (KB-270 rename)",
			evType:     "run.completed",
			payload:    `{"final_state":"need_assistance","success":false}`,
			wantParked: true,
			wantReason: AwaitingHumanGateVerdictReason,
		},
		{
			name:    "run.completed succeeded does not park",
			evType:  "run.completed",
			payload: `{"finalState":"succeeded","success":true}`,
		},
		{
			name:    "run.completed failed does not park",
			evType:  "run.completed",
			payload: `{"finalState":"failed","success":false}`,
		},
		{
			name:    "run.completed undecodable payload does not park",
			evType:  "run.completed",
			payload: `not json`,
		},
		{
			name:       "approval.requested parks on type",
			evType:     "approval.requested",
			payload:    `{"node":"ship","approvers":["dave"],"reason":"go/no-go"}`,
			wantParked: true,
			wantReason: ApprovalGateVerdictReason,
		},
		{
			name:       "approval.requested parks with empty payload",
			evType:     "approval.requested",
			payload:    `{}`,
			wantParked: true,
			wantReason: ApprovalGateVerdictReason,
		},
		{
			name:       "run.paused signal parks",
			evType:     "run.paused",
			payload:    `{"mode":"signal","signal":"resume_signal"}`,
			wantParked: true,
			wantReason: HumanPauseGateVerdictReason,
		},
		{
			name:       "run.paused external (actor) parks",
			evType:     "run.paused",
			payload:    `{"mode":"external","actor":"console"}`,
			wantParked: true,
			wantReason: HumanPauseGateVerdictReason,
		},
		{
			name:    "run.paused duration does not park",
			evType:  "run.paused",
			payload: `{"mode":"duration"}`,
		},
		{
			name:    "run.paused undecodable payload does not park",
			evType:  "run.paused",
			payload: `not json`,
		},
		{
			name:       "wait.entered signal parks via mode",
			evType:     "wait.entered",
			payload:    `{"node":"watch_deploy","mode":"signal","signal":"deploy_done"}`,
			wantParked: true,
			wantReason: SignalWaitGateVerdictReason,
		},
		{
			name:       "wait.entered parks via signal without mode",
			evType:     "wait.entered",
			payload:    `{"node":"watch_deploy","signal":"deploy_done"}`,
			wantParked: true,
			wantReason: SignalWaitGateVerdictReason,
		},
		{
			name:    "wait.entered duration does not park",
			evType:  "wait.entered",
			payload: `{"node":"sleepy","mode":"duration","duration":30}`,
		},
		{
			name:    "wait.entered undecodable payload does not park",
			evType:  "wait.entered",
			payload: `not json`,
		},
		{
			name:    "mid-flight step event does not park",
			evType:  "step.outcome",
			payload: `{"step":"s1","outcome":"ok"}`,
		},
		{
			name:    "run.failed does not park",
			evType:  "run.failed",
			payload: `{"reason":"boom","step":"s1"}`,
		},
		{
			name:    "unknown type does not park",
			evType:  "run.metadata",
			payload: `{"ticket":"CRI-104"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parked, reason := LastEventParksHumanGate(tc.evType, []byte(tc.payload))
			if parked != tc.wantParked {
				t.Fatalf("parked = %v (reason %q), want %v", parked, reason, tc.wantParked)
			}
			if tc.wantParked && reason != tc.wantReason {
				t.Fatalf("reason = %q, want %q", reason, tc.wantReason)
			}
		})
	}
}
