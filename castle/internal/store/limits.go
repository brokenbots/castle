package store

import "fmt"

// ListEvents page-size bounds.
const (
	ListEventsDefaultLimit = 500
	ListEventsMaxLimit     = 2048
)

// NormalizeListLimit validates a caller-supplied ListEvents page size:
// limit <= 0 falls back to the default page and a limit above the maximum
// is rejected with ErrInvalidLimit.
func NormalizeListLimit(limit int) (int, error) {
	if limit <= 0 {
		return ListEventsDefaultLimit, nil
	}
	if limit > ListEventsMaxLimit {
		return 0, fmt.Errorf("%w: limit %d exceeds maximum %d", ErrInvalidLimit, limit, ListEventsMaxLimit)
	}
	return limit, nil
}

// ListRunArchives page-size bounds.
const (
	ListRunArchivesDefaultLimit = 100
	ListRunArchivesMaxLimit     = 1000
)

// NormalizeArchiveLimit clamps a caller-supplied ListRunArchives page size:
// limit <= 0 falls back to the default page and a limit above the maximum
// is clamped to it.
func NormalizeArchiveLimit(limit int) int {
	if limit <= 0 {
		return ListRunArchivesDefaultLimit
	}
	if limit > ListRunArchivesMaxLimit {
		return ListRunArchivesMaxLimit
	}
	return limit
}
