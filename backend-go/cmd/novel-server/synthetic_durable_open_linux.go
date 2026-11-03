//go:build linux

package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sync"

	_ "github.com/mattn/go-sqlite3" // Registers the database/sql driver "sqlite3".
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/admission"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/authorruntime"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/policy"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectaccess"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectcatalog"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectruntime"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/writerlock"
)

// syntheticDurableConfig is the trusted bridge input for the durable lane: the
// current v7 schema contract plus the real baseline database it fingerprints.
// Both come from the administrator supplied contract directory, never from an
// author project.
type syntheticDurableConfig struct {
	Schema   admission.SchemaContract
	Baseline string
}

const (
	// syntheticBaselineDatabaseName is the trusted v7 SQLite baseline exported
	// by the original migration lane next to the contract JSON. The durable
	// creator copies it so the created database fingerprints exactly like the
	// current supported schema.
	syntheticBaselineDatabaseName = "baseline-v7.db"
	// syntheticDurableProjectName is the single real synthetic author project
	// this lane creates; no other library entry is ever openable through it.
	syntheticDurableProjectName = "synthetic-durable-author"
	syntheticDurableProjectID   = "d1ea93f0-3b7a-4c2b-9e5d-a1f2b3c4d5e6"
)

// createSyntheticDurableProject materializes a REAL current-schema author
// project inside the already private library dir: the project manifest plus a
// genuine SQLite project.db whose schema fingerprints against the trusted
// contract and whose project_core already contains its id='main' row. It adds
// no knowledge tables or columns, so AdmitAuthor's knowledge gate stays closed.
// Safe sidecars are never left behind; on any error only the files this helper
// created are removed.
func createSyntheticDurableProject(libraryDir, baseline string) (string, error) {
	if !underTempRoot(libraryDir) || !privateSyntheticGlobalRoot(filepath.Dir(libraryDir)) {
		return "", errors.New("SYNTHETIC_DURABLE_UNSAFE")
	}
	data, err := os.ReadFile(baseline)
	if err != nil {
		return "", fmt.Errorf("synthetic durable baseline: %w", err)
	}
	projectPath := filepath.Join(libraryDir, syntheticDurableProjectName)
	if _, err := os.Lstat(projectPath); err == nil {
		return "", errors.New("SYNTHETIC_DURABLE_PROJECT_EXISTS")
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	storage := filepath.Join(projectPath, projectfs.StorageDirectory)
	if err := os.MkdirAll(storage, 0700); err != nil {
		return "", err
	}
	failed := true
	defer func() {
		if failed {
			_ = os.RemoveAll(projectPath)
		}
	}()
	manifest := `{"schemaVersion":1,"kind":"ai-novel-project","projectId":"` + syntheticDurableProjectID + `","createdAt":"2026-01-01T00:00:00.000Z","storageFormat":"ai-novel","storageVersion":1}`
	if err := os.WriteFile(filepath.Join(storage, projectfs.ManifestName), []byte(manifest), 0600); err != nil {
		return "", err
	}
	database := filepath.Join(storage, projectfs.DatabaseName)
	if err := os.WriteFile(database, data, 0600); err != nil {
		return "", err
	}
	seed, err := sql.Open("sqlite3", database)
	if err != nil {
		return "", err
	}
	seed.SetMaxOpenConns(1)
	// The exact row shape proven by the durable runtime unit test: every column
	// readProjectOpenCore scans must be present and the required ones non-NULL.
	_, execErr := seed.Exec(`INSERT INTO project_core(id, project_name, writing_language, genre,
		sub_genre, target_audience, total_chapters, words_per_chapter, creative_strategy,
		narrative_thread_dormant_threshold, plot_structure, narrative_pov, core_outline,
		world_setting, golden_finger, protagonist_profile, global_guidance, writing_style,
		reference_works, character_states, created_at, updated_at) VALUES('main', 'Synthetic durable novel', 'zh-CN',
		'fantasy', 'x', 'adult', 120, 2000, 'consistency-first', 4, 'three_act', 'first_person',
		'outline', 'world', 'gold', 'lead', 'guidance', 'plain', 'reference', '{}',
		'2026-01-01 00:00:00', '2026-01-02 01:02:03')`)
	closeErr := seed.Close()
	if execErr != nil || closeErr != nil {
		return "", errors.Join(execErr, closeErr)
	}
	failed = false
	return projectPath, nil
}

// syntheticDurableOpen is the transport bridge for the durable Go author
// runtime. It shares the process-local catalog and one continuously held legacy
// writer lock with the lifecycle, and maps browser project paths to the exact
// catalog keys this lane created. Only the created synthetic project has a key,
// so no other path can reach the author runtime.
type syntheticDurableOpen struct {
	mu        sync.Mutex
	library   *projectfs.Library
	catalog   *projectcatalog.Store
	lifecycle *authorruntime.ReadLifecycle
	manager   *projectruntime.Manager
	keys      map[string]string
	workspace string
	recovery  projectcatalog.RecoveryCheck
	closed    bool
}

// newSyntheticDurableOpen builds the read/write lifecycle and the transport
// adapter under the held legacy writer lock, exactly like newSyntheticReadOpen
// does for the read lane, and registers only the exactly-created synthetic
// project. The lifecycle takes ownership of the lock; Shutdown releases it.
func newSyntheticDurableOpen(library *projectfs.Library, libraryDir, projectPath string, config syntheticDurableConfig) (*syntheticDurableOpen, error) {
	if library == nil || library.Path() != libraryDir || !underTempRoot(libraryDir) || !privateSyntheticGlobalRoot(filepath.Dir(libraryDir)) || len(config.Schema) == 0 || config.Baseline == "" {
		return nil, errors.New("SYNTHETIC_DURABLE_CONFIGURATION_INVALID")
	}
	if filepath.Dir(projectPath) != libraryDir || filepath.Base(projectPath) != syntheticDurableProjectName {
		return nil, errors.New("SYNTHETIC_DURABLE_CONFIGURATION_INVALID")
	}
	workspace, err := os.MkdirTemp(os.TempDir(), "novel-durable-open-")
	if err != nil {
		return nil, err
	}
	remove := func() { _ = os.RemoveAll(workspace) }
	scratch := filepath.Join(workspace, "scratch")
	if err := os.Mkdir(scratch, 0700); err != nil {
		remove()
		return nil, err
	}
	dataRoot := filepath.Dir(libraryDir)
	held, err := writerlock.AcquireLegacy(dataRoot)
	if err != nil {
		remove()
		return nil, err
	}
	lockOwned := true
	defer func() {
		if lockOwned {
			_ = held.Close()
		}
	}()
	metadata := filepath.Join(dataRoot, syntheticCatalogDirectoryName)
	if err := ensureSyntheticCatalogDirectory(dataRoot, metadata); err != nil {
		remove()
		return nil, err
	}
	check, err := syntheticRecoveryCheck(libraryDir, held, nil)
	if err != nil {
		remove()
		return nil, err
	}
	catalog, err := projectcatalog.OpenWithRecoveryCheck(library, metadata, check)
	if err != nil {
		remove()
		return nil, err
	}
	lifecycle, err := authorruntime.NewReadLifecycleWithHeldLock(catalog, projectaccess.Config{
		DataRoot: dataRoot, LibraryRoot: libraryDir,
		ScratchParent: scratch, MaxDatabaseBytes: 64 << 20, Schema: config.Schema,
	}, held)
	if err != nil {
		remove()
		return nil, err
	}
	lockOwned = false
	fail := func(cause error) (*syntheticDurableOpen, error) {
		_ = lifecycle.Shutdown()
		remove()
		return nil, cause
	}
	manager, err := projectruntime.NewManager(lifecycle)
	if err != nil {
		return fail(err)
	}
	project, err := library.OpenProject(projectPath)
	if err != nil {
		return fail(err)
	}
	recent, registerErr := catalog.Register(project)
	closeErr := project.Close()
	if registerErr != nil {
		return fail(registerErr)
	}
	if closeErr != nil {
		return fail(closeErr)
	}
	return &syntheticDurableOpen{library: library, catalog: catalog, lifecycle: lifecycle,
		manager: manager, keys: map[string]string{projectPath: recent.Key}, workspace: workspace, recovery: check}, nil
}

func (s *syntheticDurableOpen) VerifyRecovery() error {
	if s == nil {
		return errors.New("SYNTHETIC_DURABLE_CLOSED")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.recovery == nil {
		return errors.New("SYNTHETIC_DURABLE_CLOSED")
	}
	return s.recovery(filepath.Join(s.library.Path(), ".go-recovery-probe"), syntheticRecoveryProbeID)
}

// OpenProject maps a browser project path to the exact process-local catalog
// key of the created synthetic project, then opens it for durable editing.
// The path is never passed to the runtime and never opens a filesystem target.
func (s *syntheticDurableOpen) OpenProject(ctx context.Context, caller transport.Caller, target any, requestToken string, requestedPath any) (any, error) {
	if s == nil || ctx == nil {
		return nil, errors.New("SYNTHETIC_DURABLE_CLOSED")
	}
	browserPath, ok := target.(string)
	if !ok || browserPath == "" {
		return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.manager == nil {
		return nil, errors.New("SYNTHETIC_DURABLE_CLOSED")
	}
	if err := s.recovery(filepath.Join(s.library.Path(), ".go-recovery-probe"), syntheticRecoveryProbeID); err != nil {
		return nil, err
	}
	key := s.keys[browserPath]
	if key == "" {
		return nil, security.NewHTTPError(http.StatusNotFound, "PROJECT_NOT_FOUND")
	}
	return s.manager.OpenProject(ctx, caller, key, requestToken, requestedPath)
}

func (s *syntheticDurableOpen) CloseProject(ctx context.Context, caller transport.Caller, projectPath string, expected any) (any, error) {
	if s == nil || ctx == nil {
		return nil, errors.New("SYNTHETIC_DURABLE_CLOSED")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.manager == nil {
		return nil, errors.New("SYNTHETIC_DURABLE_CLOSED")
	}
	return s.manager.CloseProject(ctx, caller, projectPath, expected)
}

func (s *syntheticDurableOpen) ProjectRuntimeContext(ctx context.Context, caller transport.Caller) (any, error) {
	if s == nil || ctx == nil {
		return nil, errors.New("SYNTHETIC_DURABLE_CLOSED")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.manager == nil {
		return nil, errors.New("SYNTHETIC_DURABLE_CLOSED")
	}
	return s.manager.ProjectRuntimeContext(ctx, caller)
}

func (s *syntheticDurableOpen) SaveProject(ctx context.Context, caller transport.Caller, projectID string, patch map[string]any, expected any) (any, error) {
	if s == nil || ctx == nil {
		return nil, errors.New("SYNTHETIC_DURABLE_CLOSED")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.manager == nil {
		return nil, errors.New("SYNTHETIC_DURABLE_CLOSED")
	}
	return s.manager.SaveProject(ctx, caller, projectID, patch, expected)
}

func (s *syntheticDurableOpen) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	if err := s.lifecycle.Shutdown(); err != nil {
		// An admitted write handle may still own files in this workspace; the
		// legacy lock stays held while the process-level recovery cleans them up.
		return err
	}
	if err := os.RemoveAll(s.workspace); err != nil {
		return err
	}
	s.closed = true
	return nil
}

// migrationHandlerWithDurable mounts the browser project lifecycle protocol
// over the durable Go author runtime for one real synthetic project. It binds
// only BindProjects for the project domain: the read-only project binder is
// never installed, so project:open cannot be double-bound here.
func migrationHandlerWithDurable(registry *policy.Registry, address, staticDir, durableDir string, config syntheticDurableConfig) (http.Handler, func(), error) {
	if durableDir == "" || !underTempRoot(durableDir) || !privateSyntheticGlobalRoot(filepath.Dir(durableDir)) {
		return nil, nil, errors.New("synthetic durable project requires a library beneath a private temporary data root")
	}
	if len(config.Schema) == 0 || config.Baseline == "" {
		return nil, nil, errors.New("synthetic durable project requires the trusted schema contract and baseline database")
	}
	projectPath, err := createSyntheticDurableProject(durableDir, config.Baseline)
	if err != nil {
		return nil, nil, fmt.Errorf("synthetic durable project creation: %w", err)
	}
	server := transport.New(registry, security.NewOrigins("http://"+address), "go-durable-uncommitted")
	if err := server.BindNavigation(); err != nil {
		server.Close()
		return nil, nil, err
	}
	var library *projectfs.Library
	var durable *syntheticDurableOpen
	cleanup := func() {
		server.Close()
		if durable != nil {
			_ = durable.Close()
		}
		if library != nil {
			_ = library.Close()
		}
	}
	library, err = projectfs.OpenLibrary(durableDir)
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("synthetic durable library: %w", err)
	}
	durable, err = newSyntheticDurableOpen(library, durableDir, projectPath, config)
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("synthetic durable runtime: %w", err)
	}
	if err := server.BindProjects(durable); err != nil {
		cleanup()
		return nil, nil, err
	}
	mux := http.NewServeMux()
	mux.Handle("/api/", server)
	mux.Handle("/api", server)
	mux.Handle("/healthz", server)
	route, err := transport.NewLibraryRoute(server, library, 4096)
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	mux.Handle("/api/library", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := durable.VerifyRecovery(); err != nil {
			http.Error(w, "PROJECT_CATALOG_RECOVERY_PENDING", http.StatusServiceUnavailable)
			return
		}
		route.ServeHTTP(w, r)
	}))
	if staticDir != "" {
		static, err := transport.NewStaticAssets(staticDir, server.SessionStore())
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