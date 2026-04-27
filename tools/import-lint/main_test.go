package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// tempRepoWith creates a temporary directory tree from a map of
// repo-relative path → file content.
func tempRepoWith(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, src := range files {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

const castleImportsEvents = `package foo
import _ "github.com/brokenbots/overlord/shared/events"
`

const castleImportsPb = `package foo
import _ "github.com/brokenbots/overlord/shared/pb/overlord/v1"
`

const castleImportsSDK = `package foo
import _ "github.com/brokenbots/overlord/shared/sdk/overseer"
`

const overseerImportsCastle = `package foo
import _ "github.com/brokenbots/overlord/castle/internal/store"
`

// sdkImportsCastleInternal uses an internal castle path to verify the rule
// catches both cmd/ and internal/ sub-paths.
const sdkImportsCastleInternal = `package foo
import _ "github.com/brokenbots/overlord/castle/internal/hub"
`

func TestCastleImportsEvents_Forbidden(t *testing.T) {
	root := tempRepoWith(t, map[string]string{
		"castle/internal/foo/foo.go": castleImportsEvents,
	})
	vs, err := lint(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != 1 {
		t.Fatalf("expected 1 violation, got %d: %+v", len(vs), vs)
	}
	if !strings.Contains(vs[0].message, "shared/events") {
		t.Errorf("unexpected message: %s", vs[0].message)
	}
}

func TestCastleImportsPb_Forbidden(t *testing.T) {
	root := tempRepoWith(t, map[string]string{
		"castle/internal/foo/foo.go": castleImportsPb,
	})
	vs, err := lint(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != 1 {
		t.Fatalf("expected 1 violation, got %d: %+v", len(vs), vs)
	}
	if !strings.Contains(vs[0].message, "shared/pb") {
		t.Errorf("unexpected message: %s", vs[0].message)
	}
}

func TestCastleImportsSDK_Clean(t *testing.T) {
	root := tempRepoWith(t, map[string]string{
		"castle/internal/foo/foo.go": castleImportsSDK,
	})
	vs, err := lint(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != 0 {
		t.Fatalf("expected no violations, got %d: %+v", len(vs), vs)
	}
}

func TestOverseerImportsCastle_Forbidden(t *testing.T) {
	root := tempRepoWith(t, map[string]string{
		"overseer/internal/foo/foo.go": overseerImportsCastle,
	})
	vs, err := lint(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != 1 {
		t.Fatalf("expected 1 violation, got %d: %+v", len(vs), vs)
	}
}

func TestSDKImportsCastle_Forbidden(t *testing.T) {
	root := tempRepoWith(t, map[string]string{
		"shared/sdk/overseer/foo.go": sdkImportsCastleInternal,
	})
	vs, err := lint(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != 1 {
		t.Fatalf("expected 1 violation, got %d: %+v", len(vs), vs)
	}
}

func TestCastleImportsOverlordv1connect_Flagged(t *testing.T) {
	// overlordv1connect is under shared/pb/, so it is flagged like any
	// other shared/pb sub-package. Production files suppress it with
	// import-lint:allow when needed for castle service bindings.
	root := tempRepoWith(t, map[string]string{
		"castle/internal/rpc/foo.go": `package rpc
import _ "github.com/brokenbots/overlord/shared/pb/overlord/v1/overlordv1connect"
`,
	})
	vs, err := lint(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != 1 {
		t.Fatalf("expected 1 violation for overlordv1connect, got %d: %+v", len(vs), vs)
	}
}

func TestMultipleViolations(t *testing.T) {
	root := tempRepoWith(t, map[string]string{
		"castle/a/a.go":   castleImportsEvents,
		"castle/b/b.go":   castleImportsPb,
		"overseer/c/c.go": overseerImportsCastle,
	})
	vs, err := lint(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != 3 {
		t.Fatalf("expected 3 violations, got %d: %+v", len(vs), vs)
	}
}

func TestAllowDirective_Suppresses(t *testing.T) {
	root := tempRepoWith(t, map[string]string{
		"castle/internal/rpc/foo.go": `package rpc
import _ "github.com/brokenbots/overlord/shared/pb/overlord/v1" // import-lint:allow castle service types (W08)
`,
	})
	vs, err := lint(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != 0 {
		t.Fatalf("expected no violations with allow directive, got %d: %+v", len(vs), vs)
	}
}

func TestNonGoFilesSkipped(t *testing.T) {
	root := tempRepoWith(t, map[string]string{
		"castle/foo/foo.txt": `import "github.com/brokenbots/overlord/shared/events"`,
	})
	vs, err := lint(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != 0 {
		t.Fatalf("expected no violations for non-Go file, got %d", len(vs))
	}
}

// CLI contract tests — build the binary and exercise exit codes.

func buildBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "import-lint")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	cmd.Dir = "."
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}
	return bin
}

func TestCLI_MissingArg_Exit2(t *testing.T) {
	bin := buildBinary(t)
	cmd := exec.Command(bin)
	if err := cmd.Run(); err == nil {
		t.Fatal("expected non-zero exit for missing argument")
	}
	if code := cmd.ProcessState.ExitCode(); code != 2 {
		t.Fatalf("expected exit code 2, got %d", code)
	}
}

func TestCLI_CleanRepo_Exit0(t *testing.T) {
	bin := buildBinary(t)
	root := tempRepoWith(t, map[string]string{
		"castle/internal/foo/foo.go": castleImportsSDK,
	})
	cmd := exec.Command(bin, root)
	if err := cmd.Run(); err != nil {
		t.Fatalf("expected exit 0 for clean repo, got: %v", err)
	}
}

func TestCLI_Violations_Exit1(t *testing.T) {
	bin := buildBinary(t)
	root := tempRepoWith(t, map[string]string{
		"castle/internal/foo/foo.go": castleImportsEvents,
	})
	cmd := exec.Command(bin, root)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected non-zero exit for violations")
	}
	if code := cmd.ProcessState.ExitCode(); code != 1 {
		t.Fatalf("expected exit code 1, got %d", code)
	}
	if !strings.Contains(string(out), "shared/events") {
		t.Errorf("expected violation message in output, got: %s", out)
	}
}
