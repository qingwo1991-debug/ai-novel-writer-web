//go:build linux

package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/globalconfig"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/promptskills"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file is the production lane's WRITE surface for the three families whose
// stores already exist and whose state lives OUTSIDE the author's project
// database: the model configuration (`llm:*`), the global config document and the
// prompt/skill tree, plus the two confined project-file writes of the `fs:*`
// family. It adds no store, no path grammar and no admission decision of its own:
// every write is either the shipped transport binder or the shipped store, gated
// by the one admission this lane already uses for `generation:begin` and
// `db:save-summary-snapshot` - the live writer lease plus the committed startup
// generation - and, for the file writes, the caller's live project lease.
//
// # Why three family binders are not used
//
// transport.BindGlobalConfig binds config:get, config:set, startup:get-state and
// startup:appearance-ack atomically (internal/transport/globalconfig.go:18-21,
// :34-65) and transport.BindModelRouting binds six llm routing names atomically
// (internal/transport/model_routing.go:25-31); transport.BindProjectFiles binds
// four fs:* names atomically (:75-94). In every one of those families this lane
// has ALREADY published the read half (production_open_linux.go:478
// BindGlobalConfigReads, :576 BindModelRoutingReads, :585
// newProductionFSReads), so taking the family binder would fail startup with
// OPERATION_ALREADY_BOUND instead of publishing the write half. The write names
// are therefore bound one name at a time with server.Bind - the exported
// single-name binder (internal/transport/server.go:73-77), which itself refuses
// an uninventoried (OPERATION_NOT_EXPOSED) or already bound
// (OPERATION_ALREADY_BOUND) name - and each handler below is a line-for-line
// port of the family handler it replaces: same arity, same field denials, same
// refusal envelope, same store call, same order. Nothing is validated here that
// the transport binder did not already validate.
//
// The three single-name binders ARE used verbatim, so the credential handling
// they own is never reimplemented in package main: transport.BindModelSave (which
// additionally registers the submitted key with the public response sanitizer
// before any diagnostic can be emitted, internal/transport/model_mutations.go:23-72),
// transport.BindModelDelete (model_delete.go:16-45) and
// transport.BindPromptSkillsWrites (promptskills_read.go:204-300).
//
// # Bound, and the store each write commits to
//
//	llm:save-model              arity 1  [modelRecord]                  models.json
//	llm:delete-model            arity 1  [modelId]                      models.json + config.json, one recoverable journal
//	llm:set-stage-model         arity 1  [request]                      config.json stageModelIds
//	llm:set-default-model       arity 1  [modelId|null]                 config.json defaultModelId
//	llm:set-default-embedding-model arity 1 [modelId|null]              config.json defaultEmbeddingModelId
//	config:set                  arity 1  [patch]                        config.json
//	prompt:save-global          arity 1  [template]                     <data root>/prompts/<key>[.lang].json
//	prompt:delete-global        arity 2  [key, language]                <data root>/prompts/...
//	skills:uninstall-user       arity 1  [name]                         <data root>/skills/<name>/
//	fs:mkdir                    arity 3  [relativePath, claimedRoot, context]
//	fs:write-file               arity 4  [relativePath, content, claimedRoot, context]
//
// The model and config writes are single-document (or two-document, through the
// journal in internal/globalconfig/model_transaction_linux.go:277-371) atomic
// replacements inside the committed generation the lane already pinned: the same
// root `llm:get-default-model`, `llm:list-models` and `config:get` read. The
// prompt tree is the same root the bound `prompt:load-global` reads
// (production_chapter_reads_linux.go:186-197 resolves it through
// globaladmission.PinnedReady), so a written template is what the read returns.
// No write here invents a path: the prompt and skill names come from
// promptskills.Service, which validates them itself (internal/promptskills/
// service.go:27-28 promptName) and refuses anything that leaves its pinned root.
//
// # The two file writes are confined exactly like the read side
//
// fs:mkdir and fs:write-file reuse the read side's adapter verbatim:
// productionFSParseFileRequest (production_fs_reads_linux.go:494-527) refuses the
// wire shape, re-parses the frozen context, requires the claimed root to equal
// the project inside it, and confines the browser path through
// projectfs.BrowserRelativePath (internal/projectfs/browser_files.go:73-84) -
// absolute paths, drive letters, backslashes, NUL bytes, "."/".." components,
// hidden components and the storage-reserved names are refused BEFORE a project
// is borrowed. productionFSReads.withProjectFile (:312-347) then re-observes the
// caller's live project lease (productionFSHolds, :262-290), pins the project by
// id through the library, and walks the path one component at a time through
// already pinned os.Root handles, each hop re-verified by projectfs.openChild and
// the final component written through a temporary file in the pinned parent
// directory plus one rename (browser_files.go:215-254, :316-358). A write that
// escapes the project root is not a refusal this file can get wrong, because this
// file does not implement the confinement at all.
//
// # Refused: project:recent-remove
//
// The one name this file does NOT bind, although a binder exists
// (internal/transport/projects.go:293-329). That binder drives
// ProjectRecentRemoveProvider.RemoveRecentNavigation, which drops a record from a
// navigation catalog. The production lane has no such catalog to drop from: its
// shelf is the admitted library itself (productionOpen.RecentProjects,
// production_open_linux.go:344-370, served by the explicitly bound
// project:recent-list at :603), and the only catalog in this process is the
// private scratch one newProductionDurable builds at
// production_durable_linux.go:74-84 and does not retain on the returned
// productionDurableOpen. Binding this name would therefore either delete an
// author's library entry or answer `{"success":true}` for a shelf entry it never
// removed. It keeps failing closed at 501 GO_OPERATION_NOT_MIGRATED, exactly the
// state it is in today, so no existing expectation has to change for it.

const (
	productionModelSaveName         = "llm:save-model"
	productionModelDeleteName       = "llm:delete-model"
	productionModelStageSetName     = "llm:set-stage-model"
	productionModelDefaultSetName   = "llm:set-default-model"
	productionModelEmbeddingSetName = "llm:set-default-embedding-model"
	productionConfigSetName         = "config:set"

	// productionFSMkdirName and productionFSWriteFileName are the two mutating
	// names of the fs:* family a browser can honestly be given.
	productionFSMkdirName     = "fs:mkdir"
	productionFSWriteFileName = "fs:write-file"

	// productionFSWriteFileArity is the family's own wire arity for fs:write-file
	// (internal/transport/project_files.go:44): the content sits between the path
	// and the claimed root.
	productionFSWriteFileArity = 4
	productionFSWritePathPos   = 0
	productionFSWriteBodyPos   = 1
	productionFSWriteRootPos   = 2
	productionFSWriteCtxPos    = 3

	// productionPromptRootTimeout bounds one prompt-root resolution. It mirrors
	// the read side's own bound (production_chapter_reads_linux.go:161-185).
	productionPromptRootTimeout = 5 * time.Second
)

// productionWriteRefusal is the transport refusal this file answers with for a
// lane that is closed or a caller whose session or committed generation is not
// ready. Both codes are fixed symbols; no path, no committed value and no store
// message is ever formatted into them.
func productionWriteRefusal(code string) error {
	status := http.StatusInternalServerError
	switch code {
	case productionFSClosedCode:
		status = http.StatusServiceUnavailable
	case "GLOBAL_DATA_NOT_READY", "REQUEST_CANCELED":
		status = http.StatusConflict
	}
	return security.NewHTTPError(status, code)
}

// productionWriteGate is THE admission of every write this file binds, and it is
// the same one production mutations already pass in this lane.
//
//   - The lane must be open. Close sets closed and then releases the exclusive
//     writer lease the durable runtime owns (production_open_linux.go:397-439), so
//     a lane that no longer holds that lease can never reach a store through this
//     file: the gate reads the lease holder's own liveness before anything else.
//   - The caller must be an authenticated session tab. productionGlobalCaller
//     (production_chapter_reads_linux.go:192-197) is the same identity
//     prompt:load-global and the generation owner use, and it is empty for a
//     request that named no principal, cookie or tab.
//   - The committed generation must be ready for that caller. This is the exact
//     gate the shipped binders apply (internal/transport/globalconfig.go:38,
//     :50; model_mutations.go:52; promptskills_read.go:22), so a write never
//     outlives the admission that owns its root.
//
// One deliberate difference from the family binders' per-family cancellation code
// (GLOBAL_CONFIG_CANCELLED, MODEL_CONFIG_CANCELLED): a cancelled request is
// reported here as REQUEST_CANCELED, because one gate serves the model, config,
// prompt and file families and the caller cannot act differently on the others.
func (p *productionOpen) productionWriteGate(ctx context.Context, caller transport.Caller) error {
	if p == nil || ctx == nil {
		return productionWriteRefusal(productionFSClosedCode)
	}
	key := productionGlobalCaller(caller)
	p.mu.Lock()
	closed, durable, config, startup := p.closed, p.durable, p.config, p.startup
	p.mu.Unlock()
	if closed || durable == nil || config == nil || startup == nil {
		return productionWriteRefusal(productionFSClosedCode)
	}
	durable.mu.Lock()
	durableClosed := durable.closed
	durable.mu.Unlock()
	if durableClosed {
		return productionWriteRefusal(productionFSClosedCode)
	}
	if key == "" || startup.State(key, true).State != "ready" {
		return productionWriteRefusal("GLOBAL_DATA_NOT_READY")
	}
	if err := ctx.Err(); err != nil {
		return productionWriteRefusal("REQUEST_CANCELED")
	}
	return nil
}

// productionConfigPublicFields is this file's copy of the transport allowlist
// (internal/transport/globalconfig.go:67-72). It is duplicated rather than
// exported because the family binder is atomic and cannot be reused here; the
// list is finite and a field added there without being added here is refused,
// which is the fail-closed direction.
var productionConfigPublicFields = map[string]bool{
	"theme": true, "locale": true, "autoOpenNextChapterAfterFinalize": true,
	"editorFontSize": true, "editorFontFamily": true, "autoSaveInterval": true,
	"defaultModelId": true, "defaultEmbeddingModelId": true, "outlineTargetUnits": true,
	"blueprintTargetUnits": true, "stageModelIds": true,
}

// productionModelWriteRefusal is the llm family's business refusal envelope, the
// same shape the transport binder returns for a store refusal
// (internal/transport/model_delete.go:26, model_routing.go:197-215): HTTP 200 with
// `success:false` and one symbolic `Error: CODE`.
func productionModelWriteRefusal(code string) map[string]any {
	return map[string]any{"success": false, "error": "Error: " + code}
}

// productionModelWriteCode is the finite mapping the routing setters use. It is
// the transport binder's own table (internal/transport/model_routing.go:197-215)
// without its unexported symbols, so a store refusal keeps the code the family
// already published instead of becoming a generic failure.
func productionModelWriteCode(err error) string {
	for _, known := range []struct {
		err  error
		code string
	}{
		{globalconfig.ErrStageModelRequestInvalid, "LLM_STAGE_MODEL_REQUEST_INVALID"},
		{globalconfig.ErrStageModelUnavailable, "LLM_STAGE_MODEL_UNAVAILABLE"},
		{globalconfig.ErrModelCorrupt, "LLM_MODEL_CONFIG_CORRUPT"},
		{globalconfig.ErrConfigCorrupt, "LLM_GLOBAL_CONFIG_CORRUPT"},
		{globalconfig.ErrUnsafePath, "GLOBAL_CONFIG_UNSAFE_PATH"},
		{globalconfig.ErrUnsafeFile, "GLOBAL_CONFIG_UNSAFE_FILE"},
		{globalconfig.ErrTooLarge, "GLOBAL_CONFIG_TOO_LARGE"},
	} {
		if errors.Is(err, known.err) {
			return known.code
		}
	}
	return "GLOBAL_CONFIG_WRITE_FAILED"
}

// productionGlobalWrites binds the model, config and prompt writes of the one
// committed global generation this lane pinned.
type productionGlobalWrites struct {
	open    *productionOpen
	prompts *productionPromptWrites
}

// newProductionGlobalWrites returns the adapter for the lane built by
// productionHandlerWithContract. It resolves nothing and opens nothing here; every
// handler snapshots the lane's own handles per request, because Close releases
// them.
func newProductionGlobalWrites(open *productionOpen) *productionGlobalWrites {
	return &productionGlobalWrites{open: open, prompts: &productionPromptWrites{open: open}}
}

// Bind installs the global write names this file owns: the three routing setters,
// config:set, llm:delete-model, llm:save-model and the three prompt/skill writes.
// server.Bind and the three
// single-name binders all refuse a name that is not inventoried
// (OPERATION_NOT_EXPOSED) or already bound (OPERATION_ALREADY_BOUND), so a name
// this file should not publish is a startup error rather than silent dead code.
//
// llm:save-model is published only when the committed model-egress policy exists:
// the name's own binder refuses an endpoint outside that policy and requires the
// policy to answer at all (internal/transport/model_mutations.go:19-42), so a lane
// whose committed config names no usable origin keeps the name fail-closed at 501
// instead of installing a handler that could only refuse. The refusal is logged;
// the committed values are not.
func (w *productionGlobalWrites) Bind(server *transport.Server) error {
	if w == nil || w.open == nil || w.prompts == nil || server == nil {
		return errors.New("PRODUCTION_WRITE_BINDER_INVALID")
	}
	config, startup, err := w.authority()
	if err != nil {
		return err
	}
	for _, operation := range []struct {
		name    string
		handler transport.Operation
	}{
		{productionModelStageSetName, w.setStageModel},
		{productionModelDefaultSetName, w.setDefaultModel},
		{productionModelEmbeddingSetName, w.setDefaultEmbeddingModel},
		{productionConfigSetName, w.setConfig},
	} {
		if bindErr := server.Bind(operation.name, operation.handler); bindErr != nil {
			return bindErr
		}
	}
	if bindErr := server.BindModelDelete(config, startup); bindErr != nil {
		return bindErr
	}
	if err := w.bindModelSave(server, config, startup); err != nil {
		return err
	}
	return server.BindPromptSkillsWrites(w.prompts)
}

// authority snapshots the two handles every global write commits through. They are
// read under the lane lock because Close drops them, exactly like
// productionFSReads.roots does for the read side
// (production_fs_reads_linux.go:242-256).
func (w *productionGlobalWrites) authority() (*globalconfig.Service, *globalconfig.Startup, error) {
	open := w.open
	open.mu.Lock()
	defer open.mu.Unlock()
	if open.closed || open.config == nil || open.startup == nil {
		return nil, nil, errors.New("PRODUCTION_WRITE_AUTHORITY_REQUIRED")
	}
	return open.config, open.startup, nil
}

// configService snapshots the global config store again, or answers the symbolic
// refusal. The gate above already proved the lane is open; this keeps a handler
// that raced Close from dereferencing a closed store.
func (w *productionGlobalWrites) configService() (*globalconfig.Service, error) {
	config, _, err := w.authority()
	if err != nil {
		return nil, productionWriteRefusal(productionFSClosedCode)
	}
	return config, nil
}

// bindModelSave publishes llm:save-model through the shipped binder, or leaves it
// unbound with a symbolic log line when the committed egress policy is absent.
func (w *productionGlobalWrites) bindModelSave(server *transport.Server, config *globalconfig.Service, startup *globalconfig.Startup) error {
	egressPolicy, policyErr := w.open.ModelEgress()
	if policyErr != nil || egressPolicy == nil {
		log.Printf("novel-server: production model writes: %s stays unbound (committed model egress unavailable: %v)", productionModelSaveName, policyErr)
		return nil
	}
	return server.BindModelSave(config, startup, egressPolicy)
}

// setStageModel is llm:set-stage-model, ported from
// internal/transport/model_routing.go:79-133. The store resolves the model against
// the saved profiles BEFORE it touches config.json and refuses a damaged document
// rather than replacing it (internal/globalconfig/model_routing_linux.go:11-17).
func (w *productionGlobalWrites) setStageModel(ctx context.Context, caller transport.Caller, args []any) (any, error) {
	if len(args) != 1 {
		return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
	}
	if err := w.open.productionWriteGate(ctx, caller); err != nil {
		return nil, err
	}
	request, ok := args[0].(map[string]any)
	if !ok {
		return productionModelWriteRefusal("LLM_STAGE_MODEL_REQUEST_INVALID"), nil
	}
	stage, ok := request["stage"].(string)
	if !ok || !globalconfig.IsModelStage(stage) {
		return productionModelWriteRefusal("LLM_STAGE_MODEL_REQUEST_INVALID"), nil
	}
	raw, present := request["modelId"]
	if !present {
		return productionModelWriteRefusal("LLM_STAGE_MODEL_REQUEST_INVALID"), nil
	}
	// A JSON null clears the assignment; anything else must be a string.
	var modelID *string
	if raw != nil {
		text, ok := raw.(string)
		if !ok {
			return productionModelWriteRefusal("LLM_STAGE_MODEL_REQUEST_INVALID"), nil
		}
		modelID = &text
	}
	config, err := w.configService()
	if err != nil {
		return nil, err
	}
	stages, err := config.AssignStageModel(stage, modelID)
	if err != nil {
		return productionModelWriteRefusal(productionModelWriteCode(err)), nil
	}
	return map[string]any{"success": true, "stageModelIds": stages}, nil
}

// setDefaultModel is llm:set-default-model, ported from
// internal/transport/model_routing.go:135-166.
func (w *productionGlobalWrites) setDefaultModel(ctx context.Context, caller transport.Caller, args []any) (any, error) {
	if len(args) != 1 {
		return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
	}
	if err := w.open.productionWriteGate(ctx, caller); err != nil {
		return nil, err
	}
	config, err := w.configService()
	if err != nil {
		return nil, err
	}
	if err := config.SetDefaultModel(args[0]); err != nil {
		return productionModelWriteRefusal(productionModelWriteCode(err)), nil
	}
	return map[string]any{"success": true}, nil
}

// setDefaultEmbeddingModel is llm:set-default-embedding-model, ported from
// internal/transport/model_routing.go:168-194.
func (w *productionGlobalWrites) setDefaultEmbeddingModel(ctx context.Context, caller transport.Caller, args []any) (any, error) {
	if len(args) != 1 {
		return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
	}
	if err := w.open.productionWriteGate(ctx, caller); err != nil {
		return nil, err
	}
	config, err := w.configService()
	if err != nil {
		return nil, err
	}
	if err := config.SetDefaultEmbeddingModel(args[0]); err != nil {
		return productionModelWriteRefusal(productionModelWriteCode(err)), nil
	}
	return map[string]any{"success": true}, nil
}

// setConfig is config:set, ported from internal/transport/globalconfig.go:43-55.
// The two denials before the write are the ones that keep a browser out of
// deployment authority: an unknown or routing-owned field is refused with
// CONFIG_FIELD_DENIED, and the deployment proxy is refused on PRESENCE - null,
// false, 0 and "" included - with PROXY_SERVER_ADMIN_ONLY. Neither can be cleared,
// set or guessed at from a page.
func (w *productionGlobalWrites) setConfig(ctx context.Context, caller transport.Caller, args []any) (any, error) {
	if len(args) != 1 {
		return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
	}
	input, ok := args[0].(map[string]any)
	if !ok {
		return nil, security.NewHTTPError(http.StatusBadRequest, "CONFIG_INVALID")
	}
	for key := range input {
		if (!productionConfigPublicFields[key] || key == "stageModelIds") && key != "proxy" {
			return nil, security.NewHTTPError(http.StatusBadRequest, "CONFIG_FIELD_DENIED")
		}
	}
	if _, present := input["proxy"]; present {
		return nil, security.NewHTTPError(http.StatusForbidden, "PROXY_SERVER_ADMIN_ONLY")
	}
	if err := w.open.productionWriteGate(ctx, caller); err != nil {
		return nil, err
	}
	config, err := w.configService()
	if err != nil {
		return nil, err
	}
	result := config.Set(input)
	if !result.Success {
		return map[string]any{"success": false, "error": "GLOBAL_CONFIG_WRITE_FAILED"}, nil
	}
	return map[string]any{"success": true}, nil
}

// productionPromptWrites lends the same prompt and installed-skill root the bound
// prompt:load-global read serves, so the three mutating prompt/skill names write
// exactly what the read returns. It is the PromptSkillsReadProvider seam the
// transport write binder takes (internal/transport/promptskills_read.go:17-19),
// which is why no prompt name, template field or path is interpreted here: the
// service validates the name and refuses anything that leaves its pinned root
// (internal/promptskills/service.go:27-28, :104-160).
type productionPromptWrites struct {
	open *productionOpen
	mu   sync.Mutex
	root string
}

// BeginPromptSkills resolves the committed generation every request and returns a
// fresh service over it, released by the caller. The lane gate runs first, so a
// lane that no longer holds the writer lease answers the symbolic refusal instead
// of opening the root.
func (w *productionPromptWrites) BeginPromptSkills(ctx context.Context, caller transport.Caller) (*promptskills.Service, func(), error) {
	if w == nil || w.open == nil {
		return nil, nil, errors.New("PROMPT_SKILLS_ROOT_UNAVAILABLE")
	}
	if err := w.open.productionWriteGate(ctx, caller); err != nil {
		return nil, nil, err
	}
	root, err := w.pinnedRoot()
	if err != nil {
		return nil, nil, err
	}
	service, err := promptskills.New(root)
	if err != nil {
		return nil, nil, err
	}
	return service, func() { _ = service.Close() }, nil
}

// pinnedRoot is the same resolution the read side performs
// (production_chapter_reads_linux.go:161-185): the committed generation path comes
// from globaladmission.PinnedReady, re-verified on every call, and is cached only
// after it resolved once. A pin that cannot be re-proved is a symbolic refusal,
// never a stale path.
func (w *productionPromptWrites) pinnedRoot() (string, error) {
	w.mu.Lock()
	cached := w.root
	w.mu.Unlock()
	if cached != "" {
		return cached, nil
	}
	open := w.open
	open.mu.Lock()
	global, closed := open.global, open.closed
	open.mu.Unlock()
	if closed || global == nil {
		return "", productionWriteRefusal(productionFSClosedCode)
	}
	ctx, cancel := context.WithTimeout(context.Background(), productionPromptRootTimeout)
	defer cancel()
	root, err := global.DataRoot(ctx)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", ctxErr
		}
		return "", errors.New("PROMPT_SKILLS_ROOT_UNAVAILABLE")
	}
	if root == "" {
		return "", errors.New("PROMPT_SKILLS_ROOT_UNAVAILABLE")
	}
	w.mu.Lock()
	w.root = root
	w.mu.Unlock()
	return root, nil
}

// productionFileWrites binds the two mutating fs:* names a browser can honestly
// be given, over the SAME confined adapter the three fs reads use.
type productionFileWrites struct {
	reads *productionFSReads
}

// newProductionFileWrites returns the write adapter for the lane built by
// productionHandlerWithContract.
func newProductionFileWrites(open *productionOpen) *productionFileWrites {
	return &productionFileWrites{reads: newProductionFSReads(open)}
}

// Bind installs fs:mkdir and fs:write-file, one server.Bind per name.
func (w *productionFileWrites) Bind(server *transport.Server) error {
	if w == nil || w.reads == nil || server == nil {
		return errors.New("PRODUCTION_FS_WRITE_BINDER_INVALID")
	}
	for _, operation := range []struct {
		name    string
		handler transport.Operation
	}{
		{productionFSMkdirName, w.makeDirectory},
		{productionFSWriteFileName, w.writeFile},
	} {
		if err := server.Bind(operation.name, operation.handler); err != nil {
			return err
		}
	}
	return nil
}

// makeDirectory is fs:mkdir. The wire shape is the family's own
// (internal/transport/project_files.go:42) and is parsed by the read side's
// validator, so the path grammar, the claimed-root equality and the frozen context
// are the ones already proven. The success envelope is the family's
// `{success:true}` and a refusal keeps the family's business shape at HTTP 200
// (:166-176, :201-209) with one symbolic code.
func (w *productionFileWrites) makeDirectory(ctx context.Context, caller transport.Caller, args []any) (any, error) {
	request, code := productionFSParseFileRequest(args)
	if code != "" {
		return productionFSFileFailure(code), nil
	}
	err := w.reads.withProjectFile(ctx, caller, request.expected, func(project *projectfs.Project) error {
		return project.MakeBrowserDirectory(ctx, request.relative)
	})
	if err != nil {
		return productionFSFileFailure(productionFSCode(err)), nil
	}
	return map[string]any{"success": true}, nil
}

// writeFile is fs:write-file. The content sits between the path and the claimed
// root (internal/transport/project_files.go:44), so the path, root and context are
// handed to the read side's validator in their own positions and every rule that
// confines fs:read-file confines this write too. The body ceiling is
// projectfs.BrowserFileMaxBytes - the same ceiling projectfs enforces - and the
// answer keeps the family's commit-state vocabulary, so a replacement that landed
// but could not be re-verified is reported as UNKNOWN rather than as a failure
// (internal/projectfs/browser_files.go:33-35).
func (w *productionFileWrites) writeFile(ctx context.Context, caller transport.Caller, args []any) (any, error) {
	if len(args) != productionFSWriteFileArity {
		return productionFSWriteFileFailure("INVALID_ARGUMENTS", "not_committed"), nil
	}
	request, code := productionFSParseFileRequest([]any{
		args[productionFSWritePathPos],
		args[productionFSWriteRootPos],
		args[productionFSWriteCtxPos],
	})
	if code != "" {
		return productionFSWriteFileFailure(code, "not_committed"), nil
	}
	content, ok := args[productionFSWriteBodyPos].(string)
	if !ok {
		return productionFSWriteFileFailure("INVALID_ARGUMENTS", "not_committed"), nil
	}
	if int64(len(content)) > projectfs.BrowserFileMaxBytes {
		return productionFSWriteFileFailure("PROJECT_FILE_TOO_LARGE", "not_committed"), nil
	}
	err := w.reads.withProjectFile(ctx, caller, request.expected, func(project *projectfs.Project) error {
		return project.WriteBrowserFile(ctx, request.relative, []byte(content))
	})
	if err != nil {
		code, commitState := productionFSWriteCode(err)
		return productionFSWriteFileFailure(code, commitState), nil
	}
	return map[string]any{"success": true, "commitState": "committed"}, nil
}

// productionFSFileFailure is the family's business refusal envelope for fs:mkdir
// (internal/transport/project_files.go:166-176). The code is symbolic because this
// lane has no i18n table and because a code cannot leak a host path.
func productionFSFileFailure(code string) map[string]any {
	return map[string]any{"success": false, "error": code}
}

// productionFSWriteFileFailure is the family's envelope for fs:write-file, which
// also reports how far the replacement got.
func productionFSWriteFileFailure(code, commitState string) map[string]any {
	return map[string]any{"success": false, "commitState": commitState, "error": code}
}

// productionFSWriteCode maps a write refusal to one symbolic code and the commit
// state the family reports. ErrBrowserCommitUnknown is checked FIRST: that error
// is a joined value whose bytes may already be the project's content, so it must
// never be reported as a plain failure (internal/projectfs/browser_files.go:33-35,
// internal/transport/project_files.go:200-230).
func productionFSWriteCode(err error) (string, string) {
	if errors.Is(err, projectfs.ErrBrowserCommitUnknown) {
		return "PROJECT_FILE_COMMIT_UNKNOWN", "unknown"
	}
	return productionFSCode(err), "not_committed"
}
