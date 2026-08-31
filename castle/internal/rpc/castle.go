package rpc

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/brokenbots/castle/castle/internal/store"
	"github.com/brokenbots/castle/castle/internal/store/sqlite"
	pb "github.com/brokenbots/castle/shared/pb/overlord/v1" // import-lint:allow castle service bindings (W08: move to castle-proto)
	overseer "github.com/brokenbots/castle/shared/sdk/overseer"
)

const (
	cursorFlushInterval        = 250 * time.Millisecond
	cursorFlushBatch           = 100
	cursorFinalPerWriteTimeout = 6 * time.Second
	cursorFinalMaxDuration     = 15 * time.Second
)

func (s *CastleServer) ListOverseers(ctx context.Context, _ *connect.Request[pb.ListOverseersRequest]) (*connect.Response[pb.ListOverseersResponse], error) {
	list, err := s.Store.ListOverseers(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	out := make([]*pb.Overseer, 0, len(list))
	for _, o := range list {
		out = append(out, mapOverseer(o))
	}
	return connect.NewResponse(&pb.ListOverseersResponse{Overseers: out}), nil
}

func (s *CastleServer) GetOverseer(ctx context.Context, req *connect.Request[pb.GetOverseerRequest]) (*connect.Response[pb.Overseer], error) {
	o, err := s.Store.GetOverseer(ctx, req.Msg.OverseerId)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, err)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(mapOverseer(o)), nil
}

func (s *CastleServer) ListRuns(ctx context.Context, req *connect.Request[pb.ListRunsRequest]) (*connect.Response[pb.ListRunsResponse], error) {
	list, err := s.Store.ListRuns(ctx, req.Msg.OverseerId, req.Msg.Status)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	out := make([]*pb.Run, 0, len(list))
	for _, r := range list {
		out = append(out, mapRun(r))
	}
	return connect.NewResponse(&pb.ListRunsResponse{Runs: out}), nil
}

func (s *CastleServer) GetRun(ctx context.Context, req *connect.Request[pb.GetRunRequest]) (*connect.Response[pb.Run], error) {
	r, err := s.Store.GetRun(ctx, req.Msg.RunId)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, err)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(mapRun(r)), nil
}

func (s *CastleServer) ListRunEvents(ctx context.Context, req *connect.Request[pb.ListRunEventsRequest]) (*connect.Response[pb.ListRunEventsResponse], error) {
	limit := int(req.Msg.Limit)
	if limit <= 0 {
		limit = sqlite.ListEventsDefaultLimit
	}
	if limit > sqlite.ListEventsMaxLimit {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("limit %d exceeds maximum %d; page using since_seq", limit, sqlite.ListEventsMaxLimit))
	}

	all, err := s.Store.ListEvents(ctx, req.Msg.RunId, req.Msg.SinceSeq, limit)
	if err != nil {
		return nil, mapListEventsError(err)
	}

	resp := &pb.ListRunEventsResponse{Events: all}
	if len(all) > 0 {
		resp.LastSeq = all[len(all)-1].Seq
		if len(all) == limit {
			resp.NextSinceSeq = all[len(all)-1].Seq
		}
	}
	return connect.NewResponse(resp), nil
}

func (s *CastleServer) WatchRun(ctx context.Context, req *connect.Request[pb.WatchRunRequest], stream *connect.ServerStream[pb.Envelope]) error {
	runID := req.Msg.RunId
	if runID == "" {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("run_id required"))
	}
	subscriberID := req.Msg.SubscriberId
	effectiveSince := req.Msg.SinceSeq
	if subscriberID != "" && effectiveSince == 0 {
		if cursor, found, err := s.Store.GetSubscriberCursor(ctx, subscriberID, runID); err != nil {
			s.Log.Warn("watch cursor lookup failed", "run_id", runID, "subscriber_id", subscriberID, "error", err)
		} else if found {
			effectiveSince = cursor
		}
	}

	var (
		updateCursor func(uint64)
		writer       cursorWriter
	)
	updateCursor = func(uint64) {}
	if subscriberID != "" {
		writer = s.startCursorWriter(ctx, subscriberID, runID)
		updateCursor = writer.update
	}

	// Subscribe before replay so events published while replaying persisted rows
	// are queued for the subsequent buffer/live phases.
	sub := s.Hub.Subscribe(runID)
	defer s.Hub.Unsubscribe(sub)

	lastSent := effectiveSince
	terminalInReplay := false

	if subscriberID != "" {
		defer func() {
			// Ensure the highest sequence actually delivered to the client is
			// durably flushed before the writer stops. This closes the race
			// between the server's last updateCursor call and the client close.
			if lastSent > effectiveSince {
				writer.update(lastSent)
			}
			writer.flush()
			writer.stop()
		}()
	}

	_, err := forEachPersistedEventPage(ctx, s.Store, runID, effectiveSince, func(env *pb.Envelope) error {
		if env.Seq <= lastSent {
			return nil
		}
		if err := stream.Send(env); err != nil {
			return err
		}
		lastSent = env.Seq
		updateCursor(env.Seq)
		if overseer.IsTerminal(env) {
			terminalInReplay = true
			return errStopEventPagination
		}
		return nil
	})
	if err != nil {
		return mapListEventsError(err)
	}
	if terminalInReplay {
		return nil
	}

	// WatchReady is sent once replay is complete so Connect flushes response
	// headers even when no live event has arrived yet. Clients must ignore it.
	if err := stream.Send(&pb.Envelope{SchemaVersion: 1, RunId: runID, Payload: &pb.Envelope_WatchReady{WatchReady: &pb.WatchReady{}}}); err != nil {
		return connect.NewError(connect.CodeUnknown, err)
	}

	// Drain the in-memory gap-closure tier after durable replay and before
	// tailing live fan-out.
	for _, env := range s.Hub.Since(runID, lastSent) {
		if env.Seq <= lastSent {
			continue
		}
		if err := stream.Send(env); err != nil {
			return connect.NewError(connect.CodeUnknown, err)
		}
		lastSent = env.Seq
		updateCursor(env.Seq)
		if overseer.IsTerminal(env) {
			return nil
		}
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case env, ok := <-sub.C:
			if !ok {
				return nil
			}
			if env.Seq <= lastSent {
				continue
			}
			if err := stream.Send(env); err != nil {
				return connect.NewError(connect.CodeUnknown, err)
			}
			lastSent = env.Seq
			updateCursor(env.Seq)
			if overseer.IsTerminal(env) {
				return nil
			}
		}
	}
}

type cursorWriter struct {
	update func(uint64)
	flush  func()
	stop   func()
}

// isCursorBusyError reports whether err is a transient SQLite busy error that
// should be retried by the cursor writer.
func isCursorBusyError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "SQLITE_BUSY")
}

// upsertCursorWithRetry attempts to persist seq for (subscriberID, runID). It
// retries on SQLITE_BUSY errors with bounded backoff until the write succeeds,
// a non-retryable error occurs, the supplied context is canceled, or the retry
// budget is exhausted.
func upsertCursorWithRetry(
	ctx context.Context,
	st store.Store,
	subscriberID, runID string,
	seq uint64,
	perWriteTimeout, maxDuration time.Duration,
) error {
	deadline := time.Now().Add(maxDuration)
	delay := 5 * time.Millisecond
	var lastErr error
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Now().After(deadline) {
			break
		}

		// Bound the per-attempt timeout by the remaining overall budget so the
		// final attempt does not overshoot maxDuration.
		timeout := perWriteTimeout
		if remaining := time.Until(deadline); remaining < timeout {
			timeout = remaining
		}
		if timeout <= 0 {
			break
		}

		writeCtx, cancel := context.WithTimeout(context.Background(), timeout)
		lastErr = st.UpsertSubscriberCursor(writeCtx, subscriberID, runID, seq)
		cancel()
		if lastErr == nil {
			return nil
		}
		if !isCursorBusyError(lastErr) {
			return lastErr
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		delay *= 2
		if delay > 50*time.Millisecond {
			delay = 50 * time.Millisecond
		}
	}
	return lastErr
}

func (s *CastleServer) startCursorWriter(ctx context.Context, subscriberID, runID string) cursorWriter {
	var (
		mu           sync.Mutex
		hasPending   bool
		pendingSeq   uint64
		persistedSeq uint64
		pendingCount int
		once         sync.Once
	)

	// flushReq carries an optional completion channel. A nil channel is a
	// best-effort wake-up (used by batch-triggered updates); a non-nil channel
	// is closed after the requested flush attempt finishes.
	flushReq := make(chan chan struct{}, 1)
	stopCh := make(chan struct{})
	done := make(chan struct{})

	const (
		normalPerWriteTimeout = 2 * time.Second
		normalMaxDuration     = 2 * time.Second
	)

	// flush performs a single cursor write attempt with bounded retry. On
	// success it advances the persisted sequence and only clears pending state
	// when no higher sequence has arrived in the meantime. On a non-retryable
	// error it clears pending to avoid endless failure. On a retryable
	// SQLITE_BUSY error it leaves the pending sequence untouched so a later
	// flush can retry the same (or higher) value.
	flush := func(final bool, deadline time.Time) bool {
		mu.Lock()
		if !hasPending {
			mu.Unlock()
			return true
		}
		seq := pendingSeq
		mu.Unlock()

		maxDuration := normalMaxDuration
		perWrite := normalPerWriteTimeout
		if final {
			perWrite = cursorFinalPerWriteTimeout
			remaining := time.Until(deadline)
			if remaining <= 0 {
				s.Log.Warn("watch cursor final flush exhausted retry budget",
					"run_id", runID, "subscriber_id", subscriberID, "seq", seq)
				return false
			}
			maxDuration = remaining
		}

		// Cursor writes must outlive the request context: WatchRun's client
		// stream may be canceled before the final cursor flush completes, and
		// we still need to durably persist the highest delivered sequence.
		err := upsertCursorWithRetry(context.Background(), s.Store, subscriberID, runID, seq, perWrite, maxDuration)
		if err != nil {
			s.Log.Warn("watch cursor persist failed", "run_id", runID, "subscriber_id", subscriberID, "seq", seq, "error", err)
			if !isCursorBusyError(err) {
				mu.Lock()
				hasPending = false
				pendingCount = 0
				mu.Unlock()
			}
			return false
		}

		mu.Lock()
		persistedSeq = seq
		pendingCount = 0
		if pendingSeq <= persistedSeq {
			hasPending = false
		}
		mu.Unlock()
		return true
	}

	go func() {
		defer close(done)
		ticker := time.NewTicker(cursorFlushInterval)
		defer ticker.Stop()
		for {
			select {
			case doneCh := <-flushReq:
				flush(false, time.Time{})
				if doneCh != nil {
					close(doneCh)
				}
			case <-ticker.C:
				flush(false, time.Time{})
			case <-stopCh:
				finalDeadline := time.Now().Add(cursorFinalMaxDuration)
				for {
					flushed := flush(true, finalDeadline)
					mu.Lock()
					noPending := !hasPending
					mu.Unlock()
					if noPending || !flushed {
						return
					}
				}
			}
		}
	}()

	return cursorWriter{
		update: func(seq uint64) {
			if seq == 0 {
				return
			}
			mu.Lock()
			if seq <= persistedSeq {
				// Already durably persisted at or above seq.
				mu.Unlock()
				return
			}
			if seq > pendingSeq {
				pendingSeq = seq
			}
			if !hasPending {
				hasPending = true
				pendingCount = 0
			}
			pendingCount++
			shouldFlush := pendingCount >= cursorFlushBatch
			mu.Unlock()
			if shouldFlush {
				select {
				case flushReq <- nil:
				default:
				}
			}
		},
		flush: func() {
			doneCh := make(chan struct{})
			select {
			case flushReq <- doneCh:
				<-doneCh
			default:
				// A flush is already queued; wait for the next flush cycle to
				// complete by sending a fresh completion channel once there is
				// room.
				ticker := time.NewTicker(5 * time.Millisecond)
				defer ticker.Stop()
				for {
					select {
					case flushReq <- doneCh:
						<-doneCh
						return
					case <-ticker.C:
					}
				}
			}
		},
		stop: func() {
			once.Do(func() {
				close(stopCh)
			})
			<-done
		},
	}
}

func (s *CastleServer) StopRun(ctx context.Context, req *connect.Request[pb.StopRunRequest]) (*connect.Response[pb.StopRunResponse], error) {
	_, run, err := requireCallerOwnsRun(ctx, s.Store, req.Msg.RunId)
	if err != nil {
		return nil, err
	}
	reason := req.Msg.Reason
	if reason == "" {
		reason = "requested by operator"
	}
	err = s.controls.Enqueue(run.OverseerID, &pb.ControlMessage{Command: &pb.ControlMessage_RunCancel{RunCancel: &pb.RunCancel{RunId: run.ID, Reason: reason}}})
	switch {
	case errors.Is(err, ErrOverseerNotConnected):
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	case errors.Is(err, ErrControlBacklogFull):
		s.Log.Warn("control backlog full; stop run dropped", "overseer_id", run.OverseerID, "run_id", run.ID)
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	case err != nil:
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	now := time.Now().UTC()
	return connect.NewResponse(&pb.StopRunResponse{IssuedAt: timestamppb.New(now)}), nil
}

func (s *CastleServer) SendPrompt(context.Context, *connect.Request[pb.SendPromptRequest]) (*connect.Response[pb.SendPromptResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, errors.New("send prompt is not implemented"))
}
