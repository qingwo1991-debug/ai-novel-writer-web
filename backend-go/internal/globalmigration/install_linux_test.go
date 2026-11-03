//go:build linux && amd64

package globalmigration

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/globaladmission"
)

func TestCompleteVerifiedMatchesOriginalTypeScriptCheckpoint(t *testing.T) {
	contract := os.Getenv("GO_GLOBAL_INSTALL_CONTRACT")
	if contract == "" {
		if os.Getenv("GITHUB_ACTIONS") == "true" {
			t.Fatal("original TypeScript INSTALLED oracle required")
		}
		t.Skip("GitHub original TypeScript INSTALLED oracle required")
	}
	data, err := os.ReadFile(contract)
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		FormatVersion int `json:"formatVersion"`
		Cases         []struct {
			Name      string                `json:"name"`
			Roots     globaladmission.Roots `json:"goRoots"`
			SeedFiles struct {
				Legacy    []oracleFile `json:"legacy"`
				Canonical []oracleFile `json:"canonical"`
				UserData  []oracleFile `json:"userData"`
			} `json:"seedFiles"`
			Oracle struct {
				Checkpoint            string       `json:"checkpoint"`
				Generation            string       `json:"generation"`
				Source                string       `json:"source"`
				VerifiedJournalRaw    string       `json:"verifiedJournalRaw"`
				StagedFiles           []oracleFile `json:"stagedFiles"`
				FinalReceiptRaw       string       `json:"finalReceiptRaw"`
				PreservedUnknownCount int64        `json:"preservedUnknownCount"`
				Resumed               struct {
					State            string `json:"state"`
					GlobalGeneration string `json:"globalGeneration"`
				} `json:"resumed"`
				AfterSourceChange *struct {
					State            string `json:"state"`
					GlobalGeneration string `json:"globalGeneration"`
				} `json:"afterSourceChange"`
			} `json:"oracle"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.FormatVersion != 1 || len(manifest.Cases) != 5 {
		t.Fatalf("unexpected INSTALLED oracle: %+v", manifest)
	}
	for _, sample := range manifest.Cases {
		t.Run(sample.Name, func(t *testing.T) {
			roots := stageCloneRoots(t, sample.Roots)
			checkFiles(t, roots.LegacySource, sample.SeedFiles.Legacy, true)
			checkFiles(t, roots.CanonicalTarget, sample.SeedFiles.Canonical, true)
			checkFiles(t, roots.UserData, sample.SeedFiles.UserData, true)
			legacyBefore := treeInventory(t, roots.LegacySource)
			userBefore := treeInventory(t, roots.UserData)
			journalPath := filepath.Join(roots.CanonicalTarget, ".migration", "journal.json")
			raw, err := os.ReadFile(journalPath)
			if err != nil || string(raw) != sample.Oracle.VerifiedJournalRaw {
				t.Fatalf("VERIFIED journal handoff changed: %v", err)
			}
			ready, err := CompleteVerified(context.Background(), roots, true)
			if err != nil || ready.State != "ready" || ready.GlobalGeneration != sample.Oracle.Generation ||
				ready.PreservedUnknownCount != sample.Oracle.PreservedUnknownCount {
				t.Fatalf("receipt completion differs from original: ready=%+v err=%v", ready, err)
			}
			if ready.DataRoot != filepath.Join(roots.CanonicalTarget, "generations", sample.Oracle.Generation) {
				t.Fatalf("unexpected installed root: %s", ready.DataRoot)
			}
			checkFiles(t, ready.DataRoot, sample.Oracle.StagedFiles, true)
			if _, err := os.Lstat(ready.DataRoot + ".staging"); !os.IsNotExist(err) {
				t.Fatalf("staging remained after install: %v", err)
			}
			receiptPath := filepath.Join(roots.CanonicalTarget, ".migration", "receipt.json")
			receipt, err := os.ReadFile(receiptPath)
			if err != nil || string(receipt) != sample.Oracle.FinalReceiptRaw {
				t.Fatalf("receipt bytes differ from original: got=%s want=%s err=%v", receipt, sample.Oracle.FinalReceiptRaw, err)
			}
			again, err := CompleteVerified(context.Background(), roots, true)
			if err != nil || again != ready {
				t.Fatalf("receipt restart differs: ready=%+v err=%v", again, err)
			}
			// Recreate the original installed checkpoint: an installed generation
			// and VERIFIED journal exist, but the receipt has not been published.
			if sample.Oracle.Checkpoint == "installed" {
				if err := os.Remove(receiptPath); err != nil {
					t.Fatal(err)
				}
				resumed, err := CompleteVerified(context.Background(), roots, true)
				if err != nil || resumed != ready {
					t.Fatalf("installed restart differs: ready=%+v err=%v", resumed, err)
				}
				restored, err := os.ReadFile(receiptPath)
				if err != nil || string(restored) != sample.Oracle.FinalReceiptRaw {
					t.Fatalf("installed restart receipt differs: %v", err)
				}
			}
			if after := treeInventory(t, roots.LegacySource); !reflect.DeepEqual(after, legacyBefore) {
				t.Fatalf("legacy changed: before=%v after=%v", legacyBefore, after)
			}
			if after := treeInventory(t, roots.UserData); !reflect.DeepEqual(after, userBefore) {
				t.Fatalf("user-data changed: before=%v after=%v", userBefore, after)
			}
			if sample.Oracle.Resumed.State != ready.State || sample.Oracle.Resumed.GlobalGeneration != ready.GlobalGeneration {
				t.Fatal("original restart did not preserve generation")
			}
			if sample.Oracle.AfterSourceChange != nil {
				if sample.Oracle.Source != "legacy" {
					t.Fatal("source-change oracle must use legacy")
				}
				if err := os.WriteFile(filepath.Join(roots.LegacySource, "config.json"), []byte(`{"defaultModelId":"changed-after-receipt"}`), 0600); err != nil {
					t.Fatal(err)
				}
				got, err := CompleteVerified(context.Background(), roots, true)
				if err != nil || got != ready || sample.Oracle.AfterSourceChange.State != got.State ||
					sample.Oracle.AfterSourceChange.GlobalGeneration != got.GlobalGeneration {
					t.Fatalf("source change after receipt rejected: ready=%+v err=%v", got, err)
				}
				unchanged, err := os.ReadFile(receiptPath)
				if err != nil || string(unchanged) != sample.Oracle.FinalReceiptRaw {
					t.Fatalf("receipt mutated after source change: %v", err)
				}
			}
			if sample.Oracle.Source == "legacy" && sample.Oracle.Checkpoint == "installed" {
				drifted := stageCloneRoots(t, sample.Roots)
				config := filepath.Join(drifted.LegacySource, "config.json")
				if err := os.WriteFile(config, []byte(`{"defaultModelId":"synthetic-model","changed":true}`), 0600); err != nil {
					t.Fatal(err)
				}
				_, err := CompleteVerified(context.Background(), drifted, true)
				if Code(err) != "GLOBAL_VERIFIED_SOURCE_CHANGED" {
					t.Fatalf("source drift accepted: %v", err)
				}
				for _, forbidden := range []string{filepath.Join(drifted.CanonicalTarget, ".migration", "receipt.json"),
					filepath.Join(drifted.CanonicalTarget, "generations", sample.Oracle.Generation)} {
					if _, err := os.Lstat(forbidden); !os.IsNotExist(err) {
						t.Fatalf("source drift published %s: %v", forbidden, err)
					}
				}
				installedDrift := stageCloneRoots(t, sample.Roots)
				generations := filepath.Join(installedDrift.CanonicalTarget, "generations")
				if err := os.Rename(filepath.Join(generations, sample.Oracle.Generation+".staging"),
					filepath.Join(generations, sample.Oracle.Generation)); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(generations, sample.Oracle.Generation, "config.json"),
					[]byte(`{"defaultModelId":"synthetic-model","changed":true}`), 0600); err != nil {
					t.Fatal(err)
				}
				_, err = CompleteVerified(context.Background(), installedDrift, true)
				if Code(err) != "GLOBAL_VERIFIED_SOURCE_CHANGED" {
					t.Fatalf("installed drift accepted: %v", err)
				}
				if _, err := os.Lstat(filepath.Join(installedDrift.CanonicalTarget, ".migration", "receipt.json")); !os.IsNotExist(err) {
					t.Fatalf("installed drift published receipt: %v", err)
				}
			}
			if sample.Oracle.Source == "legacy" && sample.Oracle.Checkpoint == "receipt" {
				pendingRoots := stageCloneRoots(t, sample.Roots)
				pendingPath := filepath.Join(pendingRoots.CanonicalTarget, ".migration",
					"receipt.json.00000000-0000-4000-8000-000000000000.tmp")
				if err := os.WriteFile(pendingPath, []byte(sample.Oracle.FinalReceiptRaw), 0600); err != nil {
					t.Fatal(err)
				}
				beforeInfo, err := os.Lstat(pendingPath)
				if err != nil {
					t.Fatal(err)
				}
				pendingReady, err := CompleteVerified(context.Background(), pendingRoots, true)
				if err != nil || pendingReady.GlobalGeneration != sample.Oracle.Generation {
					t.Fatalf("matching receipt temp not recovered: ready=%+v err=%v", pendingReady, err)
				}
				finalInfo, err := os.Lstat(filepath.Join(pendingRoots.CanonicalTarget, ".migration", "receipt.json"))
				if err != nil || !os.SameFile(beforeInfo, finalInfo) {
					t.Fatalf("receipt temp inode not promoted: %v", err)
				}
			}
		})
	}
}
