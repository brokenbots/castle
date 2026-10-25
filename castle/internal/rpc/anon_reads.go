package rpc

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"

	"connectrpc.com/connect"

	"github.com/brokenbots/castle/castle/internal/store"
)

// allowAnonReadsProjectSetting is the ONE convention key in the project
// settings doc that castle interprets (KB-326 S3): an owning agent's
// projects may grant anonymous reads of that agent's legacy unscoped runs
// even with the dev flag off. Every other settings key stays opaque.
// The grant check requires the exact value "true" (JSON boolean); anything
// else — including a differently-spelled truthy string — denies
// defensively.
const allowAnonReadsProjectSetting = "allow_anonymous_reads"

// anonymousRunReadAllowed reports whether an anonymous caller (marked by the
// auth interceptor) may read the run (KB-326 S3):
//
//   - A project-scoped run is NEVER anonymously readable. The global dev
//     flag and per-project grants both stop at scoped runs; admission for
//     these is the project membership machinery (KB-324).
//   - A legacy unscoped run is anonymously readable when the global dev flag
//     is on, or when ANY project that the run's owning agent is a member of
//     carries the allow_anonymous_reads grant (union semantics; any single
//     grant suffices), or when the owning agent belongs to no project and
//     no grant can therefore exist. Anonymous callers present no token, so
//     membership lookups key on the run's owning agent — callers cannot
//     choose which agent's grants they inherit.
//
// The check reads the owning agent's CURRENT membership and the CURRENT
// settings docs, so a revocation or grant flip is effective immediately —
// there is no grant cache.
func (s *ServerServer) anonymousRunReadAllowed(ctx context.Context, run *store.Run) (bool, error) {
	if run.ProjectID != "" {
		return false, nil
	}
	if s.AllowAnonymousReads {
		return true, nil
	}
	projectIDs, err := s.Store.ListMemberProjects(ctx, run.OverseerID)
	if err != nil {
		return false, err
	}
	for _, projectID := range projectIDs {
		settings, _, err := s.Store.GetProjectSettings(ctx, projectID)
		if errors.Is(err, store.ErrNotFound) {
			// The project vanished between membership lookup and settings
			// read; a vanished project can no longer grant anything.
			continue
		}
		if err != nil {
			return false, err
		}
		if settings[allowAnonReadsProjectSetting] == "true" {
			return true, nil
		}
	}
	return false, nil
}

// anonymousRunReadRefusal answers an anonymous caller whose grant check
// failed. NotFound — uniform with a missing run — keeps denied runs from
// leaking their existence through the error class.
func (s *ServerServer) anonymousRunReadRefusal(ctx context.Context, run *store.Run) error {
	allowed, err := s.anonymousRunReadAllowed(ctx, run)
	if err != nil {
		return connect.NewError(connect.CodeInternal, err)
	}
	if !allowed {
		return connect.NewError(connect.CodeNotFound, errors.New("run not found"))
	}
	return nil
}

// validateProjectSettingValue enforces the interpreted-key contract of the
// settings doc (KB-326 S3): values of allow_anonymous_reads must be a JSON
// boolean (true / false). Castle persists every settings key verbatim, but
// this ONE key is interpreted at grant time, so a differently-typed value
// is rejected at write time instead of silently granting (or denying)
// nothing. name is the doc name for error messages ("settings"); name/key
// spelling mirrors the existing shape error text.
func validateProjectSettingValue(name, key, value string) error {
	if key != allowAnonReadsProjectSetting || value == "" {
		return nil
	}
	var decoded any
	if err := json.Unmarshal([]byte(value), &decoded); err != nil {
		return connect.NewError(connect.CodeInvalidArgument,
			errors.New(name+"["+strconv.Quote(key)+"] must be valid JSON"))
	}
	if _, ok := decoded.(bool); !ok {
		return connect.NewError(connect.CodeInvalidArgument,
			errors.New(name+"["+strconv.Quote(key)+"] must be the JSON boolean true or false"))
	}
	return nil
}
