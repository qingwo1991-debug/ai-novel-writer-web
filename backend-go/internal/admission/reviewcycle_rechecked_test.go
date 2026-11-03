package admission

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// The original TypeScript verifier creates these synthetic M03-only databases
// on a GitHub runner. A match here is not full-project or production admission.
func TestM03RecheckedAgainstOriginalTypeScriptOracle(t *testing.T) {
	path := os.Getenv("GO_M03_RECHECKED_CONTRACT")
	if path == "" {
		if os.Getenv("GITHUB_ACTIONS") == "true" {
			t.Fatal("original TypeScript rechecked M03 oracle required")
		}
		t.Skip("GitHub-only original TypeScript rechecked M03 oracle")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var oracle struct {
		FormatVersion int    `json:"formatVersion"`
		Scope         string `json:"scope"`
		Cases         []struct {
			Name, File   string
			Expected     bool
			CycleCount   int    `json:"cycleCount"`
			FindingCount int    `json:"findingCount"`
			SHA256       string `json:"sha256"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &oracle); err != nil {
		t.Fatal(err)
	}
	if oracle.FormatVersion != 1 || oracle.Scope != "m03-verifier-only" || len(oracle.Cases) != 7 {
		t.Fatal("unexpected original TypeScript rechecked M03 oracle")
	}
	want := map[string]bool{
		"valid-rechecked-v1":          true,
		"wrong-recheck-merged-hash":   false,
		"missing-recheck-receipt":     false,
		"wrong-recheck-evidence-hash": false,
		"finding-evidence-rebound":    false,
		"discarded-recheck-artifact":  false,
		"wrong-recheck-source":        false,
	}
	seen := map[string]bool{}
	for _, sample := range oracle.Cases {
		expected, known := want[sample.Name]
		if !known || seen[sample.Name] || sample.Expected != expected ||
			sample.CycleCount != 1 || sample.FindingCount != 1 ||
			!filepath.IsAbs(sample.File) || !m03Hash(sample.SHA256) {
			t.Fatalf("unexpected rechecked case: %+v", sample)
		}
		seen[sample.Name] = true
		t.Run(sample.Name, func(t *testing.T) {
			before, err := os.ReadFile(sample.File)
			if err != nil {
				t.Fatal(err)
			}
			if fmt.Sprintf("%x", sha256.Sum256(before)) != sample.SHA256 {
				t.Fatal("original TypeScript database hash differs")
			}
			// The original TypeScript oracle copies the fixture to a writable
			// database and enables foreign keys before verifyM03ReviewCycle. A
			// read-only connection cannot enable PRAGMA foreign_keys, so verify
			// on a writable copy while leaving the original fixture untouched.
			copyFile, err := os.CreateTemp("", "novel-m03-rechecked-*.db")
			if err != nil {
				t.Fatal(err)
			}
			copyPath := copyFile.Name()
			defer os.Remove(copyPath)
			if err := copyFile.Chmod(0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := copyFile.Write(before); err != nil {
				t.Fatal(err)
			}
			if err := copyFile.Close(); err != nil {
				t.Fatal(err)
			}
			db, err := sql.Open("sqlite3", "file:"+copyPath+"?mode=rw")
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			db.SetMaxOpenConns(1)
			if _, err := db.Exec("PRAGMA foreign_keys=ON"); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			verifyErr := VerifyM03RecheckedCycles(ctx, tx)
			if err := tx.Rollback(); err != nil {
				t.Fatal(err)
			}
			if (verifyErr == nil) != expected {
				t.Fatalf("Go rechecked M03 verifier differs from TypeScript: %v", verifyErr)
			}
			after, err := os.ReadFile(sample.File)
			if err != nil {
				t.Fatal(err)
			}
			if fmt.Sprintf("%x", sha256.Sum256(after)) != sample.SHA256 {
				t.Fatal("read-only Go verifier changed original database")
			}
		})
	}
	if len(seen) != len(want) {
		t.Fatal("original rechecked M03 cases incomplete")
	}
}
