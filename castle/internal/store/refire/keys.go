package refire

import (
	"strconv"
	"strings"
)

// NextAttemptKey derives the successor attempt's idempotency key (KB-321):
// the current key with any prior #attempt-N marker stripped (last marker
// only), then the next attempt number appended. The idempotency key includes
// the attempt count, so the refire pass is idempotent — a duplicate pass
// resolves to the same existing row instead of minting a twice-refired run.
//
// A key whose last #attempt- marker is not followed by digits (and any key
// without the marker) is left whole before appending.
func NextAttemptKey(baseKey string, currentAttempt int) string {
	return TrimAttemptSuffix(baseKey) + AttemptSuffixSeparator + strconv.Itoa(currentAttempt+1)
}

// TrimAttemptSuffix strips a trailing #attempt-N marker (N numeric) from an
// idempotency key: "key#attempt-3" -> "key". Keys without a numeric suffix on
// their last #attempt- marker are returned unchanged, so operator-chosen keys
// that merely contain the separator text keep their spelling.
func TrimAttemptSuffix(key string) string {
	i := strings.LastIndex(key, AttemptSuffixSeparator)
	if i < 0 {
		return key
	}
	digits := key[i+len(AttemptSuffixSeparator):]
	if digits == "" || !allDigits(digits) {
		return key
	}
	return key[:i]
}

func allDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
