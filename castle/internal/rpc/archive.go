package rpc

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	castlev1 "github.com/brokenbots/castle/castle/gen/castle/v1"
	castlev1connect "github.com/brokenbots/castle/castle/gen/castle/v1/castlev1connect"
	"github.com/brokenbots/castle/castle/internal/auth"
	"github.com/brokenbots/castle/castle/internal/store"
)

// archiveRecordJSON marshals the received archive record so the stored body
// is exactly what the operator sent (the durable, round-trippable evidence
// of the pre-retention archive leg), rather than a castle-side re-derivation.
func archiveRecordJSON(rec *castlev1.ArchiveRecord) (string, error) {
	if rec == nil {
		return "", errors.New("record required")
	}
	b, err := protojson.Marshal(rec)
	if err != nil {
		return "", fmt.Errorf("marshal archive record: %w", err)
	}
	return string(b), nil
}

// archiveCallerID resolves the authenticated caller identity for the archive
// provenance stamp (KB-308 scope item 2: the operator archives with its
// pre-existing token — the podstate feed's token path). Agent identities and
// orchestrator identities both carry provenance; an interceptor-less direct
// call (tests) stays empty rather than inventing anonymity. An unauthenticated
// HTTP caller never reaches the handler: the interceptor rejects it first.
func archiveCallerID(ctx context.Context) string {
	if id := auth.CallerCriteriaID(ctx); id != "" {
		return "criteria/" + id
	}
	if id := auth.CallerOrchestratorID(ctx); id != "" {
		return "orchestrator/" + id
	}
	return ""
}

// syntheticArchiveKeyPrefix is the documented namespace for archive keys
// that name no castle run (terminal CRs archived before the castle overseer
// run registered): "cr-uid/<CR metadata.uid>", derived from the CR identity
// the operator owns. Restricting agent-minted synthetic keys to it keeps
// bare run ids from becoming squat-able archive keys.
const syntheticArchiveKeyPrefix = "cr-uid/"

// ArchiveRun stores one terminal run's pre-retention archive record (KB-308).
// Idempotent: archiving the same archive key again overwrites the record
// (latest wins), so the operator's retry-with-backoff leg can never
// duplicate history. The response's archive_id is the handle the operator
// stamps into the CR status as log-provable proof the archive landed before
// finalizer removal.
//
// Authorization (KB-308 scope item 2), for agent identities: ownership
// resolves against the EFFECTIVE key — archive_key when set, else run_id —
// because that is the key the store row hangs off. A key naming an existing
// castle run must be owned by the caller (unknown: not_found; another
// identity's run: permission_denied), so history cannot be attributed to a
// run the caller never owned. run_id and archive_key must agree when both
// are set (invalid_argument otherwise) — the run-backed form of that rule
// is unconditional: a record keyed on an existing castle run must set
// run_id to that run id, so no caller can key after a run it owns while
// claiming another identity's run (or a ghost id) in the body. A run-id-less
// record must keep the documented CR-derived "cr-uid/" namespace, so run
// ids cannot be minted as squat-able keys. Orchestrator and console
// identities keep today's behavior: their records are run-id-less
// synthetic-keyed and protected by the store's first-writer-wins
// provenance gate.
func (s *ServerServer) ArchiveRun(ctx context.Context, req *connect.Request[castlev1.ArchiveRunRequest]) (*connect.Response[castlev1.ArchiveRunResponse], error) {
	r := req.Msg.GetRecord()
	if r == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("record required"))
	}
	// Effective archive key (KB-308 archive_key contract): the explicit
	// CR-derived archive_key for run-id-less records, else the castle
	// overseer run id. Store rows and the operator-stamped archive_id hang
	// off this key, so a terminal CR is never unarchivable.
	key := r.GetArchiveKey()
	if key == "" {
		key = r.GetRunId()
	}
	if key == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("record.run_id or record.archive_key required"))
	}
	if callerID := auth.CallerCriteriaID(ctx); callerID != "" {
		// Gate on the effective key, never on run_id alone: keying is what
		// the store row and provenance stamp follow, so an ownership check
		// on run_id would let a caller validate its own run while storing
		// under another identity's key (forged history), or skip the gate
		// entirely with run_id unset (a key squat that permanently denies
		// the legitimate owner — the archive leg retries rather than skips,
		// so a squatted key is unarchivable run history before #309
		// retention deletes the CR).
		run, err := s.Store.GetRun(ctx, key)
		if err == nil {
			// Run-backed effective key: the caller must own THAT run, and
			// the record must claim it too — a record keyed on an existing
			// castle run whose run_id names a different run (or a ghost id,
			// or nothing) would let a caller key after its own run while
			// forging run-attributed history for a run it never owned.
			// This also makes run_id and archive_key agree whenever both
			// are set: keying follows archive_key here, so a disagreement
			// can only appear as run_id != key.
			if callerID != run.OverseerID {
				return nil, connect.NewError(connect.CodePermissionDenied, errors.New("caller does not own this run"))
			}
			if ki := r.GetRunId(); ki != key {
				return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("record keyed on castle run %q must set record.run_id to that run id", key))
			}
		} else if errors.Is(err, store.ErrNotFound) {
			// Key names no castle run. run_id must agree with archive_key
			// when both are set; a run-backed run_id that does not resolve
			// keeps the run-backed error semantics (not_found); a pure
			// archive_key record must stay in the CR-derived namespace.
			if ka, ki := r.GetArchiveKey(), r.GetRunId(); ka != "" && ki != "" && ka != ki {
				return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("record.run_id and record.archive_key disagree; a record with both set must key exactly on its run id"))
			}
			if r.GetRunId() != "" {
				if _, _, err := requireCallerOwnsRun(ctx, s.Store, r.GetRunId()); err != nil {
					return nil, err
				}
			} else if !strings.HasPrefix(key, syntheticArchiveKeyPrefix) {
				return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("record.archive_key for a record without a castle run must start with %q", syntheticArchiveKeyPrefix))
			}
		} else {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
	}
	body, err := archiveRecordJSON(r)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	rec := &store.RunArchiveRecord{
		Key:        key,
		RunID:      r.GetRunId(),
		JobName:    r.GetJobName(),
		FinalPhase: r.GetFinalPhase(),
		Ticket:     r.GetTicket(),
		Record:     body,
		OperatorID: archiveCallerID(ctx),
		ArchivedAt: time.Now().UTC(),
	}
	created, err := s.Store.PutRunArchiveRecord(ctx, rec)
	if err != nil {
		if errors.Is(err, store.ErrArchiveOperatorConflict) {
			return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("archive record for this key was already archived by another identity: %w", err))
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	archiveID := store.ArchiveID(key)
	if s.Log != nil {
		// Log-provable gate (workstream ACCEPTANCE): one line per landed
		// archive, carrying the ids the operator stamps. Record content is
		// never logged (it can carry ticket titles and repo URLs an operator
		// did not clear for castle logs).
		s.Log.Info("run archive stored",
			"run_id", rec.RunID,
			"archive_key", key,
			"archive_id", archiveID,
			"operator_id", rec.OperatorID,
			"created", created,
			"final_phase", rec.FinalPhase)
	}
	return connect.NewResponse(&castlev1.ArchiveRunResponse{
		ArchiveId:  archiveID,
		ArchivedAt: timestamppb.New(rec.ArchivedAt),
		Created:    created,
	}), nil
}

// GetRunArchive returns one stored archive record by archive key — the
// round-trip check #309's retention spot-check reads ("for N deleted CRs,
// castle archive records exist and round-trip"). Run-backed records are
// keyed by run id, run-id-less records by their explicit archive_key.
func (s *ServerServer) GetRunArchive(ctx context.Context, req *connect.Request[castlev1.GetRunArchiveRequest]) (*connect.Response[castlev1.GetRunArchiveResponse], error) {
	if req.Msg.GetKey() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("key required"))
	}
	rec, err := s.Store.GetRunArchiveRecord(ctx, req.Msg.GetKey())
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("no archive record for this key"))
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	body := &structpb.Struct{}
	opts := protojson.UnmarshalOptions{DiscardUnknown: true}
	if err := opts.Unmarshal([]byte(rec.Record), body); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("decode stored archive record: %w", err))
	}
	return connect.NewResponse(&castlev1.GetRunArchiveResponse{
		RunId:      rec.RunID,
		Record:     body,
		ArchivedAt: timestamppb.New(rec.ArchivedAt),
		OperatorId: rec.OperatorID,
	}), nil
}

// ListRunArchives pages stored archive records in ascending archive-key
// order so the backfill coverage check can be walked deterministically.
func (s *ServerServer) ListRunArchives(ctx context.Context, req *connect.Request[castlev1.ListRunArchivesRequest]) (*connect.Response[castlev1.ListRunArchivesResponse], error) {
	limit := int(req.Msg.GetLimit())
	if limit <= 0 {
		limit = store.ListRunArchivesDefaultLimit
	}
	all, next, err := s.Store.ListRunArchives(ctx, req.Msg.GetKeyPrefix(), "", limit, req.Msg.GetCursor())
	if err != nil {
		if errors.Is(err, store.ErrInvalidCursor) {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	summaries := make([]*castlev1.ArchiveSummary, 0, len(all))
	for _, rec := range all {
		summaries = append(summaries, &castlev1.ArchiveSummary{
			RunId:      rec.RunID,
			ArchiveId:  store.ArchiveID(rec.Key),
			JobName:    rec.JobName,
			FinalPhase: rec.FinalPhase,
			Ticket:     rec.Ticket,
			ArchivedAt: timestamppb.New(rec.ArchivedAt),
			OperatorId: rec.OperatorID,
		})
	}
	return connect.NewResponse(&castlev1.ListRunArchivesResponse{
		Archives:   summaries,
		NextCursor: next,
	}), nil
}

// Compile-time check: the ServerServer must implement the archive handler
// surface exactly once this file wires in.
var _ castlev1connect.RunArchiveServiceHandler = (*ServerServer)(nil)
