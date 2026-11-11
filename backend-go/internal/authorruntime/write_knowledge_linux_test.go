//go:build linux

package authorruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/knowledgedomain"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
)

// The write fence duplicates the admission's knowledge predicate, so it needs
// the same distinction pinned: copies this lane's own knowledge operations wrote
// beside their canonical snapshot are not knowledge the fence would degrade, and
// copies with no canonical snapshot are.
const writeKnowledgeDocumentID = "b1f0a4c2-7d3e-4a58-9c61-8e2d5f7a9b03"

// writeKnowledgeFixture lays down the two fixed entries the served import lane
// writes: the canonical snapshot the served kb reads decode (optionally) and the
// reference-format copy named after the document.
func writeKnowledgeFixture(t *testing.T, projectPath string, withSnapshot bool) {
	t.Helper()
	storage := filepath.Join(projectPath, projectfs.StorageDirectory)
	if withSnapshot {
		field := func(name, kind string) knowledgedomain.Field {
			return knowledgedomain.Field{Name: name, Type: kind}
		}
		snapshot := knowledgedomain.Snapshot{Tables: []knowledgedomain.Table{{
			Name:    "documents",
			Version: 1,
			Schema: []knowledgedomain.Field{
				field("id", "utf8"), field("fileName", "utf8"), field("importedAt", "utf8"),
				field("chunkCount", "int32"), field("filePath", "utf8"), field("corpusKind", "utf8"),
			},
			Records: []map[string]any{{
				"id":         writeKnowledgeDocumentID,
				"fileName":   "第1章 门.txt",
				"importedAt": "2026-01-01T00:00:00.000Z",
				"chunkCount": json.Number("0"),
				"filePath":   "knowledge-copy:" + writeKnowledgeDocumentID,
				"corpusKind": "project-knowledge",
			}},
		}}}
		// The fixture must be a store the served validator decodes; a store it
		// rejects would make the test below pass for the wrong reason.
		if _, err := knowledgedomain.Validate(context.Background(), snapshot, knowledgedomain.Budget{
			MaxTables:    knowledgedomain.HardMaxTables,
			MaxRows:      knowledgedomain.HardMaxRows,
			MaxCells:     knowledgedomain.HardMaxCells,
			MaxTextBytes: knowledgedomain.HardMaxTextBytes,
		}); err != nil {
			t.Fatalf("the canonical knowledge fixture is not a decodable store: %v", err)
		}
		raw, err := json.Marshal(snapshot)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(storage, projectfs.CanonicalKnowledgeName), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	directory := filepath.Join(storage, "knowledge-copies")
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(writeKnowledgeDocumentID))
	copied := filepath.Join(directory, hex.EncodeToString(digest[:])+".json")
	if err := os.WriteFile(copied, []byte(`{"content":"门后有人。","indexedHash":"0","edited":false,"indexDirty":false}`), 0600); err != nil {
		t.Fatal(err)
	}
}

// TestWriteCandidateAcceptsTheLanesOwnCanonicalKnowledge proves the fence does
// not refuse the knowledge this lane can manage itself.
func TestWriteCandidateAcceptsTheLanesOwnCanonicalKnowledge(t *testing.T) {
	catalog, key, config, _, _ := readFixture(t)
	config.Knowledge = emptyKnowledgeConfig()
	writeKnowledgeFixture(t, config.ProjectPath, true)
	reader, err := OpenReadOnly(context.Background(), catalog, key, config)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if _, err := reader.AssessSQLiteWriteCandidate(context.Background(), config.Schema); err != nil {
		t.Fatalf("a project holding this lane's own canonical knowledge was refused a write candidate: %v", err)
	}
}

// TestWriteCandidateRefusesCopiesWithoutCanonicalSnapshot keeps the desktop
// refusal and its symbolic code.
func TestWriteCandidateRefusesCopiesWithoutCanonicalSnapshot(t *testing.T) {
	catalog, key, config, _, _ := readFixture(t)
	config.Knowledge = emptyKnowledgeConfig()
	writeKnowledgeFixture(t, config.ProjectPath, false)
	reader, err := OpenReadOnly(context.Background(), catalog, key, config)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if _, err := reader.AssessSQLiteWriteCandidate(context.Background(), config.Schema); !errors.Is(err, ErrWriteKnowledgeUnproved) {
		t.Fatalf("a copy footprint without a canonical snapshot reached the write fence: %v", err)
	}
}
