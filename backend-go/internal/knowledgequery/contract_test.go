package knowledgequery

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/knowledgecopy"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/knowledgedomain"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestOriginalTypeScriptKnowledgeQueryContract(t *testing.T) {
	file := os.Getenv("GO_KNOWLEDGEQUERY_CONTRACT")
	if file == "" {
		if os.Getenv("GITHUB_ACTIONS") == "true" {
			t.Fatal("original knowledge query oracle mandatory")
		}
		t.Skip("GitHub original query oracle required")
	}
	input, err := os.Open(file)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	var contract struct {
		Format int
		Cases  []struct {
			Name                         string
			Snapshot                     knowledgedomain.Snapshot
			Copies                       []struct{ ID, Raw string }
			Documents, Stats, Vectorless json.RawMessage
			Reads                        []struct {
				ID     string
				Result json.RawMessage
			}
		}
	}
	decoder := json.NewDecoder(input)
	decoder.UseNumber()
	if err := decoder.Decode(&contract); err != nil || contract.Format != 1 || len(contract.Cases) != 14 {
		t.Fatal("invalid contract", err)
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
			if len(item.Snapshot.Registry) > 0 {
				if err := os.WriteFile(filepath.Join(dir, "embedding-spaces.json"), item.Snapshot.Registry, 0600); err != nil {
					t.Fatal(err)
				}
			}
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			view, err := New(context.Background(), item.Snapshot, root, knowledgecopy.Budget{MaxMetadataBytes: 8 * 1024 * 1024, MaxCopies: 100})
			if err != nil {
				root.Close()
				t.Fatal(err)
			}
			if err := root.Close(); err != nil {
				t.Fatal(err)
			}
			check := func(label string, got any, raw json.RawMessage, err error) {
				t.Helper()
				if err != nil {
					t.Fatal(label, err)
				}
				encoded, e := json.Marshal(got)
				if e != nil {
					t.Fatal(e)
				}
				var actual, want any
				if json.Unmarshal(encoded, &actual) != nil || json.Unmarshal(raw, &want) != nil || !reflect.DeepEqual(actual, want) {
					t.Fatalf("%s mismatch\ngot %s\nwant %s", label, encoded, raw)
				}
			}
			docs, err := view.Documents(context.Background())
			check("documents", docs, item.Documents, err)
			stats, err := view.Stats(context.Background())
			check("stats", stats, item.Stats, err)
			missing, err := view.VectorlessCount(context.Background())
			check("vectorless", missing, item.Vectorless, err)
			for _, read := range item.Reads {
				result, err := view.ReadCopy(context.Background(), read.ID)
				check("readCopy", result, read.Result, err)
			}
		})
	}
}
