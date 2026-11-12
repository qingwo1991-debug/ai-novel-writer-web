//go:build linux

package projectfs

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// staticPermit is a test-only admission stub. The real permit lives above
// projectfs and re-proves the retained legacy lock and full schema admission.
type staticPermit struct {
	mu    sync.Mutex
	err   error
	calls int
}

func (p *staticPermit) Verify(ctx context.Context, project *Project) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	if project == nil {
		return ErrInvalidPath
	}
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	return p.err
}

// VerifyAdopt is the adopting half of the real permit. This stub admits the
// same way, so the borrow the test exercises is unchanged.
func (p *staticPermit) VerifyAdopt(ctx context.Context, project *Project) error {
	return p.Verify(ctx, project)
}

func (p *staticPermit) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// authorFixture replaces the junk database bytes with a real SQLite file so the
// bundled driver can actually open it. SQLite validity is not project admission;
// it only makes this handle testable.
func authorFixture(t *testing.T) (*Library, *Project, string) {
	t.Helper()
	library, path, storage := fixture(t)
	database := filepath.Join(storage, DatabaseName)
	if err := os.Remove(database); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", "file:"+database+"?mode=rwc")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE drafts(id INTEGER PRIMARY KEY, body TEXT NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO drafts(body) VALUES('原稿')"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(database, 0o600); err != nil {
		t.Fatal(err)
	}
	l, p := openFixture(t, library, path)
	return l, p, database
}

func TestAuthorDatabasePersistsWritesToOriginalProject(t *testing.T) {
	_, project, database := authorFixture(t)
	permit := &staticPermit{}
	handle, err := project.OpenAuthorDatabase(context.Background(), permit)
	if err != nil {
		t.Fatal(err)
	}
	if permit.count() == 0 {
		t.Fatal("admission permit was never consulted")
	}
	db := handle.DB()
	if db == nil {
		t.Fatal("missing database handle")
	}
	if _, err := db.Exec("INSERT INTO drafts(body) VALUES('新增正文')"); err != nil {
		t.Fatal(err)
	}
	if err := handle.Verify(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	// The write must be durable in the author's own database, not a copy.
	verify, err := sql.Open("sqlite3", "file:"+database+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer verify.Close()
	var count int
	if err := verify.QueryRow("SELECT COUNT(*) FROM drafts").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("author database lost writes: got %d rows", count)
	}
}

func TestAuthorDatabaseOpensForeignKeysAndRefusesMissingFile(t *testing.T) {
	_, project, database := authorFixture(t)
	permit := &staticPermit{}
	handle, err := project.OpenAuthorDatabase(context.Background(), permit)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	var enabled int
	if err := handle.DB().QueryRow("PRAGMA foreign_keys").Scan(&enabled); err != nil {
		t.Fatal(err)
	}
	if enabled != 1 {
		t.Fatal("author write connection must enable foreign keys")
	}
	// A missing database must never be created by an author handle.
	if err := os.Remove(database); err != nil {
		t.Fatal(err)
	}
	if _, err := project.OpenAuthorDatabase(context.Background(), &staticPermit{}); !errors.Is(err, ErrUnsafeFile) {
		t.Fatalf("missing database admitted: %v", err)
	}
	if _, err := os.Lstat(database); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("author handle recreated a missing database")
	}
}

func TestAuthorDatabaseRejectsUnsafeSidecars(t *testing.T) {
	for _, name := range []string{DatabaseName + "-wal", DatabaseName + "-shm", DatabaseName + "-journal"} {
		t.Run(name, func(t *testing.T) {
			_, project, _ := authorFixture(t)
			storage := filepath.Join(project.Path(), StorageDirectory)
			outside := filepath.Join(t.TempDir(), "outside")
			if err := os.WriteFile(outside, []byte("synthetic"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(storage, name)); err != nil {
				t.Fatal(err)
			}
			if _, err := project.OpenAuthorDatabase(context.Background(), &staticPermit{}); !errors.Is(err, ErrUnsafeFile) {
				t.Fatalf("unsafe sidecar admitted: %v", err)
			}
		})
	}
}

func TestAuthorDatabaseFailsClosedOnPermitDenial(t *testing.T) {
	_, project, _ := authorFixture(t)
	denied := errors.New("AUTHOR_ADMISSION_DENIED")
	handle, err := project.OpenAuthorDatabase(context.Background(), &staticPermit{err: denied})
	if handle != nil || !errors.Is(err, denied) {
		t.Fatalf("denied permit produced a handle: %v", err)
	}
	if _, err := project.OpenAuthorDatabase(context.Background(), nil); !errors.Is(err, ErrInvalidPath) {
		t.Fatal("nil permit admitted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := project.OpenAuthorDatabase(ctx, &staticPermit{}); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled context admitted")
	}
}

func TestAuthorDatabaseVerifyDetectsDatabaseReplacement(t *testing.T) {
	_, project, database := authorFixture(t)
	handle, err := project.OpenAuthorDatabase(context.Background(), &staticPermit{})
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	if err := handle.Verify(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Replace the entry with a different inode; the pinned identity must fail.
	if err := os.Remove(database); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(database, []byte("replacement database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := handle.Verify(context.Background()); !errors.Is(err, ErrChanged) {
		t.Fatalf("replaced database verified as current: %v", err)
	}
	// A closed handle refuses further verification.
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	if err := handle.Verify(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed handle verified: %v", err)
	}
}

func TestAuthorDatabaseCloseNeverRemovesAuthorFiles(t *testing.T) {
	_, project, database := authorFixture(t)
	handle, err := project.OpenAuthorDatabase(context.Background(), &staticPermit{})
	if err != nil {
		t.Fatal(err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	if err := handle.Close(); err != nil {
		t.Fatalf("close is not idempotent: %v", err)
	}
	info, err := os.Lstat(database)
	if err != nil {
		t.Fatalf("author database removed on close: %v", err)
	}
	if info.Size() == 0 {
		t.Fatal("author database truncated on close")
	}
	if handle.DB() != nil {
		t.Fatal("closed handle still exposes a database")
	}
}
