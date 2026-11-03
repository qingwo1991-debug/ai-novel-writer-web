package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// ReadProjectCoreTx uses the caller's admitted read transaction. It has the
// same nullable values and three read normalizers as ProjectCore.Get.
func ReadProjectCoreTx(ctx context.Context, tx *sql.Tx) (map[string]any, error) {
	if ctx == nil || tx == nil {
		return nil, ErrProjectNotOpen
	}
	columns := make([]string, len(projectCoreFields))
	values := make([]any, len(projectCoreFields))
	targets := make([]any, len(projectCoreFields))
	for i, field := range projectCoreFields {
		columns[i] = field.column
		targets[i] = &values[i]
	}
	err := tx.QueryRowContext(ctx, "SELECT "+strings.Join(columns, ", ")+" FROM project_core WHERE id = ?", "main").Scan(targets...)
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

// ReadBlueprintsTx delegates to the existing blueprint row parser, including
// JSON characters and nullable text normalization, without any repair DML.
func ReadBlueprintsTx(ctx context.Context, tx *sql.Tx) ([]map[string]any, error) {
	if ctx == nil || tx == nil {
		return nil, ErrProjectNotOpen
	}
	return blueprintRows(ctx, tx, "ORDER BY chapter_number ASC")
}
