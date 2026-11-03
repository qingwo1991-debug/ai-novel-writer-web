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

// The original TypeScript verifier generates these isolated synthetic SQLite
// databases on GitHub Actions. They are not complete project admission data.
func TestM03GeneratedAgainstOriginalTypeScriptOracle(t *testing.T) {
	path := os.Getenv("GO_M03_GENERATED_CONTRACT")
	if path == "" {
		if os.Getenv("GITHUB_ACTIONS") == "true" {
			t.Fatal("original TypeScript generated M03 oracle required")
		}
		t.Skip("GitHub-only generated M03 oracle")
	}
	manifestBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
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
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.FormatVersion != 1 || manifest.Scope != "m03-verifier-only" || len(manifest.Cases) != 7 {
		t.Fatalf("unexpected original generated M03 oracle: version=%d scope=%q cases=%d",
			manifest.FormatVersion, manifest.Scope, len(manifest.Cases))
	}
	wantNames := map[string]bool{
		"valid-generated":             true,
		"missing-composition-source":  false,
		"discarded-revision-artifact": false,
		"wrong-confirmation-binding":  false,
		"tampered-revision-body":      false,
	}
	seen := make(map[string]bool)
	for _, sample := range manifest.Cases {
		want, included := wantNames[sample.Name]
		if !included {
			continue
		}
		if seen[sample.Name] || sample.Expected != want || sample.CycleCount != 1 ||
			sample.FindingCount != 1 || !filepath.IsAbs(sample.File) || !m03Hash(sample.SHA256) {
			t.Fatalf("unexpected generated M03 oracle case: %+v", sample)
		}
		seen[sample.Name] = true
		t.Run(sample.Name, func(t *testing.T) {
			before, err := os.ReadFile(sample.File)
			if err != nil {
				t.Fatal(err)
			}
			if fmt.Sprintf("%x", sha256.Sum256(before)) != sample.SHA256 {
				t.Fatal("original generated M03 fixture hash differs")
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
			verifyErr := VerifyM03GeneratedCycles(ctx, tx)
			if err := tx.Rollback(); err != nil {
				t.Fatal(err)
			}
			if (verifyErr == nil) != want {
				t.Fatalf("Go generated M03 verifier differs from original TypeScript: %v", verifyErr)
			}
			after, err := os.ReadFile(sample.File)
			if err != nil {
				t.Fatal(err)
			}
			if fmt.Sprintf("%x", sha256.Sum256(after)) != sample.SHA256 {
				t.Fatal("read-only generated verifier changed original fixture")
			}
		})
	}
	if len(seen) != len(wantNames) {
		t.Fatalf("generated M03 oracle incomplete: got %d cases, want %d", len(seen), len(wantNames))
	}
}
