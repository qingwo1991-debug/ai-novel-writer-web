package admission

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// GitHub CI generates this oracle from the real TypeScript migration registry
// using synthetic in-memory schemas. No desktop author database is involved.
func TestTypeScriptRegistryContract(t *testing.T) {
	indexPath := os.Getenv("GO_SQLITE_CONTRACT")
	if indexPath == "" {
		t.Skip("GO_SQLITE_CONTRACT is supplied by GitHub's TypeScript contract builder")
	}
	encoded, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	contract, err := ParseContract(file)
	file.Close()
	if err != nil {
		t.Fatal(err)
	}
	var index struct {
		Fixtures []struct {
			Name        string `json:"name"`
			File        string `json:"file"`
			Version     int    `json:"version"`
			Fingerprint string `json:"fingerprint"`
		} `json:"fixtures"`
	}
	if err := json.Unmarshal(encoded, &index); err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool)
	for _, fixture := range index.Fixtures {
		fixture := fixture
		seen[filepath.Base(fixture.File)] = true
		t.Run(fixture.Name, func(t *testing.T) {
			path := fixture.File
			if !filepath.IsAbs(path) {
				path = filepath.Join(filepath.Dir(indexPath), path)
			}
			before := hashFile(t, path)
			evidence, err := VerifySnapshot(context.Background(), path, contract)
			if fixture.Version < CurrentSchemaVersion {
				requireCode(t, err, CodeMigrationRequired)
			} else if fixture.Version > CurrentSchemaVersion {
				requireCode(t, err, CodeNewerSchema)
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if evidence.UserVersion != fixture.Version || evidence.SchemaFingerprint != fixture.Fingerprint {
					t.Fatalf("Go/TS schema identity mismatch: %+v; want version %d fingerprint %s", evidence, fixture.Version, fixture.Fingerprint)
				}
				if evidence.TableCounts["contents"] != 1 {
					t.Fatalf("synthetic author row lost: %+v", evidence.TableCounts)
				}
				if evidence.ContentIdentity.Database != before {
					t.Fatal("Go/TS physical identity mismatch")
				}
			}
			if hashFile(t, path) != before {
				t.Fatal("contract verifier changed fixture bytes")
			}
		})
	}
	for _, required := range []string{"baseline-v7.db", "donor-v7.db", "baseline-v6.db"} {
		if !seen[required] {
			t.Errorf("TypeScript contract is missing mandatory fixture %s", required)
		}
	}
}
