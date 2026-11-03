package knowledgequery

import (
	"context"
	"encoding/json"
	"os"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/knowledgecopy"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/knowledgedomain"
	"golang.org/x/text/cases"
	"golang.org/x/text/language"
)

// SearchCapture is an immutable, bounded FTS projection of an already admitted
// typed snapshot. Its construction still requires the caller's writer exclusion,
// source/copy equality proof, and final proof. It does not authorize vector
// retrieval: a query vector must be matched to the active embedding space by a
// separately admitted native query implementation before this FTS fallback.
type SearchCapture struct {
	chunks        []searchChunk
	stale         map[string]bool
	hasChapter    bool
	hasCorpusKind bool
}

type searchChunk struct {
	fileName, text, docID, corpusKind string
	chapterNumber                     float64
	chapterValid                      bool
}

type SearchResult struct {
	Text     string  `json:"text"`
	Score    float64 `json:"score"`
	FileName string  `json:"fileName"`
}

type ChapterScope struct{ From, To int64 }

// NewSearchCapture first validates the complete domain and managed copy state
// through New, then copies only canonical chunk fields. It retains no native
// handle, root, input slice, or caller map.
func NewSearchCapture(ctx context.Context, snapshot knowledgedomain.Snapshot, root *os.Root, budget knowledgecopy.Budget) (*SearchCapture, error) {
	view, err := New(ctx, snapshot, root, budget)
	if err != nil {
		return nil, err
	}
	out := &SearchCapture{chunks: []searchChunk{}, stale: map[string]bool{}}
	for _, doc := range view.documents {
		if strings.HasPrefix(doc.filePath, "knowledge-copy:") && view.copies[doc.id].status != "current" {
			out.stale[doc.id] = true
		}
	}
	for _, table := range snapshot.Tables {
		if table.Name != "chunks" {
			continue
		}
		for _, field := range table.Schema {
			if field.Name == "chapterNumber" {
				out.hasChapter = true
			}
			if field.Name == "corpusKind" {
				out.hasCorpusKind = true
			}
		}
		for _, row := range table.Records {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			chunk := searchChunk{fileName: row["fileName"].(string), text: row["text"].(string), docID: row["docId"].(string)}
			if out.hasCorpusKind {
				chunk.corpusKind = row["corpusKind"].(string)
			}
			if out.hasChapter {
				chunk.chapterNumber, chunk.chapterValid = searchChapterNumber(row["chapterNumber"])
			}
			out.chunks = append(out.chunks, chunk)
		}
		break
	}
	return out, nil
}

// SearchFTS reproduces the original canonical-table fallback's term selection,
// row rank, stable ordering, (fileName,text) dedupe and topK. Bounds are smaller
// than the domain capture: at most 8 query terms and 100 returned results.
func (c *SearchCapture) SearchFTS(ctx context.Context, query string, topK int, scope *ChapterScope, excludedCorpusKinds []string) ([]SearchResult, error) {
	if err := check(ctx); err != nil {
		return nil, err
	}
	if c == nil || topK < 0 || topK > 100 || len(query) > 32*1024 || !utf8.ValidString(query) || len(excludedCorpusKinds) > 32 {
		return nil, ErrInput
	}
	if scope != nil && (scope.From < 0 || scope.To < scope.From || scope.To > 9007199254740991) {
		return nil, ErrInput
	}
	excluded := make(map[string]bool, len(excludedCorpusKinds))
	for _, kind := range excludedCorpusKinds {
		if len(kind) > 1024 || !utf8.ValidString(kind) {
			return nil, ErrInput
		}
		excluded[kind] = true
	}
	if topK == 0 {
		return []SearchResult{}, nil
	}
	terms := searchTerms(query)
	lowercase := cases.Lower(language.Und)
	type ranked struct {
		result    SearchResult
		relevance int
	}
	selected := make([]ranked, 0, len(c.chunks))
	for _, chunk := range c.chunks {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if c.stale[chunk.docID] || (scope != nil && c.hasChapter && (!chunk.chapterValid || chunk.chapterNumber < float64(scope.From) || chunk.chapterNumber > float64(scope.To))) || (c.hasCorpusKind && excluded[chunk.corpusKind]) {
			continue
		}
		// Lance's SQL LIKE is applied to original-cased terms before the JS
		// relevance pass lowercases the row. Keep those two steps separate.
		matched := len(terms) == 0
		for _, term := range terms {
			if strings.Contains(chunk.text, term) {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		lower := lowercase.String(chunk.text)
		score := 0
		for i, term := range terms {
			if strings.Contains(lower, lowercase.String(term)) {
				score += len(terms) - i
			}
		}
		selected = append(selected, ranked{result: SearchResult{Text: chunk.text, Score: 0.5, FileName: chunk.fileName}, relevance: score})
	}
	sort.SliceStable(selected, func(i, j int) bool { return selected[i].relevance > selected[j].relevance })
	out := make([]SearchResult, 0, min(topK, len(selected)))
	seen := make(map[[2]string]bool, len(selected))
	for _, candidate := range selected {
		key := [2]string{candidate.result.FileName, candidate.result.Text}
		if !seen[key] {
			seen[key] = true
			out = append(out, candidate.result)
			if len(out) == topK {
				break
			}
		}
	}
	return out, nil
}

func searchChapterNumber(value any) (float64, bool) {
	switch number := value.(type) {
	case json.Number:
		parsed, err := strconv.ParseFloat(string(number), 64)
		return parsed, err == nil
	case float64:
		return number, true
	case int:
		return float64(number), true
	case int32:
		return float64(number), true
	case int64:
		return float64(number), true
	}
	return 0, false
}

func searchTerms(query string) []string {
	raw := []string{}
	var run strings.Builder
	flush := func() {
		if run.Len() > 0 {
			raw = append(raw, run.String())
			run.Reset()
		}
	}
	for _, r := range query {
		if unicode.IsLetter(r) || unicode.IsNumber(r) || r == '-' {
			run.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	meaningful := []string{}
	for _, term := range raw {
		if utf8.RuneCountInString(term) >= 2 {
			meaningful = append(meaningful, term)
		}
	}
	if len(meaningful) > 0 {
		raw = meaningful
	}
	seen := map[string]bool{}
	lowercase := cases.Lower(language.Und)
	terms := make([]string, 0, min(8, len(raw)))
	for _, term := range raw {
		key := lowercase.String(term)
		if !seen[key] {
			seen[key] = true
			terms = append(terms, term)
			if len(terms) == 8 {
				break
			}
		}
	}
	return terms
}
