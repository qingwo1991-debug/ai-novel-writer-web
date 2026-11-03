//go:build linux

package authorruntime

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/writerlock"
)

var errInjectedDriverClose = errors.New("injected driver close failure")

type closeErrorConnector struct{}
type closeErrorDriver struct{}
type closeErrorConn struct{}

func (closeErrorConnector) Connect(context.Context) (driver.Conn, error) {
	return closeErrorConn{}, nil
}
func (closeErrorConnector) Driver() driver.Driver          { return closeErrorDriver{} }
func (closeErrorDriver) Open(string) (driver.Conn, error)  { return closeErrorConn{}, nil }
func (closeErrorConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("unused") }
func (closeErrorConn) Begin() (driver.Tx, error)           { return nil, errors.New("unused") }
func (closeErrorConn) Close() error                        { return errInjectedDriverClose }

func TestReaderClosePoisonsUnconfirmedDriverClose(t *testing.T) {
	catalog, key, config, _, _ := readFixture(t)
	reader, err := OpenReadOnly(context.Background(), catalog, key, config)
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.db.Close(); err != nil {
		t.Fatal(err)
	}
	failing := sql.OpenDB(closeErrorConnector{})
	if err := failing.PingContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	reader.db = failing
	if err := reader.Close(); !errors.Is(err, errInjectedDriverClose) || !errors.Is(err, ErrCleanup) {
		t.Fatalf("unconfirmed driver close was not reported: %v", err)
	}
	// database/sql reports success on a second DB.Close despite the earlier
	// driver error. Reader must never interpret that as confirmed cleanup.
	if err := reader.Close(); !errors.Is(err, ErrCleanup) {
		t.Fatalf("second close released unconfirmed driver: %v", err)
	}
	if competing, err := writerlock.AcquireLegacy(config.DataRoot); !errors.Is(err, writerlock.ErrLocked) {
		if competing != nil {
			_ = competing.Close()
		}
		t.Fatalf("writer lock released after driver close error: %v", err)
	}
	// The test owns this deliberate poison and releases the fixture resources.
	if err := reader.root.Close(); err != nil {
		t.Fatal(err)
	}
	if err := reader.project.Close(); err != nil {
		t.Fatal(err)
	}
	if err := reader.lock.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestReaderCloseRetriesPrivateEntryCleanupBeforeReleasingLock(t *testing.T) {
	catalog, key, config, _, _ := readFixture(t)
	reader, err := OpenReadOnly(context.Background(), catalog, key, config)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	var obstructed string
	for _, name := range []string{"snapshot.db-wal", "snapshot.db-shm", "snapshot.db-journal"} {
		path := filepath.Join(reader.directory, name)
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			obstructed = path
			break
		} else if err != nil {
			t.Fatal(err)
		}
	}
	if obstructed == "" {
		t.Fatal("no optional private snapshot entry available for cleanup obstruction")
	}
	if err := os.Mkdir(obstructed, 0700); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(obstructed, "hold")
	if err := os.WriteFile(child, []byte("private fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); !errors.Is(err, ErrCleanup) {
		t.Fatalf("nonempty snapshot entry was not retained: %v", err)
	}
	if err := reader.Verify(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("partly closed reader admitted another read: %v", err)
	}
	if competing, err := writerlock.AcquireLegacy(config.DataRoot); !errors.Is(err, writerlock.ErrLocked) {
		if competing != nil {
			_ = competing.Close()
		}
		t.Fatalf("writer lock released before cleanup completed: %v", err)
	}
	if err := os.Remove(child); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(obstructed); err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("private cleanup did not recover: %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("completed close was not idempotent: %v", err)
	}
	entries, err := os.ReadDir(config.ScratchParent)
	if err != nil || len(entries) != 0 {
		t.Fatalf("private snapshot remained after retry: %v %v", entries, err)
	}
	reacquired, err := writerlock.AcquireLegacy(config.DataRoot)
	if err != nil {
		t.Fatalf("writer lock remained held after cleanup: %v", err)
	}
	if err := reacquired.Close(); err != nil {
		t.Fatal(err)
	}
}
