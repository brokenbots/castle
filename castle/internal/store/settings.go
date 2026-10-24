package store

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
)

// ErrSettingsConflict is returned by SetProjectSettings when the caller's
// expected etag does not match the project's current settings doc (KB-324
// S1): the optimistic etag swap only accepts writes against the doc the
// writer has seen. The rpc layer maps this to FailedPrecondition.
var ErrSettingsConflict = errors.New("project settings were modified concurrently")

// SettingsETag is the content-addressed handle over a settings doc (KB-324
// S1): sha256 over the canonical JSON of the entry map with keys in sorted
// order, truncated to 32 hex chars. An absent or empty doc yields the empty
// etag so the very first swap may pass expected = "".
func SettingsETag(settings map[string]string) string {
	if len(settings) == 0 {
		return ""
	}
	keys := make([]string, 0, len(settings))
	for k := range settings {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var canonical bytes.Buffer
	// map[string]string cannot fail json.Marshal (see MarshalJSONMap).
	canonical.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			canonical.WriteByte(',')
		}
		keyJSON, _ := json.Marshal(k)
		valueJSON, _ := json.Marshal(settings[k])
		canonical.Write(keyJSON)
		canonical.WriteByte(':')
		canonical.Write(valueJSON)
	}
	canonical.WriteByte('}')
	sum := sha256.Sum256(canonical.Bytes())
	return hex.EncodeToString(sum[:])[:32]
}
