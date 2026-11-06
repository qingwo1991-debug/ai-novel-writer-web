//go:build linux

package main

// The project portability family: what a browser may do with the author's own
// book as a *book* - look at its overview, copy it out as a portable archive,
// put a portable archive back, and browse one directory of the open project.
//
// Six browser operations were unserved. This file decides all six, and the
// decision is the same shape every time: a name is served only when this lane
// can produce the thing the name promises and prove it, and otherwise it stays
// unbound so it keeps failing closed at GO_OPERATION_NOT_MIGRATED with the port
// it is missing named here (productionProjectPortabilityUnbound).
//
// SERVED
//
//   project:peek-overview   A glance at one admitted project: name, word and
//                           character counts, the six stage facts, the finalized
//                           chapter count, the newest excerpt. Read through a
//                           separate mode=ro&_query_only=on connection inside a
//                           read-only transaction, so the glance cannot create a
//                           journal, a WAL frame or a page: it is byte-neutral by
//                           construction, not by promise. No lease is taken. The
//                           channel's return type has no failure union, so every
//                           refusal is the desktop's own {"state":"unavailable"}
//                           at HTTP 200 rather than an error the page cannot read.
//
//   project:archive-export  Transport's own binder (BindArchiveExport) over a
//                           provider that proves four things before a byte is
//                           written: the caller owns the live lease whose exact
//                           projectId/leaseId/projectPath the request names, the
//                           target grant is a real directory in the library
//                           allowlist, the archive is built from a private
//                           checkpointed snapshot of the admitted database (never
//                           from the live file), and the published bytes are the
//                           receipt's bytes. The provider also proves the format:
//                           manifest, three entries, both sidecars, and a readback
//                           through archiveformat.Inspect of what it is about to
//                           hand out.
//
//   project:archive-restore The mirror. Args are two opaque grant ids and no
//                           path: the archive is a file the picker granted, the
//                           destination a directory it granted. The archive is
//                           inspected, staged into a private directory, validated
//                           against its own manifest (freeze and receipt must
//                           agree with it), admitted by the same schema verifier
//                           the lane uses at startup, and its receipt is
//                           discharged against the bytes being installed. An
//                           existing project is copied first and then refused.
//
//   fs:list-dir             Transport's confined handler (BindEditorNavigation),
//                           with a provider that re-proves the caller's lease and
//                           resolves the browser's relative path through
//                           librarypath so no request string ever reaches a
//                           syscall.
//
// REFUSED, and the port each one is missing
//
//   project:import-legacy-copy  The legacy import vector: an old desktop copy is
//                           a *directory* of project trees, a lancedb vector
//                           store and a per-project journal. Two of the three have
//                           no Go port (internal/projectfs has no vector store,
//                           and m05 character-asset migration lives in
//                           electron/services/import-legacy*.ts), and the copy
//                           carries its own portable transfer authority that this
//                           lane's restore refuses rather than half-installs.
//                           A binder here could only refuse every real request,
//                           which the unbound name already does for free.
//
//   dialog:select-novel-files   Sources in a browser can only arrive as bytes,
//                           and this mux has no upload route to put them in
//                           (POST /api/uploads is 501 in internal/transport).
//                           transport.BindImportSelection needs an
//                           importsource.Store holding owned uploads; with no
//                           ingress there is nothing to select from.
//
// The one portable subset this file implements, stated once so the two archive
// names cannot drift apart: an archive from this lane carries the author's
// verified database bytes, an EMPTY non-replayable history projection, and a
// receipt whose finalizations and summary sources are exactly the current,
// receipted ones read out of those same bytes. Export writes that subset, restore
// accepts exactly that subset, and an archive whose freeze document carries
// records is refused because recording and replaying non-replayable history is
// the port this lane does not have (electron/services/project-archive-service.ts
// policy loop plus portable-runtime-freeze.ts). Installing a redacted copy is also
// not attempted: the field-policy pass that neutralizes machine-authority columns
// is that same missing port, and a lane that ran half of it would produce an
// archive that claims portability and is not.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	_ "github.com/mattn/go-sqlite3" // Registers the database/sql driver "sqlite3".

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/admission"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/archiveformat"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/librarypath"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// The six names this file decides. The four in the first block are bound by Bind;
// the two in the second are deliberately absent so they keep answering
// GO_OPERATION_NOT_MIGRATED.
const (
	productionPortabilityPeekName    = "project:peek-overview"
	productionPortabilityExportName  = "project:archive-export"
	productionPortabilityRestoreName = "project:archive-restore"
	productionPortabilityListName    = "fs:list-dir"

	productionPortabilityLegacyCopyName = "project:import-legacy-copy"
	productionPortabilityNovelFilesName = "dialog:select-novel-files"
)

// The portable archive's three entries. The database is canonical, the two
// sidecars are what makes a copy restorable instead of merely zipped.
const (
	productionPortabilityDatabaseEntry        = "project.db"
	productionPortabilityDatabaseDisposition  = "portable-database"
	productionPortabilityFreezeEntry          = "portable-runtime-freeze.json"
	productionPortabilityFreezeDisposition    = "history-projection"
	productionPortabilityAuthorityEntry       = ".ai-novel/portable-transfer-authority.json"
	productionPortabilityAuthorityDisposition = "transfer-receipt"
	productionPortabilityArchiveExtension     = ".anovel"
)

// The symbolic refusals this file can put on the wire. Every member is in the
// closed PortableProjectOperationErrorCode union, and none of them ever names a
// host path, an archive entry or a database row.
const (
	productionPortabilityCodeInvalid        = "PORTABLE_ARCHIVE_INVALID"
	productionPortabilityCodeLimit          = "PORTABLE_ARCHIVE_LIMIT_EXCEEDED"
	productionPortabilityCodeTargetExists   = "PORTABLE_ARCHIVE_TARGET_EXISTS"
	productionPortabilityCodeSourceChanged  = "PORTABLE_SOURCE_CHANGED"
	productionPortabilityCodeUnsafe         = "PORTABLE_ARCHIVE_SOURCE_UNSAFE"
	productionPortabilityCodeContext        = "PORTABLE_CONTEXT_INVALID"
	productionPortabilityCodeGrant          = "PORTABLE_GRANT_INVALID"
	productionPortabilityCodeSchema         = "PORTABLE_SCHEMA_UNSUPPORTED"
	productionPortabilityCodeRestoreExists  = "PORTABLE_RESTORE_TARGET_EXISTS"
	productionPortabilityCodeRestoreUnsafe  = "PORTABLE_RESTORE_UNSAFE_TARGET"
	productionPortabilityCodeRestoreInvalid = "PORTABLE_RESTORE_INVALID"
)

const (
	// productionPortabilityClosedCode is the refusal of every name in this family
	// once the lane has shut: the same code the other production binders answer.
	productionPortabilityClosedCode = "PRODUCTION_CLOSED"
	// productionPortabilityDownloadPrefix is the one route this file owns on the
	// production mux. It is outside /api/operations because a download is a plain
	// navigation, not an RPC envelope.
	productionPortabilityDownloadPrefix = "/api/downloads/"
	// productionPortabilityDownloadTTL matches the desktop's own download token
	// lifetime (web/server.mjs:534 - one hour).
	productionPortabilityDownloadTTL = time.Hour
	// productionPortabilityIdentityBytes bounds the project identity a glance may
	// name. A project path or id is one string, never a query.
	productionPortabilityIdentityBytes = 4096
	// productionPortabilitySidecarBytes bounds the two sidecars this lane writes
	// and reads back. A sidecar that grew past this is not one of ours.
	productionPortabilitySidecarBytes = 16 << 20
	// productionPortabilityReserveBytes is what an export keeps back from the
	// library's 64 MiB per-artifact write budget for the two sidecars, so a
	// database that fits still has room beside it.
	productionPortabilityReserveBytes int64 = 4 << 20
	// productionPortabilityDefaultDatabaseBytes mirrors the lane's own database
	// budget for a caller that built a config without one.
	productionPortabilityDefaultDatabaseBytes int64 = 512 << 20
	// productionPortabilityTablePattern is archiveformat's table grammar as a
	// character set. A schema whose table names fall outside it cannot be
	// described by a manifest, so it is refused before the manifest is built
	// rather than after.
	productionPortabilityTablePattern = "0123456789abcdefghijklmnopqrstuvwxyz_"
)

// productionPortabilityOmitted states, in the manifest, what a portable copy of
// this lane does not carry. It is the same ledger the desktop's export keeps
// (electron/services/project-archive-service.ts): derived indexes are rebuilt,
// machine authority is dropped because it belongs to the machine that minted it,
// and history is projected - which this lane projects as nothing, see the file
// header.
func productionPortabilityOmitted() []archiveformat.OmittedItem {
	return []archiveformat.OmittedItem{
		{ID: "vectors", Reason: "rebuild-stale"},
		{ID: "knowledge-index", Reason: "rebuild-stale"},
		{ID: "logs", Reason: "exclude-machine-authority"},
		{ID: "temporary-files", Reason: "exclude-machine-authority"},
		{ID: "migration-backups", Reason: "exclude-machine-authority"},
		{ID: "machine-authority-fields", Reason: "exclude-machine-authority"},
		{ID: "non-replayable-history", Reason: "historical-nonreplayable"},
	}
}

// productionProjectPortabilityUnbound reports the two names this family refused
// and the port each one is missing, so the answer to "why is this still 501" is
// in the binary rather than in a commit message.
func productionProjectPortabilityUnbound() map[string]string {
	return map[string]string{
		productionPortabilityLegacyCopyName: "legacy-desktop-vector-and-asset-migration" +
			" (electron/services/import-legacy*.ts; no Go vector store in internal/projectfs)" +
			" plus portable-authority admission for a copied project",
		productionPortabilityNovelFilesName: "browser-upload-ingress" +
			" (POST /api/uploads is unbound in internal/transport, so no importsource" +
			" owner can hold the selected bytes)",
	}
}

// productionPortabilityDownload is one published archive. The token in the URL is
// the capability: it is minted only after an authenticated export proved its
// bytes, it names one file inside the library allowlist, and it dies with the
// session that minted it.
type productionPortabilityDownload struct {
	sessionID string
	relative  string
	name      string
	expires   time.Time
}

// productionPortability is the browser lane's project portability family. It
// borrows the cloud-backup lane for the two operations that are genuinely the
// same operation - keeping a verified pre-restore copy, and installing a verified
// database into an empty target - so this file adds decisions, not a second
// implementation of them.
type productionPortability struct {
	open   *productionOpen
	config productionConfig
	backup *productionCloudBackup
	grants *productionFSGrants
	server *transport.Server
	clock  func() time.Time

	mu        sync.Mutex
	downloads map[string]productionPortabilityDownload
}

// newProductionPortability builds the family. Nothing is read, written or bound
// until Bind.
func newProductionPortability(open *productionOpen, config productionConfig) *productionPortability {
	return &productionPortability{
		open:      open,
		config:    config,
		backup:    newProductionCloudBackup(open, config),
		grants:    newProductionFSGrants(open),
		clock:     time.Now,
		downloads: make(map[string]productionPortabilityDownload),
	}
}

// Bind installs the four served names. The order matters: fs:list-dir arrives
// through transport's own confined handler, which also publishes
// project:recent-list, so a lane that still binds that name standalone gets
// OPERATION_ALREADY_BOUND here instead of a silently different shelf.
func (b *productionPortability) Bind(server *transport.Server) error {
	if server == nil || b == nil || b.open == nil || b.backup == nil || b.grants == nil {
		return errors.New(productionPortabilityClosedCode)
	}
	b.server = server
	if err := server.BindEditorNavigation(b); err != nil {
		return err
	}
	if err := server.BindArchiveExport(b, archiveformat.DefaultLimits()); err != nil {
		return err
	}
	if err := server.Bind(productionPortabilityRestoreName, b.restore); err != nil {
		return err
	}
	return server.Bind(productionPortabilityPeekName, b.peek)
}

// DownloadHandler serves the URLs project:archive-export hands out. The parent
// mounts it at /api/downloads/ on the production mux, in front of the /api/
// pattern, because internal/transport refuses that prefix itself.
func (b *productionPortability) DownloadHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := b.download(w, r); err != nil {
			_ = security.WriteError(w, err)
		}
	})
}

// now is the one clock this family reads, so a test can pin the manifest date.
func (b *productionPortability) now() time.Time {
	if b == nil || b.clock == nil {
		return time.Now().UTC()
	}
	return b.clock().UTC()
}

// ---------------------------------------------------------------------------
// fs:list-dir and project:recent-list (transport.BindEditorNavigation)
// ---------------------------------------------------------------------------

// RecentProjects delegates to the lane's own shelf. There is exactly one mapping
// from admitted projects to the browser shape in this binary, and it is not this
// file's.
func (b *productionPortability) RecentProjects(ctx context.Context, caller transport.Caller) ([]transport.RecentNavigationProject, error) {
	if b == nil || b.open == nil {
		return nil, security.NewHTTPError(http.StatusServiceUnavailable, productionPortabilityClosedCode)
	}
	return b.open.RecentProjects(ctx, caller)
}

// ListDirectory answers one directory of the project the caller holds. The
// browser's path arrived as a string relative to the lease's own project root
// (transport computed it with filepath.Rel and already refused anything that
// escaped), and it is resolved again through librarypath's single project root,
// so a relative path is the only thing that can reach a syscall.
func (b *productionPortability) ListDirectory(ctx context.Context, caller transport.Caller, expected any, relative string) ([]projectfs.VisibleNode, error) {
	if b == nil || b.open == nil || b.open.durable == nil {
		return nil, security.NewHTTPError(http.StatusServiceUnavailable, productionPortabilityClosedCode)
	}
	owner, ownerErr := productionOwner(caller)
	if ownerErr != nil {
		return nil, security.NewHTTPError(http.StatusUnauthorized, "PROJECT_SESSION_REQUIRED")
	}
	current, present, leaseErr := b.open.durable.productionFSGrantProject(ctx, owner)
	if leaseErr != nil {
		return nil, security.NewHTTPError(http.StatusConflict, "PROJECT_SESSION_STALE")
	}
	if !present {
		return nil, security.NewHTTPError(http.StatusConflict, "PROJECT_NOT_OPEN")
	}
	want, parseErr := projectsession.ParseContext(expected)
	if parseErr != nil {
		return nil, security.NewHTTPError(http.StatusBadRequest, "PROJECT_CONTEXT_REQUIRED")
	}
	// The lease the browser named must be the lease this caller holds NOW, path
	// included: the same project at a different path is a different project.
	if want != current {
		return nil, security.NewHTTPError(http.StatusConflict, "PROJECT_SESSION_STALE")
	}
	roots, rootsErr := librarypath.NewRoots([]librarypath.Root{{
		ID:   librarypath.ProjectRootID,
		Kind: librarypath.KindProject,
		Path: current.ProjectPath,
	}})
	if rootsErr != nil {
		return nil, security.NewHTTPError(http.StatusConflict, "PROJECT_CONTEXT_REQUIRED")
	}
	// filepath.Rel answers "." for the root itself; librarypath wants "".
	if relative == "." {
		relative = ""
	}
	target, resolveErr := roots.Resolve(librarypath.ProjectRootID, relative)
	if resolveErr != nil {
		return nil, security.NewHTTPError(http.StatusForbidden, "PROJECT_CONTEXT_REQUIRED")
	}
	if !target.Exists || !target.IsDir {
		return nil, security.NewHTTPError(http.StatusNotFound, "FS_LIST_DIR_FAILED")
	}
	listing, listErr := librarypath.ListDirectory(target, librarypath.DefaultListEntries)
	if listErr != nil {
		return nil, security.NewHTTPError(http.StatusNotFound, "FS_LIST_DIR_FAILED")
	}
	nodes := make([]projectfs.VisibleNode, 0, len(listing.Entries))
	for _, entry := range listing.Entries {
		child, joinErr := librarypath.JoinRelative(target.Relative, entry.Name)
		if joinErr != nil {
			// A name the allowlist cannot express is not a node this read may
			// publish, and dropping it silently would misreport the directory.
			return nil, security.NewHTTPError(http.StatusForbidden, "FS_LIST_DIR_FAILED")
		}
		node := projectfs.VisibleNode{
			Name:  entry.Name,
			Path:  filepath.Join(target.Root, filepath.FromSlash(child)),
			IsDir: entry.Dir,
		}
		if entry.Dir {
			children := []projectfs.VisibleNode{}
			node.Children = &children
		}
		nodes = append(nodes, node)
	}
	return nodes, nil
}

// ---------------------------------------------------------------------------
// project:peek-overview
// ---------------------------------------------------------------------------

// peek answers one admitted project's overview without a lease and without a
// write. The channel has no failure union (src/shared/ipc-channels.ts), so the
// desktop's own unavailable state is the answer to every refusal - a page that
// asks about a project it cannot have learns nothing but that.
func (b *productionPortability) peek(ctx context.Context, caller transport.Caller, args []any) (any, error) {
	unavailable := map[string]any{"state": "unavailable"}
	if b == nil || b.open == nil {
		return unavailable, nil
	}
	if len(args) != 1 {
		return unavailable, nil
	}
	identity, ok := args[0].(string)
	if !ok || identity == "" || len(identity) > productionPortabilityIdentityBytes || !utf8.ValidString(identity) {
		return unavailable, nil
	}
	if err := ctx.Err(); err != nil {
		return unavailable, nil
	}
	listed, found := productionPortabilityAdmitted(b.open, identity)
	if !found {
		return unavailable, nil
	}
	payload, readErr := productionPortabilityOverview(ctx, listed)
	if readErr != nil {
		// The reason is symbolic and the answer is the same one a missing project
		// gets: a hover card cannot act on the difference.
		log.Printf("novel-server: project:peek-overview: %s", "PRODUCTION_PEEK_OVERVIEW_UNAVAILABLE")
		return unavailable, nil
	}
	return json.RawMessage(payload), nil
}

// productionPortabilityAdmitted maps a browser identity - the catalogue path the
// shelf itself publishes, or the project id it carries - onto the trusted
// startup inventory. An identity this lane never admitted is not a project it
// will describe.
func productionPortabilityAdmitted(open *productionOpen, identity string) (projectfs.ListedProject, bool) {
	if open == nil || identity == "" {
		return projectfs.ListedProject{}, false
	}
	open.mu.Lock()
	defer open.mu.Unlock()
	if open.closed {
		return projectfs.ListedProject{}, false
	}
	for _, item := range open.projects {
		if item.listed.ProjectID == identity || item.listed.Path == identity {
			return item.listed, true
		}
	}
	return projectfs.ListedProject{}, false
}

// productionPortabilityOverview reads the six stage facts, the counts and the
// newest excerpt out of the author's database through a read-only, query-only
// connection inside a read-only transaction. Nothing here can write: SQLite
// itself refuses the write, so "peek wrote nothing" is a property of the handle,
// not of the code paths above it.
func productionPortabilityOverview(ctx context.Context, listed projectfs.ListedProject) ([]byte, error) {
	if listed.Path == "" || listed.ProjectID == "" {
		return nil, errors.New("PRODUCTION_PEEK_CONTEXT_INVALID")
	}
	handle, openErr := productionPortabilityOpenReadOnly(listed.Path)
	if openErr != nil {
		return nil, openErr
	}
	defer func() { _ = handle.Close() }()
	tx, beginErr := handle.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if beginErr != nil {
		return nil, beginErr
	}
	overview, readErr := store.ReadCurrentProjectOverviewTx(ctx, tx, listed.ProjectID)
	rollbackErr := tx.Rollback()
	if readErr != nil {
		return nil, readErr
	}
	if rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
		return nil, rollbackErr
	}
	if overview == nil {
		return nil, errors.New("PRODUCTION_PEEK_OVERVIEW_ABSENT")
	}
	return json.Marshal(overview)
}

// productionPortabilityOpenReadOnly opens one project database for reading and
// only reading. mode=ro refuses every write at the SQLite level, _query_only
// refuses a statement that would write even if the mode were wrong, and the
// busy timeout turns a concurrent writer into a refusal instead of a hang.
func productionPortabilityOpenReadOnly(projectPath string) (*sql.DB, error) {
	if projectPath == "" {
		return nil, errors.New("PRODUCTION_PEEK_CONTEXT_INVALID")
	}
	database := filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName)
	return productionPortabilityOpenReadOnlyFile(database)
}

// productionPortabilityOpenReadOnlyFile is the same refusal for a database that
// is not a project's canonical one - the private staged copy an export or a
// restore proves its facts against.
func productionPortabilityOpenReadOnlyFile(database string) (*sql.DB, error) {
	if database == "" {
		return nil, errors.New("PRODUCTION_PEEK_CONTEXT_INVALID")
	}
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(database)}
	uri.RawQuery = "mode=ro&_query_only=on&_busy_timeout=5000"
	handle, openErr := sql.Open("sqlite3", uri.String())
	if openErr != nil {
		return nil, openErr
	}
	handle.SetMaxOpenConns(1)
	return handle, nil
}

// ---------------------------------------------------------------------------
// project:archive-export
// ---------------------------------------------------------------------------

// productionPortabilityFacts are the portable facts this lane reads out of one
// verified snapshot: how big the schema is, how many rows each table holds, and
// the receipted currentness facts the transfer authority is made of.
type productionPortabilityFacts struct {
	counts        map[string]uint64
	tableCount    int
	fieldCount    int
	finalizations []archiveformat.FinalizationAuthority
	summaries     []archiveformat.SummarySourceAuthority
}

// BeginArchiveExport proves the caller, the lease, the grant and the snapshot,
// then hands transport a snapshot it must still prove itself. Every refusal is a
// logged symbolic reason: the binder folds a provider refusal into
// PORTABLE_CONTEXT_INVALID, so the precise cause belongs in the log, never on the
// wire where the page cannot read it.
func (b *productionPortability) BeginArchiveExport(ctx context.Context, caller transport.Caller, expected map[string]string, grantID string) (*transport.ArchiveExportAttempt, error) {
	refuse := func(reason string) (*transport.ArchiveExportAttempt, error) {
		log.Printf("novel-server: %s refused: %s", productionPortabilityExportName, reason)
		return nil, errors.New(reason)
	}
	if ctx == nil {
		return refuse("PRODUCTION_EXPORT_CONTEXT_MISSING")
	}
	owner, ownerErr := productionOwner(caller)
	if ownerErr != nil {
		return refuse("PROJECT_SESSION_REQUIRED")
	}
	durable, library, scratch, _, laneErr := b.backup.lane()
	if laneErr != nil || durable == nil || library == nil || scratch == "" {
		return refuse(productionPortabilityClosedCode)
	}
	registry, registryErr := productionFSGrantsRegistry()
	if registryErr != nil || registry == nil {
		return refuse(productionFSGrantUnavailable.Error())
	}
	binding := librarypath.Binding{SessionID: owner.SessionID, ClientID: owner.ClientID}
	grant, grantErr := registry.Peek(grantID, binding, librarypath.OpWrite)
	if grantErr != nil {
		return refuse(productionPortabilityCodeGrant)
	}
	// 1. The caller's own lease, asked of the runtime, compared with the context
	//    the request named. A tab that has moved on cannot export through a lease
	//    id it still remembers.
	current, present, leaseErr := durable.productionFSGrantProject(ctx, owner)
	if leaseErr != nil || !present {
		return refuse("PROJECT_SESSION_REQUIRED")
	}
	if current.ProjectID != expected["projectId"] || current.LeaseID != expected["leaseId"] || current.ProjectPath != expected["projectPath"] {
		return refuse("PROJECT_SESSION_STALE")
	}
	listed, admitted := productionPortabilityAdmitted(b.open, current.ProjectPath)
	if !admitted || listed.ProjectID != current.ProjectID {
		return refuse("PROJECT_NOT_OPEN")
	}
	// 2. The output directory the grant names, re-resolved through this request's
	//    allowlist, and never author data: an export writes a new file next to the
	//    library, not into the project it is copying.
	roots, _, rootsErr := b.grants.roots(ctx, caller)
	if rootsErr != nil {
		return refuse("PROJECT_SESSION_REQUIRED")
	}
	directory, directoryErr := roots.Resolve(grant.Target.RootID, grant.Target.Relative)
	if directoryErr != nil {
		return refuse(productionPortabilityCodeGrant)
	}
	if directory.Kind != librarypath.KindLibrary {
		return refuse("PORTABLE_TARGET_INSIDE_SOURCE")
	}
	if !directory.Exists || !directory.IsDir {
		return refuse(productionPortabilityCodeGrant)
	}
	// 3. A private, checkpointed snapshot of the admitted database. The author's
	//    file is never opened for writing and never becomes an archive entry.
	project, projectErr := library.OpenProject(current.ProjectPath)
	if projectErr != nil {
		return refuse("PROJECT_NOT_OPEN")
	}
	if project.ProjectID() != current.ProjectID {
		_ = project.Close()
		return refuse("PRODUCTION_PROJECT_IDENTITY_CHANGED")
	}
	staging, stagingErr := os.MkdirTemp(scratch, "project-portability-export-")
	if stagingErr != nil {
		_ = project.Close()
		return refuse("PRODUCTION_ARCHIVE_STAGING_UNAVAILABLE")
	}
	var discardOnce sync.Once
	discard := func() {
		discardOnce.Do(func() {
			_ = project.Close()
			_ = os.RemoveAll(staging)
		})
	}
	databaseBudget := b.config.MaxDatabaseBytes
	if databaseBudget <= 0 {
		databaseBudget = productionPortabilityDefaultDatabaseBytes
	}
	snapshotRoot, rootErr := os.OpenRoot(staging)
	if rootErr != nil {
		discard()
		return refuse("PRODUCTION_ARCHIVE_STAGING_UNAVAILABLE")
	}
	proof, stageErr := project.StageDatabase(ctx, snapshotRoot, databaseBudget)
	rootCloseErr := snapshotRoot.Close()
	if stageErr != nil || rootCloseErr != nil {
		discard()
		return refuse(productionPortabilityCodeSourceChanged)
	}
	snapshotPath := filepath.Join(staging, projectfs.DatabaseName)
	if checkpointErr := productionCloudBackupCheckpoint(ctx, snapshotPath); checkpointErr != nil {
		discard()
		return refuse(productionPortabilityCodeSourceChanged)
	}
	// One entry has to be a whole database: a copy that still needs a log beside
	// it is not a portable copy. A WAL the checkpoint emptied is fine; a WAL with
	// frames left in it is not.
	for _, suffix := range []string{"-wal", "-journal"} {
		info, statErr := os.Lstat(snapshotPath + suffix)
		if statErr == nil && info.Size() != 0 {
			discard()
			return refuse(productionPortabilityCodeUnsafe)
		}
		if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
			discard()
			return refuse(productionPortabilityCodeSourceChanged)
		}
	}
	// 4. The lane's own schema verifier, on the copy that will travel. Evidence
	//    read from the copy is the only evidence worth putting in the manifest.
	evidence, admitErr := admission.VerifySnapshot(ctx, snapshotPath, b.config.Schema)
	if admitErr != nil || evidence.UserVersion != 7 || !productionPortabilityDigest(evidence.SchemaFingerprint) {
		discard()
		return refuse(productionPortabilityCodeSchema)
	}
	databaseSize, databaseDigest, digestErr := productionCloudBackupDigestFile(snapshotPath)
	if digestErr != nil || databaseDigest != evidence.ContentIdentity.Database || databaseSize <= 0 {
		discard()
		return refuse(productionPortabilityCodeSourceChanged)
	}
	// 5. The output channel is the library's own file writer, which caps one
	//    artifact at 64 MiB (internal/librarypath MaxFileBytes). Refusing the
	//    snapshot now beats publishing half of it later.
	if databaseSize+productionPortabilityReserveBytes > librarypath.MaxFileBytes {
		discard()
		return refuse(productionPortabilityCodeLimit)
	}
	facts, factsErr := productionPortabilityReadFacts(ctx, snapshotPath)
	if factsErr != nil {
		discard()
		return refuse(factsErr.Error())
	}
	generation := evidence.ContentIdentity.Database
	freezeBytes, freezeErr := productionPortabilityFreezeDocument(current.ProjectID, generation)
	if freezeErr != nil {
		discard()
		return refuse(productionPortabilityCodeInvalid)
	}
	authorityBytes, receiptID, authorityErr := productionPortabilityAuthorityDocument(current.ProjectID, generation, databaseDigest, facts)
	if authorityErr != nil {
		discard()
		return refuse(productionPortabilityCodeInvalid)
	}
	entries := []archiveformat.Entry{
		{Path: productionPortabilityDatabaseEntry, ByteSize: uint64(databaseSize), SHA256: databaseDigest, Disposition: productionPortabilityDatabaseDisposition},
		{Path: productionPortabilityFreezeEntry, ByteSize: uint64(len(freezeBytes)), SHA256: productionPortabilityDigestOf(freezeBytes), Disposition: productionPortabilityFreezeDisposition},
		{Path: productionPortabilityAuthorityEntry, ByteSize: uint64(len(authorityBytes)), SHA256: productionPortabilityDigestOf(authorityBytes), Disposition: productionPortabilityAuthorityDisposition},
	}
	total := entries[0].ByteSize + entries[1].ByteSize + entries[2].ByteSize
	manifest := archiveformat.Manifest{
		FormatVersion:             1,
		SourceSchemaVersion:       7,
		OriginProjectID:           current.ProjectID,
		SnapshotGeneration:        generation,
		CreatedAt:                 productionPortabilityTimestamp(b.now()),
		DeclaredUncompressedBytes: total,
		DeclaredCompressedBytes:   total,
		Entries:                   entries,
		SemanticCounts:            productionPortabilitySemanticCounts(facts),
		OmittedItems:              productionPortabilityOmitted(),
		TransferReceiptIDs:        []string{receiptID},
		HistoryProjectionIDs:      []string{},
	}
	if _, manifestErr := manifest.Bytes(archiveformat.DefaultLimits()); manifestErr != nil {
		discard()
		return refuse(productionPortabilityCodeInvalid)
	}
	archivePath := filepath.Join(staging, "portable"+productionPortabilityArchiveExtension)
	archiveFile, createErr := os.OpenFile(archivePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, productionProjectCreateFileMode)
	if createErr != nil {
		discard()
		return refuse("PRODUCTION_ARCHIVE_STAGING_UNAVAILABLE")
	}
	snapshot := archiveformat.ExportSnapshot{
		Manifest:          manifest,
		RuntimeFreeze:     freezeBytes,
		TransferAuthority: authorityBytes,
		Evidence: archiveformat.SourceEvidence{
			SchemaVersion:     evidence.UserVersion,
			SchemaFingerprint: evidence.SchemaFingerprint,
			TableCount:        facts.tableCount,
			FieldCount:        facts.fieldCount,
		},
		Sources: []archiveformat.ExportSource{
			{Entry: entries[0], Open: productionPortabilityEntryOpener(snapshotPath)},
			{Entry: entries[1], Open: func() (io.ReadCloser, error) {
				return io.NopCloser(bytes.NewReader(freezeBytes)), nil
			}},
			{Entry: entries[2], Open: func() (io.ReadCloser, error) {
				return io.NopCloser(bytes.NewReader(authorityBytes)), nil
			}},
		},
		// The lease must still be the caller's at both ends of the write, and the
		// source file set must be the same bytes the snapshot was taken from.
		CheckCurrent: func(checkContext context.Context) error {
			live, liveErr := productionPortabilityLiveLease(checkContext, b.open, owner, current)
			if !live || liveErr != nil {
				return errors.New("PROJECT_SESSION_STALE")
			}
			return nil
		},
		VerifyUnchanged: proof.VerifyUnchanged,
	}
	archiveName := productionPortabilityArchiveName(listed)
	attempt := &transport.ArchiveExportAttempt{
		Snapshot: snapshot,
		Writer:   archiveFile,
		Commit: func(receipt archiveformat.ExportReceipt) (string, string, error) {
			// Commit owns the staging directory's lifetime: transport does not call
			// Abort after a successful Commit, and a copy left behind on disk is
			// exactly the leak a private staging area is supposed to prevent.
			defer discard()
			return b.publishExport(ctx, productionPortabilityPublish{
				roots:          roots,
				directory:      directory,
				archivePath:    archivePath,
				archiveName:    archiveName,
				receipt:        receipt,
				manifest:       manifest,
				freezeBytes:    freezeBytes,
				authorityBytes: authorityBytes,
				receiptID:      receiptID,
				registry:       registry,
				binding:        binding,
				grantID:        grantID,
				owner:          owner,
			})
		},
		Abort: func() error {
			discard()
			return nil
		},
	}
	return attempt, nil
}

// productionPortabilityPublish is everything Commit needs to prove one published
// archive and mint exactly one URL for it.
type productionPortabilityPublish struct {
	roots          librarypath.Roots
	directory      librarypath.Target
	archivePath    string
	archiveName    string
	receipt        archiveformat.ExportReceipt
	manifest       archiveformat.Manifest
	freezeBytes    []byte
	authorityBytes []byte
	receiptID      string
	registry       *librarypath.Registry
	binding        librarypath.Binding
	grantID        string
	owner          projectsession.Owner
}

// publishExport proves the bytes on disk are the receipt's bytes, proves the
// archive reads back as an archive with this manifest, publishes it as one new
// file in the granted directory, proves the published copy again, and only then
// spends the grant and mints a download token.
func (b *productionPortability) publishExport(ctx context.Context, publish productionPortabilityPublish) (string, string, error) {
	receipt := publish.receipt
	size, digest, digestErr := productionCloudBackupDigestFile(publish.archivePath)
	if digestErr != nil || uint64(size) != receipt.TargetByteSize || digest != receipt.TargetSHA256 {
		log.Printf("novel-server: %s refused: %s", productionPortabilityExportName, "PRODUCTION_EXPORT_RECEIPT_MISMATCH")
		return "", "", errors.New(productionPortabilityCodeInvalid)
	}
	// Read the archive back as a reader of the format, not as the writer of it:
	// the manifest, every entry digest and the receipt binding are proved from
	// the published bytes.
	readback, readErr := os.Open(publish.archivePath)
	if readErr != nil {
		return "", "", readErr
	}
	archive, inspectErr := archiveformat.Inspect(readback, size, archiveformat.DefaultLimits())
	closeErr := readback.Close()
	if inspectErr != nil || closeErr != nil {
		log.Printf("novel-server: %s refused: %s", productionPortabilityExportName, "PRODUCTION_EXPORT_READBACK_REFUSED")
		return "", "", errors.New(productionPortabilityCodeInvalid)
	}
	held := archive.Manifest()
	if held.OriginProjectID != publish.manifest.OriginProjectID || held.SnapshotGeneration != publish.manifest.SnapshotGeneration ||
		held.CreatedAt != publish.manifest.CreatedAt || len(held.Entries) != 3 ||
		len(held.TransferReceiptIDs) != 1 || held.TransferReceiptIDs[0] != publish.receiptID {
		log.Printf("novel-server: %s refused: %s", productionPortabilityExportName, "PRODUCTION_EXPORT_READBACK_MISMATCH")
		return "", "", errors.New(productionPortabilityCodeInvalid)
	}
	if _, _, boundaryErr := archiveformat.ValidatePortableBoundary(held, publish.freezeBytes, publish.authorityBytes); boundaryErr != nil {
		log.Printf("novel-server: %s refused: %s", productionPortabilityExportName, "PRODUCTION_EXPORT_BOUNDARY_REFUSED")
		return "", "", errors.New(productionPortabilityCodeInvalid)
	}
	if int64(size) > librarypath.MaxFileBytes {
		return "", "", errors.New(productionPortabilityCodeLimit)
	}
	data, dataErr := os.ReadFile(publish.archivePath)
	if dataErr != nil {
		return "", "", dataErr
	}
	combined, joinErr := librarypath.JoinRelative(publish.directory.Relative, publish.archiveName)
	if joinErr != nil {
		return "", "", joinErr
	}
	published, resolveErr := publish.roots.Resolve(publish.directory.RootID, combined)
	if resolveErr != nil {
		return "", "", resolveErr
	}
	// One export is one new file. Replacing an existing archive would destroy the
	// copy it is supposed to be a second of.
	if published.Exists {
		log.Printf("novel-server: %s refused: %s", productionPortabilityExportName, productionPortabilityCodeTargetExists)
		return "", "", errors.New(productionPortabilityCodeTargetExists)
	}
	if writeErr := published.WriteOutput(data, true); writeErr != nil {
		return "", "", writeErr
	}
	writtenSize, writtenDigest, writtenErr := productionCloudBackupDigestFile(published.Path)
	if writtenErr != nil || writtenSize != int64(size) || writtenDigest != receipt.TargetSHA256 {
		// The file that landed is not the file that was proved: remove it rather
		// than leave an archive whose digest nobody can trust.
		_ = os.Remove(published.Path)
		log.Printf("novel-server: %s refused: %s", productionPortabilityExportName, "PRODUCTION_EXPORT_PUBLISHED_MISMATCH")
		return "", "", errors.New(productionPortabilityCodeInvalid)
	}
	if ctx == nil || ctx.Err() != nil {
		_ = os.Remove(published.Path)
		return "", "", errors.New(productionPortabilityCodeSourceChanged)
	}
	// The grant is one-use and is spent at the last safe moment: after the bytes
	// are durable and proved, and before any URL exists.
	if _, spendErr := publish.registry.Spend(publish.grantID, publish.binding, librarypath.OpWrite); spendErr != nil {
		_ = os.Remove(published.Path)
		return "", "", spendErr
	}
	token, tokenErr := b.mintDownload(publish.owner.SessionID, combined, publish.archiveName)
	if tokenErr != nil {
		return "", "", tokenErr
	}
	return productionPortabilityDownloadPrefix + token, publish.archiveName, nil
}

// mintDownload registers one URL capability for the session that asked for the
// export, and prunes what has expired.
func (b *productionPortability) mintDownload(sessionID, relative, name string) (string, error) {
	if b == nil || sessionID == "" || relative == "" || name == "" {
		return "", errors.New("DOWNLOAD_CLIENT_NOT_OWNED")
	}
	raw := make([]byte, 24)
	if _, readErr := rand.Read(raw); readErr != nil {
		return "", readErr
	}
	token := hex.EncodeToString(raw)
	now := b.now()
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.downloads == nil {
		b.downloads = make(map[string]productionPortabilityDownload)
	}
	for key, item := range b.downloads {
		if !now.Before(item.expires) {
			delete(b.downloads, key)
		}
	}
	b.downloads[token] = productionPortabilityDownload{
		sessionID: sessionID,
		relative:  relative,
		name:      name,
		expires:   now.Add(productionPortabilityDownloadTTL),
	}
	return token, nil
}

// download serves one minted URL. The token is the capability, and it is bound to
// the session that minted it: a stranger with the link and no cookie gets the
// same 404 an expired link gets.
func (b *productionPortability) download(w http.ResponseWriter, r *http.Request) error {
	if b == nil || b.server == nil || b.open == nil {
		return security.NewHTTPError(http.StatusServiceUnavailable, productionPortabilityClosedCode)
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return security.NewHTTPError(http.StatusMethodNotAllowed, "DOWNLOAD_METHOD_NOT_ALLOWED")
	}
	token := strings.TrimPrefix(r.URL.Path, productionPortabilityDownloadPrefix)
	if token == r.URL.Path || token == "" || len(token) > 256 || strings.ContainsAny(token, "/\\") {
		return security.NewHTTPError(http.StatusNotFound, "DOWNLOAD_EXPIRED")
	}
	principal, authErr := security.Authenticate(r)
	if authErr != nil {
		return authErr
	}
	session, sessionErr := b.server.SessionStore().Get(r, w, principal)
	if sessionErr != nil {
		return sessionErr
	}
	now := b.now()
	b.mu.Lock()
	item, minted := b.downloads[token]
	b.mu.Unlock()
	if !minted || item.sessionID != session.ID() || !now.Before(item.expires) {
		return security.NewHTTPError(http.StatusNotFound, "DOWNLOAD_EXPIRED")
	}
	_, library, _, _, laneErr := b.backup.lane()
	if laneErr != nil || library == nil {
		return security.NewHTTPError(http.StatusServiceUnavailable, productionPortabilityClosedCode)
	}
	roots, rootsErr := librarypath.NewRoots([]librarypath.Root{{
		ID:   librarypath.LibraryRootID,
		Kind: librarypath.KindLibrary,
		Path: library.Path(),
	}})
	if rootsErr != nil {
		return security.NewHTTPError(http.StatusServiceUnavailable, productionPortabilityClosedCode)
	}
	target, resolveErr := roots.Resolve(librarypath.LibraryRootID, item.relative)
	if resolveErr != nil {
		return security.NewHTTPError(http.StatusNotFound, "DOWNLOAD_EXPIRED")
	}
	file, size, openErr := productionPortabilityOpenBinary(target, librarypath.MaxFileBytes)
	if openErr != nil {
		return security.NewHTTPError(http.StatusNotFound, "DOWNLOAD_EXPIRED")
	}
	defer func() { _ = file.Close() }()
	header := w.Header()
	header.Set("Content-Type", "application/octet-stream")
	header.Set("Content-Length", strconv.FormatInt(size, 10))
	header.Set("Content-Disposition", "attachment; filename=\"download"+productionPortabilityExtension(item.name)+"\"; filename*=UTF-8''"+url.PathEscape(item.name))
	header.Set("Cache-Control", "no-store")
	header.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return nil
	}
	_, copyErr := io.Copy(w, file)
	if copyErr != nil {
		// The status line and the length are already on the wire, so a body
		// describing the failure would corrupt the file the browser is saving.
		// The broken transfer is the browser's own signal; the reason is logged.
		log.Printf("novel-server: %s: %s", productionPortabilityDownloadPrefix, "PRODUCTION_DOWNLOAD_TRANSFER_INCOMPLETE")
	}
	return nil
}

// productionPortabilityOpenBinary opens a regular file through a pinned parent
// and proves it is one. librarypath's reader is text-only
// (Target.ReadBytes refuses anything that is not UTF-8), so an archive - which
// is binary by construction - is read through os.Root instead, exactly the layer
// librarypath itself uses.
func productionPortabilityOpenBinary(target librarypath.Target, limit int64) (*os.File, int64, error) {
	if !target.Valid() || target.Path == "" {
		return nil, 0, librarypath.ErrPathInvalid
	}
	if !target.Exists || target.IsDir {
		return nil, 0, librarypath.ErrPathNotDirectory
	}
	if target.Size <= 0 || (limit > 0 && target.Size > limit) {
		return nil, 0, librarypath.ErrTooLarge
	}
	parent, name := filepath.Dir(target.Path), filepath.Base(target.Path)
	root, rootErr := os.OpenRoot(parent)
	if rootErr != nil {
		return nil, 0, rootErr
	}
	defer func() { _ = root.Close() }()
	info, statErr := root.Lstat(name)
	if statErr != nil {
		return nil, 0, statErr
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, 0, librarypath.ErrSymlinkEscape
	}
	file, openErr := root.Open(name)
	if openErr != nil {
		return nil, 0, openErr
	}
	held, heldErr := file.Stat()
	if heldErr != nil {
		_ = file.Close()
		return nil, 0, heldErr
	}
	if !held.Mode().IsRegular() || held.Size() != info.Size() || held.Size() <= 0 {
		_ = file.Close()
		return nil, 0, librarypath.ErrChanged
	}
	return file, held.Size(), nil
}

// ---------------------------------------------------------------------------
// project:archive-restore
// ---------------------------------------------------------------------------

// restore answers project:archive-restore. Two opaque grant ids arrive and no
// path: the archive is the file the archive picker granted, the destination is
// the directory the target picker granted, and both are re-resolved through the
// library allowlist for this caller.
//
// The order is the point. Prove the archive, prove its boundary, prove its
// schema, discharge its receipt, protect whatever already exists, and only then
// spend a grant or create one directory. Every refusal is a business answer at
// HTTP 200 with a code from the closed union, because the page renders it.
func (b *productionPortability) restore(ctx context.Context, caller transport.Caller, args []any) (any, error) {
	fail := func(code string) (any, error) {
		log.Printf("novel-server: %s refused: %s", productionPortabilityRestoreName, code)
		return map[string]any{"success": false, "error": code, "errorCode": code}, nil
	}
	if b == nil || b.open == nil || b.backup == nil {
		return fail(productionPortabilityCodeContext)
	}
	if len(args) != 1 {
		return fail(productionPortabilityCodeContext)
	}
	request, ok := args[0].(map[string]any)
	if !ok || len(request) != 2 {
		return fail(productionPortabilityCodeContext)
	}
	archiveGrantID, archiveOK := request["archiveGrantId"].(string)
	targetGrantID, targetOK := request["targetGrantId"].(string)
	if !archiveOK || !targetOK || archiveGrantID == "" || targetGrantID == "" ||
		len(archiveGrantID) > 512 || len(targetGrantID) > 512 ||
		!utf8.ValidString(archiveGrantID) || !utf8.ValidString(targetGrantID) {
		return fail(productionPortabilityCodeGrant)
	}
	durable, library, scratch, _, laneErr := b.backup.lane()
	if laneErr != nil || durable == nil || library == nil || scratch == "" {
		return fail(productionPortabilityClosedCode)
	}
	owner, ownerErr := productionOwner(caller)
	if ownerErr != nil {
		return fail("PROJECT_SESSION_REQUIRED")
	}
	registry, registryErr := productionFSGrantsRegistry()
	if registryErr != nil || registry == nil {
		return fail(productionPortabilityCodeGrant)
	}
	binding := librarypath.Binding{SessionID: owner.SessionID, ClientID: owner.ClientID}
	archiveGrant, archivePeekErr := registry.Peek(archiveGrantID, binding, librarypath.OpRead)
	if archivePeekErr != nil {
		return fail(productionPortabilityCodeGrant)
	}
	targetGrant, targetPeekErr := registry.Peek(targetGrantID, binding, librarypath.OpRead)
	if targetPeekErr != nil {
		return fail(productionPortabilityCodeGrant)
	}
	// 1. Both grants are paths, and both are resolved here rather than trusted.
	source, sourceErr := productionPortabilityArchiveTarget(library, archiveGrant)
	if sourceErr != nil {
		return fail(productionPortabilityCodeGrant)
	}
	if source.Size > librarypath.MaxFileBytes {
		return fail(productionPortabilityCodeLimit)
	}
	destination, destinationErr := b.backup.restoreTarget(library, targetGrant)
	if destinationErr != nil {
		return fail(productionPortabilityCodeRestoreUnsafe)
	}
	// 2. One private staging directory holds the incoming copy, the extracted
	//    entries and nothing else. It never contains a path the browser named.
	staging, stagingErr := os.MkdirTemp(scratch, "project-portability-restore-")
	if stagingErr != nil {
		return fail(productionPortabilityCodeRestoreInvalid)
	}
	defer func() { _ = os.RemoveAll(staging) }()
	archivePath := filepath.Join(staging, "incoming"+productionPortabilityArchiveExtension)
	if copyErr := productionPortabilityCopyBinary(source, archivePath); copyErr != nil {
		return fail(productionPortabilityCodeRestoreInvalid)
	}
	archiveSize, _, digestErr := productionCloudBackupDigestFile(archivePath)
	if digestErr != nil || archiveSize <= 0 {
		return fail(productionPortabilityCodeRestoreInvalid)
	}
	// 3. The format speaks first: the archive must read as one archive, with
	//    exactly the three entries a restorable copy has.
	archiveSource, sourceOpenErr := os.Open(archivePath)
	if sourceOpenErr != nil {
		return fail(productionPortabilityCodeRestoreInvalid)
	}
	portable, inspectErr := productionPortabilityInspect(archiveSource, archiveSize)
	if inspectErr != nil {
		_ = archiveSource.Close()
		return fail(productionPortabilityArchiveCode(inspectErr))
	}
	manifest := portable.Manifest()
	// 4. Exactly one extraction, admitting only the three names this lane wrote.
	extractDirectory := filepath.Join(staging, "extract")
	if mkdirErr := os.Mkdir(extractDirectory, productionProjectCreateDirectoryMode); mkdirErr != nil {
		_ = archiveSource.Close()
		return fail(productionPortabilityCodeRestoreInvalid)
	}
	stageErr := portable.Stage(&productionPortabilityStage{directory: extractDirectory})
	closeErr := archiveSource.Close()
	if stageErr != nil || closeErr != nil {
		return fail(productionPortabilityCodeRestoreInvalid)
	}
	databasePath := filepath.Join(extractDirectory, productionPortabilityDatabaseEntry)
	freezeBytes, freezeErr := productionPortabilityReadSidecar(extractDirectory, productionPortabilityFreezeEntry)
	if freezeErr != nil {
		return fail(productionPortabilityCodeRestoreInvalid)
	}
	authorityBytes, authorityErr := productionPortabilityReadSidecar(extractDirectory, productionPortabilityAuthorityEntry)
	if authorityErr != nil {
		return fail(productionPortabilityCodeRestoreInvalid)
	}
	// 5. The sidecars must agree with the manifest, and the receipt must be the
	//    one the manifest names. A document that contradicts its own archive is
	//    refused whole rather than reconciled.
	guard, authority, boundaryErr := archiveformat.ValidatePortableBoundary(manifest, freezeBytes, authorityBytes)
	if boundaryErr != nil {
		// A freeze or receipt document that contradicts its own archive is a
		// restore refusal, not an archive-format one: the ZIP is intact.
		return fail(productionPortabilityCodeRestoreInvalid)
	}
	if authority == nil || !guard.Active() {
		return fail(productionPortabilityCodeRestoreInvalid)
	}
	if len(manifest.TransferReceiptIDs) != 1 || manifest.TransferReceiptIDs[0] != authority.ReceiptID {
		return fail(productionPortabilityCodeInvalid)
	}
	// This lane cannot replay a frozen non-replayable record, so an archive that
	// carries one is refused instead of installed with its history dropped.
	if productionPortabilityFrozenRecords(freezeBytes) {
		return fail(productionPortabilityCodeInvalid)
	}
	// 6. The bytes being installed must be the bytes the receipt binds.
	evidence, admitErr := admission.VerifySnapshot(ctx, databasePath, b.config.Schema)
	if admitErr != nil || evidence.UserVersion != 7 {
		return fail(productionPortabilityCodeSchema)
	}
	databaseEntry, held := manifest.Entries[0], false
	for _, entry := range manifest.Entries {
		if entry.Disposition == productionPortabilityDatabaseDisposition {
			databaseEntry, held = entry, true
			break
		}
	}
	if !held || evidence.ContentIdentity.Database != authority.PortableDatabaseSHA256 ||
		evidence.ContentIdentity.Database != databaseEntry.SHA256 {
		return fail(productionPortabilityCodeInvalid)
	}
	if dischargeErr := productionPortabilityDischarge(ctx, databasePath, authority); dischargeErr != nil {
		return fail(productionPortabilityCodeInvalid)
	}
	// 7. Author data is never overwritten. A directory that already holds a
	//    project is copied first and then refused; a directory that is not empty
	//    is somebody's data and is refused without being touched.
	if productionCloudBackupProjectPresent(destination.path) {
		if _, copyErr := b.backup.preRestoreCopy(ctx, library, destination.path); copyErr != nil {
			return fail(productionPortabilityCodeRestoreExists)
		}
		return fail(productionPortabilityCodeRestoreExists)
	}
	empty, emptyErr := productionCloudBackupTargetEmpty(destination.path)
	if emptyErr != nil || !empty {
		return fail(productionPortabilityCodeRestoreUnsafe)
	}
	// 8. Spend both grants at the last safe moment: nothing above this line
	//    created a file, and everything below it is undoable.
	if _, spendErr := registry.Spend(targetGrantID, binding, librarypath.OpRead); spendErr != nil {
		return fail(productionPortabilityCodeGrant)
	}
	if _, spendErr := registry.Spend(archiveGrantID, binding, librarypath.OpRead); spendErr != nil {
		return fail(productionPortabilityCodeGrant)
	}
	installed, installErr := productionCloudBackupInstall(ctx, destination.path, databasePath)
	if installErr != nil {
		return fail(productionPortabilityCodeRestoreInvalid)
	}
	listed, inventoryCode := productionProjectCreateInventory(ctx, library, destination.path)
	if inventoryCode != "" {
		_ = productionCloudBackupRemoveInstalled(installed)
		return fail(productionPortabilityCodeRestoreInvalid)
	}
	verified, verifyErr := admitProductionProject(ctx, library, listed, scratch, productionConfig{
		Schema: b.config.Schema, MaxDatabaseBytes: b.config.MaxDatabaseBytes,
	})
	if verifyErr != nil || verified.ContentIdentity.Database != authority.PortableDatabaseSHA256 {
		_ = productionCloudBackupRemoveInstalled(installed)
		return fail(productionPortabilityCodeRestoreInvalid)
	}
	// The two sidecars are deliberately NOT installed: a project that carries
	// installed portable authority is unopenable in this lane
	// (internal/projectfs/authority.go), and the receipt was discharged above
	// against the very bytes that are now on disk.
	recentUpdated := b.backup.register(listed, verified)
	if !recentUpdated {
		// The copy is proved author data whether or not this process could mint a
		// live catalogue key for it, so a lost key is a warning, not a rollback.
		log.Printf("novel-server: %s: %s", productionPortabilityRestoreName, "PRODUCTION_RESTORE_CATALOG_UNREGISTERED")
	}
	receipt := map[string]any{
		"originProjectId":            authority.OriginProjectID,
		"targetProjectId":            listed.ProjectID,
		"targetProjectRoot":          destination.relative,
		"snapshotGeneration":         manifest.SnapshotGeneration,
		"portableDatabaseSha256":     authority.PortableDatabaseSHA256,
		"requiresRuntimeFreezeGuard": true,
	}
	answer := map[string]any{
		"success":              true,
		"state":                "restore-complete",
		"receipt":              receipt,
		"recentProjectUpdated": recentUpdated,
	}
	if !recentUpdated {
		answer["warning"] = "PRODUCTION_RESTORE_CATALOG_UNREGISTERED"
	}
	return answer, nil
}

// productionPortabilityArchiveTarget resolves an archive grant onto a regular
// file inside the library allowlist. The picker grants the file it chose; a
// grant that now names a directory, a link or nothing is not an archive.
func productionPortabilityArchiveTarget(library *projectfs.Library, grant librarypath.Grant) (librarypath.Target, error) {
	if library == nil {
		return librarypath.Target{}, librarypath.ErrRootInvalid
	}
	roots, rootsErr := librarypath.NewRoots([]librarypath.Root{{
		ID:   librarypath.LibraryRootID,
		Kind: librarypath.KindLibrary,
		Path: library.Path(),
	}})
	if rootsErr != nil {
		return librarypath.Target{}, rootsErr
	}
	target, resolveErr := roots.Resolve(grant.Target.RootID, grant.Target.Relative)
	if resolveErr != nil {
		return librarypath.Target{}, resolveErr
	}
	if target.Kind != librarypath.KindLibrary || !target.Exists || target.IsDir || target.Size <= 0 {
		return librarypath.Target{}, librarypath.ErrTargetInvalid
	}
	return target, nil
}

// productionPortabilityArchiveCode narrows the format's own refusal vocabulary
// onto the closed browser union. The codec knows more ways to refuse than the
// page has cases for, so anything else is answered as PORTABLE_ARCHIVE_INVALID:
// the archive did not survive inspection, which is exactly what that member
// means to the author looking at the answer.
func productionPortabilityArchiveCode(err error) string {
	code := archiveformat.Code(err)
	switch code {
	case productionPortabilityCodeInvalid, productionPortabilityCodeLimit, productionPortabilityCodeSchema:
		return code
	}
	return productionPortabilityCodeInvalid
}

// productionPortabilityInspect admits an archive file as this lane's portable
// format: exactly the three entries, in the only dispositions they may have, and
// a receipt list that names the transfer authority it carries.
func productionPortabilityInspect(source io.ReaderAt, size int64) (*archiveformat.Archive, error) {
	if source == nil || size <= 0 {
		return nil, &archiveformat.Error{Code: productionPortabilityCodeInvalid}
	}
	archive, inspectErr := archiveformat.Inspect(source, size, archiveformat.DefaultLimits())
	if inspectErr != nil {
		return nil, inspectErr
	}
	manifest := archive.Manifest()
	if len(manifest.Entries) != 3 {
		return nil, &archiveformat.Error{Code: productionPortabilityCodeInvalid}
	}
	// Match by name, not by position: the format writes one order, but a reader
	// must admit every archive that names exactly these three entries.
	dispositions := map[string]string{
		productionPortabilityDatabaseEntry:  productionPortabilityDatabaseDisposition,
		productionPortabilityFreezeEntry:    productionPortabilityFreezeDisposition,
		productionPortabilityAuthorityEntry: productionPortabilityAuthorityDisposition,
	}
	for _, entry := range manifest.Entries {
		want, known := dispositions[entry.Path]
		if !known || entry.Disposition != want {
			return nil, &archiveformat.Error{Code: productionPortabilityCodeInvalid}
		}
	}
	if len(manifest.TransferReceiptIDs) != 1 || len(manifest.HistoryProjectionIDs) != 0 {
		return nil, &archiveformat.Error{Code: productionPortabilityCodeInvalid}
	}
	return archive, nil
}

// productionPortabilityStage extracts exactly three admitted entries into one
// private directory, exclusive-create, refusing any name it did not expect.
type productionPortabilityStage struct {
	directory string
	opened    int
	complete  bool
}

// Open creates one entry file. The names are the archive's, and only the three
// this lane admits may exist here; a fourth would be a fourth file in the
// restored tree.
func (s *productionPortabilityStage) Open(entry archiveformat.Entry) (io.WriteCloser, error) {
	if s == nil || s.directory == "" || s.complete || s.opened >= 3 {
		return nil, errors.New(productionPortabilityCodeInvalid)
	}
	relative := ""
	switch {
	case strings.EqualFold(entry.Path, productionPortabilityDatabaseEntry) && entry.Disposition == productionPortabilityDatabaseDisposition:
		relative = productionPortabilityDatabaseEntry
	case strings.EqualFold(entry.Path, productionPortabilityFreezeEntry) && entry.Disposition == productionPortabilityFreezeDisposition:
		relative = productionPortabilityFreezeEntry
	case strings.EqualFold(entry.Path, productionPortabilityAuthorityEntry) && entry.Disposition == productionPortabilityAuthorityDisposition:
		relative = filepath.FromSlash(productionPortabilityAuthorityEntry)
	default:
		return nil, errors.New(productionPortabilityCodeInvalid)
	}
	path := filepath.Join(s.directory, relative)
	if mkdirErr := os.MkdirAll(filepath.Dir(path), productionProjectCreateDirectoryMode); mkdirErr != nil {
		return nil, mkdirErr
	}
	file, createErr := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, productionProjectCreateFileMode)
	if createErr != nil {
		return nil, createErr
	}
	s.opened++
	return file, nil
}

// Complete refuses an archive that never offered all three entries.
func (s *productionPortabilityStage) Complete(archiveformat.Manifest, archiveformat.Receipt) error {
	if s == nil || s.opened != 3 || s.complete {
		return errors.New(productionPortabilityCodeInvalid)
	}
	s.complete = true
	return nil
}

// Abort is the stage's own cleanup: the staging parent removes the tree.
func (s *productionPortabilityStage) Abort() error {
	if s == nil {
		return nil
	}
	s.complete = false
	return nil
}

// productionPortabilityReadSidecar reads one staged sidecar, bounded.
func productionPortabilityReadSidecar(directory, relative string) ([]byte, error) {
	path := filepath.Join(directory, filepath.FromSlash(relative))
	info, statErr := os.Lstat(path)
	if statErr != nil {
		return nil, statErr
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > productionPortabilitySidecarBytes {
		return nil, errors.New(productionPortabilityCodeInvalid)
	}
	return os.ReadFile(path)
}

// productionPortabilityFrozenRecords reports whether a freeze document claims any
// frozen non-replayable record. This lane can install the bytes but cannot replay
// a projection, so an archive that carries one is refused rather than silently
// stripped of its history.
func productionPortabilityFrozenRecords(freezeBytes []byte) bool {
	decoded := struct {
		Records                    []json.RawMessage `json:"records"`
		AvatarReferenceProjections []json.RawMessage `json:"avatarReferenceProjections"`
	}{}
	if err := json.Unmarshal(freezeBytes, &decoded); err != nil {
		return true
	}
	return len(decoded.Records) != 0 || len(decoded.AvatarReferenceProjections) != 0
}

// productionPortabilityDischarge proves the receipt is discharged by the bytes
// about to be installed: every finalization it names is still the current
// receipted finalization of its chapter in this database, and every summary
// source still names one of them. It is the same query the export used to build
// the receipt, run against the copy that will become the project.
func productionPortabilityDischarge(ctx context.Context, databasePath string, authority *archiveformat.TransferAuthority) error {
	handle, openErr := productionPortabilityOpenReadOnlyFile(databasePath)
	if openErr != nil {
		return openErr
	}
	defer func() { _ = handle.Close() }()
	facts, factsErr := productionPortabilityReadFactsFrom(ctx, handle)
	if factsErr != nil {
		return factsErr
	}
	if len(facts.finalizations) != len(authority.Finalizations) || len(facts.summaries) != len(authority.SummarySources) {
		return errors.New(productionPortabilityCodeInvalid)
	}
	for index, want := range authority.Finalizations {
		if facts.finalizations[index] != want {
			return errors.New(productionPortabilityCodeInvalid)
		}
	}
	for index, want := range authority.SummarySources {
		if facts.summaries[index] != want {
			return errors.New(productionPortabilityCodeInvalid)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// The portable facts, read the one way
// ---------------------------------------------------------------------------

// productionPortabilityReadFacts opens a verified snapshot read-only and reads
// the schema size, the per-table row counts and the receipted currentness facts.
func productionPortabilityReadFacts(ctx context.Context, databasePath string) (productionPortabilityFacts, error) {
	handle, openErr := productionPortabilityOpenReadOnlyFile(databasePath)
	if openErr != nil {
		return productionPortabilityFacts{}, openErr
	}
	defer func() { _ = handle.Close() }()
	return productionPortabilityReadFactsFrom(ctx, handle)
}

// productionPortabilityReadFactsFrom is the single implementation of "what the
// portable facts of this database are". Export writes it into the receipt and
// restore proves the receipt against it, so the two cannot disagree.
func productionPortabilityReadFactsFrom(ctx context.Context, handle *sql.DB) (productionPortabilityFacts, error) {
	facts := productionPortabilityFacts{
		counts:        make(map[string]uint64),
		finalizations: []archiveformat.FinalizationAuthority{},
		summaries:     []archiveformat.SummarySourceAuthority{},
	}
	if handle == nil {
		return facts, errors.New(productionPortabilityCodeInvalid)
	}
	// The desktop's own precondition: exactly one project_core row, and it is
	// 'main' (electron/services/portable-transfer-authority.ts:155-156).
	var cores, foreign int64
	if coreErr := handle.QueryRowContext(ctx, `SELECT COUNT(*), COUNT(CASE WHEN id <> 'main' THEN 1 END) FROM project_core`).Scan(&cores, &foreign); coreErr != nil {
		return facts, errors.New("PRODUCTION_PORTABILITY_CORE_MISSING")
	}
	if cores != 1 || foreign != 0 {
		return facts, errors.New("PRODUCTION_PORTABILITY_CORE_INVALID")
	}
	names, tablesErr := productionPortabilityTables(ctx, handle)
	if tablesErr != nil {
		return facts, tablesErr
	}
	for _, name := range names {
		var rows int64
		if countErr := handle.QueryRowContext(ctx, `SELECT COUNT(*) FROM "`+name+`"`).Scan(&rows); countErr != nil {
			return facts, countErr
		}
		if rows < 0 {
			return facts, errors.New(productionPortabilityCodeInvalid)
		}
		facts.counts[name] = uint64(rows)
		var fields int64
		if fieldErr := handle.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info(?)`, name).Scan(&fields); fieldErr != nil {
			return facts, fieldErr
		}
		facts.fieldCount += int(fields)
	}
	facts.tableCount = len(names)
	if facts.tableCount == 0 || facts.fieldCount == 0 {
		return facts, errors.New(productionPortabilityCodeInvalid)
	}
	finalizations, summaries, factErr := productionPortabilityCurrentness(ctx, handle)
	if factErr != nil {
		return facts, factErr
	}
	facts.finalizations = finalizations
	facts.summaries = summaries
	return facts, nil
}

// productionPortabilityTables lists the schema's tables, refusing any name the
// manifest's count grammar cannot carry: a table this lane cannot describe is a
// project it cannot export portably.
func productionPortabilityTables(ctx context.Context, handle *sql.DB) ([]string, error) {
	rows, queryErr := handle.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if queryErr != nil {
		return nil, queryErr
	}
	defer rows.Close()
	names := make([]string, 0, 64)
	for rows.Next() {
		var name string
		if scanErr := rows.Scan(&name); scanErr != nil {
			return nil, scanErr
		}
		if !productionPortabilityTableName(name) {
			return nil, errors.New(productionPortabilityCodeUnsafe)
		}
		names = append(names, name)
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return nil, rowsErr
	}
	return names, nil
}

// productionPortabilityTableName applies archiveformat's table grammar
// (^[a-z][a-z0-9_]{0,127}$) before a name is ever concatenated into a statement.
func productionPortabilityTableName(name string) bool {
	if len(name) == 0 || len(name) > 128 {
		return false
	}
	if name[0] < 'a' || name[0] > 'z' {
		return false
	}
	for index := 0; index < len(name); index++ {
		if strings.IndexByte(productionPortabilityTablePattern, name[index]) < 0 {
			return false
		}
	}
	return true
}

// productionPortabilityCurrentness is the receipt's own definition, translated
// from the desktop's `currentness` (electron/services/portable-transfer-
// authority.ts:154-209): the newest finalized draft of every chapter, its
// outbox receipt, and the summary snapshots that cite those receipts. A
// finalized draft without a receipt, a snapshot whose bytes are not the hash it
// claims, or a duplicated chapter is a refusal, never a partial receipt.
func productionPortabilityCurrentness(ctx context.Context, handle *sql.DB) ([]archiveformat.FinalizationAuthority, []archiveformat.SummarySourceAuthority, error) {
	finalizations := make([]archiveformat.FinalizationAuthority, 0, 32)
	rows, queryErr := handle.QueryContext(ctx, `SELECT o.finalization_id, d.id, o.chapter_number,
 o.content_hash, o.content_snapshot, d.chapter_number, d.status, c.body
 FROM drafts d JOIN contents c ON c.id = d.content_id
 LEFT JOIN finalization_outbox o ON o.draft_id = d.id
 WHERE d.status = 'finalized' AND NOT EXISTS (
 SELECT 1 FROM drafts newer WHERE newer.chapter_number = d.chapter_number
 AND newer.status = 'finalized'
 AND (newer.version > d.version OR (newer.version = d.version AND newer.id > d.id)))
 ORDER BY o.finalization_id`)
	if queryErr != nil {
		return nil, nil, errors.New("PRODUCTION_PORTABILITY_RECEIPT_UNREADABLE")
	}
	defer rows.Close()
	seenDrafts := make(map[int64]bool, 32)
	seenChapters := make(map[int64]bool, 32)
	for rows.Next() {
		var finalizationID, contentHash, contentSnapshot, status, body sql.NullString
		var draftID, chapterNumber, draftChapter sql.NullInt64
		if scanErr := rows.Scan(&finalizationID, &draftID, &chapterNumber, &contentHash, &contentSnapshot, &draftChapter, &status, &body); scanErr != nil {
			return nil, nil, scanErr
		}
		if !draftID.Valid || !chapterNumber.Valid || !draftChapter.Valid || draftID.Int64 <= 0 || chapterNumber.Int64 <= 0 {
			return nil, nil, errors.New(productionPortabilityCodeInvalid)
		}
		if !finalizationID.Valid || finalizationID.String == "" || !contentHash.Valid || !contentSnapshot.Valid {
			// A finalized draft with no outbox receipt is legacy prose, not a
			// source-bound receipt: it cannot travel as authority.
			return nil, nil, errors.New("PRODUCTION_PORTABILITY_RECEIPT_MISSING")
		}
		if status.String != "finalized" || draftChapter.Int64 != chapterNumber.Int64 {
			return nil, nil, errors.New(productionPortabilityCodeInvalid)
		}
		if !body.Valid || contentSnapshot.String != body.String || productionPortabilityDigestOf([]byte(contentSnapshot.String)) != contentHash.String {
			return nil, nil, errors.New(productionPortabilityCodeInvalid)
		}
		if seenDrafts[draftID.Int64] || seenChapters[chapterNumber.Int64] {
			return nil, nil, errors.New(productionPortabilityCodeInvalid)
		}
		seenDrafts[draftID.Int64] = true
		seenChapters[chapterNumber.Int64] = true
		finalizations = append(finalizations, archiveformat.FinalizationAuthority{
			FinalizationID: finalizationID.String,
			DraftID:        uint64(draftID.Int64),
			ChapterNumber:  uint64(chapterNumber.Int64),
			ContentHash:    contentHash.String,
		})
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return nil, nil, rowsErr
	}
	byID := make(map[string]archiveformat.FinalizationAuthority, len(finalizations))
	for _, item := range finalizations {
		byID[item.FinalizationID] = item
	}
	var summaries []archiveformat.SummarySourceAuthority
	if productionPortabilityHasTable(ctx, handle, "summary_snapshots") {
		summaryRows, summaryErr := handle.QueryContext(ctx, `SELECT s.id, s.draft_id, s.chapter_number,
 s.source_finalization_id, s.source_content_hash, s.projection_generation
 FROM summary_snapshots s JOIN drafts d ON d.id = s.draft_id
 WHERE (s.source_finalization_id <> '' OR s.source_content_hash <> '') AND d.status = 'finalized'
 AND NOT EXISTS (
 SELECT 1 FROM drafts newer WHERE newer.chapter_number = d.chapter_number
 AND newer.status = 'finalized'
 AND (newer.version > d.version OR (newer.version = d.version AND newer.id > d.id)))
 ORDER BY s.id`)
		if summaryErr != nil {
			return nil, nil, errors.New("PRODUCTION_PORTABILITY_RECEIPT_UNREADABLE")
		}
		defer summaryRows.Close()
		summaries = make([]archiveformat.SummarySourceAuthority, 0, len(finalizations))
		seenSummaries := make(map[int64]bool, len(finalizations))
		for summaryRows.Next() {
			var summaryID, draftID, chapterNumber, projectionGeneration sql.NullInt64
			var sourceFinalizationID, sourceContentHash sql.NullString
			if scanErr := summaryRows.Scan(&summaryID, &draftID, &chapterNumber, &sourceFinalizationID, &sourceContentHash, &projectionGeneration); scanErr != nil {
				return nil, nil, scanErr
			}
			if !summaryID.Valid || !draftID.Valid || !chapterNumber.Valid || !projectionGeneration.Valid ||
				summaryID.Int64 <= 0 || draftID.Int64 <= 0 || chapterNumber.Int64 <= 0 || projectionGeneration.Int64 < 0 {
				return nil, nil, errors.New(productionPortabilityCodeInvalid)
			}
			if !sourceFinalizationID.Valid || !sourceContentHash.Valid {
				return nil, nil, errors.New(productionPortabilityCodeInvalid)
			}
			// A snapshot must cite one of the receipts above, for its own draft and
			// chapter: a projection of a superseded finalization is not portable
			// authority, and pretending it is would put a stale fact in the receipt.
			held, known := byID[sourceFinalizationID.String]
			if !known || held.DraftID != uint64(draftID.Int64) || held.ChapterNumber != uint64(chapterNumber.Int64) ||
				held.ContentHash != sourceContentHash.String {
				return nil, nil, errors.New("PRODUCTION_PORTABILITY_SUMMARY_UNBOUND")
			}
			if seenSummaries[summaryID.Int64] {
				return nil, nil, errors.New(productionPortabilityCodeInvalid)
			}
			seenSummaries[summaryID.Int64] = true
			summaries = append(summaries, archiveformat.SummarySourceAuthority{
				SummaryID:            uint64(summaryID.Int64),
				DraftID:              uint64(draftID.Int64),
				ChapterNumber:        uint64(chapterNumber.Int64),
				SourceFinalizationID: sourceFinalizationID.String,
				SourceContentHash:    sourceContentHash.String,
				ProjectionGeneration: uint64(projectionGeneration.Int64),
			})
		}
		if summaryErr = summaryRows.Err(); summaryErr != nil {
			return nil, nil, summaryErr
		}
	}
	if summaries == nil {
		summaries = []archiveformat.SummarySourceAuthority{}
	}
	return finalizations, summaries, nil
}

// productionPortabilityHasTable reports whether the snapshot carries a table. An
// older database without summary_snapshots has no summary sources, which is a
// fact about it, not a failure.
func productionPortabilityHasTable(ctx context.Context, handle *sql.DB, name string) bool {
	if handle == nil || !productionPortabilityTableName(name) {
		return false
	}
	var found int64
	if scanErr := handle.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&found); scanErr != nil {
		return false
	}
	return found == 1
}

// ---------------------------------------------------------------------------
// The two sidecar documents
// ---------------------------------------------------------------------------

// productionPortabilityFreezeDocument writes the non-replayable history document
// with an empty projection. It is honest precisely because this lane refuses to
// export anything it could not project - see the file header.
func productionPortabilityFreezeDocument(projectID, generation string) ([]byte, error) {
	if projectID == "" || generation == "" {
		return nil, errors.New(productionPortabilityCodeInvalid)
	}
	document := struct {
		Version                    int              `json:"version"`
		OriginProjectID            string           `json:"originProjectId"`
		SnapshotGeneration         string           `json:"snapshotGeneration"`
		NonReplayable              bool             `json:"nonReplayable"`
		RequiresRuntimeFreezeGuard bool             `json:"requiresRuntimeFreezeGuard"`
		Records                    []map[string]any `json:"records"`
		AvatarReferenceProjections []map[string]any `json:"avatarReferenceProjections"`
	}{
		Version:                    1,
		OriginProjectID:            projectID,
		SnapshotGeneration:         generation,
		NonReplayable:              true,
		RequiresRuntimeFreezeGuard: true,
		Records:                    []map[string]any{},
		AvatarReferenceProjections: []map[string]any{},
	}
	encoded, encodeErr := productionPortabilityMarshal(document)
	if encodeErr != nil {
		return nil, encodeErr
	}
	if _, parseErr := archiveformat.ParseRuntimeFreeze(encoded); parseErr != nil {
		return nil, parseErr
	}
	return encoded, nil
}

// productionPortabilityAuthorityDocument writes the transfer receipt and proves
// it by parsing it back with the format's own reader before anyone can publish
// it. The receipt id is the digest the desktop derives, over the same payload in
// the same order, so an archive from this lane and an archive from that one are
// verified by the same rule.
func productionPortabilityAuthorityDocument(projectID, generation, databaseDigest string, facts productionPortabilityFacts) ([]byte, string, error) {
	if projectID == "" || generation == "" || !productionPortabilityDigest(databaseDigest) {
		return nil, "", errors.New(productionPortabilityCodeInvalid)
	}
	payload := struct {
		OriginProjectID        string                                `json:"originProjectId"`
		SnapshotGeneration     string                                `json:"snapshotGeneration"`
		PortableDatabaseSHA256 string                                `json:"portableDatabaseSha256"`
		Finalizations          []archiveformat.FinalizationAuthority  `json:"finalizations"`
		SummarySources         []archiveformat.SummarySourceAuthority `json:"summarySources"`
	}{
		OriginProjectID:        projectID,
		SnapshotGeneration:     generation,
		PortableDatabaseSHA256: databaseDigest,
		Finalizations:          facts.finalizations,
		SummarySources:         facts.summaries,
	}
	encoded, encodeErr := productionPortabilityMarshal(payload)
	if encodeErr != nil {
		return nil, "", encodeErr
	}
	digest := sha256.Sum256(encoded)
	receiptID := "portable-transfer:" + hex.EncodeToString(digest[:])[:32]
	document := archiveformat.TransferAuthority{
		Version:                    1,
		ReceiptID:                  receiptID,
		OriginProjectID:            projectID,
		TargetProjectID:            nil,
		SnapshotGeneration:         generation,
		SourceSchemaVersion:        7,
		PortableDatabaseSHA256:     databaseDigest,
		RequiresRuntimeFreezeGuard: true,
		Finalizations:              facts.finalizations,
		SummarySources:             facts.summaries,
	}
	authority, marshalErr := productionPortabilityMarshal(document)
	if marshalErr != nil {
		return nil, "", marshalErr
	}
	if _, parseErr := archiveformat.ParseTransferAuthority(authority); parseErr != nil {
		return nil, "", parseErr
	}
	return authority, receiptID, nil
}

// productionPortabilitySemanticCounts names every table the way the manifest
// grammar expects: "table.<name>", which is also the key the desktop writes.
func productionPortabilitySemanticCounts(facts productionPortabilityFacts) map[string]uint64 {
	counts := make(map[string]uint64, len(facts.counts))
	for name, rows := range facts.counts {
		counts["table."+name] = rows
	}
	return counts
}

// ---------------------------------------------------------------------------
// Small proofs
// ---------------------------------------------------------------------------

// productionPortabilityEntryOpener re-opens one staged entry per read, so the
// export never holds a descriptor across the write.
func productionPortabilityEntryOpener(path string) func() (io.ReadCloser, error) {
	return func() (io.ReadCloser, error) {
		return os.Open(path)
	}
}

// productionPortabilityCopyBinary copies one admitted file into the private
// stage and proves the copy by digest.
func productionPortabilityCopyBinary(source librarypath.Target, destination string) error {
	file, size, openErr := productionPortabilityOpenBinary(source, librarypath.MaxFileBytes)
	if openErr != nil {
		return openErr
	}
	defer func() { _ = file.Close() }()
	output, createErr := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, productionProjectCreateFileMode)
	if createErr != nil {
		return createErr
	}
	_, copyErr := io.Copy(output, file)
	if copyErr == nil {
		if syncErr := output.Sync(); syncErr != nil {
			copyErr = syncErr
		}
	}
	closeErr := output.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	copied, digest, copiedErr := productionCloudBackupDigestFile(destination)
	if copiedErr != nil || copied != size {
		return errors.New(productionPortabilityCodeRestoreInvalid)
	}
	if !productionPortabilityDigest(digest) {
		return errors.New(productionPortabilityCodeRestoreInvalid)
	}
	return nil
}

// productionPortabilityLiveLease answers whether the caller still holds the lease
// this export started under.
func productionPortabilityLiveLease(ctx context.Context, open *productionOpen, owner projectsession.Owner, expected projectsession.Context) (bool, error) {
	if open == nil || open.durable == nil {
		return false, errors.New(productionPortabilityClosedCode)
	}
	current, present, leaseErr := open.durable.productionFSGrantProject(ctx, owner)
	if leaseErr != nil {
		return false, leaseErr
	}
	if !present {
		return false, nil
	}
	return current == expected, nil
}

// productionPortabilityArchiveName is the file name an export publishes: the
// project's own name, reduced to what a file name may carry, and never a path.
func productionPortabilityArchiveName(listed projectfs.ListedProject) string {
	base := ""
	for _, r := range listed.Name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			base += string(r)
		case r == '-' || r == '_' || r == ' ' || r == '.':
			base += "-"
		default:
			continue
		}
		if len(base) >= 96 {
			break
		}
	}
	base = strings.Trim(base, "-.")
	if base == "" {
		base = "project"
	}
	return base + productionPortabilityArchiveExtension
}

// productionPortabilityExtension is the suffix a download's Content-Disposition
// advertises. It is derived from the name this lane minted, not from the request.
func productionPortabilityExtension(name string) string {
	extension := filepath.Ext(name)
	if extension == "" || len(extension) > 16 {
		return productionPortabilityArchiveExtension
	}
	for _, r := range extension {
		if r == '.' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			continue
		}
		return productionPortabilityArchiveExtension
	}
	return extension
}

// productionPortabilityTimestamp is the manifest's one date format: RFC 3339
// with exactly three fractional digits and Z, which is what JS emits.
func productionPortabilityTimestamp(now time.Time) string {
	return now.UTC().Format("2006-01-02T15:04:05.000Z")
}

// productionPortabilityDigest reports whether a string is a lower-case SHA-256.
func productionPortabilityDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for index := 0; index < len(value); index++ {
		if strings.IndexByte("0123456789abcdef", value[index]) < 0 {
			return false
		}
	}
	return true
}

// productionPortabilityDigestOf is the format's own hash of one byte slice.
func productionPortabilityDigestOf(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

// productionPortabilityMarshal is archiveformat's JSON spelling: struct key order,
// no HTML escaping, no trailing newline, and a literal U+2028/U+2029. The receipt
// id is a digest over bytes, so this must be the same spelling the format uses or
// two implementations would disagree about a valid receipt.
func productionPortabilityMarshal(value any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if encodeErr := encoder.Encode(value); encodeErr != nil {
		return nil, encodeErr
	}
	encoded := bytes.TrimSuffix(buffer.Bytes(), []byte{'\n'})
	encoded = bytes.ReplaceAll(encoded, []byte(`\u2028`), []byte("\u2028"))
	encoded = bytes.ReplaceAll(encoded, []byte(`\u2029`), []byte("\u2029"))
	return encoded, nil
}
