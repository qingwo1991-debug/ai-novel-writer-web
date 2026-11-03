package knowledgequery

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/knowledgecopy"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/knowledgedomain"
)

func TestOriginalTypeScriptKnowledgeSearchFTSContract(t *testing.T) {
	file := os.Getenv("GO_KNOWLEDGEQUERY_CONTRACT")
	if file == "" {
		if os.Getenv("GITHUB_ACTIONS") == "true" {
			t.Fatal("original knowledge search oracle mandatory")
		}
		t.Skip("GitHub original search oracle required")
	}
	input, err := os.Open(file)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	var contract struct {
		Format int
		Cases  []struct {
			Name     string
			Snapshot knowledgedomain.Snapshot
			Copies   []struct{ ID, Raw string }
			Searches []struct {
				Query    string
				TopK     int
				Scope    []int64
				Excluded []string
				Result   json.RawMessage
			}
		}
	}
	decoder := json.NewDecoder(input)
	decoder.UseNumber()
	if err := decoder.Decode(&contract); err != nil || contract.Format != 1 || len(contract.Cases) != 14 {
		t.Fatal("invalid search contract", err)
	}
	for _, item := range contract.Cases {
		t.Run(item.Name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Chmod(dir, 0700); err != nil {
				t.Fatal(err)
			}
			if len(item.Copies) > 0 {
				if err := os.Mkdir(filepath.Join(dir, "knowledge-copies"), 0700); err != nil {
					t.Fatal(err)
				}
				for _, copy := range item.Copies {
					sum := sha256.Sum256([]byte(copy.ID))
					if err := os.WriteFile(filepath.Join(dir, "knowledge-copies", hex.EncodeToString(sum[:])+".json"), []byte(copy.Raw), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			capture, err := NewSearchCapture(context.Background(), item.Snapshot, root, knowledgecopy.Budget{MaxMetadataBytes: 8 * 1024 * 1024, MaxCopies: 100})
			if err != nil {
				root.Close()
				t.Fatal(err)
			}
			if err := root.Close(); err != nil {
				t.Fatal(err)
			}
			if len(item.Searches) < 4 {
				t.Fatal("incomplete search oracle")
			}
			for _, request := range item.Searches {
				var scope *ChapterScope
				if len(request.Scope) != 0 {
					if len(request.Scope) != 2 {
						t.Fatal("invalid scope oracle")
					}
					scope = &ChapterScope{From: request.Scope[0], To: request.Scope[1]}
				}
				got, err := capture.SearchFTS(context.Background(), request.Query, request.TopK, scope, request.Excluded)
				if err != nil {
					t.Fatal(err)
				}
				encoded, err := json.Marshal(got)
				if err != nil {
					t.Fatal(err)
				}
				var actual, want any
				if json.Unmarshal(encoded, &actual) != nil || json.Unmarshal(request.Result, &want) != nil || !reflect.DeepEqual(actual, want) {
					t.Fatalf("search %q mismatch\ngot %s\nwant %s", request.Query, encoded, request.Result)
				}
			}
		})
	}
}
