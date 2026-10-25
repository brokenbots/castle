// Command embedcache warms the embedded-postgres binary cache: it makes
// the pinned binary archive available in the shared single-flight cache so
// the pg-backed test binaries start with zero network access. CI runs it
// (with actions/cache over the cache dir) once before `go test ./...` so
// parallel test processes never hammer the Maven mirror concurrently.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"

	"github.com/brokenbots/castle/castle/internal/store/pg/pgtest"
)

func main() {
	repoBase := flag.String("repo-base", pgtest.DefaultBinaryRepoBase, "Maven base URL for embedded-postgres binaries")
	cacheDir := flag.String("cache-dir", pgtest.DefaultPostgresCacheDir(), "cache directory the library reads")
	flag.Parse()

	downloaded, err := pgtest.EnsurePostgresBinaryIfNeeded(context.Background(), *repoBase, *cacheDir)
	if err != nil {
		log.Fatalf("embedcache: %v", err)
	}
	fmt.Printf("embedcache: %s (downloaded=%t)\n", *cacheDir, downloaded)
}
