package reviewreport

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"testing"
)

type parserContract struct {
	FormatVersion int `json:"formatVersion"`
	Cases         []struct {
		Name               string `json:"name"`
		Content            string `json:"content"`
		Version            int    `json:"version"`
		UnsupportedUnicode bool   `json:"unsupportedUnicode"`
		Expected           struct {
			OK    bool            `json:"ok"`
			Value json.RawMessage `json:"value"`
		} `json:"expected"`
	} `json:"cases"`
}

func semanticJSON(t *testing.T, raw []byte) any {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

func TestOriginalReviewParserContract(t *testing.T) {
	path := os.Getenv("GO_REVIEW_REPORT_PARSER_CONTRACT")
	if path == "" {
		if os.Getenv("GITHUB_ACTIONS") == "true" {
			t.Fatal("GitHub contract manifest missing")
		}
		t.Skip("GitHub-only original TypeScript oracle manifest")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var contract parserContract
	if err := json.Unmarshal(data, &contract); err != nil {
		t.Fatal(err)
	}
	if contract.FormatVersion != 1 || len(contract.Cases) != 25 {
		t.Fatalf("invalid parser contract: version %d, cases %d", contract.FormatVersion, len(contract.Cases))
	}
	for _, test := range contract.Cases {
		t.Run(test.Name, func(t *testing.T) {
			got, err := Parse(test.Content, test.Version)
			if test.UnsupportedUnicode {
				if !test.Expected.OK || !errors.Is(err, ErrUnsupportedUnicode) {
					t.Fatalf("documented unsupported Unicode case: TS accepted=%t, Go error=%v", test.Expected.OK, err)
				}
				return
			}
			if !test.Expected.OK {
				if err == nil {
					t.Fatalf("TS rejected case but Go accepted: %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("TS accepted case but Go rejected: %v", err)
			}
			encoded, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			if want, actual := semanticJSON(t, test.Expected.Value), semanticJSON(t, encoded); !reflect.DeepEqual(want, actual) {
				t.Fatalf("semantic mismatch\nTS: %s\nGo: %s", test.Expected.Value, encoded)
			}
		})
	}
}

func TestIsolatedSurrogateFailsClosed(t *testing.T) {
	_, err := Parse(`{"summary":"\ud800","items":[{"category":"x","severity":"pass","description":"ok"}]}`, 2)
	if !errors.Is(err, ErrUnsupportedUnicode) {
		t.Fatalf("got %v, want explicit unsupported Unicode error", err)
	}
}
