package pg

// InstallExpireLeasesHook swaps the ExpireWorkflowAssignmentLeases
// out-of-band-writer seam and returns its restore function. Exported shim
// (export_test.go idiom) so the external pg_test package can reach the
// unexported seam — pgtest imports this package, so the external test
// package is where the server-backed regression test lives.
func InstallExpireLeasesHook(hook func()) (restore func()) {
	prev := expireLeasesHook
	expireLeasesHook = hook
	return func() { expireLeasesHook = prev }
}
