package rpc

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"

	"github.com/brokenbots/castle/castle/internal/store"
	"github.com/brokenbots/castle/castle/internal/store/sqlite"
	criteria "github.com/brokenbots/criteria/sdk"
)

const maxListEventPagesPerRPC = 10

var errListEventPageLimitExceeded = errors.New("persisted event page limit exceeded")
var errStopEventPagination = errors.New("stop paged event iteration")

func forEachPersistedEventPage(ctx context.Context, st store.Store, runID string, since uint64, onEvent func(*criteria.Envelope) error) (uint64, error) {
	cursor := since
	for page := 0; page < maxListEventPagesPerRPC; page++ {
		events, err := st.ListEvents(ctx, runID, cursor, sqlite.ListEventsMaxLimit)
		if err != nil {
			return cursor, err
		}
		if len(events) == 0 {
			return cursor, nil
		}
		for _, ev := range events {
			env, convErr := eventToEnvelope(ev)
			if convErr != nil {
				return cursor, convErr
			}
			if err := onEvent(env); err != nil {
				if errors.Is(err, errStopEventPagination) {
					return cursor, nil
				}
				return cursor, err
			}
			cursor = ev.Seq
		}
		if len(events) < sqlite.ListEventsMaxLimit {
			return cursor, nil
		}
	}
	return cursor, fmt.Errorf("%w: run_id=%s pages=%d", errListEventPageLimitExceeded, runID, maxListEventPagesPerRPC)
}

func mapListEventsError(err error) error {
	switch {
	case errors.Is(err, store.ErrInvalidLimit):
		return connect.NewError(connect.CodeInvalidArgument, err)
	case errors.Is(err, errListEventPageLimitExceeded):
		return connect.NewError(connect.CodeResourceExhausted, err)
	default:
		return connect.NewError(connect.CodeInternal, err)
	}
}
