//go:build linux

package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"net/http"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/draftalg"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/importsource"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file SERVES the last refused name of the import-run family:
//
//	db:import-run-prepare-inspection   served   productionImportRunPrepareInspectionServe.prepare
//
// # The three gaps the last pass pinned, and where each one closed
//
//  1. THE CLOSURE-LOCAL STORE. The picker used to mint its importsource.Store
//     inside its own wiring closure (production_open_linux.go:904,
//     `selection, selectionErr := newProductionImportSelection(open, uploads)`),
//     so no other binder could resolve a receipt id it had issued. The store is now
//     hoisted above the `for _, bind := range []func() error{` slice
//     (production_open_linux.go:772) and reaches this binder as
//     `importPrepareInspection`; the picker, db:import-run-author-preview and this
//     name hold the same pointer, which is why an id the picker minted resolves
//     here. Two stores would be two receipt tables.
//  2. THE PREVIEW THAT REFUSED THE ID. db:import-run-author-preview answered
//     IMPORT_INSPECTION_OWNER_UNAVAILABLE for the reference's own argument
//     (production_finalized_draft_import_linux.go:224). Its arm now resolves the id
//     through the same store (previewFromInspection, called from
//     productionFinalizedDraftImports.preview), so the authorityFingerprint and
//     manifestFingerprint this request carries can be produced by a served read.
//  3. THE MISSING AUTHOR PREPARE. ImportRunRepository.prepare's author half
//     (electron/repositories/import-run-repository.ts:2005-2138) is now
//     store.ImportRuns.PrepareAuthor (internal/store/importrun_author_prepare.go).
//
// # The reference, channel by channel
//
//	electron/controllers/db-controller.ts:262-355
//	  const inspected = importInspectionStore.peek(id, sender, purpose)
//	  if (purpose === 'author-manuscript') preview(...) compare both fingerprints
//	  const inspection = importInspectionStore.consume(id, sender)
//	  const sourceIdentity = ImportSourceIdentityRepository.resolveEncodedSources(...)
//	  author: ImportRunRepository.prepare({...inspection.chapters})
//	  reference: beginParsing -> commitParsedSource per source -> finalizeParsing
//	src/shared/ipc-channels.ts:926-929   (request 0, path 1, frozen context 2) = 3
//	src/shared/import-run.ts:296-310     ImportRunPrepareFromInspectionRequest, both arms
//	src/shared/import-run.ts:369-375     ImportRunPrepareFromInspectionResult
//	src/components/dialogs/ImportNovelDialog.tsx:377-388  the author call site
//
// The answer is the reference's envelope: `{success:true, preparation}` on a
// created, resumable or exact-duplicate run, and `{success:false,
// errorCode:'AUTHOR_IMPORT_PREVIEW_STALE'}` with HTTP 200 when the authority moved
// between the preview and this call (db-controller.ts:279-283). Every other
// refusal is symbolic, through the family's own mapper
// (production_importrun_write_linux.go:475-495).
//
// # One owner this lane had to add, and why
//
// internal/importsource.Store.Peek returns the summary only - never a chapter body
// (internal/importsource/inspection.go:158-161) - and Consume is the only call that
// returns the frozen chapters, deleting the receipt as it does. The renderer's
// order is preview (one call) then prepare (another) over the SAME id
// (ImportNovelDialog.tsx:120-137, :377-388), so a preview that consumed would spend
// the receipt the prepare needs. internal/importsource may not be edited by this
// change, so this file keeps the frozen chapters it consumed in
// productionImportInspectionHandoff: the same receipt id, the same
// caller+generation+purpose binding the store itself compares, exactly one use at
// prepare, at most two entries and the store's own ten-minute receipt lifetime.
// It holds no authority of its own - it is reached only after the store returned
// those chapters for that binding - and it adds no way to name an inspection the
// store never minted.
//
// # The reference arm, and why it is the same call
//
// purpose 'reference' is served by the same handler: the reference's controller
// peeks the tab-scoped receipt, consumes it, resolves the encoded source identity
// for that purpose and then runs the durable parsing lane instead of the author
// prepare (db-controller.ts:321-355):
//
//	ImportRunRepository.beginParsing(...)          store.ImportRuns.BeginParsing
//	commitParsedSource per source (failParsedSource on a refusal)
//	finalizeParsing                                store.ImportRuns.FinalizeParsing
//
// The three store methods exist in internal/store/importrun_lifecycle.go, so the
// only thing this arm needed was a picker that mints a reference receipt: the
// transport binder now admits both purposes and the provider captures for both
// (internal/transport/import_selection.go:79-84). One receipt, one purpose, one
// binding: the handoff compares the full Binding, so a reference receipt can never
// be spent by the author arm or the other way round.
//
// The failure wording is the one substitution this lane already documents for
// db:import-run-fail: the renderer localizes a symbolic code through an i18n
// catalog this repository does not carry (electron/controllers/import-run-failure.ts),
// so FailParsedSource stores the symbolic message verbatim. No run state is
// misreported and no text is invented.
//
// # The authority fence, and the two refusals it makes identical
//
// The frozen three-key context must be the runtime's current one for this caller,
// the same proof the picker makes (production_import_selection_linux.go:287-296,
// over productionFSGrantProject). A lease that is stale and a project that is not
// this caller's both answer PROJECT_SESSION_STALE, byte for byte: the two are one
// mismatch and telling them apart would be an oracle. The check runs before the
// receipt is touched, so neither refusal spends it.
//
// # Wiring (production_open_linux.go, which this change may not edit)
//
// Above the third bind slice (`for _, bind := range []func() error{`, :772), beside
// the picker's own construction:
//
//	importSelection, importSelectionErr := newProductionImportSelection(open, uploads)
//	importPrepareInspection := newProductionImportRunPrepareInspectionServe(open.durable, importSelection)
//
// The picker's closure keeps using those two values, line :819 becomes
// `newProductionFinalizedDraftImports(open.durable, importPrepareInspection)`, and
// the slice gains one entry:
//
//	func() error { return importPrepareInspection.Bind(server) },
const (
	// productionImportRunPrepareInspectionContextIndex is where the frozen
	// three-key session sits: always its own last argument, never in the payload.
	productionImportRunPrepareInspectionContextIndex = 2
	// productionImportRunPrepareInspectionAuthorPurpose and
	// productionImportRunPrepareInspectionReferencePurpose are the two arms of
	// ImportRunPrepareFromInspectionRequest (src/shared/import-run.ts:296-310).
	productionImportRunPrepareInspectionAuthorPurpose    = "author-manuscript"
	productionImportRunPrepareInspectionReferencePurpose = "reference"
	// productionImportRunPrepareInspectionStaleCode is AUTHOR_IMPORT_PREVIEW_STALE
	// (src/shared/import-run.ts:3), the one answer that is a business envelope.
	productionImportRunPrepareInspectionStaleCode = "AUTHOR_IMPORT_PREVIEW_STALE"
	// productionImportRunPrepareInspectionPurposeCode is the store's own guard for a
	// purpose that is neither of the two.
	productionImportRunPrepareInspectionPurposeCode = "IMPORT_RUN_PURPOSE_INVALID"
	// productionImportRunPrepareInspectionIdentityKeyBytes is the 32-byte key
	// ResolveEncodedSources requires (internal/importsource/identity.go:137-145).
	productionImportRunPrepareInspectionIdentityKeyBytes = 32
	// productionImportRunPrepareInspectionHandoffLimit bounds the receipts whose
	// frozen chapters this process retains: the store keeps at most two live
	// inspections per caller (internal/importsource/StoreOptions.MaxActive), so a
	// third entry can only be a preview the caller has already moved past.
	productionImportRunPrepareInspectionHandoffLimit = 2
	// productionImportRunPrepareInspectionHandoffTTL is the store's own default
	// receipt lifetime (internal/importsource/inspection.go:84), so a handoff entry
	// never outlives the receipt it came from by more than that lifetime.
	productionImportRunPrepareInspectionHandoffTTL = 10 * time.Minute
	// productionImportRunPrepareInspectionIDLimit bounds the receipt id this binder
	// will look up: a minted id is a UUID, a dot and a 64-character digest.
	productionImportRunPrepareInspectionIDLimit = 256
)

// productionImportRunPrepareInspectionServe is the name's adapter. It holds the
// durable bridge (the only object that can name the author database), the picker's
// receipt store, one process-local identity key and the handoff above. It resolves
// nothing at construction time.
type productionImportRunPrepareInspectionServe struct {
	durable     *productionDurableOpen
	inspections *importsource.Store
	identityKey []byte
	handoff     *productionImportInspectionHandoff
}

// newProductionImportRunPrepareInspectionServe wraps the lane's durable bridge and
// the picker's inspection store. The identity key is minted per process: it is the
// HMAC key ResolveEncodedSources uses for the fingerprints it derives, and the
// picker's own receipt key is unexported in internal/importsource, so a second key
// is required rather than chosen. It never leaves the process and is never stored.
// A nil selection is accepted so a lane whose staging could not be built still
// starts; Bind then installs nothing and the name keeps failing closed.
func newProductionImportRunPrepareInspectionServe(durable *productionDurableOpen,
	selection *productionImportSelection) *productionImportRunPrepareInspectionServe {
	adapter := &productionImportRunPrepareInspectionServe{durable: durable,
		handoff: newProductionImportInspectionHandoff()}
	if selection != nil {
		adapter.inspections = selection.store
	}
	key := make([]byte, productionImportRunPrepareInspectionIdentityKeyBytes)
	if _, err := rand.Read(key); err == nil {
		adapter.identityKey = key
	}
	return adapter
}

// Bind installs the one name. A nil adapter, a nil bridge, a missing receipt store
// or a missing identity key installs nothing, so the name keeps answering the
// transport's 501 GO_OPERATION_NOT_MIGRATED instead of being published as a handler
// that could only refuse; a nil server is refused because that is a wiring bug, not
// a closed lane. Bind itself refuses an uninventoried name (OPERATION_NOT_EXPOSED)
// and an already bound one (OPERATION_ALREADY_BOUND), so a name a sibling bound
// first is a startup error rather than dead code.
func (r *productionImportRunPrepareInspectionServe) Bind(server *transport.Server) error {
	if r == nil || r.durable == nil || r.inspections == nil ||
		len(r.identityKey) != productionImportRunPrepareInspectionIdentityKeyBytes {
		return nil
	}
	if server == nil {
		return errors.New("TRANSPORT_SERVER_REQUIRED")
	}
	return server.Bind(productionImportRunPrepareInspectionOperation, r.prepare)
}

// prepare is db:import-run-prepare-inspection. The order is the reference's: the
// path assertion, the authority fence, the receipt (peek + consume, or the ordered
// replay of a preview), the source identity in its own transaction, then the store
// write the request's purpose selects - the author prepare, or the parsing lane
// beginParsing -> commitParsedSource per source -> finalizeParsing.
func (r *productionImportRunPrepareInspectionServe) prepare(ctx context.Context,
	caller transport.Caller, args []any) (any, error) {
	expected, err := productionImportRunPrepareInspectionAdmit(args)
	if err != nil {
		return nil, err
	}
	request, err := productionImportRunPrepareInspectionRequest(args[0])
	if err != nil {
		return nil, err
	}
	if err := r.assertCurrentProject(ctx, caller, expected); err != nil {
		return nil, productionImportRunWriteFailure(err)
	}
	purpose := importsource.Purpose(request.purpose)
	inspection, err := r.takeInspection(ctx, caller, expected, request.inspectionID, purpose)
	if err != nil {
		return nil, productionImportRunWriteFailure(err)
	}
	db, release, err := r.durable.borrowAuthorDatabase(ctx, caller, expected)
	if release != nil {
		defer release()
	}
	if err != nil {
		return nil, productionImportRunWriteFailure(err)
	}
	if db == nil {
		return nil, productionImportRunWriteFailure(errors.New("PRODUCTION_CLOSED"))
	}
	identity, err := r.resolveSources(ctx, db, inspection.Sources, purpose)
	if err != nil {
		return nil, productionImportRunWriteFailure(err)
	}
	if purpose == importsource.Reference {
		return r.prepareReferenceParsing(ctx, db, request, identity, inspection)
	}
	preparation, err := store.NewImportRuns(db).PrepareAuthor(ctx, store.ImportRunPrepareAuthorRequest{
		RunID:                       request.runID,
		SourceFingerprint:           identity.SourceFingerprint,
		SourceIDs:                   identity.SourceIDs,
		SourceFingerprints:          identity.SourceFingerprints,
		SourceDisplay:               productionImportRunPrepareInspectionDisplay(inspection.Sources),
		Locale:                      request.locale,
		AuthorityFingerprint:        request.authorityFingerprint,
		ExpectedManifestFingerprint: request.manifestFingerprint,
		AuthorInference:             request.authorInference,
		Chapters:                    productionImportRunPrepareInspectionChapters(inspection.Chapters),
	})
	if err != nil {
		if errors.Is(err, store.ErrImportRunAuthorPreviewStale) {
			// The authority moved after the receipt was frozen: the reference answers
			// a business envelope, not a refusal (db-controller.ts:279-283).
			return map[string]any{"success": false,
				"errorCode": productionImportRunPrepareInspectionStaleCode}, nil
		}
		return nil, productionImportRunWriteFailure(err)
	}
	return map[string]any{"success": true, "preparation": preparation}, nil
}

// prepareReferenceParsing is the reference arm's durable parse
// (electron/controllers/db-controller.ts:321-355): beginParsing opens (or re-enters)
// the parsing run, every source the inspection froze is committed under that run's
// own id - the id beginParsing answers, which is not always the requested one when
// it re-enters an existing run - and finalizeParsing turns the fully parsed run into
// the prepared one whose preparation the renderer reads.
//
// A source whose chapters cannot be committed is marked failed with the symbolic
// reason and the commit's own refusal is rethrown, in that order, exactly like the
// controller's catch block. Only the failure's wording is substituted (there is no
// i18n catalog here); the code the renderer sees is the store's own.
func (r *productionImportRunPrepareInspectionServe) prepareReferenceParsing(ctx context.Context,
	db *sql.DB, request productionImportRunPrepareInspectionRequestValue,
	identity importsource.ResolvedIdentity, inspection importsource.Inspection) (any, error) {
	runs := store.NewImportRuns(db)
	run, err := runs.BeginParsing(ctx, store.ImportRunBeginParsingRequest{
		RunID:                       request.runID,
		Purpose:                     request.purpose,
		SourceFingerprint:           identity.SourceFingerprint,
		SourceIDs:                   identity.SourceIDs,
		SourceFingerprints:          identity.SourceFingerprints,
		LegacySourceFingerprints:    identity.LegacySourceFingerprints,
		LegacyCollectionFingerprint: identity.LegacyCollectionFingerprint,
		SourceDisplay:               productionImportRunPrepareInspectionDisplay(inspection.Sources),
		Locale:                      request.locale,
	})
	if err != nil {
		return nil, productionImportRunWriteFailure(err)
	}
	parsingRunID := productionImportRunPrepareInspectionString(run, "id")
	if parsingRunID == "" {
		return nil, productionImportRunWriteFailure(errors.New("IMPORT_RUN_STATE_CONFLICT"))
	}
	for index, sourceID := range identity.SourceIDs {
		chapters := productionImportRunPrepareInspectionParsedChapters(inspection.Chapters, int64(index))
		if _, err := runs.CommitParsedSource(ctx, parsingRunID, sourceID, chapters); err != nil {
			if _, failErr := runs.FailParsedSource(ctx, parsingRunID, sourceID, err.Error()); failErr != nil {
				return nil, productionImportRunWriteFailure(failErr)
			}
			return nil, productionImportRunWriteFailure(err)
		}
	}
	preparation, err := runs.FinalizeParsing(ctx, parsingRunID)
	if err != nil {
		return nil, productionImportRunWriteFailure(err)
	}
	return map[string]any{"success": true, "preparation": preparation}, nil
}

// previewFromInspection is db:import-run-author-preview's id form: the reference's
// own argument (db-controller.ts:367-377). It resolves the receipt through the
// picker's store, borrows the same author database every read uses, and answers the
// store's FinalizedImportPreview - the exact call the chapters form of the same
// name makes, so the two forms cannot disagree.
func (r *productionImportRunPrepareInspectionServe) previewFromInspection(ctx context.Context,
	caller transport.Caller, expected map[string]any, inspectionID string) (any, error) {
	if r == nil || r.durable == nil {
		return nil, productionFinalizedDraftImportFailure(errors.New("PRODUCTION_CLOSED"))
	}
	inspection, err := r.obtainInspection(ctx, caller, expected, inspectionID)
	if err != nil {
		return nil, productionFinalizedDraftImportFailure(err)
	}
	db, release, err := r.durable.borrowAuthorDatabase(ctx, caller, expected)
	if release != nil {
		defer release()
	}
	if err != nil {
		return nil, productionFinalizedDraftImportFailure(err)
	}
	if db == nil {
		return nil, productionFinalizedDraftImportFailure(errors.New("PRODUCTION_CLOSED"))
	}
	preview, err := productionFinalizedDraftImportPreview(ctx, db,
		r.inspectionPreviewChapters(inspection))
	if err != nil {
		return nil, productionFinalizedDraftImportFailure(err)
	}
	return preview, nil
}

// assertCurrentProject proves the frozen three-key context is the runtime's current
// one for this caller, before anything reads a receipt. It is the picker's own
// proof (production_import_selection_linux.go:287-296) so the two names cannot
// disagree about which lease an id belongs to, and it is what makes a stale lease
// and a foreign project refuse identically.
func (r *productionImportRunPrepareInspectionServe) assertCurrentProject(ctx context.Context,
	caller transport.Caller, expected map[string]any) error {
	if r == nil || r.durable == nil || ctx == nil {
		return errors.New("PRODUCTION_CLOSED")
	}
	owner, err := productionOwner(caller)
	if err != nil {
		return errors.New("PROJECT_SESSION_REQUIRED")
	}
	current, present, err := r.durable.productionFSGrantProject(ctx, owner)
	if err != nil {
		return err
	}
	if !present {
		return errors.New("PROJECT_SESSION_REQUIRED")
	}
	if current.ProjectID != productionImportRunPrepareInspectionString(expected, "projectId") ||
		current.LeaseID != productionImportRunPrepareInspectionString(expected, "leaseId") ||
		current.ProjectPath != productionImportRunPrepareInspectionString(expected, "projectPath") {
		// One code for both halves of the mismatch, so a caller cannot tell a
		// foreign path from a lease this process no longer holds.
		return errors.New("PROJECT_SESSION_STALE")
	}
	return nil
}

// takeInspection is the receipt one-use path: a previewed id is taken from the
// handoff, an id that was never previewed is peeked and consumed here. Both return
// the same frozen inspection, and both answer IMPORT_INSPECTION_EXPIRED when the
// id is unknown, expired, consumed or names another caller's generation. The
// purpose is part of the binding, so a receipt minted for one arm can never be
// spent by the other.
func (r *productionImportRunPrepareInspectionServe) takeInspection(ctx context.Context,
	caller transport.Caller, expected map[string]any, inspectionID string,
	purpose importsource.Purpose) (importsource.Inspection, error) {
	if r == nil || r.inspections == nil {
		return importsource.Inspection{}, errors.New("IMPORT_INSPECTION_OWNER_UNAVAILABLE")
	}
	if ctx == nil || !productionImportRunPrepareInspectionIDValid(inspectionID) {
		return importsource.Inspection{}, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
	}
	binding, err := productionImportRunPrepareInspectionBinding(caller, expected, purpose)
	if err != nil {
		return importsource.Inspection{}, err
	}
	if inspection, present := r.handoff.take(inspectionID, binding); present {
		return inspection, nil
	}
	summary, err := r.inspections.Peek(inspectionID, binding)
	if err != nil {
		return importsource.Inspection{}, err
	}
	return r.inspections.Consume(inspectionID, summary.PreviewCAS, binding)
}

// obtainInspection is the preview's read: the same receipt, resolved once, retained
// for the prepare that follows. A repeated preview of the same id answers the same
// frozen chapters instead of refusing a receipt its own first call spent.
func (r *productionImportRunPrepareInspectionServe) obtainInspection(ctx context.Context,
	caller transport.Caller, expected map[string]any, inspectionID string) (importsource.Inspection, error) {
	if r == nil || r.inspections == nil {
		return importsource.Inspection{}, security.NewHTTPError(http.StatusNotImplemented,
			productionFinalizedDraftImportInspectionCode)
	}
	if ctx == nil || !productionImportRunPrepareInspectionIDValid(inspectionID) {
		return importsource.Inspection{}, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
	}
	// The preview's id form is the author-manuscript arm only
	// (db-controller.ts:367-377 hardcodes that purpose at the store peek), so its
	// binding is the author one.
	binding, err := productionImportRunPrepareInspectionBinding(caller, expected,
		importsource.AuthorManuscript)
	if err != nil {
		return importsource.Inspection{}, err
	}
	if inspection, present := r.handoff.peek(inspectionID, binding); present {
		return inspection, nil
	}
	summary, err := r.inspections.Peek(inspectionID, binding)
	if err != nil {
		return importsource.Inspection{}, err
	}
	inspection, err := r.inspections.Consume(inspectionID, summary.PreviewCAS, binding)
	if err != nil {
		return importsource.Inspection{}, err
	}
	r.handoff.put(inspectionID, binding, inspection)
	return inspection, nil
}

// resolveSources is ImportSourceIdentityRepository.resolveEncodedSources
// (electron/repositories/import-source-identity-repository.ts:104-...): the alias
// table's stable ids, one fingerprint per source and the collection fingerprint,
// inside its own transaction - the reference owns a separate transaction here too,
// and the prepare that follows re-reads what this one committed. The purpose is the
// request's: it is part of every digest the table derives, so an author receipt and
// a reference receipt cannot resolve to the same collection fingerprint.
func (r *productionImportRunPrepareInspectionServe) resolveSources(ctx context.Context,
	db *sql.DB, sources []importsource.Source, purpose importsource.Purpose) (importsource.ResolvedIdentity, error) {
	if len(r.identityKey) != productionImportRunPrepareInspectionIdentityKeyBytes {
		return importsource.ResolvedIdentity{}, errors.New("IMPORT_SOURCE_SECRET_INVALID")
	}
	encoded := make([]importsource.EncodedIdentity, 0, len(sources))
	for _, source := range sources {
		encoded = append(encoded, source.EncodedIdentity)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return importsource.ResolvedIdentity{}, err
	}
	defer func() { _ = tx.Rollback() }()
	resolved, err := importsource.ResolveEncodedSources(ctx, tx, encoded, purpose, r.identityKey)
	if err != nil {
		return importsource.ResolvedIdentity{}, err
	}
	if err := tx.Commit(); err != nil {
		return importsource.ResolvedIdentity{}, err
	}
	return resolved, nil
}

// inspectionPreviewChapters is the chapter array db:import-run-author-preview hands
// the store: the frozen bodies with the count the manifest is validated against, so
// the preview a caller sees and the manifest a prepare re-derives cannot disagree
// about a word.
func (r *productionImportRunPrepareInspectionServe) inspectionPreviewChapters(
	inspection importsource.Inspection) []store.FinalizedDraftImportChapter {
	chapters := make([]store.FinalizedDraftImportChapter, 0, len(inspection.Chapters))
	for _, chapter := range inspection.Chapters {
		count := int64(draftalg.CountDraftUnits(chapter.Content))
		chapters = append(chapters, store.FinalizedDraftImportChapter{ChapterNumber: int64(chapter.Number),
			Title: chapter.Title, Content: chapter.Content, WordCount: &count})
	}
	return chapters
}

// productionImportInspectionHandoff retains the frozen chapters of inspections whose
// receipt the preview already spent. It is bounded in entries and in time, keyed by
// the receipt id, and compared with the full receipt binding before it answers, so
// it can never hand a caller chapters the store would not have. It holds no key and
// writes nothing.
type productionImportInspectionHandoff struct {
	mu      sync.Mutex
	entries []productionImportInspectionHandoffEntry
	now     func() time.Time
}

type productionImportInspectionHandoffEntry struct {
	id         string
	binding    importsource.Binding
	inspection importsource.Inspection
	expires    int64
}

// newProductionImportInspectionHandoff returns an empty handoff with the wall clock
// the receipt store itself uses.
func newProductionImportInspectionHandoff() *productionImportInspectionHandoff {
	return &productionImportInspectionHandoff{now: time.Now}
}

// put retains one inspection, replacing any earlier entry for the same id, dropping
// everything expired first and evicting the oldest entry over the limit.
func (h *productionImportInspectionHandoff) put(id string, binding importsource.Binding,
	inspection importsource.Inspection) {
	if h == nil || h.now == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.pruneLocked()
	kept := make([]productionImportInspectionHandoffEntry, 0, len(h.entries)+1)
	for _, entry := range h.entries {
		if entry.id != id {
			kept = append(kept, entry)
		}
	}
	kept = append(kept, productionImportInspectionHandoffEntry{id: id, binding: binding,
		inspection: inspection, expires: h.now().Add(productionImportRunPrepareInspectionHandoffTTL).UnixMilli()})
	for len(kept) > productionImportRunPrepareInspectionHandoffLimit {
		kept = kept[1:]
	}
	h.entries = kept
}

// peek answers the same inspection without spending it, so a preview the renderer
// runs twice (its effect depends on state it may set again) keeps working.
func (h *productionImportInspectionHandoff) peek(id string, binding importsource.Binding) (importsource.Inspection, bool) {
	if h == nil || h.now == nil {
		return importsource.Inspection{}, false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.pruneLocked()
	for _, entry := range h.entries {
		if entry.id == id && entry.binding == binding {
			return entry.inspection, true
		}
	}
	return importsource.Inspection{}, false
}

// take is the prepare's one-use read: the entry is removed as it is returned, so a
// replay of the same id cannot create a second run from the same preview.
func (h *productionImportInspectionHandoff) take(id string, binding importsource.Binding) (importsource.Inspection, bool) {
	if h == nil || h.now == nil {
		return importsource.Inspection{}, false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.pruneLocked()
	for index, entry := range h.entries {
		if entry.id != id || entry.binding != binding {
			continue
		}
		remaining := make([]productionImportInspectionHandoffEntry, 0, len(h.entries)-1)
		remaining = append(remaining, h.entries[:index]...)
		remaining = append(remaining, h.entries[index+1:]...)
		h.entries = remaining
		return entry.inspection, true
	}
	return importsource.Inspection{}, false
}

// pruneLocked drops every entry whose lifetime is over. The caller holds the mutex.
func (h *productionImportInspectionHandoff) pruneLocked() {
	now := h.now().UnixMilli()
	retained := make([]productionImportInspectionHandoffEntry, 0, len(h.entries))
	for _, entry := range h.entries {
		if entry.expires > now {
			retained = append(retained, entry)
		}
	}
	h.entries = retained
}

// productionImportRunPrepareInspectionRequest is ImportRunPrepareFromInspectionRequest
// (src/shared/import-run.ts:296-310) bound from JSON. Both arms carry the inspection
// id, the run id, the purpose and the locale; the author arm adds the two
// fingerprints and the authorInference flag. An absent author field stays empty
// (false), which the store refuses with its own code.
type productionImportRunPrepareInspectionRequestValue struct {
	inspectionID         string
	runID                string
	purpose              string
	locale               string
	authorityFingerprint string
	manifestFingerprint  string
	authorInference      bool
}

// productionImportRunPrepareInspectionAdmit is the arity, the frozen three-key
// session and the expected path, in that order: a stale lease or a path in the
// wrong position is a malformed request, never authority. The two codes are the
// controller's and the path assertion's own (electron/utils/project-context.ts:12-25).
func productionImportRunPrepareInspectionAdmit(args []any) (map[string]any, error) {
	if len(args) != productionImportRunPrepareInspectionArity {
		return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
	}
	expected, err := productionImportRunWriteExpectedContext(args, productionImportRunPrepareInspectionContextIndex)
	if err != nil {
		return nil, err
	}
	path, ok := args[productionImportRunPrepareInspectionPathIndex].(string)
	if !ok || path == "" || !utf8.ValidString(path) {
		return nil, security.NewHTTPError(http.StatusBadRequest, "PROJECT_CONTEXT_REQUIRED")
	}
	if path != expected["projectPath"] {
		return nil, security.NewHTTPError(http.StatusConflict, "PROJECT_CONTEXT_CHANGED")
	}
	return expected, nil
}

// productionImportRunPrepareInspectionRequest reads the payload. The shape checks
// are the ones a Go decode owes and JavaScript's own operators made: the
// identifiers are strings, the purpose is one of the two arms, the locale is one of
// the two the renderer speaks, and an author request that cannot possibly match a
// preview is refused BEFORE a receipt is spent - the only place this binder is
// stricter than the controller, which let the store refuse after its consume.
func productionImportRunPrepareInspectionRequest(value any) (productionImportRunPrepareInspectionRequestValue, error) {
	object, ok := value.(map[string]any)
	if !ok || object == nil {
		return productionImportRunPrepareInspectionRequestValue{},
			security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
	}
	request := productionImportRunPrepareInspectionRequestValue{}
	for key, target := range map[string]*string{
		"inspectionId": &request.inspectionID, "runId": &request.runID,
		"purpose": &request.purpose, "locale": &request.locale,
		"authorityFingerprint": &request.authorityFingerprint,
		"manifestFingerprint":  &request.manifestFingerprint,
	} {
		raw, present := object[key]
		if !present {
			continue
		}
		text, ok := raw.(string)
		if !ok || !utf8.ValidString(text) {
			return productionImportRunPrepareInspectionRequestValue{},
				security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
		}
		*target = text
	}
	if !productionImportRunPrepareInspectionIDValid(request.inspectionID) || request.runID == "" {
		return productionImportRunPrepareInspectionRequestValue{},
			security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
	}
	switch request.purpose {
	case productionImportRunPrepareInspectionAuthorPurpose, productionImportRunPrepareInspectionReferencePurpose:
	default:
		return productionImportRunPrepareInspectionRequestValue{},
			security.NewHTTPError(http.StatusBadRequest, productionImportRunPrepareInspectionPurposeCode)
	}
	if request.locale != "zh-CN" && request.locale != "en-US" {
		return productionImportRunPrepareInspectionRequestValue{},
			security.NewHTTPError(http.StatusBadRequest, "IMPORT_RUN_LOCALE_INVALID")
	}
	if request.purpose == productionImportRunPrepareInspectionAuthorPurpose {
		if !productionImportRunPrepareInspectionDigest(request.authorityFingerprint) ||
			!productionImportRunPrepareInspectionDigest(request.manifestFingerprint) {
			return productionImportRunPrepareInspectionRequestValue{},
				security.NewHTTPError(http.StatusBadRequest, "IMPORT_RUN_AUTHOR_PREVIEW_REQUIRED")
		}
		if raw, present := object["authorInference"]; present {
			// `request.authorInference === true` at db-controller.ts:290: anything
			// but a JSON true freezes no inference.
			request.authorInference, _ = raw.(bool)
		}
	}
	return request, nil
}

// productionImportRunPrepareInspectionBinding is the receipt binding the picker
// minted (internal/transport/import_selection.go:104-105): the cookie session and
// the browser tab, the project generation the frozen context names, and the purpose
// the receipt was frozen for. It is derived, never read from the payload.
func productionImportRunPrepareInspectionBinding(caller transport.Caller,
	expected map[string]any, purpose importsource.Purpose) (importsource.Binding, error) {
	projectID := productionImportRunPrepareInspectionString(expected, "projectId")
	leaseID := productionImportRunPrepareInspectionString(expected, "leaseId")
	if caller.SessionID == "" || caller.ClientID == "" || projectID == "" || leaseID == "" {
		return importsource.Binding{}, security.NewHTTPError(http.StatusBadRequest, "PROJECT_SESSION_REQUIRED")
	}
	if purpose != importsource.AuthorManuscript && purpose != importsource.Reference {
		return importsource.Binding{}, security.NewHTTPError(http.StatusBadRequest,
			productionImportRunPrepareInspectionPurposeCode)
	}
	return importsource.Binding{Caller: caller.SessionID + ":" + caller.ClientID,
		Generation: projectID + ":" + leaseID, Purpose: purpose}, nil
}

// productionImportRunPrepareInspectionDisplay is the display list the run freezes:
// the picker's own names, media types and sizes, never a path.
func productionImportRunPrepareInspectionDisplay(
	sources []importsource.Source) []store.ImportRunSourceDisplay {
	display := make([]store.ImportRunSourceDisplay, 0, len(sources))
	for _, source := range sources {
		display = append(display, store.ImportRunSourceDisplay{DisplayName: source.DisplayName,
			MediaType: source.MediaType, Size: source.Size})
	}
	return display
}

// productionImportRunPrepareInspectionChapters is the chapter array the store
// normalizes: one input per frozen chapter, with the parser's own affiliations.
func productionImportRunPrepareInspectionChapters(
	chapters []importsource.Chapter) []store.ImportRunChapterInput {
	inputs := make([]store.ImportRunChapterInput, 0, len(chapters))
	for _, chapter := range chapters {
		inputs = append(inputs, store.ImportRunChapterInput{Number: int64(chapter.Number),
			SourceIndex: int64(chapter.SourceIndex), SourceChapterNumber: int64(chapter.SourceChapterNumber),
			Title: chapter.Title, Content: chapter.Content,
			ContentFingerprint: chapter.ContentFingerprint, ContentSize: int64(chapter.ContentSize)})
	}
	return inputs
}

// productionImportRunPrepareInspectionParsedChapters is the chapter array one
// source of the reference arm commits: the frozen chapters whose sourceIndex names
// that source, with the parser's own local chapter number (the reference's
// db-controller.ts:334-343 maps number = sourceChapterNumber and leaves the
// affiliation to the per-source call, so SourceIndex is zero - the store's single
// source id makes it the only legal value).
func productionImportRunPrepareInspectionParsedChapters(
	chapters []importsource.Chapter, sourceIndex int64) []store.ImportRunChapterInput {
	inputs := make([]store.ImportRunChapterInput, 0, len(chapters))
	for _, chapter := range chapters {
		if int64(chapter.SourceIndex) != sourceIndex {
			continue
		}
		inputs = append(inputs, store.ImportRunChapterInput{Number: int64(chapter.SourceChapterNumber),
			SourceIndex: 0, SourceChapterNumber: int64(chapter.SourceChapterNumber),
			Title: chapter.Title, Content: chapter.Content,
			ContentFingerprint: chapter.ContentFingerprint, ContentSize: int64(chapter.ContentSize)})
	}
	return inputs
}

// productionImportRunPrepareInspectionString reads one string out of the frozen
// context, which the admit step already proved present.
func productionImportRunPrepareInspectionString(values map[string]any, key string) string {
	text, _ := values[key].(string)
	return text
}

// productionImportRunPrepareInspectionIDValid reports whether a value can be a
// minted receipt id: the store mints a UUID, a dot and a 64-character digest
// (internal/importsource/inspection.go:88-91).
func productionImportRunPrepareInspectionIDValid(id string) bool {
	return id != "" && len(id) <= productionImportRunPrepareInspectionIDLimit &&
		utf8.ValidString(id) && !productionImportRunPrepareInspectionHasNUL(id)
}

// productionImportRunPrepareInspectionHasNUL keeps the NUL check in one place, so
// no id with a terminator can enter a binding comparison.
func productionImportRunPrepareInspectionHasNUL(value string) bool {
	for index := 0; index < len(value); index++ {
		if value[index] == 0 {
			return true
		}
	}
	return false
}

// productionImportRunPrepareInspectionDigest is the store's own digest shape
// (importRunSHA256), repeated here because a request the binder can refuse before a
// receipt is spent should be refused by the binder.
func productionImportRunPrepareInspectionDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for index := 0; index < len(value); index++ {
		character := value[index]
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}
