package admission

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

const m03MaxSafeInteger = 9007199254740991

type m03CycleBasicEvidence struct {
	cycleID, rootActionID, projectID, epoch, reviewBody, sourceContent, sourceStatus string
	reviewID, baseDraftID, chapterNumber, sourceVersion                              int64
	reviewContentHash, sourceHash, findingSetHash                                    string
	revisionStatus                                                                   string
}

// m03VerifyCycleBasicEvidence is one stage of the original M03 verifier. A
// successful return is never sufficient to admit or project a populated cycle.
func m03VerifyCycleBasicEvidence(ctx context.Context, tx *sql.Tx, cycleID string) (m03CycleBasicEvidence, error) {
	var out m03CycleBasicEvidence
	if err := m03VerifyMergeSnapshotCardinality(ctx, tx); err != nil {
		return out, err
	}
	var comparisonVersion, recheckCount int64
	var confirmationID, revisionID sql.NullInt64
	var confirmationHash, mergedHash, recheckAttemptID sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT cycle_id,root_action_id,review_id,review_content_hash,
		confirmation_review_id,confirmation_content_hash,revision_id,source_hash,finding_set_hash,
		comparison_version,revision_status,merged_hash,recheck_count,recheck_attempt_id
		FROM review_cycles WHERE cycle_id=?`, cycleID).Scan(
		&out.cycleID, &out.rootActionID, &out.reviewID, &out.reviewContentHash,
		&confirmationID, &confirmationHash, &revisionID, &out.sourceHash, &out.findingSetHash,
		&comparisonVersion, &out.revisionStatus, &mergedHash, &recheckCount, &recheckAttemptID)
	if err != nil {
		return out, err
	}
	if !m03Nonempty(out.cycleID) || !m03Nonempty(out.rootActionID) || !m03SafeInteger(out.reviewID, 1) ||
		!m03Hash(out.reviewContentHash) || !m03Hash(out.sourceHash) || !m03Hash(out.findingSetHash) ||
		!m03SafeInteger(comparisonVersion, 1) || !m03SafeInteger(recheckCount, 0) || recheckCount > 1 ||
		(out.revisionStatus != "not-generated" && out.revisionStatus != "generated" && out.revisionStatus != "merge-committed") {
		return out, errors.New("M03 cycle fields invalid")
	}
	var sourceChapter, sourceVersion sql.NullInt64
	var sourceStatus, sourceContent sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT r.base_draft_id,r.source_draft_chapter_number,r.source_draft_version,
		r.source_draft_status,r.source_content,c.body FROM reviews r JOIN contents c ON c.id=r.content_id
		WHERE r.id=?`, out.reviewID).Scan(&out.baseDraftID, &sourceChapter, &sourceVersion,
		&sourceStatus, &sourceContent, &out.reviewBody)
	if err != nil {
		return out, err
	}
	if !sourceChapter.Valid || !m03SafeInteger(sourceChapter.Int64, 1) ||
		!sourceVersion.Valid || !m03SafeInteger(sourceVersion.Int64, 1) ||
		!sourceStatus.Valid || !m03Nonempty(sourceStatus.String) || !sourceContent.Valid ||
		m03StringHash(out.reviewBody) != out.reviewContentHash || m03StringHash(sourceContent.String) != out.sourceHash {
		return out, errors.New("M03 review source hash differs")
	}
	out.chapterNumber, out.sourceVersion, out.sourceStatus, out.sourceContent = sourceChapter.Int64, sourceVersion.Int64, sourceStatus.String, sourceContent.String
	if confirmationID.Valid {
		if !m03SafeInteger(confirmationID.Int64, 1) || !confirmationHash.Valid || !m03Hash(confirmationHash.String) {
			return out, errors.New("M03 confirmation binding invalid")
		}
		var confirmationBody string
		var confirmationSource sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT r.source_content,c.body FROM reviews r JOIN contents c ON c.id=r.content_id
			WHERE r.id=? AND r.base_draft_id=?`, confirmationID.Int64, out.baseDraftID).Scan(&confirmationSource, &confirmationBody)
		if err != nil {
			return out, err
		}
		if !confirmationSource.Valid || m03StringHash(confirmationBody) != confirmationHash.String ||
			m03StringHash(confirmationSource.String) != out.sourceHash {
			return out, errors.New("M03 confirmation content differs")
		}
	} else if confirmationHash.Valid {
		return out, errors.New("M03 orphan confirmation hash")
	}
	if revisionID.Valid {
		if !m03SafeInteger(revisionID.Int64, 1) || !confirmationID.Valid {
			return out, errors.New("M03 revision binding invalid")
		}
		var baseID int64
		var revisionReviewID, revisionChapter, revisionVersion, mergedDraftID sql.NullInt64
		var revisionStatus string
		var revisionSourceStatus, revisionSourceContent sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT base_draft_id,review_source_id,source_draft_chapter_number,
			source_draft_version,source_draft_status,source_content,status,merged_to_draft_id
			FROM revisions WHERE id=?`, revisionID.Int64).Scan(&baseID, &revisionReviewID, &revisionChapter,
			&revisionVersion, &revisionSourceStatus, &revisionSourceContent, &revisionStatus, &mergedDraftID)
		if err != nil {
			return out, err
		}
		if baseID != out.baseDraftID || !revisionReviewID.Valid || revisionReviewID.Int64 != confirmationID.Int64 ||
			!revisionChapter.Valid || revisionChapter.Int64 != out.chapterNumber ||
			!revisionVersion.Valid || revisionVersion.Int64 != out.sourceVersion ||
			!revisionSourceStatus.Valid || revisionSourceStatus.String != out.sourceStatus ||
			!revisionSourceContent.Valid || revisionSourceContent.String != out.sourceContent {
			return out, errors.New("M03 revision source differs")
		}
		if out.revisionStatus == "generated" && revisionStatus != "pending" ||
			out.revisionStatus == "merge-committed" && (revisionStatus != "merged" || !mergedDraftID.Valid || mergedDraftID.Int64 != out.baseDraftID) {
			return out, errors.New("M03 revision state differs")
		}
	}
	if out.revisionStatus == "not-generated" && (revisionID.Valid || mergedHash.Valid || recheckCount != 0 || recheckAttemptID.Valid) ||
		out.revisionStatus == "generated" && (!revisionID.Valid || mergedHash.Valid || recheckCount != 0 || recheckAttemptID.Valid) ||
		out.revisionStatus == "merge-committed" && (!revisionID.Valid || !mergedHash.Valid || !m03Hash(mergedHash.String) ||
			(recheckCount == 0 && recheckAttemptID.Valid) || (recheckCount == 1 && !recheckAttemptID.Valid)) {
		return out, errors.New("M03 cycle state differs")
	}
	if out.revisionStatus == "merge-committed" {
		mergeTable, err := m03TableExists(ctx, tx, "review_cycle_merges")
		if err != nil {
			return out, err
		}
		var mergedBody string
		if mergeTable {
			err = tx.QueryRowContext(ctx, "SELECT body FROM review_cycle_merges WHERE cycle_id=?", out.cycleID).Scan(&mergedBody)
		} else {
			err = tx.QueryRowContext(ctx, `SELECT c.body FROM drafts d JOIN contents c ON c.id=d.content_id
				WHERE d.id=?`, out.baseDraftID).Scan(&mergedBody)
		}
		if err != nil {
			return out, err
		}
		if m03StringHash(mergedBody) != mergedHash.String {
			return out, errors.New("M03 merged body hash differs")
		}
	}
	var actionJSON string
	if err := tx.QueryRowContext(ctx, "SELECT action_json FROM generation_roots WHERE root_action_id=?", out.rootActionID).Scan(&actionJSON); err != nil {
		return out, err
	}
	if m03LoneSurrogateEscape(actionJSON) {
		return out, errors.New("M03 action JSON has isolated surrogate")
	}
	var action map[string]json.RawMessage
	if err := json.Unmarshal([]byte(actionJSON), &action); err != nil {
		return out, err
	}
	if err := json.Unmarshal(action["projectId"], &out.projectID); err != nil {
		return out, errors.New("M03 action project invalid")
	}
	if err := json.Unmarshal(action["epoch"], &out.epoch); err != nil {
		return out, errors.New("M03 action epoch invalid")
	}
	if !m03Nonempty(out.projectID) || !m03Nonempty(out.epoch) {
		return out, errors.New("M03 action identity empty")
	}
	rows, err := tx.QueryContext(ctx, `SELECT r.root_action_id,r.binding_json
		FROM generation_attempts a JOIN generation_runs r ON r.run_id=a.run_id
		WHERE a.root_action_id=? ORDER BY a.rowid`, out.rootActionID)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var runRoot, bindingJSON string
		if err := rows.Scan(&runRoot, &bindingJSON); err != nil {
			rows.Close()
			return out, err
		}
		if m03LoneSurrogateEscape(bindingJSON) {
			rows.Close()
			return out, errors.New("M03 binding JSON has isolated surrogate")
		}
		var binding map[string]json.RawMessage
		if err := json.Unmarshal([]byte(bindingJSON), &binding); err != nil {
			rows.Close()
			return out, err
		}
		var projectID, epoch string
		if err := json.Unmarshal(binding["projectId"], &projectID); err != nil {
			rows.Close()
			return out, errors.New("M03 attempt project invalid")
		}
		if err := json.Unmarshal(binding["epoch"], &epoch); err != nil {
			rows.Close()
			return out, errors.New("M03 attempt epoch invalid")
		}
		if runRoot != out.rootActionID || projectID != out.projectID || !m03Nonempty(epoch) {
			rows.Close()
			return out, errors.New("M03 attempt binding differs")
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	if err := VerifyM03FindingSetHash(ctx, tx, out.cycleID, out.findingSetHash); err != nil {
		return out, err
	}
	return out, nil
}

// M06 requires exactly one persisted merge snapshot for each committed cycle.
// Before M06, the original verifier instead reads the current merged draft.
func m03VerifyMergeSnapshotCardinality(ctx context.Context, tx *sql.Tx) error {
	exists, err := m03TableExists(ctx, tx, "review_cycle_merges")
	if err != nil || !exists {
		return err
	}
	var missing, stray int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM review_cycles c
		LEFT JOIN review_cycle_merges m ON m.cycle_id=c.cycle_id
		WHERE c.revision_status='merge-committed' AND m.cycle_id IS NULL`).Scan(&missing); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM review_cycle_merges m
		LEFT JOIN review_cycles c ON c.cycle_id=m.cycle_id
		WHERE c.cycle_id IS NULL OR c.revision_status<>'merge-committed'`).Scan(&stray); err != nil {
		return err
	}
	if missing != 0 || stray != 0 {
		return errors.New("M03 merge snapshot set differs")
	}
	return nil
}

func m03Nonempty(value string) bool { return strings.TrimFunc(value, m03JSWhitespace) != "" }

// ECMAScript WhiteSpace plus LineTerminator (String.trim and /\s/).
func m03JSWhitespace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', '\u00a0', '\u1680', '\u2028', '\u2029', '\u202f', '\u205f', '\u3000', '\ufeff':
		return true
	}
	return r >= '\u2000' && r <= '\u200a'
}

// encoding/json replaces isolated UTF-16 surrogate escapes with U+FFFD,
// whereas JSON.parse preserves them. Reject those records before comparing
// project identity. A valid adjacent high/low pair remains accepted.
func m03LoneSurrogateEscape(value string) bool {
	if !utf8.ValidString(value) {
		return true
	}
	inString := false
	for i := 0; i < len(value); i++ {
		switch value[i] {
		case '"':
			inString = !inString
		case '\\':
			if !inString || i+1 >= len(value) {
				continue
			}
			if value[i+1] != 'u' {
				i++
				continue
			}
			code, ok := m03HexQuad(value, i+2)
			if !ok {
				continue
			} // JSON decoder will reject malformed escapes.
			if code >= 0xdc00 && code <= 0xdfff {
				return true
			}
			if code >= 0xd800 && code <= 0xdbff {
				if i+11 >= len(value) || value[i+6] != '\\' || value[i+7] != 'u' {
					return true
				}
				low, ok := m03HexQuad(value, i+8)
				if !ok || low < 0xdc00 || low > 0xdfff {
					return true
				}
				i += 11
				continue
			}
			i += 5
		}
	}
	return false
}

func m03HexQuad(value string, offset int) (uint16, bool) {
	if offset+4 > len(value) {
		return 0, false
	}
	var result uint16
	for i := offset; i < offset+4; i++ {
		result <<= 4
		switch c := value[i]; {
		case c >= '0' && c <= '9':
			result |= uint16(c - '0')
		case c >= 'a' && c <= 'f':
			result |= uint16(c - 'a' + 10)
		case c >= 'A' && c <= 'F':
			result |= uint16(c - 'A' + 10)
		default:
			return 0, false
		}
	}
	return result, true
}
func m03SafeInteger(value, minimum int64) bool { return value >= minimum && value <= m03MaxSafeInteger }
func m03Hash(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, c := range value {
		if c < '0' || c > '9' && c < 'a' || c > 'f' {
			return false
		}
	}
	return true
}
func m03StringHash(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

// VerifyM03ReviewCycleSchema proves the schema portion of the original
// verifyM03ReviewCycle. It does not prove any populated review-cycle rows.
func VerifyM03ReviewCycleSchema(ctx context.Context, tx *sql.Tx) error {
	var enabled int
	if err := tx.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&enabled); err != nil {
		return err
	}
	if enabled != 1 {
		return errors.New("M03 foreign keys disabled")
	}
	if err := checkForeignKeys(ctx, tx); err != nil {
		return err
	}
	var version int
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	merge, err := m03TableExists(ctx, tx, "review_cycle_merges")
	if err != nil {
		return err
	}
	if !merge && version >= 7 {
		return errors.New("M03 merge table missing")
	}
	if merge {
		if err := m03ExactSchema(ctx, tx, "review_cycle_merges", []string{"cycle_id", "body"}, []string{
			"cycle_id TEXT PRIMARY KEY REFERENCES review_cycles(cycle_id) ON DELETE CASCADE", "body TEXT NOT NULL",
		}); err != nil {
			return err
		}
	}
	if err := m03ExactSchema(ctx, tx, "review_cycles", []string{
		"cycle_id", "root_action_id", "review_id", "review_content_hash", "confirmation_review_id",
		"confirmation_content_hash", "revision_id", "source_hash", "finding_set_hash", "comparison_version",
		"revision_status", "merged_hash", "recheck_count", "recheck_attempt_id",
	}, []string{
		"root_action_id TEXT NOT NULL REFERENCES generation_roots(root_action_id)",
		"review_id INTEGER NOT NULL UNIQUE REFERENCES reviews(id) ON DELETE CASCADE",
		"confirmation_review_id INTEGER UNIQUE REFERENCES reviews(id)",
		"revision_id INTEGER UNIQUE REFERENCES revisions(id)",
		"recheck_attempt_id TEXT UNIQUE REFERENCES generation_attempts(attempt_id)",
		"CHECK(recheck_count IN (0,1))", "revision_status='merge-committed'",
	}); err != nil {
		return err
	}
	return m03ExactSchema(ctx, tx, "review_findings", []string{
		"cycle_id", "finding_id", "source_project_id", "source_epoch", "source_id", "source_revision",
		"source_content_hash", "span_start", "span_end", "span_unit", "excerpt_hash", "occurrence",
		"category", "target_id", "kind", "problem_text", "expected_text", "status", "evidence_hash",
		"confirmation_item_index",
	}, []string{
		"cycle_id TEXT NOT NULL REFERENCES review_cycles(cycle_id) ON DELETE CASCADE",
		"status IN ('unverified','author-waived')", "span_unit='utf16-code-unit'", "status='author-waived'",
	})
}

func m03TableExists(ctx context.Context, tx *sql.Tx, name string) (bool, error) {
	var found int
	err := tx.QueryRowContext(ctx, "SELECT 1 FROM sqlite_master WHERE type='table' AND name=?", name).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func m03ExactSchema(ctx context.Context, tx *sql.Tx, table string, expected, fragments []string) error {
	// The table names are fixed literals from this package, never caller input.
	rows, err := tx.QueryContext(ctx, "PRAGMA table_info("+table+")")
	if err != nil {
		return err
	}
	var actual []string
	for rows.Next() {
		var cid, notNull, pk int
		var name, kind string
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &kind, &notNull, &defaultValue, &pk); err != nil {
			rows.Close()
			return err
		}
		actual = append(actual, name)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if len(actual) != len(expected) {
		return errors.New("M03 schema columns differ")
	}
	for i := range expected {
		if actual[i] != expected[i] {
			return errors.New("M03 schema columns differ")
		}
	}
	var ddl string
	if err := tx.QueryRowContext(ctx, "SELECT sql FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&ddl); err != nil {
		return err
	}
	compact := m03CompactSQL(ddl)
	for _, fragment := range fragments {
		if !strings.Contains(compact, m03CompactSQL(fragment)) {
			return errors.New("M03 schema fragment missing")
		}
	}
	return nil
}

func m03CompactSQL(value string) string {
	return strings.ToLower(strings.Map(func(r rune) rune {
		if m03JSWhitespace(r) {
			return -1
		}
		return r
	}, value))
}

// M03FindingIdentity contains exactly the immutable fields in the TypeScript
// canonicalM03FindingSetHash input. Status and evidence are deliberately absent.
type M03FindingIdentity struct {
	FindingID, SourceProjectID, SourceEpoch, SourceID string
	SourceRevision                                    int64
	SourceContentHash                                 string
	SpanStart, SpanEnd                                *int64
	SpanUnit, ExcerptHash                             *string
	Occurrence                                        *int64
	Category                                          string
	TargetID                                          *string
	Kind, ProblemText                                 string
	ExpectedText                                      *string
}

// CanonicalM03FindingSetHash mirrors JSON.stringify and UTF-16 finding-id
// order in the original TypeScript migration, including null fields.
func CanonicalM03FindingSetHash(findings []M03FindingIdentity) string {
	ordered := append([]M03FindingIdentity(nil), findings...)
	sort.Slice(ordered, func(i, j int) bool { return lessUTF16(ordered[i].FindingID, ordered[j].FindingID) })
	encoded := []byte{'['}
	for i, f := range ordered {
		if i != 0 {
			encoded = append(encoded, ',')
		}
		encoded = append(encoded, '{')
		encoded = m03StringField(encoded, "findingId", f.FindingID, false)
		encoded = m03StringField(encoded, "sourceProjectId", f.SourceProjectID, true)
		encoded = m03StringField(encoded, "sourceEpoch", f.SourceEpoch, true)
		encoded = m03StringField(encoded, "sourceId", f.SourceID, true)
		encoded = m03NumberField(encoded, "sourceRevision", &f.SourceRevision)
		encoded = m03StringField(encoded, "sourceContentHash", f.SourceContentHash, true)
		encoded = m03NumberField(encoded, "spanStart", f.SpanStart)
		encoded = m03NumberField(encoded, "spanEnd", f.SpanEnd)
		encoded = m03NullableStringField(encoded, "spanUnit", f.SpanUnit)
		encoded = m03NullableStringField(encoded, "excerptHash", f.ExcerptHash)
		encoded = m03NumberField(encoded, "occurrence", f.Occurrence)
		encoded = m03StringField(encoded, "category", f.Category, true)
		encoded = m03NullableStringField(encoded, "targetId", f.TargetID)
		encoded = m03StringField(encoded, "kind", f.Kind, true)
		encoded = m03StringField(encoded, "problemText", f.ProblemText, true)
		encoded = m03NullableStringField(encoded, "expectedText", f.ExpectedText)
		encoded = append(encoded, '}')
	}
	encoded = append(encoded, ']')
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

// VerifyM03FindingSetHash checks persisted immutable finding identities against
// a cycle's receipt. It does not verify generation or recheck receipts.
func VerifyM03FindingSetHash(ctx context.Context, tx *sql.Tx, cycleID, expectedHash string) error {
	rows, err := tx.QueryContext(ctx, `SELECT finding_id, source_project_id, source_epoch, source_id, source_revision,
		source_content_hash, span_start, span_end, span_unit, excerpt_hash, occurrence,
		category, target_id, kind, problem_text, expected_text
		FROM review_findings WHERE cycle_id=? ORDER BY finding_id`, cycleID)
	if err != nil {
		return err
	}
	var findings []M03FindingIdentity
	for rows.Next() {
		var f M03FindingIdentity
		var start, end, occurrence sql.NullInt64
		var unit, excerpt, target, expected sql.NullString
		if err := rows.Scan(&f.FindingID, &f.SourceProjectID, &f.SourceEpoch, &f.SourceID, &f.SourceRevision,
			&f.SourceContentHash, &start, &end, &unit, &excerpt, &occurrence,
			&f.Category, &target, &f.Kind, &f.ProblemText, &expected); err != nil {
			rows.Close()
			return err
		}
		if start.Valid {
			f.SpanStart = &start.Int64
		}
		if end.Valid {
			f.SpanEnd = &end.Int64
		}
		if unit.Valid {
			f.SpanUnit = &unit.String
		}
		if excerpt.Valid {
			f.ExcerptHash = &excerpt.String
		}
		if occurrence.Valid {
			f.Occurrence = &occurrence.Int64
		}
		if target.Valid {
			f.TargetID = &target.String
		}
		if expected.Valid {
			f.ExpectedText = &expected.String
		}
		findings = append(findings, f)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if CanonicalM03FindingSetHash(findings) != expectedHash {
		return errors.New("M03 finding set hash differs")
	}
	return nil
}

func m03FieldName(dst []byte, name string, comma bool) []byte {
	if comma {
		dst = append(dst, ',')
	}
	dst = appendJSString(dst, name)
	return append(dst, ':')
}

func m03StringField(dst []byte, name, value string, comma bool) []byte {
	return appendJSString(m03FieldName(dst, name, comma), value)
}

func m03NullableStringField(dst []byte, name string, value *string) []byte {
	dst = m03FieldName(dst, name, true)
	if value == nil {
		return append(dst, "null"...)
	}
	return appendJSString(dst, *value)
}

func m03NumberField(dst []byte, name string, value *int64) []byte {
	dst = m03FieldName(dst, name, true)
	if value == nil {
		return append(dst, "null"...)
	}
	return strconv.AppendInt(dst, *value, 10)
}
