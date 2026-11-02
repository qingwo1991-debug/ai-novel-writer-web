//go:build lance_native && cgo

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/knowledgecopy"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/knowledgedomain"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/lancenative"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
)

// runStaged is CI fixture setup, never application admission. It creates a fake
// project with opaque SQLite bytes so the isolated asset copier can be composed
// with the real native reader and Go table-domain validator. SQLite admission,
// copy-document crosslinks and canonical hashes remain separate gates; this
// function must not be exposed to any user operation.
func runStaged(ctx context.Context, root string) (err error) {
	libraryPath := filepath.Join(root, "stage-library")
	projectPath := filepath.Join(libraryPath, "synthetic")
	storage := filepath.Join(projectPath, ".ai-novel")
	if err := os.MkdirAll(storage, 0700); err != nil {
		return err
	}
	if err := os.CopyFS(storage, os.DirFS(filepath.Join(root, "copy"))); err != nil {
		return err
	}
	manifest := []byte(`{"schemaVersion":1,"kind":"ai-novel-project","projectId":"8b5929e6-cda4-43e2-a15c-f232571f0036","createdAt":"2026-01-01T00:00:00.000Z","storageFormat":"ai-novel","storageVersion":1}`)
	for name, data := range map[string][]byte{"project.json": manifest, "project.db": []byte("synthetic opaque DB not SQLite admission")} {
		if err := os.WriteFile(filepath.Join(storage, name), data, 0600); err != nil {
			return err
		}
	}
	library, err := projectfs.OpenLibrary(libraryPath)
	if err != nil {
		return err
	}
	defer library.Close()
	project, err := library.OpenProject(projectPath)
	if err != nil {
		return err
	}
	defer project.Close()
	destinationPath := filepath.Join(root, "go-staged")
	ipc := filepath.Join(root, "native-staged-ipc")
	for _, path := range []string{destinationPath, ipc} {
		if err := os.Mkdir(path, 0700); err != nil {
			return err
		}
	}
	destination, err := os.OpenRoot(destinationPath)
	if err != nil {
		return err
	}
	defer destination.Close()
	proof, err := project.StageCanonicalKnowledge(ctx, destination, projectfs.KnowledgeBudget{MaxBytes: 128 * 1024 * 1024, MaxEntries: 10000, MaxDepth: 32})
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, proof.Cleanup()) }()
	metadata, err := knowledgecopy.Validate(ctx, destination, knowledgecopy.Budget{MaxMetadataBytes: 8 * 1024 * 1024, MaxCopies: 100})
	if err != nil {
		return err
	}
	exported, err := lancenative.Export(ctx, filepath.Join(destinationPath, "lancedb"), ipc)
	if err != nil {
		return err
	}
	raw, err := lancenative.ReadDomain(ctx, ipc)
	if err != nil {
		return err
	}
	var snapshot knowledgedomain.Snapshot
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&snapshot); err != nil {
		return errors.New("DOMAIN_SNAPSHOT_INVALID")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return errors.New("DOMAIN_SNAPSHOT_TRAILING")
	}
	registryFile, err := destination.Open("embedding-spaces.json")
	if err != nil {
		return err
	}
	registry, readErr := io.ReadAll(io.LimitReader(registryFile, 8*1024*1024+1))
	closeErr := registryFile.Close()
	if readErr != nil || closeErr != nil || len(registry) > 8*1024*1024 {
		return errors.New("DOMAIN_REGISTRY_READ")
	}
	snapshot.Registry = registry
	domain, err := knowledgedomain.Validate(ctx, snapshot, knowledgedomain.Budget{MaxTables: 32, MaxRows: 10000, MaxCells: 200000, MaxTextBytes: 32 * 1024 * 1024})
	if err != nil {
		return err
	}
	if err := save(filepath.Join(root, "native-domain-summary.json"), domain); err != nil {
		return err
	}
	refs := []knowledgecopy.DocumentRef{}
	for _, table := range snapshot.Tables {
		if table.Name == "documents" {
			for _, row := range table.Records {
				refs = append(refs, knowledgecopy.DocumentRef{ID: row["id"].(string), FilePath: row["filePath"].(string)})
			}
		}
	}
	links, err := knowledgecopy.ValidateLinks(ctx, destination, refs, knowledgecopy.Budget{MaxMetadataBytes: 8 * 1024 * 1024, MaxCopies: 100})
	if err != nil {
		return err
	}
	if err := save(filepath.Join(root, "native-copy-links.json"), links); err != nil {
		return err
	}
	if err := proof.VerifyUnchanged(ctx); err != nil {
		return err
	}
	if err := proof.VerifyCopy(ctx); err != nil {
		return err
	}
	if err := save(filepath.Join(root, "native-staged-export.json"), exported); err != nil {
		return err
	}
	return save(filepath.Join(root, "native-staging-proof.json"), struct {
		Physical projectfs.KnowledgeSummary `json:"physical"`
		Metadata knowledgecopy.Summary      `json:"metadata"`
	}{proof.Summary(), metadata})
}
