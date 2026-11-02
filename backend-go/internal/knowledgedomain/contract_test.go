package knowledgedomain

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"testing"
)

func TestOriginalTypeScriptDomainContract(t *testing.T) {
	path := os.Getenv("GO_KNOWLEDGEDOMAIN_CONTRACT")
	if path == "" {
		if os.Getenv("GITHUB_ACTIONS") == "true" {
			t.Fatal("original domain contract mandatory")
		}
		t.Skip("GitHub original synthetic contract required")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var contract struct {
		Format int `json:"format"`
		Cases  []struct {
			Name             string   `json:"name"`
			Snapshot         Snapshot `json:"snapshot"`
			OriginalAccepted bool     `json:"originalAccepted"`
			GoAccepted       bool     `json:"goAccepted"`
		} `json:"cases"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&contract); err != nil || contract.Format != 1 || len(contract.Cases) != 22 {
		t.Fatal("invalid original contract", err)
	}
	for _, item := range contract.Cases {
		t.Run(item.Name, func(t *testing.T) {
			summary, err := Validate(context.Background(), item.Snapshot, Budget{MaxTables: 32, MaxRows: 10000, MaxCells: 200000, MaxTextBytes: 32 * 1024 * 1024})
			if (err == nil) != item.GoAccepted {
				t.Fatalf("originalAccepted=%t deliberateGoDomain=%t result=%v", item.OriginalAccepted, item.GoAccepted, err)
			}
			if err != nil {
				encoded, _ := json.Marshal(summary)
				zero, _ := json.Marshal(Summary{})
				if !bytes.Equal(encoded, zero) {
					t.Fatal("failure summary not zero")
				}
			}
		})
	}
}
