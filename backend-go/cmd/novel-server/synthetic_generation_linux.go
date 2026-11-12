//go:build linux

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/authorruntime"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/egress"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/generation"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/generationowner"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/globalconfig"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/llm"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// syntheticGeneration is the durable S1 generation provider. Every method
// re-admits the exact frozen cookie/tab/lease fence through
// BeginGenerationLedgerDB (single-writer coordinator admission, one release per
// call), then operates on the admitted author's own database. The reduced
// RunBinding derives its nine fingerprint hashes from real project state and
// the canonical begin-time budget, so the fingerprint is deterministic and
// reproducible at execute time; credentials and provider text never leave the
// server (the model lease id is the only model secret handle in a DTO).
type syntheticGeneration struct {
	lifecycle *authorruntime.ReadLifecycle
	server    *transport.Server
	registry  *generationowner.Registry
	executor  *generationowner.Executor
	loadModel func(context.Context, string) (generationowner.Profile, bool, error)
	// leaseOwner derives the LEASE OWNER this lane's ledger admission borrows the
	// project under. It is injected per lane: the production lane passes
	// productionLeaseOwner (account-level, so sibling tabs share the lease) and
	// the migration scaffold passes syntheticLeaseOwner (per tab). The run's
	// RECEIPT scope stays per-tab either way: see the Scope built in admit.
	leaseOwner func(transport.Caller) (projectsession.Owner, error)
	// snapshotSeq is the per-attempt VISIBLE snapshot revision. The durable
	// revision only advances on a flush, but the visible text advances on every
	// delta; the browser's snapshot regression check rejects two snapshots that
	// share a revision with different text, so the event stream needs its own
	// strictly increasing counter. The final read projection reuses it so the
	// settled snapshot's revision stays above every event it followed.
	snapshotSeq sync.Map
}

// nextSnapshotSeq returns the next visible snapshot revision for one attempt. It
// is called once per published event and once per projected snapshot, so the
// same artifact's revision is strictly increasing in delivery order.
func (s *syntheticGeneration) nextSnapshotSeq(attemptID string) int64 {
	if s == nil || attemptID == "" {
		return 0
	}
	for {
		value, _ := s.snapshotSeq.LoadOrStore(attemptID, int64(0))
		current := value.(int64)
		if s.snapshotSeq.CompareAndSwap(attemptID, current, current+1) {
			return current + 1
		}
	}
}

const (
	// syntheticRootOutputTokens is the canonical root output cap used both at
	// begin time (budget derivation) and at execute time, keeping the two
	// plans identical for the reduced lane.
	syntheticRootOutputTokens int64 = 8192
	// syntheticLiabilityHeadroom and syntheticOutputHeadroom pad the canonical
	// begin-time plan so an identical execute-time plan always fits the root
	// budget even when the browser requests the full canonical output.
	syntheticLiabilityHeadroom int64 = 65536
	syntheticOutputHeadroom    int64 = 16384
	// syntheticMaxPhysicalRequests and syntheticMaxActiveElapsedMs bound every
	// reduced run; the ledger enforces both per root.
	syntheticMaxPhysicalRequests int64 = 32
	syntheticMaxActiveElapsedMs  int64 = 3_600_000
	// syntheticGenerationPurpose is the registry purpose shared by every lease.
	syntheticGenerationPurpose = "generation"
	// syntheticRetryPurpose is the attempt purpose for draft-short-outline
	// retries, matching the renderer's DRAFT_SHORT_OUTLINE_PURPOSE label.
	syntheticRetryPurpose = "draft-short-outline"
	// syntheticRetryNoncePrefix distinguishes a retry invocation from the
	// original draft-short-outline dispatch on the same run.
	syntheticRetryNoncePrefix = "draft-short-outline-retry:"
)

var (
	syntheticPurposePattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9:_-]{0,127}$`)
	syntheticHashPattern      = regexp.MustCompile(`^[a-f0-9]{64}$`)
	syntheticPromptKeyPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	// syntheticGraphPromptKeyPattern is the one prompt key each graph operation
	// freezes (electron/services/generation-source-binding.ts:292). It is not part
	// of the renderer grammar above, so it is only accepted where a frozen graph
	// manifest is read back, never as renderer input.
	syntheticGraphPromptKeyPattern = regexp.MustCompile(`^graph-(?:plot|plan|event)$`)
)

// syntheticPromptSourceKey reports whether a prompt source key may be resolved:
// the ordinary renderer grammar, plus the three graph template keys the ledger
// freezes for a graph run. Only a source read accepts them, so a begin of another
// family gains no key it did not have.
func syntheticPromptSourceKey(key string) bool {
	return syntheticPromptKeyPattern.MatchString(key) || syntheticGraphPromptKeyPattern.MatchString(key)
}

// syntheticPlanning mirrors the ledger's planning gate so the reduced lane
// refuses planning/blueprint authority exactly like the ordinary binder.
func syntheticPlanning(operation string) bool {
	return strings.Contains(operation, "planning") || strings.Contains(operation, "blueprint") || strings.Contains(operation, "plot-outline")
}

// newSyntheticGeneration builds the durable generation lane from the private
// global config root (trusted model profiles) and the administrator supplied
// egress origins. The executor keeps the owner's native dispatch unless the
// test-only FakeDispatch seam is configured; production main.go never sets it.
func newSyntheticGeneration(durable *syntheticDurableOpen, server *transport.Server, config syntheticDurableConfig) (*syntheticGeneration, error) {
	if durable == nil || durable.lifecycle == nil || server == nil {
		return nil, errors.New("GENERATION_CONFIG_REQUIRED")
	}
	if config.GlobalRoot == "" || len(config.ModelOrigins) == 0 {
		return nil, errors.New("GENERATION_CONFIG_REQUIRED")
	}
	models, err := globalconfig.New(config.GlobalRoot)
	if err != nil {
		return nil, err
	}
	loadModel := func(ctx context.Context, modelID string) (generationowner.Profile, bool, error) {
		records, err := models.ListPrivateModels()
		if err != nil {
			return generationowner.Profile{}, false, err
		}
		for _, record := range records {
			if id, _ := record["id"].(string); id != modelID {
				continue
			}
			profile, err := syntheticModelProfile(record)
			if err != nil {
				return generationowner.Profile{}, false, err
			}
			return profile, true, nil
		}
		return generationowner.Profile{}, false, nil
	}
	current := func(ctx context.Context, scope generationowner.Scope) error {
		// The live lease is looked up under the LEASE OWNER the begin captured (the
		// account, for the production lane), and through the tab that captured the
		// scope: sharing the account lease never makes a tab that never opened the
		// project current. The receipt scope beside it stays per tab, so two tabs of
		// one account hold the same lease and disjoint receipts.
		currentValue, found, err := durable.lifecycle.CurrentProjectDurable(ctx, projectsession.Owner{SessionID: scope.LeaseSession, ClientID: scope.LeaseClient, Tab: scope.Tab})
		if err != nil {
			return err
		}
		if !found || currentValue.ProjectID != scope.Project || currentValue.LeaseID != scope.Epoch {
			return errors.New("GENERATION_EPOCH_STALE")
		}
		return nil
	}
	endpoints, err := egress.New(config.ModelOrigins)
	if err != nil {
		return nil, err
	}
	registry, err := generationowner.NewRegistry(generationowner.RegistryOptions{
		LoadModel: loadModel,
		Current:   current,
		AuthorityLock: func(ctx context.Context, scope generationowner.Scope) (func(), error) {
			if err := current(ctx, scope); err != nil {
				return nil, err
			}
			return func() {}, nil
		},
	})
	if err != nil {
		return nil, err
	}
	executor := &generationowner.Executor{
		Registry:       registry,
		SecretStorage:  generationowner.PlaintextCredential{},
		Egress:         endpoints,
		DispatchFunc:   config.Dispatch,
	}
	return &syntheticGeneration{
		lifecycle:  durable.lifecycle,
		server:     server,
		registry:   registry,
		executor:   executor,
		loadModel:  loadModel,
		leaseOwner: syntheticLeaseOwner,
	}, nil
}

// syntheticModelProfile converts one private models.json record into a
// generationowner.Profile. The apiKey stays inside the profile's encrypted
// credential slot and is never returned by any DTO, error, or log line.
func syntheticModelProfile(record map[string]any) (generationowner.Profile, error) {
	profile := generationowner.Profile{}
	var ok bool
	if profile.ID, ok = record["id"].(string); !ok || profile.ID == "" {
		return generationowner.Profile{}, errors.New("MODEL_PROFILE_INVALID")
	}
	if profile.Provider, ok = record["provider"].(string); !ok {
		return generationowner.Profile{}, errors.New("MODEL_PROFILE_INVALID")
	}
	if profile.Protocol, ok = record["protocol"].(string); !ok {
		return generationowner.Profile{}, errors.New("MODEL_PROFILE_INVALID")
	}
	if profile.BaseURL, ok = record["baseUrl"].(string); !ok {
		return generationowner.Profile{}, errors.New("MODEL_PROFILE_INVALID")
	}
	if profile.ModelName, ok = record["modelName"].(string); !ok {
		return generationowner.Profile{}, errors.New("MODEL_PROFILE_INVALID")
	}
	if temperature, ok := syntheticFloat64(record["temperature"]); ok {
		profile.Temperature = temperature
	}
	if maxTokens, ok := syntheticFloat64(record["maxTokens"]); ok {
		profile.MaxTokens = int64(maxTokens)
	}
	if purposes, ok := record["purposes"].([]any); ok {
		for _, purpose := range purposes {
			if text, ok := purpose.(string); ok {
				profile.Purposes = append(profile.Purposes, text)
			}
		}
	}
	if capabilities, ok := record["capabilities"]; ok {
		raw, err := json.Marshal(capabilities)
		if err != nil {
			return generationowner.Profile{}, errors.New("MODEL_PROFILE_INVALID")
		}
		profile.Capabilities = raw
	}
	key, ok := record["apiKey"].(string)
	if !ok {
		return generationowner.Profile{}, errors.New("MODEL_PROFILE_INVALID")
	}
	profile.EncryptedCredential = []byte(key)
	return profile, nil
}

// syntheticHash is sha256 lowercase hex, the same primitive as the original
// textHash. It is used for every reduced binding hash and the DTO textHash.
func syntheticHash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// syntheticCanonical returns the deterministic JSON of a finite value with
// sorted object keys (json.Marshal semantics).
func syntheticCanonical(value any) (string, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func syntheticMap(value any) map[string]any {
	if m, ok := value.(map[string]any); ok {
		return m
	}
	return nil
}

// syntheticSlice normalises the slice shapes the ledger and the browser
// contract actually produce. The ledger returns typed slices (for example
// []map[string]any for budget attempts), so a bare []any assertion would
// silently project an empty list.
func syntheticSlice(value any) []any {
	switch values := value.(type) {
	case []any:
		return values
	case []map[string]any:
		out := make([]any, len(values))
		for index, item := range values {
			out[index] = item
		}
		return out
	case []string:
		out := make([]any, len(values))
		for index, item := range values {
			out[index] = item
		}
		return out
	}
	return nil
}

func syntheticString(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	return ""
}

// syntheticFloat64 accepts both float64 (in-process) and json.Number
// (detached config decode) numeric values.
func syntheticFloat64(value any) (float64, bool) {
	switch n := value.(type) {
	case float64:
		return n, true
	case json.Number:
		if parsed, err := n.Float64(); err == nil {
			return parsed, true
		}
	case int64:
		return float64(n), true
	case int:
		return float64(n), true
	}
	return 0, false
}

func syntheticNumber(value any) int64 {
	switch n := value.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	case json.Number:
		if parsed, err := n.Int64(); err == nil {
			return parsed
		}
	case string:
		if parsed, err := strconv.ParseInt(n, 10, 64); err == nil {
			return parsed
		}
	}
	return 0
}

// syntheticArgs marshals ledger call arguments losslessly (objects decode as
// ordered plot objects inside the ledger, which order-sensitive checks need).
func syntheticArgs(values ...any) (json.RawMessage, error) {
	raw, err := json.Marshal(values)
	if err != nil {
		return nil, err
	}
	return raw, nil
}

// syntheticCall invokes a ledger method with marshaled positional arguments.
func syntheticCall(ctx context.Context, ledger *store.GenerationLedger, method string, values ...any) (any, error) {
	args, err := syntheticArgs(values...)
	if err != nil {
		return nil, err
	}
	return ledger.CallJSON(ctx, method, args)
}

// admit re-verifies the frozen context and returns the fence plus the fresh
// ledger/database pair under exactly one release function.
//
// It carries the lane's TWO identities apart on purpose. The project is borrowed
// under the LEASE OWNER (s.leaseOwner), so every tab of one account reaches the
// same single live lease; the returned Scope still records the TAB IDENTITY
// (cookie session plus tab) the receipt is scoped to, so a model lease one tab
// took can never be resolved by its sibling.
func (s *syntheticGeneration) admit(ctx context.Context, caller transport.Caller, expected any) (projectsession.Context, generationowner.Scope, *store.GenerationLedger, *sql.DB, func(), error) {
	contextValue, err := projectsession.ParseContext(expected)
	if err != nil {
		return projectsession.Context{}, generationowner.Scope{}, nil, nil, nil, err
	}
	if s.leaseOwner == nil {
		return projectsession.Context{}, generationowner.Scope{}, nil, nil, nil, projectsession.ErrContext
	}
	leaseOwner, err := s.leaseOwner(caller)
	if err != nil || !leaseOwner.Valid() {
		return projectsession.Context{}, generationowner.Scope{}, nil, nil, nil, projectsession.ErrContext
	}
	ledger, db, refresh, done, err := s.lifecycle.BeginGenerationLedgerDB(ctx, leaseOwner, contextValue)
	if err != nil {
		return projectsession.Context{}, generationowner.Scope{}, nil, nil, nil, err
	}
	// The ledger writes the author's own database, so the permit must adopt the
	// committed identity before the fence is observed again. Composing the
	// refresh into the single release covers every operation exactly once; a
	// refresh that itself fails leaves the evidence untouched, so the next
	// operation is refused rather than silently admitted.
	release := func() {
		if refresh != nil {
			_ = refresh(ctx)
		}
		done()
	}
	scope := generationowner.Scope{
		Caller:       caller.SessionID,
		Session:      caller.SessionID,
		Tab:          caller.ClientID,
		LeaseSession: leaseOwner.SessionID,
		LeaseClient:  leaseOwner.ClientID,
		Project:      contextValue.ProjectID,
		Epoch:        contextValue.LeaseID,
	}
	return contextValue, scope, ledger, db, release, nil
}

// syntheticHandle is the browser run handle (projectId/epoch/rootActionId/runId).
type syntheticHandle struct {
	ProjectID    string
	Epoch        string
	RootActionID string
	RunID        string
}

func syntheticParseHandle(value any) (syntheticHandle, error) {
	object := syntheticMap(value)
	if object == nil {
		return syntheticHandle{}, errors.New("GENERATION_RUN_NOT_FOUND")
	}
	handle := syntheticHandle{
		ProjectID:    syntheticString(object["projectId"]),
		Epoch:        syntheticString(object["epoch"]),
		RootActionID: syntheticString(object["rootActionId"]),
		RunID:        syntheticString(object["runId"]),
	}
	if handle.ProjectID == "" || handle.Epoch == "" || handle.RootActionID == "" || handle.RunID == "" {
		return syntheticHandle{}, errors.New("GENERATION_RUN_NOT_FOUND")
	}
	return handle, nil
}

// syntheticSelection is the reduced BeginGenerationRequest projection the
// synthetic lane accepts. Unknown fields are rejected so the lane never
// silently ignores renderer authority.
type syntheticSelection struct {
	Operation                 string
	UIActionNonce             string
	ModelID                   string
	HasChapterNumber          bool
	ChapterNumber             int64
	SelectedDraftIDs          []int64
	SelectedFinalizedDraftIDs []int64
	SelectedBlueprintChapters []int64
	PromptKeys                []string
	SkillStages               []any
	AuthorInputs              []map[string]any
	Output                    string
	OutputOverrides           []any
	MaterialDecision          map[string]any
	ParentRootActionID        string
	ReviewRevisionContextID   string
	// Frozen, not interpreted: this lane has no directory-range, batch or agent
	// registration registry, so these four ids and the batch intent are recorded
	// in the run's source manifest exactly as the renderer froze them. The run's
	// identity still comes from the operation, the model lease, the context
	// snapshot and the author inputs.
	ContinueDirectoryOperationID string
	BatchID                      string
	BatchIntent                  map[string]any
	FinalizedCharacterContextID  string
	AgentWorkflowRegistrationID  string
	// ImportSlot and ImportExecution are the two fields an import begin sends
	// (src/shared/generation-owner-contract.ts:266-268); productionImportGenerationSelect
	// fills them only for the stage operation that owns the slot, so no other
	// operation can carry either. ImportSlotKey and ImportContext are minted by
	// this lane when it freezes the slot, never taken from the renderer, exactly
	// like the reference's own source binding
	// (electron/services/generation-source-binding.ts:599).
	ImportSlot      map[string]any
	ImportExecution map[string]any
	ImportSlotKey   string
	ImportContext   map[string]any
}

func syntheticParseSelection(value any) (syntheticSelection, error) {
	object := syntheticMap(value)
	if object == nil {
		return syntheticSelection{}, errors.New("GENERATION_BEGIN_INVALID")
	}
	allowed := map[string]bool{
		"operation": true, "uiActionNonce": true, "modelId": true, "chapterNumber": true,
		"selectedDraftIds": true, "selectedFinalizedDraftIds": true, "selectedBlueprintChapterNumbers": true,
		"promptKeys": true, "skillStages": true, "authorInputs": true, "output": true,
		"outputOverrides": true, "parentRootActionId": true, "preparationId": true, "materialDecision": true,
		"reviewRevisionContextId": true, "importSlot": true, "importExecution": true,
		// Five keys the renderer spreads into every begin
		// (src/shared/generation-owner-contract.ts:284-294, sent through
		// src/services/workflows/workflow-main-generation.ts:125 `{ ...intent }`).
		// A fail-closed allowlist that omits them refused the whole begin, which is
		// why blueprint directory continue/append and agent-registered runs never
		// opened. They are frozen below instead of ignored.
		"continueDirectoryOperationId": true, "batchId": true, "batchIntent": true,
		"finalizedCharacterContextId": true, "agentWorkflowRegistrationId": true,
	}
	for key := range object {
		if !allowed[key] {
			return syntheticSelection{}, errors.New("GENERATION_BEGIN_INVALID")
		}
	}
	selection := syntheticSelection{
		Operation:     syntheticString(object["operation"]),
		UIActionNonce: syntheticString(object["uiActionNonce"]),
		ModelID:       syntheticString(object["modelId"]),
		Output:        syntheticString(object["output"]),
	}
	if selection.Operation == "" || selection.UIActionNonce == "" || selection.ModelID == "" {
		return syntheticSelection{}, errors.New("GENERATION_BEGIN_INVALID")
	}
	if syntheticPlanning(selection.Operation) {
		return syntheticSelection{}, errors.New("GENERATION_OPERATION_UNSUPPORTED")
	}
	if selection.Output != "visible-text" && selection.Output != "structured-data" {
		return syntheticSelection{}, errors.New("GENERATION_BEGIN_INVALID")
	}
	// An import selection is admitted only for the stage operation that owns the
	// slot and only in the reference's own stage shape. It is read here, after
	// the planning refusal above, so import-blueprints and every other planning
	// name keeps answering exactly what it answered before this widening; any
	// other request that carries an import key keeps the begin-shape refusal
	// that key has always answered.
	if err := productionImportGenerationSelect(&selection, object); err != nil {
		return syntheticSelection{}, err
	}
	if value, present := object["chapterNumber"]; present {
		chapter := syntheticNumber(value)
		if chapter <= 0 {
			return syntheticSelection{}, errors.New("GENERATION_BEGIN_INVALID")
		}
		selection.HasChapterNumber = true
		selection.ChapterNumber = chapter
	}
	var err error
	if selection.SelectedDraftIDs, err = syntheticIDList(object["selectedDraftIds"]); err != nil {
		return syntheticSelection{}, err
	}
	if selection.SelectedFinalizedDraftIDs, err = syntheticIDList(object["selectedFinalizedDraftIds"]); err != nil {
		return syntheticSelection{}, err
	}
	if selection.SelectedBlueprintChapters, err = syntheticIDList(object["selectedBlueprintChapterNumbers"]); err != nil {
		return syntheticSelection{}, err
	}
	if keys := syntheticSlice(object["promptKeys"]); keys != nil {
		for _, key := range keys {
			text, ok := key.(string)
			if !ok || text == "" || !syntheticPromptKeyPattern.MatchString(text) {
				return syntheticSelection{}, errors.New("GENERATION_BEGIN_INVALID")
			}
			selection.PromptKeys = append(selection.PromptKeys, text)
		}
	}
	if stages := syntheticSlice(object["skillStages"]); stages != nil {
		selection.SkillStages = stages
	}
	if inputs := syntheticSlice(object["authorInputs"]); inputs != nil {
		for _, input := range inputs {
			entry := syntheticMap(input)
			if entry == nil || syntheticString(entry["id"]) == "" {
				return syntheticSelection{}, errors.New("GENERATION_BEGIN_INVALID")
			}
			if _, ok := entry["text"].(string); !ok {
				return syntheticSelection{}, errors.New("GENERATION_BEGIN_INVALID")
			}
			selection.AuthorInputs = append(selection.AuthorInputs, entry)
		}
	}
	if overrides := syntheticSlice(object["outputOverrides"]); overrides != nil {
		selection.OutputOverrides = overrides
	}
	if decision := syntheticMap(object["materialDecision"]); decision != nil {
		validated, err := syntheticMaterialDecision(decision)
		if err != nil {
			return syntheticSelection{}, err
		}
		selection.MaterialDecision = validated
	}
	selection.ParentRootActionID = syntheticString(object["parentRootActionId"])
	selection.ContinueDirectoryOperationID = syntheticString(object["continueDirectoryOperationId"])
	selection.BatchID = syntheticString(object["batchId"])
	selection.BatchIntent = syntheticMap(object["batchIntent"])
	selection.FinalizedCharacterContextID = syntheticString(object["finalizedCharacterContextId"])
	selection.AgentWorkflowRegistrationID = syntheticString(object["agentWorkflowRegistrationId"])
	// The opaque handle `review-revision:prepare` minted. It is never inspected
	// here: the begin admission resolves it against the process registry and
	// refuses the run before any row when it does not resolve
	// (production_review_revision_linux.go: productionReviewRevisionFreeze).
	selection.ReviewRevisionContextID = syntheticString(object["reviewRevisionContextId"])
	return selection, nil
}

func syntheticIDList(value any) ([]int64, error) {
	values := syntheticSlice(value)
	if values == nil {
		return nil, nil
	}
	out := make([]int64, 0, len(values))
	for _, item := range values {
		id := syntheticNumber(item)
		if id <= 0 {
			return nil, errors.New("GENERATION_BEGIN_INVALID")
		}
		out = append(out, id)
	}
	return out, nil
}

// syntheticMaterialDecision validates the renderer's material decision receipt
// shape (version, verdict, prompt hash, capacity/coverage, included/omitted
// source entries). It never reads or recomputes content; it only freezes the
// renderer's closed decision into the run manifest.
func syntheticMaterialDecision(decision map[string]any) (map[string]any, error) {
	invalid := errors.New("GENERATION_MATERIAL_DECISION_INVALID")
	if syntheticNumber(decision["version"]) != 1 || decision["verdict"] != "admitted" {
		return nil, invalid
	}
	if promptHash, ok := decision["promptHash"].(string); !ok || !syntheticHashPattern.MatchString(promptHash) {
		return nil, invalid
	}
	for _, key := range []string{"reconciliationPromptHash", "shortOutlinePromptHash"} {
		if value, ok := decision[key].(string); ok && value != "" && !syntheticHashPattern.MatchString(value) {
			return nil, invalid
		}
	}
	capacity := syntheticMap(decision["capacity"])
	if capacity == nil || syntheticNumber(capacity["maxInputUnits"]) <= 0 || syntheticString(capacity["methodVersion"]) == "" {
		return nil, invalid
	}
	coverage := syntheticMap(decision["coverage"])
	if coverage == nil || !syntheticBool(coverage["complete"]) {
		return nil, invalid
	}
	for _, key := range []string{"included", "omitted"} {
		entries := syntheticSlice(decision[key])
		if entries == nil {
			return nil, invalid
		}
		for _, entry := range entries {
			item := syntheticMap(entry)
			if item == nil || syntheticString(item["sourceId"]) == "" || syntheticString(item["category"]) == "" || !syntheticHashPattern.MatchString(syntheticString(item["contentHash"])) {
				return nil, invalid
			}
		}
	}
	for key := range decision {
		switch key {
		case "version", "verdict", "promptHash", "reconciliationPromptHash", "shortOutlinePromptHash", "capacity", "coverage", "included", "omitted":
		default:
			return nil, invalid
		}
	}
	return decision, nil
}

func syntheticBool(value any) bool {
	if b, ok := value.(bool); ok {
		return b
	}
	return false
}

// syntheticPurpose maps an operation onto the constrained task purpose
// vocabulary, failing closed for operations that cannot be a purpose.
func syntheticPurpose(operation string) string {
	if syntheticPurposePattern.MatchString(operation) {
		return operation
	}
	return syntheticRetryPurpose
}

// syntheticSourceSet is one op's frozen capability read set over the admitted
// author database. Readers are closures over values captured at admission, so
// re-reading (binding validate, execute re-verify) is stable within the call.
type syntheticSourceSet struct {
	Sources   generationowner.Sources
	Selection []generationowner.SourceKey
	CoreText  string
	Core      map[string]any
}

// buildSources reads the real project state (project-core row, selected draft
// full bodies, selected blueprint rows) once and returns the capability
// selection plus the canonical project-core text used for snapshot hashes.
// Missing drafts or blueprints fail closed (GENERATION_SOURCE_UNAVAILABLE).
func syntheticBuildSources(ctx context.Context, db *sql.DB, selection syntheticSelection) (*syntheticSourceSet, error) {
	core, err := store.NewProjectCore(db).Get(ctx)
	if err != nil || core == nil {
		return nil, errors.New("GENERATION_SOURCE_UNAVAILABLE")
	}
	coreText, err := syntheticCanonical(core)
	if err != nil {
		return nil, errors.New("GENERATION_SOURCE_UNAVAILABLE")
	}
	language := syntheticString(core["writingLanguage"])
	if language != "zh-CN" && language != "en-US" {
		language = "zh-CN"
	}
	set := &syntheticSourceSet{CoreText: coreText, Core: core}
	set.Sources.ProjectCore = func(ctx context.Context, scope generationowner.Scope, key generationowner.SourceKey) (generationowner.SourceValue, error) {
		if key.Kind != "project-core" || key.ID != "main" {
			return generationowner.SourceValue{}, errors.New("GENERATION_SOURCE_UNSUPPORTED")
		}
		return generationowner.SourceValue{Revision: 1, Text: coreText}, nil
	}
	set.Sources.Blueprint = func(ctx context.Context, scope generationowner.Scope, key generationowner.SourceKey) (generationowner.SourceValue, error) {
		chapter, err := strconv.ParseInt(key.ID, 10, 64)
		if err != nil {
			return generationowner.SourceValue{}, errors.New("GENERATION_SOURCE_UNSUPPORTED")
		}
		row, err := store.NewBlueprints(db).Get(ctx, chapter)
		if err != nil || row == nil {
			return generationowner.SourceValue{}, errors.New("GENERATION_SOURCE_UNAVAILABLE")
		}
		text, err := syntheticCanonical(row)
		if err != nil {
			return generationowner.SourceValue{}, errors.New("GENERATION_SOURCE_UNAVAILABLE")
		}
		return generationowner.SourceValue{Revision: 1, Text: text}, nil
	}
	set.Sources.Draft = func(ctx context.Context, scope generationowner.Scope, key generationowner.SourceKey) (generationowner.SourceValue, error) {
		id, err := strconv.ParseInt(key.ID, 10, 64)
		if err != nil {
			return generationowner.SourceValue{}, errors.New("GENERATION_SOURCE_UNSUPPORTED")
		}
		full, err := store.NewDrafts(db).GetFull(ctx, id)
		if err != nil || full == nil {
			return generationowner.SourceValue{}, errors.New("GENERATION_SOURCE_UNAVAILABLE")
		}
		return generationowner.SourceValue{Revision: full.Version, Text: full.Content}, nil
	}
	set.Sources.Prompt = func(ctx context.Context, scope generationowner.Scope, key generationowner.SourceKey) (generationowner.SourceValue, error) {
		parts := strings.SplitN(key.ID, ":", 2)
		if len(parts) != 2 || !syntheticPromptSourceKey(parts[0]) || (parts[1] != "zh-CN" && parts[1] != "en-US") {
			return generationowner.SourceValue{}, errors.New("GENERATION_SOURCE_UNSUPPORTED")
		}
		text, err := syntheticCanonical(map[string]any{
			"key":            key.ID,
			"writingLanguage": parts[1],
			"content":        "synthetic:" + key.ID,
		})
		if err != nil {
			return generationowner.SourceValue{}, errors.New("GENERATION_SOURCE_UNAVAILABLE")
		}
		return generationowner.SourceValue{Revision: 1, Text: text}, nil
	}
	for _, id := range selection.SelectedDraftIDs {
		set.Selection = append(set.Selection, generationowner.SourceKey{Kind: "draft", ID: strconv.FormatInt(id, 10)})
	}
	for _, id := range selection.SelectedFinalizedDraftIDs {
		set.Selection = append(set.Selection, generationowner.SourceKey{Kind: "draft", ID: strconv.FormatInt(id, 10)})
	}
	for _, chapter := range selection.SelectedBlueprintChapters {
		set.Selection = append(set.Selection, generationowner.SourceKey{Kind: "blueprint", ID: strconv.FormatInt(chapter, 10)})
	}
	for _, key := range selection.PromptKeys {
		id := key
		if !strings.Contains(id, ":") {
			id = key + ":" + language
		}
		set.Selection = append(set.Selection, generationowner.SourceKey{Kind: "prompt", ID: id})
	}
	if len(set.Selection) == 0 {
		set.Selection = append(set.Selection, generationowner.SourceKey{Kind: "project-core", ID: "main"})
	}
	return set, nil
}

// syntheticOutputContract folds optional output overrides into the same
// contract shape the renderer freezes into the fingerprint.
func syntheticOutputContract(selection syntheticSelection) any {
	if len(selection.OutputOverrides) == 0 {
		return selection.Output
	}
	return map[string]any{"primary": selection.Output, "overrides": selection.OutputOverrides}
}

// syntheticCanonicalPlan derives the deterministic plan the reduced lane uses
// for both the begin-time budget and execute-time reservation. With no
// budgetDemand the plan is canonical (same messages, same root output cap).
func (s *syntheticGeneration) syntheticCanonicalPlan(ctx context.Context, scope generationowner.Scope, leaseID string, selection syntheticSelection) (generationowner.Plan, error) {
	receipt, err := s.registry.Resolve(ctx, leaseID, scope, syntheticGenerationPurpose)
	if err != nil {
		return generationowner.Plan{}, err
	}
	model, found, err := s.loadModel(ctx, selection.ModelID)
	if err != nil {
		return generationowner.Plan{}, err
	}
	if !found {
		return generationowner.Plan{}, errors.New("MODEL_NOT_FOUND")
	}
	task := generationowner.Task{
		Purpose:  syntheticPurpose(selection.Operation),
		Output:   selection.Output,
		Messages: []llm.Message{{Role: "user", Content: strings.Join(selection.PromptKeys, "\n")}},
	}
	return generationowner.BuildPlan(model, receipt, task, syntheticRootOutputTokens)
}

// syntheticBudget is the reduced lane's root budget. It derives from the
// canonical begin-time plan with headroom, so the same plan always fits at
// execute time; the ledger enforces all four positive-integer bounds.
func syntheticBudget(plan generationowner.Plan) (map[string]any, error) {
	if plan.Output <= 0 || plan.Reserved <= 0 {
		return nil, errors.New("GENERATION_PLANNING_BUDGET_INVALID")
	}
	return map[string]any{
		"maxPhysicalRequests": syntheticMaxPhysicalRequests,
		"maxTokenLiability":   plan.Reserved + syntheticLiabilityHeadroom,
		"maxOutputPerRequest": plan.Output + syntheticOutputHeadroom,
		"maxActiveElapsedMs":  syntheticMaxActiveElapsedMs,
	}, nil
}

// syntheticAuthorInputs freezes an empty selection as [] rather than as null.
// The desktop normalises the same field with `?? []` when it reads a run back
// (electron/repositories/generation-run-repository.ts:944), so a run that froze
// no author inputs answers an empty list there. Writing null here would make the
// two builds disagree on a field a renderer may iterate.
func syntheticAuthorInputs(inputs []map[string]any) []map[string]any {
	if inputs == nil {
		return []map[string]any{}
	}
	return inputs
}

// syntheticManifest is the reduced lane's frozen source manifest. The
// goOwnerSourceFingerprint is the Binding digest and must be reproduced by the
// same sources at execute time; modelLeaseId is the only model secret handle.
func syntheticManifest(selection syntheticSelection, scope generationowner.Scope, leaseID, fingerprint string) map[string]any {
	manifest := map[string]any{
		"operation":                  selection.Operation,
		"modelId":                    selection.ModelID,
		"modelLeaseId":               leaseID,
		"goOwnerSourceFingerprint":   fingerprint,
		"purpose":                    syntheticPurpose(selection.Operation),
		"output":                     selection.Output,
		"promptKeys":                 selection.PromptKeys,
		"skillStages":                selection.SkillStages,
		"authorInputs":               syntheticAuthorInputs(selection.AuthorInputs),
		"selectedDraftIds":           selection.SelectedDraftIDs,
		"selectedFinalizedDraftIds":  selection.SelectedFinalizedDraftIDs,
		"selectedBlueprintChapterNumbers": selection.SelectedBlueprintChapters,
	}
	if selection.HasChapterNumber {
		manifest["chapterNumber"] = selection.ChapterNumber
	}
	if selection.MaterialDecision != nil {
		manifest["materialDecision"] = selection.MaterialDecision
	}
	for key, value := range map[string]any{
		"continueDirectoryOperationId": selection.ContinueDirectoryOperationID,
		"batchId":                      selection.BatchID,
		"batchIntent":                  selection.BatchIntent,
		"finalizedCharacterContextId":  selection.FinalizedCharacterContextID,
		"agentWorkflowRegistrationId":  selection.AgentWorkflowRegistrationID,
	} {
		switch typed := value.(type) {
		case nil:
			continue
		case map[string]any:
			if len(typed) == 0 {
				continue
			}
		case string:
			if typed == "" {
				continue
			}
		}
		manifest[key] = value
	}
	if selection.ImportSlotKey != "" {
		// The three members the reference freezes for an import run
		// (electron/services/generation-source-binding.ts:599), written together
		// or not at all: a frozen run can never carry a slot without the key the
		// read matches it on and the context it hands back.
		manifest["importSlot"] = selection.ImportSlot
		manifest["importSlotKey"] = selection.ImportSlotKey
		manifest["importContext"] = selection.ImportContext
	}
	return manifest
}

// syntheticFingerprint derives the nine reduced binding hashes from real
// project state: binding refs (draft/blueprint/prompt capabilities), the
// project-core canonical text, renderer skill stages and author inputs, the
// model lease revision, the canonical budget, and the output contract. All
// nine values are 64-hex so the ledger's GLBinding validation accepts them.
func syntheticFingerprint(ctx context.Context, db *sql.DB, scope generationowner.Scope, binding *generationowner.Binding, set *syntheticSourceSet, selection syntheticSelection, receipt generationowner.Receipt, budget map[string]any) (map[string]any, error) {
	refs := binding.Refs()
	drafts := []any{}
	blueprints := []any{}
	prompts := []any{}
	for _, ref := range refs {
		switch {
		case strings.HasPrefix(ref.SourceID, "draft:"):
			drafts = append(drafts, map[string]any{"sourceId": ref.SourceID, "revision": ref.Revision, "contentHash": ref.ContentHash})
		case strings.HasPrefix(ref.SourceID, "blueprint:"):
			blueprints = append(blueprints, map[string]any{"sourceId": ref.SourceID, "revision": ref.Revision, "contentHash": ref.ContentHash})
		case strings.HasPrefix(ref.SourceID, "prompt:"):
			prompts = append(prompts, map[string]any{"sourceId": ref.SourceID, "revision": ref.Revision, "contentHash": ref.ContentHash})
		}
	}
	hashOf := func(value any) (string, error) {
		text, err := syntheticCanonical(value)
		if err != nil {
			return "", errors.New("GENERATION_SOURCE_UNAVAILABLE")
		}
		return syntheticHash(text), nil
	}
	chapterBrief, err := hashOf(map[string]any{
		"chapterNumber": selection.ChapterNumber,
		"blueprints":    blueprints,
		"drafts":        drafts,
	})
	if err != nil {
		return nil, err
	}
	guidance, err := hashOf(map[string]any{
		"authorInputs":   selection.AuthorInputs,
		"globalGuidance": set.Core["globalGuidance"],
		"writingStyle":   set.Core["writingStyle"],
	})
	if err != nil {
		return nil, err
	}
	dependency, err := hashOf(drafts)
	if err != nil {
		return nil, err
	}
	contextSnapshot, err := hashOf(set.CoreText)
	if err != nil {
		return nil, err
	}
	template, err := hashOf(prompts)
	if err != nil {
		return nil, err
	}
	skills := selection.SkillStages
	if skills == nil {
		skills = []any{}
	}
	skillSnapshot, err := hashOf(skills)
	if err != nil {
		return nil, err
	}
	policy, err := hashOf(budget)
	if err != nil {
		return nil, err
	}
	outputContract, err := hashOf(syntheticOutputContract(selection))
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"chapterBriefHash":    chapterBrief,
		"authorGuidanceHash":  guidance,
		"dependencyHash":      dependency,
		"contextSnapshotHash": contextSnapshot,
		"templateHash":        template,
		"skillSnapshotHash":   skillSnapshot,
		"modelLeaseRevision":  receipt.ModelRevision,
		"policyHash":          policy,
		"outputContractHash":  outputContract,
	}, nil
}

// Begin opens a durable run for the reduced selection: it admits the fence,
// freezes the review-revision context a `reviewRevisionContextId` names (if any),
// begins a model lease, freezes the real project sources, derives the nine
// hashes, computes the canonical budget, and opens the ledger run.
func (s *syntheticGeneration) Begin(ctx context.Context, caller transport.Caller, expected map[string]any, arg any) (any, error) {
	selection, err := syntheticParseSelection(arg)
	if err != nil {
		return nil, err
	}
	_, scope, ledger, db, done, err := s.admit(ctx, caller, expected)
	if err != nil {
		return nil, err
	}
	defer done()
	// An import begin joins the run its slot already owns instead of opening a
	// second one: the reference's admit answers the existing run before any new
	// root and refuses a changed model beside it
	// (electron/services/import-generation.ts:33-44). Opening a second run for one
	// slot would make the read answer GENERATION_IMPORT_SLOT_AMBIGUOUS for a slot
	// the author only resumed.
	if existing, err := productionImportGenerationExisting(ctx, db, ledger, scope.Project, selection); err != nil {
		return nil, err
	} else if existing != nil {
		return s.viewOf(ctx, ledger, db, existing)
	}
	// The review/revise half of admission runs after the lease and before the
	// model lease: it resolves the prepared context, re-proves it against the
	// database, rewrites the selection's one author input and returns the manifest
	// fields the run must freeze. A begin with no handle freezes nothing and takes
	// exactly the path it always took.
	extra, err := productionReviewRevisionFreeze(ctx, caller, db, scope, &selection)
	if err != nil {
		return nil, err
	}
	// An import selection freezes the durable import material beside the slot
	// before the ledger sees the manifest, so the read hands back exactly the
	// context the stage was admitted on. It also rewrites the action nonce to the
	// slot's own, which is what makes the ledger reuse one root per slot; an
	// ordinary begin freezes nothing here and takes the path it always took.
	if err := productionImportGenerationFreeze(ctx, db, &selection); err != nil {
		return nil, err
	}
	runValue, err := s.openRun(ctx, ledger, db, scope, selection, extra)
	if err != nil {
		return nil, err
	}
	return s.viewOf(ctx, ledger, db, runValue)
}

// openRun is the lane's one run-admission path: the model lease, the frozen
// source binding, the nine derived hashes, the canonical budget and the ledger's
// own open. extra is merged into the frozen source manifest before the ledger
// sees it, which is how a graph begin stamps the context it froze; an ordinary
// begin passes nil and freezes exactly the manifest it always froze.
func (s *syntheticGeneration) openRun(ctx context.Context, ledger *store.GenerationLedger, db *sql.DB, scope generationowner.Scope, selection syntheticSelection, extra map[string]any) (any, error) {
	receipt, err := s.registry.Begin(ctx, scope, selection.ModelID, syntheticGenerationPurpose)
	if err != nil {
		return nil, err
	}
	set, err := syntheticBuildSources(ctx, db, selection)
	if err != nil {
		return nil, err
	}
	binding, err := s.registry.BindSources(ctx, scope, receipt.LeaseID, set.Sources, set.Selection)
	if err != nil {
		return nil, err
	}
	plan, err := s.syntheticCanonicalPlan(ctx, scope, receipt.LeaseID, selection)
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
	outputContract := syntheticOutputContract(selection)
	frozenInput, err := syntheticCanonical([]any{fingerprint, set.CoreText, outputContract})
	if err != nil {
		return nil, errors.New("GENERATION_BEGIN_INVALID")
	}
	refs, err := syntheticRefsAsAny(binding.Refs())
	if err != nil {
		return nil, errors.New("GENERATION_BEGIN_INVALID")
	}
	manifest := syntheticManifest(selection, scope, receipt.LeaseID, binding.Fingerprint())
	for key, value := range extra {
		manifest[key] = value
	}
	request := map[string]any{
		"projectId":        scope.Project,
		"epoch":            scope.Epoch,
		"fingerprint":      fingerprint,
		"contextSnapshotId": syntheticHash(set.CoreText),
		"sourceManifest":   manifest,
		"sourceRefs":       refs,
		"operation":        selection.Operation,
		"uiActionNonce":    selection.UIActionNonce,
		"frozenInputHash":  syntheticHash(frozenInput),
		"budget":           budget,
	}
	if selection.ParentRootActionID != "" {
		request["parentRootActionId"] = selection.ParentRootActionID
	}
	args, err := syntheticArgs(request)
	if err != nil {
		return nil, errors.New("GENERATION_BEGIN_INVALID")
	}
	runValue, err := ledger.CallJSON(ctx, "open", args)
	if err != nil {
		return nil, err
	}
	return runValue, nil
}

// BeginGraph answers graph-generation:begin for one graph input, and only for the
// plan kind: the plot kind would have to freeze the whole plot-tree source bundle
// and the event kind a finalized chapter, and this lane captures neither. An
// unsupported kind is refused before any row is written.
//
// The same-nonce look-up runs first, exactly like the desktop owner
// (electron/services/main-generation-owner.ts:1192-1198): a nonce this project
// already spent is a replay when it froze the same input, the original's conflict
// when it did not, and a replay answers the stored run's recovery so a retried
// begin never opens a second root.
func (s *syntheticGeneration) BeginGraph(ctx context.Context, caller transport.Caller, expected map[string]any, arg any) (any, error) {
	request, err := syntheticParseGraphRequest(arg)
	if err != nil {
		return nil, err
	}
	if request.Input.Kind != "plan" {
		return nil, errors.New("GENERATION_OPERATION_UNSUPPORTED")
	}
	_, scope, ledger, db, done, err := s.admit(ctx, caller, expected)
	if err != nil {
		return nil, err
	}
	defer done()
	replay, err := syntheticGraphNonceRun(ctx, ledger, db, scope.Project, request.UIActionNonce, request.Input)
	if err != nil {
		return nil, err
	}
	if replay != nil {
		return s.graphRecovery(ctx, ledger, db, scope.Project, replay)
	}
	// The frozen selection is the desktop's own literal
	// (electron/services/main-generation-owner.ts:1201-1203): the graph operation,
	// the browser nonce, the structured-data output and the family's one prompt
	// template key. The context key is not taken from the request - main mints it -
	// so this lane mints its own.
	selection := syntheticSelection{
		Operation:     syntheticGraphOperations[request.Input.Kind],
		UIActionNonce: request.UIActionNonce,
		ModelID:       request.ModelID,
		PromptKeys:    []string{"graph-" + request.Input.Kind},
		Output:        "structured-data",
	}
	extra, err := s.syntheticFreezeGraphContext(ctx, db, scope, selection, request.Input)
	if err != nil {
		return nil, err
	}
	runValue, err := s.openRun(ctx, ledger, db, scope, selection, extra)
	if err != nil {
		return nil, err
	}
	run := syntheticMap(runValue)
	if run == nil {
		return nil, errors.New("GENERATION_RUN_NOT_FOUND")
	}
	return s.graphRecovery(ctx, ledger, db, scope.Project, run)
}

// syntheticGraphRequest is the graph-generation:begin request the browser sends
// (src/shared/graph-generation.ts:41): the graph input, the model and the action
// nonce, and nothing else. It carries no context key because the renderer never
// has one - main mints it and freezes it beside the input.
type syntheticGraphRequest struct {
	Input         syntheticGraphInput
	ModelID       string
	UIActionNonce string
}

// syntheticParseGraphRequest validates that request the way the desktop owner
// does (electron/services/main-generation-owner.ts:1191-1193): three keys, a
// usable model, and a nonce. Every shape refusal is the original's single code.
func syntheticParseGraphRequest(value any) (syntheticGraphRequest, error) {
	invalid := errors.New("GENERATION_GRAPH_INPUT_INVALID")
	object := syntheticMap(value)
	if object == nil {
		return syntheticGraphRequest{}, invalid
	}
	for key := range object {
		switch key {
		case "input", "modelId", "uiActionNonce":
		default:
			return syntheticGraphRequest{}, invalid
		}
	}
	input, err := syntheticGraphInputOf(object["input"])
	if err != nil {
		return syntheticGraphRequest{}, err
	}
	modelID := syntheticString(object["modelId"])
	nonce := syntheticString(object["uiActionNonce"])
	if modelID == "" || nonce == "" || strings.TrimSpace(nonce) == "" || len([]rune(nonce)) > 256 {
		return syntheticGraphRequest{}, invalid
	}
	return syntheticGraphRequest{Input: input, ModelID: modelID, UIActionNonce: nonce}, nil
}

// syntheticGraphNonceRun is the same-nonce look-up before admission: the run this
// project already opened for that nonce, or nil when the nonce is unspent. Two
// rows is the original's conflict, and so is a row whose frozen input differs from
// the one being requested. Only a run of this family is matched, which is the same
// filter the desktop query applies to the frozen graph context.
func syntheticGraphNonceRun(ctx context.Context, ledger *store.GenerationLedger, db *sql.DB, projectID, nonce string, input syntheticGraphInput) (map[string]any, error) {
	rows, err := db.QueryContext(ctx, `SELECT r.run_id FROM generation_runs r
		JOIN generation_roots g ON g.root_action_id=r.root_action_id
		WHERE json_extract(g.action_json,'$.projectId')=? AND json_extract(g.action_json,'$.uiActionNonce')=?
		AND json_extract(r.binding_json,'$.sourceManifest.graphGenerationContext') IS NOT NULL
		ORDER BY r.rowid`, projectID, nonce)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	runIDs := make([]string, 0, 1)
	for rows.Next() {
		var runID string
		if err := rows.Scan(&runID); err != nil {
			return nil, err
		}
		runIDs = append(runIDs, runID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(runIDs) == 0 {
		return nil, nil
	}
	if len(runIDs) > 1 {
		return nil, errors.New("GENERATION_NONCE_CONFLICT")
	}
	args, err := syntheticArgs(runIDs[0])
	if err != nil {
		return nil, errors.New("GENERATION_RUN_NOT_FOUND")
	}
	value, err := ledger.CallJSON(ctx, "get", args)
	if err != nil {
		return nil, err
	}
	run := syntheticMap(value)
	if run == nil {
		return nil, errors.New("GENERATION_RUN_NOT_FOUND")
	}
	// The frozen context is proved by graphRecovery; here only the input the nonce
	// was spent on decides between a replay and the original's conflict.
	frozen := syntheticMap(syntheticMap(syntheticMap(run["binding"])["sourceManifest"])["graphGenerationContext"])
	if frozen == nil {
		return nil, errors.New("GENERATION_GRAPH_CONTEXT_INVALID")
	}
	frozenDigest, err := syntheticGraphDigest(frozen["input"])
	if err != nil {
		return nil, errors.New("GENERATION_GRAPH_CONTEXT_INVALID")
	}
	requestDigest, err := syntheticGraphDigest(syntheticGraphInputData(input))
	if err != nil {
		return nil, errors.New("GENERATION_GRAPH_INPUT_INVALID")
	}
	if frozenDigest != requestDigest {
		return nil, errors.New("GENERATION_NONCE_CONFLICT")
	}
	return run, nil
}

// syntheticFreezeGraphContext captures the plan context from the admitted author
// database the way the desktop does
// (electron/services/graph-generation-source.ts:26-50) and returns the manifest
// keys the graph read re-proves: the input twice (inside the context and beside
// it), the context and its digest, the origin epoch, and the task with its digest.
//
// The context key is this lane's own. The desktop mints a random UUID
// (electron/services/main-generation-owner.ts:1203) and never compares it with a
// renderer value, so a stable per-(project, nonce) key is a valid main-issued key
// and makes a duplicate begin freeze the same context.
func (s *syntheticGeneration) syntheticFreezeGraphContext(ctx context.Context, db *sql.DB, scope generationowner.Scope, selection syntheticSelection, input syntheticGraphInput) (map[string]any, error) {
	core, err := store.NewProjectCore(db).Get(ctx)
	if err != nil || core == nil {
		return nil, errors.New("GENERATION_GRAPH_PROJECT_REQUIRED")
	}
	totalChapters := syntheticNumber(core["totalChapters"])
	if totalChapters < 1 || input.ChapterNumber > totalChapters {
		return nil, errors.New("GENERATION_GRAPH_BLUEPRINT_REQUIRED")
	}
	blueprint, err := store.NewBlueprints(db).Get(ctx, input.ChapterNumber)
	if err != nil {
		return nil, errors.New("GENERATION_GRAPH_CONTEXT_INVALID")
	}
	if blueprint == nil || syntheticNumber(blueprint["chapterNumber"]) != input.ChapterNumber {
		return nil, errors.New("GENERATION_GRAPH_BLUEPRINT_REQUIRED")
	}
	contextValue := map[string]any{
		"projectId":       scope.Project,
		"originEpoch":     scope.Epoch,
		"key":             syntheticGraphKey(scope.Project, selection.UIActionNonce),
		"createdAt":       time.Now().UTC().Format(time.RFC3339Nano),
		"input":           syntheticGraphInputData(input),
		"writingLanguage": syntheticGraphWritingLanguage(core["writingLanguage"]),
		"kind":            input.Kind,
		"blueprint":       blueprint,
		"totalChapters":   totalChapters,
	}
	task, err := syntheticGraphTask(input.Kind, contextValue)
	if err != nil {
		return nil, err
	}
	taskValue, err := syntheticGraphValue(task)
	if err != nil {
		return nil, errors.New("GENERATION_GRAPH_TASK_INVALID")
	}
	taskHash, err := syntheticGraphDigest(taskValue)
	if err != nil {
		return nil, errors.New("GENERATION_GRAPH_TASK_INVALID")
	}
	contextHash, err := syntheticGraphDigest(contextValue)
	if err != nil {
		return nil, errors.New("GENERATION_GRAPH_CONTEXT_INVALID")
	}
	return map[string]any{
		"graphGenerationInput":       contextValue["input"],
		"graphGenerationKey":         contextValue["key"],
		"graphGenerationContext":     contextValue,
		"graphGenerationContextHash": contextHash,
		"graphGenerationOriginEpoch": scope.Epoch,
		"graphGenerationTask":        taskValue,
		"graphGenerationTaskHash":    taskHash,
	}, nil
}

func syntheticRefsAsAny(refs []generationowner.Ref) ([]any, error) {
	raw, err := json.Marshal(refs)
	if err != nil {
		return nil, err
	}
	var out []any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// syntheticRequireRun verifies the browser handle against the ledger run:
// identity fields, binding project/epoch, and the manifest. It returns the run
// map plus its binding and manifest for the calling operation.
func syntheticRequireRun(ctx context.Context, ledger *store.GenerationLedger, scope generationowner.Scope, handle syntheticHandle) (map[string]any, map[string]any, map[string]any, error) {
	args, err := syntheticArgs(handle.RunID)
	if err != nil {
		return nil, nil, nil, errors.New("GENERATION_RUN_NOT_FOUND")
	}
	value, err := ledger.CallJSON(ctx, "get", args)
	if err != nil {
		return nil, nil, nil, err
	}
	run := syntheticMap(value)
	if run == nil {
		return nil, nil, nil, errors.New("GENERATION_RUN_NOT_FOUND")
	}
	binding := syntheticMap(run["binding"])
	manifest := syntheticMap(binding["sourceManifest"])
	if run["rootActionId"] != handle.RootActionID || run["runId"] != handle.RunID ||
		binding["projectId"] != scope.Project || binding["epoch"] != scope.Epoch || manifest == nil {
		return nil, nil, nil, errors.New("GENERATION_RUN_NOT_FOUND")
	}
	return run, binding, manifest, nil
}

// newService builds the ordinary service over the op's ledger with the reduced
// lane's snapshot publisher and a ValidateRecovery that accepts the current
// binding (resume never changes the frozen sources in this lane).
func (s *syntheticGeneration) newService(ctx context.Context, caller transport.Caller, expected projectsession.Context, ledger *store.GenerationLedger, db *sql.DB, scope generationowner.Scope, handle syntheticHandle) (*generationowner.OrdinaryService, error) {
	service, err := s.executor.NewOrdinaryService(ledger, generationowner.OrdinaryCallbacks{
		OnSnapshot: func(snapshot generation.Snapshot) {
			s.publishSnapshot(caller, expected, handle, ledger, db, snapshot)
		},
		ValidateRecovery: func(ctx context.Context, previous, next map[string]any) (bool, error) {
			// Resume in this lane stores the current frozen binding unchanged.
			// Compare canonical JSON: the ledger's stored map carries
			// json.Number values while the decoded next map carries float64,
			// so a value-level DeepEqual would wrongly refuse.
			prev, aErr := json.Marshal(previous)
			nextValue, bErr := json.Marshal(next)
			if aErr != nil || bErr != nil {
				return false, nil
			}
			return bytes.Equal(prev, nextValue), nil
		},
	})
	if err != nil {
		return nil, err
	}
	return service, nil
}

// rebuildSourcesForRun reconstructs the exact begin-time source set from the
// manifest so the binding fingerprint matches goOwnerSourceFingerprint. Prompt
// keys were stored with their language suffix at begin, so no language
// parameter is needed here.
func (s *syntheticGeneration) rebuildSourcesForRun(ctx context.Context, db *sql.DB, manifest map[string]any) (*syntheticSourceSet, error) {
	selection := syntheticSelection{
		Operation:   syntheticString(manifest["operation"]),
		ModelID:     syntheticString(manifest["modelId"]),
		Output:      syntheticString(manifest["output"]),
		PromptKeys:  syntheticStringList(manifest["promptKeys"]),
		SkillStages: syntheticSlice(manifest["skillStages"]),
	}
	selection.AuthorInputs = syntheticMapList(manifest["authorInputs"])
	selection.SelectedDraftIDs = syntheticIDListFromAny(manifest["selectedDraftIds"])
	selection.SelectedFinalizedDraftIDs = syntheticIDListFromAny(manifest["selectedFinalizedDraftIds"])
	selection.SelectedBlueprintChapters = syntheticIDListFromAny(manifest["selectedBlueprintChapterNumbers"])
	if value, present := manifest["chapterNumber"]; present {
		selection.HasChapterNumber = true
		selection.ChapterNumber = syntheticNumber(value)
	}
	return syntheticBuildSources(ctx, db, selection)
}

func syntheticStringList(value any) []string {
	values := syntheticSlice(value)
	if values == nil {
		return nil
	}
	out := make([]string, 0, len(values))
	for _, item := range values {
		if text, ok := item.(string); ok {
			out = append(out, text)
		}
	}
	return out
}

func syntheticMapList(value any) []map[string]any {
	values := syntheticSlice(value)
	if values == nil {
		return nil
	}
	out := make([]map[string]any, 0, len(values))
	for _, item := range values {
		if m := syntheticMap(item); m != nil {
			out = append(out, m)
		}
	}
	return out
}

func syntheticIDListFromAny(value any) []int64 {
	values := syntheticSlice(value)
	if values == nil {
		return nil
	}
	out := make([]int64, 0, len(values))
	for _, item := range values {
		out = append(out, syntheticNumber(item))
	}
	return out
}

// Execute re-admits the fence, resolves the begin-time lease, rebuilds and
// re-verifies the source binding, parses the browser task, prepares the frozen
// plan, then runs the ordinary service (fake dispatch in tests, native
// dispatch otherwise). The result is the MainGenerationExecuteReceipt DTO.
func (s *syntheticGeneration) Execute(ctx context.Context, caller transport.Caller, expected map[string]any, arg any) (any, error) {
	request := syntheticMap(arg)
	if request == nil {
		return nil, errors.New("GENERATION_EXECUTE_INVALID")
	}
	handle, err := syntheticParseHandle(request["handle"])
	if err != nil {
		return nil, err
	}
	nonce := syntheticString(request["invocationNonce"])
	if nonce == "" {
		return nil, errors.New("GENERATION_EXECUTE_INVALID")
	}
	contextValue, scope, ledger, db, done, err := s.admit(ctx, caller, expected)
	if err != nil {
		return nil, err
	}
	defer done()
	_, _, manifest, err := syntheticRequireRun(ctx, ledger, scope, handle)
	if err != nil {
		return nil, err
	}
	return s.syntheticDispatchFrozenRun(ctx, caller, contextValue, ledger, db, scope, handle, manifest, nonce, request["task"])
}

// syntheticDispatchFrozenRun is the half generation:execute shares with the
// import family: it re-resolves the begin-time lease, rebuilds and re-verifies
// the frozen sources, parses the browser task, prepares the plan, settles one
// attempt under the given invocation nonce and projects the receipt. handle and
// manifest are the run the caller already proved - generation:execute through
// syntheticRequireRun, the import family through that same call plus the import
// context proof - so this helper never widens which run may dispatch.
func (s *syntheticGeneration) syntheticDispatchFrozenRun(ctx context.Context, caller transport.Caller, contextValue projectsession.Context, ledger *store.GenerationLedger, db *sql.DB, scope generationowner.Scope, handle syntheticHandle, manifest map[string]any, invocationNonce string, taskValue any) (any, error) {
	leaseID := syntheticString(manifest["modelLeaseId"])
	if leaseID == "" {
		return nil, errors.New("GENERATION_LEASE_MISSING")
	}
	if _, err := s.registry.Resolve(ctx, leaseID, scope, syntheticGenerationPurpose); err != nil {
		return nil, err
	}
	set, err := s.rebuildSourcesForRun(ctx, db, manifest)
	if err != nil {
		return nil, err
	}
	bindingValue, err := s.registry.BindSources(ctx, scope, leaseID, set.Sources, set.Selection)
	if err != nil {
		return nil, err
	}
	if bindingValue.Fingerprint() != syntheticString(manifest["goOwnerSourceFingerprint"]) {
		return nil, errors.New("GENERATION_SOURCE_CHANGED")
	}
	taskRaw, err := json.Marshal(taskValue)
	if err != nil {
		return nil, errors.New("GENERATION_SEMANTIC_TASK_INVALID")
	}
	task, err := generationowner.ParseTask(taskRaw)
	if err != nil {
		return nil, err
	}
	frozen, err := s.executor.Prepare(ctx, scope, leaseID, task, syntheticRootOutputTokens, bindingValue)
	if err != nil {
		return nil, err
	}
	service, err := s.newService(ctx, caller, contextValue, ledger, db, scope, handle)
	if err != nil {
		return nil, err
	}
	receipt, err := service.Execute(ctx, frozen, handle.RunID, invocationNonce)
	if err != nil {
		return nil, err
	}
	outcome := syntheticOutcome(syntheticMap(receipt))
	view, err := s.viewOf(ctx, ledger, db, syntheticMap(receipt)["run"])
	if err != nil {
		return nil, err
	}
	return map[string]any{"outcome": outcome, "run": view}, nil
}

// syntheticOutcome projects the settle receipt into the browser
// MainGenerationExecuteReceipt outcome (completed on stop, incomplete
// otherwise; content and finishReason come from the settled artifact).
func syntheticOutcome(receipt map[string]any) map[string]any {
	artifact := syntheticMap(receipt["artifact"])
	result := syntheticMap(receipt["result"])
	finish := ""
	if result != nil {
		finish = syntheticString(result["finishReason"])
	}
	status := "incomplete"
	if finish == "stop" {
		status = "completed"
	}
	content := ""
	if artifact != nil {
		content = syntheticString(artifact["text"])
	}
	// The browser reads outcome.receipt.finishReason beside outcome.finishReason
	// (generation-runtime.ts: INVALID_MAIN_OUTCOME) and the ledger receipt nests
	// the settled value under result.finishReason, so surface it at the receipt's
	// top level too. The rest of the raw ledger receipt stays as extra context.
	projected := make(map[string]any, len(receipt)+1)
	for key, value := range receipt {
		projected[key] = value
	}
	projected["finishReason"] = finish
	outcome := map[string]any{
		"status":       status,
		"content":      content,
		"finishReason": finish,
		"receipt":      projected,
	}
	return outcome
}

// RetryDraftShortOutline re-executes a failed draft-short-outline attempt on
// the same run. The reduced lane verifies the chapter-draft manifest, the
// renderer's material decision short-outline hash, and that the failed
// attempt settled with a length stop; then it runs a synthetic continuation
// task with a retry-specific invocation nonce.
func (s *syntheticGeneration) RetryDraftShortOutline(ctx context.Context, caller transport.Caller, expected map[string]any, arg any) (any, error) {
	request := syntheticMap(arg)
	if request == nil {
		return nil, errors.New("GENERATION_DRAFT_SHORT_OUTLINE_RETRY_UNAVAILABLE")
	}
	handle, err := syntheticParseHandle(request["handle"])
	if err != nil {
		return nil, err
	}
	failedAttemptID := syntheticString(request["failedAttemptId"])
	if failedAttemptID == "" {
		return nil, errors.New("GENERATION_DRAFT_SHORT_OUTLINE_RETRY_UNAVAILABLE")
	}
	contextValue, scope, ledger, db, done, err := s.admit(ctx, caller, expected)
	if err != nil {
		return nil, err
	}
	defer done()
	run, _, manifest, err := syntheticRequireRun(ctx, ledger, scope, handle)
	if err != nil {
		return nil, err
	}
	if manifest["operation"] != "chapter-draft" {
		return nil, errors.New("GENERATION_DRAFT_SHORT_OUTLINE_RETRY_UNAVAILABLE")
	}
	decision := syntheticMap(manifest["materialDecision"])
	if decision == nil || syntheticString(decision["shortOutlinePromptHash"]) == "" {
		return nil, errors.New("GENERATION_DRAFT_SHORT_OUTLINE_RETRY_UNAVAILABLE")
	}
	if err := s.syntheticAssertShortOutlineFailure(ctx, ledger, db, run, failedAttemptID); err != nil {
		return nil, err
	}
	leaseID := syntheticString(manifest["modelLeaseId"])
	if leaseID == "" {
		return nil, errors.New("GENERATION_LEASE_MISSING")
	}
	if _, err := s.registry.Resolve(ctx, leaseID, scope, syntheticGenerationPurpose); err != nil {
		return nil, err
	}
	set, err := s.rebuildSourcesForRun(ctx, db, manifest)
	if err != nil {
		return nil, err
	}
	bindingValue, err := s.registry.BindSources(ctx, scope, leaseID, set.Sources, set.Selection)
	if err != nil {
		return nil, err
	}
	if bindingValue.Fingerprint() != syntheticString(manifest["goOwnerSourceFingerprint"]) {
		return nil, errors.New("GENERATION_SOURCE_CHANGED")
	}
	task := generationowner.Task{
		Purpose:  syntheticRetryPurpose,
		Output:   syntheticString(manifest["output"]),
		Messages: []llm.Message{{Role: "user", Content: "retry draft short outline after length stop"}},
	}
	frozen, err := s.executor.Prepare(ctx, scope, leaseID, task, syntheticRootOutputTokens, bindingValue)
	if err != nil {
		return nil, err
	}
	service, err := s.newService(ctx, caller, contextValue, ledger, db, scope, handle)
	if err != nil {
		return nil, err
	}
	receipt, err := service.Execute(ctx, frozen, handle.RunID, syntheticRetryNoncePrefix+failedAttemptID)
	if err != nil {
		return nil, err
	}
	outcome := syntheticOutcome(syntheticMap(receipt))
	view, err := s.viewOf(ctx, ledger, db, syntheticMap(receipt)["run"])
	if err != nil {
		return nil, err
	}
	return map[string]any{"outcome": outcome, "run": view}, nil
}

// syntheticAssertShortOutlineFailure mirrors the renderer's availability gate:
// the failed attempt must be the run's first attempt with the retry purpose,
// settled, without failure code or unsaved tail, with an artifact whose finish
// was a length stop or an empty stop. The ledger's own accounting plus the
// stored attempt purpose are the authority.
func (s *syntheticGeneration) syntheticAssertShortOutlineFailure(ctx context.Context, ledger *store.GenerationLedger, db *sql.DB, run map[string]any, failedAttemptID string) error {
	budgetValue, err := syntheticCall(ctx, ledger, "budget", run["rootActionId"])
	if err != nil {
		return err
	}
	budget := syntheticMap(budgetValue)
	attempts := syntheticSlice(budget["attempts"])
	first := syntheticMap(firstOf(attempts))
	if first == nil || syntheticString(first["attemptId"]) != failedAttemptID {
		return errors.New("GENERATION_DRAFT_SHORT_OUTLINE_RETRY_UNAVAILABLE")
	}
	purpose, err := s.syntheticAttemptPurpose(ctx, db, failedAttemptID)
	if err != nil {
		return err
	}
	if purpose != syntheticRetryPurpose {
		return errors.New("GENERATION_DRAFT_SHORT_OUTLINE_RETRY_UNAVAILABLE")
	}
	receiptValue, err := syntheticCall(ctx, ledger, "receipt", failedAttemptID)
	if err != nil {
		return err
	}
	receipt := syntheticMap(receiptValue)
	attempt := syntheticMap(receipt["attempt"])
	artifact := syntheticMap(receipt["artifact"])
	result := syntheticMap(receipt["result"])
	if attempt == nil || attempt["status"] != "settled" || receipt["failureCode"] != nil || artifact == nil {
		return errors.New("GENERATION_DRAFT_SHORT_OUTLINE_RETRY_UNAVAILABLE")
	}
	finish := ""
	if result != nil {
		finish = syntheticString(result["finishReason"])
	}
	text := syntheticString(artifact["text"])
	if finish != "length" && !(finish == "stop" && strings.TrimSpace(text) == "") {
		return errors.New("GENERATION_DRAFT_SHORT_OUTLINE_RETRY_UNAVAILABLE")
	}
	return nil
}

// syntheticAttemptPurpose reads the stored attempt purpose straight from the
// usage receipt the reserve wrote (the ledger receipt projection does not
// surface it).
func (s *syntheticGeneration) syntheticAttemptPurpose(ctx context.Context, db *sql.DB, attemptID string) (string, error) {
	if db == nil {
		return "", errors.New("GENERATION_DRAFT_SHORT_OUTLINE_RETRY_UNAVAILABLE")
	}
	var raw string
	err := db.QueryRowContext(ctx, "SELECT usage_receipt_json FROM generation_attempts WHERE attempt_id=?", attemptID).Scan(&raw)
	if err != nil {
		return "", errors.New("GENERATION_DRAFT_SHORT_OUTLINE_RETRY_UNAVAILABLE")
	}
	var usage map[string]any
	if err := json.Unmarshal([]byte(raw), &usage); err != nil {
		return "", errors.New("GENERATION_DRAFT_SHORT_OUTLINE_RETRY_UNAVAILABLE")
	}
	return syntheticString(usage["purpose"]), nil
}

func firstOf(values []any) any {
	if len(values) == 0 {
		return nil
	}
	return values[0]
}

// Read returns the run view for one handle.
func (s *syntheticGeneration) Read(ctx context.Context, caller transport.Caller, expected map[string]any, arg any) (any, error) {
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
	return s.viewOf(ctx, ledger, db, run)
}

// ReadCommitted returns the run view for one committed handle whose frozen
// binding epoch need not equal the caller's current lease. The caller still has
// to hold a live lease for the project it names: the admission below is the same
// single-writer fence every other operation uses, so a stale or foreign tab gets
// a lease refusal, never data.
//
// The original's read path checks identity only — projectId plus the exact
// handle — and reports an older epoch as `nonReplayable` instead of refusing it
// (electron/services/main-generation-owner.ts:333), because a lease rotates on
// every save and a committed run outlives the lease it was frozen under.
// syntheticRequireRun deliberately refuses that case, which is right for a
// mutation and wrong for history, so the epoch-independent read is its own
// method rather than a relaxed mutation gate.
func (s *syntheticGeneration) ReadCommitted(ctx context.Context, caller transport.Caller, expected map[string]any, arg any) (any, error) {
	handle, err := syntheticParseHandle(arg)
	if err != nil {
		return nil, err
	}
	_, scope, ledger, db, done, err := s.admit(ctx, caller, expected)
	if err != nil {
		return nil, err
	}
	defer done()
	args, err := syntheticArgs(handle.RunID)
	if err != nil {
		return nil, errors.New("GENERATION_RUN_NOT_FOUND")
	}
	value, err := ledger.CallJSON(ctx, "get", args)
	if err != nil {
		return nil, err
	}
	run := syntheticMap(value)
	if run == nil {
		return nil, errors.New("GENERATION_RUN_NOT_FOUND")
	}
	binding := syntheticMap(run["binding"])
	if binding == nil {
		return nil, errors.New("GENERATION_RUN_NOT_FOUND")
	}
	// Identity, exactly as the original's handleOf(run) comparison: the request
	// must name this run's own project, epoch, root and run id, and the run must
	// belong to the project this frozen context names. Only the equality between
	// binding.epoch and the *calling* lease is dropped.
	if run["rootActionId"] != handle.RootActionID || run["runId"] != handle.RunID ||
		binding["projectId"] != scope.Project || binding["epoch"] != handle.Epoch ||
		syntheticMap(binding["sourceManifest"]) == nil {
		return nil, errors.New("GENERATION_RUN_NOT_FOUND")
	}
	return s.viewFor(ctx, ledger, db, run, scope.Epoch)
}

// List returns MainGenerationRunSummary entries (newest first) for the frozen
// project. It is read-only and only ever projects runs of this project.
func (s *syntheticGeneration) List(ctx context.Context, caller transport.Caller, expected map[string]any) (any, error) {
	_, scope, ledger, _, done, err := s.admit(ctx, caller, expected)
	if err != nil {
		return nil, err
	}
	defer done()
	rows, err := ledger.ListRuns(ctx, scope.Project)
	if err != nil {
		return nil, err
	}
	out := make([]any, 0, len(rows))
	for _, row := range rows {
		summary := map[string]any{
			"handle": map[string]any{
				"projectId":    row["projectId"],
				"epoch":        row["epoch"],
				"rootActionId": row["rootActionId"],
				"runId":        row["runId"],
			},
			"status":    row["status"],
			"operation": row["operation"],
		}
		out = append(out, summary)
	}
	return out, nil
}

// Pause suspends the run's root (all reserved attempts become
// cancelled-before-dispatch) and returns the updated view.
func (s *syntheticGeneration) Pause(ctx context.Context, caller transport.Caller, expected map[string]any, arg any) (any, error) {
	return s.stopRun(ctx, caller, expected, arg, false)
}

// Cancel cancels the run's root and returns the updated view.
func (s *syntheticGeneration) Cancel(ctx context.Context, caller transport.Caller, expected map[string]any, arg any) (any, error) {
	return s.stopRun(ctx, caller, expected, arg, true)
}

func (s *syntheticGeneration) stopRun(ctx context.Context, caller transport.Caller, expected map[string]any, arg any, cancelRoot bool) (any, error) {
	handle, err := syntheticParseHandle(arg)
	if err != nil {
		return nil, err
	}
	contextValue, scope, ledger, db, done, err := s.admit(ctx, caller, expected)
	if err != nil {
		return nil, err
	}
	defer done()
	run, _, _, err := syntheticRequireRun(ctx, ledger, scope, handle)
	if err != nil {
		return nil, err
	}
	service, err := s.newService(ctx, caller, contextValue, ledger, db, scope, handle)
	if err != nil {
		return nil, err
	}
	if cancelRoot {
		_, err = service.Cancel(ctx, syntheticString(run["rootActionId"]))
	} else {
		_, err = service.Pause(ctx, syntheticString(run["rootActionId"]))
	}
	if err != nil {
		return nil, err
	}
	// The ordinary service returns the post-stop budget, not the run, so the
	// projection starts from a fresh ledger read of the same run.
	current, err := syntheticCall(ctx, ledger, "get", handle.RunID)
	if err != nil {
		return nil, err
	}
	return s.viewOf(ctx, ledger, db, current)
}

// Resume continues a paused run by storing the current (unchanged) frozen
// binding; the ordinary service's ValidateRecovery accepts exactly that.
func (s *syntheticGeneration) Resume(ctx context.Context, caller transport.Caller, expected map[string]any, arg any) (any, error) {
	handle, err := syntheticParseHandle(arg)
	if err != nil {
		return nil, err
	}
	contextValue, scope, ledger, db, done, err := s.admit(ctx, caller, expected)
	if err != nil {
		return nil, err
	}
	defer done()
	run, _, _, err := syntheticRequireRun(ctx, ledger, scope, handle)
	if err != nil {
		return nil, err
	}
	if run["status"] != "paused" {
		return nil, errors.New("GENERATION_RESUME_UNAVAILABLE")
	}
	nextRaw, err := json.Marshal(run["binding"])
	if err != nil {
		return nil, errors.New("GENERATION_RECOVERY_UNAUTHORIZED")
	}
	service, err := s.newService(ctx, caller, contextValue, ledger, db, scope, handle)
	if err != nil {
		return nil, err
	}
	if _, err := service.Resume(ctx, syntheticString(run["runId"]), nextRaw); err != nil {
		return nil, err
	}
	current, err := syntheticCall(ctx, ledger, "get", handle.RunID)
	if err != nil {
		return nil, err
	}
	return s.viewOf(ctx, ledger, db, current)
}

// Restart opens a replacement run from the new selection (the original begins
// the new run first so rejected sources leave the paused run resumable), then
// cancels the old root, exactly like the original implementation.
func (s *syntheticGeneration) Restart(ctx context.Context, caller transport.Caller, expected map[string]any, arg any, selectionArg any) (any, error) {
	handle, err := syntheticParseHandle(arg)
	if err != nil {
		return nil, err
	}
	selection, err := syntheticParseSelection(selectionArg)
	if err != nil {
		return nil, err
	}
	if selection.ParentRootActionID != "" {
		return nil, errors.New("GENERATION_RESTART_NEW_NONCE_REQUIRED")
	}
	contextValue, scope, ledger, db, done, err := s.admit(ctx, caller, expected)
	if err != nil {
		return nil, err
	}
	defer done()
	run, _, _, err := syntheticRequireRun(ctx, ledger, scope, handle)
	if err != nil {
		return nil, err
	}
	budgetValue, err := syntheticCall(ctx, ledger, "budget", run["rootActionId"])
	if err != nil {
		return nil, err
	}
	budget := syntheticMap(budgetValue)
	root := syntheticMap(budget["root"])
	if syntheticString(root["uiActionNonce"]) == selection.UIActionNonce {
		return nil, errors.New("GENERATION_RESTART_NEW_NONCE_REQUIRED")
	}
	nextValue, err := s.begin(ctx, caller, expected, scope, ledger, db, selection)
	if err != nil {
		return nil, err
	}
	service, err := s.newService(ctx, caller, contextValue, ledger, db, scope, handle)
	if err != nil {
		return nil, err
	}
	if _, err := service.Cancel(ctx, syntheticString(run["rootActionId"])); err != nil {
		return nil, err
	}
	return nextValue, nil
}

// begin is the restart path's begin: Restart reopens the selection and then
// cancels the old root.
func (s *syntheticGeneration) begin(ctx context.Context, caller transport.Caller, expected map[string]any, scope generationowner.Scope, ledger *store.GenerationLedger, db *sql.DB, selection syntheticSelection) (any, error) {
	if selection.ImportSlot != nil {
		// A restart does not freeze import material, so an import selection keeps
		// the begin-shape refusal it answered before the admission widened: an
		// import run is joined through generation:begin, which answers the slot's
		// existing run instead of opening a second root
		// (electron/services/import-generation.ts:33-40). Restarting the old root
		// while opening a slotless run of the same operation would leave a run no
		// slot owns.
		return nil, errors.New("GENERATION_BEGIN_INVALID")
	}
	receipt, err := s.registry.Begin(ctx, scope, selection.ModelID, syntheticGenerationPurpose)
	if err != nil {
		return nil, err
	}
	set, err := syntheticBuildSources(ctx, db, selection)
	if err != nil {
		return nil, err
	}
	binding, err := s.registry.BindSources(ctx, scope, receipt.LeaseID, set.Sources, set.Selection)
	if err != nil {
		return nil, err
	}
	plan, err := s.syntheticCanonicalPlan(ctx, scope, receipt.LeaseID, selection)
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
	outputContract := syntheticOutputContract(selection)
	frozenInput, err := syntheticCanonical([]any{fingerprint, set.CoreText, outputContract})
	if err != nil {
		return nil, errors.New("GENERATION_BEGIN_INVALID")
	}
	refs, err := syntheticRefsAsAny(binding.Refs())
	if err != nil {
		return nil, errors.New("GENERATION_BEGIN_INVALID")
	}
	request := map[string]any{
		"projectId":         scope.Project,
		"epoch":             scope.Epoch,
		"fingerprint":       fingerprint,
		"contextSnapshotId": syntheticHash(set.CoreText),
		"sourceManifest":    syntheticManifest(selection, scope, receipt.LeaseID, binding.Fingerprint()),
		"sourceRefs":        refs,
		"operation":         selection.Operation,
		"uiActionNonce":     selection.UIActionNonce,
		"frozenInputHash":   syntheticHash(frozenInput),
		"budget":            budget,
	}
	if selection.ParentRootActionID != "" {
		request["parentRootActionId"] = selection.ParentRootActionID
	}
	args, err := syntheticArgs(request)
	if err != nil {
		return nil, errors.New("GENERATION_BEGIN_INVALID")
	}
	runValue, err := ledger.CallJSON(ctx, "open", args)
	if err != nil {
		return nil, err
	}
	return s.viewOf(ctx, ledger, db, runValue)
}

// BindMaterialDecision freezes the renderer's closed material decision into an
// unstarted review/refine run's manifest, or returns the current view when the
// same decision is already bound (GENERATION_MATERIAL_DECISION_CONFLICT on a
// different one). The ledger enforces the unstarted/active gates.
func (s *syntheticGeneration) BindMaterialDecision(ctx context.Context, caller transport.Caller, expected map[string]any, arg any) (any, error) {
	request := syntheticMap(arg)
	if request == nil {
		return nil, errors.New("GENERATION_MATERIAL_DECISION_INVALID")
	}
	handle, err := syntheticParseHandle(request["handle"])
	if err != nil {
		return nil, err
	}
	decision, err := syntheticMaterialDecision(syntheticMap(request["materialDecision"]))
	if err != nil {
		return nil, err
	}
	_, scope, ledger, db, done, err := s.admit(ctx, caller, expected)
	if err != nil {
		return nil, err
	}
	defer done()
	run, binding, manifest, err := syntheticRequireRun(ctx, ledger, scope, handle)
	if err != nil {
		return nil, err
	}
	switch manifest["operation"] {
	case "review-chapter", "refine-draft", "refine-from-review":
	default:
		return nil, errors.New("GENERATION_MATERIAL_DECISION_INVALID")
	}
	if existing := syntheticMap(manifest["materialDecision"]); existing != nil {
		// Canonical compare: the stored decision carries json.Number values
		// while the fresh request decision carries float64.
		storedJSON, aErr := json.Marshal(existing)
		incomingJSON, bErr := json.Marshal(decision)
		if aErr == nil && bErr == nil && bytes.Equal(storedJSON, incomingJSON) {
			return s.viewOf(ctx, ledger, db, run)
		}
		return nil, errors.New("GENERATION_MATERIAL_DECISION_CONFLICT")
	}
	next := make(map[string]any, len(binding))
	for key, value := range binding {
		next[key] = value
	}
	nextManifest := make(map[string]any, len(manifest))
	for key, value := range manifest {
		nextManifest[key] = value
	}
	nextManifest["materialDecision"] = decision
	next["sourceManifest"] = nextManifest
	args, err := syntheticArgs(run["runId"], binding, next)
	if err != nil {
		return nil, errors.New("GENERATION_MATERIAL_DECISION_INVALID")
	}
	value, err := ledger.CallJSON(ctx, "replaceUnstartedBinding", args)
	if err != nil {
		return nil, err
	}
	return s.viewOf(ctx, ledger, db, value)
}

// DiscardCandidate marks the artifact discarded and returns the updated view.
func (s *syntheticGeneration) DiscardCandidate(ctx context.Context, caller transport.Caller, expected map[string]any, arg any, artifactIDArg any) (any, error) {
	handle, err := syntheticParseHandle(arg)
	if err != nil {
		return nil, err
	}
	artifactID := syntheticString(artifactIDArg)
	if artifactID == "" {
		return nil, errors.New("GENERATION_CANDIDATE_NOT_DISCARDABLE")
	}
	contextValue, scope, ledger, db, done, err := s.admit(ctx, caller, expected)
	if err != nil {
		return nil, err
	}
	defer done()
	run, _, _, err := syntheticRequireRun(ctx, ledger, scope, handle)
	if err != nil {
		return nil, err
	}
	service, err := s.newService(ctx, caller, contextValue, ledger, db, scope, handle)
	if err != nil {
		return nil, err
	}
	if _, err := service.DiscardCandidate(ctx, artifactID); err != nil {
		return nil, err
	}
	return s.viewOf(ctx, ledger, db, run)
}

// viewOf projects the run into the browser MainGenerationRunView. It derives
// candidates and diagnostics from per-attempt receipts and budget state, so
// fields match the original projection (artifacts filtered by epoch, discarded
// artifacts excluded from candidates, token liability per attempt).
func (s *syntheticGeneration) viewOf(ctx context.Context, ledger *store.GenerationLedger, db *sql.DB, runValue any) (map[string]any, error) {
	return s.viewFor(ctx, ledger, db, runValue, "")
}

// viewFor is viewOf for callers that hold the caller's current lease. An empty
// epoch means the run was reached through a path whose fence already proved
// binding.epoch equals that lease, so the original's epoch inequality folds away
// as before. A committed history read passes the caller's lease instead: the run
// it returns may be from an older epoch, and the original reports exactly that as
// `nonReplayable` rather than hiding it.
func (s *syntheticGeneration) viewFor(ctx context.Context, ledger *store.GenerationLedger, db *sql.DB, runValue any, epoch string) (map[string]any, error) {
	run := syntheticMap(runValue)
	if run == nil {
		return nil, errors.New("GENERATION_RUN_NOT_FOUND")
	}
	binding := syntheticMap(run["binding"])
	if binding == nil {
		return nil, errors.New("GENERATION_RUN_NOT_FOUND")
	}
	manifest := syntheticMap(binding["sourceManifest"])
	runID := syntheticString(run["runId"])
	rootID := syntheticString(run["rootActionId"])
	budgetValue, err := syntheticCall(ctx, ledger, "budget", rootID)
	if err != nil {
		return nil, err
	}
	budget := syntheticMap(budgetValue)
	policy := syntheticMap(budget["policy"])
	attemptValues := syntheticSlice(budget["attempts"])
	receipts := make([]map[string]any, 0, len(attemptValues))
	for _, attemptValue := range attemptValues {
		attempt := syntheticMap(attemptValue)
		receiptValue, err := syntheticCall(ctx, ledger, "receipt", syntheticString(attempt["attemptId"]))
		if err != nil {
			return nil, err
		}
		receipts = append(receipts, syntheticMap(receiptValue))
	}
	inFlightValue, err := syntheticCall(ctx, ledger, "hasInFlight", runID)
	if err != nil {
		return nil, err
	}
	blocked := budget["blockedCode"]
	candidates := []any{}
	artifacts := []any{}
	diagnostics := []any{}
	for _, receipt := range receipts {
		// Diagnostics track every attempt, discarded or not, exactly like the
		// original budgetDiagnostics projection.
		diagnostics = append(diagnostics, syntheticBudgetDiagnostic(receipt, budget))
		artifact := syntheticMap(receipt["artifact"])
		if artifact == nil {
			continue
		}
		// Candidates exclude discarded artifacts (the renderer's visibleIds
		// filter); the artifact epoch filter shapes the artifacts projection.
		discarded, err := s.syntheticArtifactDiscarded(ctx, db, syntheticString(artifact["artifactId"]))
		if err != nil {
			return nil, err
		}
		if discarded {
			continue
		}
		entry, ok := s.snapshotOf(ctx, db, receipt, binding)
		if !ok {
			continue
		}
		entry["fingerprint"] = artifact["fingerprint"]
		entry["nonReplayable"] = true
		candidates = append(candidates, entry)
		if entry["epoch"] == binding["epoch"] {
			artifacts = append(artifacts, entry)
		}
	}
	tokenLiability := int64(0)
	physical := int64(0)
	for _, attemptValue := range attemptValues {
		attempt := syntheticMap(attemptValue)
		status := syntheticString(attempt["status"])
		if status == "cancelled-before-dispatch" {
			continue
		}
		physical++
		if status == "settled" {
			value := attempt["actualTokens"]
			if value == nil {
				value = attempt["reservedTokens"]
			}
			tokenLiability += syntheticNumber(value)
		} else {
			tokenLiability += syntheticNumber(attempt["reservedTokens"])
		}
	}
	maxElapsed := syntheticNumber(policy["maxActiveElapsedMs"])
	elapsed := syntheticNumber(budget["activeElapsedMs"])
	remaining := maxElapsed - elapsed
	if remaining < 0 {
		remaining = 0
	}
	// The begin/resume/mutation paths already proved binding.epoch equals the
	// frozen lease, so there the original epoch inequality is folded away and
	// only status/budget/in-flight replayability gates remain. A history read
	// passes the caller's own lease and adds the epoch term back.
	nonReplayable := run["status"] != "running" || syntheticBool(blocked) || inFlightValue == true
	if epoch != "" && syntheticString(binding["epoch"]) != epoch {
		nonReplayable = true
	}
	handle := map[string]any{
		"projectId":    binding["projectId"],
		"epoch":        binding["epoch"],
		"rootActionId": rootID,
		"runId":        runID,
	}
	return map[string]any{
		"handle":             handle,
		"status":             run["status"],
		"operation":          manifest["operation"],
		"budgetDiagnostics":  diagnostics,
		"inFlight":           inFlightValue == true,
		"nonReplayable":      nonReplayable,
		"budget": map[string]any{
			"maxAttempts":                       policy["maxPhysicalRequests"],
			"maxRequestedOutputTokens":          policy["maxTokenLiability"],
			"maxRequestedOutputTokensPerAttempt": policy["maxOutputPerRequest"],
			"deadlineAt":                        time.Now().UnixMilli() + remaining,
		},
		"ledger": map[string]any{
			"policy":           policy,
			"tokenLiability":   tokenLiability,
			"physicalRequests": physical,
			"activeElapsedMs":  elapsed,
			"blockedCode":      blocked,
		},
		"artifacts": artifacts,
		"candidates": candidates,
	}, nil
}

// snapshotOf builds the MainGenerationSnapshot for one attempt receipt. A nil
// artifact yields no snapshot; discarded artifacts are excluded from
// candidates by the caller's artifact-status check.
func (s *syntheticGeneration) snapshotOf(ctx context.Context, db *sql.DB, receipt map[string]any, binding map[string]any) (map[string]any, bool) {
	if receipt == nil {
		return nil, false
	}
	artifact := syntheticMap(receipt["artifact"])
	if artifact == nil {
		return nil, false
	}
	attempt := syntheticMap(receipt["attempt"])
	run := syntheticMap(receipt["run"])
	if attempt == nil || run == nil {
		return nil, false
	}
	status := syntheticSnapshotStatus(receipt)
	compositionEligible := (attempt["status"] == "settled" || attempt["status"] == "unknown") && receipt["failureCode"] == nil &&
		syntheticMap(receipt["budget"]) != nil && syntheticMap(syntheticMap(receipt["budget"])["root"])["status"] != "cancelled"
	if compositionEligible {
		finish := ""
		if result := syntheticMap(receipt["result"]); result != nil {
			finish = syntheticString(result["finishReason"])
		}
		if finish != "stop" && finish != "length" {
			compositionEligible = false
		} else if strings.TrimSpace(syntheticString(artifact["text"])) == "" {
			compositionEligible = false
		} else {
			discarded, err := s.syntheticArtifactDiscarded(ctx, db, syntheticString(artifact["artifactId"]))
			if err != nil || discarded {
				compositionEligible = false
			}
		}
	}
	snapshot := map[string]any{
		"projectId":         binding["projectId"],
		"epoch":             artifact["epoch"],
		"rootActionId":      run["rootActionId"],
		"runId":             run["runId"],
		"artifactId":        artifact["artifactId"],
		"attemptId":         artifact["attemptId"],
		"revision":          artifact["revision"],
		"durableRevision":   artifact["revision"],
		"text":              artifact["text"],
		"textHash":          artifact["textHash"],
		"status":            status,
		"compositionEligible": compositionEligible,
	}
	if diagnostics := receipt["diagnostics"]; diagnostics != nil {
		snapshot["diagnostics"] = diagnostics
	}
	return snapshot, true
}

func syntheticSnapshotStatus(receipt map[string]any) string {
	attempt := syntheticMap(receipt["attempt"])
	terminal := attempt != nil && (attempt["status"] == "settled" || attempt["status"] == "unknown" || attempt["status"] == "cancelled-before-dispatch")
	if !terminal {
		return "running"
	}
	if receipt["failureCode"] != nil {
		return "failed"
	}
	finish := ""
	if result := syntheticMap(receipt["result"]); result != nil {
		finish = syntheticString(result["finishReason"])
	}
	if finish == "stop" {
		return "completed"
	}
	if attempt != nil && attempt["status"] == "cancelled-before-dispatch" {
		return "cancelled"
	}
	budget := syntheticMap(receipt["budget"])
	if budget != nil && syntheticMap(budget["root"]) != nil && syntheticMap(budget["root"])["status"] == "cancelled" {
		return "cancelled"
	}
	if finish != "" {
		return "failed"
	}
	return "unknown"
}

// syntheticBudgetDiagnostic mirrors the original budgetDiagnostics entry.
func syntheticBudgetDiagnostic(receipt map[string]any, budget map[string]any) map[string]any {
	attempt := syntheticMap(receipt["attempt"])
	result := syntheticMap(receipt["result"])
	usage := syntheticMap(result["usage"])
	actual := map[string]any(nil)
	if usage != nil && syntheticBool(usage["trusted"]) {
		actual = map[string]any{
			"input":      usage["inputTokens"],
			"completion": usage["completionTokens"],
			"reasoning":  usage["reasoningTokens"],
			"total":      usage["actualTokens"],
		}
	}
	actualState := "unknown"
	status := syntheticString(attempt["status"])
	switch status {
	case "cancelled-before-dispatch":
		actualState = "not-dispatched"
	case "settled":
		actualState = "settled"
	case "reserved", "dispatch-marked":
		actualState = "reserved"
	}
	entry := map[string]any{
		"attemptId":            syntheticString(attempt["attemptId"]),
		"runId":                syntheticString(syntheticMap(receipt["run"])["runId"]),
		"requestedOutputTokens": syntheticNumber(attempt["requestedOutputTokens"]),
		"reservedTokens":        syntheticNumber(attempt["reservedTokens"]),
		"actualState":           actualState,
		"actual":                actual,
		"finishReason":          nil,
		"failureCode":           nil,
		"reasons":               []any{},
	}
	if result != nil {
		if value := result["finishReason"]; value != nil {
			entry["finishReason"] = value
		}
	}
	if receipt["failureCode"] != nil {
		entry["failureCode"] = receipt["failureCode"]
	} else if budget != nil && budget["blockedCode"] != nil {
		entry["failureCode"] = budget["blockedCode"]
	}
	return entry
}

// syntheticArtifactDiscarded reports whether the artifact row is discarded.
func (s *syntheticGeneration) syntheticArtifactDiscarded(ctx context.Context, db *sql.DB, artifactID string) (bool, error) {
	if db == nil {
		return false, nil
	}
	var status string
	err := db.QueryRowContext(ctx, "SELECT status FROM generation_artifacts WHERE artifact_id=?", artifactID).Scan(&status)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, nil
	}
	return status == "discarded", nil
}

// publishSnapshot projects the ordinary service's progress event onto the
// browser generation:snapshot SSE channel under the exact caller tab. It is
// best-effort: a missing subscriber or transient read failure never fails the
// executing operation.
func (s *syntheticGeneration) publishSnapshot(caller transport.Caller, expected projectsession.Context, handle syntheticHandle, ledger *store.GenerationLedger, db *sql.DB, event generation.Snapshot) {
	if event.StorageFailed {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	receiptValue, err := syntheticCall(ctx, ledger, "receipt", event.AttemptID)
	if err != nil {
		return
	}
	receipt := syntheticMap(receiptValue)
	run := syntheticMap(receipt["run"])
	binding := syntheticMap(run["binding"])
	if binding == nil {
		return
	}
	snapshot, ok := s.snapshotOf(ctx, db, receipt, binding)
	if !ok {
		return
	}
	snapshot["text"] = event.VisibleText
	snapshot["textHash"] = syntheticHash(event.VisibleText)
	snapshot["revision"] = s.nextSnapshotSeq(event.AttemptID)
	snapshot["durableRevision"] = event.DurableRevision
	if event.Diagnostics != nil {
		snapshot["diagnostics"] = event.Diagnostics
	}
	snapshot["status"] = "running"
	_ = s.server.Publish(caller, "generation:snapshot", []any{snapshot})
}
