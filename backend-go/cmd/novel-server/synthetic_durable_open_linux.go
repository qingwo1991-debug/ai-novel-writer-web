//go:build linux

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"

	_ "github.com/mattn/go-sqlite3" // Registers the database/sql driver "sqlite3".
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/admission"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/authorruntime"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/generation"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/policy"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectaccess"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectcatalog"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectruntime"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
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
	// GlobalRoot and ModelOrigins are the private trusted model config root
	// and the administrator-approved egress origins for the reduced S1
	// generation lane. Both-or-neither; when both are set the durable lane
	// additionally binds the eleven S1 generation operations.
	GlobalRoot   string
	ModelOrigins []string
	// Dispatch is the executor seam used by tests to substitute the native
	// model dispatch; production main.go never sets it.
	Dispatch generation.Dispatch
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
// runtime. It embeds the shared read surface (so editor reads are served from
// the same durable project) and adds the write runtime plus the held legacy
// lock and read configuration needed to open a short-lived verified reader
// per read request. Only the created synthetic project has a key, so no other
// path can reach the author runtime.
type syntheticDurableOpen struct {
	syntheticReadSurface
	manager    *projectruntime.Manager
	held       *writerlock.Lock
	readConfig projectaccess.Config
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
	readConfig := projectaccess.Config{DataRoot: dataRoot, LibraryRoot: libraryDir,
		ScratchParent: scratch, MaxDatabaseBytes: 64 << 20, Schema: config.Schema}
	lifecycle, err := authorruntime.NewReadLifecycleWithHeldLock(catalog, readConfig, held)
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
	result := &syntheticDurableOpen{syntheticReadSurface: syntheticReadSurface{
		library: library, catalog: catalog, lifecycle: lifecycle,
		keys: map[string]string{projectPath: recent.Key}, workspace: workspace, recovery: check,
	}, manager: manager, held: held, readConfig: readConfig}
	result.readTx = result.readAuthorTx
	return result, nil
}

// readAuthorTx is the durable lane's transaction source. The durable write
// admission cannot serve read-only transactions, and the coordinator forbids a
// second read-only admission for the same project path, so every editor read
// opens a short-lived verified reader for the exact project under the still
// held legacy writer lock, guarded by the live durable lease from the same
// lifecycle. The reader's source proof is re-verified on every read against
// the same library files the write handle holds.
func (s *syntheticDurableOpen) readAuthorTx(ctx context.Context, caller transport.Caller, expected projectsession.Context, operation func(*sql.Tx) error) error {
	if s == nil || ctx == nil || s.held == nil {
		return errors.New("SYNTHETIC_DURABLE_CLOSED")
	}
	owner := projectsession.Owner{SessionID: caller.SessionID, ClientID: caller.ClientID}
	current, present, err := s.lifecycle.CurrentProjectDurable(ctx, owner)
	if err != nil {
		return err
	}
	if !present || current != expected {
		return errors.New("PROJECT_SESSION_STALE")
	}
	key := s.keys[expected.ProjectPath]
	if key == "" {
		return errors.New("PROJECT_NOT_OPEN")
	}
	config := s.readConfig
	config.ProjectPath = expected.ProjectPath
	reader, err := authorruntime.OpenReadOnlyUnderLock(ctx, s.held, s.catalog, key, config)
	if err != nil {
		return err
	}
	readErr := reader.Read(ctx, operation)
	closeErr := reader.Close()
	return errors.Join(readErr, closeErr)
}

// ReadCharactersJSON is the durable lane's character read. The shared surface
// method needs the read-only admission's private snapshot, so this lane runs
// the three character reads directly on the admitted author database under the
// same frozen cookie/tab/lease fence as project:save; the exclusive legacy
// writer lock leaves no other writer to conflict with BEGIN IMMEDIATE.
func (s *syntheticDurableOpen) ReadCharactersJSON(ctx context.Context, caller transport.Caller, raw any, operation string) (json.RawMessage, error) {
	if s == nil || ctx == nil {
		return nil, errors.New("SYNTHETIC_DURABLE_CLOSED")
	}
	expected, err := projectsession.ParseContext(raw)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.lifecycle == nil {
		return nil, errors.New("SYNTHETIC_DURABLE_CLOSED")
	}
	return s.lifecycle.ReadCharactersJSONDurable(ctx, projectsession.Owner{SessionID: caller.SessionID, ClientID: caller.ClientID}, expected, operation)
}

// CommitDraftContent is the durable lane's draft body CAS. It runs the CAS
// directly on the admitted author database and rotates the frozen lease in
// place, so the same admittedAuthor handle stays installed and the durable
// write session is never downgraded to a read-only handle. The catalog key is
// unchanged because this lane edits the author's own tree instead of switching
// sources.
func (s *syntheticDurableOpen) CommitDraftContent(ctx context.Context, caller transport.Caller, request transport.DraftLiveWriteRequest) (transport.DraftLiveWriteAck, error) {
	var zero transport.DraftLiveWriteAck
	if s == nil || ctx == nil {
		return zero, errors.New("SYNTHETIC_DURABLE_CLOSED")
	}
	expected, err := projectsession.ParseContext(request.Expected)
	if err != nil {
		return zero, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.lifecycle == nil {
		return zero, errors.New("SYNTHETIC_DURABLE_CLOSED")
	}
	owner := projectsession.Owner{SessionID: caller.SessionID, ClientID: caller.ClientID}
	baseline := request.ExpectedContent
	fresh, err := s.lifecycle.CommitDraftCASDurable(ctx, owner, expected, request.DraftID, request.Content, request.WordCount, &baseline)
	if err != nil {
		return zero, err
	}
	if fresh.ProjectID != expected.ProjectID || fresh.ProjectPath != expected.ProjectPath ||
		fresh.LeaseID == "" || fresh.LeaseID == expected.LeaseID {
		return zero, errors.New("SYNTHETIC_COMMIT_RESULT_INVALID")
	}
	return transport.DraftLiveWriteAck{ProjectSession: map[string]any{
		"projectId":   fresh.ProjectID,
		"projectPath": fresh.ProjectPath,
		"leaseId":     fresh.LeaseID,
	}}, nil
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

// migrationHandlerWithDurable mounts the browser project lifecycle protocol
// over the durable Go author runtime for one real synthetic project. BindProjects
// binds the write domain (project:open, db:close, project:get-runtime-context,
// project:save, project:update-config) plus project:recent-list only when the
// provider implements ProjectCatalogProvider — syntheticDurableOpen does not.
// The disjoint binders below are the only read/write providers installed, so no
// operation is ever double-bound and the read-only project binder (which would
// rebind project:open) is never installed here. Character reads and the draft
// body CAS are served by syntheticDurableOpen's own overrides against the
// admitted author database under the same frozen lease as project:save.
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
	// Disjoint read binders: none of these operations overlap the write domain
	// bound by BindProjects above (project:open, db:close, project:save,
	// project:update-config, project:get-runtime-context). The read-only
	// project/open and project/close binders are deliberately skipped: they
	// would rebind project:open and db:close and fail OPERATION_ALREADY_BOUND.
	for _, bind := range []func() error{
		func() error { return server.BindCharacterLiveReads(durable) },
		func() error { return server.BindDraftLiveReads(durable) },
		func() error { return server.BindDraftLiveWrites(durable) },
		func() error { return server.BindEditorNavigation(durable) },
		func() error { return server.BindCurrentOverview(durable) },
		func() error { return server.BindEditorCheckpoint(durable) },
		func() error { return server.BindEditorStageReads(durable) },
		func() error { return server.BindEditorReviewReads(durable) },
		func() error { return server.BindEditorReviewFullReads(durable) },
		func() error { return server.BindEditorReviewCycleAbsentRead(durable) },
	} {
		if err := bind(); err != nil {
			cleanup()
			return nil, nil, err
		}
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
