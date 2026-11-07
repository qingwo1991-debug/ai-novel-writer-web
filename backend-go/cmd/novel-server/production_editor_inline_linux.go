//go:build linux

package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"math"
	"net/http"
	"strings"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/generationowner"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file is the production lane's editor-inline family: the CodeMirror inline
// assist an author drives from inside the manuscript editor. All four inventoried
// names are served; the read half lives here and the write half in
// production_editor_inline_write_linux.go.
//
//	editor-inline:begin          served    (production_editor_inline_write_linux.go: productionEditorInline.begin)
//	editor-inline:read-recovery  served    (productionEditorInline.read)
//	editor-inline:execute        served    (production_editor_inline_write_linux.go: productionEditorInline.execute)
//	editor-inline:cancel         served    (productionEditorInline.cancel)
//
// The four names are inventoried at web/operations.json:188-191 and registered by
// electron/controllers/generation-controller.ts:248-251. Every one is arity 1 at
// the renderer (src/shared/editor-inline-generation.ts:27-32): one request object,
// the run handle carried as request.handle for begin's three read/execute/cancel
// siblings. The frozen project context is therefore a trailing second wire argument,
// so every name this lane installs takes `operation(2, ...)` and refuses any other
// length before it looks at the request (see productionEditorInlineChannel and
// productionEditorInlineSession).
//
// # Why the reply envelope is one envelope for all four names
//
// The web client returns `result` verbatim (src/services/browser-transport.ts:
// 156-161) and only turns a non-2xx body into a typed error. None of the four
// names answers a business DTO the renderer could interrogate: begin and
// read-recovery must yield an EditorInlineRecovery object
// (src/shared/editor-inline-generation.ts:21-26), execute a
// MainGenerationExecuteReceipt, and cancel the MainGenerationRunView itself
// (src/services/editor-inline-generation.ts:30-38 compares the returned handle
// field by field). A `{success:false,error}` body would be handed to those readers
// as a value and fail their own checks, so every name uses the family read failure
// envelope already in this package (productionGraphReadFailure/
// productionGraphReadCode: a *security.HTTPError surfaces as itself, anything else
// becomes a symbolic 500). The request is read the way the reference reads it:
// `request.handle` with every other key ignored (generation-controller.ts:249-251),
// so no stricter envelope is invented here.
//
// # begin and execute: served, with the shipped template
//
// Both were refused in a previous pass, on two premises that do not hold: that no
// Go file could name the builtin `edit_selected_text` template, and that an
// editor-inline execute would need a model call only llm:generate admits. The
// template ships (src/services/builtin-prompt-templates.ts:212 with its en-US
// overlay at src/services/prompt-language.ts:141) and now reaches this binary as a
// committed asset (internal/prompttemplate), and the model call is this lane's own:
// generation:execute and graph-generation:execute already dispatch through the
// lease and the one OrdinaryService. production_editor_inline_write_linux.go:24-123
// records the whole composition - the frozen context, the rendered task, the one
// dispatch, and the two narrowings it still keeps.
//
// The admission did not have to widen: openRun already merges a frozen manifest
// stub (synthetic_generation_linux.go:879-951), and the ledger's closed family
// allowlist (internal/store/generation_ledger.go:495-526) refuses only the keys it
// already names, which editorInline* is not. Nothing in the store changes, and this
// file changes nothing there.
//
// # read-recovery: served, from the database
//
// read-recovery reads a frozen run and hands back its own state; it dispatches
// nothing, so it is served. The proof is the reference's, in the reference's order
// (electron/services/main-generation-owner.ts:988-1004 ->
// electron/services/editor-inline-generation.ts:34-42):
//
//  1. the handle is the closed four-key shape and names this project
//     (requireEditorInlineRun, :990-991), and its runId names a run this database
//     holds (:992);
//  2. the manifest names operation 'editor-inline', sha256 of the stored
//     editorInlineContext text equals the frozen editorInlineContextHash, and
//     sha256 of its documentText equals its documentHash (:34-38);
//  3. the frozen editorInlineInput satisfies the renderer's own validator and
//     equals the context's projection of itself (:39-41);
//  4. the run is the one the handle names: rootActionId, projectId, and an epoch
//     that is either the binding's or the run's own editorInlineOriginEpoch
//     (:994-995).
//
// Step 2 is order-sensitive: the desktop hashes JSON.stringify of an
// insertion-ordered object (textHash in electron/repositories/
// generation-run-repository.ts:92-93), and the key order lives only in the stored
// binding text - the ledger's read path decodes it into maps
// (internal/store/generation_ledger.go:150-164). This adapter therefore hashes the
// EXACT stored bytes of that value, extracted from the binding document by a
// token walk (productionEditorInlineStoredContext), never a re-serialization: a
// re-marshalled copy would answer the same digest only by accident and would
// refuse a genuine run whose keys were written in another order.
//
// Step 3 uses the renderer's own arithmetic: from/to are UTF-16 code-unit offsets
// into documentText and `to > from` must hold with
// documentText.slice(from, to) === selectedText
// (electron/services/editor-inline-generation.ts:20-28), so the checks here count
// and slice code units, not bytes and not runes.
//
// sourceStatus is answered by the reference's rule: 'current' only when the root is
// not cancelled and the run's frozen binding still reproduces - this lane's
// reproduction is Execute's own check (synthetic_generation_linux.go:1310-1326:
// resolve the begin-time lease, rebuild the sources, compare the fingerprint).
// A run this lane opened carries goOwnerSourceFingerprint and its lease, so a
// healthy run answers 'current' and the renderer's execute gate opens; a run
// written by another build carries neither, and the honest answer for it stays the
// reference's own fallback, 'conflict' (:1002 keeps the original text readable
// after a source or model change).
//
// # cancel: served, as the family's own stop
//
// cancel settles the root and returns the post-stop view
// (main-generation-owner.ts:1414-1418: require the editor-inline run, cancel its
// root, project the run again). This file performs exactly that over the ordinary
// service the rest of this lane stops runs with, so a cancel writes the ledger
// control rows and nothing else and never reaches the executor's Egress: the same
// two gates generation:cancel reads come first (the committed model-egress policy,
// then the executor the service is built over -
// production_generation_lifecycle_linux.go:61-75). The stop is not routed through
// syntheticGeneration.Cancel because that path compares the binding epoch with the
// caller's CURRENT lease (syntheticRequireRun, :1181-1203), while this family's
// identity check is the epoch-tolerant one above; a recovery cancel of a run frozen
// under an earlier lease is exactly the case the reference allows.
//
// A repeated cancel answers the already cancelled view instead of a settled refusal:
// the ledger's stop returns before writing when the root is already cancelled
// (internal/store/generation_ledger.go:846-849) and the reference forwards that view,
// unlike generation:cancel, which is a user-visible stop where "changed nothing"
// must not read as success (production_generation_lifecycle_linux.go:16-24).

const (
	productionEditorInlineReadRecoveryName = "editor-inline:read-recovery"
	productionEditorInlineCancelName       = "editor-inline:cancel"
	// productionEditorInlineOperation is the single sourceManifest.operation this
	// family freezes (generation-source-binding.ts:597).
	productionEditorInlineOperation = "editor-inline"
	// productionEditorInlineIdentityMismatch is requireEditorInlineRun's only refusal
	// (main-generation-owner.ts:990-995).
	productionEditorInlineIdentityMismatch = "GENERATION_RUN_IDENTITY_MISMATCH"
	// productionEditorInlineContextInvalid is readEditorInlineContext's refusal
	// (electron/services/editor-inline-generation.ts:34-41).
	productionEditorInlineContextInvalid = "GENERATION_EDITOR_CONTEXT_INVALID"
	// productionEditorInlineInputInvalid is validateEditorInlineInput's own code
	// (:20-28), reached when the frozen input is malformed rather than mismatched.
	productionEditorInlineInputInvalid = "GENERATION_EDITOR_INPUT_INVALID"
	// productionEditorInlineRunMissing is the ledger's and the repository's code for
	// a run id this database does not hold (electron/repositories/
	// generation-run-repository.ts:126).
	productionEditorInlineRunMissing = "GENERATION_RUN_MISSING"
	// productionEditorInlineInstructionMax is EDITOR_INLINE_INSTRUCTION_MAX_LENGTH,
	// counted in code points, not UTF-16 units
	// (src/shared/editor-inline-generation.ts:5-6).
	productionEditorInlineInstructionMax = 2000
	// productionEditorInlineDocumentMax is the 8 MiB UTF-8 document bound
	// (electron/services/editor-inline-generation.ts:26).
	productionEditorInlineDocumentMax = 8 << 20
	// productionEditorInlineMaxSafeInteger is Number.MAX_SAFE_INTEGER: from/to must
	// be safe integers, so a value outside this bound is not one.
	productionEditorInlineMaxSafeInteger int64 = 1<<53 - 1
)

// productionEditorInline serves the two names above over the one generation owner
// this lane built, so both share generation:begin's registry, leases, ledger and
// the single-writer admission instead of constructing a second owner.
type productionEditorInline struct {
	owner *productionGeneration
}

// newProductionEditorInline wraps the owner newProductionGeneration returned. It
// must be called with that same owner; a nil owner installs nothing.
func newProductionEditorInline(owner *productionGeneration) *productionEditorInline {
	return &productionEditorInline{owner: owner}
}

// ready reports whether the owner this file borrows is fully assembled. A lane
// that never built one installs nothing at all.
func (e *productionEditorInline) ready() bool {
	return e != nil && e.owner != nil && e.owner.open != nil && e.owner.inner != nil
}

// Bind installs the four served names. The admitting half is bound first
// (production_editor_inline_write_linux.go), so a run exists before the reads that
// project one can be asked about it. A nil owner is not an error: the names keep
// failing closed at GO_OPERATION_NOT_MIGRATED, exactly like the sibling binders.
func (e *productionEditorInline) Bind(server *transport.Server) error {
	if server == nil {
		return errors.New("TRANSPORT_SERVER_REQUIRED")
	}
	if !e.ready() {
		return nil
	}
	for _, bind := range []struct {
		name string
		call func(context.Context, transport.Caller, map[string]any, map[string]any) (any, error)
	}{
		{productionEditorInlineBeginName, e.begin},
		{productionEditorInlineExecuteName, e.execute},
	} {
		if err := server.Bind(bind.name, productionEditorInlineWriteChannel(bind.call)); err != nil {
			return err
		}
	}
	read := productionEditorInlineChannel(e.read)
	if err := server.Bind(productionEditorInlineReadRecoveryName, read); err != nil {
		return err
	}
	cancel := productionEditorInlineChannel(e.cancel)
	return server.Bind(productionEditorInlineCancelName, cancel)
}

// productionEditorInlineChannel is the wire half both names share: the channel's
// one request plus the frozen three-key project context at args[1]. The context
// shape and its refusal codes are the family adapter's own
// (internal/transport/generation.go:260-304 and this package's
// productionGenerationMutation), and the failure envelope is the read half's.
func productionEditorInlineChannel(call func(context.Context, transport.Caller, map[string]any, syntheticHandle) (any, error)) transport.Operation {
	return func(ctx context.Context, caller transport.Caller, args []any) (any, error) {
		expected, err := productionEditorInlineSession(args)
		if err != nil {
			return productionGraphReadFailure(err)
		}
		handle, err := productionEditorInlineRequest(args[0])
		if err != nil {
			return productionGraphReadFailure(err)
		}
		if ctx == nil {
			return productionGraphReadFailure(security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS"))
		}
		value, err := call(ctx, caller, expected, handle)
		if err != nil {
			return productionGraphReadFailure(err)
		}
		return value, nil
	}
}

// productionEditorInlineRequest unwraps `{ handle }` the way the reference does
// (generation-controller.ts:249-251 hands `request.handle` to the owner). The
// handle itself is the four-key shape requireEditorInlineRun accepts: any other key
// is its identity mismatch, projectId and epoch must be non-empty strings, and the
// other two are compared against the stored row instead of being validated here,
// exactly like the original's comparisons.
func productionEditorInlineRequest(value any) (syntheticHandle, error) {
	request := syntheticMap(value)
	if request == nil {
		return syntheticHandle{}, errors.New(productionEditorInlineIdentityMismatch)
	}
	raw := syntheticMap(request["handle"])
	if raw == nil {
		return syntheticHandle{}, errors.New(productionEditorInlineIdentityMismatch)
	}
	for key := range raw {
		switch key {
		case "projectId", "epoch", "rootActionId", "runId":
		default:
			return syntheticHandle{}, errors.New(productionEditorInlineIdentityMismatch)
		}
	}
	handle := syntheticHandle{
		ProjectID:    syntheticString(raw["projectId"]),
		Epoch:        syntheticString(raw["epoch"]),
		RootActionID: syntheticString(raw["rootActionId"]),
		RunID:        syntheticString(raw["runId"]),
	}
	if handle.ProjectID == "" || handle.Epoch == "" {
		return syntheticHandle{}, errors.New(productionEditorInlineIdentityMismatch)
	}
	return handle, nil
}

// productionEditorInlineRun is one proved editor-inline run: the ledger row, its
// binding and manifest, the verified context and the model the run froze.
type productionEditorInlineRun struct {
	Run      map[string]any
	Manifest map[string]any
	ModelID  string
	Context  map[string]any
}

// resolve proves one editor-inline run for the two served names. The terms are the
// reference's, in the reference's order: the handle's project and the run row it
// names, then the frozen context, then the run identity.
func (e *productionEditorInline) resolve(ctx context.Context, scope generationowner.Scope, ledger *store.GenerationLedger, db *sql.DB, handle syntheticHandle) (*productionEditorInlineRun, error) {
	// requireEditorInlineRun's project term: the handle must name the project this
	// frozen session owns, never merely the project the request mentions
	// (main-generation-owner.ts:991).
	if handle.ProjectID != scope.Project {
		return nil, errors.New(productionEditorInlineIdentityMismatch)
	}
	args, err := syntheticArgs(handle.RunID)
	if err != nil {
		return nil, errors.New(productionEditorInlineRunMissing)
	}
	// The run row the handle names (:992). A run this database does not hold answers
	// the ledger's own RUN_MISSING code, which is the code the reference's
	// repository.get uses too (electron/repositories/generation-run-repository.ts:126).
	value, err := ledger.CallJSON(ctx, "get", args)
	if err != nil {
		return nil, err
	}
	run := syntheticMap(value)
	if run == nil {
		return nil, errors.New(productionEditorInlineRunMissing)
	}
	binding := syntheticMap(run["binding"])
	if binding == nil {
		return nil, errors.New(productionEditorInlineRunMissing)
	}
	// readEditorInlineContext (electron/services/editor-inline-generation.ts:34-41),
	// in its own order: the operation, the context digest, the document digest, then
	// the frozen input. A run of another family answers the context refusal, never a
	// context: the projection of a run this lane did not freeze is not the author's.
	manifest := syntheticMap(binding["sourceManifest"])
	if manifest == nil || syntheticString(manifest["operation"]) != productionEditorInlineOperation {
		return nil, errors.New(productionEditorInlineContextInvalid)
	}
	frozen := syntheticString(manifest["editorInlineContextHash"])
	if frozen == "" {
		return nil, errors.New(productionEditorInlineContextInvalid)
	}
	// The digest is over the exact stored text of the context value, because that is
	// the only place its insertion order still exists; see this file's header.
	raw, err := productionEditorInlineStoredContext(ctx, db, handle.RunID)
	if err != nil {
		return nil, err
	}
	if syntheticHash(string(raw)) != frozen {
		return nil, errors.New(productionEditorInlineContextInvalid)
	}
	contextValue, err := productionEditorInlineDecodeContext(raw)
	if err != nil {
		return nil, errors.New(productionEditorInlineContextInvalid)
	}
	// textHash(context.documentText) === context.documentHash
	document, ok := contextValue["documentText"].(string)
	if !ok || syntheticHash(document) != syntheticString(contextValue["documentHash"]) {
		return nil, errors.New(productionEditorInlineContextInvalid)
	}
	// validateEditorInlineInput then isDeepStrictEqual(input, projection).
	input, err := productionEditorInlineFrozenInput(syntheticMap(manifest["editorInlineInput"]))
	if err != nil {
		return nil, err
	}
	if !productionEditorInlineProjectionSame(input, contextValue) {
		return nil, errors.New(productionEditorInlineContextInvalid)
	}
	// The run identity terms (:994-995): the row the handle names, under the project
	// it claims, at the epoch it was frozen with - or the origin epoch frozen beside
	// that binding, which is what makes a recovery after a lease rotation answer.
	if syntheticString(run["rootActionId"]) != handle.RootActionID ||
		syntheticString(run["runId"]) != handle.RunID ||
		syntheticString(binding["projectId"]) != handle.ProjectID {
		return nil, errors.New(productionEditorInlineIdentityMismatch)
	}
	if handle.Epoch != syntheticString(binding["epoch"]) &&
		handle.Epoch != syntheticString(manifest["editorInlineOriginEpoch"]) {
		return nil, errors.New(productionEditorInlineIdentityMismatch)
	}
	return &productionEditorInlineRun{Run: run, Manifest: manifest,
		ModelID: productionEditorInlineModelID(manifest), Context: contextValue}, nil
}

// read answers editor-inline:read-recovery with the reference's four fields: the
// run view, the frozen model id, the proved context and the source status. It
// reserves no attempt, resolves no credential and opens no egress endpoint, so a
// lane with no committed model origin still serves it. The envelope is assembled by
// recovery, which begin answers with too, so the two cannot answer different shapes
// for the same run.
func (e *productionEditorInline) read(ctx context.Context, caller transport.Caller, expected map[string]any, handle syntheticHandle) (any, error) {
	if !e.ready() {
		return nil, errProductionGenerationUnavailable
	}
	_, scope, ledger, db, done, err := e.owner.inner.admit(ctx, caller, expected)
	if err != nil {
		return nil, err
	}
	defer done()
	return e.recovery(ctx, scope, ledger, db, handle)
}

// cancel answers editor-inline:cancel: the family's own stop over the ordinary
// service, then the re-projected run. A malformed request, a foreign handle or an
// unprovable context is refused before any row is written.
func (e *productionEditorInline) cancel(ctx context.Context, caller transport.Caller, expected map[string]any, handle syntheticHandle) (any, error) {
	if !e.ready() {
		return nil, errProductionGenerationUnavailable
	}
	if _, err := e.owner.open.ModelEgress(); err != nil {
		return nil, err
	}
	if e.owner.inner.executor == nil {
		return nil, errProductionGenerationExecuteUnavailable
	}
	contextValue, scope, ledger, db, done, err := e.owner.inner.admit(ctx, caller, expected)
	if err != nil {
		return nil, err
	}
	defer done()
	resolved, err := e.resolve(ctx, scope, ledger, db, handle)
	if err != nil {
		return nil, err
	}
	service, err := e.owner.inner.newService(ctx, caller, contextValue, ledger, db, scope, handle)
	if err != nil {
		return nil, err
	}
	rootID := syntheticString(resolved.Run["rootActionId"])
	if rootID == "" {
		return nil, errors.New(productionEditorInlineIdentityMismatch)
	}
	if _, err := service.Cancel(ctx, rootID); err != nil {
		return nil, err
	}
	current, err := syntheticCall(ctx, ledger, "get", handle.RunID)
	if err != nil {
		return nil, err
	}
	return e.owner.inner.viewOf(ctx, ledger, db, current)
}

// sourceStatus is the reference's currency answer (main-generation-owner.ts:
// 998-1004): 'current' only when the root is not cancelled and the frozen binding
// still reproduces. Reproduction is Execute's own three steps, run over the scope
// this call was admitted with - the same scope Execute uses - so it can answer
// 'current' only for a run frozen under that very lease and epoch; anything else
// answers 'conflict' instead of applying text to sources the run never froze.
func (e *productionEditorInline) sourceStatus(ctx context.Context, scope generationowner.Scope, ledger *store.GenerationLedger, db *sql.DB, resolved *productionEditorInlineRun) string {
	conflict := "conflict"
	// Every conflict branch names the step symbolically, never the underlying error,
	// so a CI run can say which input to reproduce failed without echoing a path or a
	// provider message.
	conflictAt := func(step string) string {
		log.Printf("novel-server: editor-inline sourceStatus=conflict step=%s", step)
		return conflict
	}
	if e.owner.inner.registry == nil {
		return conflictAt("registry-missing")
	}
	budget, err := syntheticCall(ctx, ledger, "budget", syntheticString(resolved.Run["rootActionId"]))
	if err != nil {
		return conflictAt("budget-read")
	}
	if syntheticString(syntheticMap(syntheticMap(budget)["root"])["status"]) == "cancelled" {
		return conflictAt("budget-cancelled")
	}
	fingerprint := syntheticString(resolved.Manifest["goOwnerSourceFingerprint"])
	// A run opened by another build freezes no source fingerprint, so this lane
	// cannot claim its sources are unchanged; the reference's own fallback is
	// conflict. A run this lane froze reproduces through the one helper execute
	// shares, so the two cannot disagree about whether the binding still holds.
	if fingerprint == "" {
		return conflictAt("fingerprint-missing")
	}
	_, current, err := e.reproduce(ctx, scope, db, resolved)
	if err != nil {
		// reproduce logs its own granular step (lease-missing/resolve/rebuild/bind).
		return conflict
	}
	if current != fingerprint {
		return conflictAt("fingerprint-mismatch")
	}
	return "current"
}

// productionEditorInlineModelID reads the model the run froze. The desktop nests it
// in the binding's model receipt, this lane's own manifest spells it as a scalar
// beside it (synthetic_generation_linux.go:737-740); both are read, and an absent
// value stays empty rather than being filled in.
func productionEditorInlineModelID(manifest map[string]any) string {
	if receipt := syntheticMap(manifest["modelReceipt"]); receipt != nil {
		if modelID := syntheticString(receipt["modelId"]); modelID != "" {
			return modelID
		}
	}
	return syntheticString(manifest["modelId"])
}

// productionEditorInlineStoredContext returns the exact stored text of the
// editorInlineContext value inside one run's binding document. The ledger's read
// path decodes bindings into maps, which loses key order; the digest this file has
// to reproduce was taken over the ordered text, so the text itself is read here.
// It is a plain SELECT on the row the ledger just answered for.
func productionEditorInlineStoredContext(ctx context.Context, db *sql.DB, runID string) ([]byte, error) {
	if db == nil {
		return nil, errors.New(productionEditorInlineContextInvalid)
	}
	var stored string
	err := db.QueryRowContext(ctx, `SELECT binding_json FROM generation_runs WHERE run_id=?`, runID).Scan(&stored)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errors.New(productionEditorInlineRunMissing)
		}
		return nil, err
	}
	manifest, ok := productionEditorInlineRawMember([]byte(stored), "sourceManifest")
	if !ok {
		return nil, errors.New(productionEditorInlineContextInvalid)
	}
	raw, ok := productionEditorInlineRawMember(manifest, "editorInlineContext")
	if !ok || len(raw) == 0 {
		return nil, errors.New(productionEditorInlineContextInvalid)
	}
	// Only the object shape is this family's frozen context. A value stored as a
	// quoted string would reach the hash with its quotes and escapes still attached,
	// which is not the text the desktop hashed, so it is refused rather than hashed
	// as something else.
	if raw[0] != '{' {
		return nil, errors.New(productionEditorInlineContextInvalid)
	}
	return raw, nil
}

// productionEditorInlineRawMember returns the raw bytes of one member of a JSON
// object, without re-serializing anything: json.RawMessage receives the value's
// exact input segment, so an object member comes back as the object's own text
// (a string member comes back quoted and escaped, which is the segment, not the
// string). A repeated key resolves to the last occurrence, which is the value
// JSON.parse keeps.
func productionEditorInlineRawMember(document []byte, name string) ([]byte, bool) {
	decoder := json.NewDecoder(bytes.NewReader(document))
	decoder.UseNumber()
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, false
	}
	var found []byte
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return nil, false
		}
		text, ok := key.(string)
		if !ok {
			return nil, false
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, false
		}
		if text == name {
			found = append(found[:0], value...)
		}
	}
	if found == nil {
		return nil, false
	}
	return found, true
}

// productionEditorInlineDecodeContext decodes the proved context text with number
// literals kept, so from/to keep their exact value instead of becoming floats.
func productionEditorInlineDecodeContext(raw []byte) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New(productionEditorInlineContextInvalid)
	}
	return object, nil
}

// productionEditorInlineInput is one validated editor-inline request as the
// renderer defines it (src/shared/editor-inline-generation.ts:7-14).
type productionEditorInlineInput struct {
	Action       string
	DocumentText string
	From         int64
	To           int64
	SelectedText string
	Instruction  string
	Instruct     bool
}

// productionEditorInlineFrozenInput is validateEditorInlineInput
// (electron/services/editor-inline-generation.ts:20-27): a closed key set, one of
// the four fixed actions without an instruction or 'instruct' with one, safe
// integer offsets inside the document, and the selection the author really had.
func productionEditorInlineFrozenInput(value map[string]any) (productionEditorInlineInput, error) {
	invalid := errors.New(productionEditorInlineInputInvalid)
	if value == nil {
		return productionEditorInlineInput{}, invalid
	}
	for key := range value {
		switch key {
		case "action", "documentText", "from", "to", "selectedText", "instruction":
		default:
			return productionEditorInlineInput{}, invalid
		}
	}
	input := productionEditorInlineInput{
		Action:       syntheticString(value["action"]),
		DocumentText: syntheticString(value["documentText"]),
		SelectedText: syntheticString(value["selectedText"]),
	}
	if _, ok := value["documentText"].(string); !ok {
		return productionEditorInlineInput{}, invalid
	}
	if _, ok := value["selectedText"].(string); !ok {
		return productionEditorInlineInput{}, invalid
	}
	instruction, hasInstruction := value["instruction"]
	switch input.Action {
	case "instruct":
		text, ok := instruction.(string)
		if !hasInstruction || !ok {
			return productionEditorInlineInput{}, invalid
		}
		input.Instruct = true
		input.Instruction = text
		// The bound counts code points, not UTF-16 units
		// (src/shared/editor-inline-generation.ts:5-6).
		if strings.TrimSpace(input.Instruction) == "" || len([]rune(input.Instruction)) > productionEditorInlineInstructionMax {
			return productionEditorInlineInput{}, invalid
		}
	case "refine", "expand", "continue", "dialogue":
		// A fixed action carries no instruction key at all
		// (electron/services/editor-inline-generation.ts:24-26).
		if hasInstruction {
			return productionEditorInlineInput{}, invalid
		}
	default:
		return productionEditorInlineInput{}, invalid
	}
	from, ok := productionEditorInlineInteger(value["from"])
	if !ok || from < 0 {
		return productionEditorInlineInput{}, invalid
	}
	to, ok := productionEditorInlineInteger(value["to"])
	if !ok || to <= from {
		return productionEditorInlineInput{}, invalid
	}
	if to > productionEditorInlineUTF16Length(input.DocumentText) {
		return productionEditorInlineInput{}, invalid
	}
	if len(input.DocumentText) > productionEditorInlineDocumentMax {
		return productionEditorInlineInput{}, invalid
	}
	// documentText.slice(from, to) === selectedText, in code units.
	selection, ok := productionEditorInlineUTF16Slice(input.DocumentText, from, to)
	if !ok || selection != input.SelectedText {
		return productionEditorInlineInput{}, invalid
	}
	input.From, input.To = from, to
	return input, nil
}

// productionEditorInlineProjectionSame is the reference's isDeepStrictEqual against
// the projection it builds from the context (electron/services/editor-inline-generation.ts:
// 40-41): the five author fields always, the instruction only when the action is
// 'instruct'.
func productionEditorInlineProjectionSame(input productionEditorInlineInput, contextValue map[string]any) bool {
	if syntheticString(contextValue["action"]) != input.Action ||
		syntheticString(contextValue["documentText"]) != input.DocumentText ||
		syntheticString(contextValue["selectedText"]) != input.SelectedText {
		return false
	}
	from, ok := productionEditorInlineInteger(contextValue["from"])
	if !ok || from != input.From {
		return false
	}
	to, ok := productionEditorInlineInteger(contextValue["to"])
	if !ok || to != input.To {
		return false
	}
	if input.Instruct {
		instruction, present := contextValue["instruction"]
		return present && syntheticString(instruction) == input.Instruction
	}
	return true
}

// productionEditorInlineInteger accepts only a safe integer, as Number.isSafeInteger
// does: a fractional or out-of-range literal is refused instead of truncated.
func productionEditorInlineInteger(value any) (int64, bool) {
	switch number := value.(type) {
	case json.Number:
		parsed, err := number.Int64()
		if err != nil {
			return 0, false
		}
		return productionEditorInlineSafeInteger(parsed)
	case float64:
		if math.IsNaN(number) || math.IsInf(number, 0) || number != math.Trunc(number) ||
			math.Abs(number) > float64(productionEditorInlineMaxSafeInteger) {
			return 0, false
		}
		return productionEditorInlineSafeInteger(int64(number))
	case int64:
		return productionEditorInlineSafeInteger(number)
	case int:
		return productionEditorInlineSafeInteger(int64(number))
	}
	return 0, false
}

func productionEditorInlineSafeInteger(value int64) (int64, bool) {
	if value > productionEditorInlineMaxSafeInteger || value < -productionEditorInlineMaxSafeInteger {
		return 0, false
	}
	return value, true
}

// productionEditorInlineUTF16Length is String.prototype.length for the document:
// code units, where every rune above the basic plane counts twice.
func productionEditorInlineUTF16Length(text string) int64 {
	units := int64(0)
	for _, r := range text {
		if r > 0xFFFF {
			units += 2
			continue
		}
		units++
	}
	return units
}

// productionEditorInlineUTF16Slice is String.prototype.slice over a validated code
// unit range. A range that begins or ends inside a surrogate pair is refused: such
// a selection is not a well-formed string and could never have arrived as one.
func productionEditorInlineUTF16Slice(text string, from, to int64) (string, bool) {
	if from < 0 || to < from {
		return "", false
	}
	var out strings.Builder
	position := int64(0)
	for _, r := range text {
		width := int64(1)
		if r > 0xFFFF {
			width = 2
		}
		next := position + width
		if position < from && from < next {
			return "", false
		}
		if position < to && to < next {
			return "", false
		}
		if position >= from && next <= to {
			out.WriteRune(r)
		}
		position = next
	}
	if position < to {
		return "", false
	}
	return out.String(), true
}
