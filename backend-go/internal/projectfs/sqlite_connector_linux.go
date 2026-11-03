//go:build linux

package projectfs

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
)

// ErrSQLiteConnectorUnconfined means the bundled sqlite3 driver's ordinary
// path connector cannot be used to write an author project. It opens the main
// database with sqlite3_open_v2 and subsequently opens WAL, shm, journal and
// temporary files through SQLite's default VFS. A pinned project.db fd alone
// cannot confine those names or prove the inode used by the driver.
var ErrSQLiteConnectorUnconfined = errors.New("SOURCE_SQLITE_VFS_UNCONFINED")

// SQLiteConnectorAssessment is a point-in-time inventory for implementing a
// future confined VFS. It is not a reusable write grant: absent sidecars can
// appear immediately after this check, including during SQLite open.
type SQLiteConnectorAssessment struct {
	WALPresent     bool
	SHMPresent     bool
	JournalPresent bool
	NeedsCustomVFS bool
}

// AssessSQLiteConnector verifies the pinned candidate and rejects unsafe
// existing sidecars. It cannot authorize SQLite path opens or publication.
func (c *SQLiteWriteCandidate) AssessSQLiteConnector(ctx context.Context) (SQLiteConnectorAssessment, error) {
	var result SQLiteConnectorAssessment
	if err := c.Verify(ctx); err != nil {
		return result, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.file == nil {
		return result, ErrClosed
	}
	p := c.project
	p.mu.Lock()
	defer p.mu.Unlock()
	p.library.mu.Lock()
	defer p.library.mu.Unlock()
	if err := p.checkIdentityLocked(); err != nil {
		return result, err
	}
	if err := p.checkWritableDirectoriesLocked(); err != nil {
		return result, err
	}
	if err := c.lock.VerifyLegacy(c.dataRoot); err != nil {
		return result, err
	}
	for _, sidecar := range []struct {
		name    string
		present *bool
	}{
		{DatabaseName + "-wal", &result.WALPresent},
		{DatabaseName + "-shm", &result.SHMPresent},
		{DatabaseName + "-journal", &result.JournalPresent},
	} {
		if err := ctx.Err(); err != nil {
			return SQLiteConnectorAssessment{}, err
		}
		info, err := p.storage.Lstat(sidecar.name)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil || !ownedWriteFile(info) {
			return SQLiteConnectorAssessment{}, ErrUnsafeFile
		}
		file, opened, err := openRegular(p.storage, sidecar.name)
		if err != nil {
			return SQLiteConnectorAssessment{}, ErrChanged
		}
		entry, entryErr := p.storage.Lstat(sidecar.name)
		closeErr := file.Close()
		if entryErr != nil || closeErr != nil || !ownedWriteFile(opened) || !ownedWriteFile(entry) || !sameVersion(info, opened) || !sameVersion(opened, entry) {
			return SQLiteConnectorAssessment{}, ErrChanged
		}
		*sidecar.present = true
	}
	if err := c.lock.VerifyLegacy(c.dataRoot); err != nil {
		return SQLiteConnectorAssessment{}, err
	}
	result.NeedsCustomVFS = true
	return result, nil
}

// OpenConfinedSQLite denies connection until a custom VFS can bind every
// SQLite file open to the pinned storage root, enforce no-follow/ownership on
// existing entries, safely create sidecars and temporary files, and verify
// the main database inode across open, transaction and close. The stock
// mattn/go-sqlite3 path connector does not accept an already-open main fd.
func (c *SQLiteWriteCandidate) OpenConfinedSQLite(ctx context.Context) (*sql.DB, error) {
	if _, err := c.AssessSQLiteConnector(ctx); err != nil {
		return nil, err
	}
	return nil, ErrSQLiteConnectorUnconfined
}
