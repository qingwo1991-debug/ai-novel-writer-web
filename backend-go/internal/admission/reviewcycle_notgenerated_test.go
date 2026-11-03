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

// The original TypeScript verifier writes these synthetic SQLite fixtures on
// a GitHub runner. Their limited schema is a verifier oracle, not a recognized
// project snapshot eligible for production admission.
func TestM03NotGeneratedAgainstOriginalTypeScriptOracle(t *testing.T) {
	manifestPath := os.Getenv("GO_M03_NOTGENERATED_CONTRACT")
	if manifestPath == "" {
		if os.Getenv("GITHUB_ACTIONS") == "true" {
			t.Fatal("original TypeScript M03 oracle required on GitHub Actions")
		}
		t.Skip("original TypeScript M03 oracle is generated on GitHub Actions")
	}
	manifestBytes, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		FormatVersion int    `json:"formatVersion"`
		Scope         string `json:"scope"`
		Cases         []struct {
			Name         string `json:"name"`
			File         string `json:"file"`
			Expected     bool   `json:"expected"`
			CycleCount   int    `json:"cycleCount"`
			FindingCount int    `json:"findingCount"`
			SHA256       string `json:"sha256"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.FormatVersion != 1 || manifest.Scope != "m03-verifier-only" || len(manifest.Cases) != 9 {
		t.Fatalf("unexpected M03 verifier oracle: version=%d scope=%q cases=%d",
			manifest.FormatVersion, manifest.Scope, len(manifest.Cases))
	}
	wantNames := map[string]bool{
		"valid-not-generated":            true,
		"review-body-hash":               false,
		"missing-effect-index":           false,
		"discarded-review-artifact":      false,
		"rebound-report-artifact":        false,
		"finding-set-hash":               false,
		"wrong-anchored-excerpt":         false,
		"frozen-source-binding":          false,
		"generated-without-confirmation": false,
	}
	seen := make(map[string]bool)
	for _, sample := range manifest.Cases {
		sample := sample
		want, known := wantNames[sample.Name]
		if !known || seen[sample.Name] || sample.Expected != want || sample.CycleCount != 1 || sample.FindingCount != 1 ||
			!filepath.IsAbs(sample.File) || !m03Hash(sample.SHA256) {
			t.Fatalf("unexpected M03 oracle case: %+v", sample)
		}
		seen[sample.Name] = true
		t.Run(sample.Name, func(t *testing.T) {
			before, err := os.ReadFile(sample.File)
			if err != nil {
				t.Fatal(err)
			}
			if fmt.Sprintf("%x", sha256.Sum256(before)) != sample.SHA256 {
				t.Fatal("original oracle fixture hash differs")
			}
			// The original TypeScript oracle copies the fixture to a writable
			// database and enables foreign keys before verifyM03ReviewCycle. A
			// read-only connection cannot enable PRAGMA foreign_keys, so verify
			// on a writable copy while leaving the original fixture untouched.
			copyFile, err := os.CreateTemp("", "novel-m03-notgenerated-*.db")
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
			verifyErr := VerifyM03NotGeneratedCycles(ctx, tx)
			if err := tx.Rollback(); err != nil {
				t.Fatal(err)
			}
			if (verifyErr == nil) != want {
				t.Fatalf("Go M03 verifier differs from original TypeScript: %v", verifyErr)
			}
			after, err := os.ReadFile(sample.File)
			if err != nil {
				t.Fatal(err)
			}
			if fmt.Sprintf("%x", sha256.Sum256(after)) != sample.SHA256 {
				t.Fatal("read-only verifier changed original oracle fixture")
			}
		})
	}
	if len(seen) != len(wantNames) {
		t.Fatal("original M03 oracle cases incomplete")
	}
}
