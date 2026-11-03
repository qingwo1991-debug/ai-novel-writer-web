package admission

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/reviewbuilder"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/reviewconsistency"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/reviewgoals"
)

func TestM03ReviewEffectAgainstOriginalTypeScriptOracle(t *testing.T) {
	path := os.Getenv("GO_M03_NOTGENERATED_CONTRACT")
	if path == "" {
		t.Skip("original TypeScript oracle is generated in GitHub Actions")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var oracle struct {
		Scope string `json:"scope"`
		Cases []struct {
			Name, File string
			Expected   bool
			SHA256     string `json:"sha256"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &oracle); err != nil {
		t.Fatal(err)
	}
	if oracle.Scope != "m03-verifier-only" || len(oracle.Cases) != 9 {
		t.Fatal("unexpected original oracle")
	}
	selected := map[string]bool{
		"valid-not-generated":       true,
		"missing-effect-index":      false,
		"discarded-review-artifact": false,
		"rebound-report-artifact":   false,
		"frozen-source-binding":     false,
	}
	for _, sample := range oracle.Cases {
		want, include := selected[sample.Name]
		if !include {
			continue
		}
		t.Run(sample.Name, func(t *testing.T) {
			if sample.Expected != want {
				t.Fatal("original oracle expectation changed")
			}
			fixture, err := os.ReadFile(sample.File)
			if err != nil {
				t.Fatal(err)
			}
			if fmt.Sprintf("%x", sha256.Sum256(fixture)) != sample.SHA256 {
				t.Fatal("original oracle database hash differs")
			}
			db, err := sql.Open("sqlite3", "file:"+sample.File+"?mode=ro")
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			db.SetMaxOpenConns(1)
			ctx := context.Background()
			tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			var source string
			var sourceID, chapter, version, reviewID, reviewIndex int64
			var status, rootID, contentHash string
			err = tx.QueryRowContext(ctx, `SELECT c.root_action_id,c.review_id,r.review_index,c.review_content_hash,
				r.base_draft_id,r.source_draft_chapter_number,r.source_draft_version,r.source_draft_status,r.source_content
				FROM review_cycles c JOIN reviews r ON r.id=c.review_id LIMIT 1`).Scan(
				&rootID, &reviewID, &reviewIndex, &contentHash, &sourceID, &chapter, &version, &status, &source)
			if err != nil {
				t.Fatal(err)
			}
			build := func(artifact string, raw json.RawMessage, reportVersion int) (string, error) {
				var frozen struct {
					Source struct {
						Content string `json:"content"`
					} `json:"source"`
					FrozenGoals       reviewgoals.FrozenGoals     `json:"frozenGoals"`
					WritingLanguage   string                      `json:"writingLanguage"`
					UILocale          string                      `json:"uiLocale"`
					PreflightFindings []reviewconsistency.Finding `json:"preflightFindings"`
				}
				if err := json.Unmarshal(raw, &frozen); err != nil {
					return "", err
				}
				compact, err := reviewbuilder.Build(reviewbuilder.Input{Content: artifact,
					SourceContent: frozen.Source.Content, FrozenGoals: frozen.FrozenGoals,
					WritingLanguage: frozen.WritingLanguage, UILocale: frozen.UILocale,
					PreflightFindings: frozen.PreflightFindings, ReportVersion: reportVersion})
				if err != nil {
					return "", err
				}
				pretty, err := reviewbuilder.Pretty(compact)
				return string(pretty), err
			}
			err = m03ProveReviewEffect(ctx, tx, m03ReviewEffectExpected{
				RootActionID: rootID, ReviewID: reviewID, ReviewIndex: reviewIndex,
				ContentHash: contentHash, SourceID: sourceID, Chapter: chapter,
				Version: version, Status: status, Content: source,
			}, build)
			if (err == nil) != want {
				t.Fatalf("Go review effect differs from original TypeScript oracle: %v", err)
			}
		})
	}
}
