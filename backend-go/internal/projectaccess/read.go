package projectaccess

import (
	"context"
	"database/sql"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/knowledgecopy"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/knowledgequery"
	"net/url"
	"path/filepath"
	"unicode/utf8"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/writerlock"
)

// KnowledgeRead is an immutable bounded copied-state capture, NOT a source handle,
// durable project session, refresh authority or write capability. Authenticate and
// bind the entire capture to one exact session/tab/lease before returning its data.
// Later author changes are NOT reflected; no current-author-state claim is made.
type KnowledgeRead struct {
	view      *knowledgequery.View
	search    *knowledgequery.SearchCapture
	finalized map[string]int64
}

func (r *KnowledgeRead) View() *knowledgequery.View {
	if r == nil {
		return nil
	}
	return r.view
}
func (r *KnowledgeRead) Search() *knowledgequery.SearchCapture {
	if r == nil {
		return nil
	}
	return r.search
}
func (r *KnowledgeRead) FinalizedChapters() map[string]int64 {
	result := map[string]int64{}
	if r != nil {
		for id, chapter := range r.finalized {
			result[id] = chapter
		}
	}
	return result
}

// InspectKnowledge follows the complete read-only Preflight lifecycle. The result
// escapes only after source/copy rechecks, owned scratch cleanup and lock release.
// Native opt-in is mandatory; non-native builds fail closed before filesystem work.
func InspectKnowledge(ctx context.Context, config Config) (*KnowledgeRead, Report, error) {
	if config.Knowledge == nil || !config.Knowledge.Native {
		return nil, Report{}, ErrConfiguration
	}
	capture := new(KnowledgeRead)
	report, err := preflightRead(ctx, config, nil, capture)
	if err != nil {
		return nil, Report{}, err
	}
	return capture, report, nil
}

// InspectKnowledgeUnderLock captures the same bounded immutable native view
// while the caller retains the exact legacy gateway lock. It never acquires a
// second lock or opens the author's database through SQLite. The caller must
// keep held live through capture, adoption and every subsequent source check.
func InspectKnowledgeUnderLock(ctx context.Context, config Config, held *writerlock.Lock) (*KnowledgeRead, Report, error) {
	if held == nil || config.Knowledge == nil || !config.Knowledge.Native {
		return nil, Report{}, ErrConfiguration
	}
	capture := new(KnowledgeRead)
	report, err := preflightReadWithLock(ctx, config, nil, capture, held)
	if err != nil {
		return nil, Report{}, err
	}
	if capture.View() == nil || capture.Search() == nil || report.Knowledge == nil {
		return nil, Report{}, ErrConfiguration
	}
	return capture, report, nil
}
func captureKnowledgeRead(ctx context.Context, stage *stagedKnowledge, databasePath string, budget knowledgecopy.Budget, result *KnowledgeRead) (err error) {
	defer func() {
		if err != nil {
			*result = KnowledgeRead{}
		}
	}()
	view, err := knowledgequery.New(ctx, stage.readSnapshot, stage.root, budget)
	if err != nil {
		return redact(err)
	}
	search, err := knowledgequery.NewSearchCapture(ctx, stage.readSnapshot, stage.root, budget)
	if err != nil {
		return redact(err)
	}
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(databasePath)}
	query := url.Values{}
	query.Set("mode", "ro")
	query.Set("_query_only", "on")
	query.Set("_foreign_keys", "on")
	query.Set("_busy_timeout", "5000")
	uri.RawQuery = query.Encode()
	db, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		return ErrFilesystem
	}
	db.SetMaxOpenConns(1)
	defer func() {
		if closeErr := db.Close(); closeErr != nil {
			err = ErrFilesystem
		}
	}()
	rows, err := db.QueryContext(ctx, "SELECT knowledge_document_id, chapter_number FROM finalization_outbox WHERE knowledge_document_id != ''")
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return ErrFilesystem
	}
	chapters := map[string]int64{}
	rowCount := 0
	textBytes := 0
	for rows.Next() {
		rowCount++
		if rowCount > 10000 {
			rows.Close()
			return ErrFilesystem
		}
		var id string
		var chapter int64
		if err := rows.Scan(&id, &chapter); err != nil {
			rows.Close()
			return ErrFilesystem
		}
		if !utf8.ValidString(id) || len(id) > 32*1024*1024-textBytes || chapter > 9007199254740991 || chapter < -9007199254740991 {
			rows.Close()
			return ErrFilesystem
		}
		textBytes += len(id)
		chapters[id] = chapter
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return ErrFilesystem
	}
	if err := rows.Close(); err != nil {
		return ErrFilesystem
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	*result = KnowledgeRead{view: view, search: search, finalized: chapters}
	return nil
}
