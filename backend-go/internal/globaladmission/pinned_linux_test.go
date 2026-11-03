//go:build linux

package globaladmission

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

const pinnedTestGeneration = "12345678-1234-4234-8234-123456789abc"

func pinnedFixture(t *testing.T) (Roots, string, string) {
	t.Helper()
	root := t.TempDir()
	canonical := filepath.Join(root, "canonical")
	control := filepath.Join(canonical, ".migration")
	dataRoot := filepath.Join(canonical, "generations", pinnedTestGeneration)
	userData := filepath.Join(root, "user-data")
	for _, directory := range []string{control, dataRoot, userData} {
		if err := os.MkdirAll(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	receipt := filepath.Join(control, "receipt.json")
	raw := `{"version":1,"generation":"` + pinnedTestGeneration + `","completed":true,"preservedUnknownCount":0,"requiredObjects":[]}`
	if err := os.WriteFile(receipt, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	return Roots{LegacySource: filepath.Join(root, "legacy"), CanonicalTarget: canonical, UserData: userData}, receipt, dataRoot
}

func TestPinnedCompletedRejectsReceiptReplacementAndMutation(t *testing.T) {
	for _, change := range []string{"replacement", "in-place", "symlink"} {
		t.Run(change, func(t *testing.T) {
			roots, receipt, _ := pinnedFixture(t)
			pinned, err := PinCompleted(context.Background(), roots, true)
			if err != nil {
				t.Fatal(err)
			}
			defer pinned.Close()
			if ready, err := pinned.Verify(context.Background()); err != nil || ready.GlobalGeneration != pinnedTestGeneration {
				t.Fatalf("valid pinned receipt rejected: %+v %v", ready, err)
			}
			switch change {
			case "replacement":
				raw, err := os.ReadFile(receipt)
				if err != nil {
					t.Fatal(err)
				}
				replacement := filepath.Join(filepath.Dir(receipt), "replacement")
				if err := os.WriteFile(replacement, raw, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(replacement, receipt); err != nil {
					t.Fatal(err)
				}
			case "in-place":
				if err := os.WriteFile(receipt, []byte(`{"version":1,"generation":"`+pinnedTestGeneration+`","completed":true,"preservedUnknownCount":1,"requiredObjects":[]}`), 0600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Remove(receipt); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(roots.UserData, receipt); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := pinned.Verify(context.Background()); err == nil {
				t.Fatal("changed receipt retained pinned authority")
			}
		})
	}
}

func TestPinnedCompletedRejectsGenerationAndAncestorReplacement(t *testing.T) {
	for _, change := range []string{"generation", "ancestor"} {
		t.Run(change, func(t *testing.T) {
			roots, _, dataRoot := pinnedFixture(t)
			pinned, err := PinCompleted(context.Background(), roots, true)
			if err != nil {
				t.Fatal(err)
			}
			defer pinned.Close()
			switch change {
			case "generation":
				if err := os.Rename(dataRoot, dataRoot+".old"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(dataRoot, 0700); err != nil {
					t.Fatal(err)
				}
			case "ancestor":
				if err := os.Rename(roots.CanonicalTarget, roots.CanonicalTarget+".old"); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(dataRoot, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(filepath.Join(roots.CanonicalTarget, ".migration"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := pinned.Verify(context.Background()); err == nil {
				t.Fatal("replaced generation or ancestor retained pinned authority")
			}
		})
	}
}

func TestPinnedIdentityAllowsGenerationContentWritesWhileFullAuditRejectsInvalidContent(t *testing.T) {
	roots, _, dataRoot := pinnedFixture(t)
	pinned, err := PinCompleted(context.Background(), roots, true)
	if err != nil {
		t.Fatal(err)
	}
	defer pinned.Close()
	if err := os.WriteFile(filepath.Join(dataRoot, "config.json"), []byte(`{"defaultModelId":"missing-model"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if ready, err := pinned.VerifyIdentity(context.Background()); err != nil || ready.GlobalGeneration != pinnedTestGeneration {
		t.Fatalf("content write changed pinned identity: %+v %v", ready, err)
	}
	if _, err := pinned.Verify(context.Background()); Code(err) != "GLOBAL_DEFAULT_MODEL_MISSING" {
		t.Fatalf("full semantic audit accepted invalid config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dataRoot, "config.json"), []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := pinned.Verify(context.Background()); err != nil {
		t.Fatalf("valid content update rejected by full audit: %v", err)
	}
}

func TestPinnedCompletedOriginalTypeScriptReadyFixtures(t *testing.T) {
	manifest := os.Getenv("GO_GLOBAL_MIGRATION_CONTRACT")
	if manifest == "" {
		if os.Getenv("GITHUB_ACTIONS") == "true" {
			t.Fatal("original global migration oracle required")
		}
		t.Skip("GitHub original TypeScript oracle required")
	}
	raw, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	var oracle struct {
		Cases []struct {
			LegacySource    string `json:"legacySource"`
			CanonicalTarget string `json:"canonicalTarget"`
			UserData        string `json:"userData"`
			Expected        struct {
				State            string `json:"state"`
				GlobalGeneration string `json:"globalGeneration"`
			} `json:"expected"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &oracle); err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, sample := range oracle.Cases {
		if sample.Expected.State != "ready" {
			continue
		}
		seen++
		roots := Roots{LegacySource: sample.LegacySource, CanonicalTarget: sample.CanonicalTarget, UserData: sample.UserData}
		pinned, err := PinCompleted(context.Background(), roots, true)
		if err != nil {
			t.Fatal(err)
		}
		ready, err := pinned.Verify(context.Background())
		closeErr := pinned.Close()
		if err != nil || closeErr != nil || ready.GlobalGeneration != sample.Expected.GlobalGeneration {
			t.Fatalf("original ready fixture changed: %+v verify=%v close=%v", ready, err, closeErr)
		}
	}
	if seen == 0 {
		t.Fatal("no original ready fixture")
	}
}
