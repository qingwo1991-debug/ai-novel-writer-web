//go:build linux

package globaladmission

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

type oracleFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int    `json:"size"`
}

func verifyOracleFiles(t *testing.T, root string, files []oracleFile) {
	t.Helper()
	for _, entry := range files {
		path := filepath.Join(root, filepath.FromSlash(entry.Path))
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		hash := sha256.Sum256(data)
		if len(data) != entry.Size || hex.EncodeToString(hash[:]) != entry.SHA256 {
			t.Fatalf("original oracle file changed: %s", path)
		}
	}
}

func TestCompletedGlobalMigrationMatchesOriginalTypeScriptOracle(t *testing.T) {
	manifest := os.Getenv("GO_GLOBAL_MIGRATION_CONTRACT")
	if manifest == "" {
		if os.Getenv("GITHUB_ACTIONS") == "true" {
			t.Fatal("original global migration oracle required")
		}
		t.Skip("GitHub original global migration oracle required")
	}
	data, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	var oracle struct {
		FormatVersion int `json:"formatVersion"`
		Cases         []struct {
			Name            string `json:"name"`
			LegacySource    string `json:"legacySource"`
			CanonicalTarget string `json:"canonicalTarget"`
			UserData        string `json:"userData"`
			Expected        struct {
				State                 string `json:"state"`
				Code                  string `json:"code"`
				GlobalGeneration      string `json:"globalGeneration"`
				DataRoot              string `json:"dataRoot"`
				LegacySourceIgnored   bool   `json:"legacySourceIgnored"`
				PreservedUnknownCount int64  `json:"preservedUnknownCount"`
			} `json:"expected"`
			ReceiptRaw      string       `json:"receiptRaw"`
			RequiredObjects []string     `json:"requiredObjects"`
			GenerationFiles []oracleFile `json:"generationFiles"`
			LegacyFiles     []oracleFile `json:"legacyFiles"`
			UserDataFiles   []oracleFile `json:"userDataFiles"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &oracle); err != nil {
		t.Fatal(err)
	}
	if oracle.FormatVersion != 1 || len(oracle.Cases) != 4 {
		t.Fatalf("unexpected oracle: %+v", oracle)
	}
	for _, test := range oracle.Cases {
		t.Run(test.Name, func(t *testing.T) {
			receiptPath := filepath.Join(test.CanonicalTarget, ".migration", "receipt.json")
			before, err := os.ReadFile(receiptPath)
			if err != nil || string(before) != test.ReceiptRaw {
				t.Fatal("original receipt missing or changed", err)
			}
			var receipt struct {
				Generation      string   `json:"generation"`
				RequiredObjects []string `json:"requiredObjects"`
			}
			if err := json.Unmarshal(before, &receipt); err != nil {
				t.Fatal(err)
			}
			if len(receipt.RequiredObjects) != len(test.RequiredObjects) {
				t.Fatal("oracle required objects changed")
			}
			for i := range receipt.RequiredObjects {
				if receipt.RequiredObjects[i] != test.RequiredObjects[i] {
					t.Fatal("oracle required objects changed")
				}
			}
			generationRoot := filepath.Join(test.CanonicalTarget, "generations", receipt.Generation)
			verifyOracleFiles(t, generationRoot, test.GenerationFiles)
			verifyOracleFiles(t, test.LegacySource, test.LegacyFiles)
			verifyOracleFiles(t, test.UserData, test.UserDataFiles)
			ready, err := VerifyCompleted(context.Background(), Roots{LegacySource: test.LegacySource,
				CanonicalTarget: test.CanonicalTarget, UserData: test.UserData})
			if test.Expected.State == "ready" {
				if err != nil || ready.State != "ready" || ready.GlobalGeneration != test.Expected.GlobalGeneration ||
					ready.DataRoot != test.Expected.DataRoot || ready.LegacySourceIgnored != test.Expected.LegacySourceIgnored ||
					ready.PreservedUnknownCount != test.Expected.PreservedUnknownCount {
					t.Fatalf("Go receipt read differs from original: ready=%+v err=%v expected=%+v", ready, err, test.Expected)
				}
			} else if test.Expected.State == "blocked" {
				if Code(err) != test.Expected.Code || ready != (Ready{}) {
					t.Fatalf("Go receipt rejection differs from original: ready=%+v err=%v expected=%+v", ready, err, test.Expected)
				}
			} else {
				t.Fatalf("invalid original oracle state: %q", test.Expected.State)
			}
			after, err := os.ReadFile(receiptPath)
			if err != nil || string(after) != test.ReceiptRaw {
				t.Fatal("Go verification altered receipt", err)
			}
			verifyOracleFiles(t, generationRoot, test.GenerationFiles)
			verifyOracleFiles(t, test.LegacySource, test.LegacyFiles)
			verifyOracleFiles(t, test.UserData, test.UserDataFiles)
		})
	}
}

func TestCompletedGlobalMigrationRejectsLinkedReceiptAndGeneration(t *testing.T) {
	root := t.TempDir()
	legacy := filepath.Join(root, "legacy")
	canonical := filepath.Join(root, "canonical")
	userData := filepath.Join(root, "user-data")
	control := filepath.Join(canonical, ".migration")
	if err := os.MkdirAll(control, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(userData, 0700); err != nil {
		t.Fatal(err)
	}
	const generation = "12345678-1234-4234-8234-123456789abc"
	valid := []byte(`{"version":1,"generation":"` + generation + `","completed":true,"preservedUnknownCount":0,"requiredObjects":[]}`)
	outside := filepath.Join(root, "outside-receipt.json")
	if err := os.WriteFile(outside, valid, 0600); err != nil {
		t.Fatal(err)
	}
	receipt := filepath.Join(control, "receipt.json")
	if err := os.Symlink(outside, receipt); err != nil {
		t.Fatal(err)
	}
	roots := Roots{LegacySource: legacy, CanonicalTarget: canonical, UserData: userData}
	if _, err := VerifyCompleted(context.Background(), roots); Code(err) != "GLOBAL_JSON_INVALID" {
		t.Fatalf("linked receipt accepted: %v", err)
	}
	if err := os.Remove(receipt); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(receipt, valid, 0600); err != nil {
		t.Fatal(err)
	}
	generations := filepath.Join(canonical, "generations")
	if err := os.Mkdir(generations, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(root, filepath.Join(generations, generation)); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyCompleted(context.Background(), roots); Code(err) != "GLOBAL_ROOT_UNSAFE" {
		t.Fatalf("linked generation accepted: %v", err)
	}
}

func TestCompletedGlobalMigrationKeepsJSTrimAndSurrogateIdentity(t *testing.T) {
	root := t.TempDir()
	canonical := filepath.Join(root, "canonical")
	userData := filepath.Join(root, "user-data")
	const generation = "12345678-1234-4234-8234-123456789abc"
	dataRoot := filepath.Join(canonical, "generations", generation)
	control := filepath.Join(canonical, ".migration")
	for _, directory := range []string{dataRoot, control, userData} {
		if err := os.MkdirAll(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	put := func(path, value string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	put(filepath.Join(control, "receipt.json"), `{"version":1,"generation":"`+generation+`","completed":true,"preservedUnknownCount":0,"requiredObjects":[]}`)
	roots := Roots{LegacySource: filepath.Join(root, "legacy"), CanonicalTarget: canonical, UserData: userData}
	mcp := filepath.Join(dataRoot, "mcp_config.json")
	put(mcp, `{"mcpServers":{"bad":{"command":"\ufeff"}}}`)
	if _, err := VerifyCompleted(context.Background(), roots); Code(err) != "GLOBAL_MCP_INVALID" {
		t.Fatalf("FEFF-only command accepted, unlike JS trim: %v", err)
	}
	if err := os.Remove(mcp); err != nil {
		t.Fatal(err)
	}
	put(filepath.Join(dataRoot, "models.json"), `[{"id":"\ud800"}]`)
	put(filepath.Join(dataRoot, "config.json"), `{"defaultModelId":"\ud801"}`)
	if _, err := VerifyCompleted(context.Background(), roots); Code(err) != "GLOBAL_JSON_INVALID" {
		t.Fatalf("distinct lone surrogate IDs were merged: %v", err)
	}
	if loneSurrogateEscape(`"\ud83d\ude00"`) {
		t.Fatal("valid UTF-16 pair rejected")
	}
}
