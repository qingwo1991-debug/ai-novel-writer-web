package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
)

const overviewCurrentStatuses = "'draft','revised','reviewed','finalized'"
const overviewMaxSafeInteger = int64(9007199254740991)

type ProjectOverviewStage struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Count  int    `json:"count"`
}

type ProjectOverview struct {
	State             string                 `json:"state"`
	ProjectID         string                 `json:"projectId"`
	Revision          string                 `json:"revision"`
	Name              string                 `json:"name"`
	TotalWords        int64                  `json:"totalWords"`
	Characters        int64                  `json:"characters"`
	FinalizedChapters int                    `json:"finalizedChapters"`
	Stages            []ProjectOverviewStage `json:"stages"`
	SetupReady        bool                   `json:"setupReady"`
	PlannedChapters   int64                  `json:"plannedChapters"`
	Excerpt           string                 `json:"excerpt,omitempty"`
}

// ReadCurrentProjectOverviewTx reproduces the six stage facts read by the
// desktop's project-peek aggregate. It borrows an admitted read transaction;
// callers retain the source proof and exact project lease through encoding.
// A missing core row is unavailable, not an initialized empty project.
func ReadCurrentProjectOverviewTx(ctx context.Context, tx *sql.Tx, projectID string) (*ProjectOverview, error) {
	if ctx == nil || tx == nil || projectID == "" {
		return nil, ErrProjectNotOpen
	}
	core, err := ReadProjectCoreTx(ctx, tx)
	if err != nil || core == nil {
		return nil, err
	}
	text := func(key string) string { value, _ := core[key].(string); return value }
	filled := func(key string) bool { return summaryTrim(text(key)) != "" }
	countFilled := func(keys ...string) int {
		count := 0
		for _, key := range keys {
			if filled(key) {
				count++
			}
		}
		return count
	}
	configuration := countFilled("genre", "subGenre", "targetAudience", "coreOutline", "worldSetting", "goldenFinger", "protagonistProfile", "globalGuidance", "writingStyle")
	architecture := countFilled("premise", "worldbuilding", "charactersArch", "synopsis")
	planned := overviewSafeInteger(core["totalChapters"])
	wordsPerChapter := overviewSafeInteger(core["wordsPerChapter"])
	setupReady := filled("coreOutline") && filled("genre") && filled("targetAudience") && planned > 0 && wordsPerChapter > 0

	blueprints, err := overviewChapters(ctx, tx, "SELECT DISTINCT chapter_number FROM blueprints")
	if err != nil {
		return nil, err
	}
	currentRows, err := tx.QueryContext(ctx, `SELECT d.chapter_number,d.word_count FROM drafts d
 WHERE d.status IN (`+overviewCurrentStatuses+`) AND NOT EXISTS (
 SELECT 1 FROM drafts newer WHERE newer.chapter_number=d.chapter_number
 AND newer.status IN (`+overviewCurrentStatuses+`)
 AND (newer.version>d.version OR newer.version=d.version AND newer.id>d.id))`)
	if err != nil {
		return nil, err
	}
	var draftNumbers []int64
	var totalWords int64
	for currentRows.Next() {
		var chapter, words sql.NullInt64
		if err := currentRows.Scan(&chapter, &words); err != nil {
			_ = currentRows.Close()
			return nil, err
		}
		if chapter.Valid && chapter.Int64 > 0 && chapter.Int64 <= overviewMaxSafeInteger {
			draftNumbers = append(draftNumbers, chapter.Int64)
		}
		if words.Valid && words.Int64 >= 0 && words.Int64 <= overviewMaxSafeInteger {
			if words.Int64 > (1<<63-1)-totalWords {
				_ = currentRows.Close()
				return nil, errors.New("PROJECT_OVERVIEW_WORDS_OVERFLOW")
			}
			totalWords += words.Int64
		}
	}
	err = currentRows.Err()
	_ = currentRows.Close()
	if err != nil {
		return nil, err
	}
	drafts := overviewUniqueChapters(draftNumbers)
	reviews, err := overviewChapters(ctx, tx, "SELECT DISTINCT d.chapter_number FROM reviews r JOIN drafts d ON d.id=r.base_draft_id")
	if err != nil {
		return nil, err
	}
	finalized, finalFacts, err := overviewFinalized(ctx, tx)
	if err != nil {
		return nil, err
	}
	var characters int64
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM characters WHERE retired=0").Scan(&characters); err != nil {
		return nil, err
	}
	if characters < 0 || characters > overviewMaxSafeInteger {
		characters = 0
	}
	stages := []ProjectOverviewStage{
		{ID: "configuration", Status: overviewFieldStatus(configuration, 9), Count: configuration},
		{ID: "architecture", Status: overviewFieldStatus(architecture, 4), Count: architecture},
		{ID: "blueprint", Status: overviewPlannedStatus(blueprints, planned), Count: len(blueprints)},
		{ID: "drafting", Status: overviewPlannedStatus(drafts, planned), Count: len(drafts)},
		{ID: "review", Status: overviewPlannedStatus(reviews, planned), Count: len(reviews)},
		{ID: "finalization", Status: overviewPlannedStatus(finalized, planned), Count: len(finalized)},
	}
	var excerpt sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT c.body FROM drafts d JOIN contents c ON c.id=d.content_id
 WHERE d.status IN (`+overviewCurrentStatuses+`) AND NOT EXISTS (
 SELECT 1 FROM drafts newer WHERE newer.chapter_number=d.chapter_number
 AND newer.status IN (`+overviewCurrentStatuses+`)
 AND (newer.version>d.version OR newer.version=d.version AND newer.id>d.id))
 ORDER BY d.chapter_number DESC,d.version DESC,d.id DESC LIMIT 1`).Scan(&excerpt)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	revisionFacts := struct {
		ProjectID  string                 `json:"projectId"`
		Core       map[string]any         `json:"core"`
		Words      int64                  `json:"totalWords"`
		Characters int64                  `json:"characters"`
		Stages     []ProjectOverviewStage `json:"stages"`
		Finalized  []overviewFinalFact    `json:"finalized"`
	}{projectID, core, totalWords, characters, stages, finalFacts}
	revisionBytes, err := json.Marshal(revisionFacts)
	if err != nil {
		return nil, err
	}
	hash := sha256.Sum256(revisionBytes)
	result := &ProjectOverview{State: "ready", ProjectID: projectID, Revision: hex.EncodeToString(hash[:]), Name: summaryTrim(text("projectName")), TotalWords: totalWords, Characters: characters, FinalizedChapters: len(finalized), Stages: stages, SetupReady: setupReady, PlannedChapters: max(0, planned)}
	if excerpt.Valid {
		runes := []rune(summaryTrim(excerpt.String))
		if len(runes) > 240 {
			runes = runes[len(runes)-240:]
		}
		result.Excerpt = string(runes)
	}
	return result, nil
}

func overviewSafeInteger(value any) int64 {
	var number int64
	switch value := value.(type) {
	case int64:
		number = value
	case int:
		number = int64(value)
	default:
		return 0
	}
	if number < -overviewMaxSafeInteger || number > overviewMaxSafeInteger {
		return 0
	}
	return number
}

func overviewChapters(ctx context.Context, tx *sql.Tx, query string) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var numbers []int64
	for rows.Next() {
		var chapter sql.NullInt64
		if err := rows.Scan(&chapter); err != nil {
			return nil, err
		}
		if chapter.Valid && chapter.Int64 > 0 && chapter.Int64 <= overviewMaxSafeInteger {
			numbers = append(numbers, chapter.Int64)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return overviewUniqueChapters(numbers), nil
}

func overviewUniqueChapters(numbers []int64) []int64 {
	seen := make(map[int64]struct{}, len(numbers))
	for _, number := range numbers {
		seen[number] = struct{}{}
	}
	out := make([]int64, 0, len(seen))
	for number := range seen {
		out = append(out, number)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func overviewFieldStatus(count, complete int) string {
	if count == complete {
		return "completed"
	}
	if count > 0 {
		return "in-progress"
	}
	return "not-started"
}

func overviewPlannedStatus(chapters []int64, planned int64) string {
	if planned <= 0 {
		if len(chapters) > 0 {
			return "in-progress"
		}
		return "unknown"
	}
	if len(chapters) == 0 {
		return "not-started"
	}
	covered := make(map[int64]struct{}, len(chapters))
	for _, chapter := range chapters {
		if chapter <= planned {
			covered[chapter] = struct{}{}
		}
	}
	if int64(len(covered)) == planned {
		return "completed"
	}
	return "in-progress"
}

type overviewFinalFact struct {
	DraftID        int64  `json:"draftId"`
	Chapter        int64  `json:"chapterNumber"`
	Version        int64  `json:"version"`
	FinalizationID string `json:"finalizationId"`
	ContentHash    string `json:"contentHash"`
}

func overviewFinalized(ctx context.Context, tx *sql.Tx) ([]int64, []overviewFinalFact, error) {
	rows, err := tx.QueryContext(ctx, `SELECT d.id,d.chapter_number,d.version,c.body,o.finalization_id,o.chapter_number,o.content_hash,o.content_snapshot
 FROM drafts d JOIN contents c ON c.id=d.content_id LEFT JOIN finalization_outbox o ON o.draft_id=d.id
 WHERE d.status='finalized' AND NOT EXISTS (
 SELECT 1 FROM drafts newer WHERE newer.chapter_number=d.chapter_number AND newer.status='finalized'
 AND (newer.version>d.version OR newer.version=d.version AND newer.id>d.id))
 ORDER BY d.chapter_number,d.version DESC,d.id DESC`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var facts []overviewFinalFact
	var chapters []int64
	for rows.Next() {
		var draftID, chapter, version, outboxChapter sql.NullInt64
		var body, finalID, hash, snapshot sql.NullString
		if err := rows.Scan(&draftID, &chapter, &version, &body, &finalID, &outboxChapter, &hash, &snapshot); err != nil {
			return nil, nil, err
		}
		if !draftID.Valid || !chapter.Valid || !version.Valid || !body.Valid || !finalID.Valid || !outboxChapter.Valid || !hash.Valid || !snapshot.Valid || draftID.Int64 <= 0 || chapter.Int64 <= 0 || version.Int64 <= 0 || draftID.Int64 > overviewMaxSafeInteger || chapter.Int64 > overviewMaxSafeInteger || version.Int64 > overviewMaxSafeInteger || summaryTrim(body.String) == "" || summaryTrim(finalID.String) == "" || outboxChapter.Int64 != chapter.Int64 || snapshot.String != body.String {
			continue
		}
		digest := sha256.Sum256([]byte(body.String))
		if hash.String != hex.EncodeToString(digest[:]) {
			continue
		}
		facts = append(facts, overviewFinalFact{draftID.Int64, chapter.Int64, version.Int64, finalID.String, hash.String})
		chapters = append(chapters, chapter.Int64)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	return overviewUniqueChapters(chapters), facts, nil
}
