//go:build linux

package authorruntime

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectpublish"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/writerlock"
)

func TestRestartedSwitchedDraftReadbackUsesDurableJournal(t *testing.T) {
	_, _, config, source, _ := readFixture(t)
	ctx := context.Background()
	seed, err := sql.Open("sqlite3", source)
	if err != nil {
		t.Fatal(err)
	}
	contentResult, err := seed.Exec("INSERT INTO contents(body) VALUES(?)", "old draft")
	if err != nil {
		t.Fatal(err)
	}
	contentID, err := contentResult.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	var chapter int64
	if err := seed.QueryRow("SELECT COALESCE(MAX(chapter_number),0)+1 FROM drafts").Scan(&chapter); err != nil {
		t.Fatal(err)
	}
	draftResult, err := seed.Exec("INSERT INTO drafts(chapter_number,version,status,content_id,word_count) VALUES(?,1,'draft',?,9)", chapter, contentID)
	if err != nil {
		t.Fatal(err)
	}
	draftID, err := draftResult.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
	library, err := projectfs.OpenLibrary(config.LibraryRoot)
	if err != nil {
		t.Fatal(err)
	}
	held, err := writerlock.AcquireLegacy(config.DataRoot)
	if err != nil {
		t.Fatal(err)
	}
	stageParent := filepath.Join(config.DataRoot, "private-publish")
	if err := os.Mkdir(stageParent, 0700); err != nil {
		t.Fatal(err)
	}
	snapshot, err := projectpublish.Stage(ctx, held, config.DataRoot, library, config.ProjectPath,
		stageParent, projectpublish.Limits{MaxFiles: 1000, MaxBytes: 256 << 20, MaxDepth: 16})
	if err != nil {
		t.Fatal(err)
	}
	changed := "new private words"
	evidence, err := snapshot.EditDraftCASPrivate(ctx, draftID, changed, 17, stringPointer("old draft"))
	if err != nil {
		t.Fatal(err)
	}
	journal, err := projectpublish.PrepareEditedExchangeJournal(ctx, snapshot,
		projectpublish.HandleDrainFunc(func(context.Context, string) error { return nil }), evidence)
	if err != nil {
		t.Fatal(err)
	}
	stage, err := snapshot.PrivatePath()
	if err != nil {
		t.Fatal(err)
	}
	storage := filepath.Join(config.ProjectPath, projectfs.StorageDirectory)
	moving := stage + "-moving"
	for _, pair := range [][2]string{{storage, moving}, {stage, storage}, {moving, stage}} {
		if err := os.Rename(pair[0], pair[1]); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := VerifyRestartedSwitchedDraftUnderLock(ctx, held, library, fixtureProjectID(),
		config.ProjectPath, stageParent, journal.Name(), config); !errors.Is(err, ErrSwitchedDraftUnverified) {
		t.Fatalf("unconfirmed exchange accepted: %v", err)
	}
	if err := journal.MarkSwitched(ctx); err != nil {
		t.Fatal(err)
	}
	journalName := journal.Name()
	if err := snapshot.Close(); err != nil {
		t.Fatal(err)
	}
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
	if err := library.Close(); err != nil {
		t.Fatal(err)
	}
	library, err = projectfs.OpenLibrary(config.LibraryRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer library.Close()
	held, err = writerlock.AcquireLegacy(config.DataRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	readback, err := VerifyRestartedSwitchedDraftUnderLock(ctx, held, library, fixtureProjectID(),
		config.ProjectPath, stageParent, journalName, config)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(changed))
	if readback.ProjectID != fixtureProjectID() || readback.DraftID != draftID ||
		readback.WordCount != 17 || readback.ContentSHA256 != hex.EncodeToString(digest[:]) ||
		readback.NewSourceInode == 0 || readback.EditedTreeHash == "" || readback.ReadbackSHA256 == "" {
		t.Fatalf("incomplete restarted readback: %+v", readback)
	}
	recovery, err := projectpublish.RecoverExchangeJournal(ctx, held, config.DataRoot,
		fixtureProjectID(), config.ProjectPath, stageParent, journalName)
	if err != nil || recovery.Class != projectpublish.JournalSwitched {
		t.Fatalf("verified journal: %+v %v", recovery, err)
	}
	// The old source has the same project ID. Its pinned storage inode must not
	// satisfy the journal's new-source identity even if a path is substituted.
	newMoving := filepath.Join(config.DataRoot, "new-source-moving")
	if err := os.Rename(storage, newMoving); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(stage, storage); err != nil {
		t.Fatal(err)
	}
	replacement, err := library.OpenProject(config.ProjectPath)
	if err != nil {
		t.Fatal(err)
	}
	if restartedProjectMatchesRecovery(replacement, recovery) {
		t.Fatal("same-ID old source matched verified new-source inode")
	}
	if err := replacement.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(storage, stage); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(newMoving, storage); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(storage, projectfs.DatabaseName), []byte("changed after restart"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyRestartedSwitchedDraftUnderLock(ctx, held, library, fixtureProjectID(),
		config.ProjectPath, stageParent, journalName, config); !errors.Is(err, ErrSwitchedDraftUnverified) {
		t.Fatalf("changed visible source accepted: %v", err)
	}
}

func stringPointer(value string) *string { return &value }
