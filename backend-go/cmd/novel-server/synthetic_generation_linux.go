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
)

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
		currentValue, found, err := durable.lifecycle.CurrentProjectDurable(ctx, projectsession.Owner{SessionID: scope.Session, ClientID: scope.Tab})
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
		lifecycle: durable.lifecycle,
		server:    server,
		registry:  registry,
		executor:  executor,
		loadModel: loadModel,
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
func (s *syntheticGeneration) admit(ctx context.Context, caller transport.Caller, expected any) (projectsession.Context, generationowner.Scope, *store.GenerationLedger, *sql.DB, func(), error) {
	contextValue, err := projectsession.ParseContext(expected)
	if err != nil {
		return projectsession.Context{}, generationowner.Scope{}, nil, nil, nil, err
	}
	if caller.SessionID == "" || caller.ClientID == "" {
		return projectsession.Context{}, generationowner.Scope{}, nil, nil, nil, projectsession.ErrContext
	}
	ledger, db, refresh, done, err := s.lifecycle.BeginGenerationLedgerDB(ctx, projectsession.Owner{SessionID: caller.SessionID, ClientID: caller.ClientID}, contextValue)
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
	scope := generationowner.Scope{Caller: caller.SessionID, Session: caller.SessionID, Tab: caller.ClientID, Project: contextValue.ProjectID, Epoch: contextValue.LeaseID}
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
		if err != nil {
			return generationowner.SourceValue{}, errors.New("GENERATION_SOURCE_BLUEPRINT_READ_ERROR")
		}
		if row == nil {
			return generationowner.SourceValue{}, errors.New("GENERATION_SOURCE_BLUEPRINT_MISSING")
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
			return generationowner.SourceValue{}, errors.New("GENERATION_SOURCE_UNSUPPORTED_DRAFT_ID")
		}
		full, err := store.NewDrafts(db).GetFull(ctx, id)
		if err != nil {
			return generationowner.SourceValue{}, errors.New("GENERATION_SOURCE_DRAFT_READ_ERROR")
		}
		if full == nil {
			return generationowner.SourceValue{}, errors.New("GENERATION_SOURCE_DRAFT_MISSING")
		}
		return generationowner.SourceValue{Revision: full.Version, Text: full.Content}, nil
	}
	set.Sources.Prompt = func(ctx context.Context, scope generationowner.Scope, key generationowner.SourceKey) (generationowner.SourceValue, error) {
		parts := strings.SplitN(key.ID, ":", 2)
		if len(parts) != 2 || !syntheticPromptKeyPattern.MatchString(parts[0]) || (parts[1] != "zh-CN" && parts[1] != "en-US") {
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
	// PROBE (temporary): surface which selection key cannot be read so the
	// symbolic failure code names the source kind instead of a generic miss.
	for _, key := range set.Selection {
		reader, ok := syntheticSourceReader(set.Sources, key.Kind)
		if !ok {
			return nil, errors.New("GENERATION_SOURCE_KIND_UNSUPPORTED_" + syntheticKindCode(key.Kind))
		}
		if _, err := reader(ctx, generationowner.Scope{}, key); err != nil {
			return nil, err
		}
	}
	return set, nil
}

// syntheticSourceReader maps a selection kind onto the resolver that serves it.
func syntheticSourceReader(sources generationowner.Sources, kind string) (generationowner.SourceReader, bool) {
	switch kind {
	case "project-core":
		return sources.ProjectCore, sources.ProjectCore != nil
	case "blueprint":
		return sources.Blueprint, sources.Blueprint != nil
	case "draft":
		return sources.Draft, sources.Draft != nil
	case "prompt":
		return sources.Prompt, sources.Prompt != nil
	}
	return nil, false
}

// syntheticKindCode renders a source kind as an all-caps symbolic fragment.
func syntheticKindCode(kind string) string {
	return strings.ToUpper(strings.ReplaceAll(kind, "-", "_"))
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
		"authorInputs":               selection.AuthorInputs,
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
		"projectId":        scope.Project,
		"epoch":            scope.Epoch,
		"fingerprint":      fingerprint,
		"contextSnapshotId": syntheticHash(set.CoreText),
		"sourceManifest":   syntheticManifest(selection, scope, receipt.LeaseID, binding.Fingerprint()),
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
	return s.viewOf(ctx, ledger, db, runValue)
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
	taskRaw, err := json.Marshal(request["task"])
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
	receipt, err := service.Execute(ctx, frozen, handle.RunID, nonce)
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
	outcome := map[string]any{
		"status":       status,
		"content":      content,
		"finishReason": finish,
		"receipt":      receipt,
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
	var result map[string]any
	if cancelRoot {
		result, err = service.Cancel(ctx, syntheticString(run["rootActionId"]))
	} else {
		result, err = service.Pause(ctx, syntheticString(run["rootActionId"]))
	}
	if err != nil {
		return nil, err
	}
	return s.viewOf(ctx, ledger, db, result["run"])
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
	result, err := service.Resume(ctx, syntheticString(run["runId"]), nextRaw)
	if err != nil {
		return nil, err
	}
	return s.viewOf(ctx, ledger, db, result["run"])
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

// begin is the shared begin implementation used by Begin and Restart.
func (s *syntheticGeneration) begin(ctx context.Context, caller transport.Caller, expected map[string]any, scope generationowner.Scope, ledger *store.GenerationLedger, db *sql.DB, selection syntheticSelection) (any, error) {
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
	// The fence already proved binding.epoch equals the frozen lease, so the
	// original epoch inequality is folded away; only status/budget/in-flight
	// replayability gates remain.
	nonReplayable := run["status"] != "running" || syntheticBool(blocked) || inFlightValue == true
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
	snapshot["revision"] = event.DurableRevision
	snapshot["durableRevision"] = event.DurableRevision
	if event.Diagnostics != nil {
		snapshot["diagnostics"] = event.Diagnostics
	}
	snapshot["status"] = "running"
	_ = s.server.Publish(caller, "generation:snapshot", []any{snapshot})
}
