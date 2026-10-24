package store

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"
)

// TimestampLayout is the canonical text encoding of timestamps wherever an
// engine carries them as opaque strings (sqlite stores RFC3339Nano TEXT;
// keyset cursors on every engine sort-key on this same text encoding).
const TimestampLayout = time.RFC3339Nano

// runCursor is the keyset cursor for ListRuns: pages key on
// (created_at DESC, id DESC), so a page stays stable across intervening
// inserts. CreatedAt carries the sort key in TimestampLayout text form on
// every engine. Timestamp columns are native on pg and TEXT on sqlite, so
// each engine converts CreatedAt to its native comparison domain itself.
type runCursor struct {
	CreatedAt string `json:"created_at"`
	ID        string `json:"id"`
}

func EncodeRunCursor(created, id string) string {
	b, _ := json.Marshal(runCursor{CreatedAt: created, ID: id})
	return base64.RawURLEncoding.EncodeToString(b)
}

func DecodeRunCursor(token string) (created, id string, err error) {
	if token == "" {
		return "", "", nil
	}
	b, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return "", "", fmt.Errorf("%w: %s", ErrInvalidCursor, err)
	}
	var c runCursor
	if err := json.Unmarshal(b, &c); err != nil {
		return "", "", fmt.Errorf("%w: %s", ErrInvalidCursor, err)
	}
	if c.CreatedAt == "" || c.ID == "" {
		return "", "", fmt.Errorf("%w: missing sort key fields", ErrInvalidCursor)
	}
	return c.CreatedAt, c.ID, nil
}

// encodeRunCursorFromTime is the pg-shape constructor: pg scans
// created_at into a time.Time and encodes it under the same cursor text
// contract sqlite uses.
func EncodeRunCursorFromTime(created time.Time, id string) string {
	return EncodeRunCursor(created.Format(TimestampLayout), id)
}

// DecodeRunCursorCreated parses a run cursor's sort key back into a time
// (pg compares TIMESTAMPTZ columns), reporting store.ErrInvalidCursor for
// unparseable tokens.
func DecodeRunCursorCreated(created string) (time.Time, error) {
	t, err := time.Parse(TimestampLayout, created)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: created_at %q: %s", ErrInvalidCursor, created, err)
	}
	return t, nil
}

// ArchiveCursor is the keyset cursor for ListRunArchives: pages key on
// archive_key ASC (the primary key), so a page stays stable across
// concurrent archives of other keys and repeats exactly which keys it
// already served.
type archiveCursor struct {
	Key string `json:"key"`
}

func EncodeArchiveCursor(key string) string {
	b, _ := json.Marshal(archiveCursor{Key: key})
	return base64.RawURLEncoding.EncodeToString(b)
}

func DecodeArchiveCursor(token string) (key string, err error) {
	if token == "" {
		return "", nil
	}
	b, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return "", fmt.Errorf("%w: %s", ErrInvalidCursor, err)
	}
	var c archiveCursor
	if err := json.Unmarshal(b, &c); err != nil {
		return "", fmt.Errorf("%w: %s", ErrInvalidCursor, err)
	}
	if c.Key == "" {
		return "", fmt.Errorf("%w: missing key", ErrInvalidCursor)
	}
	return c.Key, nil
}

// PrefixUpperBound returns the smallest string greater than every string
// with the given prefix: the last byte incremented with big-endian carry
// ("abc" → "abd"). Only reachable impossibility (every byte 0xFF) returns
// "" and callers omit the upper bound — the whole remaining key space is
// then the prefix's suffix space, so no bound can exclude a prefixed key.
// Callers must order on the same collation for both bounds: engines store
// byte-collated key columns (sqlite BINARY by construction, pg COLLATE "C").
func PrefixUpperBound(prefix string) string {
	b := []byte(prefix)
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] == 0xFF {
			continue
		}
		b[i]++
		return string(b[:i+1])
	}
	return ""
}
