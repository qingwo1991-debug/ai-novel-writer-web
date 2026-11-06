//go:build linux

package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/librarypath"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/promptskills"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file is the production lane's PICKER slice. It binds exactly ten names:
// the eight `dialog:select-*` operations and the two local `skills:*` operations
// that the renderer still sends in web mode even though there is no desktop app.
// src/services/ipc-client.ts:38-45 classifies them as capability channels and
// posts them like any other operation, so left at 501 they are visible buttons
// that cannot work. Bound here, each one answers the honest browser step instead
// of the desktop's native dialog.
//
// # The ten names, and what each one means in a browser
//
//	NAME                                  BROWSER SEMANTICS                                    REFERENCE
//	dialog:select-folder                  chooser query over the library root; answers a        ipc-channels.ts:450-453
//	                                      directory listing plus a minted read grant            StageLibrary.tsx:27-30
//	dialog:select-export-directory        chooser query; answers an OUTPUT directory with a     ipc-channels.ts:503-506
//	                                      write+mkdir grant the export really spends            export-service.ts:328-416
//	dialog:select-project-archive-export  chooser query; answers the directory one archive      ipc-channels.ts:420-423
//	                                      file is written into (write+mkdir grant)              ProjectBackupPanel.tsx:145
//	dialog:select-project-restore-target  chooser query; answers the directory a restored copy ipc-channels.ts:428-431
//	                                      is created in (read grant, because the copy is author project-store.ts:876-879
//	                                      data and a grant may not write into it)
//	dialog:select-project-archive         chooser query; answers an existing archive FILE, or   ipc-channels.ts:424-427
//	                                      lists the directory being browsed for one             ProjectBackupPanel.tsx:122-124
//	dialog:select-legacy-project          chooser query; answers a legacy project DIRECTORY in  ipc-channels.ts:325-328
//	                                      the library plus the read grant the import spends     project-store.ts:876
//	dialog:select-knowledge-files         upload slot: those files live on the author's machine ipc-channels.ts:1240
//	                                      and must be uploaded; no filesystem answer exists     knowledge-service.ts:139
//	dialog:select-knowledge-folder        upload slot for one folder upload                     ipc-channels.ts:1241
//	skills:pick-local                     upload slot; the uploaded SKILL.md is inspected with  ipc-channels.ts:726-729
//	                                      the local package validator, then a single-use        app-data-controller.ts:487-517
//	                                      candidate id is minted for the confirm step
//	skills:install-local                  installs the bytes behind that candidate id under     ipc-channels.ts:730-733
//	                                      the GitHub install path's checks, never from a         app-data-controller.ts:519-544
//	                                      server path the caller merely names
//
// # Why a chooser query, and never a host path
//
// The desktop opened a native picker in the main process and answered an opaque
// grant (electron/services/external-file-grant-service.ts:26-52); the page never
// named a path. A browser has no picker and the server is the only thing that can
// see a filesystem, so the request names a SYMBOLIC root plus an optional relative
// path, and the answer is that directory's bounded listing plus the identity the
// server resolved. The allowlist, the refusals and the tokens all come from
// internal/librarypath, and this adapter deliberately shares the exact instances
// production_fs_grants_linux.go uses:
//
//   - p.grants.roots(ctx, caller) is that file's own allowlist builder, so a
//     picker can name nothing the three `fs:grant-*` names could not redeem, and
//     a refusal here is the same refusal there.
//   - p.grants.Grants() is the same process-wide registry, which is why a token
//     minted here redeems there; production_fs_grants_linux.go:190-199 exists for
//     exactly this caller.
//
// No field of any request on this surface is ever interpreted as a host path. A
// picker answer is a read: it resolves, it lists and it mints, and a refused
// picker writes neither a file nor a database row (the test measures that with a
// tree digest on both sides of the refusal block).
//
// # The answer shape, and why each consumer can use it
//
// Every picker answers one object:
//
//	{success, grantId, displayName, rootId, relativePath, identity, kind,
//	 exists, creatable, path?, entries:[{name, isDirectory, size}], truncated,
//	 maxEntries}
//
// `grantId` and `displayName` are the desktop's own ExternalDirectoryGrant /
// ProjectDirectoryGrant (ipc-channels.ts:680-685), which is what every shipped
// consumer of these six names reads: project-store.ts:876-882 passes
// `sourceRoot.grantId` and `target.grantId` to project:import-legacy-copy,
// ProjectBackupPanel.tsx:122-124 passes `archive.grantId` and `target.grantId` to
// the archive operations, and export-service.ts:328-416 spends
// `destination.grantId` on fs:grant-mkdir and fs:grant-write-file.
// `displayName` is the label project-store.ts:877 composes a copy name from.
//
// `rootId`, `relativePath` and `identity` are the browser's half: the identity is
// the addressable name of what was picked (`library:作品/第一部`), which the server
// re-resolves through the same allowlist on the next request. `entries` is
// librarypath.ListDirectory's bounded depth-1 answer - names, kinds and sizes,
// never file contents - so a page can draw a tree and descend by asking again with
// a child's relativePath; `truncated` says more names exist instead of presenting
// a partial directory as complete.
//
// `kind`, `exists` and `creatable` are three answers to three different questions,
// and none of them is inferred from the others: `kind` is what the pick resolved to
// and, for a name that is not there yet, it is the REQUEST FORM's answer (a browse
// is a directory the author may create), never a failed stat's; `exists` says
// whether the server already saw it; `creatable` says whether this name will accept
// a create. A page therefore never has to read "no entries" as "directory" or
// "file" as "not a directory yet".
//
// `path` is answered by dialog:select-folder alone, and only because that name has
// one consumer the identity cannot serve: stores/project-store.ts:664 forwards
// this answer verbatim to `project:open`, whose string form is a catalog key that
// "may look like a path" (internal/transport/projects.go:159-180, resolved by
// production_durable_linux.go:166-196). So the parent's rule is kept literally
// here: the identity is always present, and the resolved path is added for the one
// name whose consumer needs it, so no consumer receives a value it cannot use.
// No other picker is given a path, and no request ever supplies one.
//
// # The two knowledge names answer an upload slot, not a fabricated file
//
// A knowledge file the author picks lives on the author's machine. Answering a
// grant id for it would be a lie: the only files `fs:grant-read-file` can read are
// those below the library or the admitted project
// (production_fs_grants_linux.go:279-330), and an upload staging directory is
// neither. The honest browser step is therefore the byte transfer itself, and this
// file answers the slot it must go to: POST /api/uploads, or POST
// /api/upload-folders for a whole directory, with the same headers that staging
// route already reads (internal/transferhttp/uploads.go:132-215). The page then
// calls the same name again presenting the ids that route returned. `grants` is
// empty by construction - no server file exists yet - and an id this process never
// issued, or one staged for another tab, is refused by name rather than read.
//
// A folder upload's member names are LABELS, not host paths. The bytes are already
// staged by the transfer owner and nothing here resolves a member against any
// filesystem, so the rule is structural: any valid UTF-8 component is a fine name,
// including CJK, spaces and a dot-prefixed one, and a member may sit below a
// subdirectory, because the browser's folder picker forwards whatever the author's
// tree held (素材/设定/世界观.md, .obsidian/workspace.json). What is refused is a
// name that could address something outside the upload - empty, ".", "..", an
// absolute leading separator, any backslash, a control byte - plus the bounds:
// productionPickerMaxMemberBytes per component, productionPickerMaxUploadDepth
// components, MaxPathBytes in all (productionPickerUploadMember). promptskills' own
// ASCII segment pattern is NOT applied here; it exists so a skill package is one
// promptskills can install, which is a different question from what an author may
// call a note.
//
// # The two skills names install uploaded bytes, with the GitHub path's checks
//
// skills:pick-local inspects an uploaded SKILL.md - or an uploaded folder - with
// promptskills.InspectLocal, the shipped local package validator, and mints a
// single-use candidate id. skills:install-local consumes that candidate, re-reads
// the bytes it retained, requires the digest to be unchanged, and then applies the
// same gate the GitHub install applies before publishing
// (internal/promptskills/remote.go:47-60): the inspection must be compatible,
// promptskills.ValidName must accept the frontmatter name, and the published shape
// is SKILL.md alone with no bundle, which is exactly what that path installs
// (`install(raw, name, nil, false)`).
//
// # What is deliberately not here
//
// No store, no network, no new route, no write into author data, and no host path
// accepted from a request. Upload staging and skill publication are the two
// server-side owners this file does not own, so they are seams
// (productionPickerUploads, productionPickerSkillPublisher). The production lane
// constructs this adapter without them, and then every upload-dependent name
// answers its own symbolic refusal - PICKER_UPLOAD_UNAVAILABLE or
// SKILLS_LOCAL_INSTALL_UNAVAILABLE - instead of pretending. A caller that supplies
// them gets the whole flow, which is what the test does.

// The ten bound names as constants, so a typo cannot bind the wrong operation and
// the table above stays in one place.
const (
	productionPickerFolderName        = "dialog:select-folder"
	productionPickerExportName        = "dialog:select-export-directory"
	productionPickerArchiveExportName = "dialog:select-project-archive-export"
	productionPickerRestoreTargetName = "dialog:select-project-restore-target"
	productionPickerArchiveName       = "dialog:select-project-archive"
	productionPickerLegacyName        = "dialog:select-legacy-project"
	productionPickerKnowledgeFiles    = "dialog:select-knowledge-files"
	productionPickerKnowledgeFolder   = "dialog:select-knowledge-folder"
	productionPickerSkillPick         = "skills:pick-local"
	productionPickerSkillInstall      = "skills:install-local"
)

// The two purposes dialog:select-folder carries (ipc-channels.ts:450-453).
const (
	productionPickerPurposeCreate = "project-create"
	productionPickerPurposeOpen   = "project-open"
)

// The two local skill source kinds (src/shared/writing-skills.ts:41-45).
const (
	productionPickerSkillFile   = "file"
	productionPickerSkillFolder = "folder"
)

// Grant budgets. A picker's token is spent by the operation that owns the data it
// names, not by the picker: an export spends one mkdir plus one write per chapter
// (export-service.ts:352-404), an import spends one read per file it copies and a
// plain pick is read once. The registry caps one token at 65536 uses
// (internal/librarypath/grants.go:34) and its TTL at the desktop's ten minutes
// (:26-27), so these budgets live inside a window the desktop already had.
const (
	productionPickerReadUses    = 64
	productionPickerArchiveUses = 8
	productionPickerExportUses  = 4096
)

// The upload slot. These repeat internal/transferhttp's bounds
// (internal/transferhttp/uploads.go:29-35), which are unexported there: a slot
// promising more than that route accepts would be a promise the browser cannot
// keep.
const (
	productionPickerUploadRoute          = "/api/uploads"
	productionPickerUploadFolderRoute    = "/api/upload-folders"
	productionPickerUploadNameHeader     = "X-Upload-Name"
	productionPickerUploadFolderHeader   = "X-Upload-Folder"
	productionPickerUploadRelativeHeader = "X-Upload-Relative"
	productionPickerUploadContentType    = "application/octet-stream"
	productionPickerMaxUploadBytes       = int64(64 << 20)
	productionPickerMaxUploadFiles       = 1024
	productionPickerMaxCallerBytes       = int64(256 << 20)
	productionPickerMaxKnowledgeFiles    = 16
	productionPickerMaxKnowledgeBytes    = int64(8 << 20)
	productionPickerMaxUploadDepth       = 8
	productionPickerMaxMemberBytes       = 255
)

// Candidate bounds. One pending local candidate per caller, ten minutes old and
// single use, which is the desktop's own map
// (electron/controllers/app-data-controller.ts:487-544): a new pick replaces this
// tab's previous candidate and a confirm consumes it before anything is published.
const (
	productionPickerCandidateTTL   = 10 * time.Minute
	productionPickerCandidateBytes = 32
	productionPickerMaxPresented   = promptskills.MaxBundledFiles + 2
)

// The refusal vocabulary of this file. Every code is symbolic: an os error
// sentence or a host path must never reach the browser, and a code cannot carry
// one. The six picker names answer these as transport errors, because the shipped
// consumers branch on the call throwing (ExportDialog.tsx:62-72,
// use-new-project-form.ts:44-52) and would read an object carrying `success:false`
// as a selection.
const (
	productionPickerInvalidArguments = "INVALID_ARGUMENTS"
	productionPickerRootUnknownCode  = "PICKER_ROOT_UNKNOWN"
	productionPickerPathRefusedCode  = "PICKER_PATH_REFUSED"
	productionPickerEscapeCode       = "PICKER_PATH_ESCAPE"
	productionPickerNotFoundCode     = "PICKER_NOT_FOUND"
	productionPickerNotDirectoryCode = "PICKER_NOT_DIRECTORY"
	productionPickerChangedCode      = "PICKER_PATH_CHANGED"
	productionPickerUnsafeCode       = "PICKER_PATH_UNSAFE"
	productionPickerAuthorDataCode   = "PICKER_AUTHOR_DATA"
	productionPickerLimitCode        = "PICKER_LIMIT"
	productionPickerUnavailableCode  = "PICKER_UNAVAILABLE"
	productionPickerRefusedCode      = "PICKER_REFUSED"
)

// The upload refusals, answered in the knowledge envelope so the page keeps the
// slot it needs in order to retry.
const (
	productionPickerUploadUnknownCode  = "PICKER_UPLOAD_UNKNOWN"
	productionPickerUploadTooLargeCode = "PICKER_UPLOAD_TOO_LARGE"
	productionPickerUploadNotTextCode  = "PICKER_UPLOAD_NOT_TEXT"
	productionPickerUploadRequiredCode = "PICKER_UPLOAD_REQUIRED"
	productionPickerUploadLimitCode    = "PICKER_UPLOAD_LIMIT"
	productionPickerUploadUnavailCode  = "PICKER_UPLOAD_UNAVAILABLE"
)

// The skill refusals, the same envelope with a package verdict rather than a
// transport status, because SkillSettings.tsx:243-273 branches on `success`.
const (
	productionPickerSkillDamagedCode      = "SKILLS_PACKAGE_DAMAGED"
	productionPickerSkillPathCode         = "SKILLS_PACKAGE_PATH_REFUSED"
	productionPickerSkillMissingCode      = "SKILLS_PACKAGE_MISSING"
	productionPickerSkillInvalidCode      = "SKILLS_PACKAGE_INVALID"
	productionPickerSkillUnsupportedCode  = "SKILLS_PACKAGE_BUNDLE_UNSUPPORTED"
	productionPickerSkillIncompatibleCode = "SKILLS_PACKAGE_INCOMPATIBLE"
	productionPickerSkillUnsafeCode       = "SKILLS_PACKAGE_UNSAFE"
	productionPickerSkillCandidateCode    = "SKILLS_CANDIDATE_UNKNOWN"
	productionPickerSkillChangedCode      = "SKILLS_PACKAGE_CHANGED"
	productionPickerSkillPublishCode      = "SKILLS_LOCAL_INSTALL_UNAVAILABLE"
	productionPickerSkillInstallCode      = "SKILLS_INSTALL_FAILED"
)

// productionPickerSegment bounds one uploaded path component. It is
// promptskills' own segment pattern (internal/promptskills/inspection.go:22),
// repeated because that regexp is unexported: a name this file accepts and that
// package refuses would be a package that inspects but can never install.
var productionPickerSegment = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// productionPickerMarkdown bounds the markdown kinds a single-file pick accepts,
// exactly as promptskills.PackageRoot.Load accepts them
// (internal/promptskills/package.go:32).
var productionPickerMarkdown = regexp.MustCompile(`(?i)\.(?:md|markdown)$`)

// productionPickerDigest is the only shape a bundle manifest entry may carry
// (internal/promptskills/service.go:101-110).
var productionPickerDigest = regexp.MustCompile(`^[a-f0-9]{64}$`)

// productionPickerUploads is the seam through which uploaded bytes reach a
// picker. internal/transferhttp.TransferStore is the existing owner of bounded
// browser upload staging and already keys every staged file by the cookie session
// and tab that created it (uploads.go:250-296), so a small adapter over it
// supplies these two methods. This package never learns where those bytes live and
// never accepts a path for them.
type productionPickerUploads interface {
	// ReadUpload returns one staged file's bytes and its safe display name. An id
	// this owner never staged, or one staged for another caller, is an error.
	ReadUpload(sessionID, clientID, uploadID string) (name string, data []byte, err error)
	// ReadFolder returns one staged folder upload: the folder's own display name
	// and its files by relative path. The same refusal rule applies.
	ReadFolder(sessionID, clientID, folderID string) (name string, files []productionPickerUploaded, err error)
}

// productionPickerUploaded is one file of a staged upload: a single upload's own
// bytes, or one member of a folder upload.
type productionPickerUploaded struct {
	Relative string
	Data     []byte
}

// productionPickerSkillPublisher publishes one validated local skill. It takes the
// frontmatter name and the exact SKILL.md bytes, and nothing else: no path, no
// bundle and no renderer-supplied destination, because publication belongs to the
// owner of the global skill root (internal/promptskills/install_linux.go:14-30).
// Leaving it nil is a refusal, never a fallback to writing that root from here.
type productionPickerSkillPublisher func(name, raw string) error

// productionPickerQuery is the parsed browser half of one picker request: the one
// place on this surface where a request body is read.
type productionPickerQuery struct {
	RootID         string
	RelativePath   string
	MaxEntries     int
	SuggestedName  string
	Purpose        string
	Kind           string
	CandidateID    string
	UploadIDs      []string
	FolderUploadID string
}

// productionPickerSpec is what one picker name may pick. `directory` says whether
// a non-directory is refused, `creates` whether a name that does not exist yet is
// a legitimate pick (an export or restore target the owning operation will
// create), `ops` and `uses` are the grant budget, and `revealPath` marks the one
// name whose consumer needs the resolved path.
type productionPickerSpec struct {
	directory  bool
	creates    bool
	ops        librarypath.Op
	uses       int64
	revealPath bool
}

// productionPickerSpecs is that table, one row per picker name. A name absent here
// is not a picker and never reaches the pick path.
var productionPickerSpecs = map[string]productionPickerSpec{
	// Opening or creating a book. Read only, and the one path reveal, because
	// project:open's string form consumes that value.
	productionPickerFolderName: {directory: true, creates: true, ops: librarypath.OpRead, uses: productionPickerReadUses, revealPath: true},

	// An export destination is an OUTPUT: write plus mkdir, which is exactly what
	// export-service.ts spends, and never a project tree (refused before the mint).
	productionPickerExportName: {directory: true, creates: true, ops: librarypath.OpWrite | librarypath.OpMkdir, uses: productionPickerExportUses},

	// Where one archive file is written: an output directory, created if needed.
	productionPickerArchiveExportName: {directory: true, creates: true, ops: librarypath.OpWrite | librarypath.OpMkdir, uses: productionPickerArchiveUses},

	// Where a restored copy is created. Read only: the copy is a project tree, and
	// a grant may not write author data even when the caller asks for one.
	productionPickerRestoreTargetName: {directory: true, creates: true, ops: librarypath.OpRead, uses: productionPickerReadUses},

	// An existing archive to import: a file pick, or a directory to browse.
	productionPickerArchiveName: {directory: false, creates: false, ops: librarypath.OpRead, uses: productionPickerArchiveUses},

	// A legacy project tree the import copies out of: read only, many reads.
	productionPickerLegacyName: {directory: true, creates: false, ops: librarypath.OpRead, uses: productionPickerExportUses},
}

// productionPickerCandidate is one inspected local package awaiting its confirm
// step. It keeps the bytes it inspected, so install publishes exactly what the
// author was shown.
type productionPickerCandidate struct {
	id      string
	kind    string
	name    string
	raw     string
	digest  string
	expires time.Time
}

// productionPickerCandidates is the pending-candidate table: one entry per caller,
// bounded by a TTL, consumed by the first confirm.
type productionPickerCandidates struct {
	mu      sync.Mutex
	now     func() time.Time
	entries map[string]productionPickerCandidate
}

func newProductionPickerCandidates() *productionPickerCandidates {
	return &productionPickerCandidates{now: time.Now, entries: map[string]productionPickerCandidate{}}
}

// clock is the TTL seam, and nil-safe so a zero value still expires.
func (c *productionPickerCandidates) clock() time.Time {
	if c == nil || c.now == nil {
		return time.Now()
	}
	return c.now()
}

// put replaces this caller's pending candidate with a freshly inspected one and
// prunes every expired entry, so the table cannot grow past the callers using it.
func (c *productionPickerCandidates) put(owner string, candidate productionPickerCandidate) {
	if c == nil || owner == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.clock()
	for key := range c.entries {
		if !now.Before(c.entries[key].expires) {
			delete(c.entries, key)
		}
	}
	candidate.expires = now.Add(productionPickerCandidateTTL)
	c.entries[owner] = candidate
}

// take consumes a candidate for its owner. A foreign id, an expired one and one
// that was never minted are the same refusal, because none of them may publish.
func (c *productionPickerCandidates) take(owner, id string) (productionPickerCandidate, bool) {
	if c == nil || owner == "" || id == "" {
		return productionPickerCandidate{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.clock()
	for key := range c.entries {
		if !now.Before(c.entries[key].expires) {
			delete(c.entries, key)
		}
	}
	candidate, ok := c.entries[owner]
	if !ok || candidate.id != id {
		return productionPickerCandidate{}, false
	}
	delete(c.entries, owner)
	return candidate, true
}

// productionBrowserPickers is the production lane's binder for the ten names. It
// owns no root, no registry and no path: both come from productionFSGrants, the
// adapter the three `fs:grant-*` names already redeem through.
type productionBrowserPickers struct {
	open       *productionOpen
	grants     *productionFSGrants
	candidates *productionPickerCandidates
	uploads    productionPickerUploads
	publish    productionPickerSkillPublisher
}

// newProductionBrowserPickers builds the adapter beside the grant adapter, over
// the same lane and therefore the same roots. The two seams stay nil, so every
// upload-dependent name answers its own symbolic refusal until an owner supplies
// one; nothing falls back to a server path in the meantime.
func newProductionBrowserPickers(open *productionOpen) *productionBrowserPickers {
	return &productionBrowserPickers{
		open:       open,
		grants:     newProductionFSGrants(open),
		candidates: newProductionPickerCandidates(),
	}
}

// Bind installs the ten names, one server.Bind per name
// (internal/transport/server.go:73-77), so an uninventoried or doubly bound name
// fails loudly at startup instead of becoming dead code.
func (p *productionBrowserPickers) Bind(server *transport.Server) error {
	if p == nil || p.open == nil || server == nil || p.grants == nil || p.grants.Grants() == nil || p.candidates == nil {
		return errors.New("PRODUCTION_BROWSER_PICKER_BINDER_INVALID")
	}
	for _, operation := range []string{
		productionPickerFolderName,
		productionPickerExportName,
		productionPickerArchiveExportName,
		productionPickerRestoreTargetName,
		productionPickerArchiveName,
		productionPickerLegacyName,
	} {
		if err := p.bindPick(server, operation); err != nil {
			return err
		}
	}
	if err := server.Bind(productionPickerKnowledgeFiles, p.knowledgeFiles); err != nil {
		return err
	}
	if err := server.Bind(productionPickerKnowledgeFolder, p.knowledgeFolder); err != nil {
		return err
	}
	if err := server.Bind(productionPickerSkillPick, p.pickSkill); err != nil {
		return err
	}
	return server.Bind(productionPickerSkillInstall, p.installSkill)
}

// bindPick binds one chooser query name to a handler that knows its own name, so
// the per-name spec is selected by the bound operation and not by the request.
func (p *productionBrowserPickers) bindPick(server *transport.Server, operation string) error {
	return server.Bind(operation, func(ctx context.Context, caller transport.Caller, args []any) (any, error) {
		return p.pickOperation(ctx, caller, operation, args)
	})
}

// pickOperation answers one chooser query: resolve inside the shared allowlist,
// bound the listing, mint the token the owning operation will spend.
func (p *productionBrowserPickers) pickOperation(ctx context.Context, caller transport.Caller, operation string, args []any) (any, error) {
	spec, known := productionPickerSpecs[operation]
	if !known {
		return nil, security.NewHTTPError(http.StatusNotFound, "GO_OPERATION_NOT_MIGRATED")
	}
	query, err := productionPickerQueryFor(operation, args)
	if err != nil {
		return nil, err
	}
	roots, binding, err := p.grants.roots(ctx, caller)
	if err != nil {
		return nil, err
	}
	target, err := roots.Resolve(query.RootID, query.RelativePath)
	if err != nil {
		return nil, productionPickerRefusal(err)
	}
	creates := spec.creates
	if operation == productionPickerFolderName && query.Purpose == productionPickerPurposeOpen {
		creates = false
	}
	// The two forms disagree on purpose. A directory name is refused when it is a
	// file, because a directory pick cannot answer with a file and walking into
	// its parent would answer a different question; a file-form name is allowed to
	// resolve to a directory, because `dialog:select-project-archive` is one name
	// for two steps - pick the archive, or browse to it (ipc-channels.ts:503).
	// Existence is the one rule both forms share: a name that is not there is
	// PICKER_NOT_FOUND for a file pick and for a browse alike, unless this name may
	// create it (creates), in which case the answer says so instead of inventing a
	// listing.
	if spec.directory && target.Exists && !target.IsDir {
		return nil, productionPickerRefusal(librarypath.ErrPathNotDirectory)
	}
	if !target.Exists && !creates {
		return nil, productionPickerRefusal(fs.ErrNotExist)
	}
	// A write-capable pick is an OUTPUT pick. It is refused here, before the mint,
	// so a page never holds a grant that could write author data
	// (librarypath/io.go:198-209 requireOutput). The check runs on this path and
	// not at spend time on purpose: the refusal has to reach the picker, which is
	// the only place the author can be told why.
	if spec.ops.Permits(librarypath.OpWrite) {
		authors, authorErr := p.authorPaths(ctx)
		if authorErr != nil {
			return nil, productionPickerRefusal(authorErr)
		}
		if err := productionPickerOutputTarget(roots, target, authors); err != nil {
			return nil, productionPickerRefusal(err)
		}
	}
	// The listing is taken before the token is minted: a pick that cannot be listed
	// must not leave a live capability behind.
	entries := []any{}
	truncated := false
	maxEntries := query.MaxEntries
	if maxEntries == 0 {
		maxEntries = librarypath.DefaultListEntries
	}
	if target.Exists && target.IsDir {
		listing, listErr := librarypath.ListDirectory(target, maxEntries)
		if listErr != nil {
			return nil, productionPickerRefusal(listErr)
		}
		for _, entry := range listing.Entries {
			entries = append(entries, map[string]any{
				"name":        entry.Name,
				"isDirectory": entry.Dir,
				"size":        entry.Size,
			})
		}
		truncated = listing.Truncated
	}
	registry := p.grants.Grants()
	token, err := registry.Mint(binding, target, spec.ops, librarypath.MintOptions{MaxUses: spec.uses})
	if err != nil {
		return nil, productionPickerRefusal(err)
	}
	answer := map[string]any{
		"success":      true,
		"grantId":      token,
		"displayName":  productionPickerDisplayName(target),
		"rootId":       target.RootID,
		"relativePath": target.Relative,
		"identity":     productionPickerIdentity(target),
		"kind":         productionPickerKind(target, spec),
		"exists":       target.Exists,
		"creatable":    !target.Exists && creates,
		"entries":      entries,
		"truncated":    truncated,
		"maxEntries":   maxEntries,
	}
	if spec.revealPath {
		// The one documented exception: project:open's string form is a catalog key
		// that looks like a path (internal/transport/projects.go:159-180) and this
		// name's answer is forwarded to it whole (project-store.ts:664).
		answer["path"] = target.Path
	}
	if query.SuggestedName != "" {
		// The suggested name was validated as one component and is only echoed back:
		// it is the default file name a desktop save dialog would have shown, and it
		// is never joined onto the target here.
		answer["suggestedName"] = query.SuggestedName
	}
	return answer, nil
}

// productionPickerDisplayName is the label a grant carries: the picked directory's
// or file's own name, never a host path the page could re-address.
func productionPickerDisplayName(target librarypath.Target) string {
	name := target.Path
	if index := strings.LastIndexByte(name, '/'); index >= 0 && index+1 < len(name) {
		name = name[index+1:]
	}
	if name == "" || name == "/" {
		return target.RootID
	}
	return name
}

// productionPickerIdentity is the addressable, symbolic name of what was picked: a
// root id and a relative path, which the server re-resolves on the next request.
func productionPickerIdentity(target librarypath.Target) string {
	if target.Relative == "" {
		return target.RootID
	}
	return target.RootID + "/" + target.Relative
}

// productionPickerKind names what the answer resolved to. A name that does not
// exist yet is classified by the request form and not by the failed stat: an
// unborn export directory is a directory the caller may create, and answering
// "file" would tell the renderer to draw it as one. `exists` and `creatable` carry
// the rest of the truth, so the page never has to infer a directory from a zero
// entry count.
func productionPickerKind(target librarypath.Target, spec productionPickerSpec) string {
	if target.Exists {
		if target.IsDir {
			return "directory"
		}
		return "file"
	}
	if spec.directory {
		return "directory"
	}
	return "file"
}

// authorPaths is every project tree this lane admitted: the author data a
// write-capable pick may never name, whether or not that project is open. It is the
// same inventory /api/library serves, read from the startup admission this process
// already holds, so a project tree that keeps its manifest under the hidden storage
// directory is still recognised: no allowlist resolution can address that
// component, and a probe of the visible names alone would miss every real project.
func (p *productionBrowserPickers) authorPaths(ctx context.Context) ([]string, error) {
	if p == nil || p.open == nil {
		return nil, librarypath.ErrPathUnavailable
	}
	listed, err := p.open.ListCanonicalProjects(ctx, projectfs.MaxLibraryEntries)
	if err != nil {
		return nil, err
	}
	authors := make([]string, 0, len(listed))
	for _, item := range listed {
		if item.Path != "" {
			authors = append(authors, item.Path)
		}
	}
	return authors, nil
}

// productionPickerInside reports whether one canonical path is the other or sits
// beneath it. Both are absolute and cleaned, so a string prefix with a separator
// is the whole test.
func productionPickerInside(path, parent string) bool {
	if path == "" || parent == "" {
		return false
	}
	return path == parent || strings.HasPrefix(path, parent+"/")
}

// productionPickerOutputTarget refuses a write-capable pick whose target is author
// data: the project root itself, a directory inside an admitted project tree, or a
// directory holding a project manifest. It reads one name through the same
// allowlist, reads no author file and writes nothing.
func productionPickerOutputTarget(roots librarypath.Roots, target librarypath.Target, authors []string) error {
	if target.Kind == librarypath.KindProject {
		return librarypath.ErrAuthorData
	}
	for _, author := range authors {
		if productionPickerInside(target.Path, author) {
			return librarypath.ErrAuthorData
		}
	}
	probe := librarypath.ProjectManifestName
	if target.Relative != "" {
		probe = target.Relative + "/" + librarypath.ProjectManifestName
	}
	marker, err := roots.Resolve(target.RootID, probe)
	if err != nil {
		// A name the allowlist will not even resolve cannot be a project marker.
		// Not knowing is not a claim that it is one, so the pick stands: the
		// admitted-project check above is what covers a real project tree.
		return nil
	}
	if marker.Exists {
		return librarypath.ErrAuthorData
	}
	return nil
}

// productionPickerRefusal maps one librarypath refusal to one symbolic picker
// refusal with a status a browser can distinguish. The mapping is finite on
// purpose, so a wrapped os error or host path never survives it.
func productionPickerRefusal(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return security.NewHTTPError(http.StatusServiceUnavailable, "REQUEST_CANCELED")
	}
	status, code := http.StatusInternalServerError, productionPickerRefusedCode
	switch {
	case errors.Is(err, librarypath.ErrRootUnknown):
		status, code = http.StatusForbidden, productionPickerRootUnknownCode
	case errors.Is(err, librarypath.ErrRootInvalid):
		status, code = http.StatusServiceUnavailable, productionPickerUnavailableCode
	case errors.Is(err, librarypath.ErrPathAbsolute),
		errors.Is(err, librarypath.ErrPathSeparator),
		errors.Is(err, librarypath.ErrPathEncoded),
		errors.Is(err, librarypath.ErrPathTraversal),
		errors.Is(err, librarypath.ErrPathHidden),
		errors.Is(err, librarypath.ErrPathInvalid):
		status, code = http.StatusForbidden, productionPickerPathRefusedCode
	case errors.Is(err, librarypath.ErrSymlinkEscape):
		status, code = http.StatusForbidden, productionPickerEscapeCode
	case errors.Is(err, librarypath.ErrPathNotDirectory):
		status, code = http.StatusBadRequest, productionPickerNotDirectoryCode
	case errors.Is(err, librarypath.ErrPathUnavailable):
		status, code = http.StatusServiceUnavailable, productionPickerUnavailableCode
	case errors.Is(err, librarypath.ErrChanged):
		status, code = http.StatusConflict, productionPickerChangedCode
	case errors.Is(err, librarypath.ErrUnsafeEntry):
		status, code = http.StatusForbidden, productionPickerUnsafeCode
	case errors.Is(err, librarypath.ErrAuthorData):
		status, code = http.StatusForbidden, productionPickerAuthorDataCode
	case errors.Is(err, librarypath.ErrTooLarge):
		status, code = http.StatusRequestEntityTooLarge, productionPickerUploadTooLargeCode
	case errors.Is(err, librarypath.ErrListingInvalid):
		status, code = http.StatusBadRequest, productionPickerInvalidArguments
	case errors.Is(err, librarypath.ErrBindingInvalid):
		status, code = http.StatusBadRequest, "PROJECT_SESSION_REQUIRED"
	case errors.Is(err, librarypath.ErrGrantLimit):
		status, code = http.StatusTooManyRequests, productionPickerLimitCode
	case errors.Is(err, librarypath.ErrRegistryUnavailable):
		status, code = http.StatusServiceUnavailable, productionPickerUnavailableCode
	case errors.Is(err, librarypath.ErrGrantUnknown), errors.Is(err, librarypath.ErrTargetInvalid):
		status, code = http.StatusConflict, productionPickerRefusedCode
	case errors.Is(err, fs.ErrNotExist):
		status, code = http.StatusNotFound, productionPickerNotFoundCode
	}
	return security.NewHTTPError(status, code)
}

// productionPickerQueryFor parses one picker request into the one query shape this
// surface understands. It is strict on purpose: the wire forms the desktop used are
// accepted as they are (a purpose string, a suggested name, a source kind), the
// browser adds a chooser object, and anything else is refused rather than guessed
// at. No field here is ever read as a host path.
func productionPickerQueryFor(operation string, args []any) (productionPickerQuery, error) {
	query := productionPickerQuery{RootID: librarypath.LibraryRootID}
	invalid := func() (productionPickerQuery, error) {
		return productionPickerQuery{}, security.NewHTTPError(http.StatusBadRequest, productionPickerInvalidArguments)
	}
	if len(args) > 1 {
		return invalid()
	}
	if len(args) == 0 || args[0] == nil {
		return query, nil
	}
	switch value := args[0].(type) {
	case string:
		switch operation {
		case productionPickerFolderName:
			if value != productionPickerPurposeCreate && value != productionPickerPurposeOpen {
				return invalid()
			}
			query.Purpose = value
		case productionPickerArchiveExportName, productionPickerRestoreTargetName:
			if !productionPickerSafeName(value) {
				return invalid()
			}
			query.SuggestedName = value
		case productionPickerSkillPick:
			if value != productionPickerSkillFile && value != productionPickerSkillFolder {
				return invalid()
			}
			query.Kind = value
		case productionPickerSkillInstall:
			if !productionPickerOpaqueID(value) {
				return invalid()
			}
			query.CandidateID = value
		default:
			// A name that takes no argument on the wire refuses one here too: a
			// string is not a path in disguise.
			return invalid()
		}
	case map[string]any:
		parsed, err := productionPickerQueryObject(operation, value)
		if err != nil {
			return invalid()
		}
		query = parsed
	default:
		return invalid()
	}
	return query, nil
}

// productionPickerQueryObject decodes the browser's chooser object. The allowed
// keys are per name and every value is type-checked: an unknown key is refused
// instead of ignored, so a request that tries to smuggle `path` or `grantId` in a
// body fails loudly.
func productionPickerQueryObject(operation string, value map[string]any) (productionPickerQuery, error) {
	query := productionPickerQuery{RootID: librarypath.LibraryRootID}
	allowed := map[string]bool{"rootId": true, "relativePath": true, "maxEntries": true, "suggestedName": true}
	switch operation {
	case productionPickerFolderName:
		allowed["purpose"] = true
	case productionPickerKnowledgeFiles:
		allowed = map[string]bool{"uploadIds": true}
	case productionPickerKnowledgeFolder:
		allowed = map[string]bool{"folderUploadId": true}
	case productionPickerSkillPick:
		allowed = map[string]bool{"kind": true, "uploadIds": true, "folderUploadId": true}
	case productionPickerSkillInstall:
		allowed = map[string]bool{"candidateId": true}
	}
	fail := func() (productionPickerQuery, error) {
		return productionPickerQuery{}, security.NewHTTPError(http.StatusBadRequest, productionPickerInvalidArguments)
	}
	for key := range value {
		if !allowed[key] {
			return fail()
		}
	}
	for key, item := range value {
		switch key {
		case "rootId":
			text, ok := item.(string)
			if !ok || text == "" || len(text) > 64 {
				return fail()
			}
			query.RootID = text
		case "relativePath":
			text, ok := item.(string)
			if !ok {
				return fail()
			}
			query.RelativePath = text
		case "maxEntries":
			count, ok := productionPickerCount(item)
			if !ok {
				return fail()
			}
			query.MaxEntries = count
		case "suggestedName":
			text, ok := item.(string)
			if !ok || !productionPickerSafeName(text) {
				return fail()
			}
			query.SuggestedName = text
		case "purpose":
			text, ok := item.(string)
			if !ok || (text != productionPickerPurposeCreate && text != productionPickerPurposeOpen) {
				return fail()
			}
			query.Purpose = text
		case "kind":
			text, ok := item.(string)
			if !ok || (text != productionPickerSkillFile && text != productionPickerSkillFolder) {
				return fail()
			}
			query.Kind = text
		case "candidateId":
			text, ok := item.(string)
			if !ok || !productionPickerOpaqueID(text) {
				return fail()
			}
			query.CandidateID = text
		case "uploadIds":
			ids, ok := productionPickerIDs(item)
			if !ok {
				return fail()
			}
			query.UploadIDs = ids
		case "folderUploadId":
			text, ok := item.(string)
			if !ok || !productionPickerOpaqueID(text) {
				return fail()
			}
			query.FolderUploadID = text
		}
	}
	return query, nil
}

// productionPickerIDs reads a bounded list of opaque ids. Each one is checked as an
// id, never as a path: it may only ever name an upload this server staged.
func productionPickerIDs(item any) ([]string, bool) {
	list, ok := item.([]any)
	if !ok || len(list) > productionPickerMaxKnowledgeFiles {
		return nil, false
	}
	ids := make([]string, 0, len(list))
	for _, entry := range list {
		text, ok := entry.(string)
		if !ok || !productionPickerOpaqueID(text) {
			return nil, false
		}
		ids = append(ids, text)
	}
	return ids, true
}

// productionPickerCount reads a bounded entry cap from a JSON number.
func productionPickerCount(item any) (int, bool) {
	switch value := item.(type) {
	case float64:
		if value != float64(int(value)) || value < 1 || value > librarypath.MaxListEntries {
			return 0, false
		}
		return int(value), true
	case int:
		if value < 1 || value > librarypath.MaxListEntries {
			return 0, false
		}
		return value, true
	}
	return 0, false
}

// productionPickerSafeName bounds a suggested display name: one component, valid
// UTF-8, no separator and no control byte, so it can neither be joined onto a
// directory nor carry a path out of one.
func productionPickerSafeName(value string) bool {
	if value == "" || len(value) > 200 || !utf8.ValidString(value) {
		return false
	}
	return !strings.ContainsAny(value, "/\\\x00")
}

// productionPickerOpaqueID bounds an opaque id (an upload, a folder upload or a
// candidate): printable, no separator, no NUL, bounded length. An id is not a
// path, so a traversal inside one is simply an id this server never minted.
func productionPickerOpaqueID(value string) bool {
	if value == "" || len(value) > 128 || !utf8.ValidString(value) {
		return false
	}
	for _, char := range value {
		if char < 0x21 || char == 0x7f || char == '/' || char == '\\' {
			return false
		}
	}
	return true
}

// productionPickerBinding is the caller identity a token or an upload belongs to.
func productionPickerBinding(caller transport.Caller) (librarypath.Binding, error) {
	owner, err := productionOwner(caller)
	if err != nil {
		return librarypath.Binding{}, security.NewHTTPError(http.StatusBadRequest, "PROJECT_SESSION_REQUIRED")
	}
	binding := librarypath.Binding{SessionID: owner.SessionID, ClientID: owner.ClientID}
	if !binding.Valid() {
		return librarypath.Binding{}, security.NewHTTPError(http.StatusBadRequest, "PROJECT_SESSION_REQUIRED")
	}
	return binding, nil
}

// productionPickerOwnerKey is the pending-candidate key: the cookie session and the
// tab, exactly as the upload staging owner keys its files.
func productionPickerOwnerKey(binding librarypath.Binding) string {
	return binding.SessionID + "\x00" + binding.ClientID
}

// errProductionPickerNoUploads is the seam's own refusal when no upload owner is
// installed in this lane. It is never shown: every caller maps it to a symbolic
// code.
var errProductionPickerNoUploads = errors.New("PRODUCTION_PICKER_UPLOAD_UNAVAILABLE")

// readUpload and readFolder are the two nil-safe uses of the seam. They pass the
// caller's own cookie session and tab, never an argument, so the staging owner can
// refuse another tab's id itself.
func (p *productionBrowserPickers) readUpload(binding librarypath.Binding, id string) (string, []byte, error) {
	if p.uploads == nil {
		return "", nil, errProductionPickerNoUploads
	}
	return p.uploads.ReadUpload(binding.SessionID, binding.ClientID, id)
}

func (p *productionBrowserPickers) readFolder(binding librarypath.Binding, id string) (string, []productionPickerUploaded, error) {
	if p.uploads == nil {
		return "", nil, errProductionPickerNoUploads
	}
	return p.uploads.ReadFolder(binding.SessionID, binding.ClientID, id)
}

// productionPickerUploadSlot is the upload step for one name. The next call
// presents the ids this slot's route returns, under the field `resume.idField`.
func productionPickerUploadSlot(operation, kind, idField string) map[string]any {
	slot := map[string]any{
		"kind":            kind,
		"url":             productionPickerUploadRoute,
		"folderUploadUrl": productionPickerUploadFolderRoute,
		"method":          http.MethodPost,
		"contentType":     productionPickerUploadContentType,
		"nameHeader":      productionPickerUploadNameHeader,
		"folderHeader":    productionPickerUploadFolderHeader,
		"relativeHeader":  productionPickerUploadRelativeHeader,
		"maxFileBytes":    productionPickerMaxUploadBytes,
		"maxFiles":        productionPickerMaxUploadFiles,
		"maxCallerBytes":  productionPickerMaxCallerBytes,
		"resume":          map[string]any{
			"operation": operation,
			"idField":   idField,
		},
	}
	if operation == productionPickerSkillPick {
		slot["acceptedExtensions"] = []string{"md", "markdown"}
	}
	return slot
}

// productionPickerKnowledgeAnswer is the two knowledge names' envelope. `grants` is
// always empty: a browser pick cannot produce a readable grant, and an empty list
// is what a consumer that iterates that field then sees - nothing - instead of a
// fabricated file.
func productionPickerKnowledgeAnswer(operation string, verified []any) map[string]any {
	kind, idField := "files", "uploadIds"
	if operation == productionPickerKnowledgeFolder {
		kind, idField = "folder", "folderUploadId"
	}
	return map[string]any{
		"success":   true,
		"cancelled": false,
		"slot":      operation,
		"upload":    productionPickerUploadSlot(operation, kind, idField),
		"grants":    []any{},
		"files":     verified,
		"next":      "kb:import-planning-text",
	}
}

// productionPickerKnowledgeRefusal is that envelope, refused by name. It keeps the
// slot so the page can retry the upload it just failed.
func productionPickerKnowledgeRefusal(operation, code string) map[string]any {
	kind, idField := "files", "uploadIds"
	if operation == productionPickerKnowledgeFolder {
		kind, idField = "folder", "folderUploadId"
	}
	return map[string]any{
		"success":   false,
		"cancelled": false,
		"error":     code,
		"slot":      operation,
		"upload":    productionPickerUploadSlot(operation, kind, idField),
		"grants":    []any{},
		"files":     []any{},
	}}

// productionPickerUploadedWire is one verified uploaded file. Its grantId is
// deliberately empty and `readable` says so by name: no server file exists for it
// yet, so no `fs:grant-read-file` may be spent on it. `uploadId` is the identity
// the owning import operation continues from.
func productionPickerUploadedWire(uploadID, displayName string, data []byte) map[string]any {
	return map[string]any{
		"grantId":       "",
		"displayName":   displayName,
		"uploadId":      uploadID,
		"size":          len(data),
		"contentSha256": productionPickerSHA256(data),
		"readable":      false,
	}
}

// knowledgeFiles serves `dialog:select-knowledge-files`: the upload slot when no
// file has arrived, and the verified identity of each staged file when it has.
func (p *productionBrowserPickers) knowledgeFiles(_ context.Context, caller transport.Caller, args []any) (any, error) {
	query, err := productionPickerQueryFor(productionPickerKnowledgeFiles, args)
	if err != nil {
		return nil, err
	}
	binding, err := productionPickerBinding(caller)
	if err != nil {
		return nil, err
	}
	if len(query.UploadIDs) == 0 {
		// Nothing presented: the honest answer is the slot, not a file.
		return productionPickerKnowledgeAnswer(productionPickerKnowledgeFiles, []any{}), nil
	}
	if p.uploads == nil {
		return productionPickerKnowledgeRefusal(productionPickerKnowledgeFiles, productionPickerUploadUnavailCode), nil
	}
	verified := make([]any, 0, len(query.UploadIDs))
	for _, id := range query.UploadIDs {
		name, data, readErr := p.readUpload(binding, id)
		if readErr != nil {
			return productionPickerKnowledgeRefusal(productionPickerKnowledgeFiles, productionPickerUploadCode(readErr)), nil
		}
		if int64(len(data)) > productionPickerMaxKnowledgeBytes {
			return productionPickerKnowledgeRefusal(productionPickerKnowledgeFiles, productionPickerUploadTooLargeCode), nil
		}
		if !utf8.Valid(data) {
			return productionPickerKnowledgeRefusal(productionPickerKnowledgeFiles, productionPickerUploadNotTextCode), nil
		}
		verified = append(verified, productionPickerUploadedWire(id, productionPickerDisplayFrom(name, id), data))
	}
	return productionPickerKnowledgeAnswer(productionPickerKnowledgeFiles, verified), nil
}

// knowledgeFolder serves `dialog:select-knowledge-folder`: the folder upload slot,
// and the verified members of one staged folder when it is presented.
func (p *productionBrowserPickers) knowledgeFolder(_ context.Context, caller transport.Caller, args []any) (any, error) {
	query, err := productionPickerQueryFor(productionPickerKnowledgeFolder, args)
	if err != nil {
		return nil, err
	}
	binding, err := productionPickerBinding(caller)
	if err != nil {
		return nil, err
	}
	if query.FolderUploadID == "" {
		return productionPickerKnowledgeAnswer(productionPickerKnowledgeFolder, []any{}), nil
	}
	if p.uploads == nil {
		return productionPickerKnowledgeRefusal(productionPickerKnowledgeFolder, productionPickerUploadUnavailCode), nil
	}
	// The staged folder's own display name is not joined onto anything here: the
	// verified members carry the folder id and their own relative names.
	_, staged, readErr := p.readFolder(binding, query.FolderUploadID)
	if readErr != nil {
		return productionPickerKnowledgeRefusal(productionPickerKnowledgeFolder, productionPickerUploadUnknownCode), nil
	}
	if len(staged) > productionPickerMaxUploadFiles || len(staged) == 0 {
		return productionPickerKnowledgeRefusal(productionPickerKnowledgeFolder, productionPickerUploadLimitCode), nil
	}
	verified := make([]any, 0, len(staged))
	var total int64
	for _, file := range staged {
		if !productionPickerUploadMember(file.Relative) {
			return productionPickerKnowledgeRefusal(productionPickerKnowledgeFolder, productionPickerPathRefusedCode), nil
		}
		total += int64(len(file.Data))
		if total > productionPickerMaxKnowledgeBytes {
			return productionPickerKnowledgeRefusal(productionPickerKnowledgeFolder, productionPickerUploadTooLargeCode), nil
		}
		if !utf8.Valid(file.Data) {
			return productionPickerKnowledgeRefusal(productionPickerKnowledgeFolder, productionPickerUploadNotTextCode), nil
		}
		verified = append(verified, productionPickerUploadedWire(query.FolderUploadID+"/"+file.Relative, file.Relative, file.Data))
	}
	return productionPickerKnowledgeAnswer(productionPickerKnowledgeFolder, verified), nil
}

// pickSkill serves `skills:pick-local`: inspect one uploaded SKILL.md, or one
// uploaded folder, and mint the single-use candidate its confirm step installs.
func (p *productionBrowserPickers) pickSkill(_ context.Context, caller transport.Caller, args []any) (any, error) {
	query, err := productionPickerQueryFor(productionPickerSkillPick, args)
	if err != nil {
		return nil, err
	}
	binding, err := productionPickerBinding(caller)
	if err != nil {
		return nil, err
	}
	if query.Kind == "" {
		query.Kind = productionPickerSkillFile
	}
	refusal := func(code string) map[string]any {
		kind, idField := query.Kind, "uploadIds"
		if query.Kind == productionPickerSkillFolder {
			idField = "folderUploadId"
		}
		return map[string]any{
			"success":   false,
			"cancelled": false,
			"error":     code,
			"upload":    productionPickerUploadSlot(productionPickerSkillPick, kind, idField),
		}
	}
	// Nothing presented yet: a browser has no picker, so the honest answer is the
	// upload step that must precede the inspection.
	if len(query.UploadIDs) == 0 && query.FolderUploadID == "" {
		return refusal(productionPickerUploadRequiredCode), nil
	}
	if p.uploads == nil {
		return refusal(productionPickerUploadUnavailCode), nil
	}
	presented := []productionPickerUploaded{}
	sourceName := ""
	if query.Kind == productionPickerSkillFolder {
		if query.FolderUploadID == "" || len(query.UploadIDs) != 0 {
			return nil, security.NewHTTPError(http.StatusBadRequest, productionPickerInvalidArguments)
		}
		name, staged, readErr := p.readFolder(binding, query.FolderUploadID)
		if readErr != nil {
			return refusal(productionPickerUploadCode(readErr)), nil
		}
		presented = staged
		sourceName = productionPickerSafeLabel(name)
	} else {
		if len(query.UploadIDs) != 1 || query.FolderUploadID != "" {
			return nil, security.NewHTTPError(http.StatusBadRequest, productionPickerInvalidArguments)
		}
		name, data, readErr := p.readUpload(binding, query.UploadIDs[0])
		if readErr != nil {
			return refusal(productionPickerUploadCode(readErr)), nil
		}
		display := productionPickerDisplayFrom(name, query.UploadIDs[0])
		presented = []productionPickerUploaded{{Relative: display, Data: data}}
		sourceName = productionPickerBaseName(display)
	}
	inspection, sourceName, raw, code := productionPickerInspect(query.Kind, presented, sourceName)
	if code != "" {
		return refusal(code), nil
	}
	// A bundle is refused at the pick rather than at the confirm: the GitHub install
	// path installs SKILL.md alone (`install(raw, name, nil, false)`,
	// internal/promptskills/remote.go:60) and this lane's publisher carries no
	// bundle, so accepting one would promise an install that silently drops most of
	// the package.
	if len(inspection.BundledFiles) != 0 {
		return refusal(productionPickerSkillUnsupportedCode), nil
	}
	candidateID, idErr := productionPickerToken()
	if idErr != nil {
		return refusal(productionPickerUnavailableCode), nil
	}
	p.candidates.put(productionPickerOwnerKey(binding), productionPickerCandidate{
		id:     candidateID,
		kind:   query.Kind,
		name:   sourceName,
		raw:    raw,
		digest: productionPickerSHA256([]byte(raw)),
	})
	skipped := []any{}
	if query.Kind == productionPickerSkillFolder {
		for _, file := range presented {
			if !strings.EqualFold(file.Relative, "SKILL.md") {
				skipped = append(skipped, file.Relative)
			}
		}
	}
	wire := productionPickerInspectionWire(inspection)
	wire["candidateId"] = candidateID
	wire["sourceKind"] = query.Kind
	wire["sourceName"] = sourceName
	wire["contentSha256"] = productionPickerSHA256([]byte(raw))
	wire["skippedFiles"] = skipped
	return map[string]any{"success": true, "cancelled": false, "inspection": wire}, nil
}

// installSkill serves `skills:install-local`: consume the candidate, re-verify the
// bytes it inspected, then apply the GitHub install path's own gate before
// publishing. No path is read here: the only input is the candidate id.
func (p *productionBrowserPickers) installSkill(_ context.Context, caller transport.Caller, args []any) (any, error) {
	query, err := productionPickerQueryFor(productionPickerSkillInstall, args)
	if err != nil {
		return nil, err
	}
	binding, err := productionPickerBinding(caller)
	if err != nil {
		return nil, err
	}
	if query.CandidateID == "" {
		return nil, security.NewHTTPError(http.StatusBadRequest, productionPickerInvalidArguments)
	}
	candidate, ok := p.candidates.take(productionPickerOwnerKey(binding), query.CandidateID)
	if !ok {
		return productionPickerSkillRefusal(productionPickerSkillCandidateCode), nil
	}
	// The candidate was consumed above, so a failed verify cannot be retried against
	// the same id: the author picks again, exactly as the desktop's single pending
	// candidate behaves.
	if productionPickerSHA256([]byte(candidate.raw)) != candidate.digest {
		return productionPickerSkillRefusal(productionPickerSkillChangedCode), nil
	}
	// Re-inspected with the same validator and the same fallback the pick used, so
	// the name the page was shown is the name that installs.
	inspection := promptskills.InspectLocal(candidate.raw, nil, productionPickerFallback(candidate.kind, candidate.name))
	if !inspection.Compatible {
		return productionPickerSkillRefusal(productionPickerSkillIncompatibleCode), nil
	}
	if !promptskills.ValidName(inspection.Metadata.Name) {
		return productionPickerSkillRefusal(productionPickerSkillUnsafeCode), nil
	}
	if p.publish == nil {
		// No owner for the global skill root in this lane: refuse by name rather than
		// write that root from here (internal/promptskills/package.go:22-24).
		return productionPickerSkillRefusal(productionPickerSkillPublishCode), nil
	}
	if publishErr := p.publish(inspection.Metadata.Name, candidate.raw); publishErr != nil {
		return productionPickerSkillRefusal(productionPickerSkillInstallCode), nil
	}
	return map[string]any{
		"success":   true,
		"cancelled": false,
		"skill":     promptskills.InstalledSkill{
			Name:       inspection.Metadata.Name,
			Source:     "user",
			Version:    inspection.Metadata.Version,
			Language:   inspection.Metadata.Language,
			Compatible: true,
			UTF8Bytes:  inspection.UTF8Bytes,
		},
	}, nil
}

// productionPickerSkillRefusal is the skills envelope, refused by name, so
// SkillSettings.tsx:243-273 reads `success` and shows the code.
func productionPickerSkillRefusal(code string) map[string]any {
	return map[string]any{"success": false, "cancelled": false, "error": code}
}

// productionPickerInspect validates one presented package with the validator the
// shipped local picker uses (promptskills.PackageRoot.Load, package.go:31-60) and
// returns the inspection, the display name and the SKILL.md bytes. An empty code
// means the package may be inspected; anything else is the refusal to answer.
//
// The rules are that function's rules, applied to uploaded bytes: at most
// MaxFolderEntries members, no symlinked member (there are none in an upload, so
// the check is the path shape), SKILL.md at the package root, hidden members
// skipped exactly as its walk skips them, every retained member bounded UTF-8
// text, and a frontmatter name promptskills.ValidName accepts.
func productionPickerInspect(kind string, presented []productionPickerUploaded, sourceName string) (promptskills.Inspection, string, string, string) {
	if len(presented) == 0 || len(presented) > productionPickerMaxPresented+promptskills.MaxBundledFiles {
		return promptskills.Inspection{}, "", "", productionPickerLimitCode
	}
	if kind == productionPickerSkillFile {
		file := presented[0]
		if !productionPickerMarkdown.MatchString(file.Relative) {
			return promptskills.Inspection{}, "", "", productionPickerSkillMissingCode
		}
		raw, ok := productionPickerSkillText(file.Data)
		if !ok {
			return promptskills.Inspection{}, "", "", productionPickerSkillInvalidCode
		}
		inspection := promptskills.InspectLocal(raw, nil, productionPickerFallback(kind, sourceName))
		if !promptskills.ValidName(inspection.Metadata.Name) {
			return promptskills.Inspection{}, "", "", productionPickerSkillInvalidCode
		}
		return inspection, sourceName, raw, ""
	}
	manifestName := strings.ToLower(promptskills.BundleManifest)
	files := map[string][]byte{}
	skipped := []string{}
	skill := ""
	for _, file := range presented {
		lower := strings.ToLower(file.Relative)
		if lower == manifestName {
			// The manifest is the archive's own index: checked below, never treated as
			// package content and never offered to the reference reader.
			continue
		}
		if !productionPickerSafePath(file.Relative) {
			return promptskills.Inspection{}, "", "", productionPickerSkillPathCode
		}
		if productionPickerHiddenPath(file.Relative) {
			// A hidden member is skipped, not refused: that is what the shipped
			// folder walk does (package.go:34), and refusing would fail an otherwise
			// valid package over a stray editor file.
			skipped = append(skipped, file.Relative)
			continue
		}
		if _, repeated := files[lower]; repeated {
			return promptskills.Inspection{}, "", "", productionPickerSkillPathCode
		}
		if _, ok := productionPickerSkillText(file.Data); !ok {
			return promptskills.Inspection{}, "", "", productionPickerSkillInvalidCode
		}
		files[lower] = file.Data
		if lower == "skill.md" {
			skill = file.Relative
		}
	}
	if skill == "" {
		return promptskills.Inspection{}, "", "", productionPickerSkillMissingCode
	}
	if manifest, present := productionPickerManifestMember(presented, manifestName); present {
		// A manifest that does not describe exactly the bytes that arrived is a
		// damaged archive, and a damaged archive is never inspected, never becomes a
		// candidate and never installs.
		if !productionPickerBundleIntact(manifest, files) {
			return promptskills.Inspection{}, "", "", productionPickerSkillDamagedCode
		}
	}
	raw, ok := productionPickerSkillText(files["skill.md"])
	if !ok {
		return promptskills.Inspection{}, "", "", productionPickerSkillInvalidCode
	}
	sourceName = productionPickerSafeLabel(sourceName)
	read := func(relative string) (string, bool) {
		if !productionPickerSafePath(relative) || !productionPickerTextName(relative) {
			return "", false
		}
		data, found := files[strings.ToLower(relative)]
		if !found {
			return "", false
		}
		return string(data), true
	}
	inspection := promptskills.InspectLocal(raw, read, sourceName)
	if !promptskills.ValidName(inspection.Metadata.Name) {
		return promptskills.Inspection{}, "", "", productionPickerSkillInvalidCode
	}
	inspection.UnbundledReferences = append(inspection.UnbundledReferences, skipped...)
	return inspection, sourceName, raw, ""
}

// productionPickerManifestMember returns the archive's own index, which is the one
// dot-prefixed name this lane reads, and only to verify the bytes beside it.
func productionPickerManifestMember(presented []productionPickerUploaded, lower string) ([]byte, bool) {
	for _, file := range presented {
		if strings.ToLower(file.Relative) == lower {
			return file.Data, true
		}
	}
	return nil, false
}

// productionPickerUploadCode names why an upload id could not be read: no owner is
// installed, or the owner does not know this id for this caller.
func productionPickerUploadCode(err error) string {
	if errors.Is(err, errProductionPickerNoUploads) {
		return productionPickerUploadUnavailCode
	}
	return productionPickerUploadUnknownCode
}

// productionPickerBundleIntact is promptskills' own bundle check
// (internal/promptskills/service.go:101-110) applied to uploaded bytes: version 1,
// a bounded file list, safe distinct paths, one hex digest per file, and every
// digest matching the bytes that actually arrived.
func productionPickerBundleIntact(manifest []byte, files map[string][]byte) bool {
	var decoded struct {
		Version int `json:"version"`
		Files   []struct {
			Path   string `json:"path"`
			SHA256 string `json:"sha256"`
		} `json:"files"`
	}
	if !utf8.Valid(manifest) || json.Unmarshal(manifest, &decoded) != nil {
		return false
	}
	if decoded.Version != 1 || decoded.Files == nil || len(decoded.Files) > promptskills.MaxBundledFiles {
		return false
	}
	seen := map[string]bool{"skill.md": true}
	total := 0
	for _, entry := range decoded.Files {
		lower := strings.ToLower(entry.Path)
		if !productionPickerSafePath(entry.Path) || seen[lower] || !productionPickerDigest.MatchString(entry.SHA256) {
			return false
		}
		seen[lower] = true
		data, found := files[lower]
		if !found {
			return false
		}
		if _, ok := productionPickerSkillText(data); !ok {
			return false
		}
		if productionPickerSHA256(data) != entry.SHA256 {
			return false
		}
		total += len(data)
		if total > promptskills.MaxSkillBytes {
			return false
		}
	}
	return true
}

// productionPickerSkillText applies the text gate promptskills applies to every
// package member: bounded, valid UTF-8, no NUL.
func productionPickerSkillText(data []byte) (string, bool) {
	if len(data) > promptskills.MaxSkillBytes || !utf8.Valid(data) {
		return "", false
	}
	for _, char := range data {
		if char == 0 {
			return "", false
		}
	}
	return string(data), true
}

// productionPickerSafePath bounds one uploaded relative path to the shape
// promptskills accepts: bounded components from its own segment pattern, no
// traversal, no absolute path, depth inside MaxFolderDepth. Nothing outside the
// upload can be named through one.
func productionPickerSafePath(relative string) bool {
	if relative == "" || len(relative) > librarypath.MaxPathBytes {
		return false
	}
	if strings.ContainsAny(relative, "\\\x00") {
		return false
	}
	parts := strings.Split(relative, "/")
	if len(parts) > promptskills.MaxFolderDepth {
		return false
	}
	for _, part := range parts {
		if part == "" || len(part) > 255 || part == "." || part == ".." {
			return false
		}
		if !productionPickerSegment.MatchString(part) {
			return false
		}
	}
	return true
}

// productionPickerHiddenPath reports a member the shipped folder walk would skip
// (package.go:34 skips every dot-prefixed entry at every depth).
func productionPickerHiddenPath(relative string) bool {
	for _, part := range strings.Split(relative, "/") {
		if strings.HasPrefix(part, ".") {
			return true
		}
	}
	return false
}

// productionPickerUploadMember bounds one member name of an uploaded folder. A
// member is a LABEL for bytes that are already staged, never a host path, and
// nothing here resolves it: knowledgeFolder only echoes it back as the identity of
// the upload it verified. So the rule is structural, not library-relative, because
// the browser's folder picker forwards whatever the author's tree had:
//
//   - any valid UTF-8 component is a fine name, including CJK, spaces and a
//     dot-prefixed one; an author with 素材/设定/世界观.md or a stray .obsidian
//     entry must still be able to upload the folder;
//   - no component may address anything outside the upload: empty, "." and ".."
//     are refused, so are an absolute leading separator and every backslash;
//   - bounded: one component at most productionPickerMaxMemberBytes, at most
//     productionPickerMaxUploadDepth components, no control byte, and the whole
//     name inside MaxPathBytes.
//
// productionPickerSafePath is deliberately NOT used here: it is promptskills' own
// ASCII-and-dots package-segment pattern, which exists because a skill package must
// be a package that can install, and it has no business rejecting an author's
// Chinese file name.
func productionPickerUploadMember(relative string) bool {
	if relative == "" || len(relative) > librarypath.MaxPathBytes || !utf8.ValidString(relative) {
		return false
	}
	if strings.HasPrefix(relative, "/") || strings.ContainsAny(relative, "\\\x00") {
		return false
	}
	parts := strings.Split(relative, "/")
	if len(parts) > productionPickerMaxUploadDepth {
		return false
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || len(part) > productionPickerMaxMemberBytes {
			return false
		}
		for _, char := range part {
			if char < 0x20 || char == 0x7f {
				return false
			}
		}
	}
	return true
}

// productionPickerTextName bounds a reference target to the text kinds
// promptskills' reader serves (inspection.go:90 textExtension).
func productionPickerTextName(relative string) bool {
	lower := strings.ToLower(relative)
	for _, suffix := range []string{".md", ".markdown", ".txt"} {
		if strings.HasSuffix(lower, suffix) {
			return true
		}
	}
	return false
}

// productionPickerSafeLabel returns a display label when it is one safe component
// and the empty string otherwise, so a staged name can never become a path or a
// sentence.
func productionPickerSafeLabel(name string) string {
	if productionPickerSafeName(name) && !strings.HasPrefix(name, ".") {
		return name
	}
	return ""
}

// productionPickerDisplayFrom is the display name a verified upload carries: its
// own safe name when the staging owner returned one, else its id.
func productionPickerDisplayFrom(name, id string) string {
	if productionPickerSafeName(name) {
		return name
	}
	if id != "" {
		return id
	}
	return "upload"
}

// productionPickerFallback is the name promptskills derives when a package carries
// no frontmatter name: for a file, the base name minus its extension, except that
// SKILL.md itself yields no name at all so the product default stands
// (package.go:31-60 passes "skill" as "no fallback"); for a folder, the uploaded
// folder's own name. Pick and install both go through it, so a package cannot be
// inspected under one name and installed under another.
func productionPickerFallback(kind, sourceName string) string {
	if kind == productionPickerSkillFolder {
		return productionPickerSafeLabel(sourceName)
	}
	fallback := strings.TrimSuffix(productionPickerBaseName(sourceName), productionPickerExtension(sourceName))
	if strings.EqualFold(fallback, "skill") {
		return ""
	}
	return fallback
}

// productionPickerBaseName is the last component of a relative upload path.
func productionPickerBaseName(relative string) string {
	if index := strings.LastIndexByte(relative, '/'); index >= 0 {
		return relative[index+1:]
	}
	return relative
}

// productionPickerExtension is a name's suffix including the dot, or empty.
func productionPickerExtension(name string) string {
	if index := strings.LastIndexByte(productionPickerBaseName(name), '.'); index >= 0 {
		return productionPickerBaseName(name)[index:]
	}
	return ""
}

// productionPickerInspectionWire is the inspection as the page reads it. It drops
// `content`, exactly as the desktop's withoutContent does
// (electron/controllers/app-data-controller.ts:500-517): the picker ships the
// verdict and the identity, not the body the author has just chosen.
func productionPickerInspectionWire(inspection promptskills.Inspection) map[string]any {
	wire := map[string]any{}
	data, err := json.Marshal(inspection)
	if err != nil {
		return wire
	}
	if json.Unmarshal(data, &wire) != nil {
		return map[string]any{}
	}
	delete(wire, "content")
	return wire
}

// productionPickerSHA256 is the hex digest this product already uses for uploaded
// and installed content (internal/promptskills/inspection.go:60).
func productionPickerSHA256(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// productionPickerToken mints one unguessable opaque id: 256 bits from crypto/rand,
// so another tab cannot guess a candidate or an upload id.
func productionPickerToken() (string, error) {
	var raw [productionPickerCandidateBytes]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}
