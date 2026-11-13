package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"unicode/utf16"
)

var (
	ErrPlotTreeProjectCoreMissing = errors.New("PLOT_TREE_PROJECT_CORE_MISSING")
	ErrPlotTreeSourcesChanged     = errors.New("PLOT_TREE_SOURCES_CHANGED")
	ErrPlotTreeSaveFailed         = errors.New("PLOT_TREE_SAVE_FAILED")
	ErrPlotTreeClearFailed        = errors.New("PLOT_TREE_CLEAR_FAILED")
)

// PlotTree is a borrowed admitted project. Save is an isolated service primitive,
// NOT browser generation admission; the public save RPC stays explicitly denied.
type PlotTree struct{ db *sql.DB }

func NewPlotTree(db *sql.DB) *PlotTree { return &PlotTree{db: db} }

type plotField struct {
	key   string
	value any
}
type plotObject []plotField

func plotOrdered(row map[string]any, keys ...string) plotObject {
	out := plotObject{}
	for _, key := range keys {
		if value, ok := row[key]; ok {
			out = append(out, plotField{key, value})
		}
	}
	return out
}

// JavaScript JSON.stringify does not HTML-escape or escape U+2028/U+2029. Never
// hash Go map order or encoder formatting as the author's original source ID.
func plotQuote(text string) string {
	var out strings.Builder
	out.WriteByte('"')
	for _, r := range text {
		switch r {
		case '"':
			out.WriteString(`\"`)
		case '\\':
			out.WriteString(`\\`)
		case '\b':
			out.WriteString(`\b`)
		case '\f':
			out.WriteString(`\f`)
		case '\n':
			out.WriteString(`\n`)
		case '\r':
			out.WriteString(`\r`)
		case '\t':
			out.WriteString(`\t`)
		default:
			if r < 32 {
				fmt.Fprintf(&out, `\u%04x`, r)
			} else {
				out.WriteRune(r)
			}
		}
	}
	out.WriteByte('"')
	return out.String()
}
func plotJSON(value any) (string, error) {
	switch value := value.(type) {
	case nil:
		return "null", nil
	case bool:
		if value {
			return "true", nil
		}
		return "false", nil
	case string:
		return plotQuote(value), nil
	case int:
		return phNumberString(int64(value)), nil
	case int64:
		return phNumberString(value), nil
	case float64:
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return "null", nil
		}
		return phNumberString(value), nil
	case json.Number:
		number, err := value.Float64()
		if err != nil {
			return "", err
		}
		return plotJSON(number)
	case []byte:
		items := make([]any, len(value))
		for i, b := range value {
			items[i] = int64(b)
		}
		return plotJSON(plotObject{{"type", "Buffer"}, {"data", items}})
	case plotObject:
		parts := make([]string, len(value))
		for i, field := range value {
			encoded, err := plotJSON(field.value)
			if err != nil {
				return "", err
			}
			parts[i] = plotQuote(field.key) + ":" + encoded
		}
		return "{" + strings.Join(parts, ",") + "}", nil
	case []map[string]any:
		items := make([]any, len(value))
		for i, v := range value {
			items[i] = v
		}
		return plotJSON(items)
	case []any:
		parts := make([]string, len(value))
		for i, item := range value {
			encoded, err := plotJSON(item)
			if err != nil {
				return "", err
			}
			parts[i] = encoded
		}
		return "[" + strings.Join(parts, ",") + "]", nil
	case map[string]any:
		keys := make([]string, 0, len(value))
		for key := range value {
			keys = append(keys, key)
		}
		sort.Slice(keys, func(i, j int) bool {
			a, b := utf16.Encode([]rune(keys[i])), utf16.Encode([]rune(keys[j]))
			for k := 0; k < len(a) && k < len(b); k++ {
				if a[k] != b[k] {
					return a[k] < b[k]
				}
			}
			return len(a) < len(b)
		})
		return plotJSON(plotOrdered(value, keys...))
	}
	return "", errors.New("PLOT_TREE_JSON_VALUE_INVALID")
}
func plotSnapshotJSON(snapshot map[string]any) (string, error) {
	tracks := []any{}
	for _, candidate := range snapshot["tracks"].([]any) {
		track := candidate.(map[string]any)
		events := []any{}
		for _, candidate := range track["events"].([]any) {
			event := candidate.(map[string]any)
			sources := []any{}
			for _, candidate := range event["sources"].([]any) {
				source := candidate.(map[string]any)
				sources = append(sources, plotOrdered(source, "type", "draftId", "planId", "eventId", "chapterNumber"))
			}
			events = append(events, plotObject{{"status", event["status"]}, {"chapterNumber", event["chapterNumber"]}, {"summary", event["summary"]}, {"sources", sources}})
		}
		ordered := plotOrdered(track, "id", "title", "role", "parentTrackId", "startChapter", "endChapter", "summary")
		ordered = append(ordered, plotField{"events", events})
		tracks = append(tracks, ordered)
	}
	ordered := plotOrdered(snapshot, "version", "generatedAt", "writingLanguage", "sourceRevision")
	ordered = append(ordered, plotField{"tracks", tracks})
	return plotJSON(ordered)
}
func plotRead(ctx context.Context, q rrQueryer) (map[string]any, error) {
	cores, err := rrMaps(ctx, q, "SELECT writing_language,synopsis,plot_tree_snapshot FROM project_core WHERE id='main'")
	if err != nil {
		return nil, err
	}
	if len(cores) == 0 {
		return nil, ErrPlotTreeProjectCoreMissing
	}
	core := cores[0]
	blueprints, err := rrMaps(ctx, q, `SELECT chapter_number AS chapterNumber,title,purpose,key_events AS keyEvents FROM blueprints ORDER BY chapter_number ASC`)
	if err != nil {
		return nil, err
	}
	chapters, err := rrMaps(ctx, q, `SELECT d.id AS draftId,d.chapter_number AS chapterNumber,COALESCE(o.chapter_title,b.title,'') AS title,
 COALESCE(NULLIF(s.chapter_notes,''),CASE WHEN TRIM(COALESCE(b.notes_updated_at,''))<>'' THEN b.notes ELSE '' END) AS summary
 FROM drafts d LEFT JOIN finalization_outbox o ON o.draft_id=d.id LEFT JOIN summary_snapshots s ON s.draft_id=d.id
 LEFT JOIN blueprints b ON b.chapter_number=d.chapter_number WHERE d.status='finalized' AND NOT EXISTS(
 SELECT 1 FROM drafts newer WHERE newer.chapter_number=d.chapter_number AND newer.status='finalized'
 AND(newer.version>d.version OR(newer.version=d.version AND newer.id>d.id))) ORDER BY d.chapter_number ASC`)
	if err != nil {
		return nil, err
	}
	threads, err := narrativeList(ctx, q)
	if err != nil {
		return nil, err
	}
	narrative := []map[string]any{}
	orderedThreads := []any{}
	for _, thread := range threads {
		events := []map[string]any{}
		orderedEvents := []any{}
		for _, event := range thread["events"].([]map[string]any) {
			selected := map[string]any{}
			keys := []string{"id", "chapterNumber", "type", "evidence", "reason"}
			for _, key := range keys {
				selected[key] = event[key]
			}
			events = append(events, selected)
			orderedEvents = append(orderedEvents, plotOrdered(selected, keys...))
		}
		selected := map[string]any{}
		keys := []string{"id", "title", "type", "targetStartChapter", "targetEndChapter", "authorIntent", "status"}
		for _, key := range keys {
			selected[key] = thread[key]
		}
		selected["events"] = events
		narrative = append(narrative, selected)
		ordered := plotOrdered(selected, keys...)
		ordered = append(ordered, plotField{"events", orderedEvents})
		orderedThreads = append(orderedThreads, ordered)
	}
	language := "zh-CN"
	if core["writing_language"] == "en-US" {
		language = "en-US"
	}
	orderedBlueprints := []any{}
	for _, row := range blueprints {
		orderedBlueprints = append(orderedBlueprints, plotOrdered(row, "chapterNumber", "title", "purpose", "keyEvents"))
	}
	orderedChapters := []any{}
	for _, row := range chapters {
		orderedChapters = append(orderedChapters, plotOrdered(row, "draftId", "chapterNumber", "title", "summary"))
	}
	facts := plotObject{{"writingLanguage", language}, {"synopsis", plotObject{{"content", core["synopsis"]}}}, {"blueprints", orderedBlueprints}, {"finalizedChapters", orderedChapters}, {"narrativeThreads", orderedThreads}}
	raw, err := plotJSON(facts)
	if err != nil {
		return nil, err
	}
	hash := sha256.Sum256([]byte(raw))
	result := map[string]any{"writingLanguage": language, "synopsis": map[string]any{"content": core["synopsis"]}, "blueprints": blueprints, "finalizedChapters": chapters, "narrativeThreads": narrative, "sourceRevision": hex.EncodeToString(hash[:]), "snapshot": nil}
	if phTruthy(core["plot_tree_snapshot"]) {
		text := narrativeJoinText(core["plot_tree_snapshot"])
		var parsed any
		decoder := json.NewDecoder(strings.NewReader(text))
		decoder.UseNumber()
		err := decoder.Decode(&parsed)
		if err == nil {
			var tail any
			if tailErr := decoder.Decode(&tail); tailErr != io.EOF {
				err = errors.New("extra or invalid JSON tail")
			}
		}
		if err == nil {
			snapshot, validateErr := plotValidateStored(parsed)
			if validateErr == nil {
				result["snapshot"] = snapshot
			} else {
				result["storedSnapshotInvalid"] = true
			}
		} else {
			result["storedSnapshotInvalid"] = true
		}
	}
	return result, nil
}
func (p *PlotTree) Read(ctx context.Context) (map[string]any, error) {
	if p == nil || p.db == nil {
		return nil, ErrProjectNotOpen
	}
	tx, err := p.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	result, err := plotRead(ctx, tx)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}
func (p *PlotTree) Save(ctx context.Context, snapshot map[string]any, expectedSourceRevision string) (map[string]any, error) {
	if p == nil || p.db == nil {
		return nil, ErrProjectNotOpen
	}
	var result map[string]any
	err := NewReviewRevisions(p.db).immediate(ctx, func(conn *sql.Conn) error {
		sources, err := plotRead(ctx, conn)
		if err != nil {
			return err
		}
		if sources["sourceRevision"] != expectedSourceRevision {
			return ErrPlotTreeSourcesChanged
		}
		result, err = plotValidate(snapshot, sources)
		if err != nil {
			return err
		}
		text, err := plotSnapshotJSON(result)
		if err != nil {
			return err
		}
		update, err := conn.ExecContext(ctx, "UPDATE project_core SET plot_tree_snapshot=? WHERE id='main'", text)
		return rrAffected(update, err, ErrPlotTreeSaveFailed)
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}
func (p *PlotTree) Clear(ctx context.Context) error {
	if p == nil || p.db == nil {
		return ErrProjectNotOpen
	}
	result, err := p.db.ExecContext(ctx, "UPDATE project_core SET plot_tree_snapshot='' WHERE id='main'")
	return rrAffected(result, err, ErrPlotTreeClearFailed)
}
