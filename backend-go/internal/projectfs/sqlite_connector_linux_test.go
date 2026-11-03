//go:build linux

package projectfs

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/writerlock"
)

func TestSQLiteConnectorDeniesStockPathOpenWithoutChangingSource(t *testing.T) {
	dataRoot, storage, project := writeCandidateFixture(t)
	lock, err := writerlock.AcquireLegacy(dataRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	put(t, filepath.Join(storage, DatabaseName+"-wal"), []byte("synthetic wal bytes"))
	put(t, filepath.Join(storage, DatabaseName+"-shm"), []byte("synthetic shm bytes"))
	candidate, err := project.OpenSQLiteWriteCandidate(context.Background(), lock, dataRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer candidate.Close()
	before, err := os.ReadFile(filepath.Join(storage, DatabaseName))
	if err != nil {
		t.Fatal(err)
	}
	assessment, err := candidate.AssessSQLiteConnector(context.Background())
	if err != nil || !assessment.WALPresent || !assessment.SHMPresent || assessment.JournalPresent || !assessment.NeedsCustomVFS {
		t.Fatalf("incorrect sidecar assessment: %+v %v", assessment, err)
	}
	db, err := candidate.OpenConfinedSQLite(context.Background())
	if db != nil || !errors.Is(err, ErrSQLiteConnectorUnconfined) {
		t.Fatalf("stock SQLite path connector admitted: %v %v", db, err)
	}
	after, err := os.ReadFile(filepath.Join(storage, DatabaseName))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("connector probe changed source: %v", err)
	}
}

func TestSQLiteConnectorRejectsUnsafeSidecarAndClosedCandidate(t *testing.T) {
	dataRoot, storage, project := writeCandidateFixture(t)
	lock, err := writerlock.AcquireLegacy(dataRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	candidate, err := project.OpenSQLiteWriteCandidate(context.Background(), lock, dataRoot)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(storage, DatabaseName+"-journal")
	if err := os.Symlink(DatabaseName, path); err != nil {
		t.Fatal(err)
	}
	if _, err := candidate.AssessSQLiteConnector(context.Background()); !errors.Is(err, ErrUnsafeFile) {
		t.Fatalf("linked journal accepted: %v", err)
	}
	if db, err := candidate.OpenConfinedSQLite(context.Background()); db != nil || !errors.Is(err, ErrUnsafeFile) {
		t.Fatalf("linked journal opened: %v %v", db, err)
	}
	if err := candidate.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := candidate.AssessSQLiteConnector(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed candidate assessed: %v", err)
	}
}
