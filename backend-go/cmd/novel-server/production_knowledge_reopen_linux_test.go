//go:build linux

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/admission"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// These two tests travel the real served surface over one disposable data root.
// The import writers leave .ai-novel/knowledge-snapshot.json (the store every
// served kb read decodes) and .ai-novel/knowledge-copies/<sha256hex(docId)>.json
// (the reference-format copy); the NEXT process has to be able to open the book
// it wrote. The lane helper therefore never opens a session: the refusal
// assertion below needs the wire answer, not a helper that fatals on it.
const productionKnowledgeReopenNonce = "knowledge-reopen-1"

// productionKnowledgeReopenLane installs project:open, the served knowledge
// imports and the picker uploads over an existing data root. A second call after
// closing the first is a second process opening the same library.
func productionKnowledgeReopenLane(t *testing.T, root string, contract admission.SchemaContract,
	uploads productionPickerUploads) (*transport.Server, *productionOpen) {
	t.Helper()
	layout, err := productionLayoutFor(root)
	if err != nil {
		t.Fatal(err)
	}
	startup, cancel := context.WithTimeout(context.Background(), productionStartupTimeout)
	defer cancel()
	open, err := openProduction(startup, layout, productionConfig{
		Schema: contract, MaxDatabaseBytes: productionMaxDatabaseBytes,
	})
	if err != nil {
		t.Fatal(err)
	}
	server := transport.New(migrationRegistry(t), security.NewOrigins("http://"+productionDurableAddress),
		"go-production-knowledge-reopen")
	t.Cleanup(func() {
		server.Close()
		if closeErr := open.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if bindErr := server.BindProjects(open.durable); bindErr != nil {
		t.Fatal(bindErr)
	}
	imports := newProductionKnowledgeImports(open.durable)
	if bindErr := imports.Bind(server); bindErr != nil {
		t.Fatalf("bind the served knowledge imports: %v", bindErr)
	}
	if bindErr := newProductionKnowledgeUploads(imports, uploads).Bind(server); bindErr != nil {
		t.Fatalf("bind the knowledge upload names: %v", bindErr)
	}
	return server, open
}

// TestProductionKnowledgeImportThenReopenAdmitsTheProject is the regression test
// for the lockout: one kb:import-document through the served operation, then a
// fresh process over the same root, whose project:open is a first admission.
func TestProductionKnowledgeImportThenReopenAdmitsTheProject(t *testing.T) {
	root, projectPath, contract, _, _ := productionEditorReadsRoot(t)
	uploads := newProductionPickerFixtureUploads()
	server, open := productionKnowledgeReopenLane(t, root, contract, uploads)
	author, frozen := productionExecuteSession(t, server, projectPath, productionKnowledgeReopenNonce)
	uploads.stage(author.cookie.Value, author.tab, "upload-reopen-1", productionKnowledgeNamedFile,
		[]byte(productionKnowledgeProse))
	answer := productionKnowledgeCall(t, author, productionKnowledgeImportDocumentName,
		[]any{"upload-reopen-1", projectPath, frozen})
	if answer["success"] != true {
		t.Fatalf("kb:import-document refused an importable file: %v", answer)
	}
	storage := filepath.Join(projectPath, projectfs.StorageDirectory)
	if _, err := os.Stat(filepath.Join(storage, productionKnowledgeIndexName)); err != nil {
		t.Fatalf("the import wrote no canonical snapshot: %v", err)
	}
	if _, err := os.Stat(filepath.Join(storage, productionKnowledgeCopyDirectory)); err != nil {
		t.Fatalf("the import wrote no knowledge copies: %v", err)
	}
	if err := open.Close(); err != nil {
		t.Fatalf("the import lane did not close cleanly: %v", err)
	}

	reopened, _ := productionKnowledgeReopenLane(t, root, contract, newProductionPickerFixtureUploads())
	client := newProductionDurableClient(t, reopened, "client_knowledge_reopen")
	var opened productionDurableOpenWire
	client.decode(t, "project:open", []any{projectPath, productionKnowledgeReopenNonce, projectPath}, &opened)
	if !opened.Success || !opened.DBReady || opened.Error != "" {
		t.Fatalf("reopening a project after a knowledge import failed: %+v", opened)
	}
}

// TestProductionKnowledgeReopenRefusesCopiesWithoutCanonicalSnapshot keeps the
// desktop refusal: copies with no decodable canonical snapshot are knowledge no
// Go writer can maintain, and the symbolic code is unchanged.
func TestProductionKnowledgeReopenRefusesCopiesWithoutCanonicalSnapshot(t *testing.T) {
	root, projectPath, contract, _, _ := productionEditorReadsRoot(t)
	directory := filepath.Join(projectPath, projectfs.StorageDirectory, productionKnowledgeCopyDirectory)
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("synthetic-desktop-document"))
	copied := filepath.Join(directory, hex.EncodeToString(digest[:])+productionKnowledgeCopySuffix)
	if err := os.WriteFile(copied, []byte(`{"content":"门后有人。","indexedHash":"0","edited":false,"indexDirty":false}`), 0600); err != nil {
		t.Fatal(err)
	}

	server, _ := productionKnowledgeReopenLane(t, root, contract, newProductionPickerFixtureUploads())
	client := newProductionDurableClient(t, server, "client_knowledge_reopen")
	var opened productionDurableOpenWire
	client.decode(t, "project:open", []any{projectPath, productionKnowledgeReopenNonce, projectPath}, &opened)
	if opened.Success || opened.Error != "AUTHOR_WRITE_KNOWLEDGE_UNPROVED" {
		t.Fatalf("a copy footprint without a canonical snapshot answered %+v; want AUTHOR_WRITE_KNOWLEDGE_UNPROVED", opened)
	}
}
