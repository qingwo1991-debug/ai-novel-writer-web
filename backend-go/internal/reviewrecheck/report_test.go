package reviewrecheck

import (
	"encoding/json"
	"errors"
	"os"
	"testing"
)

type recheckContract struct {
	FormatVersion int `json:"formatVersion"`
	Cases         []struct {
		Name               string  `json:"name"`
		Content            string  `json:"content"`
		Merged             string  `json:"merged"`
		Context            Context `json:"context"`
		Locale             string  `json:"locale"`
		ReportVersion      int     `json:"reportVersion"`
		ExpectedJSON       string  `json:"expectedJSON"`
		ExpectedPrettyJSON string  `json:"expectedPrettyJSON"`
	} `json:"cases"`
}

func TestSelectedSurrogateFailsClosed(t *testing.T) {
	content := `{"summary":"\ud800","items":[]}`
	_, err := Build(content, "", Context{Version: 2, Findings: []Finding{}}, "zh-CN", 2)
	if !errors.Is(err, ErrUnsupportedUnicode) {
		t.Fatalf("got %v, want unsupported Unicode", err)
	}
}

func TestOriginalRecheckReportContract(t *testing.T) {
	path := os.Getenv("GO_REVIEW_RECHECK_CONTRACT")
	if path == "" {
		if os.Getenv("GITHUB_ACTIONS") == "true" {
			t.Fatal("GitHub recheck contract manifest missing")
		}
		t.Skip("GitHub-only original TypeScript oracle manifest")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var contract recheckContract
	if err := json.Unmarshal(data, &contract); err != nil {
		t.Fatal(err)
	}
	if contract.FormatVersion != 1 || len(contract.Cases) != 24 {
		t.Fatalf("invalid recheck contract: version %d, cases %d", contract.FormatVersion, len(contract.Cases))
	}
	for _, sample := range contract.Cases {
		t.Run(sample.Name, func(t *testing.T) {
			report, err := Build(sample.Content, sample.Merged, sample.Context, sample.Locale, sample.ReportVersion)
			if err != nil {
				t.Fatal(err)
			}
			if got := string(report.MarshalJSJSON()); got != sample.ExpectedJSON {
				t.Fatalf("ordered JSON mismatch\nTS: %s\nGo: %s", sample.ExpectedJSON, got)
			}
			if got := string(report.PrettyJSJSON()); got != sample.ExpectedPrettyJSON {
				t.Fatalf("pretty JSON mismatch\nTS: %s\nGo: %s", sample.ExpectedPrettyJSON, got)
			}
		})
	}
}
