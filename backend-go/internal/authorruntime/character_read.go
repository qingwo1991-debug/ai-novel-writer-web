//go:build linux

package authorruntime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	sqlite3 "github.com/mattn/go-sqlite3"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
)

const privateCharacterDriverName = "sqlite3-private-character-read-v1"

var privateCharacterDriverOnce sync.Once

func privateCharacterDriver() string {
	privateCharacterDriverOnce.Do(func() {
		// The original repository uses BEGIN IMMEDIATE even for reads. A
		// read-only SQLite connection rejects that transaction. Confine the
		// connection to the disposable private copy and allow only read SQL
		// plus the transaction and schema-inspection operations it needs.
		sql.Register(privateCharacterDriverName, &sqlite3.SQLiteDriver{ConnectHook: func(conn *sqlite3.SQLiteConn) error {
			conn.RegisterAuthorizer(func(action int, first, second, _ string) int {
				switch action {
				case sqlite3.SQLITE_READ, sqlite3.SQLITE_SELECT, sqlite3.SQLITE_FUNCTION:
					return sqlite3.SQLITE_OK
				case sqlite3.SQLITE_TRANSACTION:
					if strings.EqualFold(first, "BEGIN") || strings.EqualFold(first, "COMMIT") || strings.EqualFold(first, "ROLLBACK") {
						return sqlite3.SQLITE_OK
					}
				case sqlite3.SQLITE_PRAGMA:
					if strings.EqualFold(first, "table_info") && second != "" {
						return sqlite3.SQLITE_OK
					}
				}
				return sqlite3.SQLITE_DENY
			})
			return nil
		}})
	})
	return privateCharacterDriverName
}

// ReadCharactersJSON executes one of the three public character reads on the
// admitted private SQLite copy. The exact session, tab and project lease remain
// held through JSON serialization and source revalidation. It never returns a
// repository or a database handle to a transport caller.
func (l *ReadLifecycle) ReadCharactersJSON(ctx context.Context, owner projectsession.Owner, expected projectsession.Context, operation string) (json.RawMessage, error) {
	if l == nil || l.active == nil || !characterReadOperation(operation) {
		return nil, ErrInvalid
	}
	handle, done, err := l.active.Begin(ctx, owner, expected)
	if err != nil {
		return nil, err
	}
	defer done()
	admitted, ok := handle.(*admittedRead)
	if !ok || admitted.reader == nil {
		return nil, ErrInvalid
	}
	return admitted.reader.readCharactersJSON(ctx, expected.ProjectID, operation)
}

func characterReadOperation(operation string) bool {
	switch operation {
	case "db:character-get-all", "db:character-roster-read", "character-identity:read":
		return true
	default:
		return false
	}
}

func (reader *Reader) readCharactersJSON(ctx context.Context, projectID, operation string) (json.RawMessage, error) {
	if reader == nil || !characterReadOperation(operation) {
		return nil, ErrInvalid
	}
	reader.mu.Lock()
	defer reader.mu.Unlock()
	if err := reader.verifyLocked(ctx); err != nil {
		return nil, err
	}
	// The original character repository uses BEGIN IMMEDIATE for its coherent
	// multi-query snapshot. That cannot run on Reader's mode=ro connection.
	// Open only its already staged private copy with a restrictive SQLite
	// authorizer. No author source pathname or repository leaves this method.
	path := filepath.Join(reader.directory, "snapshot.db")
	if err := reader.verifyPrivateCharacterFile(path); err != nil {
		return nil, err
	}
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	query := url.Values{}
	query.Set("mode", "rw")
	query.Set("_foreign_keys", "on")
	uri.RawQuery = query.Encode()
	db, err := sql.Open(privateCharacterDriver(), uri.String())
	if err != nil {
		return nil, ErrChanged
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, ErrChanged
	}
	repository := store.NewCharacters(db)
	var result any
	switch operation {
	case "db:character-get-all":
		result, err = repository.All(ctx)
	case "db:character-roster-read":
		result, err = repository.ReadForProject(ctx, projectID)
	case "character-identity:read":
		result, err = repository.IdentityView(ctx)
	}
	var wire json.RawMessage
	if err == nil {
		wire, err = json.Marshal(result)
	}
	closeErr := db.Close()
	fileErr := reader.verifyPrivateCharacterFile(path)
	verifyErr := reader.verifyLocked(ctx)
	if err := errors.Join(err, closeErr, fileErr, verifyErr); err != nil {
		return nil, err
	}
	return wire, nil
}

func (reader *Reader) verifyPrivateCharacterFile(path string) error {
	if reader.root == nil || reader.directory == "" {
		return ErrChanged
	}
	directory, err := reader.root.Stat(".")
	if err != nil || !os.SameFile(directory, reader.directoryInfo) {
		return ErrChanged
	}
	pinned, err := reader.root.OpenFile("snapshot.db", os.O_RDONLY, 0)
	if err != nil {
		return ErrChanged
	}
	info, statErr := pinned.Stat()
	closeErr := pinned.Close()
	visible, pathErr := os.Lstat(path)
	if statErr != nil || closeErr != nil || pathErr != nil || !info.Mode().IsRegular() || visible.Mode()&os.ModeSymlink != 0 || !os.SameFile(info, visible) {
		return ErrChanged
	}
	return nil
}
