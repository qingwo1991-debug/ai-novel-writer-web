package reviewgoals

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

type goalsContract struct {
	FormatVersion int `json:"formatVersion"`
	Cases         []struct {
		Name               string          `json:"name"`
		Raw                json.RawMessage `json:"raw"`
		Goals              FrozenGoals     `json:"goals"`
		Draft              string          `json:"draft"`
		Language           string          `json:"language"`
		ExpectedReview     json.RawMessage `json:"expectedReview"`
		ExpectedReviewJSON string          `json:"expectedReviewJSON"`
		ExpectedItems      json.RawMessage `json:"expectedItems"`
		ExpectedItemsJSON  string          `json:"expectedItemsJSON"`
	} `json:"cases"`
}

func decodeContractJSON(t *testing.T, data []byte) any {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

func TestOriginalGoalReviewContract(t *testing.T) {
	path := os.Getenv("GO_REVIEW_GOALS_CONTRACT")
	if path == "" {
		if os.Getenv("GITHUB_ACTIONS") == "true" {
			t.Fatal("GitHub goal-review contract manifest missing")
		}
		t.Skip("GitHub-only original TypeScript oracle manifest")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var contract goalsContract
	if err := json.Unmarshal(data, &contract); err != nil {
		t.Fatal(err)
	}
	if contract.FormatVersion != 1 || len(contract.Cases) != 26 {
		t.Fatalf("invalid goal-review contract: version %d, cases %d", contract.FormatVersion, len(contract.Cases))
	}
	for _, test := range contract.Cases {
		t.Run(test.Name, func(t *testing.T) {
			raw := decodeContractJSON(t, test.Raw)
			got := Normalize(raw, test.Goals, test.Draft, test.Language)
			gotJSON := ReviewJSON(got)
			if string(gotJSON) != test.ExpectedReviewJSON {
				t.Fatalf("review JSON mismatch\nTS: %s\nGo: %s", test.ExpectedReviewJSON, gotJSON)
			}
			if !reflect.DeepEqual(decodeContractJSON(t, test.ExpectedReview), decodeContractJSON(t, gotJSON)) {
				t.Fatalf("review semantic mismatch\nTS: %s\nGo: %s", test.ExpectedReview, gotJSON)
			}
			itemsJSON := ProjectionJSON(Project(got, test.Language))
			if string(itemsJSON) != test.ExpectedItemsJSON {
				t.Fatalf("projection JSON mismatch\nTS: %s\nGo: %s", test.ExpectedItemsJSON, itemsJSON)
			}
			if !reflect.DeepEqual(decodeContractJSON(t, test.ExpectedItems), decodeContractJSON(t, itemsJSON)) {
				t.Fatalf("projection semantic mismatch\nTS: %s\nGo: %s", test.ExpectedItems, itemsJSON)
			}
		})
	}
}
