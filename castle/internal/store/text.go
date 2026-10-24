package store

// FirstNonEmpty returns a when it is non-empty, b otherwise.
func FirstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
