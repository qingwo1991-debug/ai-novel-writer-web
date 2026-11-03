//go:build linux

package authorruntime

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
)

// ReadCurrentOverviewJSON holds the same exact project lease, private source
// verification and read transaction through aggregation and serialization.
// A project without a core row has the desktop's unavailable shape.
func (l *ReadLifecycle) ReadCurrentOverviewJSON(ctx context.Context, owner projectsession.Owner, expected projectsession.Context) (json.RawMessage, error) {
	var wire json.RawMessage
	err := l.Read(ctx, owner, expected, func(tx *sql.Tx) error {
		overview, err := store.ReadCurrentProjectOverviewTx(ctx, tx, expected.ProjectID)
		if err != nil {
			return err
		}
		if overview == nil {
			wire = json.RawMessage(`{"state":"unavailable"}`)
			return nil
		}
		wire, err = json.Marshal(overview)
		return err
	})
	if err != nil {
		return nil, err
	}
	return wire, nil
}
