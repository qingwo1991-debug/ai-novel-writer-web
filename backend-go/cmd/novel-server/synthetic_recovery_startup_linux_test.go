//go:build linux

package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/admission"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/authorruntime"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

func TestSyntheticReadRecoveryStagePersistsAndBlocksRestart(t *testing.T) {
	dataRoot := t.TempDir()
	if err := os.Chmod(dataRoot, 0700); err != nil {
		t.Fatal(err)
	}
	libraryDir := filepath.Join(dataRoot, "projects")
	if err := os.Mkdir(libraryDir, 0700); err != nil {
		t.Fatal(err)
	}
	library, err := projectfs.OpenLibrary(libraryDir)
	if err != nil {
		t.Fatal(err)
	}
	defer library.Close()
	config := syntheticReadOpenConfig{Schema: admission.SchemaContract{{Version: 7, Fingerprints: []string{"synthetic-only"}}}}
	first, err := newSyntheticReadOpen(library, libraryDir, config)
	if err != nil {
		t.Fatal(err)
	}
	stageParent := filepath.Join(dataRoot, syntheticStageParentName)
	if info, err := os.Lstat(stageParent); err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		t.Fatalf("persistent stage parent missing or unsafe: %v %+v", err, info)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(stageParent); err != nil {
		t.Fatalf("closing a reader removed the recovery parent: %v", err)
	}
	artifact := filepath.Join(stageParent, "unknown-journal")
	if err := os.WriteFile(artifact, []byte("untrusted"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := newSyntheticReadOpen(library, libraryDir, config); err == nil {
		t.Fatal("restart admitted an unknown recovery artifact")
	}
	if err := os.Remove(artifact); err != nil {
		t.Fatal(err)
	}
	second, err := newSyntheticReadOpen(library, libraryDir, config)
	if err != nil {
		t.Fatalf("failed restart retained the writer lock: %v", err)
	}
	if err := os.WriteFile(artifact, []byte("untrusted"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := second.VerifyRecovery(); err == nil {
		t.Fatal("live reader accepted a new unknown recovery artifact")
	}
	if _, err := second.OpenReadOnlyProject(context.Background(), transport.Caller{SessionID: "synthetic", ClientID: "client_1234567890"}, filepath.Join(libraryDir, "missing"), "open"); !errors.Is(err, authorruntime.ErrCatalogRecoveryUnverified) {
		t.Fatalf("project open reached listing while recovery was pending: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(artifact); err != nil {
		t.Fatal(err)
	}
	handler, cleanup, err := migrationHandlerWithModelsAndReadOpen(migrationRegistry(t), "127.0.0.1:16082", "", libraryDir, "", "", nil, &config)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if err := os.WriteFile(artifact, []byte("untrusted"), 0600); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/library", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("raw library route bypassed recovery: %d %s", response.Code, response.Body.String())
	}
}

func TestSyntheticLibraryOnlyRouteCannotBypassRecovery(t *testing.T) {
	dataRoot := t.TempDir()
	if err := os.Chmod(dataRoot, 0700); err != nil {
		t.Fatal(err)
	}
	libraryDir := filepath.Join(dataRoot, "projects")
	if err := os.Mkdir(libraryDir, 0700); err != nil {
		t.Fatal(err)
	}
	handler, cleanup, err := migrationHandler(migrationRegistry(t), "127.0.0.1:16082", "", libraryDir)
	if err != nil {
		t.Fatal(err)
	}
	stageParent := filepath.Join(dataRoot, syntheticStageParentName)
	artifact := filepath.Join(stageParent, "unknown-journal")
	if err := os.WriteFile(artifact, []byte("untrusted"), 0600); err != nil {
		cleanup()
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/library", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		cleanup()
		t.Fatalf("library-only route bypassed recovery: %d %s", response.Code, response.Body.String())
	}
	cleanup()
	if _, secondCleanup, err := migrationHandler(migrationRegistry(t), "127.0.0.1:16082", "", libraryDir); err == nil {
		secondCleanup()
		t.Fatal("library-only restart admitted an unknown recovery artifact")
	}
}
