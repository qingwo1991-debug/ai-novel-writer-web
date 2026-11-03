//go:build linux

package authorruntime

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
)

// ReadEditorStageJSON keeps the exact project lease, source proof and private
// read transaction through DTO serialization. It never grants a write handle.
func (l *ReadLifecycle) ReadEditorStageJSON(ctx context.Context, owner projectsession.Owner, expected projectsession.Context, operation string) (json.RawMessage, error) {
	if operation != "db:project-core-get" && operation != "db:blueprint-get-all" {
		return nil, ErrInvalid
	}
	var wire json.RawMessage
	err := l.Read(ctx, owner, expected, func(tx *sql.Tx) error {
		var result any
		var err error
		switch operation {
		case "db:project-core-get":
			result, err = store.ReadProjectCoreTx(ctx, tx)
		case "db:blueprint-get-all":
			result, err = store.ReadBlueprintsTx(ctx, tx)
		}
		if err != nil {
			return err
		}
		wire, err = json.Marshal(editorStageWire(result))
		return err
	})
	if err != nil {
		return nil, err
	}
	return wire, nil
}

func editorStageWire(value any) any {
	switch value := value.(type) {
	case []byte:
		data := make([]int, len(value))
		for i, b := range value {
			data[i] = int(b)
		}
		return map[string]any{"type": "Buffer", "data": data}
	case map[string]any:
		if value == nil {
			return nil
		}
		out := make(map[string]any, len(value))
		for key, item := range value {
			out[key] = editorStageWire(item)
		}
		return out
	case []map[string]any:
		out := make([]any, len(value))
		for i, item := range value {
			out[i] = editorStageWire(item)
		}
		return out
	case []any:
		out := make([]any, len(value))
		for i, item := range value {
			out[i] = editorStageWire(item)
		}
		return out
	default:
		return value
	}
}
