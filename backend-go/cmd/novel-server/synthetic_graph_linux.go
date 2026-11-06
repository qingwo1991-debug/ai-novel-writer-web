package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"strings"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/graphgen"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// GraphRead is the graph generation recovery projection for one browser handle:
// the run view, the validated frozen context and the facts the UI needs to decide
// whether the run can still be continued.
func (s *syntheticGeneration) GraphRead(ctx context.Context, caller transport.Caller, expected map[string]any, arg any) (any, error) {
	handle, err := syntheticParseHandle(arg)
	if err != nil {
		return nil, err
	}
	_, scope, ledger, db, done, err := s.admit(ctx, caller, expected)
	if err != nil {
		return nil, err
	}
	defer done()
	run, _, _, err := syntheticRequireRun(ctx, ledger, scope, handle)
	if err != nil {
		return nil, err
	}
	return s.graphRecovery(ctx, ledger, db, scope.Project, run)
}

// graphRecovery projects one admitted graph run into the browser's
// GraphGenerationRecovery (src/shared/graph-generation.ts:30-39). A handle read
// and a replayed begin answer exactly this, so it is one function rather than two
// copies of the same five projections.
//
// Two pieces of the browser contract are deliberately absent, and both are
// absent for the same reason - they can only be answered for a run that has
// produced output, and no operation that produces one is bound yet:
//
//   - effects: graph effects are recorded by confirmation, which is unbound.
//   - artifact/result: the Node projection qualifies the stored result against a
//     graph-specific reference and omits it when that qualification fails, so
//     raw unqualified output stays in the run view for copying. Re-projecting
//     the stored result without that check would present unverified text as a
//     result, which is exactly what the original refuses to do.
//
// sourceStatus is syntheticGraphSourceStatus, which answers "current" only for a
// kind whose sources this lane re-reads and proves unchanged. Confirmation writes
// finalized text over what the author already has, so a kind with no
// re-derivation still reads "conflict".
func (s *syntheticGeneration) graphRecovery(ctx context.Context, ledger *store.GenerationLedger, db *sql.DB, projectID string, run map[string]any) (map[string]any, error) {
	manifest := syntheticMap(syntheticMap(run["binding"])["sourceManifest"])
	contextValue, _, err := syntheticGraphContext(manifest, projectID)
	if err != nil {
		return nil, err
	}
	attempts, err := syntheticGraphAttemptIDs(ctx, db, syntheticString(run["runId"]))
	if err != nil {
		return nil, err
	}
	// A second attempt means the run was retried, and a recovery would have to
	// choose which raw output the author is looking at. The original refuses
	// instead of guessing.
	if len(attempts) > 1 {
		return nil, errors.New("GENERATION_GRAPH_ATTEMPT_CONFLICT")
	}
	view, err := s.viewOf(ctx, ledger, db, run)
	if err != nil {
		return nil, err
	}
	status, err := syntheticGraphSourceStatus(ctx, ledger, db, syntheticString(run["rootActionId"]), contextValue)
	if err != nil {
		return nil, err
	}
	// The frozen input is returned in the browser's own key order, after the digest
	// checks above read it as the document it is frozen as.
	contextValue["input"] = syntheticGraphContextInput(contextValue)
	return map[string]any{
		"view":         view,
		"modelId":      syntheticString(manifest["modelId"]),
		"context":      contextValue,
		"attemptCount": float64(len(attempts)),
		"sourceStatus": status,
		"effects":      []any{},
	}, nil
}

// FinalizationRead is the finalization-generation:read projection: the recovery
// of the run that currently owns a finalization slot, or JSON null when no run
// owns it yet.
//
// null is a real answer here, not a stand-in. The renderer asks this before
// begin (src/services/finalization-generation.ts:38) and reads null as "nothing
// to resume", so refusing the call would break the first step of every
// finalization instead of protecting anything. The slot is validated exactly
// like finalizationSlotKey and its key is matched against the frozen manifest of
// the runs this project's ledger holds, so an absent answer is the result of a
// real lookup rather than a constant.
//
// A matching row is refused instead of projected. That key is stamped by the
// finalization source binding alone
// (electron/services/generation-source-binding.ts:589), which this lane never
// reaches because it does not bind finalization begin, so such a row can only
// come from another writer. Recovering it would mean proving textHash(JSON.stringify(context))
// against the stored finalizationGenerationContextHash, and JSON.stringify
// writes property insertion order while every digest this lane derives is
// canonical (syntheticGraphDigest): either that comparison is skipped or a
// context the lane cannot prove is handed to the renderer. It refuses with the
// code the original raises for an unprovable context, and a row whose binding
// names another project is the original's slot conflict
// (electron/services/finalization-generation.ts:27).
func (s *syntheticGeneration) FinalizationRead(ctx context.Context, caller transport.Caller, expected map[string]any, arg any) (any, error) {
	key, err := syntheticFinalizationSlotKey(arg)
	if err != nil {
		return nil, err
	}
	_, scope, _, db, done, err := s.admit(ctx, caller, expected)
	if err != nil {
		return nil, err
	}
	defer done()
	rows, err := db.QueryContext(ctx,
		`SELECT json_extract(binding_json,'$.projectId') FROM generation_runs
		 WHERE json_extract(binding_json,'$.sourceManifest.finalizationGenerationSlotKey')=? ORDER BY rowid`, key)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	found := false
	for rows.Next() {
		// A missing projectId (NULL) is the original's `undefined !== projectId`
		// conflict, so NullString covers both the foreign row and the shapeless one.
		var owner sql.NullString
		if err := rows.Scan(&owner); err != nil {
			return nil, err
		}
		if !owner.Valid || owner.String != scope.Project {
			return nil, errors.New("GENERATION_FINALIZATION_SLOT_CONFLICT")
		}
		found = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if !found {
		return nil, nil
	}
	// The rows all belong to this project, so the original would go on to compare
	// each stored context with the slot; that comparison is what this lane cannot
	// reproduce, and it refuses rather than guesses.
	return nil, errors.New("GENERATION_FINALIZATION_CONTEXT_INVALID")
}

// syntheticFinalizationSlotKey validates one FinalizationGenerationSlot and
// returns the original's finalizationSlotKey
// (electron/services/finalization-generation-source.ts:12):
//
//	textHash(JSON.stringify([draftId, finalizationId, chapterNumber, contentHash, stepKey]))
//
// The hashed value is an array of primitives, so the insertion-order ambiguity
// that forces this lane's object digests onto canonical encoding cannot arise:
// graphgen.EncodeJSON reproduces JSON.stringify for exactly this shape, and the
// key matches the one a desktop build stores for the same slot.
func syntheticFinalizationSlotKey(value any) (string, error) {
	invalid := errors.New("GENERATION_FINALIZATION_SLOT_INVALID")
	slot := syntheticMap(value)
	if slot == nil {
		return "", invalid
	}
	for key := range slot {
		if key != "source" && key != "stepKey" {
			return "", invalid
		}
	}
	stepKey := syntheticString(slot["stepKey"])
	if stepKey != "chapter_notes" && stepKey != "character_cards" {
		return "", invalid
	}
	source := syntheticMap(slot["source"])
	if source == nil {
		return "", invalid
	}
	for key := range source {
		switch key {
		case "draftId", "chapterNumber", "finalizationId", "contentHash":
		default:
			return "", invalid
		}
	}
	draftID, draftOK := syntheticSlotID(source["draftId"])
	chapterNumber, chapterOK := syntheticSlotID(source["chapterNumber"])
	finalizationID, finalizationOK := source["finalizationId"].(string)
	contentHash, contentOK := source["contentHash"].(string)
	if !draftOK || !chapterOK || !finalizationOK || !contentOK ||
		strings.TrimSpace(finalizationID) == "" || !syntheticHex64(contentHash) {
		return "", invalid
	}
	encoded, err := graphgen.EncodeJSON([]any{draftID, finalizationID, chapterNumber, contentHash, stepKey})
	if err != nil {
		return "", invalid
	}
	return graphgen.TextHash(encoded), nil
}

// syntheticSlotID accepts exactly what the original's Number.isSafeInteger guard
// accepts: a JSON number, integral, positive and inside the safe-integer range.
// A numeric string is refused, unlike this lane's looser ledger coercion,
// because the original refuses it too.
func syntheticSlotID(value any) (int64, bool) {
	number, ok := syntheticFloat64(value)
	if !ok || math.IsNaN(number) || math.IsInf(number, 0) ||
		number != math.Trunc(number) || number < 1 || number > 1<<53-1 {
		return 0, false
	}
	return int64(number), true
}

// syntheticGraphAttemptIDs lists a run's attempts in dispatch order.
func syntheticGraphAttemptIDs(ctx context.Context, db *sql.DB, runID string) ([]string, error) {
	rows, err := db.QueryContext(ctx, "SELECT attempt_id FROM generation_attempts WHERE run_id=? ORDER BY rowid", runID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	attempts := make([]string, 0, 1)
	for rows.Next() {
		var attemptID string
		if err := rows.Scan(&attemptID); err != nil {
			return nil, err
		}
		attempts = append(attempts, attemptID)
	}
	return attempts, rows.Err()
}

// syntheticGraphSourceStatus reports whether the run's sources are still the ones
// it was frozen from.
//
// The default is "conflict", not "current". This value gates confirmation, and
// confirmation writes finalized text over what the author already has, so an
// unverifiable source state must read as changed rather than as fine. Only a kind
// whose current state can actually be re-read gets to answer "current": the event
// kind earns it from the draftId/finalizationId/contentHash triple its context
// freezes and the finalization outbox stores, the plan kind from the three facts
// re-read below.
//
// The plot kind still needs its own re-derivation and is not a shortcut away: the
// frozen plot baseline is only a CAS over the raw plot_tree_snapshot column, so a
// run whose other plot sources moved would read "current" on it. Until the binding
// rebuild exists, a plot run reports "conflict" - visibly wrong rather than
// invisibly permissive, which is the right way round for a value this one gates.
// The plan kind is re-derived here from the three facts its task is built from, so
// it answers "current" only when all three still read back unchanged.
func syntheticGraphSourceStatus(ctx context.Context, ledger *store.GenerationLedger, db *sql.DB, rootActionID string, contextValue map[string]any) (string, error) {
	budgetValue, err := syntheticCall(ctx, ledger, "budget", rootActionID)
	if err != nil {
		return "", err
	}
	root := syntheticMap(syntheticMap(budgetValue)["root"])
	if strings.TrimSpace(syntheticString(root["status"])) == "cancelled" {
		return "conflict", nil
	}
	if kind := syntheticString(contextValue["kind"]); kind == "plan" {
		// The plan task is built from exactly three frozen facts - the blueprint
		// row, the total chapter count and the writing language - so re-reading
		// those three from the admitted database and digesting them the same way is
		// a real re-derivation, not an assumption. A blueprint or a project core
		// that no longer reads back leaves the current digest empty, which the
		// equality rule reports as a changed source.
		current, err := syntheticGraphPlanCurrentHash(ctx, db, contextValue)
		if err != nil {
			return "", err
		}
		frozen, err := syntheticGraphPlanFrozenHash(contextValue)
		if err != nil {
			return "", err
		}
		return syntheticGraphHashFreshness(current, frozen), nil
	}
	if syntheticString(contextValue["kind"]) != "event" {
		return syntheticGraphFreshness("", "", ""), nil
	}
	source := syntheticMap(contextValue["source"])
	if source == nil || syntheticString(source["contentHash"]) == "" {
		return syntheticGraphFreshness("event", "", ""), nil
	}
	var current string
	err = db.QueryRowContext(ctx,
		"SELECT content_hash FROM finalization_outbox WHERE draft_id=? AND finalization_id=?",
		syntheticNumber(source["draftId"]), syntheticString(source["finalizationId"]),
	).Scan(&current)
	if err != nil {
		// A missing row means the finalized chapter is no longer the one the run
		// was frozen from, which is exactly a changed source.
		if errors.Is(err, sql.ErrNoRows) {
			return "conflict", nil
		}
		return "", err
	}
	return syntheticGraphFreshness("event", current, syntheticString(source["contentHash"])), nil
}

// syntheticGraphFreshness is the decision rule on its own, so the fail-closed
// direction can be pinned by a test that needs no database. Only the kind whose
// current state the caller re-reads may answer anything but "conflict"; every
// other kind - today the plot kind, and the empty kind, which names no operation
// at all - proves nothing and must keep reporting the safe answer.
func syntheticGraphFreshness(kind, currentHash, frozenHash string) string {
	if kind != "event" {
		return "conflict"
	}
	return syntheticGraphHashFreshness(currentHash, frozenHash)
}

// syntheticGraphHashFreshness is the equality half of the source-status rule: only
// two non-empty digests that match prove the sources did not move.
func syntheticGraphHashFreshness(currentHash, frozenHash string) string {
	if currentHash == "" || frozenHash == "" {
		return "conflict"
	}
	if currentHash == frozenHash {
		return "current"
	}
	return "conflict"
}

// syntheticGraphInputJSON is one graph input in the browser's own key order:
// `kind` first and then exactly the numbers that kind carries. The zero numbers
// are dropped, which is what the renderer's own literals omit
// (src/shared/graph-generation.ts:9).
type syntheticGraphInputJSON struct {
	Kind          string `json:"kind"`
	ChapterNumber int64  `json:"chapterNumber,omitempty"`
	PlanID        int64  `json:"planId,omitempty"`
	DraftID       int64  `json:"draftId,omitempty"`
}

// syntheticGraphInput is one validated GraphGenerationInput. Only the plan kind is
// served by this lane, but every kind is recognised so an unsupported kind is
// refused as unsupported rather than as malformed.
type syntheticGraphInput struct {
	Kind          string
	ChapterNumber int64
	PlanID        int64
	DraftID       int64
}

// syntheticGraphInputKeys is the key set each input kind may carry
// (electron/services/graph-generation-source.ts:18-21).
var syntheticGraphInputKeys = map[string][]string{
	"plot":  {"kind"},
	"plan":  {"kind", "chapterNumber"},
	"event": {"kind", "planId", "draftId"},
}

// syntheticGraphInputOf validates one graph input and returns it typed. The rules
// are the desktop's: a known kind, no key outside that kind's set, and a positive
// safe integer wherever the kind carries a number.
func syntheticGraphInputOf(value any) (syntheticGraphInput, error) {
	invalid := errors.New("GENERATION_GRAPH_INPUT_INVALID")
	object := syntheticMap(value)
	if object == nil {
		return syntheticGraphInput{}, invalid
	}
	kind := syntheticString(object["kind"])
	keys, known := syntheticGraphInputKeys[kind]
	if !known {
		return syntheticGraphInput{}, invalid
	}
	for key := range object {
		listed := false
		for _, allowed := range keys {
			if key == allowed {
				listed = true
			}
		}
		if !listed {
			return syntheticGraphInput{}, invalid
		}
	}
	input := syntheticGraphInput{Kind: kind}
	var err error
	if kind == "plan" {
		if input.ChapterNumber, err = syntheticGraphCount(object["chapterNumber"]); err != nil {
			return syntheticGraphInput{}, err
		}
	}
	if kind == "event" {
		if input.PlanID, err = syntheticGraphCount(object["planId"]); err != nil {
			return syntheticGraphInput{}, err
		}
		if input.DraftID, err = syntheticGraphCount(object["draftId"]); err != nil {
			return syntheticGraphInput{}, err
		}
	}
	return input, nil
}

// syntheticGraphCount is the Number.isSafeInteger-and-positive guard the original
// applies to every graph input number
// (electron/services/graph-generation-source.ts:20-21).
func syntheticGraphCount(value any) (int64, error) {
	number, ok := syntheticFloat64(value)
	if !ok || math.IsNaN(number) || math.IsInf(number, 0) ||
		number != math.Trunc(number) || number < 1 || number > 1<<53-1 {
		return 0, errors.New("GENERATION_GRAPH_INPUT_INVALID")
	}
	return int64(number), nil
}

// syntheticGraphInputData is the input as the frozen context stores it, and the
// document a same-nonce begin compares against.
func syntheticGraphInputData(input syntheticGraphInput) map[string]any {
	data := map[string]any{"kind": input.Kind}
	switch input.Kind {
	case "plan":
		data["chapterNumber"] = input.ChapterNumber
	case "event":
		data["planId"] = input.PlanID
		data["draftId"] = input.DraftID
	}
	return data
}

// syntheticGraphContextInput renders the frozen input in the browser's own key
// order. The renderer compares JSON.stringify(recovery.context.input) with the
// request it just sent (src/services/graph-generation.ts:56), and JSON.stringify
// is insertion-ordered: a map would sort chapterNumber ahead of kind and every
// begin would fail GRAPH_GENERATION_CONTEXT_MISMATCH.
func syntheticGraphContextInput(contextValue map[string]any) syntheticGraphInputJSON {
	input := syntheticMap(contextValue["input"])
	projected := syntheticGraphInputJSON{Kind: syntheticString(input["kind"])}
	switch projected.Kind {
	case "plan":
		projected.ChapterNumber = syntheticNumber(input["chapterNumber"])
	case "event":
		projected.PlanID = syntheticNumber(input["planId"])
		projected.DraftID = syntheticNumber(input["draftId"])
	}
	return projected
}

// syntheticGraphKey is the main-issued context/owner lookup key
// (src/shared/graph-generation.ts:14-16). The desktop mints a random UUID and
// never compares it with a renderer value, so this lane mints one stable value per
// (project, nonce) instead: a duplicate begin then freezes the same context rather
// than a second key.
func syntheticGraphKey(projectID, nonce string) string {
	return syntheticHash(projectID + "\x00" + nonce)
}

// syntheticGraphWritingLanguage is resolveWritingLanguage
// (src/shared/writing-language.ts:7-11), the same coercion the source capture
// applies to the project row.
func syntheticGraphWritingLanguage(value any) string {
	language := syntheticString(value)
	if language != "zh-CN" && language != "en-US" {
		return "zh-CN"
	}
	return language
}

// syntheticGraphValue is the manifest form of a value: exactly the JSON document
// the ledger stores, so the digest frozen beside it and the copy the read path
// re-digests are the same bytes.
func syntheticGraphValue(value any) (any, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var generic any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return nil, err
	}
	return generic, nil
}

// syntheticGraphPlanFrozenHash digests the three facts a plan task is built from,
// as the run froze them.
func syntheticGraphPlanFrozenHash(contextValue map[string]any) (string, error) {
	return syntheticGraphDigest(map[string]any{
		"blueprint":       contextValue["blueprint"],
		"totalChapters":   syntheticNumber(contextValue["totalChapters"]),
		"writingLanguage": syntheticString(contextValue["writingLanguage"]),
	})
}

// syntheticGraphPlanCurrentHash digests the same three facts as the admitted author
// database holds them now. An empty answer means the blueprint or the project core
// no longer reads back, which is a changed source.
func syntheticGraphPlanCurrentHash(ctx context.Context, db *sql.DB, contextValue map[string]any) (string, error) {
	chapter := syntheticNumber(syntheticMap(contextValue["input"])["chapterNumber"])
	if chapter < 1 {
		return "", nil
	}
	core, err := store.NewProjectCore(db).Get(ctx)
	if err != nil {
		return "", err
	}
	if core == nil {
		return "", nil
	}
	blueprint, err := store.NewBlueprints(db).Get(ctx, chapter)
	if err != nil {
		return "", err
	}
	if blueprint == nil {
		return "", nil
	}
	return syntheticGraphDigest(map[string]any{
		"blueprint":       blueprint,
		"totalChapters":   syntheticNumber(core["totalChapters"]),
		"writingLanguage": syntheticGraphWritingLanguage(core["writingLanguage"]),
	})
}
