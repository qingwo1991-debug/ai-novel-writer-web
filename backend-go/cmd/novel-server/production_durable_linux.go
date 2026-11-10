//go:build linux

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/authorruntime"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectaccess"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectcatalog"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectruntime"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/writerlock"
)

// productionCatalogDirectoryName is the private child of the lane's own scratch
// workspace that holds the process-local project catalog. It never lives under
// NOVEL_DATA_ROOT: the catalog's navigation document is rebuilt from the
// admitted library on every start, so this lane adds no new on-disk layout to a
// directory the author's tooling owns. Only the persistent writer lease entry
// (.native-web.gateway.lock) is created there, exactly as the shipped gateway
// creates it.
const productionCatalogDirectoryName = "catalog"

// errProductionLeaseRetained aborts startup without releasing the writer lease.
// It is only produced after the runtime failed to prove that every admitted
// author database was closed.
var errProductionLeaseRetained = errors.New("PRODUCTION_WRITER_LEASE_RETAINED")

// productionDurableOpen is the transport bridge for the real author runtime. It
// replaces the synthetic lane's flag-provided paths, seeded projects and
// fabricated contexts with the trusted layout of NOVEL_DATA_ROOT: one catalog
// key per admitted workspace project, the exclusive legacy writer lease held by
// this process, and the author runtime opened on the author's own project.db
// instead of a private copy.
//
// Every method here is a thin, validating adapter: no admission decision, no
// lease decision and no CAS decision is made in this file. authorruntime owns
// the lease fence and the permits; projectruntime adapts that runtime to the
// browser project lifecycle protocol.
type productionDurableOpen struct {
	mu        sync.Mutex
	lifecycle *authorruntime.ReadLifecycle
	manager   *projectruntime.Manager
	// keys maps a trusted library project path to the process-local catalog key
	// issued for it at startup. A browser path selects a key; it never becomes
	// one, and an unlisted path can never reach the runtime.
	keys map[string]string
	// catalog is the one live project catalog this process admitted at startup.
	// project:create registers a new project in it so the created book is
	// openable in this same process; a second Store would have an empty key map
	// and would make the new project unopenable until restart.
	catalog *projectcatalog.Store
	closed  bool
}

// newProductionDurable installs the durable author runtime over the admitted
// library while the caller's legacy writer lease is still held. On success the
// lifecycle owns the lease and releases it in Shutdown; on error the lifecycle
// has already shut down (and released it), and every remaining failure path
// returns with the runtime closed.
func newProductionDurable(library *projectfs.Library, layout productionLayout, scratch string, held *writerlock.Lock, config productionConfig, projects []productionProject) (*productionDurableOpen, error) {
	if library == nil || held == nil || scratch == "" || len(config.Schema) == 0 || config.MaxDatabaseBytes <= 0 {
		return nil, errors.New("PRODUCTION_DURABLE_CONFIGURATION_INVALID")
	}
	metadata := filepath.Join(scratch, productionCatalogDirectoryName)
	if err := os.Mkdir(metadata, 0700); err != nil {
		return nil, err
	}
	// The production lane has no publication journal, so it uses the plain
	// catalog constructor: a recovery fence with no exchange machinery behind it
	// would be a false claim of readiness.
	catalog, err := projectcatalog.Open(library, metadata)
	if err != nil {
		return nil, err
	}
	readConfig := projectaccess.Config{
		DataRoot: layout.dataRoot, LibraryRoot: layout.libraryRoot,
		ScratchParent: scratch, MaxDatabaseBytes: config.MaxDatabaseBytes,
		Schema: config.Schema,
	}
	lifecycle, err := authorruntime.NewReadLifecycleWithHeldLock(catalog, readConfig, held)
	if err != nil {
		return nil, err
	}
	fail := func(cause error) (*productionDurableOpen, error) {
		if shutdownErr := lifecycle.Shutdown(); shutdownErr != nil {
			// A runtime that cannot prove every author database closed must never
			// hand its lease to the next writer: the caller keeps the lease and
			// startup aborts.
			return nil, errors.Join(cause, shutdownErr, errProductionLeaseRetained)
		}
		return nil, cause
	}
	manager, err := projectruntime.NewManager(lifecycle)
	if err != nil {
		return fail(err)
	}
	keys := make(map[string]string, len(projects))
	for _, item := range projects {
		// A catalog capability names the exact admitted project identity; the
		// library refuses anything that changed since the startup inventory read.
		project, err := library.OpenProject(item.listed.Path)
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
		keys[item.listed.Path] = recent.Key
	}
	return &productionDurableOpen{lifecycle: lifecycle, manager: manager, keys: keys, catalog: catalog}, nil
}

// Close shuts the admitted runtime down. Every open author database is closed
// before the writer lease is released; a runtime that cannot confirm that keeps
// the lease instead of letting a second writer in.
func (d *productionDurableOpen) Close() error {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return nil
	}
	d.closed = true
	lifecycle := d.lifecycle
	d.mu.Unlock()
	if lifecycle == nil {
		return nil
	}
	return lifecycle.Shutdown()
}

// productionOwner is the exact cookie session plus browser tab identity. A
// browser cannot borrow another caller's lease, so an unnamed caller is refused
// before any repository or lease is touched.
func productionOwner(caller transport.Caller) (projectsession.Owner, error) {
	owner := projectsession.Owner{SessionID: caller.SessionID, ClientID: caller.ClientID}
	if owner.SessionID == "" || owner.ClientID == "" ||
		strings.ContainsAny(owner.SessionID+owner.ClientID, ":\x00") {
		return projectsession.Owner{}, errors.New("PROJECT_SESSION_REQUIRED")
	}
	return owner, nil
}

// keyFor reports the catalog key of an admitted project. The path is matched
// against the trusted startup inventory, never resolved on the filesystem.
func (d *productionDurableOpen) keyFor(projectPath string) (string, bool) {
	key, known := d.keys[projectPath]
	return key, known && key != ""
}

// OpenProject maps a browser project path to its catalog key and opens the
// author's own database through the durable runtime. The path never becomes
// authority: only the key issued for an admitted project can be opened.
func (d *productionDurableOpen) OpenProject(ctx context.Context, caller transport.Caller, target any, requestToken string, requestedPath any) (any, error) {
	if d == nil || ctx == nil {
		return nil, errors.New("PRODUCTION_CLOSED")
	}
	browserPath, ok := target.(string)
	if !ok || browserPath == "" {
		return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed || d.manager == nil {
		return nil, errors.New("PRODUCTION_CLOSED")
	}
	key, known := d.keyFor(browserPath)
	if !known {
		return nil, security.NewHTTPError(http.StatusNotFound, "PROJECT_NOT_FOUND")
	}
	return d.manager.OpenProject(ctx, caller, key, requestToken, requestedPath)
}

func (d *productionDurableOpen) CloseProject(ctx context.Context, caller transport.Caller, projectPath string, expected any) (any, error) {
	if d == nil || ctx == nil {
		return nil, errors.New("PRODUCTION_CLOSED")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed || d.manager == nil {
		return nil, errors.New("PRODUCTION_CLOSED")
	}
	return d.manager.CloseProject(ctx, caller, projectPath, expected)
}

func (d *productionDurableOpen) ProjectRuntimeContext(ctx context.Context, caller transport.Caller) (any, error) {
	if d == nil || ctx == nil {
		return nil, errors.New("PRODUCTION_CLOSED")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed || d.manager == nil {
		return nil, errors.New("PRODUCTION_CLOSED")
	}
	return d.manager.ProjectRuntimeContext(ctx, caller)
}

// SaveProject writes one allowlisted project_core patch under the frozen lease.
// The patch is applied by store.ProjectCore through the admitted handle, so the
// same permit, handle identity and post-write content-identity refresh rules
// that guard a draft save guard this write too.
func (d *productionDurableOpen) SaveProject(ctx context.Context, caller transport.Caller, projectID string, patch map[string]any, expected any) (any, error) {
	if d == nil || ctx == nil {
		return nil, errors.New("PRODUCTION_CLOSED")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed || d.manager == nil {
		return nil, errors.New("PRODUCTION_CLOSED")
	}
	return d.manager.SaveProject(ctx, caller, projectID, patch, expected)
}

// ReadDraftJSON serves the eight live draft reads from the admitted author
// database under the caller's exact frozen lease. A stale or foreign lease is
// refused by the coordinator before any SQL runs.
func (d *productionDurableOpen) ReadDraftJSON(ctx context.Context, caller transport.Caller, raw any, operation string, number int64) (json.RawMessage, error) {
	if d == nil || ctx == nil {
		return nil, errors.New("PRODUCTION_CLOSED")
	}
	expected, err := projectsession.ParseContext(raw)
	if err != nil {
		return nil, err
	}
	owner, err := productionOwner(caller)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed || d.lifecycle == nil {
		return nil, errors.New("PRODUCTION_CLOSED")
	}
	if _, known := d.keyFor(expected.ProjectPath); !known {
		return nil, errors.New("PROJECT_NOT_OPEN")
	}
	return d.lifecycle.ReadDraftJSONDurable(ctx, owner, expected, operation, number)
}

// CommitDraftContent is the draft body CAS against the author's own database.
// The full expected body is the baseline, so a save whose draft moved underneath
// the editor is refused as a conflict instead of overwriting it. On success the
// frozen lease rotates in place and the same admitted handle stays installed.
func (d *productionDurableOpen) CommitDraftContent(ctx context.Context, caller transport.Caller, request transport.DraftLiveWriteRequest) (transport.DraftLiveWriteAck, error) {
	var zero transport.DraftLiveWriteAck
	if d == nil || ctx == nil {
		return zero, errors.New("PRODUCTION_CLOSED")
	}
	expected, err := projectsession.ParseContext(request.Expected)
	if err != nil {
		return zero, err
	}
	owner, err := productionOwner(caller)
	if err != nil {
		return zero, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed || d.lifecycle == nil {
		return zero, errors.New("PRODUCTION_CLOSED")
	}
	if _, known := d.keyFor(expected.ProjectPath); !known {
		return zero, errors.New("PROJECT_NOT_OPEN")
	}
	baseline := request.ExpectedContent
	fresh, err := d.lifecycle.CommitDraftCASDurable(ctx, owner, expected,
		request.DraftID, request.Content, request.WordCount, &baseline)
	if err != nil {
		return zero, err
	}
	// The acknowledgement must name the same project under a NEW lease; anything
	// else leaves the browser with a lease this server did not issue.
	if fresh.ProjectID != expected.ProjectID || fresh.ProjectPath != expected.ProjectPath ||
		fresh.LeaseID == "" || fresh.LeaseID == expected.LeaseID {
		return zero, errors.New("PRODUCTION_COMMIT_RESULT_INVALID")
	}
	return transport.DraftLiveWriteAck{ProjectSession: map[string]any{
		"projectId":   fresh.ProjectID,
		"projectPath": fresh.ProjectPath,
		"leaseId":     fresh.LeaseID,
	}}, nil
}

// borrowAuthorDatabase borrows the admitted author database for one repository
// call. The borrow re-proves the permit and the handle identity, and the release
// adopts whatever content identity the call committed before the fence is
// observed again; a refresh that fails leaves the permit stale, so the next
// operation on this project fails closed with AUTHOR_WRITE_STALE instead of
// trusting a handle whose bytes changed underneath it. A read commits nothing,
// so its refresh is only a re-verification.
func (d *productionDurableOpen) borrowAuthorDatabase(ctx context.Context, caller transport.Caller, raw any) (*sql.DB, func(), error) {
	if d == nil || ctx == nil {
		return nil, nil, errors.New("PRODUCTION_CLOSED")
	}
	expected, err := projectsession.ParseContext(raw)
	if err != nil {
		return nil, nil, err
	}
	owner, err := productionOwner(caller)
	if err != nil {
		return nil, nil, err
	}
	// The runtime is consulted without this bridge's mutex held: the borrow
	// outlives this call, and only the runtime may hold the lease fence.
	d.mu.Lock()
	lifecycle := d.lifecycle
	closed := d.closed
	_, known := d.keyFor(expected.ProjectPath)
	d.mu.Unlock()
	if closed || lifecycle == nil {
		return nil, nil, errors.New("PRODUCTION_CLOSED")
	}
	if !known {
		return nil, nil, errors.New("PROJECT_NOT_OPEN")
	}
	db, refresh, done, err := lifecycle.DurableRepository(ctx, owner, expected)
	if err != nil {
		return nil, nil, err
	}
	if db == nil || refresh == nil || done == nil {
		if done != nil {
			done()
		}
		return nil, nil, errors.New("PRODUCTION_DURABLE_INVALID")
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			// Refresh under a fresh context so a canceled request cannot leave
			// the permit pointing at a content identity this write replaced.
			refreshCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = refresh(refreshCtx)
			done()
		})
	}
	return db, release, nil
}

// BeginDrafts borrows the admitted author database for the three self-contained
// draft mutations.
func (d *productionDurableOpen) BeginDrafts(ctx context.Context, caller transport.Caller, raw any) (*store.Drafts, func(), error) {
	db, release, err := d.borrowAuthorDatabase(ctx, caller, raw)
	if err != nil {
		return nil, nil, err
	}
	return store.NewDrafts(db), release, nil
}

// BeginBlueprints borrows the same admitted author database for the editor's
// db:blueprint-get. The borrow is the identical fence every other production
// read and write uses: it is refused when the lease is stale, when the project
// was never opened by this cookie/tab, or when the pinned v7 handle no longer
// matches its admitted content identity. store.Blueprints.Get is a single
// SELECT, so the borrow commits nothing.
func (d *productionDurableOpen) BeginBlueprints(ctx context.Context, caller transport.Caller, raw any) (*store.Blueprints, func(), error) {
	db, release, err := d.borrowAuthorDatabase(ctx, caller, raw)
	if err != nil {
		return nil, nil, err
	}
	return store.NewBlueprints(db), release, nil
}

// ReadCharactersJSON is the editor's three character reads, served from the
// admitted author database under the same frozen cookie/tab/lease fence as
// project:open and project:save. The character repository reserves a coherent
// BEGIN IMMEDIATE snapshot, which is safe here because the legacy writer lock is
// exclusive: no other writer can exist, so the reservation cannot block.
func (d *productionDurableOpen) ReadCharactersJSON(ctx context.Context, caller transport.Caller, raw any, operation string) (json.RawMessage, error) {
	return d.readDurableJSON(ctx, caller, raw, func(lifecycle *authorruntime.ReadLifecycle, owner projectsession.Owner, expected projectsession.Context) (json.RawMessage, error) {
		return lifecycle.ReadCharactersJSONDurable(ctx, owner, expected, operation)
	})
}

// ReadEditorStageJSON is the editor's db:project-core-get and
// db:blueprint-get-all, read inside one admitted read transaction.
func (d *productionDurableOpen) ReadEditorStageJSON(ctx context.Context, caller transport.Caller, raw any, operation string) (json.RawMessage, error) {
	return d.readDurableJSON(ctx, caller, raw, func(lifecycle *authorruntime.ReadLifecycle, owner projectsession.Owner, expected projectsession.Context) (json.RawMessage, error) {
		return lifecycle.ReadEditorStageJSONDurable(ctx, owner, expected, operation)
	})
}

// ReadCurrentOverviewJSON is project:overview-current on the admitted author
// database: the same six stage facts project:peek-overview aggregates, computed
// under the live lease from rows the author's own database actually holds.
func (d *productionDurableOpen) ReadCurrentOverviewJSON(ctx context.Context, caller transport.Caller, raw any) (json.RawMessage, error) {
	return d.readDurableJSON(ctx, caller, raw, func(lifecycle *authorruntime.ReadLifecycle, owner projectsession.Owner, expected projectsession.Context) (json.RawMessage, error) {
		return lifecycle.ReadCurrentOverviewJSONDurable(ctx, owner, expected)
	})
}

// readDurableJSON parses the frozen three-key context once, names the caller's
// own browser owner, refuses a closed bridge, and lends the runtime call the same
// lease the request named. Nothing here opens a database: the runtime owns the
// only admitted handle, and it refuses a lease this caller does not hold.
func (d *productionDurableOpen) readDurableJSON(ctx context.Context, caller transport.Caller, raw any, read func(*authorruntime.ReadLifecycle, projectsession.Owner, projectsession.Context) (json.RawMessage, error)) (json.RawMessage, error) {
	if d == nil || ctx == nil || read == nil {
		return nil, errors.New("PRODUCTION_CLOSED")
	}
	expected, err := projectsession.ParseContext(raw)
	if err != nil {
		return nil, err
	}
	owner, err := productionOwner(caller)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	lifecycle := d.lifecycle
	closed := d.closed
	d.mu.Unlock()
	if closed || lifecycle == nil {
		return nil, errors.New("PRODUCTION_CLOSED")
	}
	return read(lifecycle, owner, expected)
}
