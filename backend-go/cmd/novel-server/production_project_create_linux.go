//go:build linux

package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/admission"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectcatalog"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// Project creation for the production lane: `project:create` is served here so a
// browser user can START a book instead of failing closed at
// GO_OPERATION_NOT_MIGRATED (internal/transport/server.go:116).
//
// THE REFERENCE IS THE SPEC
//
//	electron/controllers/project-controller.ts:468-575
//	    ipcMain.handle('project:create', (event, config, requestToken)) - the
//	    desktop handler: capture a rollback boundary, refuse as `stale` when a
//	    newer open/create request superseded this one, sanitize the name, create
//	    the directory tree, write the manifest, create and initialize the
//	    database, seed the project_core row, register the recent-project entry,
//	    restore the previous boundary, and answer
//	    {success, projectId, projectPath, requestToken, ...databaseState} with
//	    `...databaseState` = {activeProjectPath, databaseRestored, dbReady}.
//	electron/services/project-access.ts:145-166 + :335-357
//	    createProject: the parent must be an existing canonical directory, the
//	    child name comes from sanitizeProjectDirectoryName (:104-117), an
//	    existing target is PROJECT_DIRECTORY_EXISTS, then writeManifest writes
//	    `<root>/.ai-novel/project.json` with the six-key canonical manifest,
//	    two-space indented, trailing newline, O_EXCL.
//	electron/services/project-access.ts:335-357 + electron/database.ts:25-34
//	    the storage directory is `.ai-novel`, the database `.ai-novel/project.db`,
//	    created `wx` 0600, opened with `foreign_keys = ON`, then the v0 baseline
//	    plus M01..M06 are applied - i.e. exactly the pinned v7 schema.
//	electron/controllers/project-controller.ts:498-503
//	    `fs.mkdirSync(<root>/.ai-novel/prompts)` and the two project_core writes
//	    (ProjectCoreRepository.init(name, language), then update(genre,
//	    targetAudience)). No prompt or skill file is seeded: the directory is.
//	src/shared/ipc-channels.ts:342-358
//	    the wire shape: args [config, requestToken, rendererProjectPath|null],
//	    result {success, projectId, projectPath?, requestToken, activeProjectPath,
//	    databaseRestored, dbReady, stale?, error?, errorCode?}.
//
// WHAT THIS LANE MINTS, AND FROM WHAT
//
// The schema is never hand-written here. `admission.WriteBaselineV7`
// (internal/admission/baseline_v7_ddl.go:744) materialises the pinned v7
// statements, and its own test proves those statements hash to the fingerprints
// in internal/admission/pinned_contract.go. A created database therefore has the
// one fingerprint projectaccess.AdmitAuthor will later accept.
//
// ORDER OF OPERATIONS (and why nothing half-created survives)
//
//	1. refuse a duplicate directory before a byte is written      (nothing to undo)
//	2. mkdir <library>/<name>                                     (0700)
//	3. write <root>/.ai-novel/project.json                        (O_EXCL, 0600)
//	4. mkdir <root>/.ai-novel/prompts
//	5. create + provision <root>/.ai-novel/project.db from the v7 asset,
//	   then seed the two singletons the v7 file must contain (project_core's
//	   row for project:open, character_identity_meta's row for admission)
//	6. re-enumerate the library and prove the manifest is admitted
//	7. stage a private copy and admit the database with the SAME verifier the
//	   startup harvest uses (admitProductionProject), i.e. prove that this lane
//	   can admit what it just minted
//	8. register the project with the live catalog (register it, then prove the
//	   catalog resolves the key back into an openable project)
//	9. add it to this lane's admitted inventory so project:recent-list and
//	   /api/library serve it without a restart
//
// Steps 2-9 are all-or-nothing for the directory: every failure after step 2
// removes the whole tree this call created (it never existed before, is not in
// the inventory and is not registered yet). A failure AFTER the catalog accepted
// the project first withdraws the catalog entry (projectcatalog.Store.Remove)
// and only then removes the tree; if the withdrawal itself fails the tree is
// kept, because the catalog's navigation document must never name a path that
// no longer exists. The one state a later `project:create` recovers from is a
// stray directory: it refuses with PROJECT_DIRECTORY_EXISTS and touches nothing,
// exactly like the reference, so a leftover is a refusal, never a silent adopt.
//
// `stale` is never true here, and that is a real limitation, not a shortcut: the
// reference decides `stale` from the desktop main process's serialized
// latest-open-request token (project-controller.ts:474-490), and this lane has
// no such queue - every HTTP request is independent. `project:create` is a
// `global`-scoped operation (web/operations.json:298) that carries no project
// session, so it is not lease-fenced either; it only *observes* the caller's live
// lease to report activeProjectPath truthfully, and it never rotates, closes or
// takes one.

const (
	// productionProjectCreateOperation is the only name this file binds.
	productionProjectCreateOperation = "project:create"
	// The browser sends [config, requestToken, rendererProjectPath|null]
	// (src/shared/ipc-channels.ts:342-346, src/stores/project-store.ts:524-530).
	productionProjectCreateArity          = 3
	productionProjectCreateConfigPosition = 0
	productionProjectCreateTokenPosition  = 1
	productionProjectCreateRenderPosition = 2
	// createCanonicalProjectManifest (src/shared/project-format.ts:24-30) admits
	// exactly these six keys and these three literals.
	productionProjectCreateSchemaVersion  = 1
	productionProjectCreateStorageVersion = 1
	productionProjectCreateKind           = "ai-novel-project"
	productionProjectCreateStorageFormat  = "ai-novel"
	// productionProjectCreateTimestampLayout is `new Date().toISOString()`: the
	// manifest createdAt shape the reference writes (three fractional digits, Z).
	productionProjectCreateTimestampLayout = "2006-01-02T15:04:05.000Z"
	// DIR_PROMPTS (src/shared/project-paths.ts:16).
	productionProjectCreatePromptDirectory = "prompts"
	// The reference creates the project directory with fs.mkdirSync (default
	// mode) and the database `wx` 0600 (electron/database.ts:26-27). projectfs
	// later requires the database to be single-linked and not group/other
	// writable (internal/projectfs/write_linux.go:99-105), and the project root
	// to be owner-only writable as well (:107-113).
	productionProjectCreateDirectoryMode os.FileMode = 0o700
	productionProjectCreateFileMode      os.FileMode = 0o600
	// productionProjectCreateFallbackName is the reference's own fallback for a
	// name that sanitizes to nothing (project-path.ts:17).
	productionProjectCreateFallbackName = "未命名项目"
	// productionProjectCreateMaxNameBytes keeps the directory name inside the
	// portable Linux NAME_MAX the shipped desktop also targets.
	productionProjectCreateMaxNameBytes = 255
	// productionProjectCreateCoreID is the single project_core row id
	// (electron/repositories/project-core-repository.ts:4-6: always one row,
	// id = 'main').
	productionProjectCreateCoreID = "main"
	// productionProjectCreateCoreInsert seeds the one row project:open refuses
	// to open without (internal/authorruntime/project_open.go:117-135 requires
	// project_name, total_chapters, words_per_chapter, character_states and both
	// timestamps to be non-NULL; every one of those is a column default in the
	// pinned v7 project_core, internal/admission/baseline_v7_ddl.go:486-518).
	productionProjectCreateCoreInsert = `INSERT INTO project_core
		(id, project_name, genre, target_audience, writing_language)
		VALUES (?, ?, ?, ?, ?)`
	// productionProjectCreateIdentityInsert is the character-identity singleton
	// the desktop's own M02 migration seeds in the same statement batch as the
	// table it lives in (electron/migrations/m02-character-identity.ts:33-36).
	// The exported Go asset carries no seed rows, so a Go-side mint must add it:
	// internal/admission/identity.go:23-25 fails every admission whose
	// character_identity_meta has no 'main' row.
	productionProjectCreateIdentityInsert = `INSERT INTO character_identity_meta (id, revision, legacy_count) VALUES ('main', 0, 0)`
	// productionProjectCreateRosterInsert is the character-roster singleton the
	// desktop's own roster schema seeds on first open (electron/repositories/
	// character-roster-schema.ts:75-84). A fresh project has zero characters and
	// no legacy markdown, so migration_state is 'empty' - the same row the older
	// admitted projects carry. Without it db:character-roster-read refuses a
	// just-minted project with CHARACTER_ROSTER_META_MISSING.
	productionProjectCreateRosterInsert = `INSERT INTO character_roster_meta
		(id, schema_version, revision, migration_state, legacy_markdown, projection_hash, fact_hash)
		VALUES ('main', 1, 0, 'empty', '', '', '')`
	// productionProjectCreateContinuityInsert seeds the story-continuity singleton
	// the desktop's baseline schema seeds in the same statement as its table
	// (electron/migrations/baseline-schema.ts:614, INSERT OR IGNORE INTO
	// continuity_projection_meta (id) VALUES ('main')). generation and the stale
	// watermark stay on their column defaults (0 and NULL).
	productionProjectCreateContinuityInsert = `INSERT INTO continuity_projection_meta (id) VALUES ('main')`
	// Refusal and warning codes. Every refusal code the browser already knows is
	// spelled exactly as the reference throws it
	// (electron/services/project-access.ts, src/i18n/app-errors.ts:12-52).
	productionProjectCreateInvalidArguments = "INVALID_ARGUMENTS"
	productionProjectCreateDirectoryExists  = "PROJECT_DIRECTORY_EXISTS"
	productionProjectCreateDirectoryUnsafe  = "PROJECT_DIRECTORY_UNSAFE"
	productionProjectCreateManifestInvalid  = "PROJECT_MANIFEST_INVALID"
	productionProjectCreateSchemaRefused    = "PROJECT_CREATE_SCHEMA_UNSUPPORTED"
	productionProjectCreateOpenRefused      = "PROJECT_CREATE_OPEN_FAILED"
	productionProjectCreateFailed           = "PROJECT_CREATE_FAILED"
	productionProjectCreateClosed           = "PRODUCTION_CLOSED"
	productionProjectCreateTargetRefused    = "PROJECT_CREATE_TARGET_REFUSED"
	productionProjectCreateRollbackWarning  = "PROJECT_CREATE_ROLLBACK_INCOMPLETE"
	productionProjectCreateShelfWarning     = "PROJECT_RECENT_LIST_UPDATE_FAILED"
)

// productionProjectCreateResult is the wire answer of project:create, field for
// field the shape src/shared/ipc-channels.ts:342-358 declares. An empty error or
// warning is omitted, so a success never carries a stale code.
//
// databaseRestored and dbReady report whether the caller's admitted handle is
// still usable after the call, never whether a project is open: create switches
// no database (the renderer opens the new book with project:open afterwards,
// project-controller.ts:551-553), so both are true for every answer this file
// produces, including a refusal, and a FALSE would mean this lane damaged a
// handle. activeProjectPath is the caller's own live lease, read and never
// rotated.
type productionProjectCreateResult struct {
	Success           bool    `json:"success"`
	ProjectID         string  `json:"projectId"`
	ProjectPath       string  `json:"projectPath,omitempty"`
	RequestToken      string  `json:"requestToken"`
	ActiveProjectPath *string `json:"activeProjectPath"`
	DatabaseRestored  bool    `json:"databaseRestored"`
	DBReady           bool    `json:"dbReady"`
	Stale             bool    `json:"stale,omitempty"`
	Error             string  `json:"error,omitempty"`
	Warning           string  `json:"warning,omitempty"`
	ErrorCode         string  `json:"errorCode,omitempty"`
}

// productionProjectCreateTarget is the request after the wire shape has been read
// once. Nothing after this point re-reads args.
type productionProjectCreateTarget struct {
	name     string
	genre    string
	audience string
	language string
	token    string
	// renderer is the third argument: the project the browser is leaving, not a
	// parent directory. In the reference the parent comes from a one-shot
	// directory grant (project-controller.ts:493-496) because the desktop lets the
	// user choose it; this lane has exactly one writable parent, its own pinned
	// library root, so the value can only ever be checked, never obeyed.
	renderer string
}

// productionProjectCreate binds project:create. It borrows the lane's own
// library, trusted config and durable runtime rather than opening a second view
// of the data root, so it can only mint a project the same process will admit.
type productionProjectCreate struct {
	open *productionOpen
	// config is the lane's own admission contract, the exact one project:open
	// will apply. A database this file mints is admitted against it before the
	// catalog names it, so create cannot answer success for a book this very
	// process would then refuse to open.
	config productionConfig
	// mu serialises creations in this process: the directory name is a
	// filesystem claim, and Register rewrites the catalog's navigation document.
	mu sync.Mutex
}

func newProductionProjectCreate(open *productionOpen, config productionConfig) *productionProjectCreate {
	return &productionProjectCreate{open: open, config: config}
}

// Bind installs project:create.
func (c *productionProjectCreate) Bind(server *transport.Server) error {
	if c == nil || c.open == nil || len(c.config.Schema) == 0 || c.config.MaxDatabaseBytes <= 0 || server == nil {
		return errors.New("PROJECT_CREATE_BINDER_INVALID")
	}
	return server.Bind(productionProjectCreateOperation, c.create)
}

// create answers project:create for the lane's own library. Every business
// refusal is a 200 with a documented code; none of them throws, exactly like the
// reference (project-controller.ts:556-575 answers {success:false, error}).
func (c *productionProjectCreate) create(ctx context.Context, caller transport.Caller, args []any) (any, error) {
	request, code := productionProjectCreateRequest(args)
	if code != "" {
		return c.failure(ctx, caller, request.token, code, ""), nil
	}
	if ctx == nil {
		return c.failure(ctx, caller, request.token, productionProjectCreateClosed, ""), nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	open := c.open
	open.mu.Lock()
	closed, durable, library, layout, scratch := open.closed, open.durable, open.library, open.layout, open.scratch
	open.mu.Unlock()
	if closed || durable == nil || library == nil || durable.lifecycle == nil || scratch == "" {
		return c.failure(ctx, caller, request.token, productionProjectCreateClosed, ""), nil
	}
	// The third argument is not authority: it is accepted only when this lane
	// itself vouched for it (null, its own library root, or one admitted project).
	// A request that names anything else is asking to create relative to a
	// directory this lane never enumerated, and is refused before any write.
	if !open.productionProjectCreateTargetTrusted(request.renderer, layout.libraryRoot) {
		return c.failure(ctx, caller, request.token, productionProjectCreateTargetRefused, ""), nil
	}
	directoryName, code := productionProjectCreateDirectoryName(request.name)
	if code != "" {
		return c.failure(ctx, caller, request.token, code, ""), nil
	}
	projectPath := filepath.Join(layout.libraryRoot, directoryName)
	// A duplicate name or a duplicate path is the same refusal, taken from the
	// disk fact before anything is created: the reference throws
	// PROJECT_DIRECTORY_EXISTS and leaves the tree untouched
	// (project-access.ts:162-166), and productionProjectPresent treats an
	// unreadable parent as "present" rather than as proof of absence.
	if productionProjectPresent(projectPath) {
		return c.failure(ctx, caller, request.token, productionProjectCreateDirectoryExists, ""), nil
	}
	if err := productionProjectCreateTree(ctx, projectPath, directoryName, request); err != nil {
		return c.failure(ctx, caller, request.token, productionProjectCreateFailed,
			productionProjectCreateRollbackTree(projectPath)), nil
	}
	// The manifest's admission proof: the library itself must enumerate the
	// project this call just wrote, with the same reader startup uses.
	listed, code := productionProjectCreateInventory(ctx, library, projectPath)
	if code != "" {
		return c.failure(ctx, caller, request.token, code, productionProjectCreateRollbackTree(projectPath)), nil
	}
	// The database's admission proof: stage a private copy exactly like the
	// startup harvest and verify it against the lane's own contract. A minted
	// database the lane would not admit must not reach the catalog.
	evidence, admitErr := admitProductionProject(ctx, library, listed, scratch, productionConfig{
		Schema: c.config.Schema, MaxDatabaseBytes: c.config.MaxDatabaseBytes,
	})
	if admitErr != nil {
		return c.failure(ctx, caller, request.token, productionProjectCreateSchemaRefused,
			productionProjectCreateRollbackTree(projectPath)), nil
	}
	// Register: the created project enters the SAME catalog project:open resolves
	// (authorruntime.ReadLifecycle.OpenProjectDurable ->
	// projectcatalog.Store.Resolve), never a second one.
	catalog, ok := durable.productionCreateCatalog()
	if !ok {
		return c.failure(ctx, caller, request.token, productionProjectCreateClosed,
			productionProjectCreateRollbackTree(projectPath)), nil
	}
	project, openErr := library.OpenProject(projectPath)
	if openErr != nil {
		return c.failure(ctx, caller, request.token, productionProjectCreateFailed,
			productionProjectCreateRollbackTree(projectPath)), nil
	}
	recent, registerErr := catalog.Register(project)
	closeErr := project.Close()
	if registerErr != nil {
		return c.failure(ctx, caller, request.token, productionProjectCreateFailed,
			productionProjectCreateRollbackTree(projectPath)), nil
	}
	if closeErr != nil {
		// The catalog entry is live but the capability could not be released.
		return c.failure(ctx, caller, request.token, productionProjectCreateFailed,
			productionProjectCreateWithdraw(catalog, recent.Key, projectPath)), nil
	}
	if !durable.productionProjectRememberKey(projectPath, recent.Key) {
		return c.failure(ctx, caller, request.token, productionProjectCreateClosed,
			productionProjectCreateWithdraw(catalog, recent.Key, projectPath)), nil
	}
	// Prove it opens: resolve the key the catalog just issued back into a project
	// capability. This is the same call project:open makes; the lease-level proof
	// (project:open plus a read over a separate mode=ro connection) is the test's.
	proven, proveErr := catalog.Resolve(recent.Key)
	if proveErr != nil {
		durable.productionProjectForgetKey(projectPath)
		return c.failure(ctx, caller, request.token, productionProjectCreateOpenRefused,
			productionProjectCreateWithdraw(catalog, recent.Key, projectPath)), nil
	}
	identityHeld := proven.ProjectID() == listed.ProjectID && proven.Path() == projectPath
	proveCloseErr := proven.Close()
	if !identityHeld || proveCloseErr != nil {
		durable.productionProjectForgetKey(projectPath)
		return c.failure(ctx, caller, request.token, productionProjectCreateOpenRefused,
			productionProjectCreateWithdraw(catalog, recent.Key, projectPath)), nil
	}
	// The recent-list update. The project exists, opens and is registered, so a
	// failure to reach this lane's own inventory is a warning, never an undo: the
	// book is on disk and openable, and undoing it would be the bigger lie.
	warning := ""
	if !open.productionProjectCreateShelve(listed, evidence) {
		warning = productionProjectCreateShelfWarning
	}
	return productionProjectCreateResult{
		Success:           true,
		ProjectID:         listed.ProjectID,
		ProjectPath:       projectPath,
		RequestToken:      request.token,
		ActiveProjectPath: c.activeProjectPath(ctx, caller, durable),
		DatabaseRestored:  true,
		DBReady:           true,
		Warning:           warning,
	}, nil
}

// failure builds one business answer. It never throws: the reference answers
// these as results so the renderer can keep its editor state.
func (c *productionProjectCreate) failure(ctx context.Context, caller transport.Caller, token, code, warning string) productionProjectCreateResult {
	durable := (*productionDurableOpen)(nil)
	if c != nil && c.open != nil {
		c.open.mu.Lock()
		durable = c.open.durable
		c.open.mu.Unlock()
	}
	return productionProjectCreateResult{
		Success:           false,
		ProjectID:         "",
		RequestToken:      token,
		ActiveProjectPath: c.activeProjectPath(ctx, caller, durable),
		DatabaseRestored:  true,
		DBReady:           true,
		Error:             code,
		Warning:           warning,
	}
}

// activeProjectPath reports the caller's own live lease without touching it. It
// is the truthful reading of the reference's ...databaseState.activeProjectPath
// after a boundary restore; an absent, foreign or unverifiable lease simply has
// no path to report.
func (c *productionProjectCreate) activeProjectPath(ctx context.Context, caller transport.Caller, durable *productionDurableOpen) *string {
	if ctx == nil || durable == nil || durable.lifecycle == nil {
		return nil
	}
	owner, ownerErr := productionLeaseOwner(caller)
	if ownerErr != nil {
		return nil
	}
	current, present, currentErr := durable.lifecycle.CurrentProjectDurable(ctx, owner)
	if currentErr != nil || !present || current.ProjectPath == "" {
		return nil
	}
	path := current.ProjectPath
	return &path
}

// productionProjectCreateRequest reads the three wire arguments once. It returns
// a symbolic code instead of an error because the reference answers these
// refusals as results, not throws.
func productionProjectCreateRequest(args []any) (productionProjectCreateTarget, string) {
	if len(args) != productionProjectCreateArity {
		return productionProjectCreateTarget{}, productionProjectCreateInvalidArguments
	}
	config, configOK := args[productionProjectCreateConfigPosition].(map[string]any)
	if !configOK {
		return productionProjectCreateTarget{}, productionProjectCreateInvalidArguments
	}
	token, tokenOK := args[productionProjectCreateTokenPosition].(string)
	if !tokenOK || token == "" {
		return productionProjectCreateTarget{token: token}, productionProjectCreateInvalidArguments
	}
	renderer := ""
	if args[productionProjectCreateRenderPosition] != nil {
		value, rendererOK := args[productionProjectCreateRenderPosition].(string)
		if !rendererOK {
			return productionProjectCreateTarget{token: token}, productionProjectCreateInvalidArguments
		}
		renderer = value
	}
	// name is the one required field of CreateProjectConfig
	// (src/shared/ipc-channels.ts:246-252); an empty value is not malformed - it
	// sanitizes to the reference's own fallback name.
	if _, present := config["name"]; !present {
		return productionProjectCreateTarget{token: token}, productionProjectCreateInvalidArguments
	}
	name, nameOK := productionProjectCreateOptionalString(config, "name")
	if !nameOK {
		return productionProjectCreateTarget{token: token}, productionProjectCreateInvalidArguments
	}
	genre, genreOK := productionProjectCreateOptionalString(config, "genre")
	if !genreOK {
		return productionProjectCreateTarget{token: token}, productionProjectCreateInvalidArguments
	}
	audience, audienceOK := productionProjectCreateOptionalString(config, "targetAudience")
	if !audienceOK {
		return productionProjectCreateTarget{token: token}, productionProjectCreateInvalidArguments
	}
	language := "zh-CN"
	if requested, languageOK := productionProjectCreateOptionalString(config, "writingLanguage"); !languageOK {
		return productionProjectCreateTarget{token: token}, productionProjectCreateInvalidArguments
	} else if requested == "en-US" {
		// resolveWritingLanguage (src/shared/writing-language.ts) resolves anything
		// that is not en-US to the default, which readProjectOpenCore also does
		// (internal/authorruntime/project_open.go:152-155).
		language = "en-US"
	}
	return productionProjectCreateTarget{
		name: name, genre: genre, audience: audience, language: language,
		token: token, renderer: renderer,
	}, ""
}

// productionProjectCreateOptionalString reads one optional string config field.
// An absent or null field is the empty string, exactly like an optional field of
// CreateProjectConfig (src/shared/ipc-channels.ts:246-252); any other type is a
// malformed request rather than a silently coerced value.
func productionProjectCreateOptionalString(config map[string]any, key string) (string, bool) {
	value, exists := config[key]
	if !exists || value == nil {
		return "", true
	}
	text, ok := value.(string)
	return text, ok
}

// productionProjectCreateDirectoryName is sanitizeProjectDirectoryName
// (electron/services/project-access.ts:104-117) plus the one safety condition the
// desktop gets from path.resolve: the result must be a single path component.
func productionProjectCreateDirectoryName(name string) (string, string) {
	base := strings.TrimSpace(name)
	if index := strings.LastIndexAny(base, `\/`); index >= 0 {
		base = base[index+1:]
	}
	var builder strings.Builder
	for _, character := range base {
		// WINDOWS_ILLEGAL_FILE_NAME_CHARS + replaceWindowsControlCharacters
		// (electron/controllers/project-path.ts:4-18): a name that is legal on one
		// platform but not the other is rewritten the same way on both.
		if character < 0x20 || strings.ContainsRune(`<>:"/\|?*`, character) {
			builder.WriteRune('_')
			continue
		}
		builder.WriteRune(character)
	}
	cleaned := strings.TrimSpace(builder.String())
	if cleaned == "" {
		cleaned = productionProjectCreateFallbackName
	}
	if cleaned == "." || cleaned == ".." || len(cleaned) > productionProjectCreateMaxNameBytes ||
		strings.ContainsAny(cleaned, "/\\\x00") || filepath.Base(cleaned) != cleaned || !filepath.IsLocal(cleaned) {
		return "", productionProjectCreateDirectoryUnsafe
	}
	return cleaned, ""
}

// productionProjectCreateTree writes the reference's directory layout for one new
// project. Every file is created exclusively, so a tree that half-exists can
// never be adopted by accident; the caller removes the whole tree on failure.
func productionProjectCreateTree(ctx context.Context, projectPath, name string, request productionProjectCreateTarget) error {
	if ctx == nil {
		return errors.New("PRODUCTION_PROJECT_CREATE_CONTEXT_REQUIRED")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Mkdir(projectPath, productionProjectCreateDirectoryMode); err != nil {
		return err
	}
	storage := filepath.Join(projectPath, projectfs.StorageDirectory)
	if err := os.Mkdir(storage, productionProjectCreateDirectoryMode); err != nil {
		return err
	}
	projectID, idErr := productionProjectCreateID()
	if idErr != nil {
		return idErr
	}
	if err := productionProjectCreateManifest(storage, projectID); err != nil {
		return err
	}
	// DIR_PROMPTS is created and left empty: the reference seeds no prompt or
	// skill file here either (project-controller.ts:498), the renderer's own
	// prompt template reads do that later.
	if err := os.Mkdir(filepath.Join(storage, productionProjectCreatePromptDirectory), productionProjectCreateDirectoryMode); err != nil {
		return err
	}
	return productionProjectCreateDatabase(ctx, storage, name, request)
}

// productionProjectCreateManifest writes the six-key canonical manifest exactly
// as createCanonicalProjectManifest + writeManifest do
// (src/shared/project-format.ts:24-30, project-access.ts:337-353): two-space
// indentation, a trailing newline, and O_EXCL so a racing create cannot overwrite
// a manifest.
func productionProjectCreateManifest(storage, projectID string) error {
	document := struct {
		SchemaVersion  int    `json:"schemaVersion"`
		Kind           string `json:"kind"`
		ProjectID      string `json:"projectId"`
		CreatedAt      string `json:"createdAt"`
		StorageFormat  string `json:"storageFormat"`
		StorageVersion int    `json:"storageVersion"`
	}{
		SchemaVersion:  productionProjectCreateSchemaVersion,
		Kind:           productionProjectCreateKind,
		ProjectID:      projectID,
		CreatedAt:      time.Now().UTC().Format(productionProjectCreateTimestampLayout),
		StorageFormat:  productionProjectCreateStorageFormat,
		StorageVersion: productionProjectCreateStorageVersion,
	}
	encoded, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	file, err := os.OpenFile(filepath.Join(storage, projectfs.ManifestName),
		os.O_WRONLY|os.O_CREATE|os.O_EXCL, productionProjectCreateFileMode)
	if err != nil {
		return err
	}
	if _, err := file.Write(encoded); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

// productionProjectCreateDatabase provisions project.db from the pinned v7 asset
// and seeds the two rows a real v7 file contains - project_core's, which
// project:open reads, and character_identity_meta's, which every admission
// checks. It never hand-writes schema: admission.WriteBaselineV7 owns the
// statement text and refuses to stamp anything but an empty database, and the
// caller runs on a path this call just created exclusively.
func productionProjectCreateDatabase(ctx context.Context, storage, name string, request productionProjectCreateTarget) (err error) {
	path := filepath.Join(storage, projectfs.DatabaseName)
	// The reference creates the file `wx` 0600 and then opens it
	// (electron/database.ts:26-28); creating it separately keeps the mode and the
	// exclusivity, which sql.Open alone cannot express.
	file, createErr := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, productionProjectCreateFileMode)
	if createErr != nil {
		return createErr
	}
	if closeErr := file.Close(); closeErr != nil {
		return closeErr
	}
	dsn, err := productionProjectCreateDSN(path)
	if err != nil {
		return err
	}
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	defer func() { err = errors.Join(err, db.Close()) }()
	if _, err = db.ExecContext(ctx, "PRAGMA foreign_keys = ON"); err != nil {
		return err
	}
	if err = admission.WriteBaselineV7(path, db); err != nil {
		return err
	}
	// The asset is DDL only: internal/admission/baseline_v7_ddl.go contains no
	// seed statement at all, while the desktop migration that produces the same
	// schema also seeds the two singletons this lane refuses a project without.
	// Both are data, never schema; the fingerprint is unaffected because the
	// contract hashes sqlite_master only.
	tx, txErr := db.BeginTx(ctx, nil)
	if txErr != nil {
		return txErr
	}
	committed := false
	defer func() {
		if !committed {
			err = errors.Join(err, tx.Rollback())
		}
	}()
	// project:open refuses a project whose core row is missing
	// (internal/authorruntime/project_open.go:117-135): ProjectCoreRepository
	// .init(name, language) then update(genre, targetAudience)
	// (electron/controllers/project-controller.ts:499-503). created_at and
	// updated_at stay on the pinned defaults (datetime('now')), which is exactly
	// the format that reader parses.
	if _, err = tx.ExecContext(ctx, productionProjectCreateCoreInsert,
		productionProjectCreateCoreID, name, request.genre, request.audience, request.language); err != nil {
		return err
	}
	// character_identity_meta must hold its 'main' row before ANY admission
	// (internal/admission/identity.go:23-25 fails the domain invariant when it
	// does not), and the pinned v7 asset ships the table without the row: the
	// desktop's own migration seeds it in the same statement batch
	// (electron/migrations/m02-character-identity.ts:33-36,
	// `INSERT INTO character_identity_meta VALUES('main',0,0);`). Without this row
	// this lane would mint a book it refuses to admit at the next startup.
	if _, err = tx.ExecContext(ctx, productionProjectCreateIdentityInsert); err != nil {
		return err
	}
	// The roster singleton, for the same reason as the identity singleton above:
	// the pinned v7 asset ships the table without the row, and the roster read
	// refuses a project whose main row is missing.
	if _, err = tx.ExecContext(ctx, productionProjectCreateRosterInsert); err != nil {
		return err
	}
	// The continuity singleton, for the same reason: the exported v7 asset is DDL
	// only, and the summary/finalization/character-projection reads join on its
	// main row.
	if _, err = tx.ExecContext(ctx, productionProjectCreateContinuityInsert); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	committed = true
	return nil
}

// productionProjectCreateDSN mirrors authorDatabaseDSN
// (internal/projectfs/author_linux.go:263-278): mode=rw so a missing database is
// refused, a bounded busy timeout, foreign keys on, and no journal mode, because
// this lane never changes an author project's persistent journal policy.
func productionProjectCreateDSN(path string) (string, error) {
	if path == "" || !filepath.IsAbs(path) || strings.ContainsRune(path, '\x00') {
		return "", errors.New("PRODUCTION_PROJECT_CREATE_PATH_UNSAFE")
	}
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	query := url.Values{}
	query.Set("mode", "rw")
	query.Set("_busy_timeout", "5000")
	query.Set("_foreign_keys", "on")
	uri.RawQuery = query.Encode()
	return uri.String(), nil
}

// productionProjectCreateID mints the manifest's projectId as a version 4 UUID in
// the exact shape projectfs.ParseManifest accepts
// (internal/projectfs/manifest.go:51, :166).
func productionProjectCreateID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16]), nil
}

// productionProjectCreateInventory asks the library the same question startup
// asks and returns the entry for the project this call just wrote.
func productionProjectCreateInventory(ctx context.Context, library *projectfs.Library, projectPath string) (projectfs.ListedProject, string) {
	listed, err := library.ListCanonicalProjects(ctx, projectfs.MaxLibraryEntries)
	if err != nil {
		return projectfs.ListedProject{}, productionProjectCreateFailed
	}
	for _, entry := range listed {
		if entry.Path == projectPath && entry.ProjectID != "" {
			return entry, ""
		}
	}
	return projectfs.ListedProject{}, productionProjectCreateManifestInvalid
}

// productionProjectCreateRollbackTree removes the tree this call created. It
// returns a warning code when the removal failed, because a leftover directory is
// then the state the next create answers PROJECT_DIRECTORY_EXISTS for.
func productionProjectCreateRollbackTree(projectPath string) string {
	if err := os.RemoveAll(projectPath); err != nil {
		return productionProjectCreateRollbackWarning
	}
	return ""
}

// productionProjectCreateWithdraw takes a registered project back out of the
// catalogue before its tree is removed. If the withdrawal fails the tree is kept:
// the catalog's navigation document must never name a path that no longer exists.
func productionProjectCreateWithdraw(catalog *projectcatalog.Store, key, projectPath string) string {
	if catalog == nil || key == "" {
		return productionProjectCreateRollbackWarning
	}
	if err := catalog.Remove(key); err != nil {
		return productionProjectCreateRollbackWarning
	}
	return productionProjectCreateRollbackTree(projectPath)
}

// productionProjectCreateTargetTrusted reports whether a value the browser sent
// as "the project I am leaving" is one this lane itself vouched for. The parent
// directory is never taken from it.
func (p *productionOpen) productionProjectCreateTargetTrusted(value, libraryRoot string) bool {
	if value == "" || value == libraryRoot {
		return true
	}
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return false
	}
	for _, item := range p.projects {
		if item.listed.Path == value {
			return true
		}
	}
	return false
}

// productionProjectCreateShelve adds the new project to this lane's admitted
// inventory, so project:recent-list and /api/library serve it without a restart
// (the reference's registerRecentProject, project-controller.ts:530-535). The new
// book goes first because ListCanonicalProjects reports newest-first
// (internal/projectfs/list.go:100-108) and this call just stamped the newest
// updatedAt. It returns false only when the lane is closed.
func (p *productionOpen) productionProjectCreateShelve(listed projectfs.ListedProject, evidence admission.Evidence) bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return false
	}
	projects := make([]productionProject, 0, len(p.projects)+1)
	projects = append(projects, productionProject{listed: listed, evidence: evidence})
	for _, item := range p.projects {
		if item.listed.Path != listed.Path {
			projects = append(projects, item)
		}
	}
	p.projects = projects
	return true
}

// productionCreateCatalog returns the one live catalog this process uses.
//
// The catalog is owned by the durable runtime's constructor
// (production_durable_linux.go:74-125) and shared with the ReadLifecycle that
// resolves project:open, so registering here is registering into the catalog the
// opener reads. Opening a second Store over the same directory would issue keys
// nothing resolves, which is why this file adds no catalog of its own.
func (d *productionDurableOpen) productionCreateCatalog() (*projectcatalog.Store, bool) {
	if d == nil {
		return nil, false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed || d.catalog == nil {
		return nil, false
	}
	return d.catalog, true
}

// productionProjectRememberKey records the catalog key of a newly created project
// so project:open resolves the browser path to it, exactly like the keys the
// startup harvest issued. It returns false when the runtime is closed.
func (d *productionDurableOpen) productionProjectRememberKey(projectPath, key string) bool {
	if d == nil || projectPath == "" || key == "" {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed || d.keys == nil {
		return false
	}
	d.keys[projectPath] = key
	return true
}
