//go:build linux

package authorruntime

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/writerlock"
)

func TestSQLitePublicationPlanPreservesEvidenceAndRefusesSourceWrite(t *testing.T) {
	catalog, key, config, source, original := readFixture(t)
	config.Knowledge = emptyKnowledgeConfig()
	reader, err := OpenReadOnly(context.Background(), catalog, key, config)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	parent := t.TempDir()
	if err := os.Chmod(parent, 0700); err != nil {
		t.Fatal(err)
	}
	backup, err := reader.SealDatabaseSnapshot(context.Background(), parent, 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer backup.Close()
	plan, err := reader.PrepareSQLitePublicationPlan(context.Background(), config.Schema, backup)
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.Verify(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := plan.PublicationAuthority(context.Background()); !errors.Is(err, ErrPublicationAuthorityUnavailable) {
		t.Fatalf("read-only plan granted source write: %v", err)
	}
	returned, err := plan.Evidence()
	if err != nil {
		t.Fatal(err)
	}
	returned.TableCounts["foreign"] = 42
	returned.Domain.TableCounts["foreign"] = 42
	returned.Domain.AuthorContentHash = "forged"
	if err := plan.Verify(context.Background()); err != nil {
		t.Fatalf("caller changed internal proof: %v", err)
	}
	if held, err := writerlock.AcquireLegacy(config.DataRoot); !errors.Is(err, writerlock.ErrLocked) {
		if err == nil {
			_ = held.Close()
		}
		t.Fatalf("publication preparation released writer exclusion: %v", err)
	}
	current, err := os.ReadFile(source)
	if err != nil || !bytes.Equal(current, original) {
		t.Fatalf("author bytes changed: %v", err)
	}
}

func TestSQLitePublicationPlanFailsOnMissingProofWrongReaderAndTampering(t *testing.T) {
	catalog, key, config, _, _ := readFixture(t)
	reader, err := OpenReadOnly(context.Background(), catalog, key, config)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	parent := t.TempDir()
	if err := os.Chmod(parent, 0700); err != nil {
		t.Fatal(err)
	}
	backup, err := reader.SealDatabaseSnapshot(context.Background(), parent, 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer backup.Close()
	if _, err := reader.PrepareSQLitePublicationPlan(context.Background(), config.Schema, backup); !errors.Is(err, ErrWriteEvidenceIncomplete) {
		t.Fatalf("missing knowledge preflight accepted: %v", err)
	}
	if _, err := (&Reader{}).PrepareSQLitePublicationPlan(context.Background(), config.Schema, backup); !errors.Is(err, ErrPublicationPlanIncomplete) {
		t.Fatalf("another Reader reused backup: %v", err)
	}
	if _, err := reader.PrepareSQLitePublicationPlan(context.Background(), config.Schema, nil); !errors.Is(err, ErrPublicationPlanIncomplete) {
		t.Fatalf("missing rollback bytes accepted: %v", err)
	}
}

func TestSQLitePublicationPlanRechecksSealedBackupAndLiveLock(t *testing.T) {
	catalog, key, config, _, _ := readFixture(t)
	config.Knowledge = emptyKnowledgeConfig()
	reader, err := OpenReadOnly(context.Background(), catalog, key, config)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	parent := t.TempDir()
	if err := os.Chmod(parent, 0700); err != nil {
		t.Fatal(err)
	}
	backup, err := reader.SealDatabaseSnapshot(context.Background(), parent, 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer backup.Close()
	plan, err := reader.PrepareSQLitePublicationPlan(context.Background(), config.Schema, backup)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(backup.Directory(), "snapshot.db")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("tampered private rollback")); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := plan.Verify(context.Background()); !errors.Is(err, ErrChanged) {
		t.Fatalf("tampered sealed backup accepted: %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if err := plan.PublicationAuthority(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("released legacy lock accepted: %v", err)
	}
}

func TestSQLitePublicationPlanRejectsSourceDriftAfterPreparation(t *testing.T) {
	catalog, key, config, source, _ := readFixture(t)
	config.Knowledge = emptyKnowledgeConfig()
	reader, err := OpenReadOnly(context.Background(), catalog, key, config)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	parent := t.TempDir()
	if err := os.Chmod(parent, 0700); err != nil {
		t.Fatal(err)
	}
	backup, err := reader.SealDatabaseSnapshot(context.Background(), parent, 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer backup.Close()
	plan, err := reader.PrepareSQLitePublicationPlan(context.Background(), config.Schema, backup)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(source, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("noncooperating source drift")); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := plan.Verify(context.Background()); !errors.Is(err, ErrChanged) {
		t.Fatalf("source drift accepted after plan preparation: %v", err)
	}
}
