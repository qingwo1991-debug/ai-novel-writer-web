package store

// Native port of the review-revision context freeze producer: the missing writer
// side of the `review-revision:*` family. The admission verifiers already read
// generation_runs.binding_json -> sourceManifest.reviewRevisionContext
// (reviewcycle_effect.go:165-207, reviewcycle_generated.go:204-220,
// reviewcycle_rechecked.go:383-407), but nothing in this module produced that
// value. This file is that producer.
//
// Reference (electron/services/review-revision-context.ts):
//
//	captureReviewRevisionContext  (:39-155)
//	freezeChapterGoals            (src/shared/chapter-goal-review.ts:25-40)
//	findBlueprintContinuityRisks  (src/shared/consistency-preflight.ts:84-119)
//	listFinalizedContinuityBefore (electron/repositories/summary-repository.ts:706-825)
//
// plus what those depend on: continuityStableFactKey (consistency-preflight.ts:
// 58-76), normalizedKeyPart / isExplicitTerminalSubject (:36-56),
// ConsistencyExemptionRepository.list (electron/repositories/consistency-
// exemption-repository.ts:9-17), the two store parsers parseFacts and
// parseCharacterStateCandidates (summary-repository.ts:104-174) and the identity
// literals ReviewMaterialIdentity / FinalizedSourceIdentity
// (src/shared/review-revision-generation.ts, src/shared/finalized-continuity.ts:
// 12-17).
//
// THE HASH IS THE CONTRACT. review-revision-context.ts:146 returns one object;
// generation-run-repository.ts:92 defines `textHash = sha256(text)`; and the
// admission verifier hashes the *serialized* frozen object:
//
//	reviewcycle_effect.go:189-190
//	  if !m03CanonicalJSJSON(frozen) || m03StringHash(string(frozen)) != contextHash
//	reviewcycle_effect.go:184
//	  manifest["reviewRevisionContextHash"] must equal contextHash
//
// So this port never hashes a Go encoding: it emits the bytes
// `JSON.stringify(context)` emits and hashes those. plotJSON/plotQuote
// (plottree.go:30-49) is the package's JS-faithful serializer, so every object
// below is a plotObject in the reference's literal key order, and every column
// holding stored JSON is re-parsed with the order-preserving decoder
// (rosterJSONValue, character_roster.go:140-199), because
// JSON.stringify(JSON.parse(x)) reproduces the *stored* key order.
//
// Tables read (checked against internal/admission/baseline_v7_ddl.go). This is a
// read-only builder: it creates no table, writes no row, and returns the frozen
// value for the generation-ledger writer to attach.
//
//	drafts                    id, chapter_number, version, status, content_id,
//	                          source_dependencies          (baseline_v7_ddl.go:205-217)
//	contents                  id, body                     (:195-199)
//	finalization_outbox       draft_id, finalization_id, chapter_title,
//	                          content_hash, content_snapshot (:218-234)
//	project_core              genre, sub_genre, target_audience, total_chapters,
//	                          words_per_chapter, writing_language,
//	                          creative_strategy, narrative_thread_dormant_threshold,
//	                          plot_structure, narrative_pov, writing_style,
//	                          reference_works, global_guidance, golden_finger,
//	                          core_outline, world_setting, protagonist_profile,
//	                          worldbuilding                (:486-518)
//	blueprints                chapter_number, title, role, purpose, key_events,
//	                          characters, suspense_hook, user_guidance, notes (:60-73)
//	summary_snapshots         draft_id, chapter_number, chapter_notes,
//	                          continuity_facts, character_state_candidates,
//	                          source_finalization_id, source_content_hash,
//	                          projection_generation            (:630-643)
//	continuity_projection_meta  id, generation, stale_from_chapter (:200-204)
//	characters                character_id, name, role, notes, retired, cs_*,
//	                          cs_updated_at_chapter, cs_provenance   (:166-189)
//	character_identity_meta   table presence only              (:123-125)
//	character_identity_proposals  proposal_id, source_key, source_hash,
//	                          raw_value (per-finalization frozen roster) (:130-134)
//	consistency_exemptions    stable_fact_key, reason, revoked   (:190-194)
//	sqlite_master             table presence                   (summary.go:514-517)
//
// Capabilities this lane does not have are refused, never weakened:
//   - refine-from-review (`reviewSourceId` + `confirmedReviewContent`) needs
//     parseHumanConfirmedReviewSnapshot / serializeHumanConfirmedReviewSnapshot
//     and ReviewRepository.getFull; neither is ported in this package, so
//     review-revision-context.ts:77-91 is refused with
//     REVIEW_REVISION_CONTEXT_CONFIRMATION_UNAVAILABLE. The verifier requires
//     confirmation to be absent for the admitted operations
//     (reviewcycle_effect.go:196), so nothing is silently under-frozen.
//   - the recheck arm (:61-67) needs ReviewCycleRepository.planRecheck.
//     reviewcycle_recheck.go has only internal planners, so :61-67 is refused
//     with REVIEW_REVISION_CONTEXT_RECHECK_UNAVAILABLE.
//
// Two arms follow this package's own canonical readers where the reference inlines
// a looser check. Both fail closed, and both are narrower than the reference:
//   - :114-117 skips a dependency whose kind is explicitly null and keeps
//     scanning. parseDependencies (dependencies.go:13-72) is this package's
//     canonical ledger reader and rejects such an entry outright, so the whole
//     capture refuses with GENERATION_REVIEW_HISTORY_CHANGED instead of writing a
//     predecessor computed from a list this module cannot fully trust.
//   - :132-133 re-filters CharacterRepository.getAll by the active roster; the
//     same two reads run here (charactersAll already filters retired=0 when the
//     identity tables exist) so the author card order matches the reference.
//
// The reference's own crash arms are named refusals here instead of TypeErrors: a
// null world_setting (:151), a null character notes (:137), a non-string blueprint
// characters/keyEvents (:92-94) and a non-string worldbuilding (:149) each return a
// review-revision specific code (see the sentinels below). A crash and a refusal
// both write nothing; only the refusal is reportable through the RPC layer.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
)

var (
	// review-revision-context.ts:20 `const fail = () => { throw new Error(
	// 'GENERATION_REVIEW_CONTEXT_INVALID') }` — the request guards (:41-53), an
	// unreadable project_core row (:69) and a malformed confirmation request (:78).
	ErrReviewRevisionContextInvalid = errors.New("GENERATION_REVIEW_CONTEXT_INVALID")
	// review-revision-context.ts:60
	//   if (!source || latest !== source.id || !isDeepStrictEqual(request.
	//   expectedDraft, {...})) throw new Error('GENERATION_REVIEW_SOURCE_CHANGED')
	ErrReviewRevisionSourceChanged = errors.New("GENERATION_REVIEW_SOURCE_CHANGED")
	// review-revision-context.ts:101, :116, :121, :125, :127
	//   throw new Error('GENERATION_REVIEW_HISTORY_CHANGED')
	ErrReviewRevisionHistoryChanged = errors.New("GENERATION_REVIEW_HISTORY_CHANGED")
	// review-revision-context.ts:77-91 is not portable in this lane (no
	// human-confirmed-review snapshot codec and no full review read).
	ErrReviewRevisionContextConfirmationUnavailable = errors.New("REVIEW_REVISION_CONTEXT_CONFIRMATION_UNAVAILABLE")
	// review-revision-context.ts:61-67 is not portable in this lane (no
	// ReviewCycleRepository.planRecheck entry point).
	ErrReviewRevisionContextRecheckUnavailable = errors.New("REVIEW_REVISION_CONTEXT_RECHECK_UNAVAILABLE")
	// review-revision-context.ts:149 core.worldbuilding feeds a field the verifier
	// requires to be a string (reviewcycle_effect.go:198 m03JSONIsString). A NULL
	// would make the reference emit null and the run unverifiable.
	ErrReviewRevisionContextWorldbuildingInvalid = errors.New("REVIEW_REVISION_CONTEXT_WORLDBUILDING_INVALID")
	// consistency-preflight.ts:92 `blueprint.characters.map(normalizedKeyPart)`
	// throws a TypeError on a non-string entry, and review-revision-context.ts:
	// 150 calls `(keyEvents ?? '').split(...)`, which throws on a non-string.
	ErrReviewRevisionContextBlueprintInvalid = errors.New("REVIEW_REVISION_CONTEXT_BLUEPRINT_INVALID")
	// review-revision-context.ts:151 hands [core.worldSetting, ...card.notes] to
	// freezeChapterGoals, which calls source.split on every entry.
	ErrReviewRevisionContextAuthorSourceInvalid = errors.New("REVIEW_REVISION_CONTEXT_AUTHOR_SOURCE_INVALID")
	// review-revision-context.ts:39-40 takes the project id from the owning
	// session scope. An empty id cannot produce a re-provable material identity.
	ErrReviewRevisionContextProjectRequired = errors.New("REVIEW_REVISION_CONTEXT_PROJECT_REQUIRED")
)

// The three operations REVIEW_REVISION_OPERATIONS admits (review-revision-context.ts:19).
const (
	ReviewRevisionOperationReviewChapter    = "review-chapter"
	ReviewRevisionOperationRefineDraft      = "refine-draft"
	ReviewRevisionOperationRefineFromReview = "refine-from-review"
)

// The two locales captureReviewRevisionContext admits (:43).
const (
	ReviewRevisionLocaleZH = "zh-CN"
	ReviewRevisionLocaleEN = "en-US"
)

// rrctxAuthorInputTextLimit is the per-input byte ceiling
// (:49 `Buffer.byteLength(item.text) > 8 * 1024 * 1024`).
const rrctxAuthorInputTextLimit = 8 * 1024 * 1024

// rrctxAuthorInputID is the sourceManifest.authorInputs id the verifier requires
// for the frozen context itself (reviewcycle_effect.go:205-207).
const rrctxAuthorInputID = "review-revision-context"

// rrctxBlueprintWindow is the blueprint window at :92 (`row.chapterNumber >=
// source.chapterNumber && row.chapterNumber <= source.chapterNumber + 5`).
const rrctxBlueprintWindow = 5

// rrctxJSWhitespaceClass is the ECMAScript /\s/u set as the body of a
// regular-expression character class. Go's own \s is only [\t\n\f\r ], which
// would change both the freezeChapterGoals marker (chapter-goal-review.ts:29)
// and the terminal-state preflight patterns (consistency-preflight.ts:46-54).
const rrctxJSWhitespaceClass = `\t\n\v\f\r \x{00a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}`

// ReviewRevisionContexts borrows an admitted project database. It owns no
// schema, opens no write transaction, and never re-enters the pool from inside
// the read: the frozen context must come from one snapshot of drafts, contents,
// blueprints, snapshots and roster, or a concurrent finalize would produce a
// hybrid that no later verification can reproduce.
type ReviewRevisionContexts struct{ db *sql.DB }

func NewReviewRevisionContexts(db *sql.DB) *ReviewRevisionContexts {
	return &ReviewRevisionContexts{db: db}
}

// FrozenReviewRevisionContext is the producer's output: the exact bytes
// `JSON.stringify(context)` would produce, their sha256, and the identity the
// manifest must echo. Context is kept as the ordered value too, so a caller can
// embed it in a manifest without ever re-sorting its keys.
type FrozenReviewRevisionContext struct {
	Operation     string
	DraftID       int64
	ChapterNumber int64
	Version       int64
	Status        string
	SourceHash    string
	AuthorInputs  []any
	Context       plotObject
	Text          string
	ContextHash   string
}

// SourceManifestFields is the sourceManifest fragment the generation-ledger
// writer must persist with the frozen context:
//
//	reviewcycle_effect.go:166-184 reads sourceManifest.reviewRevisionContext,
//	sourceManifest.reviewRevisionContextHash and sourceManifest.operation, and
//	:203-207 requires sourceManifest.authorInputs to be exactly one entry
//	{"id":"review-revision-context","text":<frozen text>}.
//
// Manifest key order is not hashed (only the frozen context's bytes are), but the
// values are: reviewRevisionContext is the same plotObject, so plotJSON
// reproduces Text byte for byte.
func (f *FrozenReviewRevisionContext) SourceManifestFields() plotObject {
	if f == nil {
		return nil
	}
	return plotObject{
		{"operation", f.Operation},
		{"authorInputs", []any{plotObject{{"id", rrctxAuthorInputID}, {"text", f.Text}}}},
		{"reviewRevisionContext", f.Context},
		{"reviewRevisionContextHash", f.ContextHash},
	}
}

// Capture is captureReviewRevisionContext (:39-155). It validates the request,
// proves the captured draft is still the author's selection, and returns the
// frozen context. It never writes: the surrounding transaction is a deferred
// read transaction on one reserved connection, so every SELECT below sees one
// SQLite snapshot and a failure cannot leave a write lock behind.
func (r *ReviewRevisionContexts) Capture(ctx context.Context, projectID string, request map[string]any) (*FrozenReviewRevisionContext, error) {
	values, err := rrctxRequest(request)
	if err != nil {
		return nil, err
	}
	if summaryTrim(projectID) == "" || !summarySupportedText(projectID) {
		return nil, ErrReviewRevisionContextProjectRequired
	}
	if r == nil || r.db == nil {
		return nil, ErrReviewRevisionProjectNotOpen
	}
	conn, err := r.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	// Plain BEGIN: a consistent read snapshot without the writer reservation
	// BEGIN IMMEDIATE would take. This function has no write arm to protect.
	if _, err := conn.ExecContext(ctx, "BEGIN"); err != nil {
		return nil, err
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = conn.ExecContext(cleanup, "ROLLBACK")
	}()
	frozen, err := rrctxCapture(ctx, conn, projectID, values)
	if err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, err
	}
	committed = true
	return frozen, nil
}

// rrctxExpectedDraft mirrors PrepareReviewRevisionRequest.expectedDraft.
type rrctxExpectedDraft struct {
	chapter     int64
	version     int64
	status      string
	contentHash string
	chapterOK   bool
	versionOK   bool
	statusOK    bool
	hashOK      bool
}

type rrctxRequestValues struct {
	operation        string
	draftID          int64
	expected         rrctxExpectedDraft
	authorInputs     []any
	uiLocale         string
	recheckRequested bool
}

// rrctxRequest is the request guard (:41-53). It rejects unknown keys exactly
// like the reference's Object.keys allowlists: a request carrying an unmodelled
// field must not be frozen as if the field did not exist.
func rrctxRequest(request map[string]any) (rrctxRequestValues, error) {
	var values rrctxRequestValues
	if request == nil {
		return values, ErrReviewRevisionContextInvalid
	}
	for key := range request {
		switch key {
		case "operation", "draftId", "expectedDraft", "reviewSourceId",
			"confirmedReviewContent", "reviewCycleId", "expectedMergedHash",
			"authorInputs", "uiLocale":
		default:
			return values, ErrReviewRevisionContextInvalid
		}
	}
	operation, ok := request["operation"].(string)
	if !ok {
		return values, ErrReviewRevisionContextInvalid
	}
	switch operation {
	case ReviewRevisionOperationReviewChapter, ReviewRevisionOperationRefineDraft,
		ReviewRevisionOperationRefineFromReview:
	default:
		return values, ErrReviewRevisionContextInvalid
	}
	values.operation = operation
	draftID, ok := rrInteger(request["draftId"], 1)
	if !ok {
		return values, ErrReviewRevisionContextInvalid
	}
	values.draftID = draftID
	locale, ok := request["uiLocale"].(string)
	if !ok || (locale != ReviewRevisionLocaleZH && locale != ReviewRevisionLocaleEN) {
		return values, ErrReviewRevisionContextInvalid
	}
	values.uiLocale = locale
	expectedValue, present := request["expectedDraft"]
	if !present || expectedValue == nil {
		return values, ErrReviewRevisionContextInvalid
	}
	expected, err := rrctxExpectedDraftFrom(expectedValue)
	if err != nil {
		return values, err
	}
	values.expected = expected
	inputs, err := rrctxAuthorInputs(operation, request["authorInputs"])
	if err != nil {
		return values, err
	}
	values.authorInputs = inputs
	_, hasSource := request["reviewSourceId"]
	_, hasContent := request["confirmedReviewContent"]
	// :50 only refine-from-review may carry a confirmation.
	if operation != ReviewRevisionOperationRefineFromReview && (hasSource || hasContent) {
		return values, ErrReviewRevisionContextInvalid
	}
	_, hasCycle := request["reviewCycleId"]
	_, hasMerged := request["expectedMergedHash"]
	values.recheckRequested = hasCycle || hasMerged
	if values.recheckRequested {
		cycle, cycleOK := request["reviewCycleId"].(string)
		// The expected merged hash is only type-checked here; the recheck planner
		// is what compares it (review-revision-context.ts:53).
		_, mergedOK := request["expectedMergedHash"].(string)
		// :52-53 a recheck is review-chapter only, with both strings present.
		if operation != ReviewRevisionOperationReviewChapter || !cycleOK ||
			summaryTrim(cycle) == "" || !mergedOK {
			return values, ErrReviewRevisionContextInvalid
		}
	}
	return values, nil
}

func rrctxExpectedDraftFrom(value any) (rrctxExpectedDraft, error) {
	var expected rrctxExpectedDraft
	object, ok := value.(map[string]any)
	if !ok {
		return expected, ErrReviewRevisionContextInvalid
	}
	for key := range object {
		switch key {
		case "chapterNumber", "version", "status", "contentHash":
		default:
			return expected, ErrReviewRevisionContextInvalid
		}
	}
	// The reference only deep-equals expectedDraft at :58-59, so a well-typed
	// field that disagrees is a source change, not an invalid request.
	expected.chapter, expected.chapterOK = rrInteger(object["chapterNumber"], -9007199254740991)
	expected.version, expected.versionOK = rrInteger(object["version"], -9007199254740991)
	expected.status, expected.statusOK = object["status"].(string)
	expected.contentHash, expected.hashOK = object["contentHash"].(string)
	return expected, nil
}

// rrctxAuthorInputs is :46-49. The per-operation allowlist is exact: a
// review-chapter may carry only review-focus, a refine-draft only user-prompt and
// merged-guidance, and refine-from-review nothing at all.
func rrctxAuthorInputs(operation string, value any) ([]any, error) {
	var allowed []string
	switch operation {
	case ReviewRevisionOperationRefineDraft:
		allowed = []string{"user-prompt", "merged-guidance"}
	case ReviewRevisionOperationReviewChapter:
		allowed = []string{"review-focus"}
	}
	items, ok := value.([]any)
	if !ok || len(items) > 2 {
		return nil, ErrReviewRevisionContextInvalid
	}
	out := make([]any, 0, len(items))
	ids := make([]any, 0, len(items))
	for _, item := range items {
		object, ok := item.(map[string]any)
		if !ok {
			return nil, ErrReviewRevisionContextInvalid
		}
		for key := range object {
			if key != "id" && key != "text" {
				return nil, ErrReviewRevisionContextInvalid
			}
		}
		id, idOK := object["id"].(string)
		text, textOK := object["text"].(string)
		if !idOK || !textOK {
			return nil, ErrReviewRevisionContextInvalid
		}
		// An operation with no allowlist is one this lane cannot capture at all:
		// refine-from-review is refused at :511-513 because the confirmation codec
		// is not ported. The reference validates that confirmation (:77-91) long
		// before it builds author inputs (:146-153), so refusing the inputs here as
		// well would publish CONTEXT_INVALID for a request the reference refuses as
		// unavailable - two codes for one request, decided by which guard happens to
		// run first. The capture's own refusal decides it instead.
		if allowed != nil {
			admitted := false
			for _, candidate := range allowed {
				if id == candidate {
					admitted = true
				}
			}
			if !admitted {
				return nil, ErrReviewRevisionContextInvalid
			}
		}
		for _, seen := range ids {
			if rrEqual(seen, id) {
				return nil, ErrReviewRevisionContextInvalid
			}
		}
		ids = append(ids, id)
		if len(text) > rrctxAuthorInputTextLimit {
			return nil, ErrReviewRevisionContextInvalid
		}
		// The reference structuredClone preserves the caller's own key order.
		// Inbound object order is not observable in Go, and the allowlist above
		// admits only these two names, so the frozen literal is ordered the way
		// the reference's public shape declares it.
		out = append(out, plotObject{{"id", id}, {"text", text}})
	}
	return out, nil
}

// rrctxCapture is the transaction body of captureReviewRevisionContext (:54-154).
func rrctxCapture(ctx context.Context, conn *sql.Conn, projectID string,
	values rrctxRequestValues) (*FrozenReviewRevisionContext, error) {
	// :55-56
	rows, err := rrMaps(ctx, conn, `SELECT d.id,d.chapter_number AS chapterNumber,d.version,d.status,c.body AS content
 FROM drafts d JOIN contents c ON c.id=d.content_id WHERE d.id=?`, values.draftID)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		// :60 `!source` shares the changed-source refusal.
		return nil, ErrReviewRevisionSourceChanged
	}
	source := rows[0]
	content, ok := source["content"].(string)
	if !ok || !summarySupportedText(content) {
		// better-sqlite3 would hand the caller a Buffer and textHash would hash its
		// bytes; prose is TEXT under the admitted schema, so a BLOB here cannot be
		// re-proved as the author's selection.
		return nil, ErrReviewRevisionSourceChanged
	}
	// :57
	latest, err := rrMaps(ctx, conn, `SELECT id FROM drafts WHERE chapter_number=? AND status IN ('draft','revised','reviewed','finalized')
 ORDER BY version DESC,id DESC LIMIT 1`, source["chapterNumber"])
	if err != nil {
		return nil, err
	}
	sourceHash := summaryHash(content)
	// :58-60
	if !rrEqual(source["id"], values.draftID) || len(latest) != 1 || !rrEqual(latest[0]["id"], source["id"]) ||
		!values.expected.chapterOK || !values.expected.versionOK || !values.expected.statusOK ||
		!values.expected.hashOK ||
		!rrEqual(source["chapterNumber"], values.expected.chapter) ||
		!rrEqual(source["version"], values.expected.version) ||
		!rrEqual(source["status"], values.expected.status) ||
		sourceHash != values.expected.contentHash {
		return nil, ErrReviewRevisionSourceChanged
	}
	// :61-67 the recheck arm needs ReviewCycleRepository.planRecheck. Refuse rather
	// than freeze a context that cannot carry the recheck receipt.
	if values.recheckRequested {
		return nil, ErrReviewRevisionContextRecheckUnavailable
	}
	// :68-69
	core, present, err := rrctxProjectCore(ctx, conn)
	if err != nil {
		return nil, err
	}
	if !present {
		return nil, ErrReviewRevisionContextInvalid
	}
	// :76-91 refine-from-review needs the human-confirmed review snapshot codec
	// and ReviewRepository.getFull. Refuse; never freeze a confirmation the
	// verifier could not re-prove.
	if values.operation == ReviewRevisionOperationRefineFromReview {
		return nil, ErrReviewRevisionContextConfirmationUnavailable
	}
	chapter, chapterOK := rrInteger(source["chapterNumber"], 1)
	if !chapterOK {
		return nil, ErrReviewRevisionContextInvalid
	}
	// :92-95
	blueprintAll, err := blueprintRows(ctx, conn, "ORDER BY chapter_number ASC")
	if err != nil {
		return nil, err
	}
	blueprints := make([]any, 0, rrctxBlueprintWindow+1)
	var current plotObject
	hasCurrent := false
	for _, row := range blueprintAll {
		number, ok := rrInteger(row["chapterNumber"], -9007199254740991)
		if !ok || number < chapter || number > chapter+rrctxBlueprintWindow {
			continue
		}
		object, err := rrctxBlueprint(row)
		if err != nil {
			return nil, err
		}
		blueprints = append(blueprints, object)
		if number == chapter {
			current, hasCurrent = object, true
		}
	}
	// :96
	projections, err := rrctxContinuityBefore(ctx, conn, chapter)
	if err != nil {
		return nil, err
	}
	// :97-98
	historyRows, err := rrMaps(ctx, conn, `SELECT d.id,d.chapter_number,d.version,c.body,o.finalization_id,o.chapter_title,o.content_hash,o.content_snapshot
 FROM drafts d JOIN contents c ON c.id=d.content_id LEFT JOIN finalization_outbox o ON o.draft_id=d.id
 WHERE d.status='finalized' AND d.chapter_number < ? AND NOT EXISTS (SELECT 1 FROM drafts n WHERE n.chapter_number=d.chapter_number AND n.status='finalized' AND (n.version>d.version OR n.version=d.version AND n.id>d.id))
 ORDER BY d.chapter_number,d.id`, chapter)
	if err != nil {
		return nil, err
	}
	history := make([]any, 0, len(historyRows))
	historyIDs := make([]int64, 0, len(historyRows))
	for _, row := range historyRows {
		entry, draftID, err := rrctxHistoryEntry(projectID, projections, row)
		if err != nil {
			return nil, err
		}
		history = append(history, entry)
		historyIDs = append(historyIDs, draftID)
	}
	// :114-131
	predecessor, err := rrctxPredecessor(ctx, conn, projectID, values.draftID, chapter, historyIDs)
	if err != nil {
		return nil, err
	}
	// :132-133 CharacterRepository.getAll re-filtered by the active roster.
	cards, err := charactersAll(ctx, conn)
	if err != nil {
		return nil, err
	}
	activeRows, err := rrMaps(ctx, conn, "SELECT character_id FROM characters WHERE retired=0")
	if err != nil {
		return nil, err
	}
	active := make(map[string]bool, len(activeRows))
	for _, row := range activeRows {
		if id, ok := row["character_id"].(string); ok {
			active[id] = true
		}
	}
	authorCards := make([]map[string]any, 0, len(cards))
	for _, card := range cards {
		id, ok := card["characterId"].(string)
		if ok && active[id] {
			authorCards = append(authorCards, card)
		}
	}
	language, _ := core["writingLanguage"].(string)
	characterStates, err := rrctxCharacterStates(authorCards, language)
	if err != nil {
		return nil, err
	}
	worldbuilding, ok := core["worldbuilding"].(string)
	if !ok {
		// :149 the verifier requires this field to be a string.
		return nil, ErrReviewRevisionContextWorldbuildingInvalid
	}
	// :150-151
	frozenGoals, err := rrctxFreezeGoals(chapter, current, hasCurrent, core, authorCards)
	if err != nil {
		return nil, err
	}
	// :152
	preflight, err := rrctxPreflightFindings(ctx, conn, history, current, hasCurrent)
	if err != nil {
		return nil, err
	}
	// :146-153 literal key order. confirmation and recheck are omitted, never
	// null: the verifier requires both to be absent
	// (reviewcycle_effect.go:196-197).
	frozen := plotObject{
		{"version", int64(1)},
		{"operation", values.operation},
		{"source", plotObject{
			{"id", source["id"]},
			{"chapterNumber", source["chapterNumber"]},
			{"version", source["version"]},
			{"status", source["status"]},
			{"content", content},
		}},
		{"sourceHash", sourceHash},
		{"config", rrctxConfig(core)},
		{"writingLanguage", language},
		{"uiLocale", values.uiLocale},
		{"authorInputs", values.authorInputs},
		{"characterStates", characterStates},
		{"worldbuilding", worldbuilding},
		{"history", history},
		{"blueprints", blueprints},
	}
	if predecessor != nil {
		frozen = append(frozen, plotField{"predecessor", predecessor})
	}
	frozen = append(frozen,
		plotField{"frozenGoals", frozenGoals},
		plotField{"preflightFindings", preflight})
	text, err := plotJSON(frozen)
	if err != nil {
		return nil, err
	}
	// The digest the verifier recomputes (reviewcycle_effect.go:189-190) is over
	// exactly this text, never over a re-encoding of the value.
	result := &FrozenReviewRevisionContext{
		Operation:     values.operation,
		DraftID:       values.draftID,
		ChapterNumber: chapter,
		SourceHash:    sourceHash,
		AuthorInputs:  values.authorInputs,
		Context:       frozen,
		Text:          text,
		ContextHash:   summaryHash(text),
	}
	if version, ok := rrInteger(source["version"], -9007199254740991); ok {
		result.Version = version
	}
	if status, ok := source["status"].(string); ok {
		result.Status = status
	}
	return result, nil
}

// rrctxProjectCore is ProjectCoreRepository.get over the caller's snapshot. It
// reproduces the repository's three read normalizers (writingLanguage,
// creativeStrategy, narrativeThreadDormantChapterThreshold) because config is
// part of the hashed text.
func rrctxProjectCore(ctx context.Context, q rrQueryer) (map[string]any, bool, error) {
	columns := make([]string, len(projectCoreFields))
	for i, field := range projectCoreFields {
		columns[i] = field.column
	}
	rows, err := rrMaps(ctx, q, "SELECT "+strings.Join(columns, ", ")+" FROM project_core WHERE id = ?", "main")
	if err != nil {
		return nil, false, err
	}
	if len(rows) == 0 {
		return nil, false, nil
	}
	data := make(map[string]any, len(projectCoreFields))
	for _, field := range projectCoreFields {
		data[field.key] = rows[0][field.column]
	}
	data["writingLanguage"] = projectCoreWritingLanguage(data["writingLanguage"])
	strategy, _ := data["creativeStrategy"].(string)
	switch strategy {
	case "auto", "fluent-drafting", "consistency-first", "deep-planning":
	default:
		data["creativeStrategy"] = "auto"
	}
	data["narrativeThreadDormantChapterThreshold"] = projectCoreDormantThreshold(data["narrativeThreadDormantChapterThreshold"])
	return data, true, nil
}

// rrctxConfig is the NovelConfig literal at :70-75, in that exact key order.
func rrctxConfig(core map[string]any) plotObject {
	return plotObject{
		{"genre", core["genre"]},
		{"subGenre", core["subGenre"]},
		{"targetAudience", core["targetAudience"]},
		{"totalChapters", core["totalChapters"]},
		{"wordsPerChapter", core["wordsPerChapter"]},
		{"writingLanguage", core["writingLanguage"]},
		{"creativeStrategy", core["creativeStrategy"]},
		{"narrativeThreadDormantChapterThreshold", core["narrativeThreadDormantChapterThreshold"]},
		{"plotStructure", core["plotStructure"]},
		{"narrativePOV", core["narrativePov"]},
		{"writingStyle", core["writingStyle"]},
		{"referenceWorks", core["referenceWorks"]},
		{"globalGuidance", core["globalGuidance"]},
		{"goldenFinger", core["goldenFinger"]},
		{"coreOutline", core["coreOutline"]},
		{"worldSetting", core["worldSetting"]},
		{"protagonistProfile", core["protagonistProfile"]},
	}
}

// rrctxBlueprint is the destructured blueprint at :93-94. notesUpdatedAt is
// deliberately dropped, exactly like the reference's destructuring.
func rrctxBlueprint(row map[string]any) (plotObject, error) {
	keyEvents, ok := row["keyEvents"].(string)
	if !ok {
		// :150 `(keyEvents ?? '').split(...)` throws on a non-string.
		return nil, ErrReviewRevisionContextBlueprintInvalid
	}
	characters, ok := row["characters"].([]any)
	if !ok {
		return nil, ErrReviewRevisionContextBlueprintInvalid
	}
	for _, value := range characters {
		if _, ok := value.(string); !ok {
			// consistency-preflight.ts:92 maps normalizedKeyPart over the array.
			return nil, ErrReviewRevisionContextBlueprintInvalid
		}
	}
	return plotObject{
		{"chapterNumber", row["chapterNumber"]},
		{"title", row["title"]},
		{"role", row["role"]},
		{"purpose", row["purpose"]},
		{"keyEvents", keyEvents},
		{"characters", characters},
		{"suspenseHook", row["suspenseHook"]},
		{"userGuidance", row["userGuidance"]},
		{"notes", row["notes"]},
	}, nil
}

// rrctxHistoryEntry is :99-113. The identity literal keeps the reference's key
// order and its two provenances: a row with a finalization receipt is
// "finalized", one without is preserved "legacy" prose.
func rrctxHistoryEntry(projectID string,
	projections []plotObject, row map[string]any) (plotObject, int64, error) {
	draftID, ok := rrInteger(row["id"], 1)
	if !ok {
		return nil, 0, ErrReviewRevisionHistoryChanged
	}
	chapter, ok := rrInteger(row["chapter_number"], -9007199254740991)
	if !ok {
		return nil, 0, ErrReviewRevisionHistoryChanged
	}
	body, ok := row["body"].(string)
	if !ok || !summarySupportedText(body) {
		return nil, 0, ErrReviewRevisionHistoryChanged
	}
	rawFinalization := row["finalization_id"]
	finalization, finalizationOK := rawFinalization.(string)
	if rawFinalization != nil && !finalizationOK {
		return nil, 0, ErrReviewRevisionHistoryChanged
	}
	contentHash := summaryHash(body)
	if phTruthy(finalization) {
		// :100-101 a receipt must still describe this exact prose.
		if !rrEqual(row["content_hash"], contentHash) || !rrEqual(row["content_snapshot"], body) {
			return nil, 0, ErrReviewRevisionHistoryChanged
		}
	}
	title := ""
	switch raw := row["chapter_title"].(type) {
	case nil:
	case string:
		title = raw
	default:
		return nil, 0, ErrReviewRevisionHistoryChanged
	}
	var identitySource plotObject
	provenance := "legacy"
	if phTruthy(finalization) {
		identitySource = plotObject{
			{"draftId", draftID},
			{"finalizationId", finalization},
			{"chapterNumber", chapter},
			{"contentHash", contentHash},
		}
		provenance = "finalized"
	}
	// :108-110 ReviewMaterialIdentity, in literal key order.
	identity := plotObject{
		{"projectId", projectID},
		{"sourceId", "finalized:" + phNumberString(draftID)},
		{"revision", draftID},
		{"contentHash", contentHash},
		{"provenance", provenance},
	}
	entry := plotObject{
		{"draftId", draftID},
		{"chapterNumber", chapter},
		{"chapterTitle", title},
		{"content", body},
		{"identity", identity},
	}
	if identitySource != nil {
		entry = append(entry, plotField{"source", identitySource})
	}
	// :104 `projections.find(item => item.draftId === row.id && item.sourceStatus
	// === 'current' && isDeepStrictEqual(item.source, sourceIdentity))`. A row
	// without a receipt has sourceIdentity undefined, and such a projection is
	// always 'legacy' (summary-repository.ts:768), so it can never match.
	if identitySource != nil {
		for _, projection := range projections {
			if !rrEqual(rrctxObject(projection, "draftId"), draftID) ||
				rrctxObject(projection, "sourceStatus") != "current" {
				continue
			}
			projected, ok := rrctxObject(projection, "source").(plotObject)
			if ok && rrctxSameSource(projected, identitySource) {
				entry = append(entry, plotField{"projection", projection})
				break
			}
		}
	}
	return entry, draftID, nil
}

func rrctxSameSource(left, right plotObject) bool {
	for _, key := range []string{"draftId", "finalizationId", "chapterNumber", "contentHash"} {
		if !rrEqual(rrctxObject(left, key), rrctxObject(right, key)) {
			return false
		}
	}
	return true
}

// rrctxPredecessor is :114-131. The draft records the author's selected
// predecessor; a newer row is never substituted for it.
func rrctxPredecessor(ctx context.Context, conn *sql.Conn, projectID string, draftID int64,
	chapter int64, historyIDs []int64) (plotObject, error) {
	rows, err := rrMaps(ctx, conn, "SELECT source_dependencies FROM drafts WHERE id=?", draftID)
	if err != nil {
		return nil, err
	}
	if len(rows) != 1 {
		return nil, ErrReviewRevisionHistoryChanged
	}
	raw, ok := rows[0]["source_dependencies"].(string)
	if !ok {
		return nil, ErrReviewRevisionHistoryChanged
	}
	// The reference JSON.parses the column and refuses a non-array (:115-116).
	// parseDependencies (dependencies.go:13-72) is this package's port of that
	// ledger shape; it is stricter about an unreadable entry, so a malformed
	// dependency list is a history change rather than a partial predecessor.
	dependencies, ok := parseDependencies(sql.NullString{String: raw, Valid: true})
	if !ok {
		return nil, ErrReviewRevisionHistoryChanged
	}
	var predecessor plotObject
	for _, dependency := range dependencies {
		if dependency.Kind != "" && dependency.Kind != "candidate" {
			continue
		}
		if dependency.DraftID < 1 || !summaryHashValid(dependency.ContentHash) {
			return nil, ErrReviewRevisionHistoryChanged
		}
		found, err := rrMaps(ctx, conn, `SELECT d.id,d.chapter_number,d.version,c.body
 FROM drafts d JOIN contents c ON c.id=d.content_id WHERE d.id=?`, dependency.DraftID)
		if err != nil {
			return nil, err
		}
		if len(found) != 1 {
			return nil, ErrReviewRevisionHistoryChanged
		}
		body, ok := found[0]["body"].(string)
		if !ok || summaryHash(body) != dependency.ContentHash {
			return nil, ErrReviewRevisionHistoryChanged
		}
		rowChapter, ok := rrInteger(found[0]["chapter_number"], -9007199254740991)
		if !ok {
			return nil, ErrReviewRevisionHistoryChanged
		}
		if rowChapter != chapter-1 || rrctxContainsID(historyIDs, dependency.DraftID) {
			continue
		}
		if predecessor != nil {
			return nil, ErrReviewRevisionHistoryChanged
		}
		version, ok := rrInteger(found[0]["version"], -9007199254740991)
		if !ok {
			return nil, ErrReviewRevisionHistoryChanged
		}
		predecessor = plotObject{
			{"draftId", dependency.DraftID},
			{"chapterNumber", rowChapter},
			{"chapterTitle", ""},
			{"content", body},
			{"identity", plotObject{
				{"projectId", projectID},
				{"sourceId", "candidate:" + phNumberString(dependency.DraftID)},
				{"revision", version},
				{"contentHash", dependency.ContentHash},
				{"provenance", "generated"},
			}},
		}
	}
	return predecessor, nil
}

func rrctxContainsID(ids []int64, want int64) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// rrctxCharacterStates is :134-148. Only an author-provenanced non-empty field
// becomes an author state; the annotation is time-bound, not a permanent
// constraint, and the line says so in the project's own language.
func rrctxCharacterStates(cards []map[string]any, language string) (string, error) {
	lines := make([]string, 0, len(cards))
	for _, card := range cards {
		state, ok := card["currentState"].(map[string]any)
		if !ok {
			continue
		}
		provenance, _ := state["provenance"].(map[string]any)
		fields := plotObject{}
		for _, field := range characterStateFields {
			entry, ok := provenance[field].(map[string]any)
			if !ok || entry["kind"] != "author" {
				continue
			}
			value, ok := state[field].(string)
			if !ok || value == "" {
				continue
			}
			// :139 `@ch${provenance.chapterNumber}`: a missing key interpolates as
			// `undefined`, which is not the same text as a stored null.
			annotated := "undefined"
			if number, ok := entry["chapterNumber"]; ok {
				annotated = summaryJSString(number)
			}
			fields = append(fields, plotField{field, value + " @ch" + annotated})
		}
		if len(fields) == 0 {
			continue
		}
		encoded, err := plotJSON(fields)
		if err != nil {
			return "", err
		}
		name := summaryJSString(card["name"])
		fallback := "未知"
		if language == ReviewRevisionLocaleEN {
			fallback = "unknown"
		}
		// :137 `card.role || fallback`: a null role is falsy, so it takes the
		// fallback instead of being stringified.
		role, roleOK := card["role"].(string)
		if !roleOK || role == "" {
			role = fallback
		}
		if language == ReviewRevisionLocaleEN {
			lines = append(lines, name+" ("+role+") author state (time-bound to the annotated chapter, not permanent): "+encoded)
			continue
		}
		lines = append(lines, name+"（"+role+"）作者状态（按标注章节理解，非永久约束）: "+encoded)
	}
	if len(lines) == 0 {
		if language == ReviewRevisionLocaleEN {
			return "(none)", nil
		}
		return "（暂无）", nil
	}
	return strings.Join(lines, "\n"), nil
}

// rrctxFreezeGoals is freezeChapterGoals (chapter-goal-review.ts:25-40). Only the
// current chapter's explicit list boundaries are split; prose is never
// semantically rewritten, and the goal ids are positional.
func rrctxFreezeGoals(chapter int64, current plotObject, hasCurrent bool,
	core map[string]any, cards []map[string]any) (plotObject, error) {
	keyEvents := ""
	if hasCurrent {
		value, ok := rrctxObject(current, "keyEvents").(string)
		if !ok {
			return nil, ErrReviewRevisionContextBlueprintInvalid
		}
		keyEvents = value
	}
	items := []any{}
	// chapter-goal-review.ts:27 `(keyEvents ?? '').split(/\r?\n|[；;]/u).map(trim)
	// .filter(Boolean).map((text, index) => ({ id: `ch${chapterNumber}:keyEvents:
	// ${index + 1}`, text }))`
	index := int64(0)
	for _, line := range rrctxSplit(keyEvents, true) {
		text := summaryTrim(line)
		if text == "" {
			continue
		}
		index++
		items = append(items, plotObject{
			{"id", "ch" + phNumberString(chapter) + ":keyEvents:" + phNumberString(index)},
			{"text", text},
		})
	}
	worldSetting, ok := core["worldSetting"].(string)
	if !ok {
		// :151 passes core.worldSetting straight into freezeChapterGoals, whose
		// `source.split` is a TypeError on null.
		return nil, ErrReviewRevisionContextAuthorSourceInvalid
	}
	sources := make([]string, 0, len(cards)+1)
	sources = append(sources, worldSetting)
	for _, card := range cards {
		notes, ok := card["notes"].(string)
		if !ok {
			return nil, ErrReviewRevisionContextAuthorSourceInvalid
		}
		sources = append(sources, notes)
	}
	// chapter-goal-review.ts:29-33
	mustShow := make([]string, 0)
	for _, source := range sources {
		for _, line := range rrctxSplit(source, false) {
			text, ok := rrctxMustShow(line, chapter)
			if ok {
				mustShow = append(mustShow, text)
			}
		}
	}
	for i, text := range mustShow {
		items = append(items, plotObject{
			{"id", "ch" + phNumberString(chapter) + ":mustShow:" + phNumberString(int64(i+1))},
			{"text", text},
		})
	}
	coverage := "not_configured"
	if len(items) > 0 {
		coverage = "complete"
	} else if !hasCurrent {
		// chapter-goal-review.ts:37 `keyEvents === undefined` is the only arm that
		// reports 'unknown'; a present blueprint with empty key events is
		// 'not_configured'.
		coverage = "unknown"
	}
	return plotObject{
		{"chapterNumber", chapter},
		{"coverage", coverage},
		{"items", items},
	}, nil
}

// rrctxMustShow is the marker at chapter-goal-review.ts:29-32:
//
//	/^\s*【第(\d+)章必现】\s*(\S(?:.*\S)?)\s*$/u   with Number(group1) === chapter
//
// It is implemented directly instead of with a compiled pattern because JS `.`
// excludes the four line terminators while Go `.` excludes only \n, and because
// Go's \s differs from the ECMAScript set.
func rrctxMustShow(line string, chapter int64) (string, bool) {
	runes := []rune(line)
	at := 0
	for at < len(runes) && narrativeSpace(runes[at]) {
		at++
	}
	if !strings.HasPrefix(string(runes[at:]), "【第") {
		return "", false
	}
	at += 2
	start := at
	for at < len(runes) && runes[at] >= '0' && runes[at] <= '9' {
		at++
	}
	if at == start {
		return "", false
	}
	number, err := strconv.ParseInt(string(runes[start:at]), 10, 64)
	if err != nil || number != chapter {
		return "", false
	}
	if !strings.HasPrefix(string(runes[at:]), "章必现】") {
		return "", false
	}
	at += 4
	for at < len(runes) && narrativeSpace(runes[at]) {
		at++
	}
	rest := runes[at:]
	end := len(rest)
	for end > 0 && narrativeSpace(rest[end-1]) {
		end--
	}
	text := rest[:end]
	if len(text) == 0 {
		return "", false
	}
	if narrativeSpace(text[0]) {
		return "", false
	}
	for _, r := range text {
		// JS `.` does not cross a line terminator.
		if r == '\n' || r == '\r' || r == '\u2028' || r == '\u2029' {
			return "", false
		}
	}
	return string(text), true
}

// rrctxSplit splits text the way `/\r?\n|[；;]/u` does when separators is true,
// and the way `/\r?\n/u` does when it is false: a lone CR is never a separator.
func rrctxSplit(text string, separators bool) []string {
	out := make([]string, 0, 8)
	current := make([]rune, 0, len(text))
	runes := []rune(text)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		if r == '\r' && i+1 < len(runes) && runes[i+1] == '\n' {
			out = append(out, string(current))
			current = current[:0]
			i++
			continue
		}
		if r == '\n' || (separators && (r == ';' || r == '；')) {
			out = append(out, string(current))
			current = current[:0]
			continue
		}
		current = append(current, r)
	}
	return append(out, string(current))
}

// rrctxPreflightFindings is :152:
//
//	findBlueprintContinuityRisks(history.flatMap(item => item.projection ?
//	  [item.projection] : []), currentBlueprint,
//	  ConsistencyExemptionRepository.list(db))
//
// A finding is evidence, never a writing prohibition: only a structured terminal
// character state that contradicts the current blueprint's cast is reported.
func rrctxPreflightFindings(ctx context.Context, conn *sql.Conn, history []any,
	current plotObject, hasCurrent bool) ([]any, error) {
	if !hasCurrent {
		return []any{}, nil
	}
	characters, ok := rrctxObject(current, "characters").([]any)
	if !ok {
		return nil, ErrReviewRevisionContextBlueprintInvalid
	}
	cast := make(map[string]bool, len(characters))
	for _, value := range characters {
		text, ok := value.(string)
		if !ok {
			return nil, ErrReviewRevisionContextBlueprintInvalid
		}
		cast[rrctxKeyPart(text)] = true
	}
	// electron/repositories/consistency-exemption-repository.ts:9-17
	exemptionRows, err := rrMaps(ctx, conn, `SELECT stable_fact_key AS stableFactKey, reason, revoked
 FROM consistency_exemptions ORDER BY stable_fact_key`)
	if err != nil {
		return nil, err
	}
	exempted := make(map[string]bool, len(exemptionRows))
	for _, row := range exemptionRows {
		// consistency-preflight.ts:89-91 `exemptions.filter(e => !e.revoked)`
		if rrEqual(row["revoked"], int64(1)) {
			continue
		}
		if key, ok := row["stableFactKey"].(string); ok {
			exempted[key] = true
		}
	}
	findings := []any{}
	for _, item := range history {
		entry, ok := item.(plotObject)
		if !ok {
			continue
		}
		projection, ok := rrctxObject(entry, "projection").(plotObject)
		if !ok || rrctxObject(projection, "sourceStatus") != "current" {
			continue
		}
		facts, _ := rrctxObject(projection, "facts").([]any)
		for _, value := range facts {
			fact, ok := value.(plotObject)
			if !ok {
				continue
			}
			category, ok := rrctxObject(fact, "category").(string)
			if !ok || category != "character-state" {
				continue
			}
			entities, _ := rrctxObject(fact, "entities").([]any)
			statement, _ := rrctxObject(fact, "statement").(string)
			key := rrctxStableFactKey(category, rrctxObject(fact, "sourceChapter"), entities, statement)
			if exempted[key] {
				continue
			}
			subject := ""
			for _, entity := range entities {
				text, ok := entity.(string)
				if !ok {
					continue
				}
				part := rrctxKeyPart(text)
				if cast[part] && rrctxTerminalSubject(statement, part) {
					subject = part
					break
				}
			}
			if subject == "" {
				continue
			}
			findings = append(findings, plotObject{
				{"stableFactKey", key},
				{"severity", "warning"},
				{"sourceChapter", rrctxObject(fact, "sourceChapter")},
				{"evidence", rrctxObject(fact, "evidence")},
				{"issue", plotObject{
					{"zhCN", "定稿正文绑定的派生索引提示“" + subject + "”处于死亡终态，但当前蓝图仍将其列为出场角色。请对照原文确认语义。"},
					{"enUS", "A derived index bound to finalized prose marks “" + subject + "” as dead, but the current blueprint still schedules the character to appear. Confirm the meaning against the prose."},
				}},
				{"suggestion", plotObject{
					{"zhCN", "调整蓝图，或说明这是回忆、幻象等刻意安排。"},
					{"enUS", "Adjust the blueprint, or record an intentional device such as a flashback or vision."},
				}},
			})
		}
	}
	return findings, nil
}

// rrctxTerminalSubject is isExplicitTerminalSubject
// (consistency-preflight.ts:44-56). escapeRegExp(:40-42) is applied to the
// entity, and both patterns keep the ECMAScript whitespace set.
func rrctxTerminalSubject(statement, entity string) bool {
	subject := rrctxEscapeRegExp(entity)
	divider := `[` + rrctxJSWhitespaceClass + `|:：,，、—-]*`
	chinese := regexp.MustCompile(subject + divider +
		`(?:(?:已经|已|被确认(?:为)?|被证实(?:为)?)` + divider + `)?(?:死亡|身亡|牺牲|去世)`)
	english := regexp.MustCompile(`(?i)` + subject + `[` + rrctxJSWhitespaceClass + `|:,-]+` +
		`(?:(?:is|was|has(?: been)?)[` + rrctxJSWhitespaceClass + `]+)?(?:dead|died|deceased)\b`)
	return chinese.MatchString(statement) || english.MatchString(statement)
}

// rrctxEscapeRegExp is escapeRegExp (:40-42) over the same metacharacter set.
func rrctxEscapeRegExp(value string) string {
	var out strings.Builder
	for _, r := range value {
		if strings.ContainsRune(".*+?^${}()|[]\\", r) {
			out.WriteByte('\\')
		}
		out.WriteRune(r)
	}
	return out.String()
}

// rrctxStableFactKey is continuityStableFactKey (:58-76):
//
//	const identity = [fact.category, fact.sourceChapter,
//	  fact.entities.map(normalizedKeyPart).sort().join(','),
//	  normalizedKeyPart(fact.statement)].join(':')
//	FNV-1a 64 over the code points of identity, asUintN(64), then
//	`fact:${hash.toString(16).padStart(16, '0')}`
func rrctxStableFactKey(category string, sourceChapter any, entities []any, statement string) string {
	parts := make([]string, 0, len(entities))
	for _, entity := range entities {
		if text, ok := entity.(string); ok {
			parts = append(parts, rrctxKeyPart(text))
		}
	}
	// JS Array.prototype.sort compares UTF-16 code units, not UTF-8 bytes.
	sort.SliceStable(parts, func(i, j int) bool {
		return rrctxCompareUTF16(parts[i], parts[j]) < 0
	})
	identity := strings.Join([]string{
		category,
		summaryJSString(sourceChapter),
		strings.Join(parts, ","),
		rrctxKeyPart(statement),
	}, ":")
	hash := uint64(0xcbf29ce484222325)
	for _, r := range identity {
		hash ^= uint64(r)
		hash *= 0x100000001b3
	}
	return "fact:" + fmt.Sprintf("%016x", hash)
}

func rrctxCompareUTF16(left, right string) int {
	a, b := utf16.Encode([]rune(left)), utf16.Encode([]rune(right))
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	switch {
	case len(a) < len(b):
		return -1
	case len(a) > len(b):
		return 1
	}
	return 0
}

// rrctxKeyPart is normalizedKeyPart (consistency-preflight.ts:36-38):
// `value.trim().replace(/\s+/gu, ' ')`.
func rrctxKeyPart(value string) string {
	var out strings.Builder
	pending := false
	for _, r := range summaryTrim(value) {
		if narrativeSpace(r) {
			pending = true
			continue
		}
		if pending {
			out.WriteByte(' ')
			pending = false
		}
		out.WriteRune(r)
	}
	return out.String()
}

// rrctxContinuityBefore is listFinalizedContinuityBefore
// (summary-repository.ts:706-825) over the caller's snapshot:
//
//   - :709-711 a non-safe-integer target chapter is CONTINUITY_TARGET_CHAPTER_INVALID
//     (ErrSummaryTargetChapterInvalid, summary.go:20);
//   - the query is the package's existing port summaryListSQL (summary.go:493-510),
//     whose ORDER BY s.chapter_number ASC, s.draft_id ASC is the bound and order
//     the reference asserts;
//   - each row's projection is emitted in the reference's literal key order
//     (:807-824), which the hash observes even though the map form used by
//     Summary.ListBefore does not preserve it.
func rrctxContinuityBefore(ctx context.Context, conn *sql.Conn, chapter int64) ([]plotObject, error) {
	if _, ok := rrInteger(chapter, 1); !ok {
		return nil, ErrSummaryTargetChapterInvalid
	}
	out := []plotObject{}
	rows, err := rrMaps(ctx, conn, summaryListSQL, chapter)
	if err != nil {
		return nil, err
	}
	hasIdentity, err := summaryHasIdentity(ctx, conn)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		hasBound := phTruthy(row["sourceFinalizationId"]) && phTruthy(row["sourceContentHash"])
		body, bodyOK := row["currentContent"].(string)
		snapshot, snapOK := row["contentSnapshot"].(string)
		sourceCurrent := hasBound && rrEqual(row["currentFinalizationId"], row["sourceFinalizationId"]) &&
			rrEqual(row["currentContentHash"], row["sourceContentHash"]) && bodyOK && snapOK &&
			body == snapshot && summaryHash(body) == row["currentContentHash"]
		_, idOK := rrInteger(row["currentDraftId"], 1)
		fallback := idOK && row["currentDraftStatus"] == "finalized" && bodyOK && snapOK &&
			body == snapshot && summaryHash(body) == row["currentContentHash"]
		invalidated := row["staleFromChapter"] != nil &&
			narrativeNumeric(row["chapterNumber"]) >= narrativeNumeric(row["staleFromChapter"]) &&
			narrativeNumeric(row["projectionGeneration"]) < narrativeNumeric(row["currentGeneration"])
		for _, key := range []string{"continuityFacts", "characterStateCandidates"} {
			if err := summaryJSONSupported(narrativeJoinText(row[key])); err != nil {
				return nil, err
			}
		}
		for _, key := range []string{"chapterTitle", "chapterNotes", "currentContent",
			"contentSnapshot", "sourceFinalizationId", "sourceContentHash"} {
			if text, ok := row[key].(string); ok && !summarySupportedText(text) {
				return nil, ErrSummaryInputUnsupported
			}
		}
		facts := rrctxFacts(narrativeJoinText(row["continuityFacts"]), row["chapterNumber"])
		candidates := rrctxCandidates(narrativeJoinText(row["characterStateCandidates"]))
		source := plotObject{
			{"draftId", row["draftId"]},
			{"finalizationId", row["sourceFinalizationId"]},
			{"chapterNumber", row["chapterNumber"]},
			{"contentHash", row["sourceContentHash"]},
		}
		status := "current"
		if hasIdentity && sourceCurrent {
			frozen, exists, frozenErr := summaryFrozen(ctx, conn, summarySourceMap(source))
			if errors.Is(frozenErr, ErrSummaryInputUnsupported) {
				return nil, frozenErr
			}
			status = summaryIdentityStatus(frozen, exists, frozenErr,
				rrctxFlatFacts(facts), rrctxFlatCandidates(candidates))
		}
		if !hasBound {
			status = "legacy"
		} else if !sourceCurrent || invalidated {
			status = "stale"
		}
		// :807-824 literal order.
		projection := plotObject{{"draftId", row["draftId"]}}
		if !sourceCurrent && fallback {
			projection = append(projection, plotField{"currentFinalizedDraftId", row["currentDraftId"]})
		}
		projection = append(projection,
			plotField{"chapterNumber", row["chapterNumber"]},
			plotField{"chapterTitle", row["chapterTitle"]},
			plotField{"chapterNotes", row["chapterNotes"]},
			plotField{"facts", facts})
		if len(candidates) > 0 {
			projection = append(projection, plotField{"characterStateCandidates", candidates})
		}
		if hasBound {
			projection = append(projection, plotField{"source", source})
		}
		projection = append(projection, plotField{"sourceStatus", status})
		out = append(out, projection)
	}
	return out, nil
}

// summarySourceMap converts a source plotObject to the map form summaryFrozen and
// summarySameSource expect (summary.go:518-585).
func summarySourceMap(source plotObject) map[string]any {
	return map[string]any{
		"draftId":        rrctxObject(source, "draftId"),
		"finalizationId": rrctxObject(source, "finalizationId"),
		"chapterNumber":  rrctxObject(source, "chapterNumber"),
		"contentHash":    rrctxObject(source, "contentHash"),
	}
}

// rrctxFlatFacts is the map form summaryIdentityStatus reads (summary.go:586-671):
// it only inspects entities and characterRefs identity, never object order.
func rrctxFlatFacts(facts []any) []map[string]any {
	out := make([]map[string]any, 0, len(facts))
	for _, value := range facts {
		fact, ok := value.(plotObject)
		if !ok {
			continue
		}
		entities := []string{}
		if values, ok := rrctxObject(fact, "entities").([]any); ok {
			for _, entity := range values {
				if text, ok := entity.(string); ok {
					entities = append(entities, text)
				}
			}
		}
		refs := []map[string]any{}
		if values, ok := rrctxObject(fact, "characterRefs").([]any); ok {
			for _, ref := range values {
				object, ok := ref.(plotObject)
				if !ok {
					continue
				}
				refs = append(refs, map[string]any{
					"characterId":         rrctxObject(object, "characterId"),
					"displayNameSnapshot": rrctxObject(object, "displayNameSnapshot"),
				})
			}
		}
		out = append(out, map[string]any{"category": rrctxObject(fact, "category"),
			"entities": entities, "characterRefs": refs})
	}
	return out
}

// rrctxFlatCandidates is the map form summaryIdentityStatus reads
// (summary.go:631-670): candidate identity only.
func rrctxFlatCandidates(candidates []any) []map[string]any {
	out := make([]map[string]any, 0, len(candidates))
	for _, value := range candidates {
		candidate, ok := value.(plotObject)
		if !ok {
			continue
		}
		out = append(out, map[string]any{
			"characterId":   rrctxObject(candidate, "characterId"),
			"characterName": rrctxObject(candidate, "characterName"),
		})
	}
	return out
}

// rrctxFacts is parseFacts -> normalizedFacts (summary-repository.ts:104-127) with
// the stored key order preserved: every invalid fact list collapses to [] exactly
// like the reference's catch-all, but a valid one keeps the reference's literal
// key order (:113-121) and its stored characterRefs order.
func rrctxFacts(text string, chapter any) []any {
	empty := []any{}
	parsed, ok := rrctxParseJSON(text)
	if !ok {
		return empty
	}
	items, ok := parsed.([]any)
	if !ok || len(items) > 12 {
		return empty
	}
	out := make([]any, 0, len(items))
	for _, item := range items {
		object, ok := item.(plotObject)
		if !ok {
			return empty
		}
		category := summaryJSString(rrctxObject(object, "category"))
		if category != "character-state" && category != "timeline" &&
			category != "open-thread" && category != "plot" {
			return empty
		}
		if !summaryNumberEqual(rrctxObject(object, "sourceChapter"), chapter) {
			return empty
		}
		entities := []string{}
		if values, ok := rrctxObject(object, "entities").([]any); ok {
			if len(values) > 8 {
				return empty
			}
			for _, value := range values {
				entity := summaryTrim(summaryText(value))
				if entity == "" || narrativeLength(entity) > 80 {
					return empty
				}
				entities = append(entities, entity)
			}
		}
		statement := summaryTrim(summaryText(rrctxObject(object, "statement")))
		evidence := summaryTrim(summaryText(rrctxObject(object, "evidence")))
		if statement == "" || evidence == "" {
			return empty
		}
		distinct := make([]any, 0, len(entities))
		seen := map[string]bool{}
		for _, entity := range entities {
			if seen[entity] {
				continue
			}
			seen[entity] = true
			distinct = append(distinct, entity)
		}
		fact := plotObject{
			{"category", rrctxObject(object, "category")},
			{"entities", distinct},
			{"statement", statement},
			{"sourceChapter", chapter},
			{"evidence", evidence},
		}
		if refs, ok := rrctxObject(object, "characterRefs").([]any); ok {
			clean := make([]any, 0, len(refs))
			for _, value := range refs {
				ref, ok := value.(plotObject)
				if !ok {
					return empty
				}
				id, idOK := rrctxObject(ref, "characterId").(string)
				name, nameOK := rrctxObject(ref, "displayNameSnapshot").(string)
				if !idOK || summaryTrim(id) == "" || !nameOK || !seen[name] {
					return empty
				}
				clean = append(clean, plotObject{
					{"characterId", id},
					{"displayNameSnapshot", name},
				})
			}
			fact = append(fact, plotField{"characterRefs", clean})
		}
		out = append(out, fact)
	}
	return out
}

// rrctxCandidates is parseCharacterStateCandidates (summary-repository.ts:132-174)
// with the stored key order preserved. The reference spreads the stored
// evidence/rawValue/source values unchanged, so only an order-preserving decode
// can reproduce them.
func rrctxCandidates(text string) []any {
	out := []any{}
	parsed, ok := rrctxParseJSON(text)
	if !ok {
		return out
	}
	pending, ok := parsed.([]any)
	if !ok {
		if store, valid := parsed.(plotObject); valid &&
			summaryNumberEqual(rrctxObject(store, "version"), int64(2)) {
			pending, _ = rrctxObject(store, "pending").([]any)
		}
	}
	for _, input := range pending {
		candidate, ok := input.(plotObject)
		if !ok {
			continue
		}
		bound := false
		for _, key := range []string{"candidateKey", "source", "expectedFieldRevision",
			"expectedFieldValueHash"} {
			if rrctxPresent(candidate, key) {
				bound = true
			}
		}
		key, keyOK := rrctxObject(candidate, "candidateKey").(string)
		revision, revisionOK := rrInteger(rrctxObject(candidate, "expectedFieldRevision"), 0)
		// summarySource (summary.go:210-219) reads the map form; this ordered
		// parse carries plotObject, so the same test is applied to that form.
		sourceOK := rrctxSource(rrctxObject(candidate, "source"))
		if bound && (!keyOK || key == "" || !sourceOK || !revisionOK ||
			!summaryHashValid(rrctxObject(candidate, "expectedFieldValueHash"))) {
			continue
		}
		name, nameOK := rrctxObject(candidate, "characterName").(string)
		value, valueOK := rrctxObject(candidate, "value").(string)
		field := rrctxObject(candidate, "field")
		if !nameOK || summaryTrim(name) == "" || !summaryStateField(field) || !valueOK {
			continue
		}
		if !sourceOK {
			value = summaryTrim(value)
		}
		// :153-168 literal key order.
		object := plotObject{}
		if id, ok := rrctxObject(candidate, "characterId").(string); ok && id != "" {
			object = append(object, plotField{"characterId", id})
		}
		object = append(object,
			plotField{"characterName", summaryTrim(name)},
			plotField{"field", field},
			plotField{"value", value})
		if phTruthy(rrctxObject(candidate, "evidence")) {
			object = append(object, plotField{"evidence", rrctxObject(candidate, "evidence")})
		}
		for _, name := range []string{"selectionKey", "displayName"} {
			if text, ok := rrctxObject(candidate, name).(string); ok {
				object = append(object, plotField{name, text})
			}
		}
		if rawValue := rrctxObject(candidate, "rawValue"); rrctxPresent(candidate, "rawValue") {
			object = append(object, plotField{"rawValue", rawValue})
		}
		if reason := rrctxObject(candidate, "reason"); reason == "author-protected" ||
			reason == "legacy-protected" {
			object = append(object, plotField{"reason", reason})
		}
		if keyOK {
			object = append(object, plotField{"candidateKey", key})
		}
		if sourceOK {
			object = append(object, plotField{"source", rrctxObject(candidate, "source")})
		}
		if revisionOK {
			object = append(object, plotField{"expectedFieldRevision", revision})
		}
		if text, ok := rrctxObject(candidate, "expectedFieldValueHash").(string); ok {
			object = append(object, plotField{"expectedFieldValueHash", text})
		}
		out = append(out, object)
	}
	return out
}

// rrctxSource is summarySource (summary.go:210-219) over an ordered object: a
// draftId and chapterNumber that are integers of at least one, a non-empty
// finalizationId string and a 64-hex contentHash are what make a candidate bound
// to a finalized source.
func rrctxSource(value any) bool {
	object, ok := value.(plotObject)
	if !ok {
		return false
	}
	_, idOK := rrInteger(rrctxObject(object, "draftId"), 1)
	_, chapterOK := rrInteger(rrctxObject(object, "chapterNumber"), 1)
	id, textOK := rrctxObject(object, "finalizationId").(string)
	return idOK && chapterOK && textOK && id != "" && summaryHashValid(rrctxObject(object, "contentHash"))
}

// rrctxParseJSON is the order-preserving JSON.parse used for every stored JSON
// column. rosterJSONValue (character_roster.go:140-199) reproduces JavaScript's
// own key enumeration (array-index keys first, ascending), which is what
// JSON.stringify emits after JSON.parse.
func rrctxParseJSON(text string) (any, bool) {
	if err := summaryJSONSupported(text); err != nil {
		return nil, false
	}
	if !json.Valid([]byte(text)) {
		return nil, false
	}
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.UseNumber()
	value, err := rosterJSONValue(decoder)
	if err != nil {
		return nil, false
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, false
	}
	return value, true
}

func rrctxObject(object plotObject, key string) any {
	for _, field := range object {
		if field.key == key {
			return field.value
		}
	}
	return nil
}

func rrctxPresent(object plotObject, key string) bool {
	for _, field := range object {
		if field.key == key {
			return true
		}
	}
	return false
}
