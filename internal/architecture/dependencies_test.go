package architecture

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const internalImportPrefix = "github.com/moviepilot-go/moviepilot-go/internal/"

type dependencyEdge struct {
	file       string
	importPath string
}

var protectedPackageRoots = []string{
	"auth",
	"config",
	"downloadconfig",
	"downloader",
	"indexer",
	"media",
	"mediaconfig",
	"mediaindex",
	"mediaserver/emby",
	"metadata",
	"notification",
	"notificationflow",
	"organizer",
	"scrapeflow",
	"scraper",
	"subscription",
	"torrentmeta",
}

var protectedPackageForbiddenImports = []string{
	internalImportPrefix + "app",
	internalImportPrefix + "cache",
	internalImportPrefix + "database",
	internalImportPrefix + "downloadflow",
	internalImportPrefix + "httpapi",
	internalImportPrefix + "jobs",
	internalImportPrefix + "organizationflow",
	internalImportPrefix + "subscriptionflow",
}

// These edges are migration debt, not approved architecture. The test also
// fails when an entry becomes unused so each migration must shrink the list.
var legacyDependencyBaseline = map[dependencyEdge]struct{}{
	{file: "downloadflow/handler.go", importPath: internalImportPrefix + "database"}:        {},
	{file: "downloadflow/handler.go", importPath: internalImportPrefix + "jobs"}:            {},
	{file: "downloadflow/reconciliation.go", importPath: internalImportPrefix + "database"}: {},
	{file: "downloadflow/reconciliation.go", importPath: internalImportPrefix + "jobs"}:     {},
	{file: "downloadflow/submission.go", importPath: internalImportPrefix + "database"}:     {},
	{file: "downloadflow/submission.go", importPath: internalImportPrefix + "jobs"}:         {},
	{file: "downloadflow/submission.go", importPath: "github.com/jackc/pgx/v5"}:             {},
	{file: "organizationflow/handler.go", importPath: internalImportPrefix + "database"}:    {},
	{file: "organizationflow/handler.go", importPath: internalImportPrefix + "jobs"}:        {},
	{file: "organizationflow/service.go", importPath: internalImportPrefix + "database"}:    {},
	{file: "organizationflow/service.go", importPath: internalImportPrefix + "jobs"}:        {},
	{file: "subscriptionflow/handler.go", importPath: internalImportPrefix + "database"}:    {},
	{file: "subscriptionflow/rss.go", importPath: internalImportPrefix + "database"}:        {},
	{file: "subscriptionflow/rss.go", importPath: internalImportPrefix + "jobs"}:            {},
}

func TestProductionDependencyDirection(t *testing.T) {
	t.Parallel()

	internalRoot := locateInternalRoot(t)
	files := productionGoFiles(t, internalRoot)
	usedBaseline := make(map[dependencyEdge]bool, len(legacyDependencyBaseline))
	var violations []string

	for _, filename := range files {
		relativeFile, err := filepath.Rel(internalRoot, filename)
		if err != nil {
			t.Fatalf("resolve relative path for %q: %v", filename, err)
		}
		relativeFile = filepath.ToSlash(relativeFile)
		importer := filepath.ToSlash(filepath.Dir(relativeFile))

		parsed, err := parser.ParseFile(token.NewFileSet(), filename, nil, 0)
		if err != nil {
			t.Fatalf("parse production source from %q: %v", relativeFile, err)
		}
		violations = append(violations, databaseEpisodeDomainViolations(relativeFile, parsed)...)
		for _, spec := range parsed.Imports {
			importPath, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatalf("decode import in %q: %v", relativeFile, err)
			}
			edge := dependencyEdge{file: relativeFile, importPath: importPath}
			if _, ok := legacyDependencyBaseline[edge]; ok {
				usedBaseline[edge] = true
				continue
			}
			violations = append(violations, dependencyViolations(importer, relativeFile, importPath)...)
		}
	}

	for edge := range legacyDependencyBaseline {
		if !usedBaseline[edge] {
			violations = append(violations, fmt.Sprintf(
				"stale legacy dependency baseline: %s no longer imports %s; remove the exception",
				edge.file,
				edge.importPath,
			))
		}
	}

	if len(violations) == 0 {
		return
	}
	sort.Strings(violations)
	t.Fatalf("production dependency direction violations:\n- %s", strings.Join(violations, "\n- "))
}

func databaseEpisodeDomainViolations(relativeFile string, parsed *ast.File) []string {
	databaseNames := make(map[string]struct{})
	for _, spec := range parsed.Imports {
		importPath, err := strconv.Unquote(spec.Path.Value)
		if err != nil || importPath != internalImportPrefix+"database" {
			continue
		}
		name := "database"
		if spec.Name != nil {
			name = spec.Name.Name
		}
		databaseNames[name] = struct{}{}
	}
	if len(databaseNames) == 0 {
		return nil
	}

	var violations []string
	ast.Inspect(parsed, func(node ast.Node) bool {
		selector, ok := node.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		identifier, ok := selector.X.(*ast.Ident)
		if !ok {
			return true
		}
		if _, ok := databaseNames[identifier.Name]; ok && strings.HasPrefix(selector.Sel.Name, "SubscriptionEpisode") {
			violations = append(violations, fmt.Sprintf(
				"%s selects database.%s: subscription episode domain types belong to internal/subscription",
				relativeFile,
				selector.Sel.Name,
			))
		}
		return true
	})
	return violations
}

func TestDependencyViolations(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		importer   string
		importPath string
		wantCount  int
	}{
		{name: "app owns HTTP API", importer: "app", importPath: internalImportPrefix + "httpapi"},
		{name: "HTTP API may use cache", importer: "httpapi", importPath: internalImportPrefix + "cache"},
		{name: "jobs owns River", importer: "jobs", importPath: "github.com/riverqueue/river"},
		{name: "cache owns Redis", importer: "cache", importPath: "github.com/redis/go-redis/v9"},
		{name: "database owns pgx", importer: "database", importPath: "github.com/jackc/pgx/v5"},
		{name: "business package cannot import database", importer: "subscription", importPath: internalImportPrefix + "database", wantCount: 1},
		{name: "HTTP API cannot leak into business package", importer: "mediaindex", importPath: internalImportPrefix + "httpapi", wantCount: 2},
		{name: "River cannot leak outside jobs", importer: "app", importPath: "github.com/riverqueue/river/rivertype", wantCount: 1},
		{name: "Redis cannot leak outside cache", importer: "metadata", importPath: "github.com/redis/go-redis/v9", wantCount: 1},
		{name: "pgx cannot leak into new flow file", importer: "downloadflow", importPath: "github.com/jackc/pgx/v5/pgconn", wantCount: 1},
		{name: "new flow database edge is rejected", importer: "subscriptionflow", importPath: internalImportPrefix + "database", wantCount: 1},
		{name: "notification flow cannot import database adapter", importer: "notificationflow", importPath: internalImportPrefix + "database", wantCount: 1},
		{name: "scrape flow cannot import jobs adapter", importer: "scrapeflow", importPath: internalImportPrefix + "jobs", wantCount: 1},
		{name: "scraper cannot import database adapter", importer: "scraper", importPath: internalImportPrefix + "database", wantCount: 1},
		{name: "database sql is adapter owned", importer: "organizationflow", importPath: "database/sql", wantCount: 1},
		{name: "goose is adapter owned", importer: "app", importPath: "github.com/pressly/goose/v3", wantCount: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := dependencyViolations(tt.importer, tt.importer+"/fixture.go", tt.importPath)
			if len(got) != tt.wantCount {
				t.Fatalf("dependencyViolations() = %#v, want %d violations", got, tt.wantCount)
			}
		})
	}
}

func TestDatabaseEpisodeDomainViolations(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		source    string
		wantCount int
	}{
		{
			name: "default database import",
			source: `package fixture
import "github.com/moviepilot-go/moviepilot-go/internal/database"
var _ database.SubscriptionEpisodeState
`,
			wantCount: 1,
		},
		{
			name: "aliased database import",
			source: `package fixture
import store "github.com/moviepilot-go/moviepilot-go/internal/database"
var _ = store.SubscriptionEpisodeMissing
`,
			wantCount: 1,
		},
		{
			name: "database error remains adapter owned",
			source: `package fixture
import "github.com/moviepilot-go/moviepilot-go/internal/database"
var _ = database.ErrSubscriptionEpisodeNotFound
`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			parsed, err := parser.ParseFile(token.NewFileSet(), "fixture.go", test.source, 0)
			if err != nil {
				t.Fatal(err)
			}
			got := databaseEpisodeDomainViolations("fixture.go", parsed)
			if len(got) != test.wantCount {
				t.Fatalf("databaseEpisodeDomainViolations() = %#v, want %d violations", got, test.wantCount)
			}
		})
	}
}

func locateInternalRoot(t *testing.T) string {
	t.Helper()

	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate architecture test source")
	}
	return filepath.Dir(filepath.Dir(filename))
}

func productionGoFiles(t *testing.T, internalRoot string) []string {
	t.Helper()

	var files []string
	err := filepath.WalkDir(internalRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			return nil
		}
		files = append(files, path)
		return nil
	})
	if err != nil {
		t.Fatalf("walk production Go files: %v", err)
	}
	sort.Strings(files)
	return files
}

func dependencyViolations(importer, relativeFile, importPath string) []string {
	var violations []string
	add := func(rule string) {
		violations = append(violations, fmt.Sprintf("%s imports %s: %s", relativeFile, importPath, rule))
	}

	if packageInRoots(importer, protectedPackageRoots) {
		for _, forbidden := range protectedPackageForbiddenImports {
			if importMatches(importPath, forbidden) {
				add("protected business packages cannot depend on composition, transport, persistence, task, cache, or flow packages")
				break
			}
		}
	}

	if importMatches(importPath, internalImportPrefix+"httpapi") && importer != "app" {
		add("internal/httpapi is owned by internal/app")
	}
	if importMatches(importPath, internalImportPrefix+"cache") && importer != "app" && importer != "httpapi" {
		add("internal/cache may only be imported by internal/app or internal/httpapi")
	}
	if importMatches(importPath, "github.com/riverqueue/river") && importer != "jobs" {
		add("River is owned by internal/jobs")
	}
	if importMatches(importPath, "github.com/redis/go-redis") && importer != "cache" {
		add("go-redis is owned by internal/cache")
	}
	if importMatches(importPath, "github.com/jackc/pgx") && importer != "database" && importer != "jobs" && importer != "httpapi" {
		add("pgx may only be used by database, jobs, and HTTP compatibility adapters")
	}
	if importPath == "database/sql" && importer != "database" {
		add("database/sql is owned by internal/database")
	}
	if importMatches(importPath, "github.com/pressly/goose") && importer != "database" {
		add("goose is owned by internal/database")
	}

	if isLegacyFlow(importer) && (importMatches(importPath, internalImportPrefix+"database") || importMatches(importPath, internalImportPrefix+"jobs")) {
		add("new flow-to-database or flow-to-jobs dependencies require migration instead of widening the legacy baseline")
	}

	return violations
}

func packageInRoots(packagePath string, roots []string) bool {
	for _, root := range roots {
		if packagePath == root || strings.HasPrefix(packagePath, root+"/") {
			return true
		}
	}
	return false
}

func importMatches(importPath, root string) bool {
	return importPath == root || strings.HasPrefix(importPath, root+"/")
}

func isLegacyFlow(importer string) bool {
	return importer == "downloadflow" || importer == "organizationflow" || importer == "subscriptionflow"
}
