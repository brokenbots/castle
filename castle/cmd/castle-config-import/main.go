// Command castle-config-import performs the KB-339 PROJECT CONFIG IMPORT:
// seeds each project's castle store record (ONE workflow config record per
// project) from the live deployed sources — the routes ConfigMap payload
// (workflowLibrary + projectRegistry + configLibrary + routes, partitioned
// per record) and each namespace's criteria-binding ConfigMap payload.
//
// Safety contract (confighome):
//   - every binding payload is parsed and validated into the record whose
//     registry entry carries that namespace;
//   - every record must carry an explicit --map mapping from its
//     projectRegistry name to a castle project id;
//   - the FULL round-trip is verified BEFORE any write: the assembly of
//     the imported records must render a routes payload canonically equal
//     to the live file (checksum equality — no silent shape changes), and
//     each record's rendered binding must be canonically equal to its live
//     file;
//   - with --cutover the verified records are marked cutover=done — the
//     castle-authoritative flip — so the payload's stale pre-cutover
//     shapes stop rendering. The flip is applied after the round-trip
//     checks, never instead of them.
//
// --create-projects also creates missing castle projects (open membership
// by default, see --policy). Import writes each record with the empty
// expected etag (a project that already holds a DIFFERENT record conflicts
// and must be resolved by the operator).
//
// Exit codes: 0 success, 1 operational/verification failure, 2 usage error.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/brokenbots/castle/castle/internal/confighome"
	"github.com/brokenbots/castle/castle/internal/store"
	"github.com/brokenbots/castle/castle/internal/store/pg"
	"github.com/brokenbots/castle/castle/internal/store/sqlite"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// sliceFlag collects repeated flag occurrences (confighome import accepts
// several --binding and --map values).
type sliceFlag []string

func (f *sliceFlag) String() string { return strings.Join(*f, " ") }
func (f *sliceFlag) Set(v string) error {
	*f = append(*f, v)
	return nil
}

// bindingSource pairs one --binding flag value (namespace=file).
type bindingSource struct {
	namespace string
	path      string
}

// projectMap pairs one --map flag value (registryName=slug).
type projectMap struct {
	name string
	slug string
}

type config struct {
	routesPath      string
	bindingSources  []bindingSource
	platformName    string
	projectMappings []projectMap
	createProjects  bool
	policy          string
	cutover         bool
	profile         string
	dbPath          string
	pgDSN           string
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("castle-config-import", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfg := &config{}

	bindingPairs := sliceFlag{}
	fs.Var(&bindingPairs, "binding", "namespace=file pair for a live binding payload (repeatable)")
	fs.StringVar(&cfg.routesPath, "routes-file", envOrDefault("CASTLE_ROUTES_PAYLOAD", ""), "live routes payload JSON file (or CASTLE_ROUTES_PAYLOAD)")
	fs.StringVar(&cfg.platformName, "platform", "", "the projectRegistry entry owning un-referenced overlay data (required)")
	projectPairs := sliceFlag{}
	fs.Var(&projectPairs, "map", "registryName=slug mapping (repeatable; required for every record)")
	fs.BoolVar(&cfg.createProjects, "create-projects", false, "create missing castle projects instead of failing")
	fs.StringVar(&cfg.policy, "policy", "open", "membership policy for --create-projects (open|closed)")
	fs.BoolVar(&cfg.cutover, "cutover", false, "mark the verified records cutover=done (castle-authoritative flip)")
	fs.StringVar(&cfg.profile, "store", "", "store profile: sqlite or pg (default sqlite)")
	fs.StringVar(&cfg.dbPath, "db", envOrDefault("CASTLE_DB_PATH", "./castle.db"), "sqlite target database path (or CASTLE_DB_PATH)")
	fs.StringVar(&cfg.pgDSN, "pg-dsn", envOrDefault("CASTLE_PG_DSN", ""), "Postgres target DSN for --store pg (or CASTLE_PG_DSN)")

	if err := fs.Parse(args); err != nil {
		return 2
	}
	for _, pair := range bindingPairs {
		ns, file, ok := strings.Cut(pair, "=")
		if !ok || ns == "" || file == "" {
			fmt.Fprintf(stderr, "castle-config-import: --binding wants namespace=file, got %q\n", pair)
			return 2
		}
		cfg.bindingSources = append(cfg.bindingSources, bindingSource{namespace: ns, path: file})
	}
	for _, pair := range projectPairs {
		name, slug, ok := strings.Cut(pair, "=")
		if !ok || name == "" || slug == "" {
			fmt.Fprintf(stderr, "castle-config-import: --map wants registryName=slug, got %q\n", pair)
			return 2
		}
		cfg.projectMappings = append(cfg.projectMappings, projectMap{name: name, slug: slug})
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "castle-config-import: unexpected positional arguments: %v\n", fs.Args())
		return 2
	}
	if err := cfg.validate(); err != nil {
		fmt.Fprintf(stderr, "castle-config-import: %v\n", err)
		return 2
	}

	if err := importConfig(cfg, stdout, stderr); err != nil {
		fmt.Fprintf(stderr, "castle-config-import: %v\n", err)
		return 1
	}
	return 0
}

func (c *config) validate() error {
	if c.routesPath == "" {
		return errors.New("--routes-file (or CASTLE_ROUTES_PAYLOAD) is required")
	}
	if c.platformName == "" {
		return errors.New("--platform is required: the projectRegistry entry owning un-referenced overlay data")
	}
	if c.profile == "" || strings.EqualFold(c.profile, "sqlite") {
		c.profile = "sqlite"
	} else if !strings.EqualFold(c.profile, "pg") {
		return fmt.Errorf("unknown --store profile %q (want sqlite or pg)", c.profile)
	}
	if c.policy != store.ProjectMembershipOpen && c.policy != store.ProjectMembershipClosed {
		return fmt.Errorf("unknown --policy %q (want %s or %s)", c.policy, store.ProjectMembershipOpen, store.ProjectMembershipClosed)
	}
	if c.profile == "pg" && c.pgDSN == "" {
		return errors.New("--pg-dsn (or CASTLE_PG_DSN) is required for --store pg")
	}
	return nil
}

func importConfig(cfg *config, stdout, stderr io.Writer) error {
	ctx := context.Background()

	// The live routes payload is the import seed.
	liveRoutes, err := os.ReadFile(cfg.routesPath)
	if err != nil {
		return fmt.Errorf("read routes payload: %w", err)
	}

	// Each live binding payload pairs by namespace; parsed and validated
	// before anything else so a broken binding fails the import fast.
	bindings := map[string]*confighome.Binding{}
	bindingBytes := map[string][]byte{}
	for _, source := range cfg.bindingSources {
		raw, err := os.ReadFile(source.path)
		if err != nil {
			return fmt.Errorf("read binding payload for %q: %w", source.namespace, err)
		}
		parsed, err := confighome.ParseBinding(raw)
		if err != nil {
			return fmt.Errorf("binding payload %q: %w", source.path, err)
		}
		bindings[source.namespace] = parsed
		bindingBytes[source.namespace] = raw
	}

	records, err := confighome.Partition(cfg.platformName, liveRoutes)
	if err != nil {
		return fmt.Errorf("partition routes payload: %w", err)
	}

	// Names without a mapping fail here, naming the exact missing keys so
	// the operator can re-run with the suggested --map flags.
	slugFor := map[string]string{}
	for _, m := range cfg.projectMappings {
		slugFor[m.name] = m.slug
	}
	missing := make([]string, 0, len(records))
	for name := range records {
		if slugFor[name] == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("no --map entry for: %s (add --map \"Name=slug\" per record)", strings.Join(missing, ", "))
	}

	// Attach each namespace's binding payload to its record, then validate
	// every record fail-closed before the round-trip. Every --binding
	// source must be consumed by a record's registry entry: a dangling
	// namespace fails the import instead of silently dropping data.
	names := make([]string, 0, len(records))
	for name := range records {
		names = append(names, name)
	}
	slices.Sort(names)
	consumed := make([]string, 0, len(cfg.bindingSources))
	projRecords := make([]confighome.ProjectRecord, 0, len(records))
	for _, name := range names {
		record := records[name]
		if binding, ok := bindings[record.Namespace]; ok {
			record.Binding = binding
			consumed = append(consumed, record.Namespace)
		}
		if err := record.Validate(); err != nil {
			return fmt.Errorf("record %q: %w", name, err)
		}
		projRecords = append(projRecords, confighome.ProjectRecord{
			ProjectID: slugFor[name],
			Record:    record,
		})
	}
	for _, source := range cfg.bindingSources {
		if !slices.Contains(consumed, source.namespace) {
			return fmt.Errorf("--binding namespace %q: no imported record carries it (check --map and the registry entry's namespace)", source.namespace)
		}
	}

	// The verified round-trip: the records' assembly must render a routes
	// payload canonically equal to the live file, and each binding payload
	// must render canonically equal to its live file.
	assembly := confighome.NewAssemblyFor(projRecords)
	renderedRoutes, err := assembly.RenderRoutesPayload()
	if err != nil {
		return fmt.Errorf("render routes payload from records: %w", err)
	}
	equal, checksum, err := confighome.EqualPayload(renderedRoutes, liveRoutes)
	if err != nil {
		return fmt.Errorf("compare rendered routes payload: %w", err)
	}
	liveChecksum, err := confighome.CanonicalChecksum(liveRoutes)
	if err != nil {
		return fmt.Errorf("checksum live routes payload: %w", err)
	}
	if !equal {
		fmt.Fprintf(stderr, "round-trip MISMATCH: rendered checksum %s, live checksum %s\n", checksum, liveChecksum)
		return errors.New("records rendered a routes payload that differs from the live payload; import aborted (no writes happened)")
	}

	for _, pr := range projRecords {
		if pr.Record.Binding == nil {
			continue
		}
		rendered, err := pr.Record.RenderBinding()
		if err != nil {
			return fmt.Errorf("render binding for %s: %w", pr.ProjectID, err)
		}
		equal, checksum, err := confighome.EqualPayload(rendered, bindingBytes[pr.Record.Namespace])
		if err != nil {
			return fmt.Errorf("compare rendered binding for %s: %w", pr.ProjectID, err)
		}
		liveBinding, err := confighome.CanonicalChecksum(bindingBytes[pr.Record.Namespace])
		if err != nil {
			return fmt.Errorf("checksum live binding for %s: %w", pr.ProjectID, err)
		}
		if !equal {
			fmt.Fprintf(stderr, "binding round-trip MISMATCH for %s: rendered checksum %s, live checksum %s\n", pr.ProjectID, checksum, liveBinding)
			return errors.New("record binding differs from the live binding payload; import aborted (no writes happened)")
		}
	}

	// The castle-authoritative flip rides AFTER the verified round-trips:
	// cutover=done marks each record so the payload's stale shapes stop
	// rendering from castle.
	if cfg.cutover {
		for _, pr := range projRecords {
			pr.Record.Cutover = confighome.CutoverDone
			if err := pr.Record.Validate(); err != nil {
				return fmt.Errorf("record %q after cutover flip: %w", pr.Record.RegistryName, err)
			}
		}
	}

	// Open the store per profile: Open runs the migrations so a fresh
	// target (e.g. a new project-config column) is ready.
	var st store.Store
	if cfg.profile == "pg" {
		st, err = pg.Open(cfg.pgDSN)
	} else {
		st, err = sqlite.Open(cfg.dbPath)
	}
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()

	// Resolve the target projects.
	type target struct {
		slug   string
		name   string
		doc    []byte
		exists bool
	}
	targets := make([]target, 0, len(projRecords))
	for _, pr := range projRecords {
		doc, err := pr.Record.Marshal()
		if err != nil {
			return fmt.Errorf("marshal record %q: %w", pr.Record.RegistryName, err)
		}
		_, err = st.GetProject(ctx, pr.ProjectID)
		switch {
		case err == nil:
			targets = append(targets, target{slug: pr.ProjectID, name: pr.Record.RegistryName, doc: doc, exists: true})
		case errors.Is(err, store.ErrNotFound):
			if !cfg.createProjects {
				return fmt.Errorf("project %q: not found (import --create-projects to create it)", pr.ProjectID)
			}
			targets = append(targets, target{slug: pr.ProjectID, name: pr.Record.RegistryName, doc: doc, exists: false})
		default:
			return fmt.Errorf("load project %q: %w", pr.ProjectID, err)
		}
	}

	// Create first (one pass), then write every record with the empty
	// expected etag.
	for i, t := range targets {
		if t.exists {
			continue
		}
		created := &store.Project{ID: t.slug, MembershipPolicy: cfg.policy}
		if err := st.CreateProject(ctx, created); err != nil {
			return fmt.Errorf("create project %q: %w", t.slug, err)
		}
		targets[i].exists = true
	}
	for _, t := range targets {
		etag, err := st.SetProjectConfig(ctx, t.slug, t.doc, "")
		if err != nil {
			return fmt.Errorf("write config for project %q: %w", t.slug, err)
		}
		fmt.Fprintf(stdout, "imported %q: project %s etag %s\n", t.name, t.slug, etag)
	}
	fmt.Fprintf(stdout, "verified %d record(s) with routes round-trip checksum %s\n", len(targets), checksum)
	return nil
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
