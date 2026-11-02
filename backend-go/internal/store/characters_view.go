package store

import (
	"context"
	"database/sql"
)

// ReadForProject returns the public roster and display-only derived-field grants
// from one SQLite snapshot. The project ID must come from the frozen admitted
// lease, not renderer input. No portable origin authority or write owner is added.
func (r *Characters) ReadForProject(ctx context.Context, projectID string) (map[string]any, error) {
	if r == nil || r.db == nil {
		return nil, ErrProjectNotOpen
	}
	if !summarySupportedText(projectID) || summaryTrim(projectID) == "" {
		return nil, ErrCharacterInputUnsupported
	}
	var result map[string]any
	err := NewReviewRevisions(r.db).immediate(ctx, func(conn *sql.Conn) error {
		roster, err := charactersRoster(ctx, conn)
		if err != nil {
			return err
		}
		fields, err := currentDerivedFields(ctx, conn, projectID)
		// Original controller allows raw author/history to remain readable if the
		// derived projection fails. No field grant can become current on failure.
		if err != nil {
			fields = map[string]any{}
		}
		roster["currentDerivedFields"] = fields
		result = roster
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}
