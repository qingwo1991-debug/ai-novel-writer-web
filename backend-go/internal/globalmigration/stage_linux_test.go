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

func TestVerifyPreparedMatchesOriginalTypeScriptCheckpoint(t *testing.T) {
	contract := os.Getenv("GO_GLOBAL_VERIFIED_CONTRACT")
	if contract == "" {
		if os.Getenv("GITHUB_ACTIONS") == "true" {
			t.Fatal("original TypeScript VERIFIED oracle required")
		}
		t.Skip("GitHub original TypeScript VERIFIED oracle required")
	}
	data, err := os.ReadFile(contract)
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		FormatVersion int `json:"formatVersion"`
		Cases         []struct {
			Name   string                `json:"name"`
			Roots  globaladmission.Roots `json:"goRoots"`
			Oracle struct {
				CheckpointStep     string       `json:"checkpointStep"`
				PreparedJournalRaw string       `json:"preparedJournalRaw"`
				PreparedJournal    journal      `json:"preparedJournal"`
				JournalRaw         string       `json:"journalRaw"`
				Journal            journal      `json:"journal"`
				StagingFiles       []oracleFile `json:"stagingFiles"`
				Restarted          struct {
					State            string `json:"state"`
					GlobalGeneration string `json:"globalGeneration"`
				} `json:"restarted"`
			} `json:"oracle"`
			SeedFiles struct {
				Legacy    []oracleFile `json:"legacy"`
				Canonical []oracleFile `json:"canonical"`
				UserData  []oracleFile `json:"userData"`
			} `json:"seedFiles"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.FormatVersion != 1 || len(manifest.Cases) != 4 {
		t.Fatalf("unexpected VERIFIED oracle: %+v", manifest)
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
			if err != nil || string(raw) != sample.Oracle.PreparedJournalRaw {
				t.Fatalf("PREPARED journal handoff changed: %v", err)
			}
			got, err := VerifyPrepared(context.Background(), roots, true)
			if err != nil || got.Phase != "verified" || got.Source != sample.Oracle.PreparedJournal.Source ||
				got.Generation != sample.Oracle.PreparedJournal.Generation {
				t.Fatalf("VERIFIED phase differs from original: got=%+v err=%v", got, err)
			}
			staging := filepath.Join(roots.CanonicalTarget, "generations", got.Generation+".staging")
			if sample.Oracle.Journal.Phase == "verified" {
				checkFiles(t, staging, sample.Oracle.StagingFiles, true)
			} else {
				// The interrupted original staged only one object. The resumed
				// Go phase must include that object and complete the remaining set.
				for _, staged := range sample.Oracle.StagingFiles {
					checkFiles(t, staging, []oracleFile{staged}, false)
				}
			}
			raw, err = os.ReadFile(journalPath)
			if err != nil {
				t.Fatal(err)
			}
			want := sample.Oracle.JournalRaw
			if sample.Oracle.Journal.Phase == "prepared" {
				verified := sample.Oracle.Journal
				verified.Phase = "verified"
				encoded, err := json.Marshal(verified)
				if err != nil {
					t.Fatal(err)
				}
				want = string(encoded)
			}
			if string(raw) != want {
				t.Fatalf("VERIFIED journal differs from original: got=%s want=%s", raw, want)
			}
			again, err := VerifyPrepared(context.Background(), roots, true)
			if err != nil || again != got {
				t.Fatalf("VERIFIED restart changed identity: got=%+v err=%v", again, err)
			}
			if _, err := os.Lstat(filepath.Join(roots.CanonicalTarget, "generations", got.Generation)); !os.IsNotExist(err) {
				t.Fatalf("stage installed generation: %v", err)
			}
			if _, err := os.Lstat(filepath.Join(roots.CanonicalTarget, ".migration", "receipt.json")); !os.IsNotExist(err) {
				t.Fatalf("stage published receipt: %v", err)
			}
			if after := treeInventory(t, roots.LegacySource); !reflect.DeepEqual(after, legacyBefore) {
				t.Fatalf("legacy mutated: before=%v after=%v", legacyBefore, after)
			}
			if after := treeInventory(t, roots.UserData); !reflect.DeepEqual(after, userBefore) {
				t.Fatalf("user-data mutated: before=%v after=%v", userBefore, after)
			}
			checkFiles(t, roots.LegacySource, sample.SeedFiles.Legacy, true)
			checkFiles(t, roots.UserData, sample.SeedFiles.UserData, true)
			if sample.Oracle.Restarted.State != "ready" || sample.Oracle.Restarted.GlobalGeneration != got.Generation {
				t.Fatal("original restart did not preserve generation")
			}
		})
	}
}

func stageCloneRoots(t *testing.T, source globaladmission.Roots) globaladmission.Roots {
	t.Helper()
	parent := t.TempDir()
	if err := os.Chmod(parent, 0700); err != nil {
		t.Fatal(err)
	}
	cloned := globaladmission.Roots{
		LegacySource: filepath.Join(parent, "legacy"), CanonicalTarget: filepath.Join(parent, "canonical"),
		UserData: filepath.Join(parent, "user-data"),
	}
	for _, pair := range [][2]string{{source.LegacySource, cloned.LegacySource},
		{source.CanonicalTarget, cloned.CanonicalTarget}, {source.UserData, cloned.UserData}} {
		if _, err := os.Lstat(pair[0]); os.IsNotExist(err) {
			continue
		} else if err != nil {
			t.Fatal(err)
		}
		err := filepath.WalkDir(pair[0], func(location string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			relative, err := filepath.Rel(pair[0], location)
			if err != nil {
				return err
			}
			destination := filepath.Join(pair[1], relative)
			if entry.IsDir() {
				return os.Mkdir(destination, 0700)
			}
			data, err := os.ReadFile(location)
			if err != nil {
				return err
			}
			return os.WriteFile(destination, data, 0600)
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return cloned
}

func stageTestRoots(t *testing.T) globaladmission.Roots {
	t.Helper()
	parent := t.TempDir()
	if err := os.Chmod(parent, 0700); err != nil {
		t.Fatal(err)
	}
	roots := globaladmission.Roots{
		LegacySource: filepath.Join(parent, "legacy"), CanonicalTarget: filepath.Join(parent, "canonical"),
		UserData: filepath.Join(parent, "user-data"),
	}
	for _, root := range []string{roots.LegacySource, roots.CanonicalTarget, roots.UserData} {
		if err := os.Mkdir(root, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(roots.LegacySource, "config.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	return roots
}

func TestVerifyPreparedRejectsUnsafeStagingEntries(t *testing.T) {
	for _, sample := range []struct {
		name, code string
		plant      func(t *testing.T, staging, parent string)
	}{
		{"unknown", "GLOBAL_STAGING_UNKNOWN_OBJECT", func(t *testing.T, staging, parent string) {
			t.Helper()
			if err := os.WriteFile(filepath.Join(staging, "untracked.txt"), []byte("x"), 0600); err != nil {
				t.Fatal(err)
			}
		}},
		{"symlink", "GLOBAL_ASSET_LINK", func(t *testing.T, staging, parent string) {
			t.Helper()
			if err := os.Symlink(filepath.Join(parent, "outside"), filepath.Join(staging, "config.json")); err != nil {
				t.Fatal(err)
			}
		}},
		{"hardlink", "GLOBAL_ASSET_HARDLINK", func(t *testing.T, staging, parent string) {
			t.Helper()
			outside := filepath.Join(parent, "outside")
			if err := os.WriteFile(outside, []byte("{}"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Link(outside, filepath.Join(staging, "config.json")); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(sample.name, func(t *testing.T) {
			roots := stageTestRoots(t)
			prepared, err := Prepare(context.Background(), roots, true)
			if err != nil {
				t.Fatal(err)
			}
			staging := filepath.Join(roots.CanonicalTarget, "generations", prepared.Generation+".staging")
			if err := os.Mkdir(staging, 0700); err != nil {
				t.Fatal(err)
			}
			sample.plant(t, staging, filepath.Dir(roots.CanonicalTarget))
			got, err := VerifyPrepared(context.Background(), roots, true)
			if got != (Prepared{}) || Code(err) != sample.code {
				t.Fatalf("unsafe staging entry accepted: got=%+v err=%v", got, err)
			}
			raw, err := os.ReadFile(filepath.Join(roots.CanonicalTarget, ".migration", "journal.json"))
			if err != nil {
				t.Fatal(err)
			}
			var j journal
			if err := json.Unmarshal(raw, &j); err != nil || j.Phase != "prepared" {
				t.Fatalf("unsafe staging advanced journal: %s %v", raw, err)
			}
		})
	}
}

func TestVerifyPreparedRejectsChangedSourceAfterVerified(t *testing.T) {
	roots := stageTestRoots(t)
	if _, err := Prepare(context.Background(), roots, true); err != nil {
		t.Fatal(err)
	}
	verified, err := VerifyPrepared(context.Background(), roots, true)
	if err != nil || verified.Phase != "verified" {
		t.Fatalf("initial verification failed: %+v %v", verified, err)
	}
	if err := os.WriteFile(filepath.Join(roots.LegacySource, "config.json"), []byte("{\"changed\":true}"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := VerifyPrepared(context.Background(), roots, true)
	if got != (Prepared{}) || Code(err) != "GLOBAL_VERIFIED_SOURCE_CHANGED" {
		t.Fatalf("changed verified source accepted: got=%+v err=%v", got, err)
	}
}

func TestVerifyPreparedRecoversMatchingRetainedTemp(t *testing.T) {
	roots := stageTestRoots(t)
	prepared, err := Prepare(context.Background(), roots, true)
	if err != nil {
		t.Fatal(err)
	}
	staging := filepath.Join(roots.CanonicalTarget, "generations", prepared.Generation+".staging")
	if err := os.Mkdir(staging, 0700); err != nil {
		t.Fatal(err)
	}
	temporary := filepath.Join(staging, "config.json.12345678-1234-4234-8234-123456789abc.tmp")
	if err := os.WriteFile(temporary, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Lstat(temporary)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := VerifyPrepared(context.Background(), roots, true)
	if err != nil || verified.Phase != "verified" || verified.Generation != prepared.Generation {
		t.Fatalf("matching retained temp was not recovered: %+v %v", verified, err)
	}
	if _, err := os.Lstat(temporary); !os.IsNotExist(err) {
		t.Fatalf("retained temp left behind: %v", err)
	}
	after, err := os.Lstat(filepath.Join(staging, "config.json"))
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("retained temp was not renamed to destination: %v", err)
	}
}

func TestVerifyPreparedRejectsChangedSourceAgainstPartialStaging(t *testing.T) {
	roots := stageTestRoots(t)
	prepared, err := Prepare(context.Background(), roots, true)
	if err != nil {
		t.Fatal(err)
	}
	staging := filepath.Join(roots.CanonicalTarget, "generations", prepared.Generation+".staging")
	if err := os.Mkdir(staging, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, "config.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(roots.LegacySource, "config.json"), []byte("{\"changed\":true}"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := VerifyPrepared(context.Background(), roots, true)
	if got != (Prepared{}) || Code(err) != "GLOBAL_STAGING_MISMATCH" {
		t.Fatalf("source change advanced staging: got=%+v err=%v", got, err)
	}
	raw, err := os.ReadFile(filepath.Join(roots.CanonicalTarget, ".migration", "journal.json"))
	if err != nil {
		t.Fatal(err)
	}
	var j journal
	if err := json.Unmarshal(raw, &j); err != nil || j.Phase != "prepared" {
		t.Fatalf("source change advanced journal: %s %v", raw, err)
	}
}
