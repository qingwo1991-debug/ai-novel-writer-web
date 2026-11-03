package reviewbuilder

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/reviewconsistency"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/reviewgoals"
)

func TestBuildMatchesOriginalTypeScriptBytes(t *testing.T) {
	path := os.Getenv("GO_REVIEW_BUILDER_CONTRACT")
	if path == "" {
		if os.Getenv("GITHUB_ACTIONS") == "true" {
			t.Fatal("original TypeScript review-builder oracle required")
		}
		t.Skip("GitHub original TypeScript review-builder oracle required")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		FormatVersion int `json:"formatVersion"`
		Cases         []struct {
			Name  string `json:"name"`
			Input struct {
				Content           string                      `json:"content"`
				SourceContent     string                      `json:"sourceContent"`
				FrozenGoals       reviewgoals.FrozenGoals     `json:"frozenGoals"`
				WritingLanguage   string                      `json:"writingLanguage"`
				UILocale          string                      `json:"uiLocale"`
				PreflightFindings []reviewconsistency.Finding `json:"preflightFindings"`
				ReportVersion     int                         `json:"reportVersion"`
			} `json:"input"`
			Expected struct {
				OK      bool   `json:"ok"`
				Compact string `json:"compact"`
				Pretty  string `json:"pretty"`
			} `json:"expected"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.FormatVersion != 1 || len(fixture.Cases) != 8 {
		t.Fatalf("unexpected original review-builder oracle: version=%d cases=%d", fixture.FormatVersion, len(fixture.Cases))
	}
	for _, sample := range fixture.Cases {
		t.Run(sample.Name, func(t *testing.T) {
			compact, err := Build(Input{
				Content: sample.Input.Content, SourceContent: sample.Input.SourceContent,
				FrozenGoals: sample.Input.FrozenGoals, WritingLanguage: sample.Input.WritingLanguage,
				UILocale: sample.Input.UILocale, PreflightFindings: sample.Input.PreflightFindings,
				ReportVersion: sample.Input.ReportVersion,
			})
			if !sample.Expected.OK {
				if err == nil {
					t.Fatalf("original TypeScript rejected report, Go accepted %s", compact)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if string(compact) != sample.Expected.Compact {
				t.Fatalf("compact report differs from TypeScript\nGo: %s\nTS: %s", compact, sample.Expected.Compact)
			}
			pretty, err := Pretty(compact)
			if err != nil {
				t.Fatal(err)
			}
			if string(pretty) != sample.Expected.Pretty {
				t.Fatalf("stored report bytes differ from TypeScript\nGo: %s\nTS: %s", pretty, sample.Expected.Pretty)
			}
		})
	}
}
