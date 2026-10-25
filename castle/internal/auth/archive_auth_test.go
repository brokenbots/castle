package auth

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	castlev1connect "github.com/brokenbots/castle/castle/gen/castle/v1/castlev1connect"
	"github.com/brokenbots/castle/castle/internal/store"
)

// The archive authorization matrix (KB-308, adjusted by KB-334): ArchiveRun
// is the operator's write and rides the same token path as the podstate feed
// (criteria agent tokens) — and the orchestrator role is allowed to invoke it
// under the KB-330 attribute model, because it is the operator's own
// terminal-archive leg (KB-308): it mutates no agent state, it stores the
// CR-evidence record keyed by run id / CR-uid archive key. It is NEVER
// anon-readable and never reachable by console or trigger identities, whose
// tokens must not be able to forge run history. The two read surfaces are
// observation-only: every authenticated identity may read, and they join the
// dev-mode anonymous-read surface like the ServerService reads.

func TestArchiveWriteCredentialMatrix(t *testing.T) {
	procedure := castlev1connect.RunArchiveServiceArchiveRunProcedure
	t.Run("no token", func(t *testing.T) {
		db := newAuthTestStore(t)
		call := newUnaryProbe(t, db, procedure, false, nil)
		err := call("")
		if connect.CodeOf(err) != connect.CodeUnauthenticated {
			t.Fatalf("no-token ArchiveRun: %v", err)
		}
	})
	t.Run("agent token", func(t *testing.T) {
		db := newAuthTestStore(t)
		var agentID string
		call := newUnaryProbe(t, db, procedure, false, func(ctx context.Context) { agentID = CallerCriteriaID(ctx) })
		if err := call("agent-token-1"); err != nil {
			t.Fatalf("agent ArchiveRun: %v", err)
		}
		if agentID != "agent-1" {
			t.Fatalf("caller id = %q want agent-1 (operator provenance stamp source)", agentID)
		}
	})
	t.Run("orchestrator token", func(t *testing.T) {
		db := newAuthTestStore(t)
		var orchID string
		call := newUnaryProbe(t, db, procedure, false, func(ctx context.Context) { orchID = CallerOrchestratorID(ctx) })
		if err := call("orchestrator-token-1"); err != nil {
			t.Fatalf("orchestrator ArchiveRun: %v", err)
		}
		if orchID != "orchestrator-operator" {
			t.Fatalf("caller id = %q want orchestrator-operator (archive provenance stamp source)", orchID)
		}
	})
	t.Run("orchestrator-role operator token", func(t *testing.T) {
		// The KB-330 attribute-model lane in the bug report: an operator
		// token whose role attribute is orchestrator must reach ArchiveRun
		// through the same isOrchestratorAllowed gate.
		db := newAuthTestStore(t)
		token := seedOperatorToken(t, db, "op-orch", store.OperatorRoleOrchestrator)
		call := newUnaryProbe(t, db, procedure, false, nil)
		if err := call(token); err != nil {
			t.Fatalf("orchestrator-role operator ArchiveRun: %v", err)
		}
	})
	t.Run("console token", func(t *testing.T) {
		db := newAuthTestStore(t)
		sessTok := seedConsoleUser(t, db)
		call := newUnaryProbe(t, db, procedure, false, nil)
		err := call(sessTok)
		if connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Fatalf("console ArchiveRun: %v", err)
		}
	})
	t.Run("trigger token", func(t *testing.T) {
		db := newAuthTestStore(t)
		token := seedOperatorToken(t, db, "op-trg", store.OperatorRoleTrigger)
		call := newUnaryProbe(t, db, procedure, false, nil)
		err := call(token)
		if connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Fatalf("trigger ArchiveRun: %v", err)
		}
	})
	t.Run("no token with anon reads", func(t *testing.T) {
		db := newAuthTestStore(t)
		call := newUnaryProbe(t, db, procedure, true, nil)
		err := call("")
		if connect.CodeOf(err) != connect.CodeUnauthenticated {
			t.Fatalf("no-token ArchiveRun under allowAnonReads: %v", err)
		}
	})
}

func TestArchiveReadsAuthenticatedIdentities(t *testing.T) {
	for _, procedure := range []string{
		castlev1connect.RunArchiveServiceGetRunArchiveProcedure,
		castlev1connect.RunArchiveServiceListRunArchivesProcedure,
	} {
		procedure := procedure
		t.Run(procedure, func(t *testing.T) {
			t.Run("agent", func(t *testing.T) {
				db := newAuthTestStore(t)
				call := newUnaryProbe(t, db, procedure, false, nil)
				if err := call("agent-token-1"); err != nil {
					t.Fatalf("agent read: %v", err)
				}
			})
			t.Run("orchestrator", func(t *testing.T) {
				db := newAuthTestStore(t)
				call := newUnaryProbe(t, db, procedure, false, nil)
				if err := call("orchestrator-token-1"); err != nil {
					t.Fatalf("orchestrator read: %v", err)
				}
			})
			t.Run("console", func(t *testing.T) {
				db := newAuthTestStore(t)
				sessTok := seedConsoleUser(t, db)
				call := newUnaryProbe(t, db, procedure, false, nil)
				if err := call(sessTok); err != nil {
					t.Fatalf("console read: %v", err)
				}
			})
			t.Run("no token", func(t *testing.T) {
				db := newAuthTestStore(t)
				call := newUnaryProbe(t, db, procedure, false, nil)
				if connect.CodeOf(call("")) != connect.CodeUnauthenticated {
					t.Fatal("no-token read must not pass without allowAnonReads")
				}
			})
			t.Run("no token with anon reads", func(t *testing.T) {
				db := newAuthTestStore(t)
				call := newUnaryProbe(t, db, procedure, true, nil)
				if err := call(""); err != nil {
					t.Fatalf("anon read: %v", err)
				}
			})
		})
	}
}
