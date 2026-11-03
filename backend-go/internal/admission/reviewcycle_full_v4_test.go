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

// The original desktop registry produces these recognized v4 SQLite files
// on an isolated GitHub runner. This proves the M03 verifier on a full v4
// schema, not permission to admit author data into the production runtime.
func TestM03NotGeneratedFullV4AgainstOriginalTypeScript(t *testing.T) {
	manifestPath := os.Getenv("GO_M03_FULL_V4_CONTRACT")
	if manifestPath == "" {
		if os.Getenv("GITHUB_ACTIONS") == "true" {
			t.Fatal("original TypeScript full-v4 M03 oracle required on GitHub Actions")
		}
		t.Skip("GitHub-only original TypeScript full-v4 M03 oracle")
	}
	manifestBytes, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		FormatVersion     int    `json:"formatVersion"`
		Scope             string `json:"scope"`
		SchemaFingerprint string `json:"schemaFingerprint"`
		Cases             []struct {
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
	if manifest.FormatVersion != 1 || manifest.Scope != "recognized-v4-m03" ||
		!m03Hash(manifest.SchemaFingerprint) || len(manifest.Cases) != 3 {
		t.Fatalf("unexpected full-v4 M03 oracle: version=%d scope=%q cases=%d",
			manifest.FormatVersion, manifest.Scope, len(manifest.Cases))
	}
	want := map[string]bool{
		"valid-full-v4-not-generated": true,
		"full-v4-review-hash":         false,
		"full-v4-discarded-artifact":  false,
	}
	seen := make(map[string]bool)
	for _, sample := range manifest.Cases {
		expected, known := want[sample.Name]
		if !known || seen[sample.Name] || sample.Expected != expected ||
			sample.CycleCount != 1 || sample.FindingCount != 1 ||
			!filepath.IsAbs(sample.File) || !m03Hash(sample.SHA256) {
			t.Fatalf("unexpected original full-v4 case: %+v", sample)
		}
		seen[sample.Name] = true
		t.Run(sample.Name, func(t *testing.T) {
			before, err := os.ReadFile(sample.File)
			if err != nil {
				t.Fatal(err)
			}
			if fmt.Sprintf("%x", sha256.Sum256(before)) != sample.SHA256 {
				t.Fatal("original full-v4 database hash differs")
			}
			db, err := sql.Open("sqlite3", "file:"+sample.File+"?mode=ro")
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			db.SetMaxOpenConns(1)
			ctx := context.Background()
			tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			var version int
			if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
				t.Fatal(err)
			}
			if version != 4 {
				t.Fatalf("full-v4 oracle version changed: %d", version)
			}
			fingerprint, _, err := schemaFingerprint(ctx, tx)
			if err != nil {
				t.Fatal(err)
			}
			if fingerprint != manifest.SchemaFingerprint {
				t.Fatal("Go schema fingerprint differs from original full-v4 oracle")
			}
			verifyErr := VerifyM03NotGeneratedCycles(ctx, tx)
			if err := tx.Rollback(); err != nil {
				t.Fatal(err)
			}
			if (verifyErr == nil) != expected {
				t.Fatalf("Go full-v4 M03 verifier differs from original TypeScript: %v", verifyErr)
			}
			after, err := os.ReadFile(sample.File)
			if err != nil {
				t.Fatal(err)
			}
			if fmt.Sprintf("%x", sha256.Sum256(after)) != sample.SHA256 {
				t.Fatal("read-only Go verifier changed original full-v4 database")
			}
		})
	}
	if len(seen) != len(want) {
		t.Fatal("original full-v4 M03 oracle cases incomplete")
	}
}
