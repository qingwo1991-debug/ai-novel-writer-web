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

// The generated fixture is a baseline+M01+M03 verifier oracle. It cannot
// establish full-project schema admission or production author-data safety.
func TestM03MergedWithoutRecheckAgainstOriginalTypeScriptOracle(t *testing.T) {
	manifestPath := os.Getenv("GO_M03_GENERATED_CONTRACT")
	if manifestPath == "" {
		if os.Getenv("GITHUB_ACTIONS") == "true" {
			t.Fatal("original TypeScript generated M03 oracle required on GitHub Actions")
		}
		t.Skip("original TypeScript generated M03 oracle is generated on GitHub Actions")
	}
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var oracle struct {
		FormatVersion int    `json:"formatVersion"`
		Scope         string `json:"scope"`
		Cases         []struct {
			Name, File string
			Expected   bool
			SHA256     string `json:"sha256"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &oracle); err != nil {
		t.Fatal(err)
	}
	if oracle.FormatVersion != 1 || oracle.Scope != "m03-verifier-only" || len(oracle.Cases) != 7 {
		t.Fatal("unexpected original TypeScript M03 generated oracle")
	}
	// This verifier intentionally refuses generated cycles; only a verified
	// merge without recheck can succeed in this isolated lane.
	want := map[string]bool{
		"valid-generated":             false,
		"missing-composition-source":  false,
		"discarded-revision-artifact": false,
		"wrong-confirmation-binding":  false,
		"tampered-revision-body":      false,
		"valid-merge-committed":       true,
		"merge-body-hash":             false,
	}
	seen := make(map[string]bool)
	for _, sample := range oracle.Cases {
		expected, known := want[sample.Name]
		if !known || seen[sample.Name] || !filepath.IsAbs(sample.File) || !m03Hash(sample.SHA256) {
			t.Fatalf("unexpected original TypeScript case: %+v", sample)
		}
		seen[sample.Name] = true
		if sample.Name == "valid-generated" || sample.Name == "valid-merge-committed" {
			if !sample.Expected {
				t.Fatalf("original TypeScript rejected intended positive case %s", sample.Name)
			}
		} else if sample.Expected {
			t.Fatalf("original TypeScript accepted corrupted case %s", sample.Name)
		}
		t.Run(sample.Name, func(t *testing.T) {
			before, err := os.ReadFile(sample.File)
			if err != nil {
				t.Fatal(err)
			}
			if fmt.Sprintf("%x", sha256.Sum256(before)) != sample.SHA256 {
				t.Fatal("original M03 fixture hash differs")
			}
			// The original TypeScript oracle copies the fixture to a writable
			// database and enables foreign keys before verifyM03ReviewCycle. A
			// read-only connection cannot enable PRAGMA foreign_keys, so verify
			// on a writable copy while leaving the original fixture untouched.
			copyFile, err := os.CreateTemp("", "novel-m03-merge-*.db")
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
			verifyErr := VerifyM03MergedWithoutRecheckCycles(ctx, tx)
			if err := tx.Rollback(); err != nil {
				t.Fatal(err)
			}
			if (verifyErr == nil) != expected {
				t.Fatalf("Go merged M03 verifier differs: %v", verifyErr)
			}
			after, err := os.ReadFile(sample.File)
			if err != nil {
				t.Fatal(err)
			}
			if fmt.Sprintf("%x", sha256.Sum256(after)) != sample.SHA256 {
				t.Fatal("read-only verifier changed original fixture")
			}
		})
	}
	if len(seen) != len(want) {
		t.Fatal("original M03 generated oracle incomplete")
	}
}
