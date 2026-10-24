package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// projectIDPattern constrains project IDs to lowercase slugs so ids stay
// safe to reference in UIs, tickets, and wire surfaces.
var projectIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-_.]{0,127}$`)

// ErrInvalidProjectID is returned when a project ID is not a slug.
var ErrInvalidProjectID = errors.New("invalid project id")

// ErrInvalidProjectPolicy is returned when a membership policy name is not
// one of the known policy values.
var ErrInvalidProjectPolicy = errors.New("invalid membership policy")

// ErrInvalidProjectMember is returned when a member row is malformed
// (empty member id or an unknown member kind).
var ErrInvalidProjectMember = errors.New("invalid project member")

// ValidateProjectID enforces the project slug shape.
func ValidateProjectID(id string) error {
	if !projectIDPattern.MatchString(id) {
		return fmt.Errorf("%w: %q", ErrInvalidProjectID, id)
	}
	return nil
}

// IsUniqueViolation reports whether err is a uniqueness-constraint failure.
// The store package must stay driver-agnostic, so detection matches the two
// dialect error texts the engines produce instead of importing their driver
// error types.
func IsUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") ||
		strings.Contains(msg, "duplicate key value violates unique constraint")
}

// MarshalQueues renders a project's queue list as text; an empty list stores
// as empty text (the engines treat empty text as "no queues").
func MarshalQueues(q []string) (string, error) {
	if len(q) == 0 {
		return "", nil
	}
	b, err := json.Marshal(q)
	if err != nil {
		return "", fmt.Errorf("marshal queues: %w", err)
	}
	return string(b), nil
}

// UnmarshalQueues parses the stored queues text back into a slice; empty or
// undecodable text yields nil.
func UnmarshalQueues(s string) []string {
	if s == "" {
		return nil
	}
	var q []string
	if err := json.Unmarshal([]byte(s), &q); err != nil {
		return nil
	}
	return q
}