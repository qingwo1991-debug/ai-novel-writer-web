package store

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"strings"
)

var (
	ErrProjectNotOpen                    = errors.New("PROJECT_NOT_OPEN")
	ErrProjectCoreCharactersArchReadOnly = errors.New("PROJECT_CORE_CHARACTERS_ARCH_READ_ONLY")
	ErrGenerationOwnerRequired           = errors.New("GENERATION_OWNER_REQUIRED")
	ErrProjectCoreInvalidBinding         = errors.New("PROJECT_CORE_INVALID_BINDING")
)

// ProjectCore borrows an already admitted project database. Construction performs
// no I/O; this repository never initializes rows, migrates, or changes pragmas.
type ProjectCore struct{ db *sql.DB }

func NewProjectCore(db *sql.DB) *ProjectCore { return &ProjectCore{db: db} }

type projectCoreField struct{ key, column string }

// Order and allowlist match ProjectCoreRepository.update in the TS repository.
// charactersArch is read-only, but remains part of the read model and CAS source.
var projectCoreFields = [...]projectCoreField{
	{"projectName", "project_name"},
	{"genre", "genre"},
	{"subGenre", "sub_genre"},
	{"targetAudience", "target_audience"},
	{"totalChapters", "total_chapters"},
	{"wordsPerChapter", "words_per_chapter"},
	{"writingLanguage", "writing_language"},
	{"creativeStrategy", "creative_strategy"},
	{"narrativeThreadDormantChapterThreshold", "narrative_thread_dormant_threshold"},
	{"plotStructure", "plot_structure"},
	{"narrativePov", "narrative_pov"},
	{"writingStyle", "writing_style"},
	{"referenceWorks", "reference_works"},
	{"globalGuidance", "global_guidance"},
	{"goldenFinger", "golden_finger"},
	{"coreOutline", "core_outline"},
	{"worldSetting", "world_setting"},
	{"protagonistProfile", "protagonist_profile"},
	{"premise", "premise"},
	{"worldbuilding", "worldbuilding"},
	{"synopsis", "synopsis"},
	{"characterStates", "character_states"},
	{"charactersArch", "characters_arch"},
}

// Get returns nil for a missing main row or unopened database. Interface-valued
// scans deliberately preserve SQL NULL, TEXT, INTEGER, REAL, and BLOB instead of
// coercing nullable data to zero values. Only the three TS read normalizers run.
func (r *ProjectCore) Get(ctx context.Context) (map[string]any, error) {
	if r == nil || r.db == nil {
		return nil, nil
	}
	columns := make([]string, len(projectCoreFields))
	values := make([]any, len(projectCoreFields))
	targets := make([]any, len(projectCoreFields))
	for i, field := range projectCoreFields {
		columns[i] = field.column
		targets[i] = &values[i]
	}
	err := r.db.QueryRowContext(ctx, "SELECT "+strings.Join(columns, ", ")+" FROM project_core WHERE id = ?", "main").Scan(targets...)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	data := make(map[string]any, len(projectCoreFields))
	for i, field := range projectCoreFields {
		data[field.key] = values[i]
	}
	data["writingLanguage"] = projectCoreWritingLanguage(data["writingLanguage"])
	strategy, _ := data["creativeStrategy"].(string)
	switch strategy {
	case "auto", "fluent-drafting", "consistency-first", "deep-planning":
		data["creativeStrategy"] = strategy
	default:
		data["creativeStrategy"] = "auto"
	}
	data["narrativeThreadDormantChapterThreshold"] = projectCoreDormantThreshold(data["narrativeThreadDormantChapterThreshold"])
	return data, nil
}

func projectCoreWritingLanguage(value any) string {
	if language, ok := value.(string); ok && language == "en-US" {
		return "en-US"
	}
	return "zh-CN"
}

// SQLite reads integers as int64, whereas JavaScript reads them as numbers.
// Accept Go's numeric equivalents, but never parse strings or coerce booleans.
func projectCoreDormantThreshold(value any) int64 {
	var number float64
	switch value := value.(type) {
	case float64:
		number = value
	case float32:
		number = float64(value)
	case int:
		number = float64(value)
	case int8:
		number = float64(value)
	case int16:
		number = float64(value)
	case int32:
		number = float64(value)
	case int64:
		number = float64(value)
	case uint:
		number = float64(value)
	case uint8:
		number = float64(value)
	case uint16:
		number = float64(value)
	case uint32:
		number = float64(value)
	case uint64:
		number = float64(value)
	default:
		return 3
	}
	if math.IsNaN(number) || math.IsInf(number, 0) {
		return 3
	}
	if number < 1 {
		return 1
	}
	if number > 50 {
		return 50
	}
	return int64(math.Trunc(number))
}

// better-sqlite3 rejects booleans, objects, and arrays as SQL bindings. Do not
// silently accept database/sql's bool/time/Valuer coercions at this boundary.
// The RPC layer validates field types; this check also protects direct callers.
func projectCoreBinding(value any) (any, error) {
	switch value.(type) {
	case nil, string, []byte, float64, float32, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return value, nil
	default:
		return nil, ErrProjectCoreInvalidBinding
	}
}

// projectCoreUpdateStatement builds the one UPDATE ProjectCore.Update performs:
// a dynamic SET over the allowlisted columns the caller named, `updated_at =
// datetime('now')`, and `WHERE id = ?` with 'main' (project-core-repository.ts
// :204, :208). It answers the empty string for a request that named no
// allowlisted field, because the reference returns at :201 before appending
// updated_at - such a call writes nothing at all, it does not write a timestamp.
//
// The builder is apart from Update because db:project-core-commit-generated runs
// the same statement on a reserved connection inside its own immediate
// transaction (projectcore_generated_tx.go). One builder means the two entry
// points to project_core cannot drift into writing different columns.
func projectCoreUpdateStatement(data map[string]any) (string, []any, error) {
	if _, present := data["charactersArch"]; present {
		return "", nil, ErrProjectCoreCharactersArchReadOnly
	}
	clauses := make([]string, 0, len(projectCoreFields))
	values := make([]any, 0, len(projectCoreFields))
	for _, field := range projectCoreFields {
		if field.key == "charactersArch" {
			continue
		}
		value, present := data[field.key]
		if !present {
			continue
		}
		if field.key == "narrativeThreadDormantChapterThreshold" {
			value = projectCoreDormantThreshold(value)
		}
		binding, err := projectCoreBinding(value)
		if err != nil {
			return "", nil, err
		}
		clauses = append(clauses, field.column+" = ?")
		values = append(values, binding)
	}
	if len(clauses) == 0 {
		return "", nil, nil
	}
	clauses = append(clauses, "updated_at = datetime('now')")
	values = append(values, "main")
	return "UPDATE project_core SET " + strings.Join(clauses, ", ") + " WHERE id = ?", values, nil
}

// Update ignores unknown keys and only updates allowlisted fields. Presence of
// charactersArch (including an explicit null) rejects the entire operation.
func (r *ProjectCore) Update(ctx context.Context, data map[string]any) error {
	if r == nil || r.db == nil {
		return ErrProjectNotOpen
	}
	statement, values, err := projectCoreUpdateStatement(data)
	if err != nil || statement == "" {
		return err
	}
	_, err = r.db.ExecContext(ctx, statement, values...)
	return err
}

const projectCoreSynopsisCAS = `UPDATE project_core
 SET synopsis = ?, updated_at = datetime('now')
 WHERE id = 'main'
 AND synopsis = ?
 AND premise = ?
 AND characters_arch = ?
 AND worldbuilding = ?
 AND genre = ?
 AND total_chapters = ?
 AND words_per_chapter = ?
 AND CASE WHEN writing_language = 'en-US' THEN 'en-US' ELSE 'zh-CN' END = ?
 AND plot_structure = ?
 AND narrative_pov = ?
 AND global_guidance = ?`

// ProjectCoreCommitScope is the live session identity a synopsis commit's owner
// proofs compare against: the admitted project and the writer lease the frozen
// session proves (deps.projectId and deps.epoch in the reference's
// assertSourcesCurrent). It is only consulted when the request carries a
// generationRunHandle; the plain CAS path ignores it.
type ProjectCoreCommitScope struct {
	ProjectID string
	LiveEpoch string
}

// CommitSynopsis performs one atomic SQL compare-and-set over the target body
// and all eleven source fields. It does not read/rebase a baseline, compare a
// timestamp, normalize expected values, or substitute NULL-safe equality.
//
// The reference's db-controller wraps the commit in
// withGenerationAgentChildEffect + assertGenerationSynopsisCommit whenever the
// request carries generationRunHandle (db-controller.ts:224-230,
// generation-controller.ts:47-50, main-generation-owner.ts:900-911). For the
// runs this lane can begin - no agentWorkflowRegistrationId and no frozen
// plotOutline policy - that collapses to the same proofs CommitGenerated
// already performs: run identity, live epoch, uncancelled root. They run
// inside one BEGIN IMMEDIATE with the CAS, so a refused proof writes nothing.
// authorRecovery selects the plot-outline author-edit commit, which is a
// different unported owner path and stays refused by name.
func (r *ProjectCore) CommitSynopsis(ctx context.Context, request map[string]any, scopes ...ProjectCoreCommitScope) (bool, error) {
	var scope ProjectCoreCommitScope
	if len(scopes) > 0 {
		scope = scopes[0]
	}
	if _, present := request["authorRecovery"]; present {
		return false, ErrGenerationOwnerRequired
	}
	if r == nil || r.db == nil {
		return false, ErrProjectNotOpen
	}
	if ctx == nil {
		return false, ErrProjectNotOpen
	}
	if raw, present := request["generationRunHandle"]; present && raw != nil {
		return r.commitSynopsisOwned(ctx, request, raw, scope)
	}
	values, err := projectCoreSynopsisValues(request)
	if err != nil {
		return false, err
	}
	result, err := r.db.ExecContext(ctx, projectCoreSynopsisCAS, values...)
	if err != nil {
		return false, err
	}
	changes, err := result.RowsAffected()
	return changes == 1, err
}

// projectCoreSynopsisValues binds the CAS parameters: the new synopsis body
// followed by the eleven frozen baselines, each through the same
// projectCoreBinding normalization the plain update path uses.
func projectCoreSynopsisValues(request map[string]any) ([]any, error) {
	expected, ok := request["expected"].(map[string]any)
	if !ok {
		return nil, ErrProjectCoreInvalidBinding
	}
	value, present := request["synopsis"]
	if !present {
		return nil, ErrProjectCoreInvalidBinding
	}
	binding, err := projectCoreBinding(value)
	if err != nil {
		return nil, err
	}
	values := []any{binding}
	for _, key := range []string{"synopsis", "premise", "charactersArch", "worldbuilding", "genre", "totalChapters", "wordsPerChapter", "writingLanguage", "plotStructure", "narrativePov", "globalGuidance"} {
		value, present := expected[key]
		if !present {
			return nil, ErrProjectCoreInvalidBinding
		}
		binding, err := projectCoreBinding(value)
		if err != nil {
			return nil, err
		}
		values = append(values, binding)
	}
	return values, nil
}

// commitSynopsisOwned is the generationRunHandle arm: the same run identity,
// epoch and root proofs CommitGenerated performs, then the identical CAS, all
// inside one BEGIN IMMEDIATE so a refused proof leaves the row untouched.
func (r *ProjectCore) commitSynopsisOwned(ctx context.Context, request map[string]any, raw any, scope ProjectCoreCommitScope) (bool, error) {
	object, ok := raw.(map[string]any)
	if !ok {
		return false, ErrProjectCoreGeneratedRunMissing
	}
	handle := BatchRunHandle{
		ProjectID:    projectCoreString(object["projectId"]),
		Epoch:        projectCoreString(object["epoch"]),
		RootActionID: projectCoreString(object["rootActionId"]),
		RunID:        projectCoreString(object["runId"]),
	}
	changed := false
	err := NewDatabaseMaintenance(r.db).dbMaintenanceImmediate(ctx, func(conn *sql.Conn) error {
		binding, err := projectCoreGeneratedRun(ctx, conn, ProjectCoreGeneratedRequest{
			Handle:    handle,
			ProjectID: scope.ProjectID,
		})
		if err != nil {
			return err
		}
		if binding.SourceManifest.AgentWorkflowRegistrationID != "" {
			return ErrProjectCoreGeneratedChildEffect
		}
		if handle.Epoch != scope.LiveEpoch {
			return ErrProjectCoreGeneratedEpochStale
		}
		status, err := projectCoreGeneratedRootStatus(ctx, conn, handle.RootActionID)
		if err != nil {
			return err
		}
		if status == "cancelled" {
			return ErrProjectCoreGeneratedCancelled
		}
		values, err := projectCoreSynopsisValues(request)
		if err != nil {
			return err
		}
		result, err := conn.ExecContext(ctx, projectCoreSynopsisCAS, values...)
		if err != nil {
			return err
		}
		affected, err := result.RowsAffected()
		changed = err == nil && affected == 1
		return err
	})
	return changed, err
}

func projectCoreString(value any) string {
	text, _ := value.(string)
	return text
}
