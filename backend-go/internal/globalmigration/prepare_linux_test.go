//go:build linux && amd64

package globalmigration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/globaladmission"
)

type oracleFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int    `json:"size"`
}

func checkFiles(t *testing.T, root string, files []oracleFile, exactInventory bool) {
	t.Helper()
	if exactInventory {
		actual := make([]string, 0, len(files))
		err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if path == root || entry.IsDir() {
				return nil
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			actual = append(actual, filepath.ToSlash(rel))
			return nil
		})
		if err != nil && !(os.IsNotExist(err) && len(files) == 0) {
			t.Fatal(err)
		}
		expected := make([]string, 0, len(files))
		for _, entry := range files {
			expected = append(expected, entry.Path)
		}
		sort.Strings(actual)
		sort.Strings(expected)
		if len(actual) != len(expected) {
			t.Fatalf("seed inventory changed: actual=%v expected=%v", actual, expected)
		}
		for i := range actual {
			if actual[i] != expected[i] {
				t.Fatalf("seed inventory changed: actual=%v expected=%v", actual, expected)
			}
		}
	}
	for _, entry := range files {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(entry.Path)))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		if len(data) != entry.Size || hex.EncodeToString(sum[:]) != entry.SHA256 {
			t.Fatalf("seed file changed: %s", entry.Path)
		}
	}
}

func treeInventory(t *testing.T, root string) []string {
	t.Helper()
	var found []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		found = append(found, entry.Type().String()+":"+filepath.ToSlash(rel))
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	sort.Strings(found)
	return found
}

func TestPrepareRejectsSwappedCanonicalBeforeFirstWrite(t *testing.T) {
	parent := t.TempDir()
	if err := os.Chmod(parent, 0700); err != nil {
		t.Fatal(err)
	}
	canonical := filepath.Join(parent, "canonical")
	userData := filepath.Join(parent, "user-data")
	outside := t.TempDir()
	for _, path := range []string{canonical, userData} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	roots := globaladmission.Roots{LegacySource: filepath.Join(parent, "legacy"), CanonicalTarget: canonical, UserData: userData}
	moved := canonical + "-moved"
	var hookErr error
	got, err := prepareWithHook(context.Background(), roots, true, func() {
		if hookErr = os.Rename(canonical, moved); hookErr != nil {
			return
		}
		hookErr = os.Symlink(outside, canonical)
	})
	if hookErr != nil {
		t.Fatal(hookErr)
	}
	if Code(err) != "GLOBAL_ROOT_UNSAFE" || got != (Prepared{}) {
		t.Fatalf("swapped canonical root accepted: got=%+v err=%v", got, err)
	}
	for _, root := range []string{outside, moved} {
		if _, err := os.Lstat(filepath.Join(root, ".migration")); !os.IsNotExist(err) {
			t.Fatalf("swapped root was mutated: %s: %v", root, err)
		}
		if _, err := os.Lstat(filepath.Join(root, "generations")); !os.IsNotExist(err) {
			t.Fatalf("swapped root was mutated: %s: %v", root, err)
		}
	}
}

func TestPrepareRejectsSwappedAncestorBeforeFirstWrite(t *testing.T) {
	base := t.TempDir()
	caseRoot := filepath.Join(base, "case")
	parent := filepath.Join(caseRoot, "lane")
	for _, path := range []string{caseRoot, parent} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	canonical := filepath.Join(parent, "canonical")
	userData := filepath.Join(parent, "user-data")
	for _, path := range []string{canonical, userData} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	roots := globaladmission.Roots{LegacySource: filepath.Join(parent, "legacy"), CanonicalTarget: canonical, UserData: userData}
	moved := caseRoot + "-moved"
	var hookErr error
	got, err := prepareWithHook(context.Background(), roots, true, func() {
		if hookErr = os.Rename(caseRoot, moved); hookErr != nil {
			return
		}
		hookErr = os.Symlink(moved, caseRoot)
	})
	if hookErr != nil {
		t.Fatal(hookErr)
	}
	if Code(err) != "GLOBAL_ROOT_UNSAFE" || got != (Prepared{}) {
		t.Fatalf("swapped ancestor accepted: got=%+v err=%v", got, err)
	}
	movedCanonical := filepath.Join(moved, "lane", "canonical")
	for _, name := range []string{".migration", "generations"} {
		if _, err := os.Lstat(filepath.Join(movedCanonical, name)); !os.IsNotExist(err) {
			t.Fatalf("moved root was mutated: %s: %v", name, err)
		}
	}
}

func TestPreparedJournalMatchesOriginalTypeScriptCheckpoint(t *testing.T) {
	path := os.Getenv("GO_GLOBAL_PREPARE_CONTRACT")
	if path == "" {
		if os.Getenv("GITHUB_ACTIONS") == "true" {
			t.Fatal("original TypeScript prepare oracle required")
		}
		t.Skip("GitHub original TypeScript prepare oracle required")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var oracle struct {
		FormatVersion int `json:"formatVersion"`
		Cases         []struct {
			Name   string                `json:"name"`
			Roots  globaladmission.Roots `json:"goRoots"`
			Oracle struct {
				Code       string   `json:"code"`
				JournalRaw *string  `json:"journalRaw"`
				Journal    *journal `json:"journal"`
				Restarted  *struct {
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
	if err := json.Unmarshal(data, &oracle); err != nil {
		t.Fatal(err)
	}
	if oracle.FormatVersion != 1 || len(oracle.Cases) != 5 {
		t.Fatalf("unexpected original oracle: %+v", oracle)
	}
	for _, test := range oracle.Cases {
		t.Run(test.Name, func(t *testing.T) {
			legacyBefore := treeInventory(t, test.Roots.LegacySource)
			userDataBefore := treeInventory(t, test.Roots.UserData)
			checkFiles(t, test.Roots.LegacySource, test.SeedFiles.Legacy, true)
			checkFiles(t, test.Roots.CanonicalTarget, test.SeedFiles.Canonical, false)
			checkFiles(t, test.Roots.UserData, test.SeedFiles.UserData, true)
			got, err := Prepare(context.Background(), test.Roots, true)
			if test.Oracle.Journal == nil {
				if Code(err) != test.Oracle.Code || got != (Prepared{}) {
					t.Fatalf("Go refusal differs from original: got=%+v err=%v code=%s", got, err, test.Oracle.Code)
				}
				if _, err := os.Lstat(filepath.Join(test.Roots.CanonicalTarget, ".migration", "journal.json")); !os.IsNotExist(err) {
					t.Fatalf("refused source wrote journal: %v", err)
				}
			} else {
				if err != nil || got.Phase != "prepared" || got.Source != test.Oracle.Journal.Source || len(got.Generation) != 36 {
					t.Fatalf("Go prepared journal differs from original: got=%+v err=%v", got, err)
				}
				journalPath := filepath.Join(test.Roots.CanonicalTarget, ".migration", "journal.json")
				bytes, err := os.ReadFile(journalPath)
				if err != nil {
					t.Fatal(err)
				}
				var actual journal
				if err := json.Unmarshal(bytes, &actual); err != nil || actual.Version != 1 || actual.Generation != got.Generation || actual.Source != got.Source || actual.Phase != "prepared" {
					t.Fatalf("Go journal invalid: %+v %v", actual, err)
				}
				if test.Oracle.Restarted != nil {
					if got.Generation != test.Oracle.Restarted.GlobalGeneration || test.Oracle.JournalRaw == nil || string(bytes) != *test.Oracle.JournalRaw {
						t.Fatal("restart did not preserve original PREPARED journal identity")
					}
				}
				if _, err := os.Lstat(filepath.Join(test.Roots.CanonicalTarget, ".migration", "receipt.json")); !os.IsNotExist(err) {
					t.Fatal("PREPARED was exposed as completed receipt")
				}
			}
			checkFiles(t, test.Roots.LegacySource, test.SeedFiles.Legacy, true)
			checkFiles(t, test.Roots.UserData, test.SeedFiles.UserData, true)
			checkFiles(t, test.Roots.CanonicalTarget, test.SeedFiles.Canonical, false)
			if after := treeInventory(t, test.Roots.LegacySource); !reflect.DeepEqual(after, legacyBefore) {
				t.Fatalf("legacy directory inventory changed: before=%v after=%v", legacyBefore, after)
			}
			if after := treeInventory(t, test.Roots.UserData); !reflect.DeepEqual(after, userDataBefore) {
				t.Fatalf("user-data directory inventory changed: before=%v after=%v", userDataBefore, after)
			}
		})
	}
}

func TestPrepareRejectsNonDisposableRoots(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	canonical := filepath.Join(root, "canonical")
	userData := filepath.Join(root, "user-data")
	for _, path := range []string{canonical, userData} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	roots := globaladmission.Roots{LegacySource: filepath.Join(root, "legacy"), CanonicalTarget: canonical, UserData: userData}
	if _, err := Prepare(context.Background(), roots, false); Code(err) != "GLOBAL_EXCLUSIVE_ACCESS_REQUIRED" {
		t.Fatalf("absent exclusion accepted: %v", err)
	}
	outside := globaladmission.Roots{LegacySource: "/var/lib/legacy", CanonicalTarget: "/var/lib/canonical", UserData: "/var/lib/user-data"}
	if _, err := Prepare(context.Background(), outside, true); Code(err) != "GLOBAL_ROOT_UNSAFE" {
		t.Fatalf("non-disposable root accepted: %v", err)
	}
}

// A crash before rename can leave a temp journal. This partial phase makes no
// recovery promise for that state; a later call must fail closed.
func TestPrepareRejectsLoneJournalTemp(t *testing.T) {
	parent := t.TempDir()
	if err := os.Chmod(parent, 0700); err != nil {
		t.Fatal(err)
	}
	canonical := filepath.Join(parent, "canonical")
	userData := filepath.Join(parent, "user-data")
	for _, path := range []string{canonical, userData} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	control := filepath.Join(canonical, ".migration")
	if err := os.Mkdir(control, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(control, "journal.json.interrupted.tmp"), []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	roots := globaladmission.Roots{LegacySource: filepath.Join(parent, "legacy"), CanonicalTarget: canonical, UserData: userData}
	got, err := Prepare(context.Background(), roots, true)
	if Code(err) != "GLOBAL_PARTIAL_TARGET_UNKNOWN" || got != (Prepared{}) {
		t.Fatalf("lone journal temp accepted: got=%+v err=%v", got, err)
	}
	if _, err := os.Lstat(filepath.Join(control, "journal.json")); !os.IsNotExist(err) {
		t.Fatalf("partial state gained a journal: %v", err)
	}
}
