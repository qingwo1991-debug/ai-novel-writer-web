//go:build linux

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/generationowner"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/graphgen"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/prompttemplate"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file serves the finalization generation family's three remaining names.
// It is the write half of production_finalization_generation_linux.go, and it
// uses exactly the openRun/CallJSON("open") pattern the probe proved
// (production_finalization_generation_write_linux_test.go):
//
//	finalization-generation:begin     served   (productionFinalizationServe.begin)
//	finalization-generation:execute   served   (productionFinalizationServe.execute)
//	finalization-generation:commit    served   (productionFinalizationServe.commit)
//
// begin freezes the seven-member finalization context from the shipped template
// asset (internal/prompttemplate), execute dispatches the frozen task through
// the model lease and settles it, and commit writes the reference's receipt into
// the attempt's usage receipt. The template is the resolved builtin
// JSON.stringify(getBuiltinPromptTemplate(step, lang)) carried as a committed
// asset, never restated in Go source; the loader test re-reads the TypeScript
// sources (internal/prompttemplate/finalization_test.go).
//
// The handlers borrow the same generation owner editor-inline borrows
// (production_editor_inline_write_linux.go), so admission, the registry, the
// executor and the single-writer fence are the lane's own. The wiring line that
// installs this binder lives in production_open_linux.go's owner block beside
// editor-inline: `newProductionFinalizationServe(owner).Bind(server)`.

const (
	// productionFinalizationOperationNotes and ...Cards are the sourceManifest
	// operations the reference freezes (main-generation-owner.ts:1264).
	productionFinalizationOperationNotes = "finalized-chapter-notes"
	productionFinalizationOperationCards = "finalized-character-state"
	// productionFinalizationExecuteNonce is the first-request invocation nonce
	// (main-generation-owner.ts:1398).
	productionFinalizationExecuteNonce = "finalization:0"
	// Refusals, each the reference's own string.
	productionFinalizationSlotInvalid       = "GENERATION_FINALIZATION_SLOT_INVALID"
	productionFinalizationRequestInvalid    = "GENERATION_FINALIZATION_REQUEST_INVALID"
	productionFinalizationIdentityMismatch  = "GENERATION_RUN_IDENTITY_MISMATCH"
	productionFinalizationContextInvalid    = "GENERATION_FINALIZATION_CONTEXT_INVALID"
	productionFinalizationTaskInvalid       = "GENERATION_FINALIZATION_TASK_INVALID"
	productionFinalizationArtifactInvalid   = "GENERATION_FINALIZATION_ARTIFACT_INVALID"
	productionFinalizationRunMissing        = "GENERATION_RUN_MISSING"
	productionFinalizationLeaseMissing      = "GENERATION_LEASE_MISSING"
	productionFinalizationSourceChanged     = "GENERATION_SOURCE_CHANGED"
	productionFinalizationAdmissionRequired = "GENERATION_FINALIZATION_ADMISSION_REQUIRED"
	productionFinalizationEffectSealed      = "GENERATION_FINALIZATION_EFFECT_SEALED"
	productionFinalizationNotesChanged      = "GENERATION_FINALIZATION_NOTES_CHANGED"
)

// productionFinalizationServe serves the three names over the one generation
// owner, exactly like productionEditorInline does for the inline family.
type productionFinalizationServe struct {
	owner *productionGeneration
}

func newProductionFinalizationServe(owner *productionGeneration) *productionFinalizationServe {
	return &productionFinalizationServe{owner: owner}
}

func (f *productionFinalizationServe) ready() bool {
	return f != nil && f.owner != nil && f.owner.open != nil && f.owner.inner != nil
}

// Bind installs the three served names. A nil owner is not an error: the names
// keep failing closed at GO_OPERATION_NOT_MIGRATED, exactly like the sibling
// binders. It is called from production_open_linux.go's owner block.
func (f *productionFinalizationServe) Bind(server *transport.Server) error {
	if server == nil {
		return errors.New("TRANSPORT_SERVER_REQUIRED")
	}
	if !f.ready() {
		return nil
	}
	for _, bind := range []struct {
		name string
		call func(context.Context, transport.Caller, map[string]any, map[string]any) (any, error)
	}{
		{productionFinalizationBegin, f.begin},
		{productionFinalizationExecute, f.execute},
		{productionFinalizationCommit, f.commit},
	} {
		if err := server.Bind(bind.name, productionFinalizationServeChannel(bind.call)); err != nil {
			return err
		}
	}
	return nil
}

// productionFinalizationServeChannel is the wire half the three names share: the
// channel's one request plus the frozen three-key project context at args[1].
func productionFinalizationServeChannel(call func(context.Context, transport.Caller, map[string]any, map[string]any) (any, error)) transport.Operation {
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

// productionFinalizationSlot is one validated finalization slot with its key.
type productionFinalizationSlot struct {
	StepKey        string
	DraftID        int64
	ChapterNumber  int64
	FinalizationID string
	ContentHash    string
	Key            string
}

// productionFinalizationParseSlot validates one slot exactly like
// syntheticFinalizationSlotKey and returns its typed parts plus the key.
func productionFinalizationParseSlot(value any) (productionFinalizationSlot, error) {
	key, err := syntheticFinalizationSlotKey(value)
	if err != nil {
		return productionFinalizationSlot{}, errors.New(productionFinalizationSlotInvalid)
	}
	slot := syntheticMap(value)
	source := syntheticMap(slot["source"])
	draftID, draftOK := syntheticSlotID(source["draftId"])
	chapterNumber, chapterOK := syntheticSlotID(source["chapterNumber"])
	if !draftOK || !chapterOK {
		return productionFinalizationSlot{}, errors.New(productionFinalizationSlotInvalid)
	}
	return productionFinalizationSlot{
		StepKey:        syntheticString(slot["stepKey"]),
		DraftID:        draftID,
		ChapterNumber:  chapterNumber,
		FinalizationID: syntheticString(source["finalizationId"]),
		ContentHash:    syntheticString(source["contentHash"]),
		Key:            key,
	}, nil
}

// productionFinalizationSlotText renders the slot in the reference's key order:
// source {draftId, finalizationId, chapterNumber, contentHash}, then stepKey.
func productionFinalizationSlotText(slot productionFinalizationSlot) string {
	return productionEditorInlineObject(
		productionEditorInlineField{"source", productionEditorInlineObject(
			productionEditorInlineField{"draftId", strconv.FormatInt(slot.DraftID, 10)},
			productionEditorInlineField{"finalizationId", productionEditorInlineQuote(slot.FinalizationID)},
			productionEditorInlineField{"chapterNumber", strconv.FormatInt(slot.ChapterNumber, 10)},
			productionEditorInlineField{"contentHash", productionEditorInlineQuote(slot.ContentHash)},
		)},
		productionEditorInlineField{"stepKey", productionEditorInlineQuote(slot.StepKey)},
	)
}

// productionFinalizationBeginRequest is the reference's begin envelope
// (main-generation-owner.ts:1242-1244): exactly {slot, modelId, parentRootActionId?}.
func productionFinalizationBeginRequest(value map[string]any) (productionFinalizationSlot, string, string, error) {
	invalid := errors.New(productionFinalizationSlotInvalid)
	if value == nil {
		return productionFinalizationSlot{}, "", "", invalid
	}
	for key := range value {
		switch key {
		case "slot", "modelId", "parentRootActionId":
		default:
			return productionFinalizationSlot{}, "", "", invalid
		}
	}
	modelID := syntheticString(value["modelId"])
	if modelID == "" || !utf8.ValidString(modelID) {
		return productionFinalizationSlot{}, "", "", invalid
	}
	slot, err := productionFinalizationParseSlot(value["slot"])
	if err != nil {
		return productionFinalizationSlot{}, "", "", err
	}
	return slot, modelID, syntheticString(value["parentRootActionId"]), nil
}

// productionFinalizationExecuteRequest is the reference's execute envelope
// (main-generation-owner.ts:1272-1274): {handle, retryOf?, recoverFrom?}.
func productionFinalizationExecuteRequest(value map[string]any) (syntheticHandle, error) {
	if value == nil {
		return syntheticHandle{}, errors.New(productionFinalizationRequestInvalid)
	}
	for key := range value {
		switch key {
		case "handle", "retryOf", "recoverFrom":
		default:
			return syntheticHandle{}, errors.New(productionFinalizationRequestInvalid)
		}
	}
	raw := syntheticMap(value["handle"])
	if raw == nil {
		return syntheticHandle{}, errors.New(productionFinalizationIdentityMismatch)
	}
	for key := range raw {
		switch key {
		case "projectId", "epoch", "rootActionId", "runId":
		default:
			return syntheticHandle{}, errors.New(productionFinalizationIdentityMismatch)
		}
	}
	handle := syntheticHandle{
		ProjectID:    syntheticString(raw["projectId"]),
		Epoch:        syntheticString(raw["epoch"]),
		RootActionID: syntheticString(raw["rootActionId"]),
		RunID:        syntheticString(raw["runId"]),
	}
	if handle.ProjectID == "" || handle.Epoch == "" || handle.RootActionID == "" || handle.RunID == "" {
		return syntheticHandle{}, errors.New(productionFinalizationIdentityMismatch)
	}
	// retryOf is character_cards-only; the chapter_notes path refuses it exactly
	// like the reference (main-generation-owner.ts:1372).
	if syntheticMap(value["retryOf"]) != nil {
		return syntheticHandle{}, errors.New(productionFinalizationRequestInvalid)
	}
	return handle, nil
}

// productionFinalizationCommitRequest is the reference's commit envelope
// (main-generation-owner.ts:1402-1403): {handle, artifact}.
func productionFinalizationCommitRequest(value map[string]any) (syntheticHandle, map[string]any, error) {
	if value == nil {
		return syntheticHandle{}, nil, errors.New(productionFinalizationRequestInvalid)
	}
	for key := range value {
		switch key {
		case "handle", "artifact":
		default:
			return syntheticHandle{}, nil, errors.New(productionFinalizationRequestInvalid)
		}
	}
	handle, err := productionFinalizationExecuteRequest(map[string]any{"handle": value["handle"]})
	if err != nil {
		return syntheticHandle{}, nil, err
	}
	ref := syntheticMap(value["artifact"])
	if ref == nil {
		return syntheticHandle{}, nil, errors.New(productionFinalizationArtifactInvalid)
	}
	for key := range ref {
		switch key {
		case "artifactId", "revision", "textHash":
		default:
			return syntheticHandle{}, nil, errors.New(productionFinalizationArtifactInvalid)
		}
	}
	if syntheticString(ref["artifactId"]) == "" || syntheticNumber(ref["revision"]) < 1 ||
		!syntheticHashPattern.MatchString(syntheticString(ref["textHash"])) {
		return syntheticHandle{}, nil, errors.New(productionFinalizationArtifactInvalid)
	}
	return handle, ref, nil
}

// productionFinalizationRun is one proved finalization run: the ledger row, its
// binding and manifest, the stored context/task bytes and the slot.
type productionFinalizationRun struct {
	Run      map[string]any
	Manifest map[string]any
	ModelID  string
	Slot     productionFinalizationSlot
}

// resolve proves one finalization run. The terms are the reference's require plus
// the context proof: the handle names this project and this run, the manifest
// names a finalization operation, and the stored context hashes to the frozen
// context digest.
func (f *productionFinalizationServe) resolve(ctx context.Context, scope generationowner.Scope, ledger *store.GenerationLedger, db *sql.DB, handle syntheticHandle) (*productionFinalizationRun, error) {
	if handle.ProjectID != scope.Project {
		return nil, errors.New(productionFinalizationIdentityMismatch)
	}
	args, err := syntheticArgs(handle.RunID)
	if err != nil {
		return nil, errors.New(productionFinalizationRunMissing)
	}
	value, err := ledger.CallJSON(ctx, "get", args)
	if err != nil {
		return nil, err
	}
	run := syntheticMap(value)
	if run == nil {
		return nil, errors.New(productionFinalizationRunMissing)
	}
	binding := syntheticMap(run["binding"])
	if binding == nil {
		return nil, errors.New(productionFinalizationRunMissing)
	}
	manifest := syntheticMap(binding["sourceManifest"])
	if manifest == nil {
		return nil, errors.New(productionFinalizationContextInvalid)
	}
	operation := syntheticString(manifest["operation"])
	if operation != productionFinalizationOperationNotes && operation != productionFinalizationOperationCards {
		return nil, errors.New(productionFinalizationContextInvalid)
	}
	slotKey := syntheticString(manifest["finalizationGenerationSlotKey"])
	if slotKey == "" {
		return nil, errors.New(productionFinalizationContextInvalid)
	}
	contextHash := syntheticString(manifest["finalizationGenerationContextHash"])
	if contextHash == "" {
		return nil, errors.New(productionFinalizationContextInvalid)
	}
	raw, err := productionFinalizationStoredContext(ctx, db, handle.RunID)
	if err != nil {
		return nil, err
	}
	if syntheticHash(string(raw)) != contextHash {
		return nil, errors.New(productionFinalizationContextInvalid)
	}
	// The run identity terms, epoch-tolerant like the reference's require.
	if syntheticString(run["rootActionId"]) != handle.RootActionID ||
		syntheticString(run["runId"]) != handle.RunID ||
		syntheticString(binding["projectId"]) != handle.ProjectID {
		return nil, errors.New(productionFinalizationIdentityMismatch)
	}
	if handle.Epoch != syntheticString(binding["epoch"]) &&
		handle.Epoch != syntheticString(manifest["finalizationGenerationOriginEpoch"]) {
		return nil, errors.New(productionFinalizationIdentityMismatch)
	}
	slot, err := productionFinalizationParseSlot(syntheticMap(manifest["finalizationGenerationSlot"]))
	if err != nil || slot.Key != slotKey {
		return nil, errors.New(productionFinalizationContextInvalid)
	}
	return &productionFinalizationRun{Run: run, Manifest: manifest,
		ModelID: productionEditorInlineModelID(manifest), Slot: slot}, nil
}

// productionFinalizationStoredContext returns the exact stored text of the
// finalizationGenerationContext member, the same byte-exact read the editor
// inline family performs for its own context.
func productionFinalizationStoredContext(ctx context.Context, db *sql.DB, runID string) ([]byte, error) {
	if db == nil {
		return nil, errors.New(productionFinalizationContextInvalid)
	}
	var stored string
	err := db.QueryRowContext(ctx, `SELECT binding_json FROM generation_runs WHERE run_id=?`, runID).Scan(&stored)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errors.New(productionFinalizationRunMissing)
		}
		return nil, err
	}
	manifest, ok := productionEditorInlineRawMember([]byte(stored), "sourceManifest")
	if !ok {
		return nil, errors.New(productionFinalizationContextInvalid)
	}
	raw, ok := productionEditorInlineRawMember(manifest, "finalizationGenerationContext")
	if !ok || len(raw) == 0 || raw[0] != '{' {
		return nil, errors.New(productionFinalizationContextInvalid)
	}
	return raw, nil
}

// productionFinalizationStoredTask returns the exact stored text of the
// finalizationGenerationTask member.
func productionFinalizationStoredTask(ctx context.Context, db *sql.DB, runID string) ([]byte, error) {
	if db == nil {
		return nil, errors.New(productionFinalizationTaskInvalid)
	}
	var stored string
	err := db.QueryRowContext(ctx, `SELECT binding_json FROM generation_runs WHERE run_id=?`, runID).Scan(&stored)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errors.New(productionFinalizationRunMissing)
		}
		return nil, err
	}
	manifest, ok := productionEditorInlineRawMember([]byte(stored), "sourceManifest")
	if !ok {
		return nil, errors.New(productionFinalizationContextInvalid)
	}
	raw, ok := productionEditorInlineRawMember(manifest, "finalizationGenerationTask")
	if !ok || len(raw) == 0 || raw[0] != '{' {
		return nil, errors.New(productionFinalizationTaskInvalid)
	}
	return raw, nil
}

// productionFinalizationNotesBaseline is the frozen notesBaseline member.
type productionFinalizationNotesBaseline struct {
	BlueprintExists bool
	BlueprintNotes  string
	ContinuityHash  string
}

// productionFinalizationNotesBaseline reads finalizationNotesBaseline
// (finalization-generation-source.ts:19-23): the blueprint notes row and the
// hash of the draft's summary_snapshots rows.
func readProductionFinalizationNotesBaseline(ctx context.Context, db *sql.DB, slot productionFinalizationSlot) (productionFinalizationNotesBaseline, error) {
	baseline := productionFinalizationNotesBaseline{}
	blueprint, err := store.NewBlueprints(db).Get(ctx, slot.ChapterNumber)
	if err != nil {
		return baseline, err
	}
	if blueprint != nil {
		baseline.BlueprintExists = true
		baseline.BlueprintNotes = syntheticString(blueprint["notes"])
	}
	rows, err := db.QueryContext(ctx, `SELECT chapter_number,chapter_notes,continuity_facts,source_finalization_id,source_content_hash,projection_generation FROM summary_snapshots WHERE draft_id=? ORDER BY id`, slot.DraftID)
	if err != nil {
		return baseline, err
	}
	defer func() { _ = rows.Close() }()
	var ordered []graphgen.Object
	for rows.Next() {
		var chapterNumber int64
		var chapterNotes, continuityFacts, finalizationID, contentHash string
		var projectionGeneration int64
		if err := rows.Scan(&chapterNumber, &chapterNotes, &continuityFacts, &finalizationID, &contentHash, &projectionGeneration); err != nil {
			return baseline, err
		}
		ordered = append(ordered, graphgen.Object{
			{Key: "chapter_number", Value: chapterNumber},
			{Key: "chapter_notes", Value: chapterNotes},
			{Key: "continuity_facts", Value: continuityFacts},
			{Key: "source_finalization_id", Value: finalizationID},
			{Key: "source_content_hash", Value: contentHash},
			{Key: "projection_generation", Value: projectionGeneration},
		})
	}
	if err := rows.Err(); err != nil {
		return baseline, err
	}
	if ordered == nil {
		ordered = []graphgen.Object{}
	}
	encoded, err := graphgen.EncodeJSON(ordered)
	if err != nil {
		return baseline, err
	}
	baseline.ContinuityHash = graphgen.TextHash(encoded)
	return baseline, nil
}

// productionFinalizationNotesBaselineText renders the notesBaseline member.
func productionFinalizationNotesBaselineText(baseline productionFinalizationNotesBaseline) string {
	return productionEditorInlineObject(
		productionEditorInlineField{"blueprint", productionEditorInlineObject(
			productionEditorInlineField{"exists", strconv.FormatBool(baseline.BlueprintExists)},
			productionEditorInlineField{"notes", productionEditorInlineQuote(baseline.BlueprintNotes)},
		)},
		productionEditorInlineField{"continuityHash", productionEditorInlineQuote(baseline.ContinuityHash)},
	)
}

// productionFinalizationContextText freezes the seven-member context in the
// reference's insertion order (finalization-generation-source.ts:34-36): slot,
// identity, chapterTitle, chapterEntities, writingLanguage, template,
// notesBaseline. The identity and the template are spliced in as the exact text
// the desktop hashes.
func productionFinalizationContextText(slot productionFinalizationSlot, identity *store.FinalizedCharacterContext, chapterTitle string, chapterEntities []string, language, templateText string, baseline productionFinalizationNotesBaseline) (string, error) {
	if len(templateText) == 0 || templateText[0] != '{' || !json.Valid([]byte(templateText)) {
		return "", errors.New(productionFinalizationContextInvalid)
	}
	identityText, err := graphgen.EncodeJSON(identity)
	if err != nil {
		return "", errors.New(productionFinalizationContextInvalid)
	}
	entities, err := graphgen.EncodeJSON(chapterEntities)
	if err != nil {
		return "", errors.New(productionFinalizationContextInvalid)
	}
	fields := []productionEditorInlineField{
		{"slot", productionFinalizationSlotText(slot)},
		{"identity", identityText},
		{"chapterTitle", productionEditorInlineQuote(chapterTitle)},
		{"chapterEntities", entities},
		{"writingLanguage", productionEditorInlineQuote(language)},
		{"template", templateText},
		{"notesBaseline", productionFinalizationNotesBaselineText(baseline)},
	}
	return productionEditorInlineObject(fields...), nil
}

// productionFinalizationTaskText freezes the task the reference freezes
// (finalization-generation-source.ts:47-59).
func productionFinalizationTaskText(slot productionFinalizationSlot, template prompttemplate.FinalizationPrompt, language string, identity *store.FinalizedCharacterContext, chapterTitle string) (string, error) {
	characters := slot.StepKey == "character_cards"
	purpose := productionFinalizationOperationNotes
	output := "visible-text"
	if characters {
		purpose = productionFinalizationOperationCards
		output = "structured-data"
	}
	role := strings.TrimSpace(template.Template.SystemRole)
	system := template.SystemContract
	if role != "" {
		system = role + "\n\n" + template.SystemContract
	}
	variables := map[string]string{
		"chapter_content":     identity.Content,
		"chapter_number":      strconv.FormatInt(slot.ChapterNumber, 10),
		"chapter_title":       chapterTitle,
		"existing_cards_json": "[]",
	}
	user := productionEditorInlinePrune(productionEditorInlineFill(template.Template.Content, variables))
	if characters {
		user = productionEditorInlineJoin(user, template.OutputContract)
	}
	messages := "[" + productionEditorInlineObject(
		productionEditorInlineField{"role", productionEditorInlineQuote("system")},
		productionEditorInlineField{"content", productionEditorInlineQuote(system)},
	) + "," + productionEditorInlineObject(
		productionEditorInlineField{"role", productionEditorInlineQuote("user")},
		productionEditorInlineField{"content", productionEditorInlineQuote(user)},
	) + "]"
	return productionEditorInlineObject(
		productionEditorInlineField{"purpose", productionEditorInlineQuote(purpose)},
		productionEditorInlineField{"reasoningStage", productionEditorInlineQuote("review")},
		productionEditorInlineField{"output", productionEditorInlineQuote(output)},
		productionEditorInlineField{"messages", messages},
	), nil
}

// productionFinalizationManifestText is the frozen source manifest: the reduced
// lane's members in syntheticManifest order, then chapterNumber, then the seven
// finalization members.
func productionFinalizationManifestText(selection syntheticSelection, scope generationowner.Scope, leaseID, fingerprint, slotText, slotKey, contextText, originEpoch, taskText string) string {
	fields := []productionEditorInlineField{
		{"operation", productionEditorInlineQuote(selection.Operation)},
		{"modelId", productionEditorInlineQuote(selection.ModelID)},
		{"modelLeaseId", productionEditorInlineQuote(leaseID)},
		{"goOwnerSourceFingerprint", productionEditorInlineQuote(fingerprint)},
		{"purpose", productionEditorInlineQuote(syntheticPurpose(selection.Operation))},
		{"output", productionEditorInlineQuote(selection.Output)},
		{"promptKeys", productionEditorInlineStringArray(selection.PromptKeys)},
		{"skillStages", productionEditorInlineStringArray(productionFinalizationStrings(selection.SkillStages))},
		{"authorInputs", "[]"},
		{"selectedDraftIds", productionEditorInlineIDArray(selection.SelectedDraftIDs)},
		{"selectedFinalizedDraftIds", productionEditorInlineIDArray(selection.SelectedFinalizedDraftIDs)},
		{"selectedBlueprintChapterNumbers", productionEditorInlineIDArray(selection.SelectedBlueprintChapters)},
	}
	if selection.HasChapterNumber {
		fields = append(fields, productionEditorInlineField{"chapterNumber", strconv.FormatInt(selection.ChapterNumber, 10)})
	}
	fields = append(fields,
		productionEditorInlineField{"finalizationGenerationSlot", slotText},
		productionEditorInlineField{"finalizationGenerationSlotKey", productionEditorInlineQuote(slotKey)},
		productionEditorInlineField{"finalizationGenerationContext", contextText},
		productionEditorInlineField{"finalizationGenerationContextHash", productionEditorInlineQuote(syntheticHash(string(contextText)))},
		productionEditorInlineField{"finalizationGenerationOriginEpoch", productionEditorInlineQuote(originEpoch)},
		productionEditorInlineField{"finalizationGenerationTask", taskText},
		productionEditorInlineField{"finalizationGenerationTaskHash", productionEditorInlineQuote(syntheticHash(taskText))},
	)
	return productionEditorInlineObject(fields...)
}

func productionFinalizationStrings(values []any) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if text, ok := value.(string); ok {
			out = append(out, text)
		}
	}
	return out
}

// openFinalizationRun is the family's one run admission, mirroring
// openEditorInlineRun: the selection, the lease, the frozen sources, the nine
// derived hashes, the canonical budget, the seven frozen context/task members
// and the ledger's own open, with the context and task spliced in as ordered
// JSON text.
func (f *productionFinalizationServe) openFinalizationRun(ctx context.Context, scope generationowner.Scope, ledger *store.GenerationLedger, db *sql.DB, slot productionFinalizationSlot, modelID, parentRoot string) (map[string]any, error) {
	characters := slot.StepKey == "character_cards"
	operation := productionFinalizationOperationNotes
	output := "visible-text"
	promptKey := prompttemplate.FinalizationChapterNotesKey
	if characters {
		operation = productionFinalizationOperationCards
		output = "structured-data"
		promptKey = prompttemplate.FinalizationCharacterCardsKey
	}
	selection, err := syntheticParseSelection(map[string]any{
		"operation":                       operation,
		"uiActionNonce":                   "finalization:" + slot.Key,
		"modelId":                         modelID,
		"chapterNumber":                   slot.ChapterNumber,
		"selectedDraftIds":                []any{},
		"selectedFinalizedDraftIds":       []any{slot.DraftID},
		"selectedBlueprintChapterNumbers": []any{},
		"promptKeys":                      []any{promptKey},
		"skillStages":                     []any{"review"},
		"authorInputs":                    []any{},
		"output":                          output,
	})
	if err != nil {
		return nil, err
	}
	if parentRoot != "" {
		selection.ParentRootActionID = parentRoot
	}
	receipt, err := f.owner.inner.registry.Begin(ctx, scope, selection.ModelID, syntheticGenerationPurpose)
	if err != nil {
		return nil, err
	}
	set, err := syntheticBuildSources(ctx, db, selection)
	if err != nil {
		return nil, err
	}
	language := productionEditorInlineLanguage(set.Core)
	template, err := prompttemplate.FinalizationTemplate(promptKey, language)
	if err != nil {
		return nil, errors.New(productionFinalizationContextInvalid)
	}
	identity, err := store.NewFinalizedCharacters(db).ReadContext(ctx, slot.DraftID, store.FinalizedCharacterScope{ProjectID: scope.Project, Epoch: scope.Epoch})
	if err != nil {
		return nil, err
	}
	chapterTitle, chapterEntities, err := productionFinalizationChapterParts(ctx, db, slot)
	if err != nil {
		return nil, err
	}
	baseline, err := readProductionFinalizationNotesBaseline(ctx, db, slot)
	if err != nil {
		return nil, err
	}
	contextText, err := productionFinalizationContextText(slot, identity, chapterTitle, chapterEntities, language, template.Template.Text, baseline)
	if err != nil {
		return nil, err
	}
	taskText, err := productionFinalizationTaskText(slot, template, language, identity, chapterTitle)
	if err != nil {
		return nil, err
	}
	binding, err := f.owner.inner.registry.BindSources(ctx, scope, receipt.LeaseID, set.Sources, set.Selection)
	if err != nil {
		return nil, err
	}
	plan, err := f.owner.inner.syntheticCanonicalPlan(ctx, scope, receipt.LeaseID, selection)
	if err != nil {
		return nil, err
	}
	budget, err := syntheticBudget(plan)
	if err != nil {
		return nil, err
	}
	fingerprint, err := syntheticFingerprint(ctx, db, scope, binding, set, selection, receipt, budget)
	if err != nil {
		return nil, err
	}
	frozenInput, err := syntheticCanonical([]any{fingerprint, set.CoreText, syntheticOutputContract(selection)})
	if err != nil {
		return nil, errors.New("GENERATION_BEGIN_INVALID")
	}
	refs, err := syntheticRefsAsAny(binding.Refs())
	if err != nil {
		return nil, errors.New("GENERATION_BEGIN_INVALID")
	}
	fingerprintText, err := productionEditorInlineJSON(fingerprint)
	if err != nil {
		return nil, errors.New("GENERATION_BEGIN_INVALID")
	}
	budgetText, err := productionEditorInlineJSON(budget)
	if err != nil {
		return nil, errors.New("GENERATION_BEGIN_INVALID")
	}
	refsText, err := productionEditorInlineJSON(refs)
	if err != nil {
		return nil, errors.New("GENERATION_BEGIN_INVALID")
	}
	manifest := productionFinalizationManifestText(selection, scope, receipt.LeaseID, binding.Fingerprint(),
		productionFinalizationSlotText(slot), slot.Key, contextText, scope.Epoch, taskText)
	request := productionEditorInlineObject(
		productionEditorInlineField{"projectId", productionEditorInlineQuote(scope.Project)},
		productionEditorInlineField{"epoch", productionEditorInlineQuote(scope.Epoch)},
		productionEditorInlineField{"fingerprint", fingerprintText},
		productionEditorInlineField{"contextSnapshotId", productionEditorInlineQuote(syntheticHash(set.CoreText))},
		productionEditorInlineField{"sourceManifest", manifest},
		productionEditorInlineField{"sourceRefs", refsText},
		productionEditorInlineField{"operation", productionEditorInlineQuote(selection.Operation)},
		productionEditorInlineField{"uiActionNonce", productionEditorInlineQuote(selection.UIActionNonce)},
		productionEditorInlineField{"frozenInputHash", productionEditorInlineQuote(syntheticHash(frozenInput))},
		productionEditorInlineField{"budget", budgetText},
	)
	if selection.ParentRootActionID != "" {
		request = strings.TrimSuffix(request, "}") + "," +
			productionEditorInlineQuote("parentRootActionId") + ":" + productionEditorInlineQuote(selection.ParentRootActionID) + "}"
	}
	value, err := ledger.CallJSON(ctx, "open", json.RawMessage("["+request+"]"))
	if err != nil {
		return nil, err
	}
	run := syntheticMap(value)
	if run == nil {
		return nil, errors.New(productionFinalizationRunMissing)
	}
	return run, nil
}

// productionFinalizationChapterParts reads the chapter title and the chapter
// entities the context freezes.
func productionFinalizationChapterParts(ctx context.Context, db *sql.DB, slot productionFinalizationSlot) (string, []string, error) {
	var chapterTitle string
	err := db.QueryRowContext(ctx, `SELECT chapter_title FROM finalization_outbox WHERE draft_id=?`, slot.DraftID).Scan(&chapterTitle)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil, errors.New(productionFinalizationContextInvalid)
		}
		return "", nil, err
	}
	blueprint, err := store.NewBlueprints(db).Get(ctx, slot.ChapterNumber)
	if err != nil {
		return "", nil, err
	}
	entities := []string{}
	if blueprint != nil {
		raw := syntheticString(blueprint["characters"])
		if raw != "" {
			var decoded []any
			if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
				return "", nil, errors.New(productionFinalizationContextInvalid)
			}
			for _, item := range decoded {
				if text, ok := item.(string); ok {
					entities = append(entities, text)
				}
			}
		}
	}
	if entities == nil {
		entities = []string{}
	}
	return chapterTitle, entities, nil
}

// begin answers finalization-generation:begin. It replays an owned slot and
// otherwise opens one run whose context and task are the shipped template.
func (f *productionFinalizationServe) begin(ctx context.Context, caller transport.Caller, expected map[string]any, request map[string]any) (any, error) {
	if !f.ready() {
		return nil, errProductionGenerationUnavailable
	}
	if _, err := f.owner.open.ModelEgress(); err != nil {
		return nil, err
	}
	_, scope, ledger, db, done, err := f.owner.inner.admit(ctx, caller, expected)
	if err != nil {
		return nil, err
	}
	defer done()
	slot, modelID, parentRoot, err := productionFinalizationBeginRequest(request)
	if err != nil {
		return nil, err
	}
	prior, err := productionFinalizationPriorRun(ctx, db, scope.Project, slot.Key)
	if err != nil {
		return nil, err
	}
	if prior != nil {
		handle, err := productionEditorInlineRunHandle(scope.Project, scope.Epoch, prior)
		if err != nil {
			return nil, err
		}
		return f.recovery(ctx, scope, ledger, db, handle)
	}
	opened, err := f.openFinalizationRun(ctx, scope, ledger, db, slot, modelID, parentRoot)
	if err != nil {
		return nil, err
	}
	handle, err := productionEditorInlineRunHandle(scope.Project, scope.Epoch, opened)
	if err != nil {
		return nil, err
	}
	return f.recovery(ctx, scope, ledger, db, handle)
}

// productionFinalizationPriorRun returns the run that already owns a slot key in
// this project, or nil when none does. It scans the rows without json_extract so
// it does not depend on an extension the admission fingerprint does not declare.
func productionFinalizationPriorRun(ctx context.Context, db *sql.DB, projectID, slotKey string) (map[string]any, error) {
	if db == nil {
		return nil, errors.New(productionFinalizationContextInvalid)
	}
	rows, err := db.QueryContext(ctx, `SELECT run_id,root_action_id,binding_json FROM generation_runs`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var found map[string]any
	for rows.Next() {
		var runID, rootActionID, bindingJSON string
		if err := rows.Scan(&runID, &rootActionID, &bindingJSON); err != nil {
			return nil, err
		}
		var binding map[string]any
		if err := json.Unmarshal([]byte(bindingJSON), &binding); err != nil {
			continue
		}
		if syntheticString(binding["projectId"]) != projectID {
			continue
		}
		manifest := syntheticMap(binding["sourceManifest"])
		if manifest == nil || syntheticString(manifest["finalizationGenerationSlotKey"]) != slotKey {
			continue
		}
		if found != nil {
			return nil, errors.New(productionFinalizationContextInvalid)
		}
		found = map[string]any{"runId": runID, "rootActionId": rootActionID}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return found, nil
}

// recovery assembles the FinalizationGenerationRecovery envelope: attemptCount,
// view, modelId, context, sourceStatus. The context is the proved stored context
// decoded back, so a begin answers the seven-member context it froze.
func (f *productionFinalizationServe) recovery(ctx context.Context, scope generationowner.Scope, ledger *store.GenerationLedger, db *sql.DB, handle syntheticHandle) (any, error) {
	resolved, err := f.resolve(ctx, scope, ledger, db, handle)
	if err != nil {
		return nil, err
	}
	view, err := f.owner.inner.viewOf(ctx, ledger, db, resolved.Run)
	if err != nil {
		return nil, err
	}
	budget, err := syntheticCall(ctx, ledger, "budget", syntheticString(resolved.Run["rootActionId"]))
	if err != nil {
		return nil, err
	}
	attempts := syntheticSlice(syntheticMap(budget)["attempts"])
	attemptCount := float64(len(attempts))
	contextValue, err := productionFinalizationDecodeContext(productionFinalizationStoredContext(ctx, db, handle.RunID))
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"attemptCount": attemptCount,
		"view":         view,
		"modelId":      resolved.ModelID,
		"context":      contextValue,
		"sourceStatus": f.sourceStatus(ctx, scope, ledger, db, resolved),
	}, nil
}

// productionFinalizationDecodeContext decodes the proved context text with number
// literals kept, so the seven members keep their exact stored values.
func productionFinalizationDecodeContext(raw []byte, err error) (map[string]any, error) {
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, errors.New(productionFinalizationContextInvalid)
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New(productionFinalizationContextInvalid)
	}
	return object, nil
}

// sourceStatus answers 'current' only when the run's frozen binding reproduces.
func (f *productionFinalizationServe) sourceStatus(ctx context.Context, scope generationowner.Scope, ledger *store.GenerationLedger, db *sql.DB, resolved *productionFinalizationRun) string {
	conflict := "conflict"
	conflictAt := func(step string) string {
		log.Printf("novel-server: finalization-generation sourceStatus=conflict step=%s", step)
		return conflict
	}
	if f.owner.inner.registry == nil {
		return conflictAt("registry-missing")
	}
	fingerprint := syntheticString(resolved.Manifest["goOwnerSourceFingerprint"])
	if fingerprint == "" {
		return conflictAt("fingerprint-missing")
	}
	_, current, err := f.reproduce(ctx, scope, db, resolved)
	if err != nil {
		return conflict
	}
	if current != fingerprint {
		return conflictAt("fingerprint-mismatch")
	}
	return "current"
}

// reproduce re-derives the frozen binding fingerprint the way execute does.
func (f *productionFinalizationServe) reproduce(ctx context.Context, scope generationowner.Scope, db *sql.DB, resolved *productionFinalizationRun) (*generationowner.Binding, string, error) {
	runScope := scope
	if binding := syntheticMap(syntheticMap(resolved.Run)["binding"]); binding != nil {
		runScope.Project = syntheticString(binding["projectId"])
		runScope.Epoch = syntheticString(binding["epoch"])
	}
	leaseID := syntheticString(resolved.Manifest["modelLeaseId"])
	if leaseID == "" {
		return nil, "", errors.New(productionFinalizationLeaseMissing)
	}
	set, err := f.owner.inner.rebuildSourcesForRun(ctx, db, resolved.Manifest)
	if err != nil {
		return nil, "", err
	}
	binding, err := f.owner.inner.registry.ReproduceBinding(ctx, runScope, leaseID, set.Sources, set.Selection)
	if err != nil {
		return nil, "", err
	}
	return binding, binding.Fingerprint(), nil
}

// execute answers finalization-generation:execute: the one dispatch of the
// frozen task, then the settled attempt projected as the receipt.
func (f *productionFinalizationServe) execute(ctx context.Context, caller transport.Caller, expected map[string]any, request map[string]any) (any, error) {
	if !f.ready() {
		return nil, errProductionGenerationUnavailable
	}
	if _, err := f.owner.open.ModelEgress(); err != nil {
		return nil, err
	}
	if f.owner.inner.executor == nil {
		return nil, errProductionGenerationExecuteUnavailable
	}
	handle, err := productionFinalizationExecuteRequest(request)
	if err != nil {
		return nil, err
	}
	contextValue, scope, ledger, db, done, err := f.owner.inner.admit(ctx, caller, expected)
	if err != nil {
		return nil, err
	}
	defer done()
	resolved, err := f.resolve(ctx, scope, ledger, db, handle)
	if err != nil {
		return nil, err
	}
	taskText, err := productionFinalizationStoredTask(ctx, db, handle.RunID)
	if err != nil {
		return nil, err
	}
	if syntheticHash(string(taskText)) != syntheticString(resolved.Manifest["finalizationGenerationTaskHash"]) {
		return nil, errors.New(productionFinalizationTaskInvalid)
	}
	task, err := generationowner.ParseTask(taskText)
	if err != nil {
		return nil, err
	}
	binding, fingerprint, err := f.reproduce(ctx, scope, db, resolved)
	if err != nil {
		return nil, err
	}
	if fingerprint != syntheticString(resolved.Manifest["goOwnerSourceFingerprint"]) {
		return nil, errors.New(productionFinalizationSourceChanged)
	}
	leaseID := syntheticString(resolved.Manifest["modelLeaseId"])
	frozen, err := f.owner.inner.executor.Prepare(ctx, scope, leaseID, task, syntheticRootOutputTokens, binding)
	if err != nil {
		return nil, err
	}
	service, err := f.owner.inner.newService(ctx, caller, contextValue, ledger, db, scope, handle)
	if err != nil {
		return nil, err
	}
	receipt, err := service.Execute(ctx, frozen, handle.RunID, productionFinalizationExecuteNonce)
	if err != nil {
		return nil, err
	}
	outcome := syntheticOutcome(syntheticMap(receipt))
	view, err := f.owner.inner.viewOf(ctx, ledger, db, syntheticMap(receipt)["run"])
	if err != nil {
		return nil, err
	}
	return map[string]any{"outcome": outcome, "run": view}, nil
}

// commit answers finalization-generation:commit: prove the artifact, then write
// the reference's receipt into the attempt's usage receipt.
func (f *productionFinalizationServe) commit(ctx context.Context, caller transport.Caller, expected map[string]any, request map[string]any) (any, error) {
	if !f.ready() {
		return nil, errProductionGenerationUnavailable
	}
	if _, err := f.owner.open.ModelEgress(); err != nil {
		return nil, err
	}
	handle, ref, err := productionFinalizationCommitRequest(request)
	if err != nil {
		return nil, err
	}
	_, scope, ledger, db, done, err := f.owner.inner.admit(ctx, caller, expected)
	if err != nil {
		return nil, err
	}
	defer done()
	resolved, err := f.resolve(ctx, scope, ledger, db, handle)
	if err != nil {
		return nil, err
	}
	if resolved.Slot.StepKey != "chapter_notes" {
		return nil, errors.New(productionFinalizationAdmissionRequired)
	}
	proof, err := productionFinalizationArtifact(ctx, db, ledger, resolved.Run, ref)
	if err != nil {
		return nil, err
	}
	contextText, err := productionFinalizationStoredContext(ctx, db, handle.RunID)
	if err != nil {
		return nil, err
	}
	existing, err := productionFinalizationReadEffect(ctx, db, proof.AttemptID, contextText)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return existing, nil
	}
	chapterNotes := strings.TrimSpace(proof.ArtifactText)
	identity, err := store.NewFinalizedCharacters(db).ReadContext(ctx, resolved.Slot.DraftID, store.FinalizedCharacterScope{ProjectID: scope.Project, Epoch: scope.Epoch})
	if err != nil {
		return nil, err
	}
	_, chapterEntities, err := productionFinalizationChapterParts(ctx, db, resolved.Slot)
	if err != nil {
		return nil, err
	}
	baseline, err := readProductionFinalizationNotesBaseline(ctx, db, resolved.Slot)
	if err != nil {
		return nil, err
	}
	frozenBaseline := productionFinalizationBaselineFromContext(contextText)
	if frozenBaseline != "" && frozenBaseline != productionFinalizationNotesBaselineText(baseline) {
		return nil, errors.New(productionFinalizationNotesChanged)
	}
	facts := store.BuildFinalizedContinuityFacts(resolved.Slot.ChapterNumber, chapterNotes, identity.Content, chapterEntities, identity)
	blueprintUpdated, err := store.NewBlueprints(db).UpdateNotes(ctx, resolved.Slot.ChapterNumber, chapterNotes)
	if err != nil {
		return nil, err
	}
	saveErr := store.NewSummary(db).SaveFinalizedContinuity(ctx, store.SaveFinalizedContinuityRequest{
		DraftID:              resolved.Slot.DraftID,
		ChapterNumber:        resolved.Slot.ChapterNumber,
		ChapterNotes:         chapterNotes,
		Facts:                facts,
		ProjectionGeneration: identity.ProjectionGeneration,
		Source:               identity.Source,
	})
	if saveErr != nil {
		return nil, saveErr
	}
	receipt := productionFinalizationNotesReceipt(chapterNotes, facts, blueprintUpdated)
	if err := productionFinalizationStampEffect(ctx, db, proof.AttemptID, ref, syntheticHash(string(contextText)), receipt); err != nil {
		return nil, err
	}
	return receipt, nil
}

// productionFinalizationBaselineFromContext extracts the notesBaseline text from
// the stored context, or "" when it cannot be read.
func productionFinalizationBaselineFromContext(contextText []byte) string {
	raw, ok := productionEditorInlineRawMember(contextText, "notesBaseline")
	if !ok {
		return ""
	}
	return string(raw)
}

// productionFinalizationNotesReceipt is the chapter_notes receipt in the
// reference's key order (finalization-generation.ts:104).
func productionFinalizationNotesReceipt(chapterNotes string, facts []store.FinalizedContinuityFact, blueprintUpdated bool) map[string]any {
	return map[string]any{
		"success":          true,
		"stepKey":          "chapter_notes",
		"chapterNotes":     chapterNotes,
		"factCount":        len(facts),
		"blueprintUpdated": blueprintUpdated,
	}
}

// productionFinalizationArtifact is the reference's artifact proof
// (finalization-generation.ts:52-59).
type productionFinalizationArtifactProof struct {
	AttemptID    string
	ArtifactText string
}

func productionFinalizationArtifact(ctx context.Context, db *sql.DB, ledger *store.GenerationLedger, run map[string]any, ref map[string]any) (*productionFinalizationArtifactProof, error) {
	invalid := errors.New(productionFinalizationArtifactInvalid)
	artifactID := syntheticString(ref["artifactId"])
	var runID, attemptID, status string
	err := db.QueryRowContext(ctx, `SELECT run_id,attempt_id,status FROM generation_artifacts WHERE artifact_id=?`, artifactID).Scan(&runID, &attemptID, &status)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, invalid
		}
		return nil, err
	}
	if runID != syntheticString(run["runId"]) || status == "discarded" {
		return nil, invalid
	}
	receiptValue, err := syntheticCall(ctx, ledger, "receipt", attemptID)
	if err != nil {
		return nil, err
	}
	receipt := syntheticMap(receiptValue)
	artifact := syntheticMap(receipt["artifact"])
	attempt := syntheticMap(receipt["attempt"])
	result := syntheticMap(receipt["result"])
	if artifact == nil || attempt == nil {
		return nil, invalid
	}
	if syntheticNumber(artifact["revision"]) != syntheticNumber(ref["revision"]) ||
		syntheticString(artifact["textHash"]) != syntheticString(ref["textHash"]) ||
		syntheticHash(syntheticString(artifact["text"])) != syntheticString(ref["textHash"]) ||
		strings.TrimSpace(syntheticString(artifact["text"])) == "" {
		return nil, invalid
	}
	if productionFinalizationTruthy(receipt["failureCode"]) {
		return nil, invalid
	}
	attemptStatus := syntheticString(attempt["status"])
	if attemptStatus != "settled" && attemptStatus != "unknown" {
		return nil, invalid
	}
	if result == nil || syntheticString(result["finishReason"]) != "stop" {
		return nil, invalid
	}
	return &productionFinalizationArtifactProof{AttemptID: attemptID, ArtifactText: syntheticString(artifact["text"])}, nil
}

// productionFinalizationReadEffect reads a previously stamped finalizationEffect
// for an attempt, or nil when none exists.
func productionFinalizationReadEffect(ctx context.Context, db *sql.DB, attemptID string, contextText []byte) (map[string]any, error) {
	var usage string
	err := db.QueryRowContext(ctx, `SELECT usage_receipt_json FROM generation_attempts WHERE attempt_id=?`, attemptID).Scan(&usage)
	if err != nil {
		return nil, err
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(usage), &decoded); err != nil {
		return nil, err
	}
	effect := syntheticMap(decoded["finalizationEffect"])
	if effect == nil {
		return nil, nil
	}
	if syntheticNumber(effect["version"]) != 1 || syntheticString(effect["contextHash"]) != syntheticHash(string(contextText)) {
		return nil, errors.New(productionFinalizationArtifactInvalid)
	}
	return syntheticMap(effect["receipt"]), nil
}

// productionFinalizationStampEffect writes the SavedEffect into the attempt's
// usage receipt (finalization-generation.ts:113-114).
func productionFinalizationStampEffect(ctx context.Context, db *sql.DB, attemptID string, ref map[string]any, contextHash string, receipt map[string]any) error {
	receiptText, err := productionFinalizationJSONOrdered(receipt)
	if err != nil {
		return err
	}
	effect := map[string]any{
		"version":     1,
		"artifact":    ref,
		"contextHash": contextHash,
		"receipt":     receipt,
		"receiptHash": syntheticHash(receiptText),
	}
	var usage string
	if err := db.QueryRowContext(ctx, `SELECT usage_receipt_json FROM generation_attempts WHERE attempt_id=?`, attemptID).Scan(&usage); err != nil {
		return err
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(usage), &decoded); err != nil {
		return err
	}
	decoded["finalizationEffect"] = effect
	encoded, err := json.Marshal(decoded)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `UPDATE generation_attempts SET usage_receipt_json=? WHERE attempt_id=?`, string(encoded), attemptID)
	return err
}

// productionFinalizationJSONOrdered renders a receipt map in its insertion
// order by walking the map's known keys; the chapter_notes receipt is the only
// shape this lane writes.
func productionFinalizationJSONOrdered(receipt map[string]any) (string, error) {
	fields := []productionEditorInlineField{
		{"success", "true"},
		{"stepKey", productionEditorInlineQuote(syntheticString(receipt["stepKey"]))},
		{"chapterNotes", productionEditorInlineQuote(syntheticString(receipt["chapterNotes"]))},
		{"factCount", strconv.Itoa(int(syntheticNumber(receipt["factCount"])))},
		{"blueprintUpdated", strconv.FormatBool(syntheticBool(receipt["blueprintUpdated"]))},
	}
	return productionEditorInlineObject(fields...), nil
}

// productionFinalizationTruthy is JavaScript truthiness for the one receipt
// field the artifact proof checks.
func productionFinalizationTruthy(value any) bool {
	switch v := value.(type) {
	case nil:
		return false
	case bool:
		return v
	case string:
		return v != ""
	case float64:
		return v != 0
	case json.Number:
		n, err := v.Float64()
		return err != nil || n != 0
	}
	return true
}
