package admission

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/reviewbuilder"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/reviewconsistency"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/reviewgoals"
)

// VerifyM03NotGeneratedCycles verifies populated non-revised M03 cycles on a
// caller-owned private snapshot transaction. This verifier has no production
// admission caller. Other populated statuses are deliberately refused.
func VerifyM03NotGeneratedCycles(ctx context.Context, tx *sql.Tx) error {
	if err := VerifyM03ReviewCycleSchema(ctx, tx); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, "SELECT cycle_id,revision_status FROM review_cycles ORDER BY cycle_id")
	if err != nil {
		return err
	}
	type cycle struct{ id, status string }
	var cycles []cycle
	for rows.Next() {
		var c cycle
		if err := rows.Scan(&c.id, &c.status); err != nil {
			rows.Close()
			return err
		}
		cycles = append(cycles, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, c := range cycles {
		if c.status != "not-generated" {
			return errors.New("M03 populated cycle status unsupported")
		}
		basic, err := m03VerifyCycleBasicEvidence(ctx, tx, c.id)
		if err != nil {
			return err
		}
		if basic.revisionStatus != "not-generated" {
			return errors.New("M03 cycle status changed")
		}
		confirmation, err := m03VerifyConfirmation(ctx, tx, basic)
		if err != nil {
			return err
		}
		var reviewIndex int64
		if err := tx.QueryRowContext(ctx, "SELECT review_index FROM reviews WHERE id=?", basic.reviewID).Scan(&reviewIndex); err != nil {
			return err
		}
		want := m03ReviewEffectExpected{
			RootActionID: basic.rootActionID, ReviewID: basic.reviewID,
			ReviewIndex: reviewIndex, ContentHash: basic.reviewContentHash,
			SourceID: basic.baseDraftID, Chapter: basic.chapterNumber,
			Version: basic.sourceVersion, Status: basic.sourceStatus,
			Content: basic.sourceContent,
		}
		// The finding epoch is bound to the proven review attempt, not the root.
		epoch, err := m03ProveReviewEffect(ctx, tx, want, m03BuildReviewGenerationReport)
		if err != nil {
			return err
		}
		basic.epoch = epoch
		proof := m03FindingRowProof{}
		if confirmation != nil {
			version, _ := m03JSPositiveSafeInteger(confirmation.Snapshot["schemaVersion"])
			p := &m03FindingConfirmation{
				contentHash: m03StringHash(confirmation.Body), schemaVersion: version,
			}
			if id, ok := confirmation.Snapshot["cycleId"].(string); ok {
				p.cycleID = id
			}
			if items, ok := confirmation.Snapshot["items"].([]any); ok {
				for _, raw := range items {
					item, _ := raw.(map[string]any)
					findingID, _ := item["findingId"].(string)
					decision, _ := item["decision"].(string)
					p.items = append(p.items, m03FindingConfirmationItem{findingID: findingID, decision: decision})
				}
			}
			proof.confirmation = p
		}
		if err := m03VerifyFindingRows(ctx, tx, basic, proof); err != nil {
			return err
		}
	}
	return nil
}

// m03BuildReviewGenerationReport rebuilds the original persisted report from
// the artifact text and frozen source. The persisted review body is never used
// as input to this builder.
func m03BuildReviewGenerationReport(artifactText string, frozenContext json.RawMessage, reportVersion int) (string, error) {
	var frozen struct {
		Source struct {
			Content string `json:"content"`
		} `json:"source"`
		FrozenGoals       reviewgoals.FrozenGoals     `json:"frozenGoals"`
		WritingLanguage   string                      `json:"writingLanguage"`
		UILocale          string                      `json:"uiLocale"`
		PreflightFindings []reviewconsistency.Finding `json:"preflightFindings"`
	}
	if m03LoneSurrogateEscape(string(frozenContext)) {
		return "", errors.New("M03 frozen context has isolated surrogate")
	}
	if err := json.Unmarshal(frozenContext, &frozen); err != nil {
		return "", err
	}
	compact, err := reviewbuilder.Build(reviewbuilder.Input{
		Content: artifactText, SourceContent: frozen.Source.Content,
		FrozenGoals: frozen.FrozenGoals, WritingLanguage: frozen.WritingLanguage,
		UILocale: frozen.UILocale, PreflightFindings: frozen.PreflightFindings,
		ReportVersion: reportVersion,
	})
	if err != nil {
		return "", err
	}
	pretty, err := reviewbuilder.Pretty(compact)
	return string(pretty), err
}
