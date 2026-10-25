package pgtest

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// newFastFetch builds the fetch helper with shrunk schedules: the same
// retry/poll/single-flight logic CI exercises, without multi-second sleeps
// in the unit suite.
func newFastFetch(t *testing.T, repoBase, cacheDir string, client *http.Client) *postgresBinaryFetch {
	t.Helper()
	return &postgresBinaryFetch{
		repoBase:        repoBase,
		cacheDir:        cacheDir,
		client:          client,
		downloadBackoff: []time.Duration{2 * time.Millisecond, 4 * time.Millisecond, 8 * time.Millisecond, 16 * time.Millisecond},
		pollEvery:       2 * time.Millisecond,
		waitBudget:      2 * time.Second,
		staleLock:       20 * time.Millisecond,
	}
}

// mkJar returns jar bytes whose single archive entry carries payload.
func mkJar(t *testing.T, payload []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("postgres-linux-x86_64.txz")
	if err != nil {
		t.Fatalf("build test jar: %v", err)
	}
	if _, err := w.Write(payload); err != nil {
		t.Fatalf("build test jar: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("build test jar: %v", err)
	}
	return buf.Bytes()
}

// cachePathFor returns the cache-file path the library's default cache
// locator reads for this machine's OS/arch mapping.
func cachePathFor(t *testing.T, cacheDir string) string {
	t.Helper()
	osName, arch := zonkyOSArch()
	_, _, cacheName := binaryArtifactPaths("", osName, arch)
	return filepath.Join(cacheDir, cacheName)
}

func newHandlerServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

func shaHandler(t *testing.T, jar []byte, hits *atomic.Int32) http.HandlerFunc {
	t.Helper()
	sum := sha256.Sum256(jar)
	digest := hex.EncodeToString(sum[:])
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, ".jar.sha256"):
			w.Write([]byte(digest))
		case strings.HasSuffix(r.URL.Path, ".jar"):
			if hits != nil {
				hits.Add(1)
			}
			w.Write(jar)
		default:
			http.NotFound(w, r)
		}
	}
}

func TestEnsurePostgresBinaryFetchesOnce(t *testing.T) {
	payload := []byte("embedded-postgres-binary-18.3.0")
	jar := mkJar(t, payload)

	var jarHits, shaHits atomic.Int32
	srv := newHandlerServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, ".jar.sha256"):
			shaHits.Add(1)
			w.Write([]byte(shaSumHex(jar)))
		case strings.HasSuffix(r.URL.Path, ".jar"):
			jarHits.Add(1)
			w.Write(jar)
		default:
			http.NotFound(w, r)
		}
	})

	cacheDir := t.TempDir()
	dl, err := newFastFetch(t, srv.URL, cacheDir, srv.Client()).ensure(context.Background())
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if !dl {
		t.Fatal("first call must download")
	}
	got, err := os.ReadFile(cachePathFor(t, cacheDir))
	if err != nil {
		t.Fatalf("cache file: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("cache payload mismatch: %q", got)
	}
	if got := jarHits.Load(); got != 1 {
		t.Fatalf("jar fetched %d times, want 1", got)
	}
	if got := shaHits.Load(); got != 1 {
		t.Fatalf("sidecar fetched %d times, want 1", got)
	}
}

func shaSumHex(jar []byte) string {
	sum := sha256.Sum256(jar)
	return hex.EncodeToString(sum[:])
}

func TestEnsurePostgresBinaryRetriesTransientFailures(t *testing.T) {
	payload := []byte("embedded-postgres-binary-18.3.0")
	jar := mkJar(t, payload)

	var jarHits atomic.Int32
	srv := newHandlerServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, ".jar.sha256"):
			w.Write([]byte(shaSumHex(jar)))
		case strings.HasSuffix(r.URL.Path, ".jar"):
			if jarHits.Add(1) <= 2 {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.Write(jar)
		default:
			http.NotFound(w, r)
		}
	})

	cacheDir := t.TempDir()
	dl, err := newFastFetch(t, srv.URL, cacheDir, srv.Client()).ensure(context.Background())
	if err != nil {
		t.Fatalf("ensure after two 500s: %v", err)
	}
	if !dl {
		t.Fatal("retry path must download")
	}
	if got := jarHits.Load(); got != 3 {
		t.Fatalf("jar hits = %d, want 3 (two 500s then one 200)", got)
	}
	info, err := os.Stat(cachePathFor(t, cacheDir))
	if err != nil || info.Size() == 0 {
		t.Fatalf("archive not at cache path after retries: %v", err)
	}
}

func TestEnsurePostgresBinaryRejectsShaMismatch(t *testing.T) {
	jar := mkJar(t, []byte("embedded-postgres-binary-18.3.0"))

	srv := newHandlerServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, ".jar.sha256"):
			w.Write([]byte("0000000000000000000000000000000000000000000000000000000000000000"))
		case strings.HasSuffix(r.URL.Path, ".jar"):
			w.Write(jar)
		default:
			http.NotFound(w, r)
		}
	})

	cacheDir := t.TempDir()
	if _, err := newFastFetch(t, srv.URL, cacheDir, srv.Client()).ensure(context.Background()); err == nil {
		t.Fatal("expected checksum mismatch error, got nil")
	}
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasSuffix(name, ".txz") || strings.HasSuffix(name, ".part") {
			t.Fatalf("no archive may land on checksum mismatch, found %s", name)
		}
	}
}

func TestEnsurePostgresBinarySkipsFetchWarmCache(t *testing.T) {
	jar := mkJar(t, []byte("embedded-postgres-binary-18.3.0"))

	var requests atomic.Int32
	srv := newHandlerServer(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		switch {
		case strings.HasSuffix(r.URL.Path, ".jar.sha256"):
			w.Write([]byte(shaSumHex(jar)))
		case strings.HasSuffix(r.URL.Path, ".jar"):
			w.Write(jar)
		default:
			http.NotFound(w, r)
		}
	})

	cacheDir := t.TempDir()
	if _, err := newFastFetch(t, srv.URL, cacheDir, srv.Client()).ensure(context.Background()); err != nil {
		t.Fatalf("cold-cache ensure: %v", err)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("cold cache made %d requests, want 2 (jar + sidecar)", got)
	}

	// Warm cache: same helper, fresh call, zero requests.
	dl, err := newFastFetch(t, srv.URL, cacheDir, srv.Client()).ensure(context.Background())
	if err != nil {
		t.Fatalf("warm-cache ensure: %v", err)
	}
	if dl {
		t.Fatal("warm cache must not report a download")
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("warm cache must not fetch: %d requests total, want 2", got)
	}
}

func TestEnsurePostgresBinarySidecarUnreachableStillSucceeds(t *testing.T) {
	jar := mkJar(t, []byte("embedded-postgres-binary-18.3.0"))

	// Sidecar 404s: the fetch must still succeed (an unreachable sidecar is
	// best-effort), while a *reachable* mismatch above still fails.
	srv := newHandlerServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, ".jar.sha256"):
			w.WriteHeader(http.StatusNotFound)
		case strings.HasSuffix(r.URL.Path, ".jar"):
			w.Write(jar)
		default:
			http.NotFound(w, r)
		}
	})

	cacheDir := t.TempDir()
	dl, err := newFastFetch(t, srv.URL, cacheDir, srv.Client()).ensure(context.Background())
	if err != nil {
		t.Fatalf("ensure with unreachable sidecar: %v", err)
	}
	if !dl {
		t.Fatal("expected a download")
	}
	if info, err := os.Stat(cachePathFor(t, cacheDir)); err != nil || info.Size() == 0 {
		t.Fatalf("archive not at cache path: %v", err)
	}
}

func TestEnsurePostgresBinaryTakesOverStaleLock(t *testing.T) {
	jar := mkJar(t, []byte("embedded-postgres-binary-18.3.0"))

	var requests atomic.Int32
	srv := newHandlerServer(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		switch {
		case strings.HasSuffix(r.URL.Path, ".jar.sha256"):
			w.Write([]byte(shaSumHex(jar)))
		case strings.HasSuffix(r.URL.Path, ".jar"):
			w.Write(jar)
		default:
			http.NotFound(w, r)
		}
	})

	cacheDir := t.TempDir()
	cacheFile := cachePathFor(t, cacheDir)
	lockFile := cacheFile + ".lock"
	if err := os.WriteFile(lockFile, []byte("pid=999999\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(lockFile, old, old); err != nil {
		t.Fatal(err)
	}

	dl, err := newFastFetch(t, srv.URL, cacheDir, srv.Client()).ensure(context.Background())
	if err != nil {
		t.Fatalf("ensure on stale lock: %v", err)
	}
	if !dl {
		t.Fatal("stale-lock takeover must download")
	}
	if requests.Load() == 0 {
		t.Fatal("takeover must fetch the jar")
	}
	if info, err := os.Stat(cacheFile); err != nil || info.Size() == 0 {
		t.Fatalf("archive not at cache path after takeover: %v", err)
	}
	if _, err := os.Stat(lockFile); !os.IsNotExist(err) {
		t.Fatalf("lock file must be released after success: %v", err)
	}
}

func TestEnsurePostgresBinaryBoundedByAnotherHolder(t *testing.T) {
	// The holder never finishes: the waiter must give up at the budget,
	// not block the run indefinitely. The server can 404 everything; the
	// lock must keep the fetcher from ever reaching it.
	srv := newHandlerServer(t, http.NotFound)

	cacheDir := t.TempDir()
	cacheFile := cachePathFor(t, cacheDir)
	lockFile := cacheFile + ".lock"
	if err := os.WriteFile(lockFile, []byte("pid=999998\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	f := newFastFetch(t, srv.URL, cacheDir, srv.Client())
	f.waitBudget = 50 * time.Millisecond
	f.staleLock = time.Hour // holder was written "fresh": no takeover pre-budget
	_, err := f.ensure(context.Background())
	if err == nil {
		t.Fatal("expected a bounded timeout error while another process holds the lock")
	}
	if !strings.Contains(err.Error(), "did not appear") {
		t.Fatalf("unexpected error: %v", err)
	}
}
