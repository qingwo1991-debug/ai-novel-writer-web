package store

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// Native ports of the three writers the finalization-generation commit needs
// for the chapter_notes step, which the transport adapter still refuses
// (internal/transport/summary.go:45 db:continuity-save-finalized and
// internal/transport/blueprints.go:26 db:blueprint-update-notes):
//
//	BuildFinalizedContinuityFacts  src/shared/finalized-continuity-facts.ts:57
//	(*Summary).SaveFinalizedContinuity  electron/repositories/summary-repository.ts:570
//	(*Blueprints).UpdateNotes  electron/repositories/blueprint-repository.ts:815
//
// These are write paths, unlike the read-only Summary and Blueprints methods
// beside them, so they live in their own file. They only run behind the
// finalization admission in cmd/novel-server; the IPC adapter's own refusal is
// unchanged.

var (
	// summary-repository.ts:583
	ErrContinuityProjectionInvalid = errors.New("CONTINUITY_PROJECTION_INVALID")
	// summary-repository.ts:591
	ErrContinuityEvidenceNotInSource = errors.New("CONTINUITY_EVIDENCE_NOT_IN_SOURCE")
	// summary-repository.ts:91 (normalizedFacts)
	ErrContinuityFactsInvalid = errors.New("CONTINUITY_FACTS_INVALID")
)

// FinalizedCharacterReference mirrors FinalizedCharacterReference
// (src/shared/finalized-continuity.ts:50-53).
type FinalizedCharacterReference struct {
	CharacterID         string `json:"characterId"`
	DisplayNameSnapshot string `json:"displayNameSnapshot"`
}

// FinalizedContinuityFact mirrors FinalizedContinuityFact
// (src/shared/finalized-continuity.ts:40-48).
type FinalizedContinuityFact struct {
	Category      string                        `json:"category"`
	Entities      []string                      `json:"entities"`
	Statement     string                        `json:"statement"`
	SourceChapter int64                         `json:"sourceChapter"`
	Evidence      string                        `json:"evidence"`
	CharacterRefs []FinalizedCharacterReference `json:"characterRefs,omitempty"`
}

// SaveFinalizedContinuityRequest mirrors SaveFinalizedContinuityRequest
// (src/shared/finalized-continuity.ts:248-257).
type SaveFinalizedContinuityRequest struct {
	DraftID             int64
	ChapterNumber       int64
	ChapterNotes        string
	Facts               []FinalizedContinuityFact
	ProjectionGeneration int64
	Source              FinalizedCharacterSourceIdentity
}

var finalizedContinuityFactCategories = regexp.MustCompile(`(?i)(?:角色|状态|持有|受伤|位于|死亡|身亡|牺牲|去世|character|holds?|injur|location|dead|died|deceased)`)
var finalizedContinuityTimelineCategories = regexp.MustCompile(`(?i)(?:时间|当日|翌日|多年|之前|之后|timeline|before|after|years?)`)
var finalizedContinuityOpenThreadCategories = regexp.MustCompile(`(?i)(?:伏笔|悬念|承诺|未解|线索|promise|unresolved|clue|mystery)`)

// finalizedContinuityCategory is factCategory (finalized-continuity-facts.ts:5).
func finalizedContinuityCategory(statement string) string {
	if finalizedContinuityFactCategories.MatchString(statement) {
		return "character-state"
	}
	if finalizedContinuityTimelineCategories.MatchString(statement) {
		return "timeline"
	}
	if finalizedContinuityOpenThreadCategories.MatchString(statement) {
		return "open-thread"
	}
	return "plot"
}

// finalizedContinuityBigrams is textBigrams (finalized-continuity-facts.ts:12):
// the adjacent rune pairs of every letter/number group, lowercased. A group of
// one rune contributes itself.
func finalizedContinuityBigrams(value string) map[string]bool {
	result := make(map[string]bool)
	var group []rune
	flush := func() {
		if len(group) == 0 {
			return
		}
		if len(group) < 2 {
			result[string(group[0])] = true
		} else {
			for index := 0; index+1 < len(group); index++ {
				result[string(group[index])+string(group[index+1])] = true
			}
		}
		group = group[:0]
	}
	for _, character := range strings.ToLower(value) {
		if unicode.IsLetter(character) || unicode.IsNumber(character) {
			group = append(group, character)
			continue
		}
		flush()
	}
	flush()
	return result
}

// finalizedContinuitySentences splits prose the way the reference's splitter
// does (finalized-continuity-facts.ts:23): after a sentence terminator, or at a
// newline run.
func finalizedContinuitySentences(content string) []string {
	var sentences []string
	var current strings.Builder
	flush := func() {
		text := strings.TrimSpace(current.String())
		if text != "" {
			sentences = append(sentences, text)
		}
		current.Reset()
	}
	for _, character := range content {
		switch character {
		case '\n':
			flush()
			continue
		}
		current.WriteRune(character)
		switch character {
		case '。', '！', '？', '.', '!', '?':
			flush()
		}
	}
	flush()
	return sentences
}

// finalizedContinuityEvidenceExcerpt is evidenceExcerpt
// (finalized-continuity-facts.ts:22): the source sentence whose bigram signal
// set best matches the statement after its entities are removed.
func finalizedContinuityEvidenceExcerpt(content, statement string, entities []string) string {
	sentences := finalizedContinuitySentences(content)
	factEntities := make([]string, 0, len(entities))
	for _, entity := range entities {
		if strings.Contains(statement, entity) {
			factEntities = append(factEntities, entity)
		}
	}
	statementWithoutEntities := statement
	sorted := append([]string(nil), factEntities...)
	sort.Slice(sorted, func(i, j int) bool { return len(sorted[i]) > len(sorted[j]) })
	for _, entity := range sorted {
		statementWithoutEntities = strings.ReplaceAll(statementWithoutEntities, entity, " ")
	}
	signals := finalizedContinuityBigrams(statementWithoutEntities)
	signalList := make([]string, 0, len(signals))
	for signal := range signals {
		signalList = append(signalList, signal)
	}
	candidates := sentences
	if len(factEntities) > 0 {
		candidates = nil
		for _, sentence := range sentences {
			for _, entity := range factEntities {
				if strings.Contains(sentence, entity) {
					candidates = append(candidates, sentence)
					break
				}
			}
		}
	}
	type rankedSentence struct {
		sentence  string
		score     int
		supported bool
	}
	ranked := make([]rankedSentence, 0, len(candidates))
	for _, sentence := range candidates {
		sentenceSignals := finalizedContinuityBigrams(sentence)
		matchedIndexes := make([]int, 0, len(signalList))
		for index, signal := range signalList {
			if sentenceSignals[signal] {
				matchedIndexes = append(matchedIndexes, index)
			}
		}
		independentlySupported := false
		for matchIndex := 1; matchIndex < len(matchedIndexes); matchIndex++ {
			if matchedIndexes[matchIndex]-matchedIndexes[matchIndex-1] > 1 {
				independentlySupported = true
				break
			}
		}
		ranked = append(ranked, rankedSentence{
			sentence:  sentence,
			score:     len(matchedIndexes),
			supported: len(matchedIndexes) == len(signalList) || independentlySupported,
		})
	}
	sort.SliceStable(ranked, func(i, j int) bool { return ranked[i].score > ranked[j].score })
	minimumScore := 2
	if len(factEntities) > 0 {
		minimumScore = 1
	}
	for _, candidate := range ranked {
		if candidate.score >= minimumScore && candidate.supported {
			return candidate.sentence
		}
	}
	return ""
}

// BuildFinalizedContinuityFacts ports src/shared/finalized-continuity-facts.ts:57.
func BuildFinalizedContinuityFacts(chapterNumber int64, chapterNotes, finalizedContent string, chapterEntities []string, identity *FinalizedCharacterContext) []FinalizedContinuityFact {
	seenEntities := make(map[string]bool)
	entities := make([]string, 0, len(chapterEntities)+len(identityCharacters(identity)))
	for _, entity := range append(append([]string(nil), chapterEntities...), identityCharacters(identity)...) {
		text := strings.TrimSpace(entity)
		if text == "" || seenEntities[text] {
			continue
		}
		seenEntities[text] = true
		entities = append(entities, text)
	}
	statements := finalizedContinuityStatements(chapterNotes)
	facts := make([]FinalizedContinuityFact, 0, len(statements))
	for _, statement := range statements {
		factEntities := make([]string, 0, 8)
		for _, entity := range entities {
			if strings.Contains(statement, entity) {
				factEntities = append(factEntities, entity)
				if len(factEntities) == 8 {
					break
				}
			}
		}
		if identity != nil {
			ambiguous := false
			for _, entity := range factEntities {
				count := 0
				for _, character := range identity.Characters {
					if character.DisplayNameSnapshot == entity {
						count++
					}
				}
				if count > 1 {
					ambiguous = true
					break
				}
			}
			if ambiguous {
				continue
			}
		}
		evidence := finalizedContinuityEvidenceExcerpt(finalizedContent, statement, factEntities)
		if evidence == "" {
			continue
		}
		fact := FinalizedContinuityFact{
			Category:      finalizedContinuityCategory(statement),
			Entities:      factEntities,
			Statement:     statement,
			SourceChapter: chapterNumber,
			Evidence:      evidence,
		}
		if identity != nil {
			for _, entity := range factEntities {
				var matched *FinalizedCharacterContextCharacter
				matches := 0
				for index := range identity.Characters {
					if identity.Characters[index].DisplayNameSnapshot == entity {
						matches++
						matched = &identity.Characters[index]
					}
				}
				if matches == 1 && matched != nil {
					fact.CharacterRefs = append(fact.CharacterRefs, FinalizedCharacterReference{
						CharacterID:         matched.CharacterID,
						DisplayNameSnapshot: matched.DisplayNameSnapshot,
					})
				}
			}
		}
		facts = append(facts, fact)
		if len(facts) == 12 {
			break
		}
	}
	return facts
}

// finalizedContinuityStatements ports the statement split of
// finalized-continuity-facts.ts:65-68: split on newlines and sentence
// terminators, then strip list markers.
var finalizedContinuityListMarker = regexp.MustCompile(`^\s*(?:[-*•]|\d+[.)、])\s*`)

func finalizedContinuityStatements(chapterNotes string) []string {
	var statements []string
	for _, sentence := range finalizedContinuitySentences(chapterNotes) {
		text := finalizedContinuityListMarker.ReplaceAllString(sentence, "")
		text = strings.TrimSpace(text)
		if text != "" {
			statements = append(statements, text)
		}
	}
	return statements
}

func identityCharacters(identity *FinalizedCharacterContext) []string {
	if identity == nil {
		return nil
	}
	names := make([]string, 0, len(identity.Characters))
	for _, character := range identity.Characters {
		names = append(names, character.DisplayNameSnapshot)
	}
	return names
}

// SaveFinalizedContinuity ports SummaryRepository.saveFinalizedContinuity
// (summary-repository.ts:570) for the chapter_notes commit. It is one
// read-source + evidence + generation CAS + upsert inside a single immediate
// transaction, exactly like the reference.
func (s *Summary) SaveFinalizedContinuity(ctx context.Context, input SaveFinalizedContinuityRequest) error {
	chapterNotes := strings.TrimSpace(input.ChapterNotes)
	if input.DraftID < 1 || input.ChapterNumber < 1 || input.ProjectionGeneration < 0 || chapterNotes == "" {
		return ErrContinuityProjectionInvalid
	}
	facts, err := normalizedFinalizedContinuityFacts(input.Facts, input.ChapterNumber)
	if err != nil {
		return err
	}
	encodedFacts, err := plotJSON(facts)
	if err != nil {
		return ErrContinuityFactsInvalid
	}
	return NewReviewRevisions(s.db).immediate(ctx, func(conn *sql.Conn) error {
		rows, err := rrMaps(ctx, conn, `SELECT d.id AS draftId,d.chapter_number AS chapterNumber,d.status,
		 c.body AS content,o.finalization_id AS finalizationId,o.content_hash AS contentHash,
		 o.content_snapshot AS contentSnapshot
		 FROM drafts d JOIN contents c ON c.id=d.content_id JOIN finalization_outbox o ON o.draft_id=d.id
		 WHERE d.id=?`, input.DraftID)
		if err != nil {
			return err
		}
		if len(rows) == 0 || rows[0]["status"] != "finalized" {
			return ErrContinuitySourceStale
		}
		row := rows[0]
		// The same frozen-source proof readFinalizedSourceFromDb performs
		// (finalized_character_context.go:305-345): a non-empty finalization id,
		// a 64-hex content hash, a body that equals its snapshot and a snapshot
		// that hashes to the receipt.
		finalizationID, finalizationOK := row["finalizationId"].(string)
		content, contentOK := row["content"].(string)
		snapshot, snapshotOK := row["contentSnapshot"].(string)
		contentHash := summaryText(row["contentHash"])
		if !finalizationOK || summaryTrim(finalizationID) == "" || !summaryHashValid(row["contentHash"]) ||
			!contentOK || !snapshotOK || content != snapshot || summaryHash(snapshot) != contentHash {
			return ErrContinuitySourceStale
		}
		source := FinalizedCharacterSourceIdentity{
			DraftID:        input.DraftID,
			FinalizationID: finalizationID,
			ChapterNumber:  input.ChapterNumber,
			ContentHash:    contentHash,
		}
		if source != input.Source || input.ChapterNumber != input.Source.ChapterNumber {
			return ErrContinuitySourceStale
		}
		for _, fact := range facts {
			if !strings.Contains(content, fact.Evidence) {
				return ErrContinuityEvidenceNotInSource
			}
		}
		meta, err := rrMaps(ctx, conn, "SELECT generation FROM continuity_projection_meta WHERE id='main'")
		if err != nil {
			return err
		}
		if len(meta) == 0 {
			return ErrContinuityGenerationStale
		}
		generation, ok := rrInteger(meta[0]["generation"], 0)
		if !ok || generation != input.ProjectionGeneration {
			return ErrContinuityGenerationStale
		}
		updated, err := conn.ExecContext(ctx, `UPDATE summary_snapshots
		 SET chapter_number=?,chapter_notes=?,continuity_facts=?,source_finalization_id=?,
		 source_content_hash=?,projection_generation=?,created_at=datetime('now')
		 WHERE draft_id=?`,
			input.ChapterNumber, chapterNotes, encodedFacts, input.Source.FinalizationID,
			input.Source.ContentHash, input.ProjectionGeneration, input.DraftID)
		if err != nil {
			return err
		}
		changed, err := updated.RowsAffected()
		if err != nil {
			return err
		}
		if changed == 0 {
			if _, err := conn.ExecContext(ctx, `INSERT INTO summary_snapshots (
			 draft_id,chapter_number,chapter_notes,continuity_facts,source_finalization_id,
			 source_content_hash,projection_generation) VALUES (?,?,?,?,?,?,?)`,
				input.DraftID, input.ChapterNumber, chapterNotes, encodedFacts, input.Source.FinalizationID,
				input.Source.ContentHash, input.ProjectionGeneration); err != nil {
				return err
			}
		}
		return nil
	})
}

// normalizedFinalizedContinuityFacts is normalizedFacts (summary-repository.ts:90).
func normalizedFinalizedContinuityFacts(value []FinalizedContinuityFact, chapterNumber int64) ([]FinalizedContinuityFact, error) {
	if len(value) > 12 {
		return nil, ErrContinuityFactsInvalid
	}
	out := make([]FinalizedContinuityFact, 0, len(value))
	for _, fact := range value {
		entities := make([]string, 0, len(fact.Entities))
		for _, entity := range fact.Entities {
			text := strings.TrimSpace(entity)
			if text == "" || len([]rune(text)) > 80 {
				return nil, ErrContinuityFactsInvalid
			}
			entities = append(entities, text)
		}
		statement := strings.TrimSpace(fact.Statement)
		evidence := strings.TrimSpace(fact.Evidence)
		if !validFinalizedContinuityCategory(fact.Category) || fact.SourceChapter != chapterNumber ||
			len(entities) > 8 || statement == "" || evidence == "" {
			return nil, ErrContinuityFactsInvalid
		}
		out = append(out, FinalizedContinuityFact{
			Category:      fact.Category,
			Entities:      dedupeStrings(entities),
			Statement:     statement,
			SourceChapter: chapterNumber,
			Evidence:      evidence,
			CharacterRefs: fact.CharacterRefs,
		})
	}
	return out, nil
}

func validFinalizedContinuityCategory(category string) bool {
	switch category {
	case "character-state", "timeline", "open-thread", "plot":
		return true
	}
	return false
}

func dedupeStrings(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

// UpdateNotes ports BlueprintRepository.updateNotes (blueprint-repository.ts:815):
// UPDATE blueprints SET notes, notes_updated_at, updated_at WHERE chapter_number.
func (b *Blueprints) UpdateNotes(ctx context.Context, chapterNumber int64, notes string) (bool, error) {
	if b == nil || b.db == nil {
		return false, ErrProjectNotOpen
	}
	result, err := b.db.ExecContext(ctx,
		"UPDATE blueprints SET notes=?,notes_updated_at=datetime('now'),updated_at=datetime('now') WHERE chapter_number=?",
		notes, chapterNumber)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return changed > 0, nil
}
