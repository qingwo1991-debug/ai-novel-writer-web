package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"io"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// GenerationLedger is an INTERNAL trusted storage primitive, not a generation
// admission service. It captures one already-admitted project DB; it never opens
// paths, performs DDL, switches projects, writes author data, or binds public RPC.
// The caller must reject portable projects at admission. WithFreezeGuard is an
// additional trusted fail-closed seam, not a replacement sidecar parser.
// Planning, blueprint, author-effect and composition workflows are unsupported.
// Read-only source Get/receipt/budget remain available for historical records.
type GenerationLedger struct {
	db     *sql.DB
	now    func() int64
	id     func() (string, error)
	frozen func(string, string) bool
	// onWrite runs after every committed ledger write. The lane that writes the
	// author database through this ledger's raw handle uses it to adopt the new
	// committed content identity immediately, so another tab's borrow never sees
	// the pre-write digest. Configure only before sharing the ledger.
	onWrite func()
}

func NewGenerationLedger(db *sql.DB, now func() int64, uuid func() string) *GenerationLedger {
	if now == nil {
		now = func() int64 { return time.Now().UnixMilli() }
	}
	id := secureUUID
	if uuid != nil {
		id = func() (string, error) { return uuid(), nil }
	}
	return &GenerationLedger{db: db, now: now, id: id}
}

// WithWriteHook installs a callback that runs after every committed write, so a
// caller that writes the database through this ledger's raw handle can adopt the
// new content identity before any other borrow observes it.
// Configure only before sharing the ledger with concurrent callers.
func (g *GenerationLedger) WithWriteHook(hook func()) *GenerationLedger {
	if g != nil {
		g.onWrite = hook
	}
	return g
}

// Configure only before sharing the ledger with concurrent callers.
func (g *GenerationLedger) WithFreezeGuard(guard func(table, recordID string) bool) *GenerationLedger {
	g.frozen = guard
	return g
}

func glText(v any) string { s, _ := v.(string); return s }

// Identity comparisons reject non-scalar JSON without Go interface panics.
func glScalarSame(a, b any) bool {
	switch a := a.(type) {
	case nil:
		return b == nil
	case string:
		v, ok := b.(string)
		return ok && a == v
	case bool:
		v, ok := b.(bool)
		return ok && a == v
	default:
		an, aok := plotNumber(a)
		bn, bok := plotNumber(b)
		return aok && bok && an == bn
	}
}
func glMap(v any) map[string]any { m, _ := glPlain(v).(map[string]any); return m }
func glPlain(v any) any {
	switch v := v.(type) {
	case plotObject:
		m := make(map[string]any, len(v))
		for _, f := range v {
			m[f.key] = glPlain(f.value)
		}
		return m
	case map[string]any:
		m := make(map[string]any, len(v))
		for k, x := range v {
			m[k] = glPlain(x)
		}
		return m
	case []any:
		a := make([]any, len(v))
		for i, x := range v {
			a[i] = glPlain(x)
		}
		return a
	}
	return v
}
func glField(v any, key string) any {
	if object, ok := v.(plotObject); ok {
		for _, f := range object {
			if f.key == key {
				return f.value
			}
		}
		return nil
	}
	if m, ok := v.(map[string]any); ok {
		return m[key]
	}
	return nil
}

// JSON.parse/stringify order is observable in nonce conflicts. Keep ordered
// objects (including nested opaque fields), duplicate-key replacement and JS
// array-index enumeration. Raw JSON is validated before the shared decoder to
// reject invalid UTF-8 and unpaired UTF-16 instead of replacing hash input.
func glParse(text string) (any, error) {
	if err := summaryJSONSupported(text); err != nil {
		return nil, glError("GENERATION_JSON_INVALID")
	}
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.UseNumber()
	value, err := rosterJSONValue(decoder)
	if err != nil {
		return nil, glError("GENERATION_JSON_INVALID")
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, glError("GENERATION_JSON_INVALID")
	}
	return value, nil
}
func glNormalize(v any) (any, error) {
	switch v := v.(type) {
	case json.RawMessage:
		return glParse(string(v))
	case map[string]any:
		m := make(map[string]any, len(v))
		for k, x := range v {
			n, err := glNormalize(x)
			if err != nil {
				return nil, err
			}
			m[k] = n
		}
		return m, nil
	case []any:
		a := make([]any, len(v))
		for i, x := range v {
			n, err := glNormalize(x)
			if err != nil {
				return nil, err
			}
			a[i] = n
		}
		return a, nil
	}
	return v, nil
}
func glStored(v any) (map[string]any, error) {
	text, ok := v.(string)
	if !ok {
		return nil, glError("GENERATION_JSON_INVALID")
	}
	value, err := glParse(text)
	if err != nil {
		return nil, err
	}
	m := glMap(value)
	if m == nil {
		return nil, glError("GENERATION_JSON_INVALID")
	}
	return m, nil
}
func glPatch(text any, patch map[string]any, remove ...string) (string, error) {
	value, err := glParse(glText(text))
	if err != nil {
		return "", err
	}
	object, ok := value.(plotObject)
	if !ok {
		return "", glError("GENERATION_JSON_INVALID")
	}
	drop := map[string]bool{}
	for _, k := range remove {
		drop[k] = true
	}
	out := plotObject{}
	used := map[string]bool{}
	for _, f := range object {
		if drop[f.key] {
			continue
		}
		if v, ok := patch[f.key]; ok {
			f.value = v
			used[f.key] = true
		}
		out = append(out, f)
	}
	// Call sites append at most one new field; this is not an order-preserving
	// arbitrary map merge. Refuse an ambiguous new-field order explicitly.
	var added string
	for k := range patch {
		if !used[k] && !drop[k] {
			if added != "" {
				return "", glError("GENERATION_JSON_ORDER_REQUIRED")
			}
			added = k
		}
	}
	if added != "" {
		out = append(out, plotField{added, patch[added]})
	}
	return plotJSON(out)
}
func glOrderSensitive(value any) error {
	if m, ok := value.(map[string]any); ok && len(m) > 1 {
		return glError("GENERATION_JSON_ORDER_REQUIRED")
	}
	return nil
}
func glSame(a, b any) bool {
	// isDeepStrictEqual ignores object insertion order, but not array order.
	aj, ae := plotJSON(glPlain(a))
	bj, be := plotJSON(glPlain(b))
	return ae == nil && be == nil && aj == bj
}
func glOne(ctx context.Context, conn *sql.Conn, query, missing string, args ...any) (map[string]any, error) {
	rows, err := rrMaps(ctx, conn, query, args...)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		if missing != "" {
			return nil, glError(missing)
		}
		return nil, nil
	}
	return rows[0], nil
}
func glAttemptMaps(value any) []map[string]any {
	if rows, ok := value.([]map[string]any); ok {
		return rows
	}
	return nil
}

// CallJSON is the lossless JSON entry for oracle/internal adapters. Call accepts
// maps for ordinary arguments, but open/replace/resume require RawMessage for
// fingerprint and budget (or the complete request). Never silently sort those
// two order-sensitive fields and pretend to match JSON.stringify replay checks.
func (g *GenerationLedger) CallJSON(ctx context.Context, method string, args json.RawMessage) (any, error) {
	value, err := glParse(string(args))
	if err != nil {
		return nil, err
	}
	list, ok := value.([]any)
	if !ok {
		return nil, glError("GENERATION_INPUT_INVALID")
	}
	return g.Call(ctx, method, list)
}
func (g *GenerationLedger) Call(ctx context.Context, method string, args []any) (any, error) {
	if g == nil || g.db == nil {
		return nil, glError("GENERATION_DATABASE_NOT_READY")
	}
	// Reject cycles before recursive normalization of trusted Go map input.
	if _, err := json.Marshal(args); err != nil {
		return nil, glError("GENERATION_INPUT_INVALID")
	}
	normalized, err := glNormalize(args)
	if err != nil {
		return nil, err
	}
	args = normalized.([]any)
	// Unknown workflows are errors, never successful no-ops.
	read := method == "get" || method == "budget" || method == "attemptCount" || method == "hasInFlight" || method == "hasNonCancelledAttempt" || method == "findInvocation" || method == "invocationState" || method == "receipt"
	write := method == "open" || method == "reserve" || method == "markDispatched" || method == "snapshot" || method == "settle" || method == "block" || method == "pause" || method == "resume" || method == "replaceUnstartedBinding" || method == "recoverInterrupted" || method == "discardCandidate" || method == "recordEffect" || method == "recordAgentReceipt" || method == "activateRoot"
	if !read && !write {
		return nil, glError("GENERATION_OPERATION_UNSUPPORTED")
	}
	var result any
	if write {
		err = NewReviewRevisions(g.db).immediate(ctx, func(conn *sql.Conn) error { var e error; result, e = g.call(ctx, conn, method, args); return e })
		if err == nil && g.onWrite != nil {
			g.onWrite()
		}
	} else {
		conn, e := g.db.Conn(ctx)
		if e != nil {
			return nil, e
		}
		defer conn.Close()
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
			if _, err := conn.ExecContext(cleanup, "ROLLBACK"); err != nil {
				_ = conn.Raw(func(any) error { return driver.ErrBadConn })
			}
		}()
		result, err = g.call(ctx, conn, method, args)
		if err == nil {
			_, err = conn.ExecContext(ctx, "COMMIT")
			committed = err == nil
		}
	}
	if err != nil {
		return nil, err
	}
	return result, nil
}

// ListRuns returns identity+status summaries for every generation run recorded
// in this captured project database, newest first. It is strictly read-only and
// never normalizes or rewrites stored bindings. Runs whose binding cannot be
// parsed or whose projectId differs from the requested project are skipped,
// exactly like the original run-summary projection, so a shared database can
// never leak another project's runs.
func (g *GenerationLedger) ListRuns(ctx context.Context, projectID string) ([]map[string]any, error) {
	if g == nil || g.db == nil {
		return nil, glError("GENERATION_DATABASE_NOT_READY")
	}
	conn, err := g.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
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
		if _, err := conn.ExecContext(cleanup, "ROLLBACK"); err != nil {
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
	}()
	rows, err := rrMaps(ctx, conn, "SELECT run_id,root_action_id,binding_json,status FROM generation_runs ORDER BY rowid DESC")
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		binding, err := glStored(row["binding_json"])
		if err != nil || glText(binding["projectId"]) != projectID {
			continue
		}
		summary := map[string]any{
			"projectId":    binding["projectId"],
			"epoch":        binding["epoch"],
			"rootActionId": row["root_action_id"],
			"runId":        row["run_id"],
			"status":       row["status"],
		}
		if operation := glText(glMap(binding["sourceManifest"])["operation"]); operation != "" {
			summary["operation"] = operation
		}
		out = append(out, summary)
	}
	// Commit before releasing the connection: a read transaction left open on a
	// pooled connection makes the next statement fail with "cannot start a
	// transaction within a transaction".
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, err
	}
	committed = true
	return out, nil
}

func (g *GenerationLedger) get(ctx context.Context, conn *sql.Conn, runID string) (map[string]any, error) {
	row, err := glOne(ctx, conn, "SELECT * FROM generation_runs WHERE run_id=?", "GENERATION_RUN_MISSING", runID)
	if err != nil {
		return nil, err
	}
	binding, err := glStored(row["binding_json"])
	if err != nil {
		return nil, err
	}
	delete(binding, "$runEffects")
	delete(binding, "$blueprintRetirement")
	return map[string]any{"runId": row["run_id"], "rootActionId": row["root_action_id"], "binding": binding, "status": row["status"]}, nil
}
func (g *GenerationLedger) budget(ctx context.Context, conn *sql.Conn, rootID string) (map[string]any, error) {
	row, err := glOne(ctx, conn, "SELECT * FROM generation_roots WHERE root_action_id=?", "GENERATION_ROOT_MISSING", rootID)
	if err != nil {
		return nil, err
	}
	root, err := glStored(row["action_json"])
	if err != nil {
		return nil, err
	}
	policy, err := glStored(row["budget_json"])
	if err != nil {
		return nil, err
	}
	rows, err := rrMaps(ctx, conn, "SELECT attempt_json FROM generation_attempts WHERE root_action_id=? ORDER BY rowid", rootID)
	if err != nil {
		return nil, err
	}
	attempts := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		a, err := glStored(row["attempt_json"])
		if err != nil {
			return nil, err
		}
		attempts = append(attempts, a)
	}
	elapsed, ok := rrInteger(row["active_elapsed_ms"], 0)
	if !ok {
		return nil, glError("INVALID_LEDGER")
	}
	if row["active_since_ms"] != nil {
		since, ok := rrInteger(row["active_since_ms"], -9007199254740991)
		if !ok {
			return nil, glError("INVALID_LEDGER")
		}
		if delta := g.now() - since; delta > 0 {
			elapsed += delta
		}
	}
	return map[string]any{"root": root, "policy": policy, "activeElapsedMs": elapsed, "attempts": attempts, "blockedCode": row["blocked_code"]}, nil
}

// glFamilyManifestAllowed is the narrow widening of the closed manifest. Each
// family key stays refused for every other operation, so a run can never smuggle
// an agent session, graph context or finalization slot into an ordinary
// generation manifest: the key is only readable when the operation that owns it
// is the one the frozen binding actually names.
func glFamilyManifestAllowed(key, operation string) bool {
	switch key {
	case "agentSession":
		return operation == "agent-round"
	case "agentContext", "agentContextHash":
		// The frozen agent context is the message list an agent round sends, and
		// its digest is what every recovery re-proves before answering one. Both
		// are readable only for the operation whose begin composed them, so an
		// ordinary generation run cannot present a context it never froze.
		return operation == "agent-round"
	case "graphGenerationContext":
		switch operation {
		case "plot-tree-snapshot", "narrative-thread-plan-candidate",
			"narrative-thread-event-candidate", "finalized-chapter-notes":
			return true
		}
	case "finalizationGenerationSlotKey",
		"finalizationGenerationSlot", "finalizationGenerationContext",
		"finalizationGenerationContextHash", "finalizationGenerationOriginEpoch",
		"finalizationGenerationTask", "finalizationGenerationTaskHash":
		// The slot key is what the finalization read matches a run on, and the
		// context, its hash, the origin epoch and the task are what that read
		// re-proves before it hands a run back as the author's slot. Both steps
		// freeze all of it (finalization-generation-source.ts:588-592), so both
		// operations may carry it - and no other operation may, because a run of
		// another family holding this key would be handed back as a slot's
		// recovery and would block the real begin behind SLOT_CONFLICT.
		return operation == "finalized-character-state" || operation == "finalized-chapter-notes"
	case "graphGenerationInput", "graphGenerationKey", "graphGenerationContextHash",
		"graphGenerationOriginEpoch", "graphGenerationTask", "graphGenerationTaskHash":
		switch operation {
		case "plot-tree-snapshot", "narrative-thread-plan-candidate",
			"narrative-thread-event-candidate":
			return true
		}
	case "legacyRosterContext":
		// The legacy roster context is the frozen source proof a legacy-roster run
		// re-reads before answering a recovery. It is readable only for the one
		// operation that freezes it, so an ordinary generation run cannot smuggle
		// a roster context into a binding it does not own.
		return operation == "legacy-character-roster-repair"
	case "importSlot", "importSlotKey", "importContext":
		// The import slot, its key and the frozen import context are what the
		// import-generation read matches a run on and hands back as the author's
		// recovery (electron/services/import-generation.ts:14-23,
		// electron/services/generation-source-binding.ts:599); they are also the
		// members the import-run receipt path already reads out of a frozen
		// binding (internal/store/importrun_receipts.go:954-960). They are
		// readable only for the two stage operations this lane can freeze. The
		// blueprint stage is deliberately absent: its operation name contains
		// "blueprint", so glPlanning refuses it as GENERATION_OPERATION_UNSUPPORTED
		// with or without these keys, and naming it here would only move that
		// refusal one check later. Every other operation - chapter-draft,
		// agent-round, a blueprint directory name - keeps the refusal it answered
		// before this case.
		return operation == "import-global-facts" || operation == "analyze-writing-style"
	}
	return false
}

// glEffectKeyAllowed is the narrow widening of the receipt key allowlist. An
// effect key is legal only for the operation whose domain write produced it, so
// an ordinary generation attempt still cannot carry a graph or finalization
// effect even though the keys are now spellable.
func glEffectKeyAllowed(key, operation string) bool {
	switch key {
	case "graphEffects":
		switch operation {
		case "plot-tree-snapshot", "narrative-thread-plan-candidate",
			"narrative-thread-event-candidate", "finalized-chapter-notes":
			return true
		}
	case "finalizationEffect":
		// The reference stamps this effect for BOTH steps
		// (electron/services/finalization-generation.ts:113-114); allowing only
		// the character-state step would make a notes commit unstampable, and
		// allowing it elsewhere would let an ordinary attempt present an effect
		// with no domain write behind it.
		return operation == "finalized-character-state" || operation == "finalized-chapter-notes"
	case "reviewRevisionEffect":
		// The reference stamps this on the attempt that produced the review or the
		// revision (electron/services/review-revision-generation.ts:93-97). It is
		// keyed by the operation the run froze, so an ordinary generation attempt
		// cannot present an effect whose domain write never happened.
		return operation == "review-chapter" || operation == "refine-draft" || operation == "refine-from-review"
	case "visibleComposition":
		// The visible composition receipt belongs to the attempt that composed it
		// from that run's own artifacts, so any non-empty operation may carry it.
		return operation != ""
	case "legacyRosterProposal":
		// The legacy roster stage stamps this on the attempt whose settled artifact
		// it staged as one character_identity_proposals row
		// (electron/services/legacy-roster-generation.ts:64-65). It is legal only on
		// the run whose operation froze the roster context, so an ordinary attempt
		// cannot present a proposal effect with no domain write behind it.
		return operation == "legacy-character-roster-repair"
	}
	return false
}

func glPlanning(operation string) bool {
	// generate-plot-outline is the one planning-family name the reduced lane
	// serves: the workflow drives its chapter batches, checkpoint and commit
	// itself, and the run freezes no planning scope, effect keys or continuation
	// receipts, so it rides the ordinary visible-text path. Every other
	// planning/blueprint name keeps the refusal.
	if operation == "generate-plot-outline" {
		return false
	}
	return strings.Contains(operation, "planning") || strings.Contains(operation, "blueprint") || strings.Contains(operation, "plot-outline")
}
func glOrdinaryBinding(binding map[string]any) error {
	if binding == nil {
		return glError("GENERATION_BINDING_INVALID")
	}
	for _, k := range []string{"$runEffects", "$blueprintRetirement"} {
		if _, exists := binding[k]; exists {
			return glError("GENERATION_EFFECT_UNSUPPORTED")
		}
	}
	manifest := glMap(binding["sourceManifest"])
	// importSlotKey and importContext join the closed list with importSlot: the
	// three are the import family's own binding, so a manifest that carries any of
	// them is refused unless the operation that froze it is the import one.
	for _, key := range []string{"agentSession", "agentContext", "agentContextHash",
		"graphGenerationContext", "importSlot", "importSlotKey", "importContext", "legacyRosterContext",
		"finalizationGenerationSlotKey", "finalizationGenerationSlot", "finalizationGenerationContext",
		"finalizationGenerationContextHash", "finalizationGenerationOriginEpoch", "finalizationGenerationTask",
		"finalizationGenerationTaskHash", "graphGenerationInput", "graphGenerationKey",
		"graphGenerationContextHash", "graphGenerationOriginEpoch", "graphGenerationTask",
		"graphGenerationTaskHash"} {
		if _, ok := manifest[key]; !ok {
			continue
		}
		if !glFamilyManifestAllowed(key, glText(manifest["operation"])) {
			return glError("GENERATION_OPERATION_UNSUPPORTED")
		}
	}
	if glPlanning(glText(manifest["operation"])) {
		return glError("GENERATION_OPERATION_UNSUPPORTED")
	}
	policy := glMap(manifest["policy"])
	if _, ok := policy["planning"]; ok {
		return glError("GENERATION_OPERATION_UNSUPPORTED")
	}
	return nil
}

// Mutating ANY part of a root is refused when another run under it carries
// planning/effect authority. Reads do not call this guard. All queries use the
// transaction's reserved connection, including freeze checks and callbacks.
func (g *GenerationLedger) mutableRoot(ctx context.Context, conn *sql.Conn, rootID string) error {
	row, err := glOne(ctx, conn, "SELECT action_json FROM generation_roots WHERE root_action_id=?", "", rootID)
	if err != nil || row == nil {
		return err
	}
	action, err := glStored(row["action_json"])
	if err != nil {
		return err
	}
	if glPlanning(glText(action["operation"])) {
		return glError("GENERATION_OPERATION_UNSUPPORTED")
	}
	if g.frozen != nil && g.frozen("generation_roots", rootID) {
		return glError("PORTABLE_RUNTIME_FROZEN")
	}
	runs, err := rrMaps(ctx, conn, "SELECT run_id,binding_json FROM generation_runs WHERE root_action_id=? ORDER BY rowid", rootID)
	if err != nil {
		return err
	}
	effectKeys := map[string]bool{}
	agentEffects := false
	for _, run := range runs {
		if g.frozen != nil && g.frozen("generation_runs", glText(run["run_id"])) {
			return glError("PORTABLE_RUNTIME_FROZEN")
		}
		binding, err := glStored(run["binding_json"])
		if err != nil {
			return err
		}
		if err := glOrdinaryBinding(binding); err != nil {
			return err
		}
		operation := glText(glMap(binding["sourceManifest"])["operation"])
		if operation == "agent-round" {
			agentEffects = true
		}
		for _, key := range []string{"graphEffects", "finalizationEffect", "reviewRevisionEffect", "legacyRosterProposal"} {
			if glEffectKeyAllowed(key, operation) {
				effectKeys[key] = true
			}
		}
	}
	rows, err := rrMaps(ctx, conn, "SELECT attempt_id,usage_receipt_json FROM generation_attempts WHERE root_action_id=? ORDER BY rowid", rootID)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if g.frozen != nil && g.frozen("generation_attempts", glText(row["attempt_id"])) {
			return glError("PORTABLE_RUNTIME_FROZEN")
		}
		usage, err := glStored(row["usage_receipt_json"])
		if err != nil {
			return err
		}
		for key := range usage {
			switch key {
			// draftCommit and directoryProgress are the two receipts a batch
			// chapter writes after its draft lands: the first records the draft
			// row the commit created, the second the directory counter it moved.
			// Both are projections of rows that exist elsewhere in this database,
			// so admitting the keys here does not let an attempt claim an effect
			// the store did not perform.
			case "requestHash", "usagePolicy", "purpose", "replayTask", "budgetDecision", "reconciliationInjected", "artifactIdentity", "result", "draftCommit", "directoryProgress", "visibleComposition":
			case "agentActions", "agentAuthorSelections", "agentSourceTransition", "agentControlEpoch":
				// The agent action map and control epoch are legal only on a run
				// whose operation is agent-round; an ordinary attempt cannot
				// carry them (generation_ledger_agent.go writes them gated).
				if !agentEffects {
					return glError("GENERATION_EFFECT_UNSUPPORTED")
				}
			case "graphEffects", "finalizationEffect", "reviewRevisionEffect", "legacyRosterProposal":
				if !effectKeys[key] {
					return glError("GENERATION_EFFECT_UNSUPPORTED")
				}
			default:
				return glError("GENERATION_EFFECT_UNSUPPORTED")
			}
		}
		if glPlanning(glText(usage["purpose"])) {
			return glError("GENERATION_OPERATION_UNSUPPORTED")
		}
	}
	return nil
}
func (g *GenerationLedger) mutableRun(ctx context.Context, conn *sql.Conn, runID string) error {
	run, err := g.get(ctx, conn, runID)
	if err != nil {
		return err
	}
	return g.mutableRoot(ctx, conn, glText(run["rootActionId"]))
}
func (g *GenerationLedger) inFlight(ctx context.Context, conn *sql.Conn, runID string) (bool, error) {
	row, err := glOne(ctx, conn, `SELECT 1 FROM generation_attempts WHERE run_id=? AND json_extract(attempt_json,'$.status') IN ('reserved','dispatch-marked') LIMIT 1`, "", runID)
	return row != nil, err
}
func (g *GenerationLedger) receipt(ctx context.Context, conn *sql.Conn, attemptID string, supplied map[string]any) (map[string]any, error) {
	row, err := glOne(ctx, conn, `SELECT t.attempt_id,t.run_id,t.attempt_json,t.usage_receipt_json,
 a.artifact_id,a.artifact_json,a.revision FROM generation_attempts t
 LEFT JOIN generation_artifacts a ON a.attempt_id=t.attempt_id WHERE t.attempt_id=?`, "GENERATION_ATTEMPT_MISSING", attemptID)
	if err != nil {
		return nil, err
	}
	run, err := g.get(ctx, conn, glText(row["run_id"]))
	if err != nil {
		return nil, err
	}
	usage, err := glStored(row["usage_receipt_json"])
	if err != nil {
		return nil, err
	}
	attempt, err := glStored(row["attempt_json"])
	if err != nil {
		return nil, err
	}
	var artifact any
	if row["artifact_json"] != nil {
		visible, err := glStored(row["artifact_json"])
		if err != nil {
			return nil, err
		}
		identity := glMap(usage["artifactIdentity"])
		binding := glMap(run["binding"])
		text, textOK := visible["text"].(string)
		if !glScalarSame(visible["attemptId"], row["attempt_id"]) || !glScalarSame(visible["rootActionId"], run["rootActionId"]) || identity == nil || !glScalarSame(visible["artifactId"], row["artifact_id"]) || !glScalarSame(visible["artifactId"], identity["artifactId"]) || !glScalarSame(visible["epoch"], identity["epoch"]) || !glSame(visible["fingerprint"], identity["fingerprint"]) || !glSame(visible["fingerprint"], binding["fingerprint"]) || !glScalarSame(visible["projectId"], binding["projectId"]) || !rrEqual(visible["revision"], row["revision"]) || !textOK || recoveryHash(text) != visible["textHash"] {
			return nil, glError("ARTIFACT_INTEGRITY_FAILED")
		}
		artifact = visible
	}
	budget := supplied
	if glMap(budget["root"])["rootActionId"] != run["rootActionId"] {
		budget, err = g.budget(ctx, conn, glText(run["rootActionId"]))
		if err != nil {
			return nil, err
		}
	}
	result := usage["result"]
	out := map[string]any{"run": run, "attempt": attempt, "artifact": artifact, "budget": budget, "result": result}
	// The renderer receipt labels each attempt with the semantic task purpose;
	// the ledger stores it on the usage receipt, so surface it beside result.
	if phTruthy(usage["purpose"]) {
		out["purpose"] = usage["purpose"]
	}
	storedResult := glMap(result)
	for _, key := range []string{"diagnostics", "failureCode"} {
		if phTruthy(storedResult[key]) {
			out[key] = storedResult[key]
		}
	}
	if phTruthy(usage["budgetDecision"]) {
		out["budgetDecision"] = usage["budgetDecision"]
	}
	return out, nil
}
func (g *GenerationLedger) findInvocation(ctx context.Context, conn *sql.Conn, runID, nonce, hash string) (map[string]any, error) {
	row, err := glOne(ctx, conn, "SELECT attempt_id,usage_receipt_json FROM generation_attempts WHERE run_id=? AND invocation_nonce=?", "", runID, nonce)
	if err != nil || row == nil {
		return nil, err
	}
	usage, err := glStored(row["usage_receipt_json"])
	if err != nil {
		return nil, err
	}
	if usage["requestHash"] != hash {
		return nil, glError("GENERATION_INVOCATION_CONFLICT")
	}
	return g.receipt(ctx, conn, glText(row["attempt_id"]), nil)
}

func (g *GenerationLedger) open(ctx context.Context, conn *sql.Conn, raw any) (map[string]any, error) {
	request := glMap(raw)
	if request == nil {
		return nil, glError("GENERATION_BINDING_INVALID")
	}
	frozen, err := GLBinding(request)
	if err != nil {
		return nil, err
	}
	if err := glOrdinaryBinding(request); err != nil {
		return nil, err
	}
	if glPlanning(glText(request["operation"])) {
		return nil, glError("GENERATION_OPERATION_UNSUPPORTED")
	}
	for _, key := range []string{"fingerprint", "budget"} {
		if err := glOrderSensitive(glField(raw, key)); err != nil {
			return nil, err
		}
	}
	rootID, err := g.id()
	if err != nil {
		return nil, err
	}
	action := map[string]any{"projectId": request["projectId"], "epoch": request["epoch"], "operation": request["operation"], "uiActionNonce": request["uiActionNonce"], "frozenInputHash": request["frozenInputHash"], "rootActionId": rootID, "status": "active"}
	key, err := GLRootKey(action)
	if err != nil {
		return nil, err
	}
	parent := request["parentRootActionId"]
	if parent != nil {
		if _, ok := parent.(string); !ok {
			return nil, glError("GENERATION_PARENT_INVALID")
		}
	}
	openKey, err := plotJSON([]any{key, parent})
	if err != nil {
		return nil, err
	}
	existing, err := glOne(ctx, conn, "SELECT run_id FROM generation_runs WHERE open_key=?", "", openKey)
	if err != nil {
		return nil, err
	}
	budgetJSON, err := plotJSON(glField(raw, "budget"))
	if err != nil {
		return nil, err
	}
	fingerprintJSON, err := plotJSON(glField(raw, "fingerprint"))
	if err != nil {
		return nil, err
	}
	if existing != nil {
		runID := glText(existing["run_id"])
		if err := g.mutableRun(ctx, conn, runID); err != nil {
			return nil, err
		}
		prior, err := g.get(ctx, conn, runID)
		if err != nil {
			return nil, err
		}
		stored, err := glOne(ctx, conn, "SELECT r.binding_json,root.budget_json FROM generation_runs r JOIN generation_roots root ON root.root_action_id=r.root_action_id WHERE r.run_id=?", "GENERATION_RUN_MISSING", runID)
		if err != nil {
			return nil, err
		}
		ordered, err := glParse(glText(stored["binding_json"]))
		if err != nil {
			return nil, err
		}
		priorFingerprint, err := plotJSON(glField(ordered, "fingerprint"))
		if err != nil {
			return nil, err
		}
		priorBudget, err := glParse(glText(stored["budget_json"]))
		if err != nil {
			return nil, err
		}
		priorBudgetJSON, err := plotJSON(priorBudget)
		if err != nil {
			return nil, err
		}
		priorBinding := glMap(prior["binding"])
		withoutEpoch := func(v any) any {
			values, ok := plotArray(v)
			if !ok {
				return nil
			}
			out := make([]any, len(values))
			for i, value := range values {
				m := glMap(value)
				delete(m, "epoch")
				out[i] = m
			}
			return out
		}
		if !glSame(priorBinding["sourceManifest"], frozen["sourceManifest"]) || !glSame(withoutEpoch(priorBinding["sourceRefs"]), withoutEpoch(frozen["sourceRefs"])) || priorFingerprint != fingerprintJSON || !glScalarSame(priorBinding["contextSnapshotId"], frozen["contextSnapshotId"]) || priorBudgetJSON != budgetJSON {
			return nil, glError("GENERATION_NONCE_CONFLICT")
		}
		return prior, nil
	}
	if phTruthy(parent) {
		if err := g.mutableRoot(ctx, conn, glText(parent)); err != nil {
			return nil, err
		}
		budget, err := g.budget(ctx, conn, glText(parent))
		if err != nil {
			return nil, err
		}
		action = glMap(budget["root"])
		if !glScalarSame(action["projectId"], request["projectId"]) || !glScalarSame(action["epoch"], request["epoch"]) || action["status"] != "active" || phTruthy(budget["blockedCode"]) {
			return nil, glError("GENERATION_PARENT_INVALID")
		}
	} else {
		policy := glMap(request["budget"])
		for _, k := range []string{"maxPhysicalRequests", "maxTokenLiability", "maxOutputPerRequest", "maxActiveElapsedMs"} {
			if _, ok := rrInteger(policy[k], 1); !ok {
				return nil, glError("INVALID_BUDGET")
			}
		}
		actionJSON, err := plotJSON(plotOrdered(action, "projectId", "epoch", "operation", "uiActionNonce", "frozenInputHash", "rootActionId", "status"))
		if err != nil {
			return nil, err
		}
		if _, err := conn.ExecContext(ctx, "INSERT INTO generation_roots(root_action_id,idempotency_key,action_json,budget_json) VALUES(?,?,?,?)", action["rootActionId"], key, actionJSON, budgetJSON); err != nil {
			return nil, err
		}
	}
	runID, err := g.id()
	if err != nil {
		return nil, err
	}
	bindingJSON, err := plotJSON(plotObject{{"projectId", frozen["projectId"]}, {"epoch", frozen["epoch"]}, {"fingerprint", glField(raw, "fingerprint")}, {"contextSnapshotId", frozen["contextSnapshotId"]}, {"sourceManifest", glField(raw, "sourceManifest")}, {"sourceRefs", glField(raw, "sourceRefs")}})
	if err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, "INSERT INTO generation_runs VALUES(?,?,?,?,?,?)", runID, action["rootActionId"], bindingJSON, "running", g.now(), openKey); err != nil {
		return nil, err
	}
	return g.get(ctx, conn, runID)
}

func (g *GenerationLedger) stopClock(ctx context.Context, conn *sql.Conn, rootID string) error {
	budget, err := g.budget(ctx, conn, rootID)
	if err != nil {
		return err
	}
	for _, a := range glAttemptMaps(budget["attempts"]) {
		if a["status"] == "dispatch-marked" {
			return nil
		}
	}
	// Original takes a second budget reading only after it finds the root idle.
	budget, err = g.budget(ctx, conn, rootID)
	if err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, "UPDATE generation_roots SET active_elapsed_ms=?,active_since_ms=NULL WHERE root_action_id=?", budget["activeElapsedMs"], rootID)
	return err
}
func (g *GenerationLedger) pause(ctx context.Context, conn *sql.Conn, rootID string, cancel bool) error {
	if err := g.mutableRoot(ctx, conn, rootID); err != nil {
		return err
	}
	budget, err := g.budget(ctx, conn, rootID)
	if err != nil {
		return err
	}
	root := glMap(budget["root"])
	if root["status"] == "cancelled" || root["status"] == "sealed" {
		return nil
	}
	rootStatus, runStatus := "paused", "paused"
	if cancel {
		rootStatus, runStatus = "cancelled", "cancelled"
	}
	row, err := glOne(ctx, conn, "SELECT action_json FROM generation_roots WHERE root_action_id=?", "GENERATION_ROOT_MISSING", rootID)
	if err != nil {
		return err
	}
	encoded, err := glPatch(row["action_json"], map[string]any{"status": rootStatus})
	if err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "UPDATE generation_roots SET action_json=?,active_elapsed_ms=?,active_since_ms=NULL WHERE root_action_id=?", encoded, budget["activeElapsedMs"], rootID); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "UPDATE generation_runs SET status=? WHERE root_action_id=?", runStatus, rootID); err != nil {
		return err
	}
	for _, attempt := range glAttemptMaps(budget["attempts"]) {
		if attempt["status"] == "reserved" {
			if err := glUpdateAttempt(ctx, conn, glText(attempt["attemptId"]), map[string]any{"status": "cancelled-before-dispatch"}); err != nil {
				return err
			}
		}
	}
	return nil
}
func glUpdateAttempt(ctx context.Context, conn *sql.Conn, attemptID string, patch map[string]any) error {
	row, err := glOne(ctx, conn, "SELECT attempt_json FROM generation_attempts WHERE attempt_id=?", "GENERATION_ATTEMPT_MISSING", attemptID)
	if err != nil {
		return err
	}
	encoded, err := glPatch(row["attempt_json"], patch)
	if err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, "UPDATE generation_attempts SET attempt_json=? WHERE attempt_id=?", encoded, attemptID)
	return err
}

var glThink = regexp.MustCompile(`(?i)</?think\b`)

func (g *GenerationLedger) call(ctx context.Context, conn *sql.Conn, method string, args []any) (any, error) {
	arg := func(i int) any {
		if i < len(args) {
			return args[i]
		}
		return nil
	}
	text := func(i int) string { return glText(arg(i)) }
	switch method {
	case "get":
		return g.get(ctx, conn, text(0))
	case "budget":
		return g.budget(ctx, conn, text(0))
	case "open":
		return g.open(ctx, conn, arg(0))
	case "attemptCount":
		row, err := glOne(ctx, conn, "SELECT COUNT(*) AS n FROM generation_attempts WHERE run_id=?", "", text(0))
		if err != nil {
			return nil, err
		}
		return row["n"], nil
	case "hasInFlight":
		return g.inFlight(ctx, conn, text(0))
	case "hasNonCancelledAttempt":
		rows, err := rrMaps(ctx, conn, "SELECT attempt_json FROM generation_attempts WHERE run_id=?", text(0))
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			a, err := glStored(row["attempt_json"])
			if err != nil {
				return nil, err
			}
			if a["status"] != "cancelled-before-dispatch" {
				return true, nil
			}
		}
		return false, nil
	case "findInvocation":
		out, err := g.findInvocation(ctx, conn, text(0), text(1), text(2))
		if out == nil {
			return nil, err
		}
		return out, err
	case "invocationState":
		row, err := glOne(ctx, conn, `SELECT attempt_id,attempt_json FROM generation_attempts WHERE run_id=? AND invocation_nonce=?`, "", text(0), text(1))
		if err != nil || row == nil {
			return nil, err
		}
		attempt, err := glStored(row["attempt_json"])
		if err != nil {
			return nil, err
		}
		return map[string]any{"attemptId": glText(row["attempt_id"]), "status": glText(attempt["status"])}, nil
	case "receipt":
		return g.receipt(ctx, conn, text(0), glMap(arg(1)))
	case "reserve":
		runID, nonce, hash := text(0), text(1), text(2)
		if err := g.mutableRun(ctx, conn, runID); err != nil {
			return nil, err
		}
		prior, err := g.findInvocation(ctx, conn, runID, nonce, hash)
		if err != nil {
			return nil, err
		}
		if prior != nil {
			return prior, nil
		}
		if nonce == "" || !plotRevisionPattern.MatchString(hash) {
			return nil, glError("GENERATION_INVOCATION_INVALID")
		}
		live, err := g.inFlight(ctx, conn, runID)
		if err != nil {
			return nil, err
		}
		if live {
			return nil, glError("GENERATION_DISPATCH_IN_FLIGHT")
		}
		run, err := g.get(ctx, conn, runID)
		if err != nil {
			return nil, err
		}
		budget, err := g.budget(ctx, conn, glText(run["rootActionId"]))
		if err != nil {
			return nil, err
		}
		if run["status"] != "running" || phTruthy(budget["blockedCode"]) {
			return nil, glError("GENERATION_DISPATCH_BLOCKED")
		}
		binding, root := glMap(run["binding"]), glMap(budget["root"])
		if !glScalarSame(binding["projectId"], root["projectId"]) || !glScalarSame(binding["epoch"], root["epoch"]) {
			return nil, glError("GENERATION_EPOCH_STALE")
		}
		if glPlanning(text(6)) || glPlanning(glText(glMap(arg(7))["operation"])) {
			return nil, glError("GENERATION_OPERATION_UNSUPPORTED")
		}
		attemptID, err := g.id()
		if err != nil {
			return nil, err
		}
		reservationID, err := g.id()
		if err != nil {
			return nil, err
		}
		attempt := map[string]any{"attemptId": attemptID, "reservationId": reservationID, "rootActionId": run["rootActionId"], "status": "reserved", "reservedTokens": glPlain(arg(3)), "requestedOutputTokens": glPlain(arg(4))}
		if err := GLAssertReservation(root, glMap(budget["policy"]), glAttemptMaps(budget["attempts"]), attempt, budget["activeElapsedMs"]); err != nil {
			return nil, err
		}
		encoded, err := plotJSON(plotOrdered(attempt, "attemptId", "reservationId", "rootActionId", "status", "reservedTokens", "requestedOutputTokens"))
		if err != nil {
			return nil, err
		}
		initial, err := plotJSON(plotObject{{"requestHash", hash}, {"usagePolicy", arg(5)}})
		if err != nil {
			return nil, err
		}
		if _, err := conn.ExecContext(ctx, "INSERT INTO generation_attempts VALUES(?,?,?,?,?,?,?)", attemptID, reservationID, runID, run["rootActionId"], encoded, initial, nonce); err != nil {
			return nil, err
		}
		artifactID, err := g.id()
		if err != nil {
			return nil, err
		}
		storedRun, err := glOne(ctx, conn, "SELECT binding_json FROM generation_runs WHERE run_id=?", "GENERATION_RUN_MISSING", runID)
		if err != nil {
			return nil, err
		}
		orderedBinding, err := glParse(glText(storedRun["binding_json"]))
		if err != nil {
			return nil, err
		}
		fingerprint := glField(orderedBinding, "fingerprint")
		artifact := plotObject{{"artifactId", artifactID}, {"attemptId", attemptID}, {"rootActionId", run["rootActionId"]}, {"projectId", binding["projectId"]}, {"epoch", binding["epoch"]}, {"fingerprint", fingerprint}, {"revision", int64(0)}, {"text", ""}, {"textHash", recoveryHash("")}}
		usage := plotObject{{"requestHash", hash}, {"usagePolicy", arg(5)}}
		for _, f := range []struct {
			index int
			key   string
		}{{6, "purpose"}, {7, "replayTask"}, {8, "budgetDecision"}, {9, "reconciliationInjected"}} {
			if phTruthy(glPlain(arg(f.index))) {
				v := arg(f.index)
				if f.index == 9 {
					v = true
				}
				usage = append(usage, plotField{f.key, v})
			}
		}
		usage = append(usage, plotField{"artifactIdentity", plotObject{{"artifactId", artifactID}, {"epoch", binding["epoch"]}, {"fingerprint", fingerprint}}})
		usageJSON, err := plotJSON(usage)
		if err != nil {
			return nil, err
		}
		artifactJSON, err := plotJSON(artifact)
		if err != nil {
			return nil, err
		}
		if _, err := conn.ExecContext(ctx, "UPDATE generation_attempts SET usage_receipt_json=? WHERE attempt_id=?", usageJSON, attemptID); err != nil {
			return nil, err
		}
		if _, err := conn.ExecContext(ctx, "INSERT INTO generation_artifacts VALUES(?,?,?,?,?,?)", artifactID, attemptID, runID, artifactJSON, 0, "partial"); err != nil {
			return nil, err
		}
		return g.receipt(ctx, conn, attemptID, nil)
	case "markDispatched":
		receipt, err := g.receipt(ctx, conn, text(0), nil)
		if err != nil {
			return nil, err
		}
		run, attempt, budget := glMap(receipt["run"]), glMap(receipt["attempt"]), glMap(receipt["budget"])
		if err := g.mutableRun(ctx, conn, glText(run["runId"])); err != nil {
			return nil, err
		}
		elapsed, ok := rrInteger(budget["activeElapsedMs"], 0)
		maximum, maxOK := rrInteger(glMap(budget["policy"])["maxActiveElapsedMs"], 1)
		if !ok || !maxOK || elapsed >= maximum || glMap(budget["root"])["status"] != "active" || phTruthy(budget["blockedCode"]) {
			return nil, glError("GENERATION_DISPATCH_BLOCKED")
		}
		if err := GLAssertAttemptTransition(glText(attempt["status"]), "dispatch-marked"); err != nil {
			return nil, err
		}
		if err := glUpdateAttempt(ctx, conn, text(0), map[string]any{"status": "dispatch-marked"}); err != nil {
			return nil, err
		}
		_, err = conn.ExecContext(ctx, "UPDATE generation_roots SET active_since_ms=COALESCE(active_since_ms,?) WHERE root_action_id=?", g.now(), run["rootActionId"])
		return nil, err
	case "snapshot":
		receipt, err := g.receipt(ctx, conn, text(0), nil)
		if err != nil {
			return nil, err
		}
		run, attempt, prior, budget := glMap(receipt["run"]), glMap(receipt["attempt"]), glMap(receipt["artifact"]), glMap(receipt["budget"])
		if err := g.mutableRun(ctx, conn, glText(run["runId"])); err != nil {
			return nil, err
		}
		if prior == nil {
			return nil, glError("ARTIFACT_INTEGRITY_FAILED")
		}
		root := glMap(budget["root"])
		if !glScalarSame(prior["projectId"], root["projectId"]) || !glScalarSame(prior["epoch"], root["epoch"]) {
			return nil, glError("GENERATION_EPOCH_STALE")
		}
		expected, expectedOK := rrInteger(arg(1), -9007199254740991)
		body, bodyOK := arg(2).(string)
		revision, revisionOK := rrInteger(prior["revision"], -9007199254740991)
		if expectedOK && bodyOK && revisionOK && revision == expected+1 && prior["text"] == body {
			return prior, nil
		}
		if attempt["status"] == "settled" || attempt["status"] == "unknown" || attempt["status"] == "cancelled-before-dispatch" {
			return nil, glError("ARTIFACT_ATTEMPT_TERMINAL")
		}
		if !expectedOK || !bodyOK || !revisionOK || !utf8.ValidString(body) {
			return nil, glError("ARTIFACT_SNAPSHOT_CONFLICT")
		}
		if revision != expected || !strings.HasPrefix(body, glText(prior["text"])) || glThink.MatchString(body) {
			return nil, glError("ARTIFACT_SNAPSHOT_CONFLICT")
		}
		row, err := glOne(ctx, conn, "SELECT artifact_json FROM generation_artifacts WHERE attempt_id=?", "ARTIFACT_INTEGRITY_FAILED", text(0))
		if err != nil {
			return nil, err
		}
		encoded, err := glPatch(row["artifact_json"], map[string]any{"text": body, "textHash": recoveryHash(body), "revision": expected + 1})
		if err != nil {
			return nil, err
		}
		updated, err := conn.ExecContext(ctx, "UPDATE generation_artifacts SET artifact_json=?,revision=? WHERE attempt_id=? AND revision=?", encoded, expected+1, text(0), expected)
		if err := rrAffected(updated, err, glError("ARTIFACT_SNAPSHOT_CONFLICT")); err != nil {
			return nil, err
		}
		return glStored(encoded)
	case "settle":
		receipt, err := g.receipt(ctx, conn, text(0), nil)
		if err != nil {
			return nil, err
		}
		run, attempt := glMap(receipt["run"]), glMap(receipt["attempt"])
		if err := g.mutableRun(ctx, conn, glText(run["runId"])); err != nil {
			return nil, err
		}
		usage := glMap(arg(1))
		status := "unknown"
		trusted := phTruthy(usage["trusted"])
		if trusted {
			status = "settled"
		}
		if err := GLAssertAttemptTransition(glText(attempt["status"]), status); err != nil {
			return nil, err
		}
		patch := map[string]any{"status": status}
		if trusted {
			if value, exists := usage["actualTokens"]; exists {
				patch["actualTokens"] = value
			}
		}
		if err := glUpdateAttempt(ctx, conn, text(0), patch); err != nil {
			return nil, err
		}
		row, err := glOne(ctx, conn, "SELECT usage_receipt_json FROM generation_attempts WHERE attempt_id=?", "GENERATION_ATTEMPT_MISSING", text(0))
		if err != nil {
			return nil, err
		}
		result := plotObject{{"usage", arg(1)}, {"finishReason", arg(2)}}
		if phTruthy(arg(3)) {
			result = append(result, plotField{"failureCode", arg(3)})
		}
		if phTruthy(glPlain(arg(4))) {
			result = append(result, plotField{"diagnostics", arg(4)})
		}
		encoded, err := glPatch(row["usage_receipt_json"], map[string]any{"result": result})
		if err != nil {
			return nil, err
		}
		if _, err := conn.ExecContext(ctx, "UPDATE generation_attempts SET usage_receipt_json=? WHERE attempt_id=?", encoded, text(0)); err != nil {
			return nil, err
		}
		if err := g.stopClock(ctx, conn, glText(run["rootActionId"])); err != nil {
			return nil, err
		}
		return g.receipt(ctx, conn, text(0), nil)
	case "block":
		if err := g.mutableRoot(ctx, conn, text(0)); err != nil {
			return nil, err
		}
		_, err := conn.ExecContext(ctx, "UPDATE generation_roots SET blocked_code=? WHERE root_action_id=?", text(1), text(0))
		return nil, err
	case "pause":
		return nil, g.pause(ctx, conn, text(0), phTruthy(arg(1)))
	case "recoverInterrupted":
		roots, err := rrMaps(ctx, conn, "SELECT root_action_id FROM generation_roots ORDER BY rowid")
		if err != nil {
			return nil, err
		}
		// No partial recovery of a mixed unsupported database, and no portable
		// history silently reported as recovered by this normal-only primitive.
		for _, root := range roots {
			if err := g.mutableRoot(ctx, conn, glText(root["root_action_id"])); err != nil {
				return nil, err
			}
		}
		for _, root := range roots {
			rootID := glText(root["root_action_id"])
			budget, err := g.budget(ctx, conn, rootID)
			if err != nil {
				return nil, err
			}
			for _, attempt := range glAttemptMaps(budget["attempts"]) {
				if attempt["status"] == "dispatch-marked" {
					if err := glUpdateAttempt(ctx, conn, glText(attempt["attemptId"]), map[string]any{"status": "unknown"}); err != nil {
						return nil, err
					}
				}
			}
			if err := g.pause(ctx, conn, rootID, false); err != nil {
				return nil, err
			}
		}
		return nil, nil
	case "discardCandidate":
		row, err := glOne(ctx, conn, "SELECT run_id FROM generation_artifacts WHERE artifact_id=?", "", text(0))
		if err != nil {
			return nil, err
		}
		if row != nil {
			if err := g.mutableRun(ctx, conn, glText(row["run_id"])); err != nil {
				return nil, err
			}
		}
		_, err = conn.ExecContext(ctx, "UPDATE generation_artifacts SET status='discarded' WHERE artifact_id=?", text(0))
		return nil, err
	// RecordEffect is how a graph or finalization run publishes the domain effect
	// it produced. It patches exactly one named receipt key, refuses every key the
	// run's own operation does not own, and is idempotent: replaying the same
	// [key, value] leaves the row untouched.
	case "recordEffect":
		run, err := g.get(ctx, conn, text(0))
		if err != nil {
			return nil, err
		}
		if err := g.mutableRun(ctx, conn, text(0)); err != nil {
			return nil, err
		}
		key := text(2)
		binding := glMap(run["binding"])
		if !glEffectKeyAllowed(key, glText(glMap(binding["sourceManifest"])["operation"])) {
			return nil, glError("GENERATION_EFFECT_UNSUPPORTED")
		}
		// The attempt must belong to this run. Scoping the lookup by run_id alone
		// is not enough and the attempt id alone is worse: a caller who may mutate
		// one run could otherwise write an effect into an attempt belonging to a
		// different run, which is exactly the cross-run contamination the
		// operation-scoped allowlist above exists to prevent.
		row, err := glOne(ctx, conn, "SELECT usage_receipt_json FROM generation_attempts WHERE attempt_id=? AND run_id=?", "GENERATION_ATTEMPT_MISSING", text(1), text(0))
		if err != nil {
			return nil, err
		}
		current, err := glStored(row["usage_receipt_json"])
		if err != nil {
			return nil, err
		}
		if existing, ok := current[key]; ok {
			before, beforeErr := json.Marshal(existing)
			after, afterErr := json.Marshal(arg(3))
			if beforeErr == nil && afterErr == nil && string(before) == string(after) {
				return g.receipt(ctx, conn, text(1), nil)
			}
		}
		encoded, err := glPatch(row["usage_receipt_json"], map[string]any{key: arg(3)})
		if err != nil {
			return nil, err
		}
		if _, err := conn.ExecContext(ctx, "UPDATE generation_attempts SET usage_receipt_json=? WHERE attempt_id=?", encoded, text(1)); err != nil {
			return nil, err
		}
		// receipt takes an attempt id; the project id in text(0) would never match.
		return g.receipt(ctx, conn, text(1), nil)
	// RecordAgentReceipt is the agent tool protocol's receipt writer: it patches
	// the closed agent action-map vocabulary on one attempt's usage receipt,
	// gated to operation agent-round (generation_ledger_agent.go). The fourth
	// argument is the patch object and the fifth the append order for new keys.
	case "recordAgentReceipt":
		patch := glMap(arg(2))
		if patch == nil {
			return nil, glError("GENERATION_INPUT_INVALID")
		}
		var order []string
		if values, ok := plotArray(arg(3)); ok {
			for _, raw := range values {
				if key, ok := raw.(string); ok {
					order = append(order, key)
				}
			}
		}
		return g.recordAgentReceipt(ctx, conn, text(0), text(1), patch, order)
	// ActivateRoot re-activates a paused root and its runs without rewriting any
	// binding, for the agent round's completed-round resume (generation_ledger_agent.go).
	case "activateRoot":
		return nil, g.activateRoot(ctx, conn, text(0))
	case "replaceUnstartedBinding", "resume":
		// replace validates the next frozen binding before reading the run in
		// the original; preserve that error precedence even for missing runs.
		if method == "replaceUnstartedBinding" {
			if _, err := GLBinding(glMap(arg(2))); err != nil {
				return nil, err
			}
		}
		if err := g.mutableRun(ctx, conn, text(0)); err != nil {
			return nil, err
		}
		run, err := g.get(ctx, conn, text(0))
		if err != nil {
			return nil, err
		}
		budget, err := g.budget(ctx, conn, glText(run["rootActionId"]))
		if err != nil {
			return nil, err
		}
		nextRaw := arg(1)
		if method == "replaceUnstartedBinding" {
			nextRaw = arg(2)
		}
		next := glMap(nextRaw)
		frozen, err := GLBinding(next)
		if err != nil {
			return nil, err
		}
		if err := glOrdinaryBinding(next); err != nil {
			return nil, err
		}
		if err := glOrderSensitive(glField(nextRaw, "fingerprint")); err != nil {
			return nil, err
		}
		root := glMap(budget["root"])
		if method == "replaceUnstartedBinding" {
			if !glSame(run["binding"], arg(1)) {
				return nil, glError("GENERATION_BINDING_CONFLICT")
			}
			row, err := glOne(ctx, conn, "SELECT COUNT(*) AS n FROM generation_attempts WHERE run_id=?", "", text(0))
			if err != nil {
				return nil, err
			}
			if run["status"] != "running" || root["status"] != "active" || !rrEqual(row["n"], int64(0)) || phTruthy(budget["blockedCode"]) {
				return nil, glError("GENERATION_MATERIAL_DECISION_TOO_LATE")
			}
		} else {
			live := false
			for _, a := range glAttemptMaps(budget["attempts"]) {
				if a["status"] == "dispatch-marked" {
					live = true
				}
			}
			// A committed run outlives the lease it was frozen under, so the
			// lane re-freezes a running root too (the architecture workflow
			// resumes its main run between steps after a save), not only a
			// paused one. A live dispatch still refuses.
			if live || (root["status"] != "paused" && root["status"] != "active") || phTruthy(budget["blockedCode"]) || !glScalarSame(next["projectId"], glMap(run["binding"])["projectId"]) {
				return nil, glError("GENERATION_RESUME_REFUSED")
			}
			row, err := glOne(ctx, conn, "SELECT action_json FROM generation_roots WHERE root_action_id=?", "GENERATION_ROOT_MISSING", run["rootActionId"])
			if err != nil {
				return nil, err
			}
			encoded, err := glPatch(row["action_json"], map[string]any{"epoch": next["epoch"], "status": "active"})
			if err != nil {
				return nil, err
			}
			if _, err := conn.ExecContext(ctx, "UPDATE generation_roots SET action_json=? WHERE root_action_id=?", encoded, run["rootActionId"]); err != nil {
				return nil, err
			}
		}
		bindingJSON, err := plotJSON(plotObject{{"projectId", frozen["projectId"]}, {"epoch", frozen["epoch"]}, {"fingerprint", glField(nextRaw, "fingerprint")}, {"contextSnapshotId", frozen["contextSnapshotId"]}, {"sourceManifest", glField(nextRaw, "sourceManifest")}, {"sourceRefs", glField(nextRaw, "sourceRefs")}})
		if err != nil {
			return nil, err
		}
		if method == "resume" {
			// The original validates a frozen projection but stores encode(next),
			// including opaque extra binding fields. Do not silently drop them.
			bindingJSON, err = plotJSON(nextRaw)
			if err != nil {
				return nil, err
			}
			_, err = conn.ExecContext(ctx, "UPDATE generation_runs SET binding_json=?,status='running' WHERE run_id=?", bindingJSON, text(0))
			return nil, err
		}
		if _, err := conn.ExecContext(ctx, "UPDATE generation_runs SET binding_json=? WHERE run_id=?", bindingJSON, text(0)); err != nil {
			return nil, err
		}
		return g.get(ctx, conn, text(0))
	}
	return nil, glError("GENERATION_OPERATION_UNSUPPORTED")
}
