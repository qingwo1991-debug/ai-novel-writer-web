//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/admission"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/globaladmission"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/globalconfig"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/policy"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// NOVEL_DATA_ROOT is the author data mount the shipped web gateway already uses.
// Every path below is derived from it in the same way, so a data directory the
// existing gateway wrote is admitted unchanged.
// This lane never creates a directory, a database or a migration journal: the
// only writes it makes are inside the private staging workspace it owns.
const (
	productionLibraryDirectory = "workspaces"
	productionConfigDirectory  = "config"
	productionConfigTarget     = "ai-novel-writer"
	productionMigrationTarget  = "ai-novel-writer-web-runtime"
	productionLegacyDirectory  = "legacy-global"
	// productionMaxDatabaseBytes bounds one staged author database copy. It is
	// the same 64 MiB budget the durable synthetic lane admits.
	productionMaxDatabaseBytes = 64 << 20
	// productionStartupTimeout bounds the whole read-only startup admission,
	// including one staged copy per library project.
	productionStartupTimeout = 5 * time.Minute
	// productionRecentLimit matches the shipped recent-project shelf bound.
	productionRecentLimit = 20
	// productionLibraryEntries matches the browser chooser bound.
	productionLibraryEntries = 4096
)

// productionLayout is the trusted physical layout of one author data root.
// No component is request-derived; each is a fixed child of NOVEL_DATA_ROOT.
type productionLayout struct {
	dataRoot        string
	libraryRoot     string
	canonicalTarget string
	migrationRoot   string
	legacySource    string
}

func productionLayoutFor(raw string) (productionLayout, error) {
	value := strings.TrimSpace(raw)
	if value == "" || strings.ContainsRune(value, '\x00') || !filepath.IsAbs(value) {
		return productionLayout{}, errors.New("PRODUCTION_DATA_ROOT_UNSAFE")
	}
	// Operators pass a configured mount, so normalising a trailing separator is
	// not authority: nothing here is a browser-supplied path.
	root := filepath.Clean(value)
	if root == string(filepath.Separator) {
		return productionLayout{}, errors.New("PRODUCTION_DATA_ROOT_UNSAFE")
	}
	return productionLayout{
		dataRoot:        root,
		libraryRoot:     filepath.Join(root, productionLibraryDirectory),
		canonicalTarget: filepath.Join(root, productionConfigDirectory, productionConfigTarget),
		migrationRoot:   filepath.Join(root, productionConfigDirectory, productionMigrationTarget),
		legacySource:    filepath.Join(root, productionLegacyDirectory),
	}, nil
}

// productionConfig is the trusted backend input of this lane. Schema is the
// shipped pinned v7 admission contract; a test substitutes a synthetic one so
// the staging wiring can be proven without the TypeScript schema oracle.
type productionConfig struct {
	Schema           admission.SchemaContract
	MaxDatabaseBytes int64
}

// productionProject is one library entry whose database was admitted as a
// current-schema author database, with the evidence proving it was read.
type productionProject struct {
	listed   projectfs.ListedProject
	evidence admission.Evidence
}

// productionOpen is the read-only production lane. It pins the completed global
// data migration, owns the author library, and answers only the operations that
// read real state: startup/config from the committed generation, the recent
// shelf and the library chooser from admitted author databases.
//
// It is deliberately not a project lifecycle: no author database stays open, no
// project capability is issued, and project:open / project:create / db:draft-*
// keep failing closed at GO_OPERATION_NOT_MIGRATED.
type productionOpen struct {
	mu       sync.Mutex
	layout   productionLayout
	global   *globaladmission.PinnedReady
	config   *globalconfig.Service
	startup  *globalconfig.Startup
	library  *projectfs.Library
	scratch  string
	projects []productionProject
	closed   bool
}

// openProduction admits a real author data directory, or fails closed with a
// symbolic code. A directory that is not a completed global migration, or not
// an author library, is never repaired, seeded or created here.
func openProduction(ctx context.Context, layout productionLayout, config productionConfig) (*productionOpen, error) {
	if ctx == nil || len(config.Schema) == 0 || config.MaxDatabaseBytes <= 0 {
		return nil, errors.New("PRODUCTION_CONFIGURATION_INVALID")
	}
	if err := requirePhysicalDirectory(layout.dataRoot); err != nil {
		return nil, err
	}
	// The production entrypoint holds the util-linux flock on
	// $NOVEL_DATA_ROOT/.native-web.gateway.lock for the process lifetime
	// (docker/web-entrypoint.sh), so this process inherits that exclusion instead
	// of re-acquiring the same inode, which would fail closed. exclusive is a
	// trusted claim made by main; no request field can reach it.
	global, err := globaladmission.PinCompleted(ctx, globaladmission.Roots{
		LegacySource:    layout.legacySource,
		CanonicalTarget: layout.canonicalTarget,
		UserData:        layout.migrationRoot,
	}, true)
	if err != nil {
		return nil, fmt.Errorf("production global data: %w", err)
	}
	open := &productionOpen{layout: layout, global: global}
	fail := func(cause error) (*productionOpen, error) {
		_ = open.Close()
		return nil, cause
	}
	// PinCompleted already verified the migration semantics; that identity read
	// returns the receipt facts (committed generation, ignored legacy source,
	// preserved unknown count) the startup state must report.
	ready, err := global.VerifyIdentity(ctx)
	if err != nil {
		return fail(fmt.Errorf("production global data: %w", err))
	}
	open.config, err = globalconfig.New(filepath.Clean(ready.DataRoot))
	if err != nil {
		return fail(fmt.Errorf("production global config: %w", err))
	}
	// The startup capture mirrors the shipped gateway exactly: the generation is
	// the committed receipt generation, never a request value or a directory
	// name, and the notice reports the migration facts the receipt proves.
	open.startup = globalconfig.NewStartup(globalconfig.StartupCapture{
		Generation: ready.GlobalGeneration,
		MigrationNotice: &globalconfig.MigrationNotice{
			LegacySourceIgnored:   ready.LegacySourceIgnored,
			PreservedUnknownCount: ready.PreservedUnknownCount,
		},
	})
	open.library, err = projectfs.OpenLibrary(layout.libraryRoot)
	if err != nil {
		return fail(fmt.Errorf("production library: %w", err))
	}
	open.scratch, err = os.MkdirTemp(os.TempDir(), "novel-production-admission-")
	if err != nil {
		return fail(err)
	}
	listed, err := open.library.ListCanonicalProjects(ctx, projectfs.MaxLibraryEntries)
	if err != nil {
		return fail(fmt.Errorf("production library inventory: %w", err))
	}
	admitted := make([]productionProject, 0, len(listed))
	var rejected error
	for _, entry := range listed {
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		evidence, admitErr := admitProductionProject(ctx, open.library, entry, open.scratch, config)
		if admitErr != nil {
			if rejected == nil {
				rejected = admitErr
			}
			log.Printf("novel-server: not a current-schema author database, left unserved: %s: %v", entry.Path, admitErr)
			continue
		}
		admitted = append(admitted, productionProject{listed: entry, evidence: evidence})
	}
	// A library holding projects this build cannot admit is a configuration
	// failure, not an empty shelf: report it instead of hiding the author's data.
	if len(listed) > 0 && len(admitted) == 0 {
		return fail(fmt.Errorf("production library holds no admitted author database: %w", rejected))
	}
	open.projects = admitted
	log.Printf("novel-server: admitted %d of %d author projects from %s", len(admitted), len(listed), open.layout.libraryRoot)
	return open, nil
}

// admitProductionProject proves one library entry is a current-schema author
// database without writing it and without leaving a handle open. It stages the
// exact database set into a private copy, exactly like projectaccess.Preflight,
// and then verifies that copy against the trusted contract. The author's own
// database is only ever byte-copied, never opened.
func admitProductionProject(ctx context.Context, library *projectfs.Library, entry projectfs.ListedProject, workspace string, config productionConfig) (admission.Evidence, error) {
	var zero admission.Evidence
	project, err := library.OpenProject(entry.Path)
	if err != nil {
		return zero, err
	}
	defer project.Close()
	if project.ProjectID() != entry.ProjectID {
		return zero, errors.New("PRODUCTION_PROJECT_IDENTITY_CHANGED")
	}
	directory, err := os.MkdirTemp(workspace, "admit-")
	if err != nil {
		return zero, err
	}
	defer func() { _ = os.RemoveAll(directory) }()
	root, err := os.OpenRoot(directory)
	if err != nil {
		return zero, err
	}
	_, stageErr := project.StageDatabase(ctx, root, config.MaxDatabaseBytes)
	closeErr := root.Close()
	if stageErr != nil {
		return zero, stageErr
	}
	if closeErr != nil {
		return zero, closeErr
	}
	evidence, err := admission.VerifySnapshot(ctx, filepath.Join(directory, "snapshot.db"), config.Schema)
	if err != nil {
		return zero, err
	}
	return evidence, nil
}

func requirePhysicalDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("PRODUCTION_DATA_ROOT_UNSAFE")
	}
	return nil
}

func (p *productionOpen) closedError() error {
	if p == nil || p.closed {
		return errors.New("PRODUCTION_CLOSED")
	}
	return nil
}

// ListCanonicalProjects is the /api/library inventory. It serves only the
// projects whose author database was admitted at startup, so the chooser and
// the recent shelf can never disagree about what this build can read.
func (p *productionOpen) ListCanonicalProjects(ctx context.Context, maxEntries int) ([]projectfs.ListedProject, error) {
	if p == nil || ctx == nil || maxEntries < 1 || maxEntries > projectfs.MaxLibraryEntries {
		return nil, errors.New("PRODUCTION_LIBRARY_REQUEST_INVALID")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.closedError(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	listed := make([]projectfs.ListedProject, 0, len(p.projects))
	for _, item := range p.projects {
		if len(listed) >= maxEntries {
			break
		}
		listed = append(listed, item.listed)
	}
	return listed, nil
}

// RecentProjects is project:recent-list. The shipped gateway indexes the
// canonical library directly instead of the desktop recent-projects.json, so
// this shelf is the admitted library in the library's own newest-first order.
func (p *productionOpen) RecentProjects(ctx context.Context, _ transport.Caller) ([]transport.RecentNavigationProject, error) {
	if p == nil || ctx == nil {
		return nil, errors.New("PRODUCTION_LIBRARY_REQUEST_INVALID")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.closedError(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	result := make([]transport.RecentNavigationProject, 0, len(p.projects))
	for _, item := range p.projects {
		if len(result) >= productionRecentLimit {
			break
		}
		result = append(result, transport.RecentNavigationProject{
			Name: item.listed.Name, Path: item.listed.Path,
			UpdatedAt: item.listed.UpdatedAt, ProjectID: item.listed.ProjectID,
		})
	}
	return result, nil
}

func (p *productionOpen) Close() error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	workspace := p.scratch
	p.scratch = ""
	library, config, global := p.library, p.config, p.global
	p.mu.Unlock()
	var result error
	if library != nil {
		result = errors.Join(result, library.Close())
	}
	if config != nil {
		result = errors.Join(result, config.Close())
	}
	if global != nil {
		result = errors.Join(result, global.Close())
	}
	if workspace != "" {
		result = errors.Join(result, os.RemoveAll(workspace))
	}
	return result
}

// productionHandler is the only startup path main uses. The schema authority is
// the shipped pinned v7 contract, never a caller-selected file.
func productionHandler(registry *policy.Registry, address, staticDir, dataRoot string) (http.Handler, func(), error) {
	contract, err := admission.PinnedSchemaContract()
	if err != nil {
		return nil, nil, fmt.Errorf("production schema contract: %w", err)
	}
	return productionHandlerWithContract(registry, address, staticDir, dataRoot, productionConfig{
		Schema: contract, MaxDatabaseBytes: productionMaxDatabaseBytes,
	})
}

// productionHandlerWithContract is the lane's test seam: it exists so a
// synthetic tiny database can prove the staging and verification wiring even
// though its schema fingerprint is not in the shipped contract.
func productionHandlerWithContract(registry *policy.Registry, address, staticDir, dataRoot string, config productionConfig) (http.Handler, func(), error) {
	layout, err := productionLayoutFor(dataRoot)
	if err != nil {
		return nil, nil, err
	}
	startupContext, cancel := context.WithTimeout(context.Background(), productionStartupTimeout)
	defer cancel()
	open, err := openProduction(startupContext, layout, config)
	if err != nil {
		return nil, nil, err
	}
	server := transport.New(registry, security.NewOrigins("http://"+address), "go-production-admitted")
	if bindErr := server.BindNavigation(); bindErr != nil {
		_ = open.Close()
		return nil, nil, bindErr
	}
	// A real author data directory is admitted read-only this round, so only the
	// read and acknowledgement half of the global config family is published.
	// config:set would write the author's committed generation and keeps failing
	// closed at GO_OPERATION_NOT_MIGRATED until a lane owns that write.
	if bindErr := server.BindGlobalConfigReads(open.config, open.startup); bindErr != nil {
		_ = open.Close()
		return nil, nil, bindErr
	}
	// project:recent-list is bound on its own: the editor navigation family would
	// also publish fs:list-dir, which needs a live project lease this lane can
	// never issue because project:open is not admitted.
	if bindErr := server.Bind("project:recent-list", func(ctx context.Context, caller transport.Caller, args []any) (any, error) {
		if len(args) != 0 {
			return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
		}
		return open.RecentProjects(ctx, caller)
	}); bindErr != nil {
		_ = open.Close()
		return nil, nil, bindErr
	}
	route, err := transport.NewLibraryRoute(server, open, productionLibraryEntries)
	if err != nil {
		_ = open.Close()
		return nil, nil, err
	}
	mux := http.NewServeMux()
	mux.Handle("/api/library", route)
	mux.Handle("/api/", server)
	mux.Handle("/api", server)
	mux.Handle("/healthz", server)
	var static *transport.StaticAssets
	cleanup := func() {
		server.Close()
		if static != nil {
			_ = static.Close()
		}
		_ = open.Close()
	}
	if staticDir != "" {
		static, err = transport.NewStaticAssets(staticDir, server.SessionStore())
		if err != nil {
			cleanup()
			return nil, nil, fmt.Errorf("static assets: %w", err)
		}
		mux.Handle("/", static)
	} else {
		mux.Handle("/", server)
	}
	return mux, cleanup, nil
}
