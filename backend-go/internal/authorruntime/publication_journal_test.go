//go:build linux

package authorruntime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/writerlock"
)

func preparedPublicationFixture(t *testing.T) (dataRoot, libraryRoot, projectPath, source, backupPath, scratch string, original []byte) {
	t.Helper()
	catalog, key, config, source, original := readFixture(t)
	config.Knowledge = emptyKnowledgeConfig()
	reader, err := OpenReadOnly(context.Background(), catalog, key, config)
	if err != nil {
		t.Fatal(err)
	}
	backupParent := t.TempDir()
	if err := os.Chmod(backupParent, 0700); err != nil {
		t.Fatal(err)
	}
	backup, err := reader.SealDatabaseSnapshot(context.Background(), backupParent, 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := reader.PrepareSQLitePublicationPlan(context.Background(), config.Schema, backup)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte("synthetic guarded CAS"))
	intent, err := PreparePublicationIntent(context.Background(), plan, hex.EncodeToString(hash[:]))
	if err != nil || intent.Version != 1 || intent.ProjectID == "" || intent.IntentID == "" {
		t.Fatalf("prepare durable intent: %+v %v", intent, err)
	}
	if _, err := PreparePublicationIntent(context.Background(), plan, hex.EncodeToString(hash[:])); !errors.Is(err, ErrPublicationJournalExists) {
		t.Fatalf("duplicate intent accepted: %v", err)
	}
	backupPath = backup.Directory()
	if err := backup.Close(); err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	return config.DataRoot, config.LibraryRoot, config.ProjectPath, source, backupPath, config.ScratchParent, original
}

func recoverPreparedFixture(t *testing.T, dataRoot, libraryRoot, projectPath, backupPath, scratch string) (PublicationRecovery, error) {
	t.Helper()
	library, err := projectfs.OpenLibrary(libraryRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer library.Close()
	lock, err := writerlock.AcquireLegacy(dataRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	return RecoverPublicationIntent(context.Background(), lock, dataRoot, library, projectPath, backupPath, scratch, 64<<20)
}

func TestPreparedPublicationIntentSurvivesRestartAndClassifiesUnchangedSource(t *testing.T) {
	dataRoot, libraryRoot, projectPath, source, backupPath, scratch, original := preparedPublicationFixture(t)
	result, err := recoverPreparedFixture(t, dataRoot, libraryRoot, projectPath, backupPath, scratch)
	if err != nil || result.Class != "source-unchanged" || result.Intent.ProjectID == "" || result.Intent.Before.Members[0].Name != "snapshot.db" {
		t.Fatalf("restart recovery lost exact pre-write state: %+v %v", result, err)
	}
	after, err := os.ReadFile(source)
	if err != nil || !bytes.Equal(after, original) {
		t.Fatal("preparation changed author bytes", err)
	}
	if _, err := os.Stat(filepath.Join(backupPath, publicationJournalName)); err != nil {
		t.Fatal("prepared marker did not persist", err)
	}
}

func TestPreparedPublicationIntentClassifiesChangedSourceWithoutRestoring(t *testing.T) {
	dataRoot, libraryRoot, projectPath, source, backupPath, scratch, original := preparedPublicationFixture(t)
	file, err := os.OpenFile(source, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("synthetic interrupted publisher")); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	result, err := recoverPreparedFixture(t, dataRoot, libraryRoot, projectPath, backupPath, scratch)
	if err != nil || result.Class != "source-changed" {
		t.Fatalf("changed source classified as safe: %+v %v", result, err)
	}
	after, err := os.ReadFile(source)
	if err != nil || !bytes.Equal(after, append(append([]byte(nil), original...), []byte("synthetic interrupted publisher")...)) {
		t.Fatal("recovery silently restored or changed author bytes", err)
	}
}

func TestPreparedPublicationIntentRejectsTamperedBackupBeforeSourceInspection(t *testing.T) {
	dataRoot, libraryRoot, projectPath, _, backupPath, scratch, _ := preparedPublicationFixture(t)
	file, err := os.OpenFile(filepath.Join(backupPath, sealedSnapshotManifest), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("tampered")); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := recoverPreparedFixture(t, dataRoot, libraryRoot, projectPath, backupPath, scratch); !errors.Is(err, ErrPublicationJournalInvalid) {
		t.Fatalf("tampered manifest admitted: %v", err)
	}
}

func TestPreparedPublicationIntentClassifiesMissingSourceForOperatorReview(t *testing.T) {
	dataRoot, libraryRoot, projectPath, source, backupPath, scratch, _ := preparedPublicationFixture(t)
	if err := os.Remove(source); err != nil {
		t.Fatal(err)
	}
	result, err := recoverPreparedFixture(t, dataRoot, libraryRoot, projectPath, backupPath, scratch)
	if err != nil || result.Class != "source-unreadable" {
		t.Fatalf("missing source was treated as a completed rollback: %+v %v", result, err)
	}
	if _, err := os.Stat(source); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recovery created source bytes: %v", err)
	}
}
