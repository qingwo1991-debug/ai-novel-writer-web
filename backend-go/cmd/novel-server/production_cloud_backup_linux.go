//go:build linux

package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"hash"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"
	"unicode/utf8"

	// The staged copy's WAL checkpoint below needs the same driver the author
	// database already uses (internal/admission), so this lane links it too.
	_ "github.com/mattn/go-sqlite3"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/admission"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/archiveformat"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/cloudbackup"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/librarypath"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/webdav"
)

// This file is the production lane's answer to the eight `cloud-backup:*` names
// in web/operations.json:66-73. Six are bound here; two are deliberately left
// unbound so they keep answering 501 GO_OPERATION_NOT_MIGRATED, and this comment
// names the port each of those two is missing.
//
//	cloud-backup:view             served   derived local binding view
//	cloud-backup:connect          UNBOUND  missing port, see below
//	cloud-backup:confirm-binding  served   re-affirms the derived binding
//	cloud-backup:backup           served   archive + manifest + completion
//	cloud-backup:list             served   verified catalogue of one book
//	cloud-backup:restore-copy     served   verified archive -> fresh project
//	cloud-backup:cancel           served   per-process operation registry
//	cloud-backup:clear-credential UNBOUND  missing port, see below
//
// # The destination this lane can honestly offer
//
// The desktop's destination is a WebDAV endpoint the author types, with a secret
// the main process keeps (electron/services/cloud-credential-store.ts) and a
// binding document whose single writer is B02
// (docs/plans/novel-quality-program-v3-2026-09-13/specs/B02.md:15). This lane has
// none of those three ports:
//
//   - EGRESS. A request-supplied endpoint cannot become a dialable origin here.
//     internal/egress admits only an immutable origin set, and this lane's one
//     committed set is derived from the author's own model profiles
//     (production_model_egress_linux.go:47-56: "the production lane has no
//     administrator flag"). operations.json:20 says proxy/egress/global-credential
//     administration needs role enforcement a string routing scope cannot
//     distinguish, so an author-supplied backup origin has no admittable policy.
//   - CREDENTIAL. There is no server-side secret store bound to an endpoint
//     account id; internal/secrets owns a key file, not the per-endpoint records
//     cloud-backup:connect would have to create, read at call time and clear.
//   - BINDING. cloud-backup-bindings.json has exactly one writer (B02), and this
//     lane must not become a second one. Nothing here persists a binding.
//
// So cloud-backup:connect and cloud-backup:clear-credential stay unbound: both
// exist only to administer that remote account, and answering either without the
// three ports above would be a confident wrong answer about the author's backup.
//
// What IS served is the self-hosted destination this product actually has: the
// server's own append-only generation store (internal/cloudbackup), addressed by
// the reserved local account id below. The wire shapes are unchanged, and the
// metadata protocol is internal/webdav's own v1 generation protocol - the same
// manifest/completion pair, the same catalogue - so a future remote destination is
// a second implementation, not a second format.
//
// # A binding this lane derives instead of storing
//
// view and confirm-binding report a binding computed from the caller's own live
// lease: cloudBookId is the project id, account is the reserved local id, mode is
// writable, revision is always 0. Nothing is written, so bindingSaved is true for
// a value that cannot diverge. A rebind can only re-affirm the empty parent list:
// a non-empty selection is refused with CLOUD_BACKUP_BINDING_READ_ONLY rather than
// silently dropped, because a lineage this lane did not record must not be
// reported as recorded.
//
// # What a backup is
//
// The exact bytes archiveformat produces for an export, built through that
// package's own Build (no second archive writer): one entry, `project.db`, whose
// content is the private database snapshot projectfs.StageDatabase took and
// admission.VerifySnapshot admitted at schema 7. The archive deliberately carries
// no portable transfer authority, and says so in its manifest's omittedItems: this
// lane does not implement receipt admission or runtime replay
// (internal/projectfs/authority.go:12-14), so a restore refuses an archive that
// does carry them instead of pretending to honour them.
//
// # How a backup is proved
//
// Publish writes the archive, then manifest.json, then completion.json, renames
// the staging directory into the collection, and then re-reads the PUBLISHED
// generation: completion -> manifest digest -> archive digest and byte count
// (internal/cloudbackup/store.go Verify). A generation that cannot be re-proved is
// removed again and the call refuses; no receipt is ever built from the bytes in
// memory.
//
// # How a restore protects the live project
//
// A restore only ever CREATES a copy. If the grant's target already holds an
// admitted project, this lane first takes a verified pre-restore backup of that
// project into the same store and then refuses: replacing a live project requires
// the author runtime's lease-fenced publication barrier (internal/projectpublish)
// and a freeze of the open project, neither of which this binder owns. The live
// project is therefore never touched, and the author still holds a proved copy of
// it.
//
// # The missing chooser port
//
// restore-copy consumes the `targetGrantId` src/shared/cloud-backup.ts:107-108
// introduces ("the renderer must not carry a path"), minted with the create
// permission exactly like the desktop's resolveRestoreTargetGrant
// (electron/controllers/cloud-backup-controller.ts:255-258). The binder that will
// mint it is `dialog:select-project-restore-target`, which this lane does not
// serve yet, so no browser can obtain a token today: every call answers
// CLOUD_BACKUP_INPUT_INVALID at :493's equivalent. The redemption itself is real
// and is exercised by this file's test through the same process-wide
// librarypath registry that chooser will use.
const (
	productionCloudBackupViewName    = "cloud-backup:view"
	productionCloudBackupConfirmName = "cloud-backup:confirm-binding"
	productionCloudBackupBackupName  = "cloud-backup:backup"
	productionCloudBackupListName    = "cloud-backup:list"
	productionCloudBackupRestoreName = "cloud-backup:restore-copy"
	productionCloudBackupCancelName  = "cloud-backup:cancel"
	// The two names this lane deliberately leaves unbound. They are named here so
	// the refusal is a decision in code, and this file's test asserts they still
	// answer GO_OPERATION_NOT_MIGRATED: a half-served credential surface would be
	// worse than an honest 501.
	productionCloudBackupConnectName = "cloud-backup:connect"
	productionCloudBackupClearName   = "cloud-backup:clear-credential"

	// productionCloudBackupDirectory is the store's one fixed child of
	// NOVEL_DATA_ROOT. It is not derived from a request and not a project, so the
	// startup library inventory (which enumerates the sibling `workspaces` root)
	// never sees it.
	productionCloudBackupDirectory = "cloud-backup"
	// productionCloudBackupLocalAccountID is the reserved account id of the
	// server's own store. It names no credential, because this destination has
	// none.
	productionCloudBackupLocalAccountID = "local"
	// productionCloudBackupTimestampLayout is the protocol's own createdAt shape
	// (internal/webdav/metadata.go validTime), not an ISO string this file invents.
	productionCloudBackupTimestampLayout = "2006-01-02T15:04:05.000Z"
	// productionCloudBackupMaxGenerations bounds one listing; the store enforces
	// the same ceiling itself.
	productionCloudBackupMaxGenerations = 256
	// productionCloudBackupStagedDatabase is the file projectfs.StageDatabase
	// writes its private snapshot under (internal/projectfs/stream.go:184). The
	// archive entry that carries those bytes is named project.db instead, which is
	// the portable name.
	productionCloudBackupStagedDatabase = "snapshot.db"
	// productionCloudBackupDatabaseEntry is the single entry a portable copy of
	// one author database carries. Its disposition is the format's own.
	productionCloudBackupDatabaseEntry       = "project.db"
	productionCloudBackupDatabaseDisposition = "portable-database"
	// productionCloudBackupModeWritable and productionCloudBackupModeOriginReadonly
	// are the two binding modes src/shared/cloud-backup.ts:7 declares. A backup of a
	// live project is writable; a restored copy is origin-readonly, because it came
	// from a cloud generation and pushing it back is a separate decision.
	productionCloudBackupModeWritable       = "writable"
	productionCloudBackupModeOriginReadonly = "origin-readonly"
	// productionCloudBackupPersistenceSessionOnly is the account view's persistence
	// for a destination with no credential: nothing is stored for it, at rest or in
	// the session.
	productionCloudBackupPersistenceSessionOnly = "session-only"
)

// Refusal codes. The browser's CloudBackupErrorCode union is closed
// (src/shared/cloud-backup.ts:53-69), so every refusal this file produces is one
// of those members; a code outside the union would be unrenderable. Codes are
// symbolic strings and are never formatted from a request value.
const (
	productionCloudBackupInputInvalid       = "CLOUD_BACKUP_INPUT_INVALID"
	productionCloudBackupDisclosureRequired = "CLOUD_BACKUP_DISCLOSURE_CONFIRMATION_REQUIRED"
	productionCloudBackupNotConfigured      = "CLOUD_BACKUP_NOT_CONFIGURED"
	productionCloudBackupCredentialMissing  = "CLOUD_BACKUP_CREDENTIAL_UNAVAILABLE"
	productionCloudBackupBindingReadOnly    = "CLOUD_BACKUP_BINDING_READ_ONLY"
	productionCloudBackupBindingConflict    = "CLOUD_BACKUP_BINDING_CONFLICT"
	productionCloudBackupOperationConflict  = "CLOUD_BACKUP_OPERATION_CONFLICT"
	productionCloudBackupOperationNotFound  = "CLOUD_BACKUP_OPERATION_NOT_FOUND"
	productionCloudBackupRemoteInvalid      = "CLOUD_BACKUP_REMOTE_INVALID"
	productionCloudBackupArchiveFailed      = "CLOUD_BACKUP_ARCHIVE_FAILED"
	productionCloudBackupRestoreFailed      = "CLOUD_BACKUP_RESTORE_FAILED"
	productionCloudBackupStateNotSaved      = "CLOUD_BACKUP_LOCAL_STATE_NOT_SAVED"
	productionCloudBackupCancelled          = "CLOUD_BACKUP_CANCELLED"
	productionCloudBackupFailed             = "CLOUD_BACKUP_FAILED"
)

// errProductionCloudBackupClosed is the lane-closed symbolic refusal.
var errProductionCloudBackupClosed = errors.New("PRODUCTION_CLOSED")

// The two grammars the wire already uses: a generation and an operation are
// version-4 UUIDs (electron/controllers/cloud-backup-controller.ts:58), and a book
// id is internal/webdav's own id pattern.
var (
	productionCloudBackupUUIDPattern = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	productionCloudBackupBookPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@+\-]{0,511}$`)
)

// productionCloudBackupSession is one CloudBackupProjectSessionContext
// (src/shared/cloud-backup.ts:1-5) after the wire shape has been read once.
type productionCloudBackupSession struct {
	projectID   string
	leaseID     string
	projectPath string
}

// productionCloudBackupTarget is one resolved restore destination: the library-
// relative name that may be answered, and the absolute path that may only be
// opened.
type productionCloudBackupTarget struct {
	relative string
	path     string
}

// productionCloudBackup is the binder. It owns no authority of its own: every
// answer is derived from the caller's live lease, the lane's admitted library, or
// the store.
type productionCloudBackup struct {
	open   *productionOpen
	config productionConfig

	// mu guards the lazily opened store, its refusal, and the operation registry.
	// The store is opened on first use so that binding this family writes nothing
	// at startup.
	mu         sync.Mutex
	store      *cloudbackup.Store
	storeErr   error
	operations map[string]context.CancelFunc
}

// newProductionCloudBackup returns the adapter for the lane built by
// productionHandlerWithContract. It opens nothing and resolves nothing here.
func newProductionCloudBackup(open *productionOpen, config productionConfig) *productionCloudBackup {
	return &productionCloudBackup{open: open, config: config, operations: make(map[string]context.CancelFunc)}
}

// Bind installs the six served names. The two refused names are absent on
// purpose, so they keep failing closed at GO_OPERATION_NOT_MIGRATED; see this
// file's header for the port each one is missing.
func (b *productionCloudBackup) Bind(server *transport.Server) error {
	if server == nil || b == nil || b.open == nil {
		return errProductionCloudBackupClosed
	}
	for _, binding := range []struct {
		name    string
		handler transport.Operation
	}{
		{productionCloudBackupViewName, b.view},
		{productionCloudBackupConfirmName, b.confirmBinding},
		{productionCloudBackupBackupName, b.backup},
		{productionCloudBackupListName, b.list},
		{productionCloudBackupRestoreName, b.restoreCopy},
		{productionCloudBackupCancelName, b.cancel},
	} {
		if err := server.Bind(binding.name, binding.handler); err != nil {
			return err
		}
	}
	return nil
}

// view answers cloud-backup:view: the binding and account this lane can derive for
// the caller's own project. A caller holding no project lease gets the honest
// `unconfigured` answer with both values null, which is what the renderer already
// handles (src/components/backups/ProjectBackupPanel.tsx:96-120).
func (b *productionCloudBackup) view(ctx context.Context, caller transport.Caller, args []any) (any, error) {
	requested, ok := productionCloudBackupSessionArg(args)
	if !ok {
		return productionCloudBackupFailure(productionCloudBackupInputInvalid, ""), nil
	}
	durable, _, _, _, err := b.lane()
	if err != nil {
		return productionCloudBackupFailure(productionCloudBackupFailed, ""), nil
	}
	owner, ownerErr := productionOwner(caller)
	if ownerErr != nil {
		return productionCloudBackupFailure(productionCloudBackupInputInvalid, ""), nil
	}
	current, present, currentErr := durable.productionFSGrantProject(ctx, owner)
	if currentErr != nil {
		return productionCloudBackupFailure(productionCloudBackupInputInvalid, ""), nil
	}
	if !present {
		return map[string]any{
			"success": true,
			"state":   "unconfigured",
			"binding": nil,
			"account": nil,
		}, nil
	}
	if !productionCloudBackupSameSession(current, requested) {
		return productionCloudBackupFailure(productionCloudBackupInputInvalid, ""), nil
	}
	return map[string]any{
		"success": true,
		"state":   "configured",
		"binding": productionCloudBackupBindingView(current.ProjectID, current.ProjectID, []string{}, productionCloudBackupModeWritable),
		"account": productionCloudBackupAccountView(caller.Principal),
	}, nil
}

// confirmBinding answers cloud-backup:confirm-binding. The binding is derived, so
// this call can only re-affirm it: the account must be the reserved local id, the
// book must be the caller's own project, the parent selection must be empty
// (nothing here records lineage), and the expected revision must be null or 0.
func (b *productionCloudBackup) confirmBinding(ctx context.Context, caller transport.Caller, args []any) (any, error) {
	request, ok := productionCloudBackupObject(args)
	if !ok || !productionCloudBackupKeys(request,
		[]string{"projectSession", "localEndpointAccountId", "lastSelectedParentGenerationIds", "expectedRevision"},
		[]string{"cloudBookId"}) {
		return productionCloudBackupFailure(productionCloudBackupInputInvalid, ""), nil
	}
	requested, ok := productionCloudBackupSessionValue(request["projectSession"])
	if !ok {
		return productionCloudBackupFailure(productionCloudBackupInputInvalid, ""), nil
	}
	accountID, ok := productionCloudBackupText(request["localEndpointAccountId"])
	if !ok || accountID != productionCloudBackupLocalAccountID {
		// Only the reserved local account exists; an unknown account id has no
		// credential behind it, which is exactly :191's refusal.
		return productionCloudBackupFailure(productionCloudBackupCredentialMissing, ""), nil
	}
	bookID, hasBook := productionCloudBackupOptionalText(request["cloudBookId"])
	if !hasBook {
		return productionCloudBackupFailure(productionCloudBackupInputInvalid, ""), nil
	}
	parents, ok := productionCloudBackupTextList(request["lastSelectedParentGenerationIds"])
	if !ok {
		return productionCloudBackupFailure(productionCloudBackupInputInvalid, ""), nil
	}
	durable, _, _, _, err := b.lane()
	if err != nil {
		return productionCloudBackupFailure(productionCloudBackupFailed, ""), nil
	}
	owner, ownerErr := productionOwner(caller)
	if ownerErr != nil {
		return productionCloudBackupFailure(productionCloudBackupInputInvalid, ""), nil
	}
	current, present, currentErr := durable.productionFSGrantProject(ctx, owner)
	if currentErr != nil {
		return productionCloudBackupFailure(productionCloudBackupInputInvalid, ""), nil
	}
	if !present {
		return productionCloudBackupFailure(productionCloudBackupNotConfigured, ""), nil
	}
	if !productionCloudBackupSameSession(current, requested) {
		return productionCloudBackupFailure(productionCloudBackupInputInvalid, ""), nil
	}
	if bookID != "" && bookID != current.ProjectID {
		return productionCloudBackupFailure(productionCloudBackupBindingConflict, ""), nil
	}
	if len(parents) != 0 {
		// A lineage this lane does not record must not be acknowledged: the next
		// backup would publish an archive whose parent list is empty while the
		// author believed otherwise.
		return productionCloudBackupFailure(productionCloudBackupBindingReadOnly, ""), nil
	}
	if !productionCloudBackupRevisionArg(request["expectedRevision"]) {
		return productionCloudBackupFailure(productionCloudBackupBindingConflict, ""), nil
	}
	return map[string]any{
		"success": true,
		"state":   "configured",
		"binding": productionCloudBackupBindingView(current.ProjectID, current.ProjectID, []string{}, productionCloudBackupModeWritable),
	}, nil
}

// backup answers cloud-backup:backup: it captures the caller's own project into
// the store and returns the verified generation.
func (b *productionCloudBackup) backup(ctx context.Context, caller transport.Caller, args []any) (any, error) {
	request, ok := productionCloudBackupObject(args)
	if !ok || !productionCloudBackupKeys(request,
		[]string{"operationId", "projectSession", "disclosureConfirmed"}, nil) {
		return productionCloudBackupFailure(productionCloudBackupInputInvalid, ""), nil
	}
	requested, ok := productionCloudBackupSessionValue(request["projectSession"])
	if !ok {
		return productionCloudBackupFailure(productionCloudBackupInputInvalid, ""), nil
	}
	operationID, ok := productionCloudBackupUUID(request["operationId"])
	if !ok {
		return productionCloudBackupFailure(productionCloudBackupInputInvalid, ""), nil
	}
	if confirmed, ok := request["disclosureConfirmed"].(bool); !ok || !confirmed {
		return productionCloudBackupFailure(productionCloudBackupDisclosureRequired, operationID), nil
	}
	return b.run(ctx, operationID, func(operationContext context.Context, id string) any {
		durable, library, _, _, err := b.lane()
		if err != nil {
			return productionCloudBackupFailure(productionCloudBackupFailed, id)
		}
		owner, ownerErr := productionOwner(caller)
		if ownerErr != nil {
			return productionCloudBackupFailure(productionCloudBackupInputInvalid, id)
		}
		current, present, currentErr := durable.productionFSGrantProject(operationContext, owner)
		if currentErr != nil {
			return productionCloudBackupFailure(productionCloudBackupInputInvalid, id)
		}
		if !present {
			return productionCloudBackupFailure(productionCloudBackupNotConfigured, id)
		}
		if !productionCloudBackupSameSession(current, requested) {
			return productionCloudBackupFailure(productionCloudBackupInputInvalid, id)
		}
		project, projectErr := library.OpenProject(current.ProjectPath)
		if projectErr != nil {
			return productionCloudBackupFailure(productionCloudBackupStateNotSaved, id)
		}
		if project.ProjectID() != current.ProjectID {
			_ = project.Close()
			log.Printf("novel-server: cloud-backup: %s", "PRODUCTION_PROJECT_IDENTITY_CHANGED")
			return productionCloudBackupFailure(productionCloudBackupFailed, id)
		}
		generation, captureErr := b.capture(operationContext, project, current.ProjectID)
		closeErr := project.Close()
		if captureErr != nil {
			return productionCloudBackupFailure(productionCloudBackupCode(captureErr, productionCloudBackupArchiveFailed), id)
		}
		if closeErr != nil {
			return productionCloudBackupFailure(productionCloudBackupStateNotSaved, id)
		}
		return map[string]any{
			"success":      true,
			"state":        "backup-complete",
			"operationId":  id,
			"generation":   generation,
			"backupPoint":  generation.GenerationID,
			"bindingSaved": true,
			"binding":      productionCloudBackupBindingView(current.ProjectID, current.ProjectID, []string{}, productionCloudBackupModeWritable),
		}
	}), nil
}

// list answers cloud-backup:list: the verified catalogue of one book. A generation
// whose archive cannot be re-proved is omitted by the store, not advertised.
func (b *productionCloudBackup) list(ctx context.Context, caller transport.Caller, args []any) (any, error) {
	request, ok := productionCloudBackupObject(args)
	if !ok || !productionCloudBackupKeys(request,
		[]string{"localEndpointAccountId", "cloudBookId"}, nil) {
		return productionCloudBackupFailure(productionCloudBackupInputInvalid, ""), nil
	}
	accountID, ok := productionCloudBackupText(request["localEndpointAccountId"])
	if !ok || accountID != productionCloudBackupLocalAccountID {
		return productionCloudBackupFailure(productionCloudBackupCredentialMissing, ""), nil
	}
	bookID, ok := productionCloudBackupBook(request["cloudBookId"])
	if !ok {
		return productionCloudBackupFailure(productionCloudBackupInputInvalid, ""), nil
	}
	store, storeErr := b.storeFor()
	if storeErr != nil {
		return productionCloudBackupFailure(productionCloudBackupStateNotSaved, ""), nil
	}
	if !b.knownBook(bookID) {
		// A book id this lane never admitted as a project is not a listing this
		// store answers, so an unrelated id cannot be used to probe the store.
		return productionCloudBackupFailure(productionCloudBackupNotConfigured, ""), nil
	}
	generations, listErr := store.List(ctx, bookID)
	if listErr != nil {
		return productionCloudBackupFailure(productionCloudBackupCode(listErr, productionCloudBackupStateNotSaved), ""), nil
	}
	return map[string]any{
		"success":     true,
		"state":       "listed",
		"generations": generations,
	}, nil
}

// restoreCopy answers cloud-backup:restore-copy. The archive is proved from the
// store before anything is written, the grant is spent only after that proof, and
// the restored copy is admitted by the same schema verifier the lane uses at
// startup.
func (b *productionCloudBackup) restoreCopy(ctx context.Context, caller transport.Caller, args []any) (any, error) {
	request, ok := productionCloudBackupObject(args)
	if !ok || !productionCloudBackupKeys(request,
		[]string{"operationId", "localEndpointAccountId", "cloudBookId", "generationId", "targetGrantId"}, nil) {
		return productionCloudBackupFailure(productionCloudBackupInputInvalid, ""), nil
	}
	accountID, ok := productionCloudBackupText(request["localEndpointAccountId"])
	if !ok || accountID != productionCloudBackupLocalAccountID {
		return productionCloudBackupFailure(productionCloudBackupCredentialMissing, ""), nil
	}
	bookID, ok := productionCloudBackupBook(request["cloudBookId"])
	if !ok {
		return productionCloudBackupFailure(productionCloudBackupInputInvalid, ""), nil
	}
	generationID, ok := productionCloudBackupUUID(request["generationId"])
	if !ok {
		return productionCloudBackupFailure(productionCloudBackupInputInvalid, ""), nil
	}
	operationID, ok := productionCloudBackupUUID(request["operationId"])
	if !ok {
		return productionCloudBackupFailure(productionCloudBackupInputInvalid, ""), nil
	}
	grantID, ok := productionCloudBackupText(request["targetGrantId"])
	if !ok || grantID == "" || len(grantID) > 512 {
		return productionCloudBackupFailure(productionCloudBackupInputInvalid, operationID), nil
	}
	return b.run(ctx, operationID, func(operationContext context.Context, id string) any {
		_, library, scratch, _, err := b.lane()
		if err != nil {
			return productionCloudBackupFailure(productionCloudBackupFailed, id)
		}
		store, storeErr := b.storeFor()
		if storeErr != nil {
			return productionCloudBackupFailure(productionCloudBackupStateNotSaved, id)
		}
		// The grant is the only target authority: the request carries an opaque id
		// and never a path (src/shared/cloud-backup.ts:107-108).
		owner, ownerErr := productionOwner(caller)
		if ownerErr != nil {
			return productionCloudBackupFailure(productionCloudBackupInputInvalid, id)
		}
		registry, registryErr := productionFSGrantsRegistry()
		if registryErr != nil {
			return productionCloudBackupFailure(productionCloudBackupRestoreFailed, id)
		}
		binding := librarypath.Binding{SessionID: owner.SessionID, ClientID: owner.ClientID}
		grant, grantErr := registry.Peek(grantID, binding, librarypath.OpMkdir)
		if grantErr != nil {
			// Exactly the desktop's own answer for an unusable target grant
			// (electron/controllers/cloud-backup-controller.ts:492-493).
			return productionCloudBackupFailure(productionCloudBackupInputInvalid, id)
		}
		target, targetErr := b.restoreTarget(library, grant)
		if targetErr != nil {
			return productionCloudBackupFailure(productionCloudBackupInputInvalid, id)
		}
		// 1. Prove the archive BEFORE spending the grant or creating a directory.
		staging, stagingErr := os.MkdirTemp(scratch, "cloud-backup-restore-")
		if stagingErr != nil {
			return productionCloudBackupFailure(productionCloudBackupStateNotSaved, id)
		}
		defer func() { _ = os.RemoveAll(staging) }()
		archivePath := filepath.Join(staging, "restored.ainovel")
		archiveFile, createErr := os.OpenFile(archivePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if createErr != nil {
			return productionCloudBackupFailure(productionCloudBackupStateNotSaved, id)
		}
		storeManifest, copyErr := store.CopyTo(operationContext, bookID, generationID, archiveFile)
		if closeErr := archiveFile.Close(); copyErr == nil && closeErr != nil {
			copyErr = closeErr
		}
		if copyErr != nil {
			return productionCloudBackupFailure(productionCloudBackupCode(copyErr, productionCloudBackupRemoteInvalid), id)
		}
		// 2. Prove the portable format and this lane's own admission of it. The
		//    handle stays open: archiveformat.Stage re-reads the same bytes, and a
		//    closed reader would make that proof impossible.
		archiveSource, sourceOpenErr := os.Open(archivePath)
		if sourceOpenErr != nil {
			return productionCloudBackupFailure(productionCloudBackupStateNotSaved, id)
		}
		archiveStat, statErr := archiveSource.Stat()
		if statErr != nil {
			_ = archiveSource.Close()
			return productionCloudBackupFailure(productionCloudBackupStateNotSaved, id)
		}
		portable, inspectErr := productionCloudBackupInspect(archiveSource, archiveStat.Size())
		if inspectErr != nil {
			_ = archiveSource.Close()
			// A digest or schema that does not match its manifest is never a
			// restore: the archive is refused whole.
			return productionCloudBackupFailure(productionCloudBackupRemoteInvalid, id)
		}
		if portable.Manifest().OriginProjectID != storeManifest.OriginProjectID ||
			portable.Manifest().SnapshotGeneration != storeManifest.PortableSnapshotGeneration {
			_ = archiveSource.Close()
			return productionCloudBackupFailure(productionCloudBackupRemoteInvalid, id)
		}
		// 3. A target that already holds an admitted project is copied first and
		//    then refused: this binder does not own the lease-fenced replacement.
		//    This check comes BEFORE the empty-directory check, because a live
		//    project is exactly the case whose data must be preserved.
		if productionCloudBackupProjectPresent(target.path) {
			if _, preErr := b.preRestoreCopy(operationContext, library, target.path); preErr != nil {
				_ = archiveSource.Close()
				return productionCloudBackupFailure(productionCloudBackupCode(preErr, productionCloudBackupRestoreFailed), id)
			}
			_ = archiveSource.Close()
			log.Printf("novel-server: cloud-backup: %s", "PRODUCTION_CLOUD_BACKUP_LIVE_TARGET_REFUSED")
			return productionCloudBackupFailure(productionCloudBackupRestoreFailed, id)
		}
		// A directory that is not a project but is not empty is somebody's data: a
		// restore only ever fills an empty or missing directory.
		empty, emptyErr := productionCloudBackupTargetEmpty(target.path)
		if emptyErr != nil || !empty {
			_ = archiveSource.Close()
			return productionCloudBackupFailure(productionCloudBackupInputInvalid, id)
		}
		// 4. Extract the one entry into a private stage and admit it.
		extractDirectory := filepath.Join(staging, "extract")
		if mkdirErr := os.Mkdir(extractDirectory, productionProjectCreateDirectoryMode); mkdirErr != nil {
			_ = archiveSource.Close()
			return productionCloudBackupFailure(productionCloudBackupStateNotSaved, id)
		}
		stage := &productionCloudBackupStage{directory: extractDirectory}
		stageErr := portable.Stage(stage)
		// The archive is fully read once Stage returns; nothing below needs it.
		if closeErr := archiveSource.Close(); stageErr == nil && closeErr != nil {
			stageErr = closeErr
		}
		if stageErr != nil {
			return productionCloudBackupFailure(productionCloudBackupRemoteInvalid, id)
		}
		entry := portable.Manifest().Entries[0]
		extracted := filepath.Join(extractDirectory, productionCloudBackupDatabaseEntry)
		evidence, admissionErr := admission.VerifySnapshot(operationContext, extracted, b.config.Schema)
		if admissionErr != nil {
			return productionCloudBackupFailure(productionCloudBackupRestoreFailed, id)
		}
		if evidence.ContentIdentity.Database != entry.SHA256 {
			return productionCloudBackupFailure(productionCloudBackupRemoteInvalid, id)
		}
		// 5. Spend the one-use grant at the last safe moment, then install.
		if _, spendErr := registry.Spend(grantID, binding, librarypath.OpMkdir); spendErr != nil {
			return productionCloudBackupFailure(productionCloudBackupInputInvalid, id)
		}
		installed, installErr := productionCloudBackupInstall(operationContext, target.path, extracted)
		if installErr != nil {
			return productionCloudBackupFailure(productionCloudBackupRestoreFailed, id)
		}
		listed, inventoryCode := productionProjectCreateInventory(operationContext, library, target.path)
		if inventoryCode != "" {
			_ = productionCloudBackupRemoveInstalled(installed)
			return productionCloudBackupFailure(productionCloudBackupRestoreFailed, id)
		}
		verified, verifyErr := admitProductionProject(operationContext, library, listed, scratch, productionConfig{
			Schema: b.config.Schema, MaxDatabaseBytes: b.config.MaxDatabaseBytes,
		})
		if verifyErr != nil {
			_ = productionCloudBackupRemoveInstalled(installed)
			log.Printf("novel-server: cloud-backup: %s", "PRODUCTION_CLOUD_BACKUP_RESTORED_SCHEMA_REFUSED")
			return productionCloudBackupFailure(productionCloudBackupRestoreFailed, id)
		}
		if verified.ContentIdentity.Database != entry.SHA256 {
			_ = productionCloudBackupRemoveInstalled(installed)
			return productionCloudBackupFailure(productionCloudBackupRemoteInvalid, id)
		}
		// 6. Navigation is best effort: the copy is verified author data whether or
		//    not this process can also mint a live catalogue key for it, and a
		//    rollback here would delete data that is already proved.
		recentUpdated := b.register(listed, verified)
		receipt := map[string]any{
			"originProjectId":            storeManifest.OriginProjectID,
			"targetProjectId":            listed.ProjectID,
			"targetProjectRoot":          target.relative,
			"snapshotGeneration":         portable.Manifest().SnapshotGeneration,
			"portableDatabaseSha256":     evidence.ContentIdentity.Database,
			"requiresRuntimeFreezeGuard": true,
		}
		return map[string]any{
			"success":              true,
			"state":                "restore-complete",
			"operationId":          id,
			"receipt":              receipt,
			"bindingSaved":         true,
			"binding":              productionCloudBackupBindingView(listed.ProjectID, bookID, []string{generationID}, productionCloudBackupModeOriginReadonly),
			"recentProjectUpdated": recentUpdated,
		}
	}), nil
}

// cancel answers cloud-backup:cancel: it cancels the one running operation with
// that id, and reports the id it never saw as not found rather than as success.
func (b *productionCloudBackup) cancel(ctx context.Context, caller transport.Caller, args []any) (any, error) {
	if b == nil || len(args) != 1 {
		return productionCloudBackupFailure(productionCloudBackupInputInvalid, ""), nil
	}
	operationID, ok := productionCloudBackupUUID(args[0])
	if !ok {
		return productionCloudBackupFailure(productionCloudBackupInputInvalid, ""), nil
	}
	b.mu.Lock()
	cancel, running := b.operations[operationID]
	b.mu.Unlock()
	if !running || cancel == nil {
		return productionCloudBackupFailure(productionCloudBackupOperationNotFound, operationID), nil
	}
	cancel()
	return map[string]any{"success": true, "cancelled": true}, nil
}

// run owns one operation id for the duration of body. A second call with the same
// id is a conflict, never a second concurrent operation
// (electron/controllers/cloud-backup-controller.ts:274), and the registry entry is
// removed however body returns.
func (b *productionCloudBackup) run(ctx context.Context, operationID string, body func(context.Context, string) any) any {
	if b == nil || operationID == "" || body == nil {
		return productionCloudBackupFailure(productionCloudBackupInputInvalid, operationID)
	}
	operationContext, cancel := context.WithCancel(ctx)
	b.mu.Lock()
	if b.operations == nil {
		b.operations = make(map[string]context.CancelFunc)
	}
	if _, running := b.operations[operationID]; running {
		b.mu.Unlock()
		cancel()
		return productionCloudBackupFailure(productionCloudBackupOperationConflict, operationID)
	}
	b.operations[operationID] = cancel
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		delete(b.operations, operationID)
		b.mu.Unlock()
		cancel()
	}()
	return body(operationContext, operationID)
}

// capture turns one open project into a verified generation. It is the one place a
// backup is built, so the live-project path and the pre-restore copy cannot drift
// apart.
func (b *productionCloudBackup) capture(ctx context.Context, project *projectfs.Project, bookID string) (webdav.Generation, error) {
	var empty webdav.Generation
	if project == nil || !productionCloudBackupBookPattern.MatchString(bookID) {
		return empty, cloudbackup.ErrInput
	}
	store, err := b.storeFor()
	if err != nil {
		return empty, err
	}
	_, _, scratch, _, laneErr := b.lane()
	if laneErr != nil {
		return empty, laneErr
	}
	staging, stagingErr := os.MkdirTemp(scratch, "cloud-backup-capture-")
	if stagingErr != nil {
		return empty, stagingErr
	}
	defer func() { _ = os.RemoveAll(staging) }()
	// The database snapshot: projectfs stages the db/wal/shm/journal set into a
	// private directory and hashes it three times; nothing here opens the author's
	// database for writing.
	snapshotRoot, rootErr := os.OpenRoot(staging)
	if rootErr != nil {
		return empty, rootErr
	}
	proof, stageErr := project.StageDatabase(ctx, snapshotRoot, b.config.MaxDatabaseBytes)
	closeRootErr := snapshotRoot.Close()
	if stageErr != nil {
		return empty, stageErr
	}
	if closeRootErr != nil {
		return empty, closeRootErr
	}
	snapshotPath := filepath.Join(staging, productionCloudBackupStagedDatabase)
	// The staged set is a raw db/wal/shm copy, so the main file alone is only the
	// complete database once its committed frames are folded in. The checkpoint
	// runs on the PRIVATE copy, never on the author's project.
	if checkpointErr := productionCloudBackupCheckpoint(ctx, snapshotPath); checkpointErr != nil {
		return empty, checkpointErr
	}
	// After that, a non-empty WAL or a rollback journal means the copy cannot be
	// represented as one self-contained file, and an archive of the main file alone
	// would silently drop committed pages: refuse instead of archiving a lie.
	for _, suffix := range []string{"-wal", "-journal"} {
		sidecar := filepath.Join(staging, productionCloudBackupStagedDatabase+suffix)
		if info, statErr := os.Lstat(sidecar); statErr == nil && info.Size() > 0 {
			log.Printf("novel-server: cloud-backup: %s", "PRODUCTION_CLOUD_BACKUP_DATABASE_SIDECAR_PRESENT")
			return empty, cloudbackup.ErrInput
		}
	}
	snapshotSize, snapshotSHA, hashErr := productionCloudBackupDigestFile(snapshotPath)
	if hashErr != nil {
		return empty, hashErr
	}
	evidence, admissionErr := admission.VerifySnapshot(ctx, snapshotPath, b.config.Schema)
	if admissionErr != nil {
		return empty, admissionErr
	}
	if evidence.ContentIdentity.Database != snapshotSHA {
		return empty, cloudbackup.ErrCorrupt
	}
	generationID, idErr := productionProjectCreateID()
	if idErr != nil {
		return empty, idErr
	}
	createdAt := time.Now().UTC().Format(productionCloudBackupTimestampLayout)
	portable := archiveformat.Manifest{
		FormatVersion:             1,
		SourceSchemaVersion:       7,
		OriginProjectID:           project.ProjectID(),
		SnapshotGeneration:        generationID,
		CreatedAt:                 createdAt,
		DeclaredUncompressedBytes: uint64(snapshotSize),
		DeclaredCompressedBytes:   uint64(snapshotSize),
		Entries: []archiveformat.Entry{{
			Path:        productionCloudBackupDatabaseEntry,
			ByteSize:    uint64(snapshotSize),
			SHA256:      snapshotSHA,
			Disposition: productionCloudBackupDatabaseDisposition,
		}},
		SemanticCounts: map[string]uint64{productionCloudBackupDatabaseDisposition: 1},
		// The authority this archive deliberately does not carry, said out loud in
		// the manifest rather than implied by its absence.
		OmittedItems:       []archiveformat.OmittedItem{{ID: "portable-transfer-authority", Reason: "exclude-machine-authority"}},
		TransferReceiptIDs: []string{},
		HistoryProjectionIDs: []string{},
	}
	if portable.OriginProjectID != bookID {
		return empty, cloudbackup.ErrInput
	}
	archivePath := filepath.Join(staging, "archive.ainovel")
	archiveFile, createErr := os.OpenFile(archivePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if createErr != nil {
		return empty, createErr
	}
	source, openErr := os.Open(snapshotPath)
	if openErr != nil {
		_ = archiveFile.Close()
		return empty, openErr
	}
	measured := &productionCloudBackupDigestWriter{writer: archiveFile, digest: sha256.New()}
	receipt, buildErr := archiveformat.Build(measured, portable,
		[]archiveformat.Source{{Entry: portable.Entries[0], Reader: source}}, archiveformat.DefaultLimits())
	sourceCloseErr := source.Close()
	// The source must not have moved under the archive: the same proof startup
	// admission re-runs, repeated after the copy.
	unchangedErr := proof.VerifyUnchanged(ctx)
	if buildErr == nil && unchangedErr != nil {
		buildErr = unchangedErr
	}
	if syncErr := archiveFile.Sync(); buildErr == nil && syncErr != nil {
		buildErr = syncErr
	}
	if closeErr := archiveFile.Close(); buildErr == nil && closeErr != nil {
		buildErr = closeErr
	}
	if buildErr != nil {
		return empty, buildErr
	}
	if sourceCloseErr != nil {
		return empty, sourceCloseErr
	}
	if measured.count != int64(receipt.ArchiveBytes) {
		return empty, cloudbackup.ErrCorrupt
	}
	archiveSHA := measured.sum()
	publishedSize, publishedSHA, hashErr := productionCloudBackupDigestFile(archivePath)
	if hashErr != nil {
		return empty, hashErr
	}
	if publishedSHA != archiveSHA || publishedSize != measured.count {
		return empty, cloudbackup.ErrCorrupt
	}
	archiveReader, archiveOpenErr := os.Open(archivePath)
	if archiveOpenErr != nil {
		return empty, archiveOpenErr
	}
	defer archiveReader.Close()
	return store.Publish(ctx, cloudbackup.PublishInput{
		BookID:              bookID,
		GenerationID:        generationID,
		OriginProjectID:     project.ProjectID(),
		SnapshotGeneration:  generationID,
		ParentGenerationIDs: []string{},
		CreatedAt:           createdAt,
		Archive:             archiveReader,
		ArchiveSHA256:       archiveSHA,
		ArchiveByteSize:     measured.count,
	})
}

// preRestoreCopy records a verified backup of a project a restore was pointed at
// but may not replace. It returns the generation id so a caller (and a test) can
// prove the copy exists and is valid.
func (b *productionCloudBackup) preRestoreCopy(ctx context.Context, library *projectfs.Library, target string) (string, error) {
	project, openErr := library.OpenProject(target)
	if openErr != nil {
		return "", openErr
	}
	generation, captureErr := b.capture(ctx, project, project.ProjectID())
	closeErr := project.Close()
	if captureErr != nil {
		return "", captureErr
	}
	if closeErr != nil {
		return "", closeErr
	}
	return generation.GenerationID, nil
}

// restoreTarget resolves the grant's target inside the library allowlist and
// reports the library-relative name it addresses. The absolute path is returned
// separately and never placed on the wire.
func (b *productionCloudBackup) restoreTarget(library *projectfs.Library, grant librarypath.Grant) (productionCloudBackupTarget, error) {
	var empty productionCloudBackupTarget
	if library == nil {
		return empty, productionFSHTTPError("PRODUCTION_CLOSED")
	}
	roots, rootsErr := librarypath.NewRoots([]librarypath.Root{
		{ID: librarypath.LibraryRootID, Kind: librarypath.KindLibrary, Path: library.Path()},
	})
	if rootsErr != nil {
		return empty, rootsErr
	}
	target, resolveErr := roots.Resolve(grant.Target.RootID, grant.Target.Relative)
	if resolveErr != nil {
		return empty, resolveErr
	}
	// A restore creates one new child of the library root: never the root itself,
	// never the project root the caller already holds, and never a nested name
	// whose parent this lane would have to mint.
	if target.RootID != librarypath.LibraryRootID || target.Relative == "" || !filepath.IsLocal(target.Relative) {
		return empty, librarypath.ErrTargetInvalid
	}
	if target.Exists && !target.IsDir {
		return empty, librarypath.ErrTargetInvalid
	}
	// Relative is the only form that may appear in an answer: Target.Path is a host
	// path and librarypath forbids putting it on a wire.
	return productionCloudBackupTarget{relative: target.Relative, path: target.Path}, nil
}

// register puts a restored copy into the catalogue project:open resolves, exactly
// like project:create does (production_project_create_linux.go:300-350), and
// reports whether the recent shelf learned about it. A failure here never undoes
// the copy: the restored data is proved author data, and deleting it because this
// process could not mint a navigation key would be the bigger loss.
func (b *productionCloudBackup) register(listed projectfs.ListedProject, evidence admission.Evidence) bool {
	if b == nil || b.open == nil {
		return false
	}
	durable, library, _, _, err := b.lane()
	if err != nil || durable == nil || library == nil {
		return false
	}
	catalog, ok := durable.productionCreateCatalog()
	if !ok {
		return false
	}
	project, openErr := library.OpenProject(listed.Path)
	if openErr != nil {
		return false
	}
	recent, registerErr := catalog.Register(project)
	closeErr := project.Close()
	if registerErr != nil {
		return false
	}
	if closeErr != nil {
		durable.productionProjectForgetKey(listed.Path)
		return false
	}
	if !durable.productionProjectRememberKey(listed.Path, recent.Key) {
		return false
	}
	// Prove it opens: resolve the key the catalog just issued back into a project
	// capability, and check the identity it answers with.
	proven, proveErr := catalog.Resolve(recent.Key)
	if proveErr != nil {
		durable.productionProjectForgetKey(listed.Path)
		return false
	}
	identityHeld := proven.ProjectID() == listed.ProjectID && proven.Path() == listed.Path
	proveCloseErr := proven.Close()
	if !identityHeld || proveCloseErr != nil {
		durable.productionProjectForgetKey(listed.Path)
		return false
	}
	return b.open.productionProjectCreateShelve(listed, evidence)
}

// lane returns the durable runtime, the admitted library, the private scratch
// directory and the trusted layout under the lane lock.
func (b *productionCloudBackup) lane() (*productionDurableOpen, *projectfs.Library, string, productionLayout, error) {
	var zero productionLayout
	if b == nil || b.open == nil {
		return nil, nil, "", zero, errProductionCloudBackupClosed
	}
	b.open.mu.Lock()
	defer b.open.mu.Unlock()
	if b.open.closed || b.open.durable == nil || b.open.library == nil || b.open.scratch == "" {
		return nil, nil, "", zero, errProductionCloudBackupClosed
	}
	return b.open.durable, b.open.library, b.open.scratch, b.open.layout, nil
}

// storeFor opens the store on first use. A refusal is remembered: a store root
// that is not a private directory does not become usable on a later call.
func (b *productionCloudBackup) storeFor() (*cloudbackup.Store, error) {
	if b == nil || b.open == nil {
		return nil, cloudbackup.ErrClosed
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.store != nil {
		return b.store, nil
	}
	if b.storeErr != nil {
		return nil, b.storeErr
	}
	layout := b.open.layout
	b.open.mu.Lock()
	closed := b.open.closed
	b.open.mu.Unlock()
	if closed {
		b.storeErr = errProductionCloudBackupClosed
		return nil, b.storeErr
	}
	store, err := cloudbackup.Open(filepath.Join(layout.dataRoot, productionCloudBackupDirectory), cloudbackup.Limits{
		MaxArchiveBytes: b.config.MaxDatabaseBytes,
		MaxGenerations:  productionCloudBackupMaxGenerations,
	})
	if err != nil {
		b.storeErr = err
		return nil, err
	}
	b.store = store
	return store, nil
}

// knownBook reports whether a book id names a project this lane admitted. It keeps
// the store's catalogue from answering for an id no project ever had.
func (b *productionCloudBackup) knownBook(bookID string) bool {
	if b == nil || b.open == nil {
		return false
	}
	b.open.mu.Lock()
	defer b.open.mu.Unlock()
	if b.open.closed {
		return false
	}
	for _, item := range b.open.projects {
		if item.listed.ProjectID == bookID {
			return true
		}
	}
	return false
}

// productionCloudBackupStage is the private extraction destination
// archiveformat.Stage writes into. It admits exactly the one entry a portable
// database copy carries, so no archive path can name a file of its own.
type productionCloudBackupStage struct {
	directory string
	opened    int
	complete  bool
}

func (s *productionCloudBackupStage) Open(entry archiveformat.Entry) (io.WriteCloser, error) {
	if s == nil || s.directory == "" || s.complete {
		return nil, cloudbackup.ErrInput
	}
	if entry.Path != productionCloudBackupDatabaseEntry || entry.Disposition != productionCloudBackupDatabaseDisposition {
		return nil, cloudbackup.ErrInput
	}
	if s.opened != 0 {
		return nil, cloudbackup.ErrInput
	}
	file, err := os.OpenFile(filepath.Join(s.directory, productionCloudBackupDatabaseEntry),
		os.O_WRONLY|os.O_CREATE|os.O_EXCL, productionProjectCreateFileMode)
	if err != nil {
		return nil, err
	}
	s.opened++
	return file, nil
}

func (s *productionCloudBackupStage) Complete(archiveformat.Manifest, archiveformat.Receipt) error {
	if s == nil || s.opened == 0 || s.complete {
		return cloudbackup.ErrInput
	}
	s.complete = true
	return nil
}

func (s *productionCloudBackupStage) Abort() error {
	if s == nil || s.directory == "" {
		return nil
	}
	return os.RemoveAll(s.directory)
}

// productionCloudBackupDigestWriter hashes and counts what archiveformat.Build
// writes, so the archive's own digest is the one the store will publish.
type productionCloudBackupDigestWriter struct {
	writer io.Writer
	digest hash.Hash
	count  int64
}

func (w *productionCloudBackupDigestWriter) Write(p []byte) (int, error) {
	if w == nil || w.writer == nil || w.digest == nil {
		return 0, cloudbackup.ErrClosed
	}
	written, err := w.writer.Write(p)
	if written > 0 {
		_, _ = w.digest.Write(p[:written])
		w.count += int64(written)
	}
	return written, err
}

func (w *productionCloudBackupDigestWriter) sum() string {
	if w == nil || w.digest == nil {
		return ""
	}
	return hex.EncodeToString(w.digest.Sum(nil))
}

// productionCloudBackupInspect admits one archive file as a portable archive and
// enforces this lane's narrower contract: exactly one portable database entry, no
// transfer receipts and no history projections.
func productionCloudBackupInspect(source io.ReaderAt, size int64) (*archiveformat.Archive, error) {
	if source == nil || size <= 0 {
		return nil, cloudbackup.ErrInput
	}
	archive, inspectErr := archiveformat.Inspect(source, size, archiveformat.DefaultLimits())
	if inspectErr != nil {
		return nil, inspectErr
	}
	manifest := archive.Manifest()
	if len(manifest.Entries) != 1 ||
		manifest.Entries[0].Path != productionCloudBackupDatabaseEntry ||
		manifest.Entries[0].Disposition != productionCloudBackupDatabaseDisposition {
		return nil, cloudbackup.ErrInput
	}
	if len(manifest.TransferReceiptIDs) != 0 || len(manifest.HistoryProjectionIDs) != 0 {
		// This lane does not implement portable receipt admission or runtime
		// replay (internal/projectfs/authority.go:12-14), so an archive that
		// carries authority is refused rather than half-installed.
		return nil, cloudbackup.ErrInput
	}
	return archive, nil
}

// productionCloudBackupInstall writes the restored copy into an empty target and
// returns the paths it created, so a failure can undo exactly those.
func productionCloudBackupInstall(ctx context.Context, target, database string) ([]string, error) {
	if ctx == nil || target == "" || database == "" {
		return nil, cloudbackup.ErrInput
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var created []string
	fail := func(cause error) ([]string, error) {
		_ = productionCloudBackupRemoveInstalled(created)
		return nil, cause
	}
	if _, statErr := os.Lstat(target); errors.Is(statErr, os.ErrNotExist) {
		if mkdirErr := os.Mkdir(target, productionProjectCreateDirectoryMode); mkdirErr != nil {
			return fail(mkdirErr)
		}
		created = append(created, target)
	} else if statErr != nil {
		return nil, statErr
	}
	storage := filepath.Join(target, projectfs.StorageDirectory)
	if mkdirErr := os.Mkdir(storage, productionProjectCreateDirectoryMode); mkdirErr != nil {
		return fail(mkdirErr)
	}
	created = append(created, storage)
	projectID, idErr := productionProjectCreateID()
	if idErr != nil {
		return fail(idErr)
	}
	// The canonical six-key manifest, written by the same function project:create
	// uses, so a restored copy is indistinguishable from a created one.
	if manifestErr := productionProjectCreateManifest(storage, projectID); manifestErr != nil {
		return fail(manifestErr)
	}
	created = append(created, filepath.Join(storage, projectfs.ManifestName))
	installedDatabase := filepath.Join(storage, projectfs.DatabaseName)
	if copyErr := productionCloudBackupCopyFile(database, installedDatabase); copyErr != nil {
		return fail(copyErr)
	}
	created = append(created, installedDatabase)
	if promptErr := os.Mkdir(filepath.Join(storage, productionProjectCreatePromptDirectory), productionProjectCreateDirectoryMode); promptErr != nil {
		return fail(promptErr)
	}
	created = append(created, filepath.Join(storage, productionProjectCreatePromptDirectory))
	return created, nil
}

// productionCloudBackupRemoveInstalled removes the paths one install created,
// deepest first, and never follows anything it did not create.
func productionCloudBackupRemoveInstalled(created []string) error {
	var joined error
	for index := len(created) - 1; index >= 0; index-- {
		if err := os.RemoveAll(created[index]); err != nil {
			joined = errors.Join(joined, err)
		}
	}
	return joined
}

// productionCloudBackupCopyFile copies one regular file exclusively and proves the
// copy by digest; the installed database must be the archived bytes, not merely
// the bytes a read produced.
func productionCloudBackupCopyFile(source, destination string) error {
	input, openErr := os.Open(source)
	if openErr != nil {
		return openErr
	}
	defer input.Close()
	output, createErr := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, productionProjectCreateFileMode)
	if createErr != nil {
		return createErr
	}
	digest := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(output, digest), input)
	if copyErr == nil {
		if syncErr := output.Sync(); syncErr != nil {
			copyErr = syncErr
		}
	}
	if closeErr := output.Close(); copyErr == nil && closeErr != nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		return copyErr
	}
	sourceSize, sourceSHA, sourceErr := productionCloudBackupDigestFile(source)
	if sourceErr != nil {
		return sourceErr
	}
	if written != sourceSize || hex.EncodeToString(digest.Sum(nil)) != sourceSHA {
		return cloudbackup.ErrCorrupt
	}
	return nil
}

// errProductionCloudBackupCheckpoint is the refusal of a staged copy whose WAL
// could not be folded into its main file.
var errProductionCloudBackupCheckpoint = errors.New("PRODUCTION_CLOUD_BACKUP_CHECKPOINT_BLOCKED")

// productionCloudBackupCheckpoint folds the committed WAL frames of the private
// staged copy into its main file, so the archive's one entry is a self-contained
// database. It opens that copy in read-write mode and nothing else: the author's
// project is never opened here, and the caller has already proved the copy equals
// the source.
func productionCloudBackupCheckpoint(ctx context.Context, path string) error {
	if ctx == nil || path == "" {
		return cloudbackup.ErrInput
	}
	handle, openErr := sql.Open("sqlite3", path)
	if openErr != nil {
		return openErr
	}
	handle.SetMaxOpenConns(1)
	defer func() { _ = handle.Close() }()
	if _, execErr := handle.ExecContext(ctx, "PRAGMA busy_timeout=5000"); execErr != nil {
		return execErr
	}
	// wal_checkpoint answers (busy, log frames, checkpointed frames). A non-WAL
	// database answers 0,-1,-1, which is already what this function wants.
	var busy, logFrames, checkpointed int
	if queryErr := handle.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &logFrames, &checkpointed); queryErr != nil {
		return queryErr
	}
	if busy != 0 {
		return errProductionCloudBackupCheckpoint
	}
	// The copy keeps the journal mode it arrived in; TRUNCATE has already emptied
	// the log, which is what makes the one entry complete.
	return handle.Close()
}

// productionCloudBackupDigestFile returns one file's size and SHA-256.
func productionCloudBackupDigestFile(path string) (int64, string, error) {
	file, openErr := os.Open(path)
	if openErr != nil {
		return 0, "", openErr
	}
	defer file.Close()
	digest := sha256.New()
	written, copyErr := io.Copy(digest, file)
	if copyErr != nil {
		return 0, "", copyErr
	}
	return written, hex.EncodeToString(digest.Sum(nil)), nil
}

// productionCloudBackupTargetEmpty reports whether a restore target holds nothing
// yet: a missing directory and an empty one are both usable.
func productionCloudBackupTargetEmpty(target string) (bool, error) {
	info, statErr := os.Lstat(target)
	if errors.Is(statErr, os.ErrNotExist) {
		return true, nil
	}
	if statErr != nil {
		return false, statErr
	}
	if !info.IsDir() {
		return false, nil
	}
	entries, readErr := os.ReadDir(target)
	if readErr != nil {
		return false, readErr
	}
	return len(entries) == 0, nil
}

// productionCloudBackupProjectPresent reports whether a directory already holds a
// canonical project, using the same two names the startup inventory requires.
func productionCloudBackupProjectPresent(target string) bool {
	if target == "" {
		return false
	}
	_, manifestErr := os.Lstat(filepath.Join(target, projectfs.StorageDirectory, projectfs.ManifestName))
	if manifestErr != nil {
		return false
	}
	_, databaseErr := os.Lstat(filepath.Join(target, projectfs.StorageDirectory, projectfs.DatabaseName))
	return databaseErr == nil
}

// productionCloudBackupCode maps a store, archive or platform refusal onto the
// browser's closed error union. It never formats the cause into the answer.
func productionCloudBackupCode(err error, fallback string) string {
	switch {
	case err == nil:
		return fallback
	case errors.Is(err, cloudbackup.ErrCancelled), errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return productionCloudBackupCancelled
	case errors.Is(err, cloudbackup.ErrCorrupt), errors.Is(err, cloudbackup.ErrTooLarge):
		return productionCloudBackupRemoteInvalid
	case errors.Is(err, cloudbackup.ErrNotFound):
		return productionCloudBackupNotConfigured
	case errors.Is(err, cloudbackup.ErrInput):
		return productionCloudBackupArchiveFailed
	case errors.Is(err, errProductionCloudBackupClosed), errors.Is(err, cloudbackup.ErrClosed):
		return productionCloudBackupFailed
	case errors.Is(err, projectfs.ErrChanged), archiveformat.Code(err) == "PORTABLE_SOURCE_CHANGED":
		return productionCloudBackupArchiveFailed
	}
	return fallback
}

// productionCloudBackupFailure is one CloudBackupFailure envelope
// (src/shared/cloud-backup.ts:71-76): a business answer at HTTP 200, like every
// other refusal the desktop controller returns.
func productionCloudBackupFailure(code, operationID string) map[string]any {
	answer := map[string]any{
		"success":   false,
		"state":     "failed",
		"errorCode": code,
	}
	if operationID != "" {
		answer["operationId"] = operationID
	}
	return answer
}

// productionCloudBackupBindingView is one CloudBackupBindingView
// (src/shared/cloud-backup.ts:9-16), derived and never stored.
func productionCloudBackupBindingView(projectID, bookID string, parents []string, mode string) map[string]any {
	if parents == nil {
		parents = []string{}
	}
	return map[string]any{
		"localProjectId":                 projectID,
		"cloudBookId":                    bookID,
		"localEndpointAccountId":         productionCloudBackupLocalAccountID,
		"lastSelectedParentGenerationIds": parents,
		"mode":                           mode,
		"revision":                       0,
	}
}

// productionCloudBackupAccountView is one CloudBackupAccountView
// (src/shared/cloud-backup.ts:18-23) for a destination with no credential. The
// endpoint is the reserved local scheme rather than any host string, and the
// username is the authenticated principal - never a request value.
func productionCloudBackupAccountView(principal string) map[string]any {
	return map[string]any{
		"accountId":   productionCloudBackupLocalAccountID,
		"endpoint":    "local",
		"username":    principal,
		"persistence": productionCloudBackupPersistenceSessionOnly,
	}
}

// productionCloudBackupKeys admits a request object whose keys are exactly the
// required ones plus, at most, the optional ones: an unknown key is refused rather
// than ignored, so a caller cannot believe it sent a field this lane never read.
func productionCloudBackupKeys(object map[string]any, required, optional []string) bool {
	allowed := make(map[string]bool, len(required)+len(optional))
	for _, key := range required {
		allowed[key] = true
		if _, present := object[key]; !present {
			return false
		}
	}
	for _, key := range optional {
		allowed[key] = true
	}
	for key := range object {
		if !allowed[key] {
			return false
		}
	}
	return true
}

// productionCloudBackupObject reads the one request object a name takes.
func productionCloudBackupObject(args []any) (map[string]any, bool) {
	if len(args) != 1 {
		return nil, false
	}
	object, ok := args[0].(map[string]any)
	if !ok || len(object) == 0 {
		return nil, false
	}
	return object, true
}

// productionCloudBackupSessionArg reads cloud-backup:view's bare session argument.
func productionCloudBackupSessionArg(args []any) (productionCloudBackupSession, bool) {
	if len(args) != 1 {
		return productionCloudBackupSession{}, false
	}
	return productionCloudBackupSessionValue(args[0])
}

// productionCloudBackupSessionValue reads one CloudBackupProjectSessionContext: a
// three-key object whose every value is a non-empty string.
func productionCloudBackupSessionValue(value any) (productionCloudBackupSession, bool) {
	object, ok := value.(map[string]any)
	if !ok || len(object) != 3 {
		return productionCloudBackupSession{}, false
	}
	projectID, projectOK := productionCloudBackupText(object["projectId"])
	leaseID, leaseOK := productionCloudBackupText(object["leaseId"])
	projectPath, pathOK := productionCloudBackupText(object["projectPath"])
	if !projectOK || !leaseOK || !pathOK || !filepath.IsAbs(projectPath) {
		return productionCloudBackupSession{}, false
	}
	return productionCloudBackupSession{projectID: projectID, leaseID: leaseID, projectPath: filepath.Clean(projectPath)}, true
}

// productionCloudBackupSameSession proves the request means the project the caller
// currently holds. It is not authority: the lease is, and this only refuses a
// request about some other project.
func productionCloudBackupSameSession(current projectsession.Context, requested productionCloudBackupSession) bool {
	return current.ProjectID == requested.projectID &&
		current.LeaseID == requested.leaseID &&
		current.ProjectPath == requested.projectPath
}

// productionCloudBackupText reads one non-empty, valid-UTF-8 string.
func productionCloudBackupText(value any) (string, bool) {
	text, ok := value.(string)
	if !ok || text == "" || len(text) > 4096 || !utf8.ValidString(text) {
		return "", false
	}
	return text, true
}

// productionCloudBackupOptionalText reads an optional string field: absent is ""
// with ok true, present but unusable is ok false.
func productionCloudBackupOptionalText(value any) (string, bool) {
	if value == nil {
		return "", true
	}
	return productionCloudBackupText(value)
}

// productionCloudBackupBook reads one cloud book id.
func productionCloudBackupBook(value any) (string, bool) {
	text, ok := productionCloudBackupText(value)
	if !ok || !productionCloudBackupBookPattern.MatchString(text) {
		return "", false
	}
	return text, true
}

// productionCloudBackupUUID reads one version-4 UUID.
func productionCloudBackupUUID(value any) (string, bool) {
	text, ok := productionCloudBackupText(value)
	if !ok || !productionCloudBackupUUIDPattern.MatchString(text) {
		return "", false
	}
	return text, true
}

// productionCloudBackupTextList reads a list of UUIDs; an empty list is valid and
// a non-list is not.
func productionCloudBackupTextList(value any) ([]string, bool) {
	items, ok := value.([]any)
	if !ok || len(items) > 256 {
		return nil, false
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		text, valid := productionCloudBackupUUID(item)
		if !valid {
			return nil, false
		}
		out = append(out, text)
	}
	return out, true
}

// productionCloudBackupRevisionArg accepts only the revision the derived binding
// has: null (never confirmed) or 0.
func productionCloudBackupRevisionArg(value any) bool {
	if value == nil {
		return true
	}
	number, ok := productionCloudBackupRevisionNumber(value)
	return ok && number == 0
}

// productionCloudBackupRevisionNumber is the one numeric revision shape the wire
// can carry after JSON decoding; it exists so the type assertion is written once.
func productionCloudBackupRevisionNumber(value any) (float64, bool) {
	number, ok := value.(float64)
	return number, ok
}
