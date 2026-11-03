//go:build linux

package projectfs

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sync"

	_ "github.com/mattn/go-sqlite3" // Registers the database/sql driver "sqlite3".
)

// AuthorPermit is admission evidence produced by a higher layer after it proved
// the project is a supported current-schema author project under the retained
// legacy writer lock. It is deliberately an interface so that projectfs does
// not depend on the admission implementation and cannot mint its own permit.
//
// Verify must re-check the full admission state (retained lock, project and
// storage identity, supported schema, portable-authority absence) against the
// exact Project passed in, and must fail closed for anything it cannot prove.
// It must be called WITHOUT the project or library mutex held: portable-authority
// verification locks the project itself, so a concurrent call would self-deadlock.
type AuthorPermit interface {
	Verify(context.Context, *Project) error
}

// AuthorDatabase is a write-capable handle to an existing author project.db.
//
// Concurrency model: this runtime assumes cooperating writers only. The legacy
// gateway lock held for the lifetime of the handle excludes other native/legacy
// web backends, and the repository's full-body CAS rejects stale-baseline
// saves, so two authors or agents cannot silently overwrite each other. A
// non-cooperating same-UID process that replaces the database between our
// identity check and the driver's open is outside this guarantee; it is
// detected by the post-open identity verification whenever the inode differs,
// but it is not defended against in general.
//
// SQLite also opens WAL/shm/journal sidecars through its default VFS, which is
// why the bundled driver cannot offer a genuinely confined descriptor (see
// ErrSQLiteConnectorUnconfined). Instead this handle pins the storage directory
// and the database identity before and after the open, refuses unsafe existing
// sidecars, and re-proves identity on every Verify. It never sets a journal
// mode, never creates a missing database, and never removes author files.
type AuthorDatabase struct {
	mu       sync.Mutex
	project  *Project
	permit   AuthorPermit
	db       *sql.DB
	path     string
	identity fs.FileInfo
	closed   bool
}

// OpenAuthorDatabase opens the existing project.db read-write for an admitted
// project. The caller must retain the Project, the authenticating permit state
// and the underlying legacy writer lock until Close returns.
func (p *Project) OpenAuthorDatabase(ctx context.Context, permit AuthorPermit) (*AuthorDatabase, error) {
	if ctx == nil || p == nil || p.library == nil || permit == nil {
		return nil, ErrInvalidPath
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Admission first, before taking the project mutex: the permit Verify
	// (portable authority + content identity) locks the project itself.
	if err := permit.Verify(ctx, p); err != nil {
		return nil, err
	}
	handle, err := p.openAuthorDatabaseLocked(ctx)
	if err != nil {
		return nil, err
	}
	handle.permit = permit
	// Re-prove admission after the open. openAuthorDatabaseLocked has released
	// the project/library mutexes, so the permit may lock them again.
	if err := permit.Verify(ctx, p); err != nil {
		_ = handle.Close()
		return nil, err
	}
	return handle, nil
}

// openAuthorDatabaseLocked performs the identity-checked open while holding the
// project and library mutexes, then releases them before returning so the caller
// can re-prove admission without self-deadlocking. On error it closes any db it
// opened.
func (p *Project) openAuthorDatabaseLocked(ctx context.Context) (*AuthorDatabase, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.library.mu.Lock()
	defer p.library.mu.Unlock()
	if err := p.checkIdentityLocked(); err != nil {
		return nil, err
	}
	if err := p.checkWritableDirectoriesLocked(); err != nil {
		return nil, err
	}
	if err := p.checkSidecarsLocked(); err != nil {
		return nil, err
	}
	before, err := p.storage.Lstat(DatabaseName)
	if err != nil || !ownedWriteFile(before) {
		return nil, ErrUnsafeFile
	}
	path := filepath.Join(p.path, StorageDirectory, DatabaseName)
	dsn, err := authorDatabaseDSN(path)
	if err != nil {
		return nil, ErrInvalidPath
	}
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, ErrUnsafeFile
	}
	// One connection only: every statement must share the connection that
	// carried _foreign_keys=on, and the repository serializes writers anyway.
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, ErrUnsafeFile
	}
	// Post-open verification. SQLite may legitimately have rolled back a hot
	// journal, so size/time are allowed to change; inode, ownership and safety
	// of the entry must not.
	opened, err := p.storage.Lstat(DatabaseName)
	if err != nil || !ownedWriteFile(opened) || !os.SameFile(before, opened) {
		db.Close()
		return nil, ErrChanged
	}
	if err := p.checkIdentityLocked(); err != nil {
		db.Close()
		return nil, err
	}
	if err := p.checkWritableDirectoriesLocked(); err != nil {
		db.Close()
		return nil, err
	}
	if err := p.checkSidecarsLocked(); err != nil {
		db.Close()
		return nil, err
	}
	return &AuthorDatabase{project: p, db: db, path: path, identity: opened}, nil
}

// DB returns the single-connection handle. Callers must not close it and must
// re-Verify around any write transaction they consider consequential.
func (a *AuthorDatabase) DB() *sql.DB {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.db
}

// Verify re-proves the retained lock and admission state, the project/storage
// identity, the database inode we opened, and the safety of any sidecars. The
// final permit re-check runs without the project mutex held to avoid a
// self-deadlock, mirroring OpenAuthorDatabase.
func (a *AuthorDatabase) Verify(ctx context.Context) error {
	if a == nil || ctx == nil {
		return ErrInvalidPath
	}
	a.mu.Lock()
	if a.closed || a.db == nil {
		a.mu.Unlock()
		return ErrClosed
	}
	if err := ctx.Err(); err != nil {
		a.mu.Unlock()
		return err
	}
	if err := a.permit.Verify(ctx, a.project); err != nil {
		a.mu.Unlock()
		return err
	}
	p := a.project
	p.mu.Lock()
	defer p.mu.Unlock()
	p.library.mu.Lock()
	defer p.library.mu.Unlock()
	if err := p.checkIdentityLocked(); err != nil {
		a.mu.Unlock()
		return err
	}
	if err := p.checkWritableDirectoriesLocked(); err != nil {
		a.mu.Unlock()
		return err
	}
	if err := p.checkSidecarsLocked(); err != nil {
		a.mu.Unlock()
		return err
	}
	opened, err := p.storage.Lstat(DatabaseName)
	if err != nil || !ownedWriteFile(opened) || !os.SameFile(a.identity, opened) {
		a.mu.Unlock()
		return ErrChanged
	}
	a.mu.Unlock()
	return a.permit.Verify(ctx, p)
}

// Close closes only this handle. It never rolls back, truncates, checkpoints,
// removes or otherwise rewrites an author file, and it does not release the
// caller's legacy lock or the Project.
func (a *AuthorDatabase) Close() error {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return nil
	}
	a.closed = true
	db := a.db
	a.db = nil
	if db == nil {
		return nil
	}
	if err := db.Close(); err != nil {
		return errors.Join(ErrChanged, err)
	}
	return nil
}

// checkSidecarsLocked rejects unsafe pre-existing WAL/shm/journal entries. It
// cannot prevent SQLite from creating new sidecars during a write; it only
// refuses to build on top of already dangerous ones such as symlinks.
func (p *Project) checkSidecarsLocked() error {
	for _, name := range []string{DatabaseName + "-wal", DatabaseName + "-shm", DatabaseName + "-journal"} {
		info, err := p.storage.Lstat(name)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil || !ownedWriteFile(info) {
			return ErrUnsafeFile
		}
	}
	return nil
}

// authorDatabaseDSN mirrors store.ExistingDSN without importing the store
// package: mode=rw refuses a missing database, busy timeout bounds lock waits,
// and foreign keys are enabled on the connection. No journal mode is set, so
// opening an author project never changes its persistent journal policy.
func authorDatabaseDSN(path string) (string, error) {
	if path == "" || path == ":memory:" {
		return "", ErrInvalidPath
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(absolute)}
	query := url.Values{}
	query.Set("mode", "rw")
	query.Set("_busy_timeout", "5000")
	query.Set("_foreign_keys", "on")
	uri.RawQuery = query.Encode()
	return uri.String(), nil
}