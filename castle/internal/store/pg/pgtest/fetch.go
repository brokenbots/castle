// fetch.go makes the embedded-postgres binary provisioning deterministic
// across the parallel test processes that boot the server. The underlying
// library fetches the ~15MB binary archive from Maven Central inside every
// process (its download mutex is process-local), so a cold `go test ./...`
// hammers the mirror with identical requests and a single transient non-200
// turns the whole CI job red with `no version found matching <version>`.
//
// This helper pins the same binary the library boots, seeds the cache the
// library reads, and guarantees:
//   - a warm cache means the server starts with zero network access;
//   - only one process per runner downloads, via a lock file other
//     processes poll on (with stale-holder takeover so a crashed holder
//     never wedges the run);
//   - transient failures are retried with backoff, the jar is verified
//     against the Maven .sha256 sidecar when the sidecar is reachable, and
//     the archive lands in the cache atomically (temp file + rename) so a
//     concurrent reader never sees a partial archive.
package pgtest

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
)

// PinnedPostgresVersion is the embedded-postgres binary pgtest runs: the
// library's V18 constant. pgtest pins it explicitly (not by inheriting
// DefaultConfig) and the CI cache key pins the same string, so a library
// upgrade cannot silently change what pgtest downloads and what CI caches.
const (
	// PinnedPostgresVersion must equal embeddedpostgres.V18; init enforces it.
	PinnedPostgresVersion = "18.3.0"

	// DefaultBinaryRepoBase is the Maven base URL the library's
	// BinaryRepositoryURL knob also points at by default.
	DefaultBinaryRepoBase = "https://repo1.maven.org/maven2"
)

func init() {
	if embeddedpostgres.V18 != PinnedPostgresVersion {
		panic(fmt.Sprintf("pgtest: pinned version %s drifted from embeddedpostgres.V18=%s", PinnedPostgresVersion, embeddedpostgres.V18))
	}
}

// DefaultPostgresCacheDir is the cache directory the library resolves when
// no CachePath is configured: <userHome>/.embedded-postgres-go. pgtest
// passes it explicitly to both the fetch helper and the server config so
// the code and the CI cache key cannot drift apart.
func DefaultPostgresCacheDir() string {
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".embedded-postgres-go")
	}
	// The library falls back to a bare ".embedded-postgres-go" in the
	// working directory when the home dir is unknown; mirror it so the file
	// this helper writes is the file the library reads.
	return ".embedded-postgres-go"
}

// zonky
// zonkyOSArch maps the runtime OS/arch onto the zonkyio
// embedded-postgres-binaries artifact naming. It mirrors the library's
// defaultVersionStrategy (version >= 14.2) so the archive this helper
// writes into the cache is exactly the file the library's default cache
// locator reads back.
func zonkyOSArch() (string, string) {
	osName, arch := runtime.GOOS, runtime.GOARCH
	if osName == "linux" {
		// Zone arm binaries: arm64v8 and the arm32v6/arm32v7 machine names.
		switch {
		case arch == "arm64":
			arch += "v8"
		case arch == "arm":
			switch machine := linuxMachineName(); {
			case strings.HasPrefix(machine, "armv7"):
				arch += "32v7"
			case strings.HasPrefix(machine, "armv6"):
				arch += "32v6"
			}
		}
		if isAlpineLinux() {
			arch += "-alpine"
		}
	}
	// Postgres 14+ publishes macos arm64 builds; the library suffixes those.
	if osName == "darwin" && arch == "arm64" {
		arch += "v8"
	}
	return osName, arch
}

func linuxMachineName() string {
	if out, err := exec.Command("uname", "-m").Output(); err == nil {
		return strings.TrimSpace(string(out))
	}
	return ""
}

func isAlpineLinux() bool {
	_, err := os.Stat("/etc/alpine-release")
	return err == nil
}

// binaryArtifactPaths builds the Maven layout paths for the zonky
// embedded-postgres binaries: defaultRemoteFetchStrategy fetches
// <base>/io/zonky/test/postgres/embedded-postgres-binaries-<os>-<arch>/<version>/
// embedded-postgres-binaries-<os>-<arch>-<version>.jar and the
// defaultCacheLocator reads <cacheDir>/<same base name>.txz from the jar's
// single archive entry.
func binaryArtifactPaths(base, osName, arch string) (jarURL, shaURL, cacheName string) {
	trimmed := strings.TrimRight(base, "/")
	dir := fmt.Sprintf("%s/io/zonky/test/postgres/embedded-postgres-binaries-%s-%s/%s",
		trimmed, osName, arch, PinnedPostgresVersion)
	name := fmt.Sprintf("embedded-postgres-binaries-%s-%s-%s", osName, arch, PinnedPostgresVersion)
	return dir + "/" + name + ".jar", dir + "/" + name + ".jar.sha256", name + ".txz"
}

// postgresBinaryFetch carries one fetch-or-skip pass over the binary
// cache. Zero values resolve to production defaults; the helper's tests
// shrink the schedules to keep the suite fast.
type postgresBinaryFetch struct {
	repoBase string
	cacheDir string
	client   *http.Client
	// downloadBackoff holds the delays between jar download attempts; five
	// attempts with 2s/4s/8s/16s backoff by default.
	downloadBackoff []time.Duration
	// pollEvery is the waiter cadence while another process downloads,
	// waitBudget the waiter's overall bound before giving up, and
	// staleLock the lock-file age after which a dead holder is evicted.
	pollEvery, waitBudget, staleLock time.Duration
}

func (f *postgresBinaryFetch) normalize() {
	if f.repoBase == "" {
		f.repoBase = DefaultBinaryRepoBase
	}
	if f.client == nil {
		f.client = http.DefaultClient
	}
	if len(f.downloadBackoff) == 0 {
		f.downloadBackoff = []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second}
	}
	if f.pollEvery == 0 {
		f.pollEvery = 2 * time.Second
	}
	if f.waitBudget == 0 {
		f.waitBudget = 10 * time.Minute
	}
	if f.staleLock == 0 {
		f.staleLock = 5 * time.Minute
	}
}

// EnsurePostgresBinary makes the pinned embedded-postgres binary archive
// available in the OS-wide cache the library reads, downloading it through
// the single-flight fetch when the cache is cold.
func EnsurePostgresBinary(ctx context.Context) error {
	_, err := EnsurePostgresBinaryIfNeeded(ctx, DefaultBinaryRepoBase, DefaultPostgresCacheDir())
	return err
}

// EnsurePostgresBinaryIfNeeded is the parameterized form of
// EnsurePostgresBinary: repoBase is the Maven base URL (or an httptest
// stand-in in tests), cacheDir the cache the server config and the CI
// key share. It returns whether this call downloaded the archive.
func EnsurePostgresBinaryIfNeeded(ctx context.Context, repoBase, cacheDir string) (bool, error) {
	f := postgresBinaryFetch{repoBase: repoBase, cacheDir: cacheDir}
	f.normalize()
	return f.ensure(ctx)
}

func (f *postgresBinaryFetch) ensure(ctx context.Context) (bool, error) {
	osName, arch := zonkyOSArch()
	jarURL, shaURL, cacheName := binaryArtifactPaths(f.repoBase, osName, arch)
	cacheFile := filepath.Join(f.cacheDir, cacheName)
	lockFile := cacheFile + ".lock"

	if err := os.MkdirAll(f.cacheDir, 0o755); err != nil {
		return false, fmt.Errorf("embedded-postgres cache dir %s: %w", f.cacheDir, err)
	}

	// Warm cache: the server starts with zero network access.
	if archiveReady(cacheFile) {
		return false, nil
	}

	deadline := time.Now().Add(f.waitBudget)
	for {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		// The archive may land while we poll for the holder's lock.
		if archiveReady(cacheFile) {
			return false, nil
		}
		if acquireLock(lockFile) {
			err := f.downloadAndInstall(ctx, jarURL, shaURL, cacheFile)
			if rmErr := os.Remove(lockFile); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
				err = errors.Join(err, fmt.Errorf("release fetch lock: %w", rmErr))
			}
			if err != nil {
				// Give the lock up: the next waiting process retries as the
				// holder rather than everyone failing together.
				return false, err
			}
			return true, nil
		}
		// A holder that died mid-download leaves a stale lock behind; take
		// it over instead of waiting the budget out.
		if staleLock(lockFile, f.staleLock) {
			_ = os.Remove(lockFile)
			continue
		}
		if time.Now().After(deadline) {
			return false, fmt.Errorf("embedded-postgres fetch: %s did not appear after %s (watching lock %s)",
				cacheName, f.waitBudget, lockFile)
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(f.pollEvery):
		}
	}
}

// acquireLock takes the single-flight lock, stamping the holder for
// diagnostics. A failed O_EXCL create means another process holds it.
func acquireLock(lockFile string) bool {
	f, err := os.OpenFile(lockFile, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return false
	}
	defer f.Close()
	fmt.Fprintf(f, "pid=%d\nstarted=%d\n", os.Getpid(), time.Now().Unix())
	return true
}

func staleLock(lockFile string, after time.Duration) bool {
	info, err := os.Stat(lockFile)
	if err != nil {
		return false
	}
	return info.ModTime().Before(time.Now().Add(-after))
}

func archiveReady(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir() && info.Size() > 0
}

// downloadAndInstall fetches the jar, verifies it against the .sha256
// sidecar when the sidecar is reachable, and installs the single .txz
// entry atomically into the cache path the library reads.
func (f *postgresBinaryFetch) downloadAndInstall(ctx context.Context, jarURL, shaURL, cacheFile string) error {
	jarBody, err := retryableGet(ctx, f.client, jarURL, f.downloadBackoff)
	if err != nil {
		return err
	}
	if shaBody, err := downloadOnce(ctx, f.client, shaURL); err == nil {
		sum := sha256.Sum256(jarBody)
		if want := strings.TrimSpace(string(shaBody)); want != hex.EncodeToString(sum[:]) {
			return fmt.Errorf("embedded-postgres fetch: %s checksum mismatch (advertised %q, jar %s)", jarURL, want, hex.EncodeToString(sum[:]))
		}
	}
	// A broken sidecar does not block the fetch: the library treats the
	// sidecar the same way, and CI warms the cache from Maven Central.
	return installZipEntryTo(jarBody, cacheFile)
}

// installZipEntryTo extracts the single *.txz entry of the fetched jar and
// writes it to cacheFile via temp-file-and-rename so a concurrent reader
// never observes a partial archive.
func installZipEntryTo(jarBytes []byte, cacheFile string) error {
	zr, err := zip.NewReader(bytes.NewReader(jarBytes), int64(len(jarBytes)))
	if err != nil {
		return fmt.Errorf("embedded-postgres fetch: read jar: %w", err)
	}
	var entry *zip.File
	for _, cand := range zr.File {
		if cand.FileInfo().IsDir() {
			continue
		}
		if strings.HasSuffix(cand.Name, ".txz") {
			entry = cand
			break
		}
	}
	if entry == nil {
		return errors.New("embedded-postgres fetch: no .txz binary entry in the fetched jar")
	}
	in, err := entry.Open()
	if err != nil {
		return fmt.Errorf("embedded-postgres fetch: open jar entry %s: %w", entry.Name, err)
	}
	defer in.Close()

	tmp, err := os.CreateTemp(filepath.Dir(cacheFile), ".embedded-postgres-*.part")
	if err != nil {
		return fmt.Errorf("embedded-postgres fetch: temp file in %s: %w", filepath.Dir(cacheFile), err)
	}
	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return fmt.Errorf("embedded-postgres fetch: write cache: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("embedded-postgres fetch: flush cache: %w", err)
	}
	if err := os.Rename(tmp.Name(), cacheFile); err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("embedded-postgres fetch: publish cache file: %w", err)
	}
	// 0o600: the cache is single-user (CI runner / developer home) and the
	// server binary must not be world-writable.
	os.Chmod(cacheFile, 0o600)
	return nil
}

// retryableGet fetches url retrying network failures and non-200 status
// with the configured backoff schedule; both are treated as transient.
func retryableGet(ctx context.Context, client *http.Client, url string, backoff []time.Duration) ([]byte, error) {
	if client == nil {
		client = http.DefaultClient
	}
	var lastErr error
	for attempt := 0; ; attempt++ {
		body, err := downloadOnce(ctx, client, url)
		if err == nil {
			return body, nil
		}
		lastErr = err
		if attempt >= len(backoff) {
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff[attempt]):
		}
	}
	return nil, lastErr
}

func downloadOnce(ctx context.Context, client *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("embedded-postgres fetch: %s: %w", url, err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embedded-postgres fetch: %s: %w", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		// The library maps any non-200 to the same error; treat it as the
		// transient mirror failure it in fact is.
		return nil, fmt.Errorf("embedded-postgres fetch: GET %s: %s", url, resp.Status)
	}
	return body, nil
}
