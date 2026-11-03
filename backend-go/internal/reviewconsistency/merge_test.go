package reviewconsistency

import (
	"encoding/json"
	"os"
	"testing"
)

func TestMergeMatchesOriginalTypeScriptBytes(t *testing.T) {
	contract := os.Getenv("GO_REVIEW_CONSISTENCY_MERGE_CONTRACT")
	if contract == "" {
		if os.Getenv("GITHUB_ACTIONS") == "true" {
			t.Fatal("original TypeScript review-consistency oracle required")
		}
		t.Skip("GitHub original TypeScript review-consistency oracle required")
	}
	data, err := os.ReadFile(contract)
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		FormatVersion int `json:"formatVersion"`
		Cases         []struct {
			Name         string    `json:"name"`
			ReviewJSON   string    `json:"reviewJSON"`
			Findings     []Finding `json:"findings"`
			Locale       string    `json:"locale"`
			ExpectedJSON string    `json:"expectedJSON"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.FormatVersion != 1 || len(manifest.Cases) != 6 {
		t.Fatalf("unexpected original review-consistency oracle: %d cases", len(manifest.Cases))
	}
	for _, sample := range manifest.Cases {
		t.Run(sample.Name, func(t *testing.T) {
			input, err := DecodeReview([]byte(sample.ReviewJSON))
			if err != nil {
				t.Fatal(err)
			}
			merged, err := Merge(input, sample.Findings, sample.Locale)
			if err != nil {
				t.Fatal(err)
			}
			actual, err := merged.MarshalJSJSON()
			if err != nil {
				t.Fatal(err)
			}
			if string(actual) != sample.ExpectedJSON {
				t.Fatalf("Go merge bytes differ from original TypeScript: got=%s want=%s", actual, sample.ExpectedJSON)
			}
			before, err := input.MarshalJSJSON()
			if err != nil || string(before) != sample.ReviewJSON {
				t.Fatalf("merge changed its input: got=%s err=%v", before, err)
			}
		})
	}
}
