//go:build linux

// Package authorruntime composes a read-only, source-verified author snapshot.
// It does not grant a durable writer or publish a project operation.
package authorruntime

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"sync"

	_ "github.com/mattn/go-sqlite3"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/admission"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectaccess"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectcatalog"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/writerlock"
)

var ErrInvalid = errors.New("AUTHOR_READ_INVALID")
var ErrClosed = errors.New("AUTHOR_READ_CLOSED")
var ErrChanged = errors.New("AUTHOR_READ_SOURCE_CHANGED")
var ErrCleanup = errors.New("AUTHOR_READ_CLEANUP_FAILED")

// Reader owns a private admitted SQLite copy, its source project capability,
// and the exact legacy gateway lock. Every Read rechecks source authority and
// holds exclusion until the callback and transaction have completed.
type Reader struct {
	mu            sync.Mutex
	lock          *writerlock.Lock
	ownsLock      bool
	catalog       *projectcatalog.Store
	key           string
	project       *projectfs.Project
	proof         *projectfs.SnapshotProof
	assets        *projectfs.AssetInventory
	db            *sql.DB
	root          *os.Root
	directory     string
	directoryInfo os.FileInfo
	dataRoot      string
	report        projectaccess.Report
	closed        bool
	closeDone     bool
	closePoisoned bool
}

// OpenReadOnly accepts a process-local catalog key and trusted administrator
// configuration. The project path in config must match the resolved capability.
// It first proves the complete copied preflight under one continuous lock,
// then admits a new private SQLite copy which stays alive until Close.
// The original author database is never opened through SQLite.
func OpenReadOnly(ctx context.Context, catalog *projectcatalog.Store, key string, config projectaccess.Config) (reader *Reader, err error) {
	if ctx == nil || catalog == nil || key == "" || config.DataRoot == "" {
		return nil, ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	lock, err := writerlock.AcquireLegacy(config.DataRoot)
	if err != nil {
		return nil, err
	}
	return openReadOnlyWithLock(ctx, catalog, key, config, lock, true)
}

// OpenReadOnlyUnderLock borrows the exact live gateway exclusion from a
// coordinator. The caller keeps ownership of held through every Reader call
// and must close all readers before releasing the lock. Reader.Close does not
// release a borrowed lock, including on failed admission.
func OpenReadOnlyUnderLock(ctx context.Context, held *writerlock.Lock, catalog *projectcatalog.Store, key string, config projectaccess.Config) (*Reader, error) {
	if held == nil {
		return nil, ErrInvalid
	}
	return openReadOnlyWithLock(ctx, catalog, key, config, held, false)
}

// openReadOnlyPinnedProjectUnderLock admits a caller-owned, already pinned
// project without issuing a catalog key. It is for recovery readback while the
// catalog is still fenced by an unresolved exchange journal. Ownership of
// project transfers on call, including when admission fails. This reader must
// be closed before recovery clearance or the held legacy lock is released.
func openReadOnlyPinnedProjectUnderLock(ctx context.Context, held *writerlock.Lock, project *projectfs.Project, config projectaccess.Config) (*Reader, error) {
	if held == nil || project == nil {
		return nil, ErrInvalid
	}
	return openReadOnlyPinnedProject(ctx, nil, "", config, held, false, project)
}

func openReadOnlyWithLock(ctx context.Context, catalog *projectcatalog.Store, key string, config projectaccess.Config, lock *writerlock.Lock, ownsLock bool) (reader *Reader, err error) {
	if ctx == nil || catalog == nil || key == "" || config.DataRoot == "" {
		return nil, ErrInvalid
	}
	defer func() {
		if err != nil && ownsLock {
			_ = lock.Close()
		}
	}()
	if err := lock.VerifyLegacy(config.DataRoot); err != nil {
		return nil, err
	}
	project, err := catalog.Resolve(key)
	if err != nil {
		return nil, err
	}
	return openReadOnlyPinnedProject(ctx, catalog, key, config, lock, ownsLock, project)
}

func openReadOnlyPinnedProject(ctx context.Context, catalog *projectcatalog.Store, key string, config projectaccess.Config, lock *writerlock.Lock, ownsLock bool, project *projectfs.Project) (reader *Reader, err error) {
	if project == nil {
		return nil, ErrInvalid
	}
	defer func() {
		if err != nil {
			_ = project.Close()
		}
	}()
	if ctx == nil || lock == nil || project.Path() != config.ProjectPath || filepath.Dir(project.Path()) != config.LibraryRoot {
		return nil, ErrInvalid
	}
	if err := lock.VerifyLegacy(config.DataRoot); err != nil {
		return nil, err
	}
	report, err := projectaccess.PreflightUnderLock(ctx, config, lock)
	if err != nil {
		return nil, err
	}
	if report.ProjectID != project.ProjectID() || !report.LegacyGatewayExcluded {
		return nil, ErrChanged
	}
	if err := lock.VerifyLegacy(config.DataRoot); err != nil {
		return nil, err
	}
	if err := project.VerifyNoPortableAuthority(ctx); err != nil {
		return nil, ErrChanged
	}
	assets, err := project.InventoryCanonicalAssets(ctx)
	if err != nil {
		return nil, ErrChanged
	}
	directory, err := os.MkdirTemp(config.ScratchParent, "author-read-")
	if err != nil {
		return nil, ErrInvalid
	}
	var root *os.Root
	var db *sql.DB
	defer func() {
		if err == nil {
			return
		}
		if db != nil {
			_ = db.Close()
		}
		if root != nil {
			for _, name := range []string{"snapshot.db", "snapshot.db-wal", "snapshot.db-shm", "snapshot.db-journal"} {
				_ = root.Remove(name)
			}
			_ = root.Close()
		}
		_ = os.Remove(directory)
	}()
	if err := os.Chmod(directory, 0700); err != nil {
		return nil, ErrInvalid
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() {
		return nil, ErrInvalid
	}
	root, err = os.OpenRoot(directory)
	if err != nil {
		return nil, ErrInvalid
	}
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		return nil, ErrChanged
	}
	proof, err := project.StageDatabase(ctx, root, config.MaxDatabaseBytes)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(directory, "snapshot.db")
	evidence, err := admission.VerifySnapshot(ctx, path, config.Schema)
	if err != nil {
		return nil, err
	}
	if evidence.UserVersion != report.SQLite.UserVersion || evidence.SchemaFingerprint != report.SQLite.SchemaFingerprint || evidence.ContentIdentity != report.SQLite.ContentIdentity {
		return nil, ErrChanged
	}
	if err := proof.VerifyUnchanged(ctx); err != nil {
		return nil, ErrChanged
	}
	if err := assets.VerifyUnchanged(ctx); err != nil {
		return nil, ErrChanged
	}
	if err := lock.VerifyLegacy(config.DataRoot); err != nil {
		return nil, err
	}
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	query := url.Values{}
	query.Set("mode", "ro")
	query.Set("_query_only", "on")
	query.Set("_foreign_keys", "on")
	uri.RawQuery = query.Encode()
	db, err = sql.Open("sqlite3", uri.String())
	if err != nil {
		return nil, ErrInvalid
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.PingContext(ctx); err != nil {
		return nil, ErrInvalid
	}
	reader = &Reader{lock: lock, ownsLock: ownsLock, catalog: catalog, key: key, project: project, proof: proof, assets: assets, db: db, root: root, directory: directory, directoryInfo: info, dataRoot: config.DataRoot, report: report}
	return reader, nil
}

func (reader *Reader) verifyLocked(ctx context.Context) error {
	if reader.closed {
		return ErrClosed
	}
	if ctx == nil {
		return ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := reader.lock.VerifyLegacy(reader.dataRoot); err != nil {
		return err
	}
	if reader.catalog != nil {
		current, err := reader.catalog.Resolve(reader.key)
		if err != nil {
			return ErrChanged
		}
		identity := current.Identity().Same(reader.project.Identity()) && current.ProjectID() == reader.report.ProjectID
		closeErr := current.Close()
		if !identity || closeErr != nil {
			return ErrChanged
		}
	}
	if err := reader.project.VerifyNoPortableAuthority(ctx); err != nil {
		return ErrChanged
	}
	if err := reader.proof.VerifyUnchanged(ctx); err != nil {
		return ErrChanged
	}
	if err := reader.assets.VerifyUnchanged(ctx); err != nil {
		return ErrChanged
	}
	return reader.lock.VerifyLegacy(reader.dataRoot)
}

func (reader *Reader) Verify(ctx context.Context) error {
	if reader == nil {
		return ErrInvalid
	}
	reader.mu.Lock()
	defer reader.mu.Unlock()
	return reader.verifyLocked(ctx)
}

func (reader *Reader) Report() (projectaccess.Report, error) {
	if reader == nil {
		return projectaccess.Report{}, ErrInvalid
	}
	reader.mu.Lock()
	defer reader.mu.Unlock()
	if reader.closed {
		return projectaccess.Report{}, ErrClosed
	}
	return reader.report, nil
}

// Read opens a read-only transaction on the private copied database. The
// source is checked before and after the callback, including on callback
// failure. Returning a *sql.Tx outside this callback is unsupported.
func (reader *Reader) Read(ctx context.Context, operation func(*sql.Tx) error) error {
	if reader == nil || operation == nil {
		return ErrInvalid
	}
	reader.mu.Lock()
	defer reader.mu.Unlock()
	if err := reader.verifyLocked(ctx); err != nil {
		return err
	}
	tx, err := reader.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return ErrChanged
	}
	operationErr := operation(tx)
	rollbackErr := tx.Rollback()
	verifyErr := reader.verifyLocked(ctx)
	return errors.Join(operationErr, rollbackErr, verifyErr)
}

// Close releases the private DB before the legacy lock. It removes only the
// fixed snapshot entries through the pinned private directory and never
// recursively deletes a substituted host path.
func (reader *Reader) Close() error {
	if reader == nil {
		return ErrInvalid
	}
	reader.mu.Lock()
	defer reader.mu.Unlock()
	if reader.closeDone {
		return nil
	}
	if reader.closePoisoned {
		return ErrCleanup
	}
	// Reject further reads as soon as cleanup starts, including when a later
	// step needs a retry. The writer lock stays held until every owned resource
	// has been confirmed released.
	reader.closed = true
	// database/sql marks DB closed before closing pooled driver connections.
	// A second Close may therefore return nil after the first returned an
	// unconfirmed driver error. Retain the legacy lock permanently in that case.
	if reader.db != nil {
		if err := reader.db.Close(); err != nil {
			reader.closePoisoned = true
			return errors.Join(ErrCleanup, err)
		}
		reader.db = nil
	}
	if reader.root != nil {
		var cleanup error
		for _, name := range []string{"snapshot.db", "snapshot.db-wal", "snapshot.db-shm", "snapshot.db-journal"} {
			if err := reader.root.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
				cleanup = errors.Join(cleanup, err)
			}
		}
		if cleanup != nil {
			return errors.Join(ErrCleanup, cleanup)
		}
		if err := reader.root.Close(); err != nil {
			// os.Root.Close may have consumed its descriptor even when it reports
			// an error. Reusing it or releasing the writer lock is unprovable.
			reader.closePoisoned = true
			return errors.Join(ErrCleanup, err)
		}
		reader.root = nil
	}
	if reader.directory != "" {
		info, err := os.Lstat(reader.directory)
		if err != nil || !os.SameFile(info, reader.directoryInfo) {
			return errors.Join(ErrCleanup, err)
		}
		if err := os.Remove(reader.directory); err != nil {
			return errors.Join(ErrCleanup, err)
		}
		reader.directory = ""
	}
	if reader.project != nil {
		if err := reader.project.Close(); err != nil {
			// Project.Close is itself one-shot. Preserve the lock after an
			// unconfirmed close instead of treating a later no-op as success.
			reader.closePoisoned = true
			return errors.Join(ErrCleanup, err)
		}
		reader.project = nil
	}
	if reader.ownsLock {
		if err := reader.lock.Close(); err != nil {
			reader.closePoisoned = true
			return errors.Join(ErrCleanup, err)
		}
	}
	reader.closeDone = true
	return nil
}
