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
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3" // Registers the database/sql driver "sqlite3".
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/admission"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/authorruntime"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/egress"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/generation"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/globalconfig"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/policy"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectaccess"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectcatalog"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectruntime"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/promptskills"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
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
	// prompts is the pinned private prompt and installed-skill root. It is nil
	// when the lane was started without the trusted config root, and the two
	// prompt/skill reads are then never bound at all.
	prompts *promptskills.Service
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
	// The prompt template and installed-skill reads share the same private
	// trusted config root as the model profiles. That root is pinned by its
	// owner (absolute, current UID, no symlink, not group/other writable) and
	// never reaches the browser; a lane without it binds neither read.
	var prompts *promptskills.Service
	if config.GlobalRoot != "" {
		prompts, err = promptskills.New(config.GlobalRoot)
		if err != nil {
			return fail(err)
		}
	}
	result := &syntheticDurableOpen{syntheticReadSurface: syntheticReadSurface{
		library: library, catalog: catalog, lifecycle: lifecycle,
		keys: map[string]string{projectPath: recent.Key}, workspace: workspace, recovery: check,
	}, manager: manager, held: held, readConfig: readConfig, prompts: prompts}
	result.readTx = result.readAuthorTx
	return result, nil
}

// Close releases the shared read surface and the pinned private prompt and
// skill root this lane owns, so the root descriptor is released with the rest
// of the lane instead of outliving it.
func (s *syntheticDurableOpen) Close() error {
	if s == nil {
		return nil
	}
	err := s.syntheticReadSurface.Close()
	if s.prompts != nil {
		err = errors.Join(err, s.prompts.Close())
	}
	return err
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

// CommitCharacterRoster is the durable lane's db:character-roster-commit. The
// roster author pipeline (identity commit, compatibility prose, projection
// refresh, roster-meta bump and readback) runs on the admitted author database
// under the same frozen cookie/tab/lease fence as every other durable write, so
// it borrows one handle, re-observes that same context from the lifecycle that
// admitted it, and never names a database path.
//
// The author scope's project is the admitted project and its epoch is the admitted
// lease: the identity approval records which frozen session authorized the roster,
// and the lease is exactly the value that changes when that session is replaced.
func (s *syntheticDurableOpen) CommitCharacterRoster(ctx context.Context, caller transport.Caller, request map[string]any, raw any) (map[string]any, error) {
	if s == nil || ctx == nil {
		return nil, errors.New("SYNTHETIC_DURABLE_CLOSED")
	}
	expected, err := projectsession.ParseContext(raw)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	lifecycle := s.lifecycle
	closed := s.closed
	s.mu.Unlock()
	if closed || lifecycle == nil {
		return nil, errors.New("SYNTHETIC_DURABLE_CLOSED")
	}
	db, release, err := s.beginAuthorRepository(ctx, caller, raw)
	if err != nil {
		return nil, err
	}
	defer release()
	owner := projectsession.Owner{SessionID: caller.SessionID, ClientID: caller.ClientID}
	authorize := func() error {
		current, present, err := lifecycle.CurrentProjectDurable(ctx, owner)
		if err != nil {
			return err
		}
		if !present || current != expected {
			return errors.New("PROJECT_SESSION_STALE")
		}
		return nil
	}
	scope := store.RosterAuthorScope{ProjectID: expected.ProjectID, Epoch: expected.LeaseID}
	return store.NewRosterAuthor(db).CommitAuthorRoster(ctx, request, scope, authorize)
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

// beginAuthorRepository admits the author project for this exact session, tab
// and frozen context and returns the author's database plus a release that
// refreshes the permit before the fence is observed again. Borrowing
// repositories write the author's own database, never a renderer path.
func (s *syntheticDurableOpen) beginAuthorRepository(ctx context.Context, caller transport.Caller, raw any) (*sql.DB, func(), error) {
	if s == nil || ctx == nil {
		return nil, nil, errors.New("SYNTHETIC_DURABLE_CLOSED")
	}
	expected, err := projectsession.ParseContext(raw)
	if err != nil {
		return nil, nil, err
	}
	s.mu.Lock()
	if s.closed || s.lifecycle == nil {
		s.mu.Unlock()
		return nil, nil, errors.New("SYNTHETIC_DURABLE_CLOSED")
	}
	lifecycle := s.lifecycle
	s.mu.Unlock()
	db, refresh, done, err := lifecycle.DurableRepository(ctx, projectsession.Owner{SessionID: caller.SessionID, ClientID: caller.ClientID}, expected)
	if err != nil {
		return nil, nil, err
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			refreshCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = refresh(refreshCtx)
			done()
		})
	}
	return db, release, nil
}

// BeginRecovery is the durable lane's recovery candidate repository.
func (s *syntheticDurableOpen) BeginRecovery(ctx context.Context, caller transport.Caller, raw any) (*store.Recovery, func(), error) {
	db, release, err := s.beginAuthorRepository(ctx, caller, raw)
	if err != nil {
		return nil, nil, err
	}
	return store.NewRecovery(db), release, nil
}

// BeginSummary is the durable lane's continuity and summary repository.
func (s *syntheticDurableOpen) BeginSummary(ctx context.Context, caller transport.Caller, raw any) (*store.Summary, func(), error) {
	db, release, err := s.beginAuthorRepository(ctx, caller, raw)
	if err != nil {
		return nil, nil, err
	}
	return store.NewSummary(db), release, nil
}

// BeginDrafts is the durable lane's draft repository for the six names the live
// draft binders leave unbound: the three self-contained author mutations
// (db:draft-create, db:draft-update-status, db:draft-delete) and the three
// authoritative export reads (db:draft-authority-sequence,
// db:draft-export-snapshot, db:draft-export-authority-current). All six are
// plain SQLite work on the admitted author database: none needs a generation
// owner, a source project, LanceDB or a filesystem path, and none rotates the
// frozen lease, because the original never changed the session for them. The
// draft body CAS keeps its own override (CommitDraftContent) and the eight live
// reads keep BindDraftLiveReads, so nothing here is bound twice.
func (s *syntheticDurableOpen) BeginDrafts(ctx context.Context, caller transport.Caller, raw any) (*store.Drafts, func(), error) {
	db, release, err := s.beginAuthorRepository(ctx, caller, raw)
	if err != nil {
		return nil, nil, err
	}
	return store.NewDrafts(db), release, nil
}

// BeginReviewRevisions is the durable lane's review and revision repository.
func (s *syntheticDurableOpen) BeginReviewRevisions(ctx context.Context, caller transport.Caller, raw any) (*store.ReviewRevisions, func(), error) {
	db, release, err := s.beginAuthorRepository(ctx, caller, raw)
	if err != nil {
		return nil, nil, err
	}
	return store.NewReviewRevisions(db), release, nil
}

// BeginFinalization is the durable lane's finalization-outbox repository. The
// three names bound to it (finalization:commit, finalization:retry and
// db:finalization-link-knowledge-document) freeze the author's confirmed prose
// and the outbox row in one IMMEDIATE transaction on the admitted author
// database, so the durable session is never downgraded and the catalog key is
// unchanged. The scope is the frozen project and the frozen lease, and the
// verifier below refuses every mutation the transport did not authorize for this
// exact request.
//
// This lane owns no manuscript publisher, so markPublished can never be reached:
// no file is written, no path is resolved and no filesystem collision is
// scanned. BindFinalization therefore binds exactly three names and leaves the
// publication-published receipt unminted rather than fabricated.
func (s *syntheticDurableOpen) BeginFinalization(ctx context.Context, caller transport.Caller, raw any) (*store.FinalizationOutbox, func(), error) {
	expected, err := projectsession.ParseContext(raw)
	if err != nil {
		return nil, nil, err
	}
	db, release, err := s.beginAuthorRepository(ctx, caller, raw)
	if err != nil {
		return nil, nil, err
	}
	scope := store.FinalizationScope{ProjectID: expected.ProjectID, Epoch: expected.LeaseID}
	return store.NewFinalizationOutbox(db, scope, durableFinalizationOwner{expected: expected}), release, nil
}

// BeginImportRuns is the durable lane's import-run bookkeeping repository. The
// ten bound names read and advance the stage/lease/checkpoint rows an earlier
// app version already wrote (db:import-run-get, -list-resumable, -list-chapters,
// -start-resume, -renew-execution, -request-cancel, -cancel-at-boundary,
// -complete-batch, -advance-stage, -complete). Every one of them is plain SQLite
// work on the admitted author database: none opens a source file, a knowledge
// store or a generation graph, and none rotates the frozen lease.
//
// Run creation stays out of scope on purpose. prepare-inspection, author-preview,
// finalize-parsing and restart mint the run rows themselves and need the tab-bound
// in-memory inspection owner or the generation-source proof; the effect receipts
// need authoritative readback from the global-facts, blueprint-range and
// finalized-draft repositories. Those ten names therefore keep failing closed at
// GO_OPERATION_NOT_MIGRATED instead of being approximated here.
func (s *syntheticDurableOpen) BeginImportRuns(ctx context.Context, caller transport.Caller, raw any) (*store.ImportRuns, func(), error) {
	db, release, err := s.beginAuthorRepository(ctx, caller, raw)
	if err != nil {
		return nil, nil, err
	}
	return store.NewImportRuns(db), release, nil
}

// durableFinalizationOwner is the trusted owner proof verifier of that
// repository. It binds the opaque proof to this exact captured project and lease
// and refuses any mutation whose action or frozen values differ from the ones the
// transport authorized for this request, so a renderer cannot reach the store
// through a fabricated proof (store/finalization_outbox.go:63-82). It needs no
// second connection: the store re-reads the outbox row and re-verifies the frozen
// content hash inside the same IMMEDIATE transaction before it calls this
// verifier (:179-190, :294-305), and its own CAS repeats both facts in the UPDATE.
//
// The one receipt this verifier cannot prove is a managed knowledge-base
// document: the knowledge base is not part of this lane's admitted database, so
// the transport freezes the exact identity the request carried and this verifier
// refuses any other. It never mints one.
type durableFinalizationOwner struct{ expected projectsession.Context }

func (o durableFinalizationOwner) VerifyFinalizationOwnerProof(_ context.Context, _ *sql.Conn, scope store.FinalizationScope, mutation store.FinalizationMutation, proof any) error {
	authorized, ok := proof.(transport.FinalizationProof)
	if !ok {
		return store.ErrFinalizationOwnerRequired
	}
	if scope.ProjectID != o.expected.ProjectID || scope.Epoch != o.expected.LeaseID ||
		authorized.ProjectID != o.expected.ProjectID || authorized.LeaseID != o.expected.LeaseID ||
		authorized.ProjectPath != o.expected.ProjectPath || mutation.Action != authorized.Action {
		return store.ErrFinalizationOwnerRequired
	}
	switch authorized.Action {
	case "commit":
		if mutation.Input.DraftID != authorized.DraftID ||
			mutation.Input.FinalizationID != authorized.FinalizationID ||
			mutation.Input.ContentHash != authorized.ContentHash {
			return store.ErrFinalizationOwnerRequired
		}
	case "knowledge-link":
		if mutation.Existing == nil || mutation.Existing.DraftID != authorized.DraftID ||
			mutation.Existing.FinalizationID != authorized.FinalizationID ||
			mutation.Existing.ContentHash != authorized.ContentHash ||
			mutation.KnowledgeDocumentID != authorized.KnowledgeDocumentID {
			return store.ErrFinalizationOwnerRequired
		}
	case "publication-pending":
		if mutation.Existing == nil || mutation.Existing.FinalizationID != authorized.FinalizationID ||
			mutation.Existing.ContentHash != authorized.ContentHash ||
			mutation.LastError != authorized.LastError {
			return store.ErrFinalizationOwnerRequired
		}
	default:
		// publication-published needs the real publisher receipt this lane
		// cannot produce, so it is refused here as well as being unreachable.
		return store.ErrFinalizationOwnerRequired
	}
	return nil
}

// BeginPostHistory is the durable lane's post-process receipt and LLM call
// ledger repository. All nine names in that family are bound to it: the five
// reads by BindPostHistoryReads and the four mutations (create-run,
// mark-step-ok, mark-step-failed, log-llm-call) by BindPostHistoryWrites. The
// repository borrows the admitted author database and never a path; its writes
// commit on that handle and the permit is refreshed on release, exactly like the
// draft CAS.
func (s *syntheticDurableOpen) BeginPostHistory(ctx context.Context, caller transport.Caller, raw any) (*store.PostHistory, func(), error) {
	db, release, err := s.beginAuthorRepository(ctx, caller, raw)
	if err != nil {
		return nil, nil, err
	}
	return store.NewPostHistory(db), release, nil
}

// BeginNarrative is the durable lane's narrative thread and consistency
// exemption repository. The three reads (thread list, relevant thread list,
// exemption list) are bound by BindNarrativeReads, the five mutating plan and
// exemption names (plan-create, plan-update, plan-delete, exemption-save,
// exemption-revoke) by BindNarrativeWrites, and db:narrative-thread-event-confirm
// by BindNarrativeEventConfirm, so this family is served completely. Both halves
// borrow the admitted author database and never a path; a write commits on that
// handle and the permit is refreshed on release, exactly like the draft CAS and
// the post-process writes.
func (s *syntheticDurableOpen) BeginNarrative(ctx context.Context, caller transport.Caller, raw any) (*store.Narrative, func(), error) {
	db, release, err := s.beginAuthorRepository(ctx, caller, raw)
	if err != nil {
		return nil, nil, err
	}
	return store.NewNarrative(db), release, nil
}

// BeginBlueprints is the durable lane's blueprint and character-sync
// repository. The three reads (chapter blueprint get, character-sync get,
// pending character-sync list) are bound by BindBlueprintReads and the four
// mutating names a single store method already expresses (upsert, upsert-many,
// delete, commit-range) by BindBlueprintWrites, so this family is served except
// db:blueprint-get-all, which is already served by the editor stage read
// binder, and db:blueprint-update-notes, which stays unbound because the
// original refuses it unconditionally. Both halves borrow the admitted author
// database and never a path; a write commits on that handle and the permit is
// refreshed on release, exactly like the draft CAS and the other write binders.
func (s *syntheticDurableOpen) BeginBlueprints(ctx context.Context, caller transport.Caller, raw any) (*store.Blueprints, func(), error) {
	db, release, err := s.beginAuthorRepository(ctx, caller, raw)
	if err != nil {
		return nil, nil, err
	}
	return store.NewBlueprints(db), release, nil
}

// BeginProjectCore is the durable lane's project core repository for the two
// mutating names BindProjectCoreWrites binds (db:project-core-update and
// db:project-core-synopsis-commit). db:project-core-get is already served by
// BindEditorStageReads, so this borrow never carries a read. A write commits on
// the admitted author database and the permit is refreshed on release, exactly
// like the draft CAS, the blueprint writes and the other write binders.
func (s *syntheticDurableOpen) BeginProjectCore(ctx context.Context, caller transport.Caller, raw any) (*store.ProjectCore, func(), error) {
	db, release, err := s.beginAuthorRepository(ctx, caller, raw)
	if err != nil {
		return nil, nil, err
	}
	return store.NewProjectCore(db), release, nil
}

// BeginPlotTree is the durable lane's plot tree repository. The single read
// (db:plot-tree-read) is bound to it by BindPlotTreeReads and the one mutation
// the store expresses as a plain write (db:plot-tree-clear) by
// BindPlotTreeClear; db:plot-tree-save stays unbound because the original
// refuses it unconditionally, so this lane can never replace the stored
// snapshot. The repository borrows the admitted author database and never a
// path; the clear commits on that handle and the permit is refreshed on release,
// exactly like the other write binders.
func (s *syntheticDurableOpen) BeginPlotTree(ctx context.Context, caller transport.Caller, raw any) (*store.PlotTree, func(), error) {
	db, release, err := s.beginAuthorRepository(ctx, caller, raw)
	if err != nil {
		return nil, nil, err
	}
	return store.NewPlotTree(db), release, nil
}

// BeginPromptSkills lends the durable lane's pinned private prompt and
// installed-skill root. The read binder serves prompt:load-global and
// skills:list-user from it; the disjoint write binder serves
// prompt:save-global, prompt:delete-global and skills:uninstall-user from the
// same service. None of those five names carries a frozen session context, so
// unlike the project-family borrows there is no lease to recheck; the service
// itself is the only authority and it refuses any path that leaves the root.
func (s *syntheticDurableOpen) BeginPromptSkills(ctx context.Context, _ transport.Caller) (*promptskills.Service, func(), error) {
	if s == nil || ctx == nil {
		return nil, nil, errors.New("SYNTHETIC_DURABLE_CLOSED")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.prompts == nil {
		return nil, nil, errors.New("PROMPT_SKILLS_ROOT_UNAVAILABLE")
	}
	return s.prompts, func() {}, nil
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

// RemoveRecentNavigation is the durable lane's project:recent-remove. The
// browser names the shelf entry with the path project:recent-list handed it, so
// this method maps that path to the process-local catalog key this lane already
// issued and lets projectcatalog drop the navigation record. The catalog never
// deletes a project directory, and this method never opens, creates or reads a
// filesystem path of its own.
//
// The admitted runtime is intentionally left alone: the original drops a
// recent-list entry and revokes preview capabilities, but it does not close the
// project the author is editing (project-controller.ts:896-904). The
// path-to-key mapping this lane issued at creation is kept for the same reason
// — the key belongs to the admitted runtime, not to the shelf — so a later
// project:open on a removed path is refused through the catalog's redacted open
// error instead of minting a fresh capability. A refusal here is symbolic,
// because a catalog error can carry a host path.
func (s *syntheticDurableOpen) RemoveRecentNavigation(ctx context.Context, _ transport.Caller, target string) (any, error) {
	if s == nil || ctx == nil {
		return nil, errors.New("SYNTHETIC_DURABLE_CLOSED")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.catalog == nil {
		return nil, errors.New("SYNTHETIC_DURABLE_CLOSED")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key := s.keys[target]
	if key == "" {
		return nil, errors.New("PROJECT_RECENT_TARGET_INVALID")
	}
	if err := s.catalog.Remove(key); err != nil {
		return nil, err
	}
	return map[string]any{"success": true}, nil
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
	// models and endpoints own descriptors on the pinned private global root and
	// on the administrator approved egress policy; both are released with the
	// rest of the lane.
	var models *globalconfig.Service
	var endpoints *egress.Client
	cleanup := func() {
		server.Close()
		if durable != nil {
			_ = durable.Close()
		}
		if library != nil {
			_ = library.Close()
		}
		if endpoints != nil {
			endpoints.Close()
		}
		if models != nil {
			_ = models.Close()
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
	// project:recent-remove is the one project-family name BindProjects leaves
	// unbound (its optional ProjectCatalogProvider needs both recent methods,
	// and this lane serves the shelf through BindEditorNavigation's
	// project:recent-list instead). Dropping a shelf entry is a navigation
	// metadata write on this lane's own catalog: no author database, no
	// filesystem path and no frozen lease, so it is bound on its own.
	if err := server.BindProjectRecentRemove(durable); err != nil {
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
		// The rest of the draft family is plain SQLite on that same admitted
		// author database: three self-contained mutations (create, update-status,
		// delete) through the same borrow-and-refresh permit path, and the three
		// authoritative export reads. None of them is in the live read row set or
		// the body CAS above, so the family is served completely except the
		// import batch, which belongs to the import pipeline.
		func() error { return server.BindDraftAuthorWrites(durable) },
		func() error { return server.BindDraftAuthorityReads(durable) },
		func() error { return server.BindEditorNavigation(durable) },
		func() error { return server.BindCurrentOverview(durable) },
		func() error { return server.BindEditorCheckpoint(durable) },
		func() error { return server.BindEditorStageReads(durable) },
		func() error { return server.BindEditorReviewReads(durable) },
		func() error { return server.BindEditorReviewFullReads(durable) },
		func() error { return server.BindEditorReviewCycleAbsentRead(durable) },
		// Post-process receipt and LLM ledger reads run on the same admitted
		// author database. The four mutating names in that family are bound
		// separately just below, so this family is served completely.
		func() error { return server.BindPostHistoryReads(durable) },
		// The four mutating names write that same admitted author database
		// through the same borrow-and-refresh permit path the draft CAS uses:
		// one admitted handle per request, refreshed before the fence is
		// observed again, never a database path.
		func() error { return server.BindPostHistoryWrites(durable) },
		// Narrative thread and consistency exemption reads run on the same
		// admitted author database.
		func() error { return server.BindNarrativeReads(durable) },
		// The five mutating plan and exemption names write that same admitted
		// author database through the same borrow-and-refresh permit path the
		// draft CAS and the post-process writes use: one admitted handle per
		// request, refreshed before the fence is observed again, never a
		// database path. db:narrative-thread-event-confirm is not one of them
		// (its refusal is a transport error, not an `Error: CODE` result), so
		// it is bound separately just below and the family is served completely.
		func() error { return server.BindNarrativeWrites(durable) },
		// Thread confirmation is an ordinary author write on that same admitted
		// author database: store.ConfirmEvent re-reads the frozen finalization
		// outbox itself and binds the evidence to that prose, so no generation
		// graph admission is composed and no confirmation authority is invented.
		func() error { return server.BindNarrativeEventConfirm(durable) },
		// Blueprint and character-sync reads run on the same admitted author
		// database. This binds only the three reads; db:blueprint-get-all is
		// already bound by BindEditorStageReads above, so it must not be bound
		// here as well.
		func() error { return server.BindBlueprintReads(durable) },
		// The four mutating blueprint names write that same admitted author
		// database through the same borrow-and-refresh permit path the draft CAS
		// and the other write binders use: one admitted handle per request,
		// refreshed before the fence is observed again, never a database path.
		// db:blueprint-update-notes is not among them: the original refuses it
		// unconditionally, so it stays unbound and keeps failing closed.
		func() error { return server.BindBlueprintWrites(durable) },
		// The two mutating project core names write that same admitted author
		// database through the same borrow-and-refresh permit path: one
		// admitted handle per request, refreshed before the fence is observed
		// again, never a database path. db:project-core-get is already bound by
		// BindEditorStageReads above, so BindProjectCoreWrites binds only the
		// two writes.
		func() error { return server.BindProjectCoreWrites(durable) },
		// db:character-roster-commit is the one author-facing character write:
		// store.RosterAuthor runs the original's identity commit, prose and state
		// writes, projection refresh, roster-meta bump and readback on that same
		// admitted author database, through the same borrow-and-refresh path and
		// under a re-observed lease. The three character reads stay with
		// BindCharacterLiveReads above, so nothing is bound twice.
		func() error { return server.BindCharacterRosterCommit(durable) },
		// The plot tree read runs on the same admitted author database. This
		// binds only db:plot-tree-read; db:plot-tree-save stays unbound because
		// the original refuses it unconditionally and it keeps failing closed.
		func() error { return server.BindPlotTreeReads(durable) },
		// db:plot-tree-clear is a one-statement mutation of that same admitted
		// database (it blanks the stored snapshot and needs no source-revision
		// CAS), so it is bound on its own, with the original's transport-error
		// refusal envelope instead of the save name's `Error: CODE` result.
		func() error { return server.BindPlotTreeClear(durable) },
	} {
		if err := bind(); err != nil {
			cleanup()
			return nil, nil, err
		}
	}
	// The reduced S1 generation lane binds exactly the eleven migrated
	// generation-family names; every other generation-family operation stays
	// GO_OPERATION_NOT_MIGRATED. It is only wired when an administrator
	// supplies the private trusted model config root (with egress origins).
	if config.GlobalRoot != "" {
		provider, err := newSyntheticGeneration(durable, server, config)
		if err != nil {
			cleanup()
			return nil, nil, fmt.Errorf("synthetic generation lane: %w", err)
		}
		binder := &transport.GenerationProvider{
			Begin:                  provider.Begin,
			BindMaterialDecision:   provider.BindMaterialDecision,
			Execute:                provider.Execute,
			Read:                   provider.Read,
			List:                   provider.List,
			Pause:                  provider.Pause,
			Cancel:                 provider.Cancel,
			Resume:                 provider.Resume,
			Restart:                provider.Restart,
			DiscardCandidate:       provider.DiscardCandidate,
			RetryDraftShortOutline: provider.RetryDraftShortOutline,
		}
		if err := server.BindGeneration(binder); err != nil {
			cleanup()
			return nil, nil, err
		}
		// The graph family shares the same durable admission but not the same
		// request shape: its channels carry `{ handle }` and the finalization slot
		// read carries `{ slot }`, so it binds through its own row set on top of
		// the same adapter. Only the two reads are bound here; finalization begin,
		// execute and commit stay 501 because they need the slot context capture
		// and the model dispatch this lane does not have.
		if err := server.BindGraphGeneration(&transport.GraphGenerationProvider{
			Read:             provider.GraphRead,
			FinalizationRead: provider.FinalizationRead,
		}); err != nil {
			cleanup()
			return nil, nil, err
		}
	}
	// The protected global config, startup snapshot and private model store are
	// served from that same pinned private root, so they are wired by the same
	// condition. The startup capture mirrors the trusted claim the synthetic
	// model lane already makes for an administrator supplied private root
	// (main.go:287-291, StartupCapture{Generation:"synthetic-model-config-v1"}):
	// the root and its capture are host configuration, never request input, and
	// the appearance acknowledgement is scoped to that generation. Model saves
	// keep their own administrator approved egress policy.
	if config.GlobalRoot != "" {
		models, err = globalconfig.New(config.GlobalRoot)
		if err != nil {
			cleanup()
			return nil, nil, fmt.Errorf("synthetic durable global config: %w", err)
		}
		endpoints, err = egress.New(config.ModelOrigins)
		if err != nil {
			cleanup()
			return nil, nil, fmt.Errorf("synthetic durable model origins: %w", err)
		}
		startup := globalconfig.NewStartup(globalconfig.StartupCapture{Generation: "synthetic-model-config-v1"})
		for _, bind := range []func() error{
			func() error { return server.BindGlobalConfig(models, startup) },
			func() error { return server.BindModelReads(models, startup) },
			func() error { return server.BindModelSave(models, startup, endpoints) },
			func() error { return server.BindModelDelete(models, startup) },
			// The six model-routing preference names are config.json reads and
			// single-document writes only: no provider dispatch, no egress
			// policy, no execution lease. They therefore share this root's
			// condition and stay out of the generation lane below.
			func() error { return server.BindModelRouting(models, startup) },
		} {
			if err := bind(); err != nil {
				cleanup()
				return nil, nil, fmt.Errorf("synthetic durable model binding: %w", err)
			}
		}
	}
	// The prompt template and installed-skill names are served from that same
	// pinned private config root, so they are wired by the same condition. The
	// read binder carries prompt:load-global and skills:list-user; the disjoint
	// write binder carries prompt:save-global, prompt:delete-global and
	// skills:uninstall-user. The remote and local install names stay unbound:
	// they need GitHub egress or a native file picker this lane does not own.
	if config.GlobalRoot != "" {
		if err := server.BindPromptSkillsReads(durable); err != nil {
			cleanup()
			return nil, nil, err
		}
		if err := server.BindPromptSkillsWrites(durable); err != nil {
			cleanup()
			return nil, nil, err
		}
	}
	// The four recovery candidate operations share the durable author admission:
	// their repository writes the author's own database, never a renderer path.
	if err := server.BindRecovery(durable); err != nil {
		cleanup()
		return nil, nil, err
	}
	// Continuity and summary snapshots share the same durable author admission.
	if err := server.BindSummary(durable); err != nil {
		cleanup()
		return nil, nil, err
	}
	// Review and revision writes keep their source CAS in the repository; this
	// binding adds only the operations the editor read binders leave unbound.
	if err := server.BindReviewRevisionRemainder(durable); err != nil {
		cleanup()
		return nil, nil, err
	}
	// The import-run bookkeeping advances and inspects the durable import state
	// machine on that same admitted author database. Run CREATION (parsing,
	// inspection, restart) and the effect receipts stay unbound: they need owners
	// this lane does not hold, so they keep failing closed.
	if err := server.BindImportRuns(durable); err != nil {
		cleanup()
		return nil, nil, err
	}
	// The finalization outbox commits the author-confirmed prose of one draft on
	// the same admitted author database. Those rows ARE the manuscript of record
	// for the durable lane, so the three names that write them are bound here;
	// the publication receipt stays unminted because this lane owns no publisher.
	if err := server.BindFinalization(durable); err != nil {
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
