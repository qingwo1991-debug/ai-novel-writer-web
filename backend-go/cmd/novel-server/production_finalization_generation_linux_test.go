//go:build linux

package main

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3" // Registers the database/sql driver "sqlite3".
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// The stale lease below is a lease this session never held. It is not a secret
// and it is not derived from the live one: the point is that it is refused.
const productionFinalizationGenerationStaleLease = "finalization-generation-stale-lease"

func productionFinalizationGenerationRefused(t *testing.T, client *productionDurableClient, operation string, args []any) {
	t.Helper()
	response := client.call(t, operation, args)
	if response.Code != http.StatusNotImplemented {
		t.Fatalf("%s answered %d %s, want 501 GO_OPERATION_NOT_MIGRATED", operation, response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "GO_OPERATION_NOT_MIGRATED") {
		t.Fatalf("%s answered %d %s without GO_OPERATION_NOT_MIGRATED", operation, response.Code, response.Body.String())
	}
}

// productionFinalizationGenerationSeamProof re-reads every line the seam table
// names, so the documentation cannot rot: a moved declaration or a renamed
// refusal fails here instead of leaving a note that points at nothing.
func productionFinalizationGenerationSeamProof(t *testing.T, operation string) {
	t.Helper()
	seam, found := productionFinalizationGenerationSeam(operation)
	if !found {
		t.Fatalf("%s has no documented seam", operation)
	}
	if len(seam.Missing) == 0 {
		t.Fatalf("%s documents no missing piece", operation)
	}
	for _, line := range seam.Missing {
		raw, err := os.ReadFile(line.Path)
		if err != nil {
			t.Fatalf("%s: %s: %v", operation, line.Path, err)
		}
		// The marker is searched for across the whole file rather than only at the
		// recorded line: a seam that is still documented but has been shifted by an
		// unrelated edit is not a regression, while a seam that was deleted is. The
		// recorded line is still reported so a reader can find it quickly.
		if !productionSeamContains(string(raw), line.Marker) {
			t.Fatalf("%s: no longer documents %q anywhere in %s (it was recorded at line %d)",
				operation, line.Marker, line.Path, line.Line)
		}
	}
}

// productionFinalizationGenerationRun reads the one run that claims a slot back
// out of the author database over its own read-only connection, so a handle the
// test drives is a row the file really holds. It also fails when a second run
// claims the same slot, which is how a refused begin proves it claimed nothing.
func productionFinalizationGenerationRun(t *testing.T, database, slotKey string) (rootActionID, runID string) {
	t.Helper()
	db := productionFinalizationGenerationReadOnly(t, database)
	defer func() { _ = db.Close() }()
	rows, err := db.Query("SELECT run_id,root_action_id,binding_json FROM generation_runs ORDER BY rowid")
	if err != nil {
		t.Fatalf("read the seeded runs back: %v", err)
	}
	defer func() { _ = rows.Close() }()
	claims := 0
	for rows.Next() {
		var candidateRun, candidateRoot, binding string
		if err := rows.Scan(&candidateRun, &candidateRoot, &binding); err != nil {
			t.Fatalf("read the seeded runs back: %v", err)
		}
		var decoded map[string]any
		if err := json.Unmarshal([]byte(binding), &decoded); err != nil {
			t.Fatalf("decode a seeded binding: %v", err)
		}
		if syntheticString(syntheticMap(decoded["sourceManifest"])["finalizationGenerationSlotKey"]) != slotKey {
			continue
		}
		claims++
		rootActionID, runID = candidateRoot, candidateRun
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the seeded runs back: %v", err)
	}
	if claims != 1 {
		t.Fatalf("%d runs claim the slot, want exactly the one the fixture wrote", claims)
	}
	return rootActionID, runID
}

// productionFinalizationGenerationCount reads one integer back over a read-only
// connection: what a write would have added is only absent when the file says so.
func productionFinalizationGenerationCount(t *testing.T, database, query string, args ...any) int {
	t.Helper()
	db := productionFinalizationGenerationReadOnly(t, database)
	defer func() { _ = db.Close() }()
	var found int
	if err := db.QueryRow(query, args...).Scan(&found); err != nil {
		t.Fatalf("read %q: %v", query, err)
	}
	return found
}

// productionFinalizationGenerationReadOnly opens the admitted author database
// without write authority, so nothing this test reads can repair what it is
// checking.
func productionFinalizationGenerationReadOnly(t *testing.T, database string) *sql.DB {
	t.Helper()
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(database)}
	uri.RawQuery = "mode=ro"
	db, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		t.Fatalf("open the author database read-only: %v", err)
	}
	db.SetMaxOpenConns(1)
	return db
}
