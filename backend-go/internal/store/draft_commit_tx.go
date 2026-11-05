package store

// generation:commit-draft's ONE transaction.
//
// The reference is GenerationDraftEffects.commit (electron/services/
// generation-draft-effects.ts:154-203) reached through MainGenerationOwner.
// commitDraft (electron/services/main-generation-owner.ts:1539-1545) and the
// chapter-draft controller row (electron/controllers/generation-controller.ts:
// 271-274). Everything the effect needs is derived from rows this database
// already holds; the request carries no text (GenerationDraftCommitRequest,
// src/shared/generation-owner-contract.ts:131-137), so the committed body is
// recomposed here from the run's own artifacts through the pure ports in
// internal/generation (ComposeDraftVisibleSources / CountDraftUnits /
// DraftTargetUnitRange), which are the ports of src/shared/draft-visible-text.ts
// and src/shared/draft-units.ts.
//
// # Why this lives in the store and not in a lane helper
//
// The write is one IMMEDIATE transaction over contents, drafts and
// generation_attempts, and the next-chapter check it makes has to read the batch
// rows it is about to extend INSIDE that transaction - otherwise two devices can
// commit the same chapter (:196-201 reads the batch, writes, and reads it again
// before the transaction closes). store.Drafts.Create (draft_author.go:59) opens
// its own deferred transaction, so it cannot be joined to the receipt stamp;
// draftCreateInTx below is that variant, executing the original's three
// statements on the connection that already holds the write lock.
//
// # Statement order, because a refusal code depends on it
//
//	BEGIN IMMEDIATE
//	SELECT run_id,root_action_id,binding_json,status           -- the run
//	SELECT attempt_id,run_id,usage_receipt_json ... ORDER BY rowid
//	  -> verifiedCommit per row (compositionFrom + the receipt shape)
//	  -> >1 verified commit is RECEIPT_INVALID, no body match is RECEIPT_INVALID
//	target check (source, chapterNumber, operation, batchId)
//	duplicate replay (no row written) or COMMIT_CONFLICT
//	assertSources: live epoch, cancelled root
//	compositionFrom -> COMPOSITION_REQUIRED / INTEGRITY_FAILED / ...
//	last artifact's finishReason -> INCOMPLETE
//	authorInputs draft:target-units -> INCOMPLETE
//	countDraftUnits(composed) against the target range -> INCOMPLETE
//	readBatch (first) -> BATCH_PROGRESS_CONFLICT
//	sourceRefs -> source_dependencies, ValidateDraftDependencies
//	INSERT contents, INSERT drafts (MAX(version)+1), SELECT version
//	UPDATE generation_attempts SET usage_receipt_json = {...,draftCommit}
//	readBatch (second)
//	COMMIT
//
// # Documented reductions
//
//   - A plot-outline run composes under PLOT_OUTLINE_PROTOCOL (a sibling slice).
//     Its stored composition is refused with GENERATION_COMPOSITION_ALGORITHM_INVALID
//     rather than composed here.
//   - assertSources' final step is deps.rebuildBinding() plus a deep comparison of
//     the result with the frozen binding (GENERATION_SOURCE_CHANGED,
//     main-generation-owner.ts:741-743). rebuildBinding has no Go port in this
//     package, so a chapter-draft whose sources drifted after begin is not
//     re-detected here; the epoch and cancelled-root halves of assertSources are.
//   - requireRun(execution) also calls assertBlueprintRecoveryWritable and
//     assertPlotOutlineWritable, which are no-ops for an ordinary chapter-draft
//     run and have their own readers in this lane.
//   - draftEffects.commit is wrapped by agents.withChildEffect, which reduces to
//     `if (!id) return effect()` for a run whose manifest carries no
//     agentWorkflowRegistrationId (agent-generation.ts:344-349). The lane's own
//     begin cannot mint that key, and this store refuses such a run instead of
//     projecting an agent workflow it does not own.
//   - A receipt whose id/version are not safe integers cannot name a draft row and
//     is refused as RECEIPT_INVALID; every receipt this path writes is integral.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/generation"
)

// The refusal codes of this write, each one the literal the reference throws.
var (
	ErrDraftCommitTargetInvalid       = errors.New("GENERATION_DRAFT_TARGET_INVALID")
	ErrDraftCommitConflict            = errors.New("GENERATION_DRAFT_COMMIT_CONFLICT")
	ErrDraftCommitCompositionRequired = errors.New("GENERATION_DRAFT_COMPOSITION_REQUIRED")
	ErrDraftCommitIncomplete          = errors.New("GENERATION_DRAFT_INCOMPLETE")
	ErrDraftCommitReceiptInvalid      = errors.New("GENERATION_DRAFT_RECEIPT_INVALID")
	ErrDraftCommitEpochStale          = errors.New("GENERATION_EPOCH_STALE")
	ErrDraftCommitActionCancelled     = errors.New("GENERATION_ACTION_CANCELLED")
	ErrDraftCommitBatchConflict       = errors.New("GENERATION_BATCH_PROGRESS_CONFLICT")
)

// DraftCommitRequest is GenerationDraftCommitRequest
// (src/shared/generation-owner-contract.ts:131-137). It carries no content: the
// body is recomposed from the run's artifacts, and ExpectedCompositionHash is the
// hash the renderer computed over the text it believes it composed, so the commit
// can refuse a stale or foreign composition without ever trusting text off the
// wire.
//
// LiveEpoch is deps.epoch, the lease the calling session holds right now. The
// reference compares it with the run's frozen epoch inside assertSources
// (main-generation-owner.ts:159), which runs AFTER the duplicate replay, so a
// duplicate commit still answers while the run's epoch is stale.
type DraftCommitRequest struct {
	Handle                  BatchRunHandle
	ExpectedCompositionHash string
	ChapterNumber           int64
	Source                  string
	BatchID                 string
	LiveEpoch               string
}

// DraftCommitReceipt is the receipt the effects layer answers with
// (generation-draft-effects.ts:130-136, 189-194): the draft row it created or the
// one it replays, never a path or a ledger detail.
type DraftCommitReceipt struct {
	Success     bool   `json:"success"`
	ID          int64  `json:"id"`
	Version     int64  `json:"version"`
	ContentHash string `json:"contentHash"`
	Content     string `json:"content"`
}

// CommitGenerationDraft is GenerationDraftEffects.commit: one IMMEDIATE
// transaction that refuses or writes, and writes nothing at all when it refuses.
// A duplicate commit replays the receipt the first one stamped and leaves the
// database byte-identical.
func CommitGenerationDraft(ctx context.Context, db *sql.DB, request DraftCommitRequest) (receipt *DraftCommitReceipt, retErr error) {
	if db == nil {
		return nil, ErrProjectNotOpen
	}
	if ctx == nil || request.Handle.RunID == "" {
		return nil, glError("GENERATION_RUN_NOT_FOUND")
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return nil, err
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		// A cancelled request must not return a connection that still holds a
		// manual transaction to the pool (drafts.go:241-252 keeps the same rule).
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := conn.ExecContext(cleanup, "ROLLBACK"); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("rollback draft commit: %w", err))
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
	}()
	value, err := dcCommitBody(ctx, conn, request)
	if err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, err
	}
	committed = true
	return value, nil
}

// dcCommitBody is the transaction body. Every read and write below shares the
// reserved connection the caller opened, so no other writer can interleave
// between the duplicate check, the batch check and the two INSERTs.
func dcCommitBody(ctx context.Context, conn *sql.Conn, request DraftCommitRequest) (*DraftCommitReceipt, error) {
	run, err := bpLoadRun(ctx, conn, request.Handle.RunID)
	if err != nil {
		return nil, err
	}
	manifest := run.manifest()
	previous, err := dcReadCommit(ctx, conn, run)
	if err != nil {
		return nil, err
	}
	// :157-160 - the target has to be this run's own chapter, written as this
	// transaction's source, under the batch the run was frozen for. A manifest
	// without a batchId reads as "" here, which is the original's `undefined`.
	chapterNumber, chapterOK := rrInteger(manifest["chapterNumber"], -9007199254740991)
	if request.Source != "write" || !chapterOK || request.ChapterNumber != chapterNumber ||
		glText(manifest["operation"]) != "chapter-draft" ||
		request.BatchID != glText(manifest["batchId"]) {
		return nil, ErrDraftCommitTargetInvalid
	}
	if previous != nil {
		// :162-165 - a saved chapter is re-answered, not re-written.
		if previous.ContentHash != request.ExpectedCompositionHash {
			return nil, ErrDraftCommitConflict
		}
		return previous, nil
	}
	if err := dcAssertSources(ctx, conn, run, request.LiveEpoch); err != nil {
		return nil, err
	}
	composed, err := dcComposition(ctx, conn, run)
	if err != nil {
		return nil, err
	}
	// :167-170 - the committed text is the composition the renderer hashed. A
	// missing composition, a non-draft algorithm or another hash is the same
	// refusal: nothing was composed for THIS text.
	if composed == nil || !dcDraftAlgorithm(composed.Algorithm) || composed.TextHash != request.ExpectedCompositionHash {
		return nil, ErrDraftCommitCompositionRequired
	}
	// :171 - the last artifact of the composition must have finished, not stopped
	// on a length cap.
	if glText(composed.lastResult["finishReason"]) != "stop" {
		return nil, ErrDraftCommitIncomplete
	}
	// :172-175 - the run's own target units, and the composed text against the
	// bottom of that range.
	target, err := dcTargetUnits(manifest)
	if err != nil {
		return nil, err
	}
	units := generation.CountDraftUnits(composed.Text)
	targetRange, rangeErr := generation.DraftTargetUnitRange(target)
	if rangeErr != nil || int64(units) < targetRange.Minimum {
		return nil, ErrDraftCommitIncomplete
	}
	if request.BatchID != "" {
		if err := dcBatchGuard(ctx, conn, run, request, target); err != nil {
			return nil, err
		}
	}
	dependencies, err := dcDependencies(ctx, conn, run)
	if err != nil {
		return nil, err
	}
	id, err := draftCreateInTx(ctx, conn, DraftCreateParams{
		ChapterNumber: request.ChapterNumber,
		Source:        "write",
		Content:       composed.Text,
		WordCount:     int64(units),
		Dependencies:  dependencies,
	})
	if err != nil {
		return nil, err
	}
	version, err := dcDraftVersion(ctx, conn, id)
	if err != nil {
		return nil, err
	}
	receipt := &DraftCommitReceipt{Success: true, ID: id, Version: version, ContentHash: composed.TextHash, Content: composed.Text}
	// :188-197 - the receipt is stamped into the attempt that produced the last
	// artifact, with the epoch that attempt was dispatched under.
	if err := dcStampReceipt(ctx, conn, run, composed, request, receipt); err != nil {
		return nil, err
	}
	// :200 - the transaction closes only if the batch it just extended still reads
	// back as the same batch. A conflict here rolls the whole commit back.
	if request.BatchID != "" {
		if _, err := dcReadBatch(ctx, conn, run, request); err != nil {
			return nil, err
		}
	}
	return receipt, nil
}

// dcAssertSources is assertSourcesCurrent(request.handle)
// (main-generation-owner.ts:729-743) as far as this package can prove it: the
// frozen epoch must still be the live one, the root must not be cancelled, and
// the rebuildBinding comparison is documented as out of scope in the file header.
func dcAssertSources(ctx context.Context, conn *sql.Conn, run *bpRun, liveEpoch string) error {
	if liveEpoch == "" || glText(run.binding["epoch"]) != liveEpoch {
		return ErrDraftCommitEpochStale
	}
	root, err := rrMaps(ctx, conn,
		"SELECT json_extract(action_json,'$.status') AS status FROM generation_roots WHERE root_action_id=?", run.rootID)
	if err != nil {
		return err
	}
	if len(root) == 0 {
		return glError("GENERATION_ROOT_MISSING")
	}
	if glText(root[0]["status"]) == "cancelled" {
		return ErrDraftCommitActionCancelled
	}
	return nil
}

// dcTargetUnits is the authorInputs draft:target-units lookup (:172). An absent
// input, a non-integer and a value below 1 are all INCOMPLETE, in that order of
// precedence.
func dcTargetUnits(manifest map[string]any) (int64, error) {
	text, ok := dcAuthorInput(manifest, "draft:target-units")
	if !ok {
		return 0, ErrDraftCommitIncomplete
	}
	target, parsed := generation.ParseDraftTargetUnits(text)
	if !parsed || !generation.DraftTargetUnitsUsable(target) {
		return 0, ErrDraftCommitIncomplete
	}
	return target, nil
}

// dcAuthorInput is `authorInputs.find(item => item.id === id)?.text`.
func dcAuthorInput(manifest map[string]any, id string) (string, bool) {
	inputs, ok := plotArray(manifest["authorInputs"])
	if !ok {
		return "", false
	}
	for _, input := range inputs {
		entry := glMap(input)
		if entry == nil || glText(entry["id"]) != id {
			continue
		}
		text, ok := entry["text"].(string)
		return text, ok
	}
	return "", false
}

// dcBatchGuard is the first readBatch of :196-199. readBatch throws its own
// snapshot conflict before it returns progress, and only then is the progress
// compared with this run and this chapter.
func dcBatchGuard(ctx context.Context, conn *sql.Conn, run *bpRun, request DraftCommitRequest, target int64) error {
	progress, err := dcReadBatch(ctx, conn, run, request)
	if err != nil {
		return err
	}
	if progress.RootHandle.RootActionID != run.rootID || progress.NextChapterNumber == nil ||
		*progress.NextChapterNumber != request.ChapterNumber || progress.TargetUnits != target {
		return ErrDraftCommitBatchConflict
	}
	for _, chapter := range progress.CompletedChapters {
		if chapter.ChapterNumber == request.ChapterNumber {
			return ErrDraftCommitBatchConflict
		}
	}
	return nil
}

// dcReadBatch is readBatch (generation-draft-effects.ts:99-103): the strict batch
// snapshot, whose source conflict is raised before progress is served.
func dcReadBatch(ctx context.Context, conn *sql.Conn, run *bpRun, request DraftCommitRequest) (*GenerationBatchProgress, error) {
	commits, err := bpBatchCommits(ctx, conn, request.BatchID)
	if err != nil {
		return nil, err
	}
	snapshot, err := bpSnapshot(ctx, conn, request.Handle.ProjectID, request.BatchID, commits)
	if err != nil {
		return nil, err
	}
	if snapshot.conflict != "" {
		return nil, glError(snapshot.conflict)
	}
	if snapshot.progress == nil {
		return nil, glError("GENERATION_BATCH_PROGRESS_INVALID")
	}
	return snapshot.progress, nil
}

// dcReadCommit is readCommit (generation-draft-effects.ts:75-95):
// readSaveState over the run's attempts, the one-commit rule, and the "the draft
// body no longer is the receipt" refusal.
func dcReadCommit(ctx context.Context, conn *sql.Conn, run *bpRun) (*DraftCommitReceipt, error) {
	rows, err := rrMaps(ctx, conn,
		"SELECT attempt_id,run_id,usage_receipt_json FROM generation_attempts WHERE run_id=? ORDER BY rowid", run.runID)
	if err != nil {
		return nil, err
	}
	var found *dcVerifiedCommit
	for _, row := range rows {
		verified, err := dcVerifiedCommitOf(ctx, conn, run, row)
		if err != nil {
			return nil, err
		}
		if verified == nil {
			continue
		}
		if found != nil {
			// :79-80 - more than one saved chapter under one run is damage, not a
			// choice to make.
			return nil, ErrDraftCommitReceiptInvalid
		}
		found = verified
	}
	if found == nil {
		return nil, nil
	}
	if !found.matchesBody {
		// :92-94 - the receipt names text the draft row no longer holds.
		return nil, ErrDraftCommitReceiptInvalid
	}
	return found.receipt, nil
}

// dcVerifiedCommit is one verified draftCommit receipt plus whether the draft row
// it names still holds that body (generation-draft-effects.ts:65-77).
type dcVerifiedCommit struct {
	receipt     *DraftCommitReceipt
	matchesBody bool
}

func dcVerifiedCommitOf(ctx context.Context, conn *sql.Conn, run *bpRun, row map[string]any) (*dcVerifiedCommit, error) {
	usageJSON, _ := row["usage_receipt_json"].(string)
	if usageJSON == "" {
		return nil, glError("GENERATION_DRAFT_RECEIPT_INVALID")
	}
	usage, err := glStored(usageJSON)
	if err != nil {
		return nil, err
	}
	stored := glMap(usage["draftCommit"])
	if stored == nil {
		// An absent or null receipt means "no commit"; any other truthy value is
		// the receipt the original refuses to read.
		if phTruthy(usage["draftCommit"]) {
			return nil, ErrDraftCommitReceiptInvalid
		}
		return nil, nil
	}
	composed, err := dcComposition(ctx, conn, run)
	if err != nil {
		return nil, err
	}
	if composed == nil || !dcDraftAlgorithm(composed.Algorithm) {
		return nil, ErrDraftCommitReceiptInvalid
	}
	success, _ := stored["success"].(bool)
	draftID, draftOK := rrInteger(stored["id"], -9007199254740991)
	version, versionOK := rrInteger(stored["version"], -9007199254740991)
	contentHash, hashOK := stored["contentHash"].(string)
	content, contentOK := stored["content"].(string)
	batchID, batchOK := dcStringField(stored, "batchId")
	chapterNumber, chapterOK := rrInteger(stored["chapterNumber"], -9007199254740991)
	manifestChapter, manifestChapterOK := rrInteger(run.manifest()["chapterNumber"], -9007199254740991)
	manifestBatch, manifestBatchOK := dcStringField(run.manifest(), "batchId")
	identity := glMap(usage["artifactIdentity"])
	if !success || !draftOK || !versionOK || !hashOK || !contentOK || !batchOK || !chapterOK ||
		!manifestChapterOK || !manifestBatchOK || identity == nil ||
		chapterNumber != manifestChapter || batchID != manifestBatch ||
		contentHash != generation.TextHash(content) || contentHash != composed.TextHash ||
		content != composed.Text || !dcHandleIs(stored["handle"], run, glText(identity["epoch"])) {
		return nil, ErrDraftCommitReceiptInvalid
	}
	draft, err := bpDraft(ctx, conn, draftID)
	if err != nil {
		return nil, err
	}
	if draft != nil && (chapterNumber != draft.chapterNumber || version != draft.version) {
		return nil, ErrDraftCommitReceiptInvalid
	}
	matchesBody := draft != nil && draft.bodyOK && content == draft.body
	return &dcVerifiedCommit{
		receipt: &DraftCommitReceipt{
			Success: true, ID: draftID, Version: version, ContentHash: contentHash, Content: content,
		},
		matchesBody: matchesBody,
	}, nil
}

// dcStringField reads one optional string field. An absent or JSON-null key is
// the empty string with ok true, because the original compares `undefined` with
// `undefined` as equal; any other non-string value is not a manifest field.
func dcStringField(object map[string]any, key string) (string, bool) {
	value, present := object[key]
	if !present || value == nil {
		return "", true
	}
	text, ok := value.(string)
	return text, ok
}

// dcHandleIs is isDeepStrictEqual(stored.handle, {...handleOf(run), epoch}). The
// stored handle must carry exactly the four keys with exactly these values, so a
// receipt that renamed or extended the handle is refused.
func dcHandleIs(value any, run *bpRun, epoch string) bool {
	handle := glMap(value)
	if handle == nil || len(handle) != 4 {
		return false
	}
	return glText(handle["projectId"]) == glText(run.binding["projectId"]) &&
		glText(handle["epoch"]) == epoch &&
		glText(handle["rootActionId"]) == run.rootID &&
		glText(handle["runId"]) == run.runID
}

// dcStampReceipt writes the draftCommit receipt into the attempt that produced the
// composition's last artifact (:188-197). The ordered object reproduces
// JSON.stringify of `{...receipt, chapterNumber, handle, ...(batchId ? {batchId} : {})}`
// key for key, and glPatch performs the `{...usage, draftCommit}` merge that keeps
// every other field of the stored receipt where it was.
func dcStampReceipt(ctx context.Context, conn *sql.Conn, run *bpRun, composed *dcComposed, request DraftCommitRequest, receipt *DraftCommitReceipt) error {
	identity := glMap(composed.lastIdentity)
	stored := plotObject{
		{"success", true},
		{"id", receipt.ID},
		{"version", receipt.Version},
		{"contentHash", receipt.ContentHash},
		{"content", receipt.Content},
		{"chapterNumber", request.ChapterNumber},
		{"handle", plotObject{
			{"projectId", glText(run.binding["projectId"])},
			{"epoch", glText(identity["epoch"])},
			{"rootActionId", run.rootID},
			{"runId", run.runID},
		}},
	}
	if request.BatchID != "" {
		stored = append(stored, plotField{"batchId", request.BatchID})
	}
	patched, err := glPatch(composed.lastUsage, map[string]any{"draftCommit": stored})
	if err != nil {
		return err
	}
	result, err := conn.ExecContext(ctx,
		"UPDATE generation_attempts SET usage_receipt_json=? WHERE attempt_id=?", patched, composed.lastAttemptID)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return ErrDraftCommitReceiptInvalid
	}
	return nil
}

// dcDraftVersion is the `SELECT version FROM drafts WHERE id=?` the reference runs
// right after the create (:192). It is redundant by construction and kept because
// the answer it stamps into the receipt is the row's own value, not the one the
// INSERT computed.
func dcDraftVersion(ctx context.Context, conn *sql.Conn, id int64) (int64, error) {
	rows, err := rrMaps(ctx, conn, "SELECT version FROM drafts WHERE id=?", id)
	if err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		return 0, glError("GENERATION_DRAFT_RECEIPT_INVALID")
	}
	version, ok := rrInteger(rows[0]["version"], -9007199254740991)
	if !ok {
		return 0, glError("GENERATION_DRAFT_RECEIPT_INVALID")
	}
	return version, nil
}

// dcDependencies is the source_dependencies list of :181-188: every selected
// candidate draft in sourceRefs order, then every finalized selection as a sealed
// dependency whose chapter number is read from the draft it names.
func dcDependencies(ctx context.Context, conn *sql.Conn, run *bpRun) ([]any, error) {
	refs, ok := plotArray(run.binding["sourceRefs"])
	if !ok {
		return nil, nil
	}
	dependencies := make([]any, 0, len(refs))
	for _, ref := range refs {
		entry := glMap(ref)
		if entry == nil {
			continue
		}
		draftID, matched, err := dcSourceID(glText(entry["sourceId"]), "draft:")
		if err != nil {
			return nil, err
		}
		if !matched {
			continue
		}
		dependencies = append(dependencies, plotObject{
			{"kind", "candidate"},
			{"draftId", draftID},
			{"contentHash", glText(entry["contentHash"])},
		})
	}
	for _, ref := range refs {
		entry := glMap(ref)
		if entry == nil {
			continue
		}
		sourceID := glText(entry["sourceId"])
		if !strings.HasPrefix(sourceID, "finalized:") {
			continue
		}
		rest := sourceID[len("finalized:"):]
		separator := strings.IndexByte(rest, ':')
		if separator <= 0 || separator == len(rest)-1 || dcHasLineBreak(rest[separator+1:]) {
			continue
		}
		digits := rest[:separator]
		if !dcDigits(digits) {
			continue
		}
		draftID, err := strconv.ParseInt(digits, 10, 64)
		if err != nil || draftID < 1 || draftID > maxSafeInteger {
			return nil, ErrDraftDependencyInvalid
		}
		rows, err := rrMaps(ctx, conn, "SELECT chapter_number FROM drafts WHERE id=?", draftID)
		if err != nil {
			return nil, err
		}
		sealed := plotObject{
			{"kind", "finalized"},
			{"draftId", draftID},
			{"finalizationId", rest[separator+1:]},
		}
		if len(rows) != 0 {
			// A missing draft leaves chapterNumber out, which the create gate below
			// refuses exactly like the original's `undefined`.
			if chapter, ok := rrInteger(rows[0]["chapter_number"], -9007199254740991); ok {
				sealed = append(sealed, plotField{"chapterNumber", chapter})
			}
		}
		sealed = append(sealed, plotField{"contentHash", glText(entry["contentHash"])})
		dependencies = append(dependencies, sealed)
	}
	return dependencies, nil
}

// dcSourceID is `/^(prefix)(\d+)$/` on one sourceId.
func dcSourceID(sourceID, prefix string) (int64, bool, error) {
	if !strings.HasPrefix(sourceID, prefix) {
		return 0, false, nil
	}
	digits := sourceID[len(prefix):]
	if !dcDigits(digits) {
		return 0, false, nil
	}
	draftID, err := strconv.ParseInt(digits, 10, 64)
	if err != nil || draftID < 1 || draftID > maxSafeInteger {
		// Number() would produce a value the dependency gate cannot accept.
		return 0, false, ErrDraftDependencyInvalid
	}
	return draftID, true, nil
}

// dcDigits reports whether text is a non-empty run of ASCII digits, which is what
// both dependency patterns require (`\d`).
func dcDigits(text string) bool {
	if text == "" {
		return false
	}
	for _, c := range text {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// dcHasLineBreak reports whether text contains a line terminator, so `.` in the
// original's `(.+)` matches nothing here.
func dcHasLineBreak(text string) bool {
	return strings.ContainsAny(text, "\n\r\u2028\u2029")
}

// draftCreateInTx is DraftRepository.create (:314-359) on a connection that
// already holds this write's transaction. The statements, their order and both
// dependency refusals are Drafts.Create's (draft_author.go:59-124); only the
// transaction boundary moved, because the receipt stamp has to share it.
func draftCreateInTx(ctx context.Context, conn *sql.Conn, params DraftCreateParams) (int64, error) {
	serialized := "[]"
	if params.Dependencies != nil {
		encoded, err := plotJSON(params.Dependencies)
		if err != nil {
			return 0, ErrDraftDependencyInvalid
		}
		serialized = encoded
	}
	dependencies, valid := parseDependencies(sql.NullString{String: serialized, Valid: true})
	if !valid {
		return 0, ErrDraftDependencyInvalid
	}
	ids := make([]int64, 0, len(dependencies))
	for _, dependency := range dependencies {
		ids = append(ids, dependency.DraftID)
	}
	states, err := dcDependencyStates(ctx, conn, ids)
	if err != nil {
		return 0, err
	}
	if !dependenciesCurrent(dependencies, states, make(map[int64]bool)) {
		return 0, ErrDraftDependencyStale
	}
	var maxVersion sql.NullInt64
	if err := conn.QueryRowContext(ctx,
		"SELECT MAX(version) FROM drafts WHERE chapter_number = ?", params.ChapterNumber).Scan(&maxVersion); err != nil {
		return 0, err
	}
	inserted, err := conn.ExecContext(ctx, "INSERT INTO contents(body) VALUES(?)", params.Content)
	if err != nil {
		return 0, err
	}
	contentID, err := inserted.LastInsertId()
	if err != nil {
		return 0, err
	}
	inserted, err = conn.ExecContext(ctx, draftCreateSQL, params.ChapterNumber, maxVersion.Int64+1,
		params.Source, contentID, params.WordCount, serialized)
	if err != nil {
		return 0, err
	}
	id, err := inserted.LastInsertId()
	if err != nil {
		return 0, err
	}
	return id, nil
}

// dcDependencyStates is loadDependencyStates (dependencies.go:110-172) for the
// connection, not a transaction: the query, its chunking, the transitive
// expansion and every field the decision functions read are the same, so
// dependencyCurrent / lineageCurrent / dependenciesCurrent decide identically.
func dcDependencyStates(ctx context.Context, conn *sql.Conn, ids []int64) (map[int64]dependencyState, error) {
	states := make(map[int64]dependencyState)
	queued := make(map[int64]bool)
	pending := make([]int64, 0, len(ids))
	for _, id := range ids {
		if !queued[id] {
			queued[id] = true
			pending = append(pending, id)
		}
	}
	for offset := 0; offset < len(pending); {
		end := offset + 500
		if end > len(pending) {
			end = len(pending)
		}
		chunk := pending[offset:end]
		offset = end
		args := make([]any, len(chunk))
		for index, id := range chunk {
			args[index] = id
		}
		query := `SELECT d.id AS id, d.chapter_number AS chapterNumber, d.status AS status,
 d.source_dependencies AS sourceDependencies, c.body AS body,
 f.finalization_id AS finalizationId, f.content_hash AS contentHash, f.content_snapshot AS contentSnapshot,
 NOT EXISTS (SELECT 1 FROM drafts newer WHERE newer.chapter_number = d.chapter_number
 AND newer.status = 'finalized' AND (newer.version > d.version OR
 (newer.version = d.version AND newer.id > d.id))) AS authoritative
 FROM drafts d JOIN contents c ON c.id = d.content_id
 LEFT JOIN finalization_outbox f ON f.draft_id = d.id
 WHERE d.id IN (` + strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",") + ")"
		rows, err := rrMaps(ctx, conn, query, args...)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			id, ok := rrInteger(row["id"], -9007199254740991)
			if !ok {
				return nil, glError("GENERATION_DRAFT_DEPENDENCY_INVALID")
			}
			var state dependencyState
			if chapter, ok := rrInteger(row["chapterNumber"], -9007199254740991); ok {
				state.chapter = chapter
			}
			state.status = glText(row["status"])
			if body, ok := row["body"].(string); ok {
				state.hash = generation.TextHash(body)
			}
			if snapshot, ok := row["contentSnapshot"].(string); ok {
				body, _ := row["body"].(string)
				state.receiptMatches = snapshot == body
			} else {
				state.receiptMatches = true
			}
			if value, ok := row["finalizationId"].(string); ok {
				state.finalizationID = sql.NullString{String: value, Valid: true}
			}
			if value, ok := row["contentHash"].(string); ok {
				state.receiptHash = sql.NullString{String: value, Valid: true}
			}
			dependencies, valid := parseDependencies(sql.NullString{String: glText(row["sourceDependencies"]), Valid: row["sourceDependencies"] != nil})
			state.dependencies, state.valid = dependencies, valid
			authoritative, _ := rrInteger(row["authoritative"], 0)
			state.authoritative = authoritative != 0
			states[id] = state
			if state.valid {
				for _, dependency := range state.dependencies {
					if !queued[dependency.DraftID] {
						queued[dependency.DraftID] = true
						pending = append(pending, dependency.DraftID)
					}
				}
			}
		}
	}
	return states, nil
}

// dcComposed is one recomposed visible composition
// (generation-run-repository.ts:460-516) plus the attempt that carries its last
// artifact, which is where the receipt is stamped.
type dcComposed struct {
	Algorithm     string
	Text          string
	TextHash      string
	ArtifactIDs   []string
	Sources       []dcSource
	lastAttemptID string
	lastUsage     string
	lastIdentity  map[string]any
	lastResult    map[string]any
}

// dcSource is one entry of VisibleCompositionReceipt['sources'].
type dcSource struct {
	artifactID string
	revision   int64
	textHash   string
}

// The two attempt purposes :472 refuses as composition sources.
const (
	// DRAFT_RECONCILE_PURPOSE, src/shared/draft-reconciliation.ts:12.
	dcReconcilePurpose = "chapter-draft-reconcile"
	// DRAFT_SHORT_OUTLINE_PURPOSE, src/shared/draft-short-outline.ts:3.
	dcShortOutlinePurpose = "chapter-draft-short-outline"
)

// dcDraftAlgorithm is isDraftVisibleTextVersion (src/shared/draft-visible-text.ts:5-7).
func dcDraftAlgorithm(algorithm string) bool {
	return algorithm == generation.DraftVisibleTextVersion || algorithm == generation.DraftVisibleTextVersionV1
}

// dcComposition is readVisibleComposition(runId) (generation-run-repository.ts:
// 659-674): the newest stored visibleComposition receipt of the run is recomposed
// from the run's artifacts and compared with what the receipt claims. A run with
// no receipt has no composition, which is not an error on this path.
func dcComposition(ctx context.Context, conn *sql.Conn, run *bpRun) (*dcComposed, error) {
	rows, err := rrMaps(ctx, conn, `SELECT json_quote(json_extract(usage_receipt_json,'$.visibleComposition')) AS composition
 FROM generation_attempts WHERE run_id=? ORDER BY rowid DESC`, run.runID)
	if err != nil {
		return nil, err
	}
	var read *dcRows
	for _, row := range rows {
		text, _ := row["composition"].(string)
		if text == "" || text == "null" {
			continue
		}
		stored, err := glStored(text)
		if err != nil {
			return nil, err
		}
		if read == nil {
			if read, err = dcReadRows(ctx, conn, run); err != nil {
				return nil, err
			}
		}
		algorithm := glText(stored["algorithm"])
		ids, ok := plotArray(stored["artifactIds"])
		current, err := dcVisible(run, read, ids, ok, algorithm)
		if err != nil {
			return nil, err
		}
		if !dcCompositionSame(stored, current) {
			return nil, glError("GENERATION_COMPOSITION_INTEGRITY_FAILED")
		}
		return current, nil
	}
	return nil, nil
}

// dcCompositionSame is the stored-versus-current comparison of :666-670. The
// stored receipt never carries `text` (composeVisible drops it), so only the
// algorithm, the hash and the two source projections are compared.
func dcCompositionSame(stored map[string]any, current *dcComposed) bool {
	if glText(stored["algorithm"]) != current.Algorithm {
		return false
	}
	if textHash, _ := stored["textHash"].(string); textHash != current.TextHash {
		return false
	}
	// `chapters` is present only on the plot-outline protocol, which this read
	// does not compose; a stored key of either shape cannot match.
	if _, present := stored["chapters"]; present {
		return false
	}
	sources, ok := plotArray(stored["sources"])
	if !ok || len(sources) != len(current.Sources) {
		return false
	}
	for index, value := range sources {
		entry := glMap(value)
		if entry == nil || len(entry) != 3 {
			return false
		}
		revision, ok := rrInteger(entry["revision"], -9007199254740991)
		if !ok || glText(entry["artifactId"]) != current.Sources[index].artifactID ||
			revision != current.Sources[index].revision ||
			glText(entry["textHash"]) != current.Sources[index].textHash {
			return false
		}
	}
	return true
}

// dcRows is the projection of receiptSelect (generation-run-repository.ts:85-90)
// for one root: the artifact rows of every attempt, and the frozen fingerprint the
// artifact payload has to agree with.
type dcRows struct {
	byArtifact  map[string]*dcArtifactRow
	fingerprint string
}

// dcArtifactRow is one generation_artifacts row joined with its attempt's receipt
// projection.
type dcArtifactRow struct {
	artifactID   string
	attemptID    string
	runID        string
	artifactJSON any
	revision     int64
	status       string
	ordinal      int64
	purpose      string
	attempt      string
	identity     map[string]any
	result       map[string]any
	usage        string
}

func dcReadRows(ctx context.Context, conn *sql.Conn, run *bpRun) (*dcRows, error) {
	fingerprint, err := plotJSON(run.binding["fingerprint"])
	if err != nil {
		return nil, err
	}
	rows, err := rrMaps(ctx, conn, `SELECT t.attempt_id AS attemptId, t.run_id AS attemptRunId, t.rowid AS ordinal,
 t.attempt_json AS attemptJson, t.usage_receipt_json AS usage,
 json_extract(t.usage_receipt_json,'$.purpose') AS purpose,
 json_extract(t.usage_receipt_json,'$.artifactIdentity') AS identity,
 json_extract(t.usage_receipt_json,'$.result') AS result,
 a.artifact_id AS artifactId, a.run_id AS artifactRunId, a.artifact_json AS artifactJson,
 a.revision AS revision, a.status AS artifactStatus
 FROM generation_attempts t LEFT JOIN generation_artifacts a ON a.attempt_id=t.attempt_id
 WHERE t.root_action_id=? ORDER BY t.rowid`, run.rootID)
	if err != nil {
		return nil, err
	}
	read := &dcRows{byArtifact: make(map[string]*dcArtifactRow, len(rows)), fingerprint: fingerprint}
	for _, row := range rows {
		artifactID, _ := row["artifactId"].(string)
		if artifactID == "" {
			continue
		}
		id, idOK := rrInteger(row["ordinal"], -9007199254740991)
		revision, revisionOK := rrInteger(row["revision"], -9007199254740991)
		if !idOK || !revisionOK {
			return nil, glError("ARTIFACT_INTEGRITY_FAILED")
		}
		entry := &dcArtifactRow{
			artifactID:   artifactID,
			attemptID:    glText(row["attemptId"]),
			runID:        glText(row["artifactRunId"]),
			artifactJSON: row["artifactJson"],
			revision:     revision,
			status:       glText(row["artifactStatus"]),
			ordinal:      id,
			purpose:      glText(row["purpose"]),
			attempt:      glText(row["attemptJson"]),
			usage:        glText(row["usage"]),
		}
		if value, ok := row["identity"].(string); ok && value != "" && value != "null" {
			if entry.identity, err = glStored(value); err != nil {
				return nil, err
			}
		}
		if value, ok := row["result"].(string); ok && value != "" && value != "null" {
			if entry.result, err = glStored(value); err != nil {
				return nil, err
			}
		}
		read.byArtifact[artifactID] = entry
	}
	return read, nil
}

// dcVisible is visibleComposition (generation-run-repository.ts:460-516) for the
// two draft algorithms. The plot-outline protocol is a sibling slice and is
// refused with the same code the original uses for an algorithm it does not
// compose under.
func dcVisible(run *bpRun, read *dcRows, ids []any, idsWellFormed bool, algorithm string) (*dcComposed, error) {
	if !dcDraftAlgorithm(algorithm) && algorithm != generation.VisibleContinuationVersion {
		return nil, glError("GENERATION_COMPOSITION_ALGORITHM_INVALID")
	}
	artifactIDs := make([]string, 0, len(ids))
	seen := make(map[string]bool, len(ids))
	for _, value := range ids {
		text, ok := value.(string)
		if !ok || text == "" || seen[text] {
			return nil, glError("GENERATION_COMPOSITION_INVALID")
		}
		seen[text] = true
		artifactIDs = append(artifactIDs, text)
	}
	if !idsWellFormed || len(artifactIDs) == 0 || len(artifactIDs) > 32 {
		return nil, glError("GENERATION_COMPOSITION_INVALID")
	}
	composed := &dcComposed{Algorithm: algorithm, ArtifactIDs: artifactIDs, Sources: make([]dcSource, 0, len(artifactIDs))}
	text := ""
	previousOrdinal := int64(-1)
	for index, artifactID := range artifactIDs {
		row := read.byArtifact[artifactID]
		if row == nil || row.runID != run.runID || row.status == "discarded" || row.ordinal <= previousOrdinal {
			return nil, glError("GENERATION_COMPOSITION_SOURCE_INVALID")
		}
		artifact, err := dcHydrate(run, read, row)
		if err != nil {
			return nil, err
		}
		if row.purpose == dcReconcilePurpose || row.purpose == dcShortOutlinePurpose {
			return nil, glError("GENERATION_COMPOSITION_SOURCE_INVALID")
		}
		// The attempt's own terminal state and its visible text, both from the row
		// read, are what :474-475 checks.
		attempt, err := glStored(row.attempt)
		if err != nil {
			return nil, err
		}
		status := glText(attempt["status"])
		rawText, _ := artifact["text"].(string)
		if (status != "settled" && status != "unknown") || trimJS(rawText) == "" {
			return nil, glError("GENERATION_COMPOSITION_SOURCE_NOT_TERMINAL")
		}
		finishReason := glText(row.result["finishReason"])
		if finishReason != "stop" && finishReason != "length" {
			return nil, glError("GENERATION_COMPOSITION_SOURCE_UNTRUSTED")
		}
		draftAlgorithm := ""
		if dcDraftAlgorithm(algorithm) {
			draftAlgorithm = algorithm
		}
		var next string
		condense := draftAlgorithm != "" && row.purpose == generation.DraftCondensePurpose
		switch {
		case condense && (len(composed.Sources) == 0 || index != len(artifactIDs)-1):
			return nil, glError("GENERATION_COMPOSITION_SOURCE_INVALID")
		case condense:
			next = generation.SanitizeDraftText(rawText, draftAlgorithm)
		case draftAlgorithm != "" && len(composed.Sources) > 0:
			next = generation.ComposeDraftVisibleContinuation(text, rawText, draftAlgorithm)
		case draftAlgorithm != "":
			next = generation.SanitizeDraftText(rawText, draftAlgorithm)
		case len(composed.Sources) > 0:
			next = generation.ComposeVisibleContinuation(text, rawText)
		default:
			next = trimJS(rawText)
		}
		if trimJS(next) == "" {
			return nil, glError("GENERATION_COMPOSITION_NO_PROGRESS")
		}
		if condense {
			target, err := dcTargetUnits(run.manifest())
			if err != nil {
				return nil, glError("GENERATION_COMPOSITION_SOURCE_INVALID")
			}
			targetRange, rangeErr := generation.DraftTargetUnitRange(target)
			if rangeErr != nil || finishReason != "stop" ||
				int64(generation.CountDraftUnits(text)) <= targetRange.Maximum {
				return nil, glError("GENERATION_COMPOSITION_NO_PROGRESS")
			}
		} else {
			if draftAlgorithm != "" && len(composed.Sources) > 0 && finishReason == "length" &&
				generation.CountDraftUnits(next)-generation.CountDraftUnits(text) < 300 {
				return nil, glError("GENERATION_COMPOSITION_NO_PROGRESS")
			}
			if len(composed.Sources) > 0 &&
				dcLetterDigitCount(next) <= dcLetterDigitCount(text) {
				return nil, glError("GENERATION_COMPOSITION_NO_PROGRESS")
			}
		}
		text = next
		previousOrdinal = row.ordinal
		textHash, _ := artifact["textHash"].(string)
		revision, revisionOK := rrInteger(artifact["revision"], -9007199254740991)
		if !revisionOK {
			return nil, glError("ARTIFACT_INTEGRITY_FAILED")
		}
		composed.Sources = append(composed.Sources, dcSource{artifactID: artifactID, revision: revision, textHash: textHash})
		composed.lastAttemptID = row.attemptID
		composed.lastUsage = row.usage
		composed.lastIdentity = row.identity
		composed.lastResult = row.result
	}
	composed.Text = text
	composed.TextHash = generation.TextHash(text)
	return composed, nil
}

// dcHydrate is hydrateReceipt (generation-run-repository.ts:363-377) reduced to
// the artifact half: the payload, the usage identity and the frozen binding have
// to agree on the artifact, the epoch, the fingerprint and the project, and the
// payload's hash has to still cover its own text.
func dcHydrate(run *bpRun, read *dcRows, row *dcArtifactRow) (map[string]any, error) {
	artifact := glMap(row.artifactJSON)
	if artifact == nil {
		return nil, glError("ARTIFACT_INTEGRITY_FAILED")
	}
	identity := row.identity
	if identity == nil {
		return nil, glError("ARTIFACT_INTEGRITY_FAILED")
	}
	text, textOK := artifact["text"].(string)
	textHash, hashOK := artifact["textHash"].(string)
	revision, revisionOK := rrInteger(artifact["revision"], -9007199254740991)
	fingerprint, err := plotJSON(artifact["fingerprint"])
	if err != nil {
		return nil, err
	}
	identityFingerprint, err := plotJSON(identity["fingerprint"])
	if err != nil {
		return nil, err
	}
	if glText(artifact["attemptId"]) != row.attemptID ||
		glText(artifact["rootActionId"]) != run.rootID ||
		glText(artifact["artifactId"]) != row.artifactID ||
		glText(identity["artifactId"]) != row.artifactID ||
		glText(artifact["epoch"]) != glText(identity["epoch"]) ||
		fingerprint != identityFingerprint || fingerprint != read.fingerprint ||
		glText(artifact["projectId"]) != glText(run.binding["projectId"]) ||
		!textOK || !hashOK || !revisionOK || revision != row.revision ||
		generation.TextHash(text) != textHash {
		return nil, glError("ARTIFACT_INTEGRITY_FAILED")
	}
	return artifact, nil
}

// dcLetterDigitCount is `text.match(/[\p{L}\p{N}]/gu)?.length ?? 0`
// (generation-run-repository.ts:508): one per code point in the Unicode general
// category Letter or Number.
func dcLetterDigitCount(text string) int {
	count := 0
	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			count++
		}
	}
	return count
}
