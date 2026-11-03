//go:build linux

package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/admission"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/authorruntime"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/writerlock"
)

func TestSyntheticReadCloseDrainsExactLeaseWithoutChangingSource(t *testing.T) {
	oracle := os.Getenv("GO_SQLITE_CONTRACT")
	if oracle == "" {
		if os.Getenv("GITHUB_ACTIONS") == "true" {
			t.Fatal("original schema oracle required")
		}
		t.Skip("GitHub original schema oracle required")
	}
	file, err := os.Open(oracle)
	if err != nil {
		t.Fatal(err)
	}
	contract, err := admission.ParseContract(file)
	_ = file.Close()
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := os.ReadFile(filepath.Join(filepath.Dir(oracle), "baseline-v7.db"))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	libraryDir := filepath.Join(root, "projects")
	projectPath := filepath.Join(libraryDir, "close-project")
	storage := filepath.Join(projectPath, projectfs.StorageDirectory)
	if err := os.MkdirAll(storage, 0700); err != nil {
		t.Fatal(err)
	}
	manifest := `{"schemaVersion":1,"kind":"ai-novel-project","projectId":"8b5929e6-cda4-43e2-a15c-f232571f0036","createdAt":"2026-01-01T00:00:00.000Z","storageFormat":"ai-novel","storageVersion":1}`
	if err := os.WriteFile(filepath.Join(storage, projectfs.ManifestName), []byte(manifest), 0600); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(storage, projectfs.DatabaseName)
	if err := os.WriteFile(source, baseline, 0600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", source)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO project_core(id, project_name, writing_language, total_chapters, words_per_chapter, character_states, created_at, updated_at) VALUES('main', 'Close test', 'zh-CN', 120, 2000, '{}', '2026-01-01 00:00:00', '2026-01-01 00:00:00')`)
	closeErr := db.Close()
	if err != nil || closeErr != nil {
		t.Fatal(err, closeErr)
	}
	before, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	library, err := projectfs.OpenLibrary(libraryDir)
	if err != nil {
		t.Fatal(err)
	}
	defer library.Close()
	bridge, err := newSyntheticReadOpen(library, libraryDir, syntheticReadOpenConfig{Schema: contract})
	if err != nil {
		t.Fatal(err)
	}
	defer bridge.Close()
	caller := transport.Caller{Principal: "synthetic-author", SessionID: "cookie-session-1", ClientID: "client_1234567890"}
	opened, err := bridge.OpenReadOnlyProject(context.Background(), caller, projectPath, "close-test-open")
	if err != nil {
		t.Fatal(err)
	}
	result, ok := opened.(authorruntime.ProjectOpenResult)
	if !ok || !result.Success {
		t.Fatalf("project was not admitted: %+v", opened)
	}
	encoded, err := json.Marshal(map[string]any{"projectId": result.Project["id"], "leaseId": result.Project["sessionLease"], "projectPath": projectPath})
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(encoded, &raw); err != nil {
		t.Fatal(err)
	}
	expected, err := projectsession.ParseContext(raw)
	if err != nil {
		t.Fatal(err)
	}
	foreign := caller
	foreign.ClientID = "client_0987654321"
	if _, err := bridge.CloseReadOnlyProject(context.Background(), foreign, projectPath, raw); err == nil {
		t.Fatal("foreign tab closed project")
	}
	if _, err := bridge.CloseReadOnlyProject(context.Background(), caller, filepath.Join(root, "other"), raw); err == nil {
		t.Fatal("different path closed project")
	}
	started := make(chan struct{})
	release := make(chan struct{})
	readDone := make(chan error, 1)
	owner := projectsession.Owner{SessionID: caller.SessionID, ClientID: caller.ClientID}
	go func() {
		readDone <- bridge.lifecycle.Read(context.Background(), owner, expected, func(*sql.Tx) error {
			close(started)
			<-release
			return nil
		})
	}()
	<-started
	type closeOutcome struct {
		result any
		err    error
	}
	closeDone := make(chan closeOutcome, 1)
	go func() {
		result, err := bridge.CloseReadOnlyProject(context.Background(), caller, projectPath, raw)
		closeDone <- closeOutcome{result: result, err: err}
	}()
	select {
	case outcome := <-closeDone:
		t.Fatalf("close passed an in-flight read: %v", outcome.err)
	default:
	}
	close(release)
	if err := <-readDone; err != nil {
		t.Fatal(err)
	}
	if outcome := <-closeDone; outcome.err != nil || !reflect.DeepEqual(outcome.result, map[string]any{"success": true}) {
		t.Fatal("close outcome was not confirmed", outcome.result, outcome.err)
	}
	if _, present, err := bridge.lifecycle.Current(context.Background(), owner); err != nil || present {
		t.Fatal("close did not remove exact lease", present, err)
	}
	if _, err := bridge.CloseReadOnlyProject(context.Background(), caller, projectPath, raw); err == nil {
		t.Fatal("replayed close reported success")
	}
	if contender, err := writerlock.AcquireLegacy(root); !errors.Is(err, writerlock.ErrLocked) {
		if contender != nil {
			_ = contender.Close()
		}
		t.Fatal("shared gateway lock released before shutdown", err)
	}
	after, err := os.ReadFile(source)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("close changed author source", err)
	}
	if err := bridge.Close(); err != nil {
		t.Fatal(err)
	}
	lock, err := writerlock.AcquireLegacy(root)
	if err != nil {
		t.Fatal("shutdown did not release gateway lock", err)
	}
	_ = lock.Close()
}
