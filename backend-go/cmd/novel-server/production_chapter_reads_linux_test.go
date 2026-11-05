//go:build linux

package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
)

// The prompt and chapter families are the two smallest remaining families in
// web/operations.json: `chapter` has four names and `prompt` three. This test
// pins the exact split the lane ships - one prompt read bound, six names refused
// - one subtest per operation, so a CI failure names the operation that moved.
//
// Everything runs inside the disposable root productionDurableDataRoot builds:
// the author database, the committed global generation and the prompt files all
// live in the test's own t.TempDir(). No real author data root is read or
// written, and no request in this test ever carries a path of its own.
const (
	productionPromptTemplateKey  = "production-prompt"
	productionPromptTemplateBody = "生产提示词：门后有灯。"
	productionPromptEnglishKey   = "production-english-prompt"
	productionPromptEnglishBody  = "Production English prompt."
)

// seedProductionPromptFiles writes the prompt objects where the shipped app
// keeps them - inside the committed global generation - so prompt:load-global
// has to parse real files under the pinned root instead of returning a constant
// shape. Each name carries the key and the writing language the loader
// cross-checks, and each file is private (0600), exactly like the author's own.
func seedProductionPromptFiles(t *testing.T, root string) string {
	t.Helper()
	directory := filepath.Join(root, productionConfigDirectory, productionConfigTarget,
		"generations", productionTestGeneration, "prompts")
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	files := []struct {
		name string
		body string
	}{
		{productionPromptTemplateKey + ".zh-CN.json",
			`{"key":"` + productionPromptTemplateKey + `","writingLanguage":"zh-CN","content":"` +
				productionPromptTemplateBody + `"}`},
		{productionPromptEnglishKey + ".en-US.json",
			`{"key":"` + productionPromptEnglishKey + `","writingLanguage":"en-US","content":"` +
				productionPromptEnglishBody + `"}`},
	}
	for _, file := range files {
		if err := os.WriteFile(filepath.Join(directory, file.name), []byte(file.body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return directory
}

// productionPromptSnapshot reads every prompt object so a refusal can be proven
// to have added, removed and rewritten none of them.
func productionPromptSnapshot(t *testing.T, directory string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	files := make(map[string]string, len(entries))
	for _, entry := range entries {
		body, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		files[entry.Name()] = string(body)
	}
	return files
}

// productionPromptCompare asserts the global prompt directory still holds exactly
// the files it held before the operation under test ran.
func productionPromptCompare(t *testing.T, directory string, before map[string]string, label string) {
	t.Helper()
	after := productionPromptSnapshot(t, directory)
	if len(after) != len(before) {
		t.Fatalf("%s changed the global prompt directory: %d files, want %d", label, len(after), len(before))
	}
	for name, body := range before {
		other, present := after[name]
		if !present {
			t.Fatalf("%s removed the prompt object %q", label, name)
		}
		if body != other {
			t.Fatalf("%s rewrote the prompt object %q", label, name)
		}
	}
}

// productionFamilyNotMigrated asserts one name this lane deliberately leaves
// unbound. The registry answers 501 before any handler, lease or database is
// reached, so neither a live nor a stale project lease can turn it into a 200.
func productionFamilyNotMigrated(t *testing.T, client *productionDurableClient, operation string, args []any) {
	t.Helper()
	response := client.call(t, operation, args)
	if response.Code != http.StatusNotImplemented {
		t.Fatalf("%s answered %d, but this lane binds no handler for it: %s",
			operation, response.Code, response.Body.String())
	}
	var wire struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &wire); err != nil {
		t.Fatalf("%s: %v %s", operation, err, response.Body.String())
	}
	if wire.Error.Code != "GO_OPERATION_NOT_MIGRATED" {
		t.Fatalf("%s failed closed with %q instead of GO_OPERATION_NOT_MIGRATED: %s",
			operation, wire.Error.Code, response.Body.String())
	}
}

// TestProductionPromptAndChapterFamilies proves the split this lane ships:
// prompt:load-global answers 200 from prompt objects the committed generation
// really holds, while the four chapter names stay unbound because each one
// deletes facts or consumes an authorization this lane cannot mint. The two
// prompt writes are bound by the write batch, so they are not refused here.
// Each refusal is asserted under a live lease and under a stale one, and each
// one must leave both the author database and the global prompt directory
// byte-identical.
func TestProductionPromptAndChapterFamilies(t *testing.T) {
	root, projectPath := productionDurableDataRoot(t)
	promptDirectory := seedProductionPromptFiles(t, root)
	handler, cleanup, err := productionHandlerWithContract(migrationRegistry(t), productionDurableAddress, "", root,
		productionConfig{Schema: productionDurableContract(t), MaxDatabaseBytes: productionMaxDatabaseBytes})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	author := newProductionDurableClient(t, handler, productionDurableTab)
	var opened productionDurableOpenWire
	author.decode(t, "project:open", []any{projectPath, "family-open-1", projectPath}, &opened)
	if !opened.Success || !opened.DBReady || opened.Error != "" {
		t.Fatalf("project:open refused an admitted project: %+v", opened)
	}
	frozen := map[string]any{
		"projectId":   opened.Project["id"],
		"projectPath": opened.Project["path"],
		"leaseId":     opened.Project["sessionLease"],
	}
	if frozen["leaseId"] == "" || frozen["leaseId"] == nil {
		t.Fatalf("project:open returned no session lease: %v", opened.Project)
	}
	stale := map[string]any{"projectId": frozen["projectId"], "projectPath": projectPath, "leaseId": "stale-lease"}

	// The baseline is taken after open and after the prompts are seeded, so
	// nothing this test asserts can be blamed on opening the project.
	database := filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName)
	seededDatabase, err := os.ReadFile(database)
	if err != nil {
		t.Fatal(err)
	}
	promptsBefore := productionPromptSnapshot(t, promptDirectory)
	unchanged := func(t *testing.T, label string) {
		t.Helper()
		after, err := os.ReadFile(database)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(after, seededDatabase) {
			t.Fatalf("%s changed the admitted author database", label)
		}
		productionPromptCompare(t, promptDirectory, promptsBefore, label)
	}
	// The table the chapter family reads and writes does not exist in the
	// admitted author database of this lane: chapter:list-incomplete-deletions
	// could only answer an invented empty list, and the three deletions have no
	// ledger to begin. If this ever becomes false, the family has a schema and
	// the refusal below has to be re-decided instead of silently kept.
	if bytes.Contains(seededDatabase, []byte("chapter_deletion_operations")) {
		t.Fatal("the admitted author database carries chapter_deletion_operations; rebind or re-refuse the chapter family")
	}

	// prompt:load-global: the one bound name. It reads the two seeded prompt
	// objects out of the pinned root and reports no diagnostics.
	t.Run("prompt:load-global", func(t *testing.T) {
		response := author.call(t, "prompt:load-global", []any{})
		if response.Code != http.StatusOK {
			t.Fatalf("prompt:load-global: HTTP %d %s", response.Code, response.Body.String())
		}
		var wire struct {
			Result struct {
				Templates   []map[string]any `json:"templates"`
				Diagnostics []any            `json:"diagnostics"`
			} `json:"result"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &wire); err != nil {
			t.Fatalf("prompt:load-global: %v %s", err, response.Body.String())
		}
		contents := make(map[string]string, len(wire.Result.Templates))
		for _, template := range wire.Result.Templates {
			key, _ := template["key"].(string)
			content, _ := template["content"].(string)
			contents[key] = content
		}
		if len(contents) != 2 ||
			contents[productionPromptTemplateKey] != productionPromptTemplateBody ||
			contents[productionPromptEnglishKey] != productionPromptEnglishBody {
			t.Fatalf("prompt:load-global did not read the seeded prompt objects: %s", response.Body.String())
		}
		if len(wire.Result.Diagnostics) != 0 {
			t.Fatalf("prompt:load-global reported diagnostics for valid prompt objects: %s", response.Body.String())
		}
		// Host paths are not browser capabilities: the pinned root must not travel.
		if body := response.Body.String(); strings.Contains(body, promptDirectory) || strings.Contains(body, root) {
			t.Fatalf("prompt:load-global leaked the private prompt root: %s", body)
		}
		// This name takes ZERO arguments, so it carries no lease. A project
		// session context - live or stale - in the argument position is a
		// malformed request, never authority.
		for _, candidate := range []struct {
			label   string
			session map[string]any
		}{
			{"live", frozen},
			{"stale", stale},
		} {
			refused := author.call(t, "prompt:load-global", []any{candidate.session})
			if refused.Code != http.StatusBadRequest {
				t.Fatalf("prompt:load-global accepted a %s project context: HTTP %d %s",
					candidate.label, refused.Code, refused.Body.String())
			}
			if body := refused.Body.String(); strings.Contains(body, productionPromptTemplateBody) {
				t.Fatalf("prompt:load-global returned templates despite a %s context: %s", candidate.label, body)
			}
		}
		unchanged(t, "prompt:load-global")
	})

	// chapter:delete-finalized: refused. The shipped path INSERTs the deletion
	// operation row and then deletes the finalized facts.
	t.Run("chapter:delete-finalized", func(t *testing.T) {
		request := map[string]any{"draftId": float64(1), "chapterNumber": float64(1)}
		productionFamilyNotMigrated(t, author, "chapter:delete-finalized", []any{request, projectPath, frozen})
		productionFamilyNotMigrated(t, author, "chapter:delete-finalized", []any{request, projectPath, stale})
		unchanged(t, "chapter:delete-finalized")
	})
	// chapter:retry-deletion: refused. The shipped path re-pends the operation
	// row and deletes the manuscript and knowledge projections again.
	t.Run("chapter:retry-deletion", func(t *testing.T) {
		productionFamilyNotMigrated(t, author, "chapter:retry-deletion", []any{"ci-operation", projectPath, frozen})
		productionFamilyNotMigrated(t, author, "chapter:retry-deletion", []any{"ci-operation", projectPath, stale})
		unchanged(t, "chapter:retry-deletion")
	})
	// chapter:confirm-legacy-knowledge-absent: refused. The shipped path consumes
	// the legacy authorization and then deletes the finalized facts on the
	// strength of that write.
	t.Run("chapter:confirm-legacy-knowledge-absent", func(t *testing.T) {
		productionFamilyNotMigrated(t, author, "chapter:confirm-legacy-knowledge-absent", []any{"ci-operation", projectPath, frozen})
		productionFamilyNotMigrated(t, author, "chapter:confirm-legacy-knowledge-absent", []any{"ci-operation", projectPath, stale})
		unchanged(t, "chapter:confirm-legacy-knowledge-absent")
	})
	// chapter:list-incomplete-deletions: refused. Its only SELECT reads a table
	// this lane's schema does not carry and is filtered by a portable-runtime
	// freeze this backend does not implement, so an answer could only be a
	// fabricated empty list.
	t.Run("chapter:list-incomplete-deletions", func(t *testing.T) {
		productionFamilyNotMigrated(t, author, "chapter:list-incomplete-deletions", []any{projectPath, frozen})
		productionFamilyNotMigrated(t, author, "chapter:list-incomplete-deletions", []any{projectPath, stale})
		unchanged(t, "chapter:list-incomplete-deletions")
	})
}
