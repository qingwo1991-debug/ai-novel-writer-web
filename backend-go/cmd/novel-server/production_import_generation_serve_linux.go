//go:build linux

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/generation"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/graphgen"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file serves the import generation family's entry points on top of the
// admission production_import_finalization_generation_linux.go used to refuse:
//
//	import-generation:read     served  (productionImportGenerationServe.read)
//	import-generation:execute  served  (productionImportGenerationServe.execute)
//
// The family has no begin of its own. An import run is opened by generation:begin
// with an importSlot selection, which syntheticParseSelection now admits through
// productionImportGenerationSelect only for the stage operation that owns the
// slot, and which productionImportGenerationFreeze turns into the three frozen
// manifest members the reference writes (electron/services/generation-source-binding.ts:599):
// importSlot, importSlotKey and importContext. The ledger admits those three keys
// only for the same two operations (internal/store/generation_ledger.go,
// glFamilyManifestAllowed), so a chapter-draft, an agent round or a blueprint
// directory still answers GENERATION_OPERATION_UNSUPPORTED for the binding and
// GENERATION_BEGIN_INVALID for the begin.
//
// # What is still refused, and why
//
// The blueprint stage is not served: its operation name is import-blueprints,
// which contains "blueprint", so syntheticPlanning refuses the begin and
// glPlanning refuses the binding before any import shape is looked at. That is
// the same refusal both gates answered before this change and neither is widened
// here. A slot of stage "blueprints" is still a valid shape for the read, which
// answers JSON null for it because no run can own it.
//
// # read
//
// The renderer sends {slot} and reads back an ImportGenerationRecovery or JSON
// null (src/shared/import-generation.ts:28). The recovery is the lookup the
// reference performs (electron/services/import-generation.ts:14-23): the runs of
// this project whose frozen manifest carries the slot's key, at most one of them,
// projected as {view, modelId, frozenContext}. A run whose stored context does not
// re-derive its own slot key is refused with GENERATION_IMPORT_CONTEXT_REQUIRED
// rather than handed back as the author's slot.
//
// # execute
//
// The renderer sends {handle, ordinal, task, execution} and reads back a
// MainGenerationExecuteReceipt (src/shared/import-generation.ts:29). The dispatch
// is generation:execute's own: syntheticGeneration.syntheticDispatchFrozenRun
// re-resolves the begin-time lease, rebuilds and re-verifies the frozen sources,
// parses the task the renderer rendered from its own prompt assets, prepares the
// plan and settles exactly one attempt under the nonce import:<ordinal>. The
// import half adds only the reference's own guards
// (electron/services/main-generation-owner.ts:1473-1496): the run must be an
// import run whose frozen context re-proves, the ordinal must be the run's next
// one, the stage's effect must not be sealed, every earlier attempt must have
// stopped, and the import run must still be running under the execution lease the
// request names.
//
// Nothing here authors prose. The only text dispatched is the task the renderer
// sent, exactly the authority generation:execute already accepts.

const (
	// The two sourceManifest operations this lane can freeze. Their stage
	// selections are the renderer's own literals
	// (src/services/workflows/commands/import-novel.command.ts:211,
	// src/services/workflows/commands/analyze-style.command.ts:44).
	productionImportGlobalFactsOperation  = "import-global-facts"
	productionImportWritingStyleOperation = "analyze-writing-style"

	// productionImportExecuteNoncePrefix is the reference's nonce family: a
	// begin's action nonce is import:<slotKey> (main-generation-owner.ts:424) and
	// one attempt's invocation nonce is import:<ordinal> (:1475).
	productionImportExecuteNoncePrefix = "import:"

	// Refusals. Each is the reference's own string where the reference has one.
	productionImportSlotInvalid       = "GENERATION_IMPORT_SLOT_INVALID"
	productionImportStageInvalid      = "GENERATION_IMPORT_STAGE_INVALID"
	productionImportExecutionRequired = "GENERATION_IMPORT_EXECUTION_REQUIRED"
	productionImportExecutionStale    = "GENERATION_IMPORT_EXECUTION_STALE"
	// productionImportAuthorityStale is the store's own execution-lease code
	// (internal/store/importrun.go:641-652), which the reference lets escape
	// unchanged from assertExecutionAuthority.
	productionImportAuthorityStale     = "IMPORT_RUN_EXECUTION_STALE"
	productionImportEffectSealed       = "GENERATION_IMPORT_EFFECT_SEALED"
	productionImportContextRequired    = "GENERATION_IMPORT_CONTEXT_REQUIRED"
	productionImportSlotAmbiguous      = "GENERATION_IMPORT_SLOT_AMBIGUOUS"
	productionImportModelChanged       = "GENERATION_IMPORT_MODEL_CHANGED"
	productionImportManifestRequired   = "GENERATION_IMPORT_MANIFEST_REQUIRED"
	productionImportContentChanged     = "GENERATION_IMPORT_CONTENT_CHANGED"
	productionImportContextLimit       = "GENERATION_IMPORT_CONTEXT_LIMIT"
	productionImportOrdinalInvalid     = "GENERATION_IMPORT_ORDINAL_INVALID"
	productionImportOrdinalGap         = "GENERATION_IMPORT_ORDINAL_GAP"
	productionImportPreviousIncomplete = "GENERATION_IMPORT_PREVIOUS_ATTEMPT_INCOMPLETE"
	// productionImportRequestInvalid is this lane's own envelope refusal: the
	// reference reads four fields off the request without validating its shape.
	productionImportRequestInvalid = "GENERATION_IMPORT_REQUEST_INVALID"
)

// productionImportBatchPattern is IMPORT_RUN_CHAPTER_BATCH_CHECKPOINT
// (src/shared/import-run.ts:131-146): start-end-fingerprintPrefixes.
var productionImportBatchPattern = regexp.MustCompile(`^([1-9][0-9]*)-([1-9][0-9]*)-([a-f0-9]{8}(?:\.[a-f0-9]{8})*)$`)

// productionImportStageSelection is one import stage's frozen shape: the only
// operation, prompt keys, skill stages and output that selection may carry
// (electron/services/import-generation.ts:38-42).
type productionImportStageSelection struct {
	Operation   string
	PromptKeys  []string
	SkillStages []string
	Output      string
}

// productionImportStageSelectionOf returns the admitted shape of one stage, or
// false when this lane cannot freeze it. "blueprints" is absent on purpose: the
// planning gates refuse its operation before this table is consulted, and listing
// it here would suggest a stage this lane can open.
func productionImportStageSelectionOf(stage string) (productionImportStageSelection, bool) {
	switch stage {
	case "global":
		return productionImportStageSelection{
			Operation:   productionImportGlobalFactsOperation,
			PromptKeys:  []string{"infer_novel_config_with_vectors", "infer_novel_config"},
			SkillStages: []string{"planning"},
			Output:      "structured-data",
		}, true
	case "style":
		return productionImportStageSelection{
			Operation:   productionImportWritingStyleOperation,
			PromptKeys:  []string{"analyze_writing_style"},
			SkillStages: []string{},
			Output:      "visible-text",
		}, true
	}
	return productionImportStageSelection{}, false
}

// productionImportSlot is one validated ImportGenerationSlot
// (src/shared/import-generation.ts:9-13).
type productionImportSlot struct {
	RunID   string
	Stage   string
	BatchID string
}

// productionImportBatch is one parsed chapter-batch checkpoint id.
type productionImportBatch struct {
	Start    int64
	End      int64
	Prefixes []string
}

// productionImportBatchOf parses a batch id exactly like
// parseImportRunChapterBatchCheckpointId (src/shared/import-run.ts:131).
func productionImportBatchOf(batchID string) (productionImportBatch, bool) {
	match := productionImportBatchPattern.FindStringSubmatch(batchID)
	if match == nil {
		return productionImportBatch{}, false
	}
	start, startErr := strconv.ParseInt(match[1], 10, 64)
	end, endErr := strconv.ParseInt(match[2], 10, 64)
	if startErr != nil || endErr != nil || start > 9007199254740991 || end > 9007199254740991 {
		return productionImportBatch{}, false
	}
	prefixes := strings.Split(match[3], ".")
	if end < start || int64(len(prefixes)) != end-start+1 {
		return productionImportBatch{}, false
	}
	return productionImportBatch{Start: start, End: end, Prefixes: prefixes}, true
}

// productionImportSlotOf validates one slot the way validateImportGenerationSlot
// does (electron/services/import-generation-source.ts:10-17): three keys, a
// bounded run id, one of the three stages, and the stage's own batch id rule.
func productionImportSlotOf(value any) (productionImportSlot, error) {
	invalid := errors.New(productionImportSlotInvalid)
	object := syntheticMap(value)
	if object == nil || len(object) != 3 {
		return productionImportSlot{}, invalid
	}
	for key := range object {
		switch key {
		case "runId", "stage", "batchId":
		default:
			return productionImportSlot{}, invalid
		}
	}
	slot := productionImportSlot{
		RunID:   syntheticString(object["runId"]),
		Stage:   syntheticString(object["stage"]),
		BatchID: syntheticString(object["batchId"]),
	}
	if slot.RunID == "" || len(slot.RunID) > 256 || !utf8.ValidString(slot.RunID) ||
		len(slot.BatchID) > 256 || !utf8.ValidString(slot.BatchID) {
		return productionImportSlot{}, invalid
	}
	switch slot.Stage {
	case "global", "style":
		if slot.BatchID != "done" {
			return productionImportSlot{}, invalid
		}
	case "blueprints":
		if _, ok := productionImportBatchOf(slot.BatchID); !ok {
			return productionImportSlot{}, invalid
		}
	default:
		return productionImportSlot{}, invalid
	}
	return slot, nil
}

// productionImportSlotKey is importGenerationSlotKey
// (electron/services/import-generation-source.ts:19):
// textHash(JSON.stringify([runId, stage, batchId])).
func productionImportSlotKey(slot productionImportSlot) (string, error) {
	encoded, err := graphgen.EncodeJSON([]string{slot.RunID, slot.Stage, slot.BatchID})
	if err != nil {
		return "", errors.New(productionImportSlotInvalid)
	}
	return syntheticHash(encoded), nil
}

// productionImportExecution is one ImportRunExecutionAuthority
// (src/shared/import-run.ts:74-77): exactly an owner and a safe-integer epoch.
type productionImportExecution struct {
	Owner string
	Epoch int64
}

// productionImportExecutionOf validates the execution lease the reference
// dereferences: a missing or unusable one is EXECUTION_REQUIRED, because the
// authority check it feeds refuses that shape identically.
func productionImportExecutionOf(value any) (productionImportExecution, error) {
	invalid := errors.New(productionImportExecutionRequired)
	object := syntheticMap(value)
	if object == nil || len(object) != 2 {
		return productionImportExecution{}, invalid
	}
	for key := range object {
		switch key {
		case "owner", "epoch":
		default:
			return productionImportExecution{}, invalid
		}
	}
	owner := syntheticString(object["owner"])
	if owner == "" || len(owner) > 160 || !utf8.ValidString(owner) {
		return productionImportExecution{}, invalid
	}
	epoch, ok := productionImportSafeInteger(object["epoch"])
	if !ok {
		return productionImportExecution{}, invalid
	}
	return productionImportExecution{Owner: owner, Epoch: epoch}, nil
}

// productionImportSafeInteger is Number.isSafeInteger over the wire value: only
// an integral JSON number within the safe range is accepted, so a fractional
// epoch or ordinal cannot be coerced into one.
func productionImportSafeInteger(value any) (int64, bool) {
	number, ok := syntheticFloat64(value)
	if !ok || number > 9007199254740991 || number < -9007199254740991 || float64(int64(number)) != number {
		return 0, false
	}
	return int64(number), true
}

// productionImportGenerationSelect is the begin half of the import admission. It
// runs after every other selection field has been validated, so a request that
// carries an import key answers the code it answered before this widening unless
// it is a well-formed selection of the one stage operation that owns the slot.
// Both import keys are filled here, and nothing else about the selection changes.
func productionImportGenerationSelect(selection *syntheticSelection, object map[string]any) error {
	invalid := errors.New("GENERATION_BEGIN_INVALID")
	if selection == nil {
		return invalid
	}
	rawSlot, slotPresent := object["importSlot"]
	rawExecution, executionPresent := object["importExecution"]
	if !slotPresent {
		// An execution authority with no slot is a request this lane could never
		// freeze; the reference refuses it too (GENERATION_IMPORT_SLOT_REQUIRED).
		if executionPresent {
			return invalid
		}
		return nil
	}
	slot, err := productionImportSlotOf(rawSlot)
	if err != nil {
		return invalid
	}
	stage, admitted := productionImportStageSelectionOf(slot.Stage)
	if !admitted || stage.Operation != selection.Operation {
		// The operation is what decides whether an import slot may be frozen. A
		// chapter-draft, an agent round, a blueprint directory name - any other
		// operation at all - keeps the refusal this key answered before.
		return invalid
	}
	if !executionPresent {
		return invalid
	}
	execution, err := productionImportExecutionOf(rawExecution)
	if err != nil {
		return invalid
	}
	// The stage's three shape fields are read from the request, not from the
	// selection: this admission runs before the loop that fills them, so it can
	// only judge what the renderer actually sent.
	promptKeys := syntheticSlice(object["promptKeys"])
	if len(promptKeys) != len(stage.PromptKeys) {
		return invalid
	}
	for index, key := range stage.PromptKeys {
		if syntheticString(promptKeys[index]) != key {
			return invalid
		}
	}
	skillStages := syntheticSlice(object["skillStages"])
	if len(skillStages) != len(stage.SkillStages) {
		return invalid
	}
	for index, value := range stage.SkillStages {
		if syntheticString(skillStages[index]) != value {
			return invalid
		}
	}
	if syntheticString(object["output"]) != stage.Output {
		return invalid
	}
	selection.ImportSlot = map[string]any{"runId": slot.RunID, "stage": slot.Stage, "batchId": slot.BatchID}
	selection.ImportExecution = map[string]any{"owner": execution.Owner, "epoch": execution.Epoch}
	return nil
}

// productionImportRunState is the part of the import_runs row this family reads.
type productionImportRunState struct {
	Stage                string
	Status               string
	CancelRequested      int64
	ExecutionOwner       string
	ExecutionEpoch       int64
	LeaseExpiresAt       int64
	ManifestFingerprint  string
	ManifestChapterCount int64
	ManifestWordCount    int64
}

// productionImportRunStateOf reads one import run, or nil when it does not exist.
func productionImportRunStateOf(ctx context.Context, db *sql.DB, runID string) (*productionImportRunState, error) {
	if db == nil {
		return nil, errors.New(productionImportExecutionStale)
	}
	state := productionImportRunState{}
	err := db.QueryRowContext(ctx, `SELECT stage, status, cancel_requested, execution_owner,
 execution_epoch, lease_expires_at, manifest_fingerprint, manifest_chapter_count, manifest_word_count
 FROM import_runs WHERE id=?`, runID).Scan(&state.Stage, &state.Status, &state.CancelRequested,
		&state.ExecutionOwner, &state.ExecutionEpoch, &state.LeaseExpiresAt,
		&state.ManifestFingerprint, &state.ManifestChapterCount, &state.ManifestWordCount)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &state, nil
}

// productionImportGenerationExisting answers the run the slot already owns, or
// nil when it owns none. It is the reference's find before any new root: more
// than one owned run is AMBIGUOUS, and a run frozen for another model is
// MODEL_CHANGED (electron/services/import-generation.ts:15-44).
func productionImportGenerationExisting(ctx context.Context, db *sql.DB, ledger *store.GenerationLedger, projectID string, selection syntheticSelection) (map[string]any, error) {
	if selection.ImportSlot == nil {
		return nil, nil
	}
	slot, err := productionImportSlotOf(selection.ImportSlot)
	if err != nil {
		return nil, errors.New("GENERATION_BEGIN_INVALID")
	}
	key, err := productionImportSlotKey(slot)
	if err != nil {
		return nil, errors.New("GENERATION_BEGIN_INVALID")
	}
	runID, err := productionImportFindRun(ctx, db, projectID, key)
	if err != nil {
		return nil, err
	}
	if runID == "" {
		return nil, nil
	}
	run, err := productionImportLedgerRun(ctx, ledger, runID)
	if err != nil {
		return nil, err
	}
	if run == nil {
		return nil, nil
	}
	manifest := syntheticMap(syntheticMap(run["binding"])["sourceManifest"])
	if syntheticString(manifest["operation"]) != selection.Operation {
		return nil, errors.New("GENERATION_BEGIN_INVALID")
	}
	if syntheticString(manifest["modelId"]) != selection.ModelID {
		return nil, errors.New(productionImportModelChanged)
	}
	return run, nil
}

// productionImportGenerationFreeze freezes the durable import material beside the
// slot: the run's own manifest identity and the exact chapter selection the stage
// reads (captureImportGenerationContext,
// electron/services/import-generation-source.ts:34-47). It refuses before any
// ledger row when the import run is not running this stage under the lease the
// request names, so a begin cannot freeze material the import run no longer owns.
func productionImportGenerationFreeze(ctx context.Context, db *sql.DB, selection *syntheticSelection) error {
	if selection == nil || selection.ImportSlot == nil {
		return nil
	}
	slot, err := productionImportSlotOf(selection.ImportSlot)
	if err != nil {
		return errors.New("GENERATION_BEGIN_INVALID")
	}
	key, err := productionImportSlotKey(slot)
	if err != nil {
		return errors.New("GENERATION_BEGIN_INVALID")
	}
	state, err := productionImportRunStateOf(ctx, db, slot.RunID)
	if err != nil {
		return err
	}
	if state == nil || state.Stage != slot.Stage || state.Status != "running" || state.CancelRequested != 0 {
		return errors.New(productionImportExecutionStale)
	}
	execution, err := productionImportExecutionOf(selection.ImportExecution)
	if err != nil {
		return errors.New("GENERATION_BEGIN_INVALID")
	}
	if state.ExecutionOwner != execution.Owner || state.ExecutionEpoch != execution.Epoch ||
		state.LeaseExpiresAt <= time.Now().UnixMilli() {
		return errors.New(productionImportAuthorityStale)
	}
	if !syntheticHashPattern.MatchString(state.ManifestFingerprint) {
		return errors.New(productionImportManifestRequired)
	}
	chapters, err := productionImportChapters(ctx, db, slot)
	if err != nil {
		return err
	}
	// The frozen chapter list is the one bounded part of the context
	// (electron/services/import-generation-source.ts:45), so a stage whose
	// selection is too large is refused instead of frozen into a binding.
	encoded, err := syntheticCanonical(chapters)
	if err != nil || len(encoded) > 8*1024*1024 {
		return errors.New(productionImportContextLimit)
	}
	core, err := store.NewProjectCore(db).Get(ctx)
	if err != nil || core == nil {
		return errors.New(productionImportContextLimit)
	}
	slotValue := map[string]any{"runId": slot.RunID, "stage": slot.Stage, "batchId": slot.BatchID}
	selection.ImportSlot = slotValue
	selection.ImportSlotKey = key
	selection.ImportContext = map[string]any{
		"slot":                slotValue,
		"manifestFingerprint": state.ManifestFingerprint,
		"totalChapters":       state.ManifestChapterCount,
		"totalWords":          state.ManifestWordCount,
		"core":                core,
		"prompts":             map[string]any{},
		"chapters":            chapters,
	}
	// The reference rewrites the action nonce to the slot's own before opening the
	// root (main-generation-owner.ts:424), which is what makes one slot one root.
	selection.UIActionNonce = productionImportExecuteNoncePrefix + key
	return nil
}

// productionImportChapter is one frozen chapter of the import context.
type productionImportChapter struct {
	Number      int64
	Title       string
	Content     string
	Fingerprint string
}

// productionImportChapters reads the chapters one stage captures, verifies each
// one against its stored fingerprint, and projects the frozen list. The
// selection rule is captureImportGenerationContext's own
// (electron/services/import-generation-source.ts:29-33): the first and last for
// the global stage, and the first three plus the last two for the style stage.
func productionImportChapters(ctx context.Context, db *sql.DB, slot productionImportSlot) ([]any, error) {
	if db == nil {
		return nil, errors.New(productionImportContentChanged)
	}
	rows, err := db.QueryContext(ctx, `SELECT chapter_number, title, content_snapshot, content_fingerprint
 FROM import_run_chapters WHERE run_id=? ORDER BY chapter_number`, slot.RunID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	all := make([]productionImportChapter, 0, 8)
	for rows.Next() {
		chapter := productionImportChapter{}
		if err := rows.Scan(&chapter.Number, &chapter.Title, &chapter.Content, &chapter.Fingerprint); err != nil {
			return nil, err
		}
		all = append(all, chapter)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	selected := make([]productionImportChapter, 0, len(all))
	switch slot.Stage {
	case "global":
		if len(all) > 0 {
			selected = append(selected, all[0])
			if last := all[len(all)-1]; last.Number != all[0].Number {
				selected = append(selected, last)
			}
		}
	case "style":
		for index, chapter := range all {
			if len(all) <= 5 || index < 3 || index >= len(all)-2 {
				selected = append(selected, chapter)
			}
		}
	default:
		return nil, errors.New(productionImportStageInvalid)
	}
	if len(selected) == 0 {
		return nil, errors.New(productionImportContentChanged)
	}
	out := make([]any, 0, len(selected))
	for _, chapter := range selected {
		if syntheticHash(chapter.Content) != chapter.Fingerprint {
			return nil, errors.New(productionImportContentChanged)
		}
		out = append(out, map[string]any{
			"number":             chapter.Number,
			"title":              chapter.Title,
			"content":            chapter.Content,
			"contentFingerprint": chapter.Fingerprint,
			"wordCount":          int64(generation.CountDraftUnits(chapter.Content)),
		})
	}
	return out, nil
}

// productionImportGenerationServe serves the two names over the one generation
// owner generation:execute already dispatches through, so admission, the
// registry, the executor and the single-writer fence are the lane's own.
type productionImportGenerationServe struct {
	owner *productionGeneration
}

func newProductionImportGenerationServe(owner *productionGeneration) *productionImportGenerationServe {
	return &productionImportGenerationServe{owner: owner}
}

func (i *productionImportGenerationServe) ready() bool {
	return i != nil && i.owner != nil && i.owner.open != nil && i.owner.inner != nil
}

// Bind installs the two served names. A nil or closed owner is not an error: both
// keep failing closed at GO_OPERATION_NOT_MIGRATED, exactly like the sibling
// binders. It is wired from production_open_linux.go's owner block, beside
// newProductionFinalizationServe(owner).Bind(server).
func (i *productionImportGenerationServe) Bind(server *transport.Server) error {
	if server == nil {
		return errors.New("TRANSPORT_SERVER_REQUIRED")
	}
	if !i.ready() {
		return nil
	}
	if err := server.Bind(productionImportGenerationRead, productionImportGenerationReadChannel(i.read)); err != nil {
		return err
	}
	return server.Bind(productionImportGenerationExecute,
		productionGenerationMutation(1, func(ctx context.Context, caller transport.Caller, expected map[string]any, args []any) (any, error) {
			return i.execute(ctx, caller, expected, syntheticMap(args[0]))
		}))
}

// productionImportGenerationReadChannel is the read half's wire shape: the
// channel's one request plus the frozen three-key project context at args[1], and
// the read family's symbolic refusal mapping.
func productionImportGenerationReadChannel(call func(context.Context, transport.Caller, map[string]any, map[string]any) (any, error)) transport.Operation {
	return func(ctx context.Context, caller transport.Caller, args []any) (any, error) {
		expected, err := productionEditorInlineSession(args)
		if err != nil {
			return productionGraphReadFailure(err)
		}
		if ctx == nil {
			return productionGraphReadFailure(security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS"))
		}
		value, err := call(ctx, caller, expected, syntheticMap(args[0]))
		if err != nil {
			return productionGraphReadFailure(err)
		}
		return value, nil
	}
}

// productionImportReadRequest is the read's envelope: `{ slot }` and nothing else.
func productionImportReadRequest(request map[string]any) (productionImportSlot, error) {
	if len(request) != 1 {
		return productionImportSlot{}, errors.New(productionImportSlotInvalid)
	}
	raw, present := request["slot"]
	if !present {
		return productionImportSlot{}, errors.New(productionImportSlotInvalid)
	}
	return productionImportSlotOf(raw)
}

// productionImportFindRun is the reference's find
// (electron/services/import-generation.ts:15): the run of this project whose
// frozen manifest carries the slot key, or "" when none does. Two owned runs for
// one slot is the reference's AMBIGUOUS: neither can be the one the author holds.
func productionImportFindRun(ctx context.Context, db *sql.DB, projectID, key string) (string, error) {
	if db == nil {
		return "", errors.New(productionImportContextRequired)
	}
	rows, err := db.QueryContext(ctx, `SELECT run_id, json_extract(binding_json,'$.projectId') FROM generation_runs
 WHERE json_extract(binding_json,'$.sourceManifest.importSlotKey')=? ORDER BY rowid`, key)
	if err != nil {
		return "", err
	}
	defer func() { _ = rows.Close() }()
	found := ""
	owned := 0
	for rows.Next() {
		var runID string
		var project sql.NullString
		if err := rows.Scan(&runID, &project); err != nil {
			return "", err
		}
		// The reference filters the rows by projectId after the key query, so a
		// foreign row is invisible rather than a conflict.
		if !project.Valid || project.String != projectID {
			continue
		}
		owned++
		if owned > 1 {
			return "", errors.New(productionImportSlotAmbiguous)
		}
		found = runID
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	return found, nil
}

// productionImportStoredBinding returns the exact stored binding text of one run,
// the same byte-exact read the finalization family performs for its own members.
func productionImportStoredBinding(ctx context.Context, db *sql.DB, runID string) ([]byte, error) {
	if db == nil {
		return nil, errors.New("GENERATION_RUN_NOT_FOUND")
	}
	var stored string
	err := db.QueryRowContext(ctx, `SELECT binding_json FROM generation_runs WHERE run_id=?`, runID).Scan(&stored)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errors.New("GENERATION_RUN_NOT_FOUND")
		}
		return nil, err
	}
	return []byte(stored), nil
}

// productionImportLedgerRun reads one run through the ledger, so the view is
// projected from the same rows the rest of the lane reads.
func productionImportLedgerRun(ctx context.Context, ledger *store.GenerationLedger, runID string) (map[string]any, error) {
	args, err := syntheticArgs(runID)
	if err != nil {
		return nil, errors.New("GENERATION_RUN_NOT_FOUND")
	}
	value, err := ledger.CallJSON(ctx, "get", args)
	if err != nil {
		return nil, err
	}
	return syntheticMap(value), nil
}

// productionImportDecodeContext decodes frozen JSON with number literals kept, so
// the context the read answers is the one that was frozen.
func productionImportDecodeContext(raw []byte) (map[string]any, error) {
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, errors.New(productionImportContextRequired)
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New(productionImportContextRequired)
	}
	return object, nil
}

// productionImportRunContext proves a run is an import run of the slot this
// request names, and returns that slot. It is the reference's context(run)
// (electron/services/import-generation.ts:20-23) plus the stage operation the
// frozen manifest must name.
func productionImportRunContext(manifest map[string]any) (productionImportSlot, error) {
	slot, err := productionImportSlotOf(manifest["importSlot"])
	if err != nil {
		return productionImportSlot{}, errors.New(productionImportContextRequired)
	}
	key, err := productionImportSlotKey(slot)
	if err != nil || key != syntheticString(manifest["importSlotKey"]) {
		return productionImportSlot{}, errors.New(productionImportContextRequired)
	}
	contextValue := syntheticMap(manifest["importContext"])
	if contextValue == nil {
		return productionImportSlot{}, errors.New(productionImportContextRequired)
	}
	inner, err := productionImportSlotOf(contextValue["slot"])
	if err != nil {
		return productionImportSlot{}, errors.New(productionImportContextRequired)
	}
	innerKey, err := productionImportSlotKey(inner)
	if err != nil || innerKey != key {
		return productionImportSlot{}, errors.New(productionImportContextRequired)
	}
	stage, admitted := productionImportStageSelectionOf(slot.Stage)
	if !admitted || syntheticString(manifest["operation"]) != stage.Operation {
		return productionImportSlot{}, errors.New(productionImportStageInvalid)
	}
	return slot, nil
}

// productionImportAssertExecution re-proves the import run's own execution lease
// (electron/services/import-generation.ts:25-28): the authority first
// (IMPORT_RUN_EXECUTION_STALE, the store's code), then the stage, status and
// cancellation of the row (GENERATION_IMPORT_EXECUTION_STALE).
func productionImportAssertExecution(ctx context.Context, db *sql.DB, slot productionImportSlot, execution productionImportExecution) error {
	state, err := productionImportRunStateOf(ctx, db, slot.RunID)
	if err != nil {
		return err
	}
	if state == nil || state.Status != "running" || state.ExecutionOwner != execution.Owner ||
		state.ExecutionEpoch != execution.Epoch || state.LeaseExpiresAt <= time.Now().UnixMilli() {
		return errors.New(productionImportAuthorityStale)
	}
	if state.Stage != slot.Stage || state.CancelRequested != 0 {
		return errors.New(productionImportExecutionStale)
	}
	return nil
}

// productionImportAssertMutable refuses a stage whose effect receipt already
// exists (electron/services/import-generation.ts:29-33): a sealed stage owns its
// effects, so dispatching it again could apply them twice.
func productionImportAssertMutable(ctx context.Context, db *sql.DB, slot productionImportSlot) error {
	receipt, err := store.NewImportRuns(db).GetEffectReceipt(ctx, slot.RunID, slot.Stage, slot.BatchID)
	if err != nil {
		return err
	}
	if receipt != nil {
		return errors.New(productionImportEffectSealed)
	}
	return nil
}

// productionImportAttemptState counts the run's attempts and reports whether this
// exact nonce was already spent, which is the reference's replay branch
// (main-generation-owner.ts:1477-1482) without its receipt projection.
func productionImportAttemptState(ctx context.Context, db *sql.DB, runID, nonce string) (int64, bool, error) {
	if db == nil {
		return 0, false, errors.New(productionImportOrdinalGap)
	}
	var count int64
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM generation_attempts WHERE run_id=?`, runID).Scan(&count); err != nil {
		return 0, false, err
	}
	var spent int64
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM generation_attempts WHERE run_id=? AND invocation_nonce=?`, runID, nonce).Scan(&spent); err != nil {
		return 0, false, err
	}
	return count, spent > 0, nil
}

// productionImportPreviousStopped is the reference's previous-attempt guard
// (main-generation-owner.ts:1492-1493): every attempt of this run must have
// settled with a stop, or the next ordinal would build on a broken chain.
func productionImportPreviousStopped(ctx context.Context, db *sql.DB, runID string) (bool, error) {
	if db == nil {
		return false, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT usage_receipt_json FROM generation_attempts WHERE run_id=? ORDER BY rowid`, runID)
	if err != nil {
		return false, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var raw sql.NullString
		if err := rows.Scan(&raw); err != nil {
			return false, err
		}
		if !raw.Valid {
			return false, nil
		}
		var receipt map[string]any
		if err := json.Unmarshal([]byte(raw.String), &receipt); err != nil {
			return false, nil
		}
		result := syntheticMap(receipt["result"])
		if result == nil || syntheticString(result["finishReason"]) != "stop" {
			return false, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	return true, nil
}

// productionImportExecuteRequest is the execute envelope
// (src/shared/import-generation.ts:29): {handle, ordinal, task, execution}.
type productionImportExecuteRequest struct {
	Handle    syntheticHandle
	Ordinal   int64
	Task      any
	Execution productionImportExecution
}

// productionImportExecuteRequestOf validates that envelope. A missing or unusable
// execution lease is the reference's own EXECUTION_REQUIRED; a handle that is not
// four non-empty strings is the lane's run-identity refusal.
func productionImportExecuteRequestOf(value map[string]any) (productionImportExecuteRequest, error) {
	invalid := errors.New(productionImportRequestInvalid)
	if value == nil {
		return productionImportExecuteRequest{}, invalid
	}
	for key := range value {
		switch key {
		case "handle", "ordinal", "task", "execution":
		default:
			return productionImportExecuteRequest{}, invalid
		}
	}
	handle, err := syntheticParseHandle(value["handle"])
	if err != nil {
		return productionImportExecuteRequest{}, err
	}
	task, present := value["task"]
	if !present || task == nil {
		return productionImportExecuteRequest{}, invalid
	}
	ordinal, ok := productionImportSafeInteger(value["ordinal"])
	if !ok || ordinal < 0 {
		return productionImportExecuteRequest{}, errors.New(productionImportOrdinalInvalid)
	}
	rawExecution, present := value["execution"]
	if !present {
		return productionImportExecuteRequest{}, errors.New(productionImportExecutionRequired)
	}
	execution, err := productionImportExecutionOf(rawExecution)
	if err != nil {
		return productionImportExecuteRequest{}, err
	}
	return productionImportExecuteRequest{Handle: handle, Ordinal: ordinal, Task: task, Execution: execution}, nil
}

// read answers import-generation:read: the run the slot owns, or JSON null when
// none does.
func (i *productionImportGenerationServe) read(ctx context.Context, caller transport.Caller, expected map[string]any, request map[string]any) (any, error) {
	if !i.ready() {
		return nil, errProductionGenerationUnavailable
	}
	slot, err := productionImportReadRequest(request)
	if err != nil {
		return nil, err
	}
	key, err := productionImportSlotKey(slot)
	if err != nil {
		return nil, errors.New(productionImportSlotInvalid)
	}
	_, scope, ledger, db, done, err := i.owner.inner.admit(ctx, caller, expected)
	if err != nil {
		return nil, err
	}
	defer done()
	runID, err := productionImportFindRun(ctx, db, scope.Project, key)
	if err != nil {
		return nil, err
	}
	if runID == "" {
		return nil, nil
	}
	stored, err := productionImportStoredBinding(ctx, db, runID)
	if err != nil {
		return nil, err
	}
	manifestRaw, ok := productionEditorInlineRawMember(stored, "sourceManifest")
	if !ok {
		return nil, errors.New(productionImportContextRequired)
	}
	contextRaw, ok := productionEditorInlineRawMember(manifestRaw, "importContext")
	if !ok {
		return nil, errors.New(productionImportContextRequired)
	}
	contextValue, err := productionImportDecodeContext(contextRaw)
	if err != nil {
		return nil, err
	}
	inner, err := productionImportSlotOf(contextValue["slot"])
	if err != nil {
		return nil, errors.New(productionImportContextRequired)
	}
	innerKey, err := productionImportSlotKey(inner)
	if err != nil || innerKey != key {
		return nil, errors.New(productionImportContextRequired)
	}
	run, err := productionImportLedgerRun(ctx, ledger, runID)
	if err != nil {
		return nil, err
	}
	if run == nil {
		return nil, errors.New("GENERATION_RUN_NOT_FOUND")
	}
	manifest := syntheticMap(syntheticMap(run["binding"])["sourceManifest"])
	view, err := i.owner.inner.viewOf(ctx, ledger, db, run)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"view":          view,
		"modelId":       syntheticString(manifest["modelId"]),
		"frozenContext": contextValue,
	}, nil
}

// execute answers import-generation:execute: the one dispatch of the browser's
// task over the run's frozen lease, then the settled attempt projected as the
// receipt.
func (i *productionImportGenerationServe) execute(ctx context.Context, caller transport.Caller, expected map[string]any, request map[string]any) (any, error) {
	if !i.ready() {
		return nil, errProductionGenerationUnavailable
	}
	if _, err := i.owner.open.ModelEgress(); err != nil {
		return nil, err
	}
	if i.owner.inner.executor == nil {
		return nil, errProductionGenerationExecuteUnavailable
	}
	parsed, err := productionImportExecuteRequestOf(request)
	if err != nil {
		return nil, err
	}
	contextValue, scope, ledger, db, done, err := i.owner.inner.admit(ctx, caller, expected)
	if err != nil {
		return nil, err
	}
	defer done()
	_, _, manifest, err := syntheticRequireRun(ctx, ledger, scope, parsed.Handle)
	if err != nil {
		return nil, err
	}
	slot, err := productionImportRunContext(manifest)
	if err != nil {
		return nil, err
	}
	nonce := productionImportExecuteNoncePrefix + strconv.FormatInt(parsed.Ordinal, 10)
	count, replayed, err := productionImportAttemptState(ctx, db, parsed.Handle.RunID, nonce)
	if err != nil {
		return nil, err
	}
	if !replayed {
		if count != parsed.Ordinal {
			return nil, errors.New(productionImportOrdinalGap)
		}
		if err := productionImportAssertMutable(ctx, db, slot); err != nil {
			return nil, err
		}
		stopped, err := productionImportPreviousStopped(ctx, db, parsed.Handle.RunID)
		if err != nil {
			return nil, err
		}
		if !stopped {
			return nil, errors.New(productionImportPreviousIncomplete)
		}
		if err := productionImportAssertExecution(ctx, db, slot, parsed.Execution); err != nil {
			return nil, err
		}
	}
	return i.owner.inner.syntheticDispatchFrozenRun(ctx, caller, contextValue, ledger, db, scope,
		parsed.Handle, manifest, nonce, parsed.Task)
}
