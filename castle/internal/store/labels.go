package store

import (
	"encoding/json"
	"fmt"
)

// MarshalLabels renders an assignment's label map as text. Labels are stored
// as a JSON object string on every engine; an empty map stores as empty text
// (the engines treat empty text as "no labels").
func MarshalLabels(m map[string]string) (string, error) {
	if len(m) == 0 {
		return "", nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "", fmt.Errorf("marshal labels: %w", err)
	}
	return string(b), nil
}

// UnmarshalLabels parses the stored labels text back into a map; empty or
// undecodable text yields nil.
func UnmarshalLabels(s string) map[string]string {
	if s == "" {
		return nil
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return nil
	}
	return m
}

// MarshalJSONMap shares the labels serialization convention with other
// opaque map[string]string columns (settings, info): an empty map stores as
// empty text, never as "{}" or "null".
func MarshalJSONMap(m map[string]string) string {
	if len(m) == 0 {
		return ""
	}
	b, err := json.Marshal(m)
	if err != nil {
		// map[string]string cannot fail json.Marshal in practice; fall back
		// to empty so an impossible error never surfaces as a corrupt value.
		return ""
	}
	return string(b)
}

// UnmarshalJSONMap parses the stored text back into a map; empty or
// undecodable text yields nil.
func UnmarshalJSONMap(s string) map[string]string {
	return UnmarshalLabels(s)
}
