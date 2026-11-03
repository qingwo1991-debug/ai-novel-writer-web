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

func TestSwitchedDraftReadbackUsesPinnedSourceWithoutCatalogKey(t *testing.T) {
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
	defer library.Close()
	held, err := writerlock.AcquireLegacy(config.DataRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	stageParent := filepath.Join(config.DataRoot, "private-publish")
	if err := os.Mkdir(stageParent, 0700); err != nil {
		t.Fatal(err)
	}
	snapshot, err := projectpublish.Stage(ctx, held, config.DataRoot, library, config.ProjectPath,
		stageParent, projectpublish.Limits{MaxFiles: 1000, MaxBytes: 256 << 20, MaxDepth: 16})
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()
	baseline, changed := "old draft", "new private words"
	evidence, err := snapshot.EditDraftCASPrivate(ctx, draftID, changed, 17, &baseline)
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
	if err := journal.MarkSwitched(ctx); err != nil {
		t.Fatal(err)
	}
	recovery, err := projectpublish.RecoverExchangeJournal(ctx, held, config.DataRoot,
		fixtureProjectID(), config.ProjectPath, stageParent, journal.Name())
	if err != nil || recovery.Class != projectpublish.JournalSwitched {
		t.Fatalf("journal recovery: %+v %v", recovery, err)
	}
	expected := SwitchedDraftExpectation{ProjectID: fixtureProjectID(), DraftID: draftID,
		Content: changed, WordCount: 17,
		EditedTreeHash:      hex.EncodeToString(evidence.AfterStageTreeSHA256[:]),
		EditedOperationHash: recovery.EditedOperationHash}
	wrong := expected
	wrong.Content = "different body"
	if _, err := VerifySwitchedDraftUnderLock(ctx, held, library, snapshot, config.ProjectPath,
		stageParent, journal.Name(), config, wrong); !errors.Is(err, ErrSwitchedDraftUnverified) {
		t.Fatalf("changed CAS body accepted: %v", err)
	}
	readback, err := VerifySwitchedDraftUnderLock(ctx, held, library, snapshot,
		config.ProjectPath, stageParent, journal.Name(), config, expected)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(changed))
	if readback.ProjectID != expected.ProjectID || readback.DraftID != draftID ||
		readback.WordCount != 17 || readback.ContentSHA256 != hex.EncodeToString(digest[:]) ||
		readback.ReadbackSHA256 != recovery.EditedProofHash || readback.NewSourceInode == 0 {
		t.Fatalf("incomplete switched readback: %+v", readback)
	}
	badCount := expected
	badCount.WordCount++
	if _, err := VerifySwitchedDraftUnderLock(ctx, held, library, snapshot, config.ProjectPath,
		stageParent, journal.Name(), config, badCount); !errors.Is(err, ErrSwitchedDraftUnverified) {
		t.Fatalf("wrong word count accepted: %v", err)
	}
	if err := os.WriteFile(filepath.Join(storage, projectfs.DatabaseName), []byte("changed after marker"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifySwitchedDraftUnderLock(ctx, held, library, snapshot, config.ProjectPath,
		stageParent, journal.Name(), config, expected); !errors.Is(err, ErrSwitchedDraftUnverified) {
		t.Fatalf("changed visible tree accepted: %v", err)
	}
}

func fixtureProjectID() string { return "8b5929e6-cda4-43e2-a15c-f232571f0036" }
