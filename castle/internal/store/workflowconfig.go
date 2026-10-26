package store

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"

	"github.com/brokenbots/castle/castle/internal/confighome"
)

// ErrProjectConfigConflict is returned by SetProjectConfig when the caller's
// expected etag does not match the project's current workflow config record
// (KB-339): the optimistic etag swap only accepts writes against the record
// the writer has seen. The rpc layer maps this to FailedPrecondition, same
// as the project settings swap.
var ErrProjectConfigConflict = errors.New("project workflow config was modified concurrently")

// ErrInvalidWorkflowConfig is returned by SetProjectConfig when the doc is
// not JSON the store can canonicalize (KB-339). The rpc layer validates the
// record's shape before this ever sees the doc; this protects the store's
// canonical storage form.
var ErrInvalidWorkflowConfig = errors.New("project workflow config doc is not valid JSON")

// CanonicalWorkflowConfigDoc renders the store's storage form of a workflow
// config record doc: the canonical JSON every byte-wise variant of the same
// record collapses to. Callers hand it valid record JSON; a malformed doc
// fails with ErrInvalidWorkflowConfig.
func CanonicalWorkflowConfigDoc(doc []byte) ([]byte, error) {
	canon, err := confighome.Canonicalize(doc)
	if err != nil {
		return nil, ErrInvalidWorkflowConfig
	}
	return canon, nil
}

// ProjectConfigETag is the content-addressed handle over a project's
// workflow config record doc (KB-339): sha256 over the canonical form of the
// doc (confighome.Canonicalize, so two byte-wise variants of the same record
// compare equal), truncated to 32 hex chars. An absent or empty doc yields
// the empty etag so the very first swap may pass expected = "".
func ProjectConfigETag(doc []byte) string {
	if len(bytes.TrimSpace(doc)) == 0 {
		return ""
	}
	canon, err := confighome.Canonicalize(doc)
	if err != nil {
		// The input is not JSON; hash the raw bytes so a doc that no
		// canonical form exists for still gets a stable handle. Writers
		// never reach this (the rpc layer parses the record first).
		sum := sha256.Sum256(doc)
		return hex.EncodeToString(sum[:])[:32]
	}
	sum := sha256.Sum256(canon)
	return hex.EncodeToString(sum[:])[:32]
}
