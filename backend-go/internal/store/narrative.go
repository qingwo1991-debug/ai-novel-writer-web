package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"strings"
)

var (
	ErrNarrativeThreadPlanInvalid = errors.New("NARRATIVE_THREAD_PLAN_INVALID")
	ErrNarrativeThreadRangeInvalid = errors.New("NARRATIVE_THREAD_RANGE_INVALID")
	ErrNarrativeThreadPlanNotFound = errors.New("NARRATIVE_THREAD_PLAN_NOT_FOUND")
	ErrNarrativeThreadEventInvalid = errors.New("NARRATIVE_THREAD_EVENT_INVALID")
	ErrNarrativeThreadEventSourceNotFinalized = errors.New("NARRATIVE_THREAD_EVENT_SOURCE_NOT_FINALIZED")
	ErrNarrativeThreadEvidenceNotInSource = errors.New("NARRATIVE_THREAD_EVIDENCE_NOT_IN_SOURCE")
	ErrConsistencyExemptionKeyInvalid = errors.New("CONSISTENCY_EXEMPTION_KEY_INVALID")
	ErrConsistencyExemptionReasonInvalid = errors.New("CONSISTENCY_EXEMPTION_REASON_INVALID")
)

// Narrative borrows an admitted project database; it never migrates, opens paths,
// changes journal policy, or reads published prose/knowledge files.
type Narrative struct{ db *sql.DB }

func NewNarrative(db *sql.DB) *Narrative { return &Narrative{db: db} }

// ECMAScript trim and /\s/u share this set. In particular FEFF is whitespace,
// whereas NEL (0085), which Go's strings.TrimSpace removes, is not.
func narrativeSpace(r rune) bool {
	return r == '\t' || r == '\n' || r == '\v' || r == '\f' || r == '\r' ||
		r == ' ' || r == '\u00a0' || r == '\u1680' || (r >= '\u2000' && r <= '\u200a') ||
		r == '\u2028' || r == '\u2029' || r == '\u202f' || r == '\u205f' || r == '\u3000' || r == '\ufeff'
}

func narrativeLength(text string) int {
	n := 0
	for _, r := range text { n++; if r > 0xffff { n++ } }
	return n
}

func narrativeText(value any, limit int) (string, bool) {
	text, ok := value.(string)
	if !ok { return "", false }
	text = strings.TrimFunc(text, narrativeSpace)
	return text, text != "" && narrativeLength(text) <= limit
}

func narrativeCompact(text string) string {
	return strings.Map(func(r rune) rune { if narrativeSpace(r) { return -1 }; return r }, text)
}

func narrativePlanInput(input map[string]any) ([]any, error) {
	title, titleOK := narrativeText(input["title"], 120)
	kind, kindOK := narrativeText(input["type"], 60)
	intent, intentOK := narrativeText(input["authorIntent"], 1000)
	if !titleOK || !kindOK || !intentOK { return nil, ErrNarrativeThreadPlanInvalid }
	start, startOK := rrInteger(input["targetStartChapter"], 1)
	end, endOK := rrInteger(input["targetEndChapter"], 1)
	if !startOK || !endOK || end < start { return nil, ErrNarrativeThreadRangeInvalid }
	return []any{title, kind, start, end, intent}, nil
}

const narrativePlanColumns = `id,title,type,target_start_chapter AS targetStartChapter,
 target_end_chapter AS targetEndChapter,author_intent AS authorIntent,
 created_at AS createdAt,updated_at AS updatedAt`

func narrativeGetPlan(ctx context.Context, q rrQueryer, id any) (map[string]any, error) {
	rows, err := rrMaps(ctx, q, "SELECT "+narrativePlanColumns+" FROM narrative_thread_plans WHERE id=?", id)
	if err != nil || len(rows) == 0 { return nil, err }
	return rows[0], nil
}

func (n *Narrative) CreatePlan(ctx context.Context, input map[string]any) (map[string]any, error) {
	if n == nil || n.db == nil { return nil, ErrProjectNotOpen }
	args, err := narrativePlanInput(input)
	if err != nil { return nil, err }
	var output map[string]any
	err = NewReviewRevisions(n.db).immediate(ctx, func(conn *sql.Conn) error {
		result, err := conn.ExecContext(ctx, `INSERT INTO narrative_thread_plans
 (title,type,target_start_chapter,target_end_chapter,author_intent) VALUES(?,?,?,?,?)`, args...)
		if err != nil { return err }
		id, err := result.LastInsertId()
		if err != nil { return err }
		output, err = narrativeGetPlan(ctx, conn, id)
		return err
	})
	if err != nil { return nil, err }
	return output, nil
}

func (n *Narrative) UpdatePlan(ctx context.Context, id int64, input map[string]any) (map[string]any, error) {
	if n == nil || n.db == nil { return nil, ErrProjectNotOpen }
	args, err := narrativePlanInput(input)
	if err != nil { return nil, err }
	args = append(args, id)
	var output map[string]any
	err = NewReviewRevisions(n.db).immediate(ctx, func(conn *sql.Conn) error {
		result, err := conn.ExecContext(ctx, `UPDATE narrative_thread_plans SET title=?,type=?,
 target_start_chapter=?,target_end_chapter=?,author_intent=?,updated_at=datetime('now') WHERE id=?`, args...)
		if err := rrAffected(result, err, ErrNarrativeThreadPlanNotFound); err != nil { return err }
		output, err = narrativeGetPlan(ctx, conn, id)
		return err
	})
	if err != nil { return nil, err }
	return output, nil
}

func (n *Narrative) DeletePlan(ctx context.Context, id int64) error {
	if n == nil || n.db == nil { return ErrProjectNotOpen }
	result, err := n.db.ExecContext(ctx, "DELETE FROM narrative_thread_plans WHERE id=?", id)
	return rrAffected(result, err, ErrNarrativeThreadPlanNotFound)
}

func (n *Narrative) GetPlan(ctx context.Context, id int64) (map[string]any, error) {
	if n == nil || n.db == nil { return nil, ErrProjectNotOpen }
	return narrativeGetPlan(ctx, n.db, id)
}

func narrativeBinding(value any) error {
	// database/sql silently binds bool as INTEGER, unlike better-sqlite3.
	// Reject unsupported bindings at each original statement's precedence point.
	switch value.(type) {
	case nil, string, []byte, int, int32, int64, float64, json.Number:
		return nil
	}
	return errors.New("SQLite3 can only bind numbers, strings, bigints, buffers, and null")
}

func (n *Narrative) ConfirmEvent(ctx context.Context, input map[string]any) (map[string]any, error) {
	if n == nil || n.db == nil { return nil, ErrProjectNotOpen }
	evidence, evidenceOK := narrativeText(input["evidence"], 240)
	reason, reasonOK := narrativeText(input["reason"], 500)
	kind, kindOK := input["type"].(string)
	if !evidenceOK || !reasonOK || !kindOK || (kind != "planted" && kind != "progressing" && kind != "resolved" && kind != "abandoned") {
		return nil, ErrNarrativeThreadEventInvalid
	}
	var output map[string]any
	err := NewReviewRevisions(n.db).immediate(ctx, func(conn *sql.Conn) error {
		if err := narrativeBinding(input["draftId"]); err != nil { return err }
		sources, err := rrMaps(ctx, conn, `SELECT d.id AS draftId,d.chapter_number AS chapterNumber,
 COALESCE(o.chapter_title,'') AS chapterTitle,o.content_snapshot AS snapshot FROM drafts d
 JOIN finalization_outbox o ON o.draft_id=d.id WHERE d.id=? AND d.status='finalized'`, input["draftId"])
		if err != nil { return err }
		if len(sources) == 0 { return ErrNarrativeThreadEventSourceNotFinalized }
		source := sources[0]
		// A corrupt BLOB is not prose. Do not stringify it or panic calling a
		// string operation. Evidence is bound to frozen outbox prose, not c.body.
		snapshot, ok := source["snapshot"].(string)
		if !ok { return ErrNarrativeThreadEventSourceNotFinalized }
		if !strings.Contains(narrativeCompact(snapshot), narrativeCompact(evidence)) { return ErrNarrativeThreadEvidenceNotInSource }
		if err := narrativeBinding(input["planId"]); err != nil { return err }
		plan, err := narrativeGetPlan(ctx, conn, input["planId"])
		if err != nil { return err }
		if plan == nil { return ErrNarrativeThreadPlanNotFound }
		result, err := conn.ExecContext(ctx, `INSERT INTO narrative_thread_confirmations
 (plan_id,draft_id,event_type,evidence,reason) VALUES(?,?,?,?,?)`, input["planId"], input["draftId"], kind, evidence, reason)
		if err != nil { return err }
		id, err := result.LastInsertId()
		if err != nil { return err }
		created, err := rrMaps(ctx, conn, "SELECT created_at AS createdAt FROM narrative_thread_confirmations WHERE id=?", id)
		if err != nil { return err }
		if len(created) == 0 { return ErrNarrativeThreadEventInvalid }
		// Preserve the original {id, ...input, evidence, reason, ...source}
		// order, including caller extra fields and an extra input.id override.
		output = map[string]any{"id": id}
		for key, value := range input { output[key] = value }
		output["evidence"], output["reason"] = evidence, reason
		output["chapterNumber"], output["chapterTitle"] = source["chapterNumber"], source["chapterTitle"]
		output["createdAt"] = created[0]["createdAt"]
		return nil
	})
	if err != nil { return nil, err }
	return output, nil
}

func narrativeNumeric(value any) float64 {
	switch value := value.(type) { case int64: return float64(value); case float64: return value }
	return math.NaN()
}

// Only the original read-side Array.join/includes operations stringify raw
// SQLite scalars. Never apply this conversion to validation or source authority.
func narrativeJoinText(value any) string {
	switch value := value.(type) {
	case string: return value
	case []byte: return string([]rune(string(value))) // Buffer.toString UTF-8 replacement
	case int64: return phNumberString(value)
	case float64: return phNumberString(value)
	case bool: if value { return "true" }; return "false"
	}
	return ""
}

func narrativeTitleLength(value any) int {
	switch value := value.(type) {
	case string: return narrativeLength(value)
	case []byte: return len(value) // Buffer.length, not the decoded text length
	}
	return 0
}

func narrativeList(ctx context.Context, q rrQueryer) ([]map[string]any, error) {
	latest, err := rrMaps(ctx, q, "SELECT COALESCE(MAX(chapter_number),0) AS chapterNumber FROM drafts WHERE status='finalized'")
	if err != nil { return nil, err }
	chapter := narrativeNumeric(latest[0]["chapterNumber"])
	plans, err := rrMaps(ctx, q, "SELECT "+narrativePlanColumns+" FROM narrative_thread_plans ORDER BY id ASC")
	if err != nil { return nil, err }
	for _, plan := range plans {
		events, err := rrMaps(ctx, q, `SELECT c.id,c.plan_id AS planId,c.draft_id AS draftId,c.event_type AS type,
 c.evidence,c.reason,c.created_at AS createdAt,d.chapter_number AS chapterNumber,
 COALESCE(o.chapter_title,'') AS chapterTitle FROM narrative_thread_confirmations c
 JOIN drafts d ON d.id=c.draft_id AND d.status='finalized'
 JOIN finalization_outbox o ON o.draft_id=d.id WHERE c.plan_id=? ORDER BY d.chapter_number ASC,c.id ASC`, plan["id"])
		if err != nil { return nil, err }
		var status any = "planned"
		lastChapter := plan["targetStartChapter"]
		if len(events) > 0 {
			last := events[len(events)-1]
			if last["type"] != nil { status = last["type"] }
			if last["chapterNumber"] != nil { lastChapter = last["chapterNumber"] }
		}
		terminal := rrEqual(status, "resolved") || rrEqual(status, "abandoned")
		dormant := float64(0)
		if !terminal { dormant = math.Max(0, chapter-narrativeNumeric(lastChapter)) }
		var dormantValue any = dormant
		if !math.IsNaN(dormant) && !math.IsInf(dormant, 0) && dormant <= 9007199254740991 && math.Trunc(dormant) == dormant { dormantValue = int64(dormant) }
		plan["status"], plan["dormantChapters"] = status, dormantValue
		plan["overdue"], plan["events"] = !terminal && chapter > narrativeNumeric(plan["targetEndChapter"]), events
	}
	return plans, nil
}

func (n *Narrative) List(ctx context.Context) ([]map[string]any, error) {
	if n == nil || n.db == nil { return nil, ErrProjectNotOpen }
	// All plan/event/max-chapter reads share one SQLite snapshot. A concurrent
	// supersession/deletion cannot produce a hybrid view. Never re-enter n.db.
	tx, err := n.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil { return nil, err }
	defer tx.Rollback()
	rows, err := narrativeList(ctx, tx)
	if err != nil { return nil, err }
	if err := tx.Commit(); err != nil { return nil, err }
	return rows, nil
}

func (n *Narrative) Relevant(ctx context.Context, input map[string]any) ([]map[string]any, error) {
	rows, err := n.List(ctx)
	if err != nil { return nil, err }
	title, _ := input["title"].(string)
	keyEvents, _ := input["keyEvents"].(string)
	currentText := title+"\n"+keyEvents
	characters := make([]string, 0)
	switch values := input["characters"].(type) {
	case []string: characters = append(characters, values...)
	case []any: for _, value := range values { if text, ok := value.(string); ok { characters = append(characters, text) } }
	}
	chapterValue, _ := phNumber(input["chapterNumber"])
	chapter := narrativeNumeric(chapterValue)
	result := make([]map[string]any, 0)
	for _, thread := range rows {
		if rrEqual(thread["status"], "resolved") || rrEqual(thread["status"], "abandoned") { continue }
		relevant := chapter >= narrativeNumeric(thread["targetStartChapter"]) && chapter <= narrativeNumeric(thread["targetEndChapter"])
		threadTitle := narrativeJoinText(thread["title"])
		parts := make([]string, 0)
		for _, key := range []string{"title", "type", "authorIntent"} { parts = append(parts, narrativeJoinText(thread[key])) }
		for _, event := range thread["events"].([]map[string]any) {
			for _, key := range []string{"evidence", "reason"} { parts = append(parts, narrativeJoinText(event[key])) }
		}
		threadText := strings.Join(parts, "\n")
		for _, character := range characters {
			character = strings.TrimFunc(character, narrativeSpace)
			if character != "" && strings.Contains(threadText, character) { relevant = true; break }
		}
		if narrativeTitleLength(thread["title"]) >= 2 && strings.Contains(currentText, threadTitle) { relevant = true }
		if relevant { result = append(result, thread) }
	}
	return result, nil
}

func (n *Narrative) Exemptions(ctx context.Context) ([]map[string]any, error) {
	if n == nil || n.db == nil { return []map[string]any{}, nil }
	rows, err := rrMaps(ctx, n.db, "SELECT stable_fact_key AS stableFactKey,reason,revoked FROM consistency_exemptions ORDER BY stable_fact_key")
	if err != nil { return nil, err }
	for _, row := range rows { row["revoked"] = rrEqual(row["revoked"], int64(1)) }
	return rows, nil
}

func (n *Narrative) SaveExemption(ctx context.Context, key, reason string) error {
	if n == nil || n.db == nil { return ErrProjectNotOpen }
	key, ok := narrativeText(key, 500)
	if !ok { return ErrConsistencyExemptionKeyInvalid }
	reason, ok = narrativeText(reason, 500)
	if !ok { return ErrConsistencyExemptionReasonInvalid }
	_, err := n.db.ExecContext(ctx, `INSERT INTO consistency_exemptions(stable_fact_key,reason,revoked) VALUES(?,?,0)
 ON CONFLICT(stable_fact_key) DO UPDATE SET reason=excluded.reason,revoked=0`, key, reason)
	return err
}

func (n *Narrative) RevokeExemption(ctx context.Context, key string) error {
	if n == nil || n.db == nil { return ErrProjectNotOpen }
	key, ok := narrativeText(key, 500)
	if !ok { return ErrConsistencyExemptionKeyInvalid }
	_, err := n.db.ExecContext(ctx, "UPDATE consistency_exemptions SET revoked=1 WHERE stable_fact_key=?", key)
	return err
}
