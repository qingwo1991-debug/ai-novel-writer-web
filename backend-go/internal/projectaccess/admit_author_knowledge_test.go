//go:build linux

package projectaccess

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

// These tests pin the one distinction the knowledge admission draws: knowledge
// this lane wrote beside its own canonical snapshot is not the desktop asset that
// must stay refused. The writer under test is the served import lane, which
// leaves .ai-novel/knowledge-snapshot.json (the store every served kb read
// decodes) beside .ai-novel/knowledge-copies/<sha256hex(docId)>.json.
const knowledgeFixtureDocumentID = "6f1d5d0e-9c57-4a1e-8a2b-2f0f2f5b7c11"

// knowledgeFixtureSnapshot is a minimal store of exactly the shape the served
// import writes: the `documents` table with the six fields it declares, one row
// whose filePath carries the knowledge-copy marker, and no registry.
func knowledgeFixtureSnapshot(t *testing.T) []byte {
	t.Helper()
	field := func(name, kind string) knowledgedomain.Field {
		return knowledgedomain.Field{Name: name, Type: kind}
	}
	snapshot := knowledgedomain.Snapshot{
		Tables: []knowledgedomain.Table{{
			Name:    "documents",
			Version: 1,
			Schema: []knowledgedomain.Field{
				field("id", "utf8"), field("fileName", "utf8"), field("importedAt", "utf8"),
				field("chunkCount", "int32"), field("filePath", "utf8"), field("corpusKind", "utf8"),
			},
			Records: []map[string]any{{
				"id":         knowledgeFixtureDocumentID,
				"fileName":   "第1章 门.txt",
				"importedAt": "2026-01-01T00:00:00.000Z",
				"chunkCount": json.Number("0"),
				"filePath":   "knowledge-copy:" + knowledgeFixtureDocumentID,
				"corpusKind": "project-knowledge",
			}},
		}},
	}
	// A fixture the served validator would reject would make every test below pass
	// for the wrong reason, so the store is proven here, in the test, first.
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
	return raw
}

// knowledgeFixtureWrite lays down the two fixed knowledge entries independently.
// withSnapshot=false withCopies=true is the desktop footprint: reference-format
// copies with no canonical JSON store beside them.
func knowledgeFixtureWrite(t *testing.T, projectPath string, withSnapshot, withCopies bool) {
	t.Helper()
	storage := filepath.Join(projectPath, projectfs.StorageDirectory)
	if withSnapshot {
		writeAccess(t, filepath.Join(storage, projectfs.CanonicalKnowledgeName), knowledgeFixtureSnapshot(t))
	}
	if withCopies {
		directory := filepath.Join(storage, "knowledge-copies")
		if err := os.MkdirAll(directory, 0700); err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256([]byte(knowledgeFixtureDocumentID))
		writeAccess(t, filepath.Join(directory, hex.EncodeToString(digest[:])+".json"),
			[]byte(`{"content":"门后有人。","indexedHash":"0","edited":false,"indexDirty":false}`))
	}
}

// TestAdmitAuthorOpensTheLanesOwnCanonicalKnowledge is the regression test for
// the import-then-reopen lockout: a project carrying the exact knowledge layout
// the served import writes must be admitted.
func TestAdmitAuthorOpensTheLanesOwnCanonicalKnowledge(t *testing.T) {
	config, project, held := admittedAuthorFixture(t)
	knowledgeFixtureWrite(t, config.ProjectPath, true, true)
	permit, err := AdmitAuthor(context.Background(), config, project, held)
	if err != nil {
		t.Fatalf("a project holding this lane's own canonical knowledge was refused: %v", err)
	}
	if err := permit.Verify(context.Background(), project); err != nil {
		t.Fatalf("verify failed on a project this lane's own knowledge store left unchanged: %v", err)
	}
	if err := permit.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestAdmitAuthorRefusesCopiesWithoutCanonicalSnapshot keeps the desktop refusal:
// copies with no decodable canonical snapshot are knowledge no Go writer can
// maintain, and the symbolic code is unchanged.
func TestAdmitAuthorRefusesCopiesWithoutCanonicalSnapshot(t *testing.T) {
	config, project, held := admittedAuthorFixture(t)
	knowledgeFixtureWrite(t, config.ProjectPath, false, true)
	if _, err := AdmitAuthor(context.Background(), config, project, held); !errors.Is(err, ErrAuthorKnowledgeUnproved) {
		t.Fatalf("a copy footprint without a canonical snapshot was admitted: %v", err)
	}
}

// TestAdmitAuthorRefusesUndecodableCanonicalKnowledge keeps the "decodable" half
// of the distinction: a snapshot that is present but is not a store this lane's
// reads could decode proves nothing, so the copies beside it stay unproved.
func TestAdmitAuthorRefusesUndecodableCanonicalKnowledge(t *testing.T) {
	config, project, held := admittedAuthorFixture(t)
	knowledgeFixtureWrite(t, config.ProjectPath, true, true)
	writeAccess(t, filepath.Join(config.ProjectPath, projectfs.StorageDirectory, projectfs.CanonicalKnowledgeName),
		[]byte(`{"tables":[{"name":"documents","records":[]}]}`))
	if _, err := AdmitAuthor(context.Background(), config, project, held); !errors.Is(err, ErrAuthorKnowledgeUnproved) {
		t.Fatalf("an undecodable canonical snapshot admitted a copy footprint: %v", err)
	}
}

// TestAdmitAuthorRefusesCopiesTheSnapshotDoesNotName is the other direction of
// "copies matching it": a copy file no canonical document names is knowledge this
// snapshot does not describe, so the store is not solely this lane's.
func TestAdmitAuthorRefusesCopiesTheSnapshotDoesNotName(t *testing.T) {
	config, project, held := admittedAuthorFixture(t)
	knowledgeFixtureWrite(t, config.ProjectPath, true, true)
	digest := sha256.Sum256([]byte("synthetic-foreign-document"))
	writeAccess(t, filepath.Join(config.ProjectPath, projectfs.StorageDirectory, "knowledge-copies",
		hex.EncodeToString(digest[:])+".json"),
		[]byte(`{"content":"外来文档","indexedHash":"0","edited":false,"indexDirty":false}`))
	if _, err := AdmitAuthor(context.Background(), config, project, held); !errors.Is(err, ErrAuthorKnowledgeUnproved) {
		t.Fatalf("a copy the canonical snapshot does not name was admitted: %v", err)
	}
}
