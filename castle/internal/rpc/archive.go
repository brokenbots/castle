package rpc

import (
	"context"
	"errors"
	"fmt"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	castlev1 "github.com/brokenbots/castle/castle/gen/castle/v1"
	castlev1connect "github.com/brokenbots/castle/castle/gen/castle/v1/castlev1connect"
	"github.com/brokenbots/castle/castle/internal/auth"
	"github.com/brokenbots/castle/castle/internal/store"
	"github.com/brokenbots/castle/castle/internal/store/sqlite"
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

// ArchiveRun stores one terminal run's pre-retention archive record (KB-308).
// Idempotent: archiving the same run_id again overwrites the record (latest
// wins), so the operator's retry-with-backoff leg can never duplicate
// history. The response's archive_id is the handle the operator stamps into
// the CR status as log-provable proof the archive landed before finalizer
// removal.
func (s *ServerServer) ArchiveRun(ctx context.Context, req *connect.Request[castlev1.ArchiveRunRequest]) (*connect.Response[castlev1.ArchiveRunResponse], error) {
	r := req.Msg.GetRecord()
	if r == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("record required"))
	}
	if r.GetRunId() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("record.run_id required"))
	}
	body, err := archiveRecordJSON(r)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	archiveID := store.ArchiveID(r.GetRunId())
	rec := &store.RunArchiveRecord{
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
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if s.Log != nil {
		// Log-provable gate (workstream ACCEPTANCE): one line per landed
		// archive, carrying the ids the operator stamps. Record content is
		// never logged (it can carry ticket titles and repo URLs an operator
		// did not clear for castle logs).
		s.Log.Info("run archive stored",
			"run_id", rec.RunID,
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

// GetRunArchive returns one stored archive record by run id — the round-trip
// check #309's retention spot-check reads ("for N deleted CRs, castle
// archive records exist and round-trip").
func (s *ServerServer) GetRunArchive(ctx context.Context, req *connect.Request[castlev1.GetRunArchiveRequest]) (*connect.Response[castlev1.GetRunArchiveResponse], error) {
	if req.Msg.GetRunId() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("run_id required"))
	}
	rec, err := s.Store.GetRunArchiveRecord(ctx, req.Msg.GetRunId())
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("no archive record for this run id"))
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

// ListRunArchives pages stored archive records in ascending run-id order so
// the backfill coverage check can be walked deterministically.
func (s *ServerServer) ListRunArchives(ctx context.Context, req *connect.Request[castlev1.ListRunArchivesRequest]) (*connect.Response[castlev1.ListRunArchivesResponse], error) {
	limit := int(req.Msg.GetLimit())
	if limit <= 0 {
		limit = sqlite.ListRunArchivesDefaultLimit
	}
	all, next, err := s.Store.ListRunArchives(ctx, req.Msg.GetRunIdPrefix(), "", limit, req.Msg.GetCursor())
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
			ArchiveId:  store.ArchiveID(rec.RunID),
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
