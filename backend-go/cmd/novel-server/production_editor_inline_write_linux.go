//go:build linux

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/generationowner"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/prompttemplate"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file serves the two editor-inline names the read slice left unbound, so the
// family's four inventoried names are all answered:
//
//	editor-inline:begin          served   (productionEditorInline.begin)
//	editor-inline:read-recovery  served   (production_editor_inline_linux.go)
//	editor-inline:execute        served   (productionEditorInline.execute)
//	editor-inline:cancel         served   (production_editor_inline_linux.go)
//
// They are inventoried at web/operations.json:188-191 and registered by
// electron/controllers/generation-controller.ts:248-251 with arity 1 each; both
// names here take the request plus the frozen three-key project context, the same
// wire arity the two served names use (production_editor_inline_linux.go:239-269).
//
// # Why the earlier refusal no longer holds
//
// The previous pass refused both names saying begin needs the builtin template
// `edit_selected_text` "which the prompt loader does not name" and execute needs a
// model call "which only llm:generate admits". Both premises are wrong:
//
//   - The template ships. src/services/builtin-prompt-templates.ts:212 is the zh-CN
//     template and src/services/prompt-language.ts:141 its en-US overlay; the
//     desktop resolves it with getBuiltinPromptTemplate and freezes
//     JSON.stringify of the resolved object (electron/controllers/
//     generation-controller.ts:128-132, generation-source-binding.ts:480-505,
//     :560). That object is now a committed asset this binary embeds
//     (internal/prompttemplate/edit-selected-text-v1.json), returned as the exact
//     compact text the desktop freezes, so no prompt prose is written in Go and the
//     digest frozen beside it is the digest of the shipped template.
//
//   - The model call exists and is this lane's own. generation:execute already
//     dispatches through generationowner.Executor behind the single-writer
//     admission (production_generation_begin_linux.go:97-124), and
//     graph-generation:execute already does what an inline execute needs: resolve
//     the begin-time lease, rebuild and re-fingerprint the frozen sources, Prepare
//     the frozen task, and settle through OrdinaryService
//     (production_graph_generation_execute_linux.go:215-276). No second provider
//     path and no change to the model authority is involved: the dispatch is the
//     lease's, and the lease is minted by Registry.Begin inside the run admission.
//
// # begin: what it freezes
//
// The answer is the same four-field EditorInlineRecovery the recovery read answers
// (view, modelId, context, sourceStatus), so the renderer's `open` and `read` run
// one validator over one shape (src/services/editor-inline-generation.ts:20-38).
// The context is the reference's own (electron/services/editor-inline-generation.ts:
// 29-33): the five (or six) author fields, `kind: 'author-draft'`, the sha256 of
// documentText, the writing language, and the resolved PromptTemplate object. It is
// frozen as JSON TEXT, not as a map: the read half hashes the exact stored bytes of
// that member (production_editor_inline_linux.go:521-554, :355-363), so the
// admitting path must hand the ledger the very bytes it hashed. That is why
// openEditorInlineRun writes the request text itself instead of
// marshalling a map: encoding/json sorts map keys and HTML-escapes <, > and &, and
// either would change the digest of a manuscript that contains them.
//
// The task is frozen beside it exactly as the reference freezes it
// (electron/services/editor-inline-generation.ts:66-73: purpose
// `editor-ai-<action>`, reasoningStage `review` for refine and `drafting`
// otherwise, output visible-text, a composed system role and the rendered body).
// Every prose fragment comes from the same committed asset - the body, the output
// contract, the system contract, the four fixed instructions, and the author-note
// rule and prefix - and the renderer is renderPrompt's own
// (src/shared/render-prompt.ts:57-76): fill {{variable}} once, append the builtin
// output contract, then prune. A run frozen here and a run frozen by the desktop
// ask the model the same thing.
//
// # execute: one dispatch, then the attempt settles
//
// execute re-proves the run with the family's own resolve (tolerant of the origin
// epoch, exactly like the reference's require), re-reads the frozen task from the
// stored binding text and re-checks its digest, then walks generation:execute's
// path: resolve the lease, rebuild the sources from the manifest, bind and compare
// the fingerprint, Prepare the frozen task, and hand it to the one OrdinaryService
// with the reference's fixed invocation nonce `editor-inline:0`
// (electron/services/main-generation-owner.ts:1437). The ledger reserves, marks
// dispatched and only then dispatches, and settles that same attempt afterwards, so
// a failure is an attempt that settled, never text this lane invented.
//
// A second execute of the same handle re-derives the same request hash - the task is
// frozen, and the plan is planned from the lease's own frozen model profile
// (internal/generationowner/dispatch.go:56-59) - so the ledger's findInvocation
// answers the settled attempt and no second request is dispatched. That is why the
// reference's explicit GENERATION_EDITOR_ATTEMPT_CONFLICT counter
// (main-generation-owner.ts:1442) has no work to do here: only an identical replay
// can reach the service twice under one frozen lease.
//
// # What this lane deliberately does not do
//
//   - It reads no author-installed override. The desktop prefers
//     `prompts/edit_selected_text[.<lang>].json` over the builtin when one is
//     installed (generation-source-binding.ts:483-505); this lane freezes the
//     shipped template only. An override would have to be resolved from the global
//     data root through internal/promptskills and rendered with the two rewrites the
//     shipped template makes identity (required-context append and optional-section
//     pruning); until that lands, the frozen template is the builtin, and the
//     loader refuses an asset that would need those rewrites.
//   - It does not re-freeze the binding after a lease rotation. The reference
//     resumes a non-replayable run before dispatching (main-generation-owner.ts:
//     1444); here the frozen lease no longer resolves, so execute refuses with the
//     lease's own code instead of pretending the sources are unchanged.

const (
	// productionEditorInlineBeginName is the renderer's begin
	// (electron/controllers/generation-controller.ts:248).
	productionEditorInlineBeginName = "editor-inline:begin"
	// productionEditorInlineExecuteName is the renderer's execute (:250).
	productionEditorInlineExecuteName = "editor-inline:execute"
	// productionEditorInlineExecuteNonce is the one invocation nonce this family
	// mints (electron/services/main-generation-owner.ts:1437).
	productionEditorInlineExecuteNonce = "editor-inline:0"
	// productionEditorInlinePromptKey is the only prompt key a begin freezes
	// (main-generation-owner.ts:1432).
	productionEditorInlinePromptKey = "edit_selected_text"
	// productionEditorInlineRequestInvalid is the reference's code for a begin
	// request of the wrong shape (main-generation-owner.ts:1422), and for a frozen
	// input that fails its own validator.
	productionEditorInlineRequestInvalid = "GENERATION_EDITOR_INPUT_INVALID"
	// productionEditorInlineTemplateInvalid is buildEditorInlineContext's refusal
	// for a template that is not the shipped one
	// (electron/services/editor-inline-generation.ts:31).
	productionEditorInlineTemplateInvalid = "GENERATION_EDITOR_TEMPLATE_INVALID"
	// productionEditorInlineTaskInvalid is readEditorInlineTask's refusal (:45-49).
	productionEditorInlineTaskInvalid = "GENERATION_EDITOR_TASK_INVALID"
	// productionEditorInlineNonceConflict is the replay refusal for a spent nonce
	// that froze a different input or model (main-generation-owner.ts:1428).
	productionEditorInlineNonceConflict = "GENERATION_NONCE_CONFLICT"
	// productionEditorInlineLeaseMissing and productionEditorInlineSourceChanged are
	// the dispatch gates this lane shares with generation:execute
	// (synthetic_generation_linux.go:1313-1327).
	productionEditorInlineLeaseMissing = "GENERATION_LEASE_MISSING"
	productionEditorInlineSourceChanged = "GENERATION_SOURCE_CHANGED"
	// productionEditorInlineNonceMax is the reference's bound on uiActionNonce
	// (main-generation-owner.ts:1422).
	productionEditorInlineNonceMax = 256
	// productionEditorInlineVisibleText is the one output the family freezes
	// (main-generation-owner.ts:1432).
	productionEditorInlineVisibleText = "visible-text"
)

// productionEditorInlineToken is renderPrompt's interpolation token,
// /\{\{(\w+)\}\}/g (src/shared/render-prompt.ts:35).
var productionEditorInlineToken = regexp.MustCompile(`\{\{(\w+)\}\}`)

// productionEditorInlineWriteChannel is the wire half the two served names share:
// the channel's one request plus the frozen three-key project context at args[1].
// The fence and its two codes are the read half's own
// (production_editor_inline_linux.go:239-269), and the failure envelope is the same
// read envelope, because both names answer a value the renderer consumes verbatim
// and only a non-2xx body becomes a typed error there
// (src/services/browser-transport.ts:156-164).
func productionEditorInlineWriteChannel(call func(context.Context, transport.Caller, map[string]any, map[string]any) (any, error)) transport.Operation {
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

// productionEditorInlineSession validates the trailing frozen context both served
// halves take: exactly two wire arguments, and a context that is exactly the
// projectId/leaseId/projectPath triple of non-empty strings.
func productionEditorInlineSession(args []any) (map[string]any, error) {
	if len(args) != 2 {
		return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
	}
	raw, ok := args[1].(map[string]any)
	if !ok || len(raw) != 3 {
		return nil, security.NewHTTPError(http.StatusBadRequest, "PROJECT_SESSION_REQUIRED")
	}
	expected := make(map[string]any, 3)
	for _, key := range []string{"projectId", "leaseId", "projectPath"} {
		text, ok := raw[key].(string)
		if !ok || text == "" || !utf8.ValidString(text) {
			return nil, security.NewHTTPError(http.StatusBadRequest, "PROJECT_SESSION_REQUIRED")
		}
		expected[key] = text
	}
	return expected, nil
}

// begin answers editor-inline:begin. A malformed request is refused before any row
// is written; a spent nonce answers the run it already opened; anything else opens
// one run whose frozen context and task are the shipped prompt.
func (e *productionEditorInline) begin(ctx context.Context, caller transport.Caller, expected map[string]any, request map[string]any) (any, error) {
	if !e.ready() {
		return nil, errProductionGenerationUnavailable
	}
	// The same gate generation:begin passes first: a lane with no committed model
	// boundary refuses before a root row exists, rather than freezing a run whose
	// dispatch could never leave.
	if _, err := e.owner.open.ModelEgress(); err != nil {
		return nil, err
	}
	// The reference asserts the session before it reads the request
	// (main-generation-owner.ts:1420), so a malformed begin under a lease this server
	// never issued is the session's refusal, not this family's.
	_, scope, ledger, db, done, err := e.owner.inner.admit(ctx, caller, expected)
	if err != nil {
		return nil, err
	}
	defer done()
	input, modelID, nonce, err := productionEditorInlineBeginRequest(request)
	if err != nil {
		return nil, err
	}
	// The reference's same-nonce look-up runs before anything is opened
	// (main-generation-owner.ts:1424-1430): a nonce this project already spent is a
	// replay when it froze the same input and model, and a conflict when it did not.
	prior, err := productionEditorInlinePriorRun(ctx, db, scope.Project, nonce)
	if err != nil {
		return nil, err
	}
	if prior != nil {
		frozen, err := productionEditorInlineFrozenInput(syntheticMap(prior.Manifest["editorInlineInput"]))
		if err != nil || frozen != input || syntheticString(prior.Manifest["modelId"]) != modelID {
			return nil, errors.New(productionEditorInlineNonceConflict)
		}
		// The replay answers the run the nonce already names, at the epoch that run
		// was frozen under - not the caller's current lease.
		handle := syntheticHandle{ProjectID: scope.Project, Epoch: prior.Epoch,
			RootActionID: prior.RootActionID, RunID: prior.RunID}
		return e.recovery(ctx, scope, ledger, db, handle)
	}
	opened, err := e.openEditorInlineRun(ctx, scope, ledger, db, input, modelID, nonce)
	if err != nil {
		return nil, err
	}
	handle, err := productionEditorInlineRunHandle(scope.Project, scope.Epoch, opened)
	if err != nil {
		return nil, err
	}
	// The run is answered through the same proof the recovery read performs, so a
	// begin can only answer a run its own read would serve.
	return e.recovery(ctx, scope, ledger, db, handle)
}

// reproduce re-derives the frozen source binding for a resolved run: the exact
// reproduction syntheticGeneration.Execute uses to gate a dispatch (resolve the
// begin-time lease, rebuild the frozen sources, bind them and read the digest). It
// returns the rebuilt binding and its fingerprint, or an error when the lease, the
// sources or the model no longer reproduce. begin's sourceStatus and execute share
// this one helper, so a source that reproduces in execute can never read "conflict"
// here and vice versa.

// productionEditorInlineTrace is a length + prefix for a diagnostic log line: it
// names the shape of an identifier without printing the whole value.
func productionEditorInlineTrace(value string) string {
	if value == "" {
		return "-"
	}
	prefix := value
	if len(prefix) > 8 {
		prefix = prefix[:8]
	}
	return prefix + "…(" + strconv.Itoa(len(value)) + ")"
}

func (e *productionEditorInline) reproduce(ctx context.Context, scope generationowner.Scope, db *sql.DB, resolved *productionEditorInlineRun) (*generationowner.Binding, string, error) {
	// Each refusal names the step symbolically (never the error text), so a CI run
	// can tell which input to the shared reproduction failed.
	//
	// The begin-time lease was minted under the run's own frozen project and epoch
	// (openEditorInlineRun writes both from the scope it admitted). Resolve and
	// BindSources must use THAT scope, not the caller's live session lease: a
	// recovery read of a run frozen under an earlier lease still reproduces, exactly
	// as the reference proves the live lease at admission and the run's own epoch
	// separately (main-generation-owner.ts:990-995).
	runScope := scope
	if binding := syntheticMap(syntheticMap(resolved.Run)["binding"]); binding != nil {
		runScope.Project = syntheticString(binding["projectId"])
		runScope.Epoch = syntheticString(binding["epoch"])
	}
	leaseID := syntheticString(resolved.Manifest["modelLeaseId"])
	if leaseID == "" {
		log.Printf("novel-server: editor-inline sourceStatus=conflict step=%s", "lease-missing")
		return nil, "", errors.New(productionEditorInlineLeaseMissing)
	}
	if _, err := e.owner.inner.registry.Resolve(ctx, leaseID, runScope, syntheticGenerationPurpose); err != nil {
		// Names, not values: enough to see whether the rebuilt scope's epoch differs
		// from what the registry holds, without echoing any secret in full.
		log.Printf("novel-server: editor-inline sourceStatus=conflict step=resolve project=%s epoch=%s lease=%s",
			productionEditorInlineTrace(runScope.Project), productionEditorInlineTrace(runScope.Epoch),
			productionEditorInlineTrace(leaseID))
		return nil, "", err
	}
	set, err := e.owner.inner.rebuildSourcesForRun(ctx, db, resolved.Manifest)
	if err != nil {
		log.Printf("novel-server: editor-inline sourceStatus=conflict step=%s", "rebuild")
		return nil, "", err
	}
	binding, err := e.owner.inner.registry.BindSources(ctx, runScope, leaseID, set.Sources, set.Selection)
	if err != nil {
		log.Printf("novel-server: editor-inline sourceStatus=conflict step=%s", "bind")
		return nil, "", err
	}
	return binding, binding.Fingerprint(), nil
}

// execute answers editor-inline:execute: the one dispatch of the frozen task, then
// the settled attempt projected as the browser's MainGenerationExecuteReceipt.
func (e *productionEditorInline) execute(ctx context.Context, caller transport.Caller, expected map[string]any, request map[string]any) (any, error) {
	if !e.ready() {
		return nil, errProductionGenerationUnavailable
	}
	if _, err := e.owner.open.ModelEgress(); err != nil {
		return nil, err
	}
	if e.owner.inner.executor == nil {
		return nil, errProductionGenerationExecuteUnavailable
	}
	handle, err := productionEditorInlineRequest(request)
	if err != nil {
		return nil, err
	}
	contextValue, scope, ledger, db, done, err := e.owner.inner.admit(ctx, caller, expected)
	if err != nil {
		return nil, err
	}
	defer done()
	// requireEditorInlineRun's terms: the run the handle names, under this project,
	// at the epoch it was frozen with - and the frozen context proved from its own
	// stored bytes (main-generation-owner.ts:988-996).
	resolved, err := e.resolve(ctx, scope, ledger, db, handle)
	if err != nil {
		return nil, err
	}
	taskText, err := productionEditorInlineStoredTask(ctx, db, handle.RunID)
	if err != nil {
		return nil, err
	}
	if syntheticHash(string(taskText)) != syntheticString(resolved.Manifest["editorInlineTaskHash"]) {
		return nil, errors.New(productionEditorInlineTaskInvalid)
	}
	task, err := generationowner.ParseTask(taskText)
	if err != nil {
		return nil, err
	}
	binding, fingerprint, err := e.reproduce(ctx, scope, db, resolved)
	if err != nil {
		return nil, err
	}
	if fingerprint != syntheticString(resolved.Manifest["goOwnerSourceFingerprint"]) {
		return nil, errors.New(productionEditorInlineSourceChanged)
	}
	leaseID := syntheticString(resolved.Manifest["modelLeaseId"])
	frozen, err := e.owner.inner.executor.Prepare(ctx, scope, leaseID, task, syntheticRootOutputTokens, binding)
	if err != nil {
		return nil, err
	}
	service, err := e.owner.inner.newService(ctx, caller, contextValue, ledger, db, scope, handle)
	if err != nil {
		return nil, err
	}
	receipt, err := service.Execute(ctx, frozen, handle.RunID, productionEditorInlineExecuteNonce)
	if err != nil {
		return nil, err
	}
	outcome := syntheticOutcome(syntheticMap(receipt))
	view, err := e.owner.inner.viewOf(ctx, ledger, db, syntheticMap(receipt)["run"])
	if err != nil {
		return nil, err
	}
	return map[string]any{"outcome": outcome, "run": view}, nil
}

// recovery is the answer begin and read-recovery both give: the run view, the
// frozen model, the proved context and the reference's currency answer. It is the
// only place either name assembles that envelope, so the two cannot drift.
func (e *productionEditorInline) recovery(ctx context.Context, scope generationowner.Scope, ledger *store.GenerationLedger, db *sql.DB, handle syntheticHandle) (any, error) {
	resolved, err := e.resolve(ctx, scope, ledger, db, handle)
	if err != nil {
		return nil, err
	}
	view, err := e.owner.inner.viewOf(ctx, ledger, db, resolved.Run)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"view":         view,
		"modelId":      resolved.ModelID,
		"context":      resolved.Context,
		"sourceStatus": e.sourceStatus(ctx, scope, ledger, db, resolved),
	}, nil
}

// productionEditorInlineBeginRequest is the reference's begin envelope
// (main-generation-owner.ts:1419-1423): exactly `{ input, modelId, uiActionNonce }`,
// a nonce of at most 256 characters, a model id, and an input the family's own
// validator accepts.
func productionEditorInlineBeginRequest(value map[string]any) (productionEditorInlineInput, string, string, error) {
	invalid := errors.New(productionEditorInlineRequestInvalid)
	if value == nil {
		return productionEditorInlineInput{}, "", "", invalid
	}
	for key := range value {
		switch key {
		case "input", "modelId", "uiActionNonce":
		default:
			return productionEditorInlineInput{}, "", "", invalid
		}
	}
	nonce := syntheticString(value["uiActionNonce"])
	if strings.TrimSpace(nonce) == "" || utf8.RuneCountInString(nonce) > productionEditorInlineNonceMax || !utf8.ValidString(nonce) {
		return productionEditorInlineInput{}, "", "", invalid
	}
	modelID := syntheticString(value["modelId"])
	if modelID == "" || !utf8.ValidString(modelID) {
		return productionEditorInlineInput{}, "", "", invalid
	}
	input, err := productionEditorInlineFrozenInput(syntheticMap(value["input"]))
	if err != nil {
		return productionEditorInlineInput{}, "", "", err
	}
	return input, modelID, nonce, nil
}

// productionEditorInlineRunHandle is the four-key handle for a run this lane just
// opened.
func productionEditorInlineRunHandle(projectID, epoch string, run map[string]any) (syntheticHandle, error) {
	handle := syntheticHandle{
		ProjectID:    projectID,
		Epoch:        epoch,
		RootActionID: syntheticString(run["rootActionId"]),
		RunID:        syntheticString(run["runId"]),
	}
	if handle.ProjectID == "" || handle.Epoch == "" || handle.RootActionID == "" || handle.RunID == "" {
		return syntheticHandle{}, errors.New(productionEditorInlineRunMissing)
	}
	return handle, nil
}

// productionEditorInlinePrior is one run a nonce already spent: the identity the
// browser would hold for it, and its frozen manifest.
type productionEditorInlinePrior struct {
	RunID        string
	RootActionID string
	Epoch        string
	Manifest     map[string]any
}

// productionEditorInlinePriorRun is the reference's same-nonce look-up
// (main-generation-owner.ts:1424): the runs of this project whose root action froze
// this uiActionNonce. More than one is the reference's NONCE_CONFLICT, because two
// runs under one nonce cannot both be the one the renderer holds.
//
// It scans the roots instead of asking json_extract for the three fields, because
// the admission fingerprints sqlite_master and this lookup must not depend on an
// extension the schema does not declare.
func productionEditorInlinePriorRun(ctx context.Context, db *sql.DB, projectID, nonce string) (*productionEditorInlinePrior, error) {
	if db == nil {
		return nil, errors.New(productionEditorInlineNonceConflict)
	}
	rows, err := db.QueryContext(ctx,
		`SELECT a.action_json, r.run_id, r.binding_json FROM generation_roots a
		 JOIN generation_runs r ON r.root_action_id = a.root_action_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var found *productionEditorInlinePrior
	for rows.Next() {
		// A row another build wrote may carry a null action or binding; it can never
		// be a run this family froze, so it is skipped rather than refusing the lookup.
		var actionJSON, runID, bindingJSON sql.NullString
		if err := rows.Scan(&actionJSON, &runID, &bindingJSON); err != nil {
			return nil, err
		}
		if !actionJSON.Valid || !runID.Valid || !bindingJSON.Valid {
			continue
		}
		var action map[string]any
		if err := json.Unmarshal([]byte(actionJSON.String), &action); err != nil {
			return nil, err
		}
		if syntheticString(action["operation"]) != productionEditorInlineOperation ||
			syntheticString(action["projectId"]) != projectID ||
			syntheticString(action["uiActionNonce"]) != nonce {
			continue
		}
		if found != nil {
			return nil, errors.New(productionEditorInlineNonceConflict)
		}
		var binding map[string]any
		if err := json.Unmarshal([]byte(bindingJSON.String), &binding); err != nil {
			return nil, err
		}
		manifest := syntheticMap(binding["sourceManifest"])
		if manifest == nil {
			return nil, errors.New(productionEditorInlineContextInvalid)
		}
		found = &productionEditorInlinePrior{
			RunID:        runID.String,
			RootActionID: syntheticString(action["rootActionId"]),
			Epoch:        syntheticString(binding["epoch"]),
			Manifest:     manifest,
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return found, nil
}

// productionEditorInlineStoredTask returns the exact stored text of the
// editorInlineTask member inside one run's binding document. The ledger's read path
// decodes bindings into maps and loses key order, and this digest was taken over the
// ordered text, so the text itself is read here - the same reason the read half
// reads the context member instead of re-serializing it.
func productionEditorInlineStoredTask(ctx context.Context, db *sql.DB, runID string) ([]byte, error) {
	if db == nil {
		return nil, errors.New(productionEditorInlineTaskInvalid)
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
	raw, ok := productionEditorInlineRawMember(manifest, "editorInlineTask")
	// Only the object shape is this family's frozen task, exactly as the context
	// member is only the object shape.
	if !ok || len(raw) == 0 || raw[0] != '{' {
		return nil, errors.New(productionEditorInlineTaskInvalid)
	}
	return raw, nil
}

// openEditorInlineRun is the family's one run admission: the reference's begin
// selection (main-generation-owner.ts:1431-1432), the lease, the frozen sources, the
// nine derived hashes, the canonical budget, and the ledger's own open - with the
// editor-inline context and task frozen as ordered JSON text.
//
// It mirrors syntheticGeneration.openRun (synthetic_generation_linux.go:879-951) and
// differs in exactly one thing: openRun marshals the frozen manifest with
// encoding/json, which sorts map keys and HTML-escapes <, >, & inside any value it
// carries, while this family must hand the ledger the bytes it hashed.
func (e *productionEditorInline) openEditorInlineRun(ctx context.Context, scope generationowner.Scope, ledger *store.GenerationLedger, db *sql.DB, input productionEditorInlineInput, modelID, nonce string) (map[string]any, error) {
	selection, err := syntheticParseSelection(map[string]any{
		"operation":                       productionEditorInlineOperation,
		"uiActionNonce":                   nonce,
		"modelId":                         modelID,
		"selectedDraftIds":                []any{},
		"selectedFinalizedDraftIds":       []any{},
		"selectedBlueprintChapterNumbers": []any{},
		"promptKeys":                      []any{productionEditorInlinePromptKey},
		"skillStages":                     []any{},
		"authorInputs":                    []any{},
		"output":                          productionEditorInlineVisibleText,
	})
	if err != nil {
		return nil, err
	}
	receipt, err := e.owner.inner.registry.Begin(ctx, scope, selection.ModelID, syntheticGenerationPurpose)
	if err != nil {
		return nil, err
	}
	set, err := syntheticBuildSources(ctx, db, selection)
	if err != nil {
		return nil, err
	}
	language := productionEditorInlineLanguage(set.Core)
	edit, err := prompttemplate.EditSelectedTextEdit(language)
	if err != nil {
		return nil, errors.New(productionEditorInlineTemplateInvalid)
	}
	contextText, err := productionEditorInlineContextText(input, language, edit.Template.Text)
	if err != nil {
		return nil, err
	}
	taskText, err := productionEditorInlineTaskText(input, edit)
	if err != nil {
		return nil, err
	}
	binding, err := e.owner.inner.registry.BindSources(ctx, scope, receipt.LeaseID, set.Sources, set.Selection)
	if err != nil {
		return nil, err
	}
	plan, err := e.owner.inner.syntheticCanonicalPlan(ctx, scope, receipt.LeaseID, selection)
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
		return nil, errors.New(productionEditorInlineRequestInvalid)
	}
	refs, err := syntheticRefsAsAny(binding.Refs())
	if err != nil {
		return nil, errors.New(productionEditorInlineRequestInvalid)
	}
	fingerprintText, err := productionEditorInlineJSON(fingerprint)
	if err != nil {
		return nil, errors.New(productionEditorInlineRequestInvalid)
	}
	budgetText, err := productionEditorInlineJSON(budget)
	if err != nil {
		return nil, errors.New(productionEditorInlineRequestInvalid)
	}
	refsText, err := productionEditorInlineJSON(refs)
	if err != nil {
		return nil, errors.New(productionEditorInlineRequestInvalid)
	}
	// The ledger's "open" call, written as text: every value that reaches the binding
	// is quoted the way JSON.stringify quotes it, and the context and task are spliced
	// in as the exact bytes hashed above.
	request := productionEditorInlineObject(
		productionEditorInlineField{"projectId", productionEditorInlineQuote(scope.Project)},
		productionEditorInlineField{"epoch", productionEditorInlineQuote(scope.Epoch)},
		productionEditorInlineField{"fingerprint", fingerprintText},
		productionEditorInlineField{"contextSnapshotId", productionEditorInlineQuote(syntheticHash(set.CoreText))},
		productionEditorInlineField{"sourceManifest", productionEditorInlineManifestText(selection, scope, receipt.LeaseID, binding.Fingerprint(), input, language, contextText, taskText)},
		productionEditorInlineField{"sourceRefs", refsText},
		productionEditorInlineField{"operation", productionEditorInlineQuote(selection.Operation)},
		productionEditorInlineField{"uiActionNonce", productionEditorInlineQuote(selection.UIActionNonce)},
		productionEditorInlineField{"frozenInputHash", productionEditorInlineQuote(syntheticHash(frozenInput))},
		productionEditorInlineField{"budget", budgetText},
	)
	value, err := ledger.CallJSON(ctx, "open", json.RawMessage("["+request+"]"))
	if err != nil {
		return nil, err
	}
	run := syntheticMap(value)
	if run == nil {
		return nil, errors.New(productionEditorInlineRunMissing)
	}
	return run, nil
}

// productionEditorInlineManifestText is this family's frozen source manifest: the
// reduced lane's members in the order syntheticManifest writes them
// (synthetic_generation_linux.go:736-762), then the six editor-inline members the
// reference freezes (electron/services/generation-source-binding.ts:581-585).
func productionEditorInlineManifestText(selection syntheticSelection, scope generationowner.Scope, leaseID, fingerprint string, input productionEditorInlineInput, language, contextText, taskText string) string {
	return productionEditorInlineObject(
		productionEditorInlineField{"operation", productionEditorInlineQuote(selection.Operation)},
		productionEditorInlineField{"modelId", productionEditorInlineQuote(selection.ModelID)},
		productionEditorInlineField{"modelLeaseId", productionEditorInlineQuote(leaseID)},
		productionEditorInlineField{"goOwnerSourceFingerprint", productionEditorInlineQuote(fingerprint)},
		productionEditorInlineField{"purpose", productionEditorInlineQuote(syntheticPurpose(selection.Operation))},
		productionEditorInlineField{"output", productionEditorInlineQuote(selection.Output)},
		productionEditorInlineField{"promptKeys", productionEditorInlineStringArray(selection.PromptKeys)},
		productionEditorInlineField{"skillStages", "[]"},
		productionEditorInlineField{"authorInputs", "[]"},
		productionEditorInlineField{"selectedDraftIds", productionEditorInlineIDArray(selection.SelectedDraftIDs)},
		productionEditorInlineField{"selectedFinalizedDraftIds", productionEditorInlineIDArray(selection.SelectedFinalizedDraftIDs)},
		productionEditorInlineField{"selectedBlueprintChapterNumbers", productionEditorInlineIDArray(selection.SelectedBlueprintChapters)},
		productionEditorInlineField{"editorInlineInput", productionEditorInlineFrozenInputText(input)},
		productionEditorInlineField{"editorInlineOriginEpoch", productionEditorInlineQuote(scope.Epoch)},
		productionEditorInlineField{"editorInlineContext", contextText},
		productionEditorInlineField{"editorInlineContextHash", productionEditorInlineQuote(syntheticHash(contextText))},
		productionEditorInlineField{"editorInlineTask", taskText},
		productionEditorInlineField{"editorInlineTaskHash", productionEditorInlineQuote(syntheticHash(taskText))},
	)
}

// productionEditorInlineField is one member of an insertion-ordered JSON object:
// the key, and the member's value already encoded as JSON text.
type productionEditorInlineField struct {
	Key   string
	Value string
}

// productionEditorInlineObject renders an object in the order its fields were given.
func productionEditorInlineObject(fields ...productionEditorInlineField) string {
	parts := make([]string, len(fields))
	for index, field := range fields {
		parts[index] = productionEditorInlineQuote(field.Key) + ":" + field.Value
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// productionEditorInlineQuote writes one JSON string the way JSON.stringify does:
// no HTML escaping, no U+2028/U+2029 escaping, and control characters as \u00xx.
// This is the quoting the ledger's own serializer uses
// (internal/store/plottree.go:33), so the text handed to the ledger and the text the
// ledger stores are the same bytes.
func productionEditorInlineQuote(text string) string {
	var out strings.Builder
	out.WriteByte('"')
	for _, r := range text {
		switch r {
		case '"':
			out.WriteString(`\"`)
		case '\\':
			out.WriteString(`\\`)
		case '\b':
			out.WriteString(`\b`)
		case '\f':
			out.WriteString(`\f`)
		case '\n':
			out.WriteString(`\n`)
		case '\r':
			out.WriteString(`\r`)
		case '\t':
			out.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(&out, `\u%04x`, r)
				continue
			}
			out.WriteRune(r)
		}
	}
	out.WriteByte('"')
	return out.String()
}

// productionEditorInlineJSON is the compact JSON of a value no digest is taken over
// (the nine hashes, the budget, the source refs). None of them carries prose, so the
// standard encoder's key order and HTML escaping cannot reach a hashed text.
func productionEditorInlineJSON(value any) (string, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// productionEditorInlineStringArray renders a string list in its own order.
func productionEditorInlineStringArray(values []string) string {
	parts := make([]string, len(values))
	for index, value := range values {
		parts[index] = productionEditorInlineQuote(value)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// productionEditorInlineIDArray renders an id list in its own order.
func productionEditorInlineIDArray(values []int64) string {
	parts := make([]string, len(values))
	for index, value := range values {
		parts[index] = strconv.FormatInt(value, 10)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// productionEditorInlineFrozenInputText is the frozen editorInlineInput: the reference's
// projection of the validated input (electron/services/editor-inline-generation.ts:
// 40), which the recovery read compares against the context field by field.
func productionEditorInlineFrozenInputText(input productionEditorInlineInput) string {
	fields := []productionEditorInlineField{
		{"action", productionEditorInlineQuote(input.Action)},
		{"documentText", productionEditorInlineQuote(input.DocumentText)},
		{"from", strconv.FormatInt(input.From, 10)},
		{"to", strconv.FormatInt(input.To, 10)},
		{"selectedText", productionEditorInlineQuote(input.SelectedText)},
	}
	if input.Instruct {
		fields = append(fields, productionEditorInlineField{"instruction", productionEditorInlineQuote(input.Instruction)})
	}
	return productionEditorInlineObject(fields...)
}

// productionEditorInlineContextText freezes one EditorInlineContext as the text the
// desktop hashes (electron/services/editor-inline-generation.ts:29-33): the author
// fields, then kind, the document digest, the writing language and the resolved
// template. The template is spliced in verbatim, so the frozen context carries the
// shipped object and no Go re-encoding of it.
func productionEditorInlineContextText(input productionEditorInlineInput, language, templateText string) (string, error) {
	if !utf8.ValidString(input.DocumentText) || !utf8.ValidString(input.SelectedText) || !utf8.ValidString(input.Instruction) {
		return "", errors.New(productionEditorInlineRequestInvalid)
	}
	if len(templateText) == 0 || templateText[0] != '{' || !json.Valid([]byte(templateText)) {
		return "", errors.New(productionEditorInlineTemplateInvalid)
	}
	fields := []productionEditorInlineField{
		{"action", productionEditorInlineQuote(input.Action)},
		{"documentText", productionEditorInlineQuote(input.DocumentText)},
		{"from", strconv.FormatInt(input.From, 10)},
		{"to", strconv.FormatInt(input.To, 10)},
		{"selectedText", productionEditorInlineQuote(input.SelectedText)},
	}
	if input.Instruct {
		fields = append(fields, productionEditorInlineField{"instruction", productionEditorInlineQuote(input.Instruction)})
	}
	fields = append(fields,
		productionEditorInlineField{"kind", productionEditorInlineQuote("author-draft")},
		productionEditorInlineField{"documentHash", productionEditorInlineQuote(syntheticHash(input.DocumentText))},
		productionEditorInlineField{"writingLanguage", productionEditorInlineQuote(language)},
		productionEditorInlineField{"template", templateText},
	)
	return productionEditorInlineObject(fields...), nil
}

// productionEditorInlineTaskText freezes the task the reference freezes for an
// inline edit (electron/services/editor-inline-generation.ts:66-73): the purpose
// names the action, refine reviews and the rest draft, the output is visible text,
// and the two messages are the composed system role and the rendered body.
func productionEditorInlineTaskText(input productionEditorInlineInput, edit prompttemplate.EditorInline) (string, error) {
	instruction, ok := productionEditorInlineInstruction(input, edit)
	if !ok {
		return "", errors.New(productionEditorInlineRequestInvalid)
	}
	role := strings.TrimSpace(edit.Template.SystemRole)
	if input.Instruct {
		role = strings.TrimSpace(productionEditorInlineJoin(role, edit.InstructRule))
	}
	system := edit.SystemContract
	if role != "" {
		system = role + "\n\n" + edit.SystemContract
	}
	body := productionEditorInlineFill(edit.Template.Content, map[string]string{
		"edit_instruction": instruction,
		"selected_text":    input.SelectedText,
	})
	user := productionEditorInlinePrune(body + "\n\n" + edit.Template.SystemSuffix)
	stage := "drafting"
	if input.Action == "refine" {
		stage = "review"
	}
	messages := "[" + productionEditorInlineObject(
		productionEditorInlineField{"role", productionEditorInlineQuote("system")},
		productionEditorInlineField{"content", productionEditorInlineQuote(system)},
	) + "," + productionEditorInlineObject(
		productionEditorInlineField{"role", productionEditorInlineQuote("user")},
		productionEditorInlineField{"content", productionEditorInlineQuote(user)},
	) + "]"
	return productionEditorInlineObject(
		productionEditorInlineField{"purpose", productionEditorInlineQuote("editor-ai-" + input.Action)},
		productionEditorInlineField{"reasoningStage", productionEditorInlineQuote(stage)},
		productionEditorInlineField{"output", productionEditorInlineQuote(productionEditorInlineVisibleText)},
		productionEditorInlineField{"messages", messages},
	), nil
}

// productionEditorInlineInstruction is the reference's editInstruction
// (electron/services/editor-inline-generation.ts:50-57): a fixed action takes its
// shipped instruction, and an author note is prefixed with the shipped lead-in and
// appended after a newline, because the note outranks every other preference.
func productionEditorInlineInstruction(input productionEditorInlineInput, edit prompttemplate.EditorInline) (string, bool) {
	if input.Instruct {
		return edit.InstructPrefix + "\n" + input.Instruction, true
	}
	instruction, ok := edit.Instructions[input.Action]
	return instruction, ok
}

// productionEditorInlineJoin is the reference's `[a, b].filter(Boolean).join('\n')`
// over two already-trimmed parts.
func productionEditorInlineJoin(parts ...string) string {
	kept := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != "" {
			kept = append(kept, part)
		}
	}
	return strings.Join(kept, "\n")
}

// productionEditorInlineFill is renderPrompt's fillVariables
// (src/shared/render-prompt.ts:35-37): one pass, values written literally, and an
// unknown token left exactly as it was.
func productionEditorInlineFill(text string, variables map[string]string) string {
	return productionEditorInlineToken.ReplaceAllStringFunc(text, func(token string) string {
		key := strings.TrimSuffix(strings.TrimPrefix(token, "{{"), "}}")
		if value, ok := variables[key]; ok {
			return value
		}
		return token
	})
}

// productionEditorInlinePrune is the part of pruneEmptyOptionalPromptSections a
// shipped template can reach (src/services/builtin-prompt-templates.ts:175-186): the
// blank-line collapse and the edge trim. The two deletions before them match optional
// labels and context headings, and internal/prompttemplate refuses an asset carrying
// any of them, so this lane's render is identity to them rather than silent about
// them.
func productionEditorInlinePrune(text string) string {
	for strings.Contains(text, "\n\n\n") {
		text = strings.ReplaceAll(text, "\n\n\n", "\n\n")
	}
	return strings.Trim(text, "\n")
}

// productionEditorInlineLanguage is the writing language of the admitted project,
// normalized the way the reduced lane normalizes it
// (synthetic_generation_linux.go:606-609).
func productionEditorInlineLanguage(core map[string]any) string {
	if syntheticString(core["writingLanguage"]) == prompttemplate.LanguageEnglish {
		return prompttemplate.LanguageEnglish
	}
	return prompttemplate.LanguageChinese
}
