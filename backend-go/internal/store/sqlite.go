package store

import (
	"fmt"
	"net/url"
	"path/filepath"

	_ "github.com/mattn/go-sqlite3" // Registers database/sql driver "sqlite3".
)

// ExistingDSN builds an escaped SQLite file URI for an existing database.
// mode=rw refuses missing files. No journal_mode/WAL setting is made: opening
// an author's project must not silently change its persistent journal policy.
// The caller opens with sql.Open("sqlite3", dsn), pings, validates the project
// schema/version/fingerprint, and only then constructs NewDrafts(db).
func ExistingDSN(path string) (string, error) {
	if path == "" || path == ":memory:" {
		return "", fmt.Errorf("existing SQLite database path required")
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
