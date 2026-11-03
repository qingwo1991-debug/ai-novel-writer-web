//go:build linux && amd64

package authorruntime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectcatalog"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectpublish"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/writerlock"
)

func TestDraftCommitServiceSyntheticCASAndCatalogReadback(t *testing.T) {
	_, _, config, source, _ := readFixture(t)
	ctx := context.Background()
	seed, err := sql.Open("sqlite3", source)
	if err != nil {
		t.Fatal(err)
	}
	inserted, err := seed.Exec("INSERT INTO contents(body) VALUES(?)", "old draft")
	if err != nil {
		t.Fatal(err)
	}
	contentID, err := inserted.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	var chapter int64
	if err := seed.QueryRow("SELECT COALESCE(MAX(chapter_number),0)+1 FROM drafts").Scan(&chapter); err != nil {
		t.Fatal(err)
	}
	inserted, err = seed.Exec("INSERT INTO drafts(chapter_number,version,status,content_id,word_count) VALUES(?,1,'draft',?,9)", chapter, contentID)
	if err != nil {
		t.Fatal(err)
	}
	draftID, err := inserted.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
	oldBytes, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	library, err := projectfs.OpenLibrary(config.LibraryRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer library.Close()
	held, err := writerlock.AcquireLegacy(config.DataRoot)
	if err != nil {
		t.Fatal(err)
	}
	stageParent := filepath.Join(config.DataRoot, "draft-commit-stage")
	if err := os.Mkdir(stageParent, 0700); err != nil {
		t.Fatal(err)
	}
	const changed = "new private words"
	var retainedPath string
	readback := func(ctx context.Context, path, id string, recovery projectpublish.JournalRecovery) error {
		if path != config.ProjectPath || id != fixtureProjectID() || recovery.EditedDraftID != draftID {
			return ErrSwitchedDraftUnverified
		}
		journalName := "novel-exchange-" + recovery.JournalID + ".prepared.json"
		facts, err := VerifyRestartedSwitchedDraftUnderLock(ctx, held, library, id, path,
			stageParent, journalName, config)
		if err != nil {
			return err
		}
		hash := sha256.Sum256([]byte(changed))
		if facts.ContentSHA256 != hex.EncodeToString(hash[:]) || facts.WordCount != 17 {
			return ErrSwitchedDraftUnverified
		}
		retainedPath = recovery.StagePath
		return nil
	}
	check, err := CatalogJournalRecoveryCheck(ctx, held, config.DataRoot, stageParent, readback)
	if err != nil {
		t.Fatal(err)
	}
	metadata := t.TempDir()
	if err := os.Chmod(metadata, 0700); err != nil {
		t.Fatal(err)
	}
	catalog, err := projectcatalog.OpenWithRecoveryCheck(library, metadata, check)
	if err != nil {
		t.Fatal(err)
	}
	project, err := library.OpenProject(config.ProjectPath)
	if err != nil {
		t.Fatal(err)
	}
	initial, err := catalog.Register(project)
	if closeErr := project.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	lifecycle, err := NewReadLifecycleWithHeldLock(catalog, configWithoutProject(config), held)
	if err != nil {
		_ = held.Close()
		t.Fatal(err)
	}
	poisoned := false
	defer func() {
		err := lifecycle.Shutdown()
		if poisoned {
			if !errors.Is(err, projectsession.ErrActiveCleanup) {
				t.Errorf("uncertain commit released coordinator: %v", err)
			}
		} else if err != nil {
			t.Errorf("shutdown: %v", err)
		}
	}()
	owner := projectsession.Owner{SessionID: "synthetic-cookie", ClientID: "synthetic-tab"}
	oldLease, err := lifecycle.Open(ctx, owner, initial.Key, "initial-open")
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewDraftCommitService(lifecycle, library, stageParent,
		projectpublish.Limits{MaxFiles: 1000, MaxBytes: 256 << 20, MaxDepth: 16})
	if err != nil {
		t.Fatal(err)
	}
	expectedBody := "old draft"
	result, err := service.CommitDraftCAS(ctx, DraftCommitRequest{Owner: owner, Expected: oldLease,
		CatalogKey: initial.Key, DraftID: draftID, Content: changed, WordCount: 17,
		ExpectedContent: &expectedBody})
	if err != nil {
		t.Fatal(err)
	}
	if result.Key == "" || result.Key == initial.Key || result.Context.LeaseID == oldLease.LeaseID ||
		result.Readback.DraftID != draftID || result.Readback.WordCount != 17 {
		t.Fatalf("unverified commit result: %+v", result)
	}
	if err := lifecycle.Read(ctx, owner, result.Context, func(tx *sql.Tx) error {
		var body string
		if err := tx.QueryRow("SELECT c.body FROM drafts d JOIN contents c ON c.id=d.content_id WHERE d.id=?", draftID).Scan(&body); err != nil {
			return err
		}
		if body != changed {
			return ErrChanged
		}
		return nil
	}); err != nil {
		t.Fatalf("fresh catalog-backed read: %v", err)
	}
	if _, err := catalog.Resolve(initial.Key); !errors.Is(err, projectcatalog.ErrCatalogKey) {
		t.Fatalf("old catalog key survived: %v", err)
	}
	retained, err := os.ReadFile(filepath.Join(retainedPath, projectfs.DatabaseName))
	if err != nil || !bytes.Equal(retained, oldBytes) {
		t.Fatalf("old source bytes were not retained: %v", err)
	}
	currentBytes, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	stale := "old draft"
	_, err = service.CommitDraftCAS(ctx, DraftCommitRequest{Owner: owner, Expected: result.Context,
		CatalogKey: result.Key, DraftID: draftID, Content: "must not publish", WordCount: 16,
		ExpectedContent: &stale})
	if !errors.Is(err, store.ErrDraftContentConflict) {
		t.Fatalf("stale CAS received a success: %v", err)
	}
	if err := lifecycle.Read(ctx, owner, result.Context, func(*sql.Tx) error { return nil }); err != nil {
		t.Fatalf("preflight conflict poisoned healthy reader: %v", err)
	}
	afterConflict, err := os.ReadFile(source)
	if err != nil || !bytes.Equal(afterConflict, currentBytes) {
		t.Fatalf("failed CAS changed the author source: %v", err)
	}
	// A failure after the action begins has an unknown publication outcome and
	// must retain the writer lock. The tiny stage budget fails inside Stage.
	failingService, err := NewDraftCommitService(lifecycle, library, stageParent,
		projectpublish.Limits{MaxFiles: 2, MaxBytes: 1, MaxDepth: 1})
	if err != nil {
		t.Fatal(err)
	}
	currentBody := changed
	_, err = failingService.CommitDraftCAS(ctx, DraftCommitRequest{Owner: owner, Expected: result.Context,
		CatalogKey: result.Key, DraftID: draftID, Content: "unpublished", WordCount: 11,
		ExpectedContent: &currentBody})
	poisoned = true
	if !errors.Is(err, projectsession.ErrCommitOutcomeUnknown) {
		t.Fatalf("in-action stage failure did not seal outcome: %v", err)
	}
	readWasCalled := false
	err = lifecycle.Read(ctx, owner, result.Context, func(*sql.Tx) error {
		readWasCalled = true
		return nil
	})
	if !errors.Is(err, projectsession.ErrActiveClosed) || readWasCalled {
		t.Fatalf("uncertain commit served a fresh-lease read: %v, callback called: %t", err, readWasCalled)
	}
	if _, err := lifecycle.Open(ctx, owner, result.Key, "retry-after-uncertain-cas"); !errors.Is(err, projectsession.ErrActiveClosed) {
		t.Fatalf("uncertain commit admitted a new reader: %v", err)
	}
	if err := held.Close(); !errors.Is(err, writerlock.ErrRetained) {
		t.Fatalf("uncertain commit released the legacy writer lock: %v", err)
	}
}
