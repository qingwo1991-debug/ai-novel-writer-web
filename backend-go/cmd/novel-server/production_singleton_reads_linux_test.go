//go:build linux

package main

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3" // Registers the database/sql driver "sqlite3".
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/admission"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
)

// The fixture is one disposable t.TempDir() (syntheticProductionDataDir): a
// committed global generation with one installed skill, one admitted author
// project whose real baseline rows carry a review, a revision and two recovery
// candidates, plus a real tree OUTSIDE the project that only a confinement
// escape could reach. No real author data root is read or written.
const (
	productionSingletonProjectName  = "singleton-reads-novel"
	productionSingletonProjectID    = "5c8e2a71-3f4d-4b96-a0d2-7e6f1b9c8d34"
	productionSingletonDraftBody    = "第一章原稿：他推开门。"
	productionSingletonReviewBody   = "审读正文：门后有人。"
	productionSingletonOtherReview  = "第二章审读：窗外有风。"
	productionSingletonRevisionBody = "改稿正文：她推开门。"
	productionSingletonCandidateID  = "ci-singleton-candidate-1"
	productionSingletonResolvedID   = "ci-singleton-candidate-2"
	productionSingletonVisibleText  = "候选正文：门后有灯。"
	productionSingletonChapterTitle = "第一章 门"
	productionSingletonSkillName    = "ci-singleton-skill"
	productionSingletonSkillBody    = "# 技能\n读取参考资料。\n"
	productionSingletonOutsideBody  = "sealed singleton bytes that no listing may serve"

	// productionSingletonSource is the exact source snapshot the store compares
	// a candidate against (internal/store/recovery.go:148-157 through
	// internal/store/plottree.go:34-49): chapterNumber first, then title, role,
	// purpose, keyEvents, the characters array, suspenseHook and userGuidance;
	// plotJSON keeps that quoted key order and plotQuote leaves these runes
	// unescaped (plottree.go:33). It is the blueprint row seeded for chapter 1,
	// so the store's own integrity comparison - not this test - decides that the
	// candidate is source-current.
	productionSingletonSource = `{"chapterNumber":1,"title":"` + productionSingletonChapterTitle +
		`","role":"","purpose":"","keyEvents":"","characters":[],"suspenseHook":"","userGuidance":""}`
)

// productionSingletonSchema is the smallest real schema these five reads project:
// the project core row the lane's open path expects, the draft table
// store.recoveryCurrent joins through, the blueprint table it re-reads the
// current source from (internal/store/blueprints.go:44-46, :56-57), the two
// review/revision tables the repository SELECTs, and the recovery candidate
// table.
var productionSingletonSchema = []string{
	"CREATE TABLE blueprints (chapter_number INTEGER PRIMARY KEY, title TEXT, role TEXT, purpose TEXT, key_events TEXT, characters TEXT, suspense_hook TEXT, user_guidance TEXT, notes TEXT, notes_updated_at TEXT)",
	"CREATE TABLE contents (id INTEGER PRIMARY KEY, body TEXT)",
	"CREATE TABLE drafts (id INTEGER PRIMARY KEY, chapter_number INTEGER, version INTEGER, status TEXT, source TEXT, content_id INTEGER, word_count INTEGER, source_dependencies TEXT, created_at TEXT, updated_at TEXT)",
	"CREATE TABLE project_core (id TEXT PRIMARY KEY, project_name TEXT, writing_language TEXT, genre TEXT, sub_genre TEXT, target_audience TEXT, total_chapters INTEGER, words_per_chapter INTEGER, creative_strategy TEXT, narrative_thread_dormant_threshold INTEGER, plot_structure TEXT, narrative_pov TEXT, core_outline TEXT, world_setting TEXT, golden_finger TEXT, protagonist_profile TEXT, global_guidance TEXT, writing_style TEXT, reference_works TEXT, character_states TEXT, created_at TEXT, updated_at TEXT)",
	"CREATE TABLE recovery_candidates (candidate_id TEXT PRIMARY KEY, run_id TEXT, step_id TEXT, project_id TEXT, chapter_number INTEGER, chapter_title TEXT, source_snapshot TEXT, source_hash TEXT, source_draft_id INTEGER, source_draft_version INTEGER, source_draft_identity_captured INTEGER, visible_text TEXT, content_hash TEXT, failure_code TEXT, failure_reason TEXT, replaces_candidate_id TEXT, status TEXT, resolved_at TEXT, created_at TEXT)",
	"CREATE TABLE reviews (id INTEGER PRIMARY KEY, base_draft_id INTEGER, review_index INTEGER, content_id INTEGER, created_at TEXT, source_draft_chapter_number INTEGER, source_draft_version INTEGER, source_draft_status TEXT, source_content TEXT)",
	"CREATE TABLE revisions (id INTEGER PRIMARY KEY, base_draft_id INTEGER, revision_index INTEGER, revision_type TEXT, status TEXT, merged_to_draft_id INTEGER, user_prompt TEXT, review_source_id INTEGER, content_id INTEGER, word_count INTEGER, created_at TEXT, updated_at TEXT, source_draft_chapter_number INTEGER, source_draft_version INTEGER, source_draft_status TEXT, source_content TEXT)",
}

// productionSingletonContract builds the accepted schema fingerprint
// independently of the verifier's sqlite_master reader, in the same
// (type, name) order productionDurableContract does: the verifier hashes that
// projection, so a contract built from these exact DDL strings is the oracle.
func productionSingletonContract(t *testing.T) admission.SchemaContract {
	t.Helper()
	const prefix = "CREATE TABLE "
	entries := make([]productionSchemaEntry, 0, len(productionSingletonSchema))
	for _, statement := range productionSingletonSchema {
		name, _, found := strings.Cut(strings.TrimPrefix(statement, prefix), " ")
		if !found || name == "" || !strings.HasPrefix(statement, prefix) {
			t.Fatalf("unparseable schema statement: %s", statement)
		}
		entries = append(entries, productionSchemaEntry{Kind: "table", Name: name, Table: name, SQL: statement})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	encoded, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(encoded)
	return admission.SchemaContract{{Version: admission.CurrentSchemaVersion, Fingerprints: []string{hex.EncodeToString(digest[:])}}}
}

// productionSingletonDigest is the recovery candidate integrity hash the store
// verifies (internal/store/recovery.go:89-92): sha256 hex of the column text.
func productionSingletonDigest(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// productionSingletonDataRoot seeds the disposable data root and the author rows.
func productionSingletonDataRoot(t *testing.T) (root, projectPath string) {
	t.Helper()
	root = syntheticProductionDataDir(t)
	library := filepath.Join(root, productionLibraryDirectory)
	seedProductionProject(t, library, productionSingletonProjectName, productionSingletonProjectID,
		admission.CurrentSchemaVersion, productionSingletonSchema)
	projectPath = filepath.Join(library, productionSingletonProjectName)
	productionSingletonSeedRows(t, projectPath)
	return root, projectPath
}

// productionSingletonSeedRows writes the rows the five reads have to project. The
// second review (and its second draft) exists so "latest"/"next index" can only be
// right if the scalar id argument really selects a base draft: the id-1 answer
// differs from the id-2 answer for every one of them.
func productionSingletonSeedRows(t *testing.T, projectPath string) {
	t.Helper()
	database := filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName)
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(database)}
	uri.RawQuery = "mode=rw"
	db, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := db.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
	}()
	db.SetMaxOpenConns(1)
	snapshot := productionSingletonSource
	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO blueprints (chapter_number, title, characters, notes) VALUES (1, ?, '[]', '')`,
			[]any{productionSingletonChapterTitle}},
		{`INSERT INTO blueprints (chapter_number, title, characters, notes) VALUES (2, ?, '[]', '')`,
			[]any{"第二章 风"}},
		{`INSERT INTO project_core (id, project_name, writing_language, genre, sub_genre,
 target_audience, total_chapters, words_per_chapter, creative_strategy,
 narrative_thread_dormant_threshold, plot_structure, narrative_pov, core_outline,
 world_setting, golden_finger, protagonist_profile, global_guidance, writing_style,
 reference_works, character_states, created_at, updated_at)
 VALUES ('main', 'Singleton Novel', 'zh-CN', 'fantasy', '', '', 12, 3000, 'auto', 3,
 '', '', '', '', '', '', '', '', '', '{}', '2024-05-06 07:08:09', '2024-05-06 07:08:09')`, nil},
		{`INSERT INTO contents (id, body) VALUES (1, ?)`, []any{productionSingletonDraftBody}},
		{`INSERT INTO contents (id, body) VALUES (2, ?)`, []any{productionSingletonReviewBody}},
		{`INSERT INTO contents (id, body) VALUES (3, ?)`, []any{productionSingletonOtherReview}},
		{`INSERT INTO contents (id, body) VALUES (4, ?)`, []any{productionSingletonRevisionBody}},
		{`INSERT INTO drafts (id, chapter_number, version, status, source, content_id,
 word_count, source_dependencies, created_at, updated_at)
 VALUES (1, 1, 1, 'draft', 'write', 1, 11, '[]', '2024-05-06 07:08:09', '2024-05-06 07:08:09')`, nil},
		{`INSERT INTO drafts (id, chapter_number, version, status, source, content_id,
 word_count, source_dependencies, created_at, updated_at)
 VALUES (2, 2, 1, 'draft', 'write', 3, 11, '[]', '2024-05-06 07:08:09', '2024-05-06 07:08:09')`, nil},
		{`INSERT INTO reviews (id, base_draft_id, review_index, content_id, created_at,
 source_draft_chapter_number, source_draft_version, source_draft_status, source_content)
 VALUES (1, 1, 1, 2, '2024-05-06 07:08:09', 1, 1, 'draft', ?)`, []any{productionSingletonDraftBody}},
		{`INSERT INTO reviews (id, base_draft_id, review_index, content_id, created_at,
 source_draft_chapter_number, source_draft_version, source_draft_status, source_content)
 VALUES (2, 2, 5, 3, '2024-05-06 07:08:10', 2, 1, 'draft', ?)`, []any{productionSingletonOtherReview}},
		{`INSERT INTO revisions (id, base_draft_id, revision_index, revision_type, status,
 merged_to_draft_id, user_prompt, review_source_id, content_id, word_count, created_at,
 updated_at, source_draft_chapter_number, source_draft_version, source_draft_status, source_content)
 VALUES (1, 1, 1, 'refine', 'pending', NULL, '润色', 1, 4, 11, '2024-05-06 07:08:09',
 '2024-05-06 07:08:09', 1, 1, 'draft', ?)`, []any{productionSingletonDraftBody}},
		{`INSERT INTO recovery_candidates (candidate_id, run_id, step_id, project_id,
 chapter_number, chapter_title, source_snapshot, source_hash, source_draft_id,
 source_draft_version, source_draft_identity_captured, visible_text, content_hash,
 failure_code, failure_reason, replaces_candidate_id, status, resolved_at, created_at)
 VALUES (?, 'ci-run-1', 'ci-step-1', ?, 1, ?, ?, ?, 1, 1, 1, ?, ?, 'GENERATION_FAILED',
 '合成失败', NULL, 'pending', NULL, '2024-05-06 07:08:09')`,
			[]any{productionSingletonCandidateID, productionSingletonProjectID, productionSingletonChapterTitle,
				snapshot, productionSingletonDigest(snapshot), productionSingletonVisibleText,
				productionSingletonDigest(productionSingletonVisibleText)}},
		{`INSERT INTO recovery_candidates (candidate_id, run_id, step_id, project_id,
 chapter_number, chapter_title, source_snapshot, source_hash, source_draft_id,
 source_draft_version, source_draft_identity_captured, visible_text, content_hash,
 failure_code, failure_reason, replaces_candidate_id, status, resolved_at, created_at)
 VALUES (?, 'ci-run-2', 'ci-step-2', ?, 1, ?, ?, ?, 1, 1, 1, ?, ?, 'GENERATION_FAILED',
 '合成失败', NULL, 'resolved', '2024-05-06 07:09:09', '2024-05-06 07:08:11')`,
			[]any{productionSingletonResolvedID, productionSingletonProjectID, productionSingletonChapterTitle,
				snapshot, productionSingletonDigest(snapshot), productionSingletonVisibleText,
				productionSingletonDigest(productionSingletonVisibleText)}},
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement.query, statement.args...); err != nil {
			t.Fatalf("seed %q: %v", statement.query, err)
		}
	}
}

// productionSingletonSeedSkill writes one installed skill where the shipped app
// keeps it - under the committed global generation, beside `prompts/` - private
// (0600 file, 0700 directories) exactly like the author's own.
func productionSingletonSeedSkill(t *testing.T, root string) string {
	t.Helper()
	directory := filepath.Join(root, productionConfigDirectory, productionConfigTarget,
		"generations", productionTestGeneration, "skills", productionSingletonSkillName)
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "SKILL.md"), []byte(productionSingletonSkillBody), 0600); err != nil {
		t.Fatal(err)
	}
	return directory
}

// productionSingletonRefused asserts one transport-envelope refusal: not 200, one
// of the symbolic codes the family's own mapper can produce, and no byte from
// outside the project.
func productionSingletonRefused(t *testing.T, client *productionDurableClient, operation, label string, args []any, want ...string) {
	t.Helper()
	response := client.call(t, operation, args)
	if response.Code == http.StatusOK {
		t.Fatalf("%s answered 200 for a %s: %s", operation, label, response.Body.String())
	}
	if strings.Contains(response.Body.String(), productionSingletonOutsideBody) {
		t.Fatalf("%s leaked bytes from outside the project: %s", operation, response.Body.String())
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("%s: %s: %v %s", operation, label, err, response.Body.String())
	}
	for _, code := range want {
		if body.Error.Code == code {
			return
		}
	}
	t.Fatalf("%s refused a %s with %q; want one of %v: %s", operation, label, body.Error.Code, want, response.Body.String())
}

// TestProductionSingletonReadsServeReadOnlyNames is the whole slice: one subtest
// per operation. The five bound names answer 200 with the rows and files the
// fixture really holds; fs:list-dir is refused at the registry and stays 501, so
// an absolute path, a ".." traversal and an escaping symlink never reach a walk;
// one stale lease is refused; and the author database is byte-identical after
// every read.
func TestProductionSingletonReadsServeReadOnlyNames(t *testing.T) {
	root, projectPath := productionSingletonDataRoot(t)
	skillDirectory := productionSingletonSeedSkill(t, root)
	// A real outside tree plus a planted directory link inside the project: the
	// escape fs:list-dir's absolute-path handler would have followed.
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "outside-secret.txt"), []byte(productionSingletonOutsideBody), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(projectPath, "escape-dir")); err != nil {
		t.Fatal(err)
	}

	handler, cleanup, err := productionHandlerWithContract(migrationRegistry(t), productionDurableAddress, "", root,
		productionConfig{Schema: productionSingletonContract(t), MaxDatabaseBytes: productionMaxDatabaseBytes})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	author := newProductionDurableClient(t, handler, productionDurableTab)
	var opened productionDurableOpenWire
	author.decode(t, "project:open", []any{projectPath, "singleton-open-1", projectPath}, &opened)
	if !opened.Success || !opened.DBReady || opened.Error != "" {
		t.Fatalf("project:open refused an admitted project: %+v", opened)
	}
	frozen := map[string]any{
		"projectId":   opened.Project["id"],
		"projectPath": opened.Project["path"],
		"leaseId":     opened.Project["sessionLease"],
	}
	if frozen["leaseId"] == nil || frozen["leaseId"] == "" {
		t.Fatalf("project:open returned no session lease: %v", opened.Project)
	}

	// The baselines are captured after the lease pinned the admitted bytes, so
	// every assertion below is measured against the same tree.
	database := filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName)
	seededDatabase, err := os.ReadFile(database)
	if err != nil {
		t.Fatal(err)
	}
	generationRoot := filepath.Join(root, productionConfigDirectory, productionConfigTarget,
		"generations", productionTestGeneration)
	before := productionFSSnapshot(t, projectPath)
	generationBefore := productionFSSnapshot(t, generationRoot)
	outsideBefore := productionFSSnapshot(t, outside)

	// db:review-get-latest: the newest review of THE NAMED base draft, with the
	// frozen source it was generated against.
	t.Run("db:review-get-latest", func(t *testing.T) {
		var latest map[string]any
		productionEditorDecode(t, author, productionSingletonReviewLatestName,
			[]any{int64(1), projectPath, frozen}, &latest)
		if latest["id"] != float64(1) || latest["reviewIndex"] != float64(1) || latest["baseDraftId"] != float64(1) {
			t.Fatalf("db:review-get-latest returned the wrong row: %+v", latest)
		}
		if latest["content"] != productionSingletonReviewBody {
			t.Fatalf("db:review-get-latest returned content %v; want the stored review body", latest["content"])
		}
		source, ok := latest["sourceDraft"].(map[string]any)
		if !ok {
			t.Fatalf("db:review-get-latest dropped the frozen source draft: %+v", latest)
		}
		if source["content"] != productionSingletonDraftBody ||
			source["chapterNumber"] != float64(1) || source["version"] != float64(1) ||
			source["status"] != "draft" {
			t.Fatalf("db:review-get-latest projected the wrong source draft: %+v", source)
		}
		// A base draft this project has no review for stays the original's
		// nullable null, not an error and not another draft's row.
		var absent any
		productionEditorDecode(t, author, productionSingletonReviewLatestName,
			[]any{int64(77), projectPath, frozen}, &absent)
		if absent != nil {
			t.Fatalf("db:review-get-latest invented a row for an unseeded draft: %v", absent)
		}
	})

	// db:review-next-index: MAX(review_index)+1 for the named base draft only.
	t.Run("db:review-next-index", func(t *testing.T) {
		var next int64
		productionEditorDecode(t, author, productionSingletonReviewNextName,
			[]any{int64(1), projectPath, frozen}, &next)
		if next != 2 {
			t.Fatalf("db:review-next-index = %d for base draft 1; want 2", next)
		}
		var other int64
		productionEditorDecode(t, author, productionSingletonReviewNextName,
			[]any{int64(2), projectPath, frozen}, &other)
		if other != 6 {
			t.Fatalf("db:review-next-index = %d for base draft 2; want 6", other)
		}
	})

	// db:revision-next-index: MAX(revision_index)+1 for the named base draft.
	t.Run("db:revision-next-index", func(t *testing.T) {
		var next int64
		productionEditorDecode(t, author, productionSingletonRevisionNextName,
			[]any{int64(1), projectPath, frozen}, &next)
		if next != 2 {
			t.Fatalf("db:revision-next-index = %d for base draft 1; want 2", next)
		}
		var absent int64
		productionEditorDecode(t, author, productionSingletonRevisionNextName,
			[]any{int64(77), projectPath, frozen}, &absent)
		if absent != 1 {
			t.Fatalf("db:revision-next-index = %d for a draft with no revision; want the original's 1", absent)
		}
	})

	// db:recovery-candidate-list: the PENDING candidate the database holds, with
	// its integrity hashes verified by the store and its source still current.
	t.Run("db:recovery-candidate-list", func(t *testing.T) {
		var candidates []map[string]any
		productionEditorDecode(t, author, productionSingletonRecoveryListName,
			[]any{projectPath, frozen}, &candidates)
		if len(candidates) != 1 {
			t.Fatalf("db:recovery-candidate-list returned %d candidates; want only the pending one: %+v",
				len(candidates), candidates)
		}
		candidate := candidates[0]
		if candidate["candidateId"] != productionSingletonCandidateID ||
			candidate["chapterTitle"] != productionSingletonChapterTitle ||
			candidate["visibleText"] != productionSingletonVisibleText ||
			candidate["failureCode"] != "GENERATION_FAILED" ||
			candidate["status"] != "pending" {
			t.Fatalf("db:recovery-candidate-list projected the wrong candidate: %+v", candidate)
		}
		// sourceCurrent is the store's own answer: it re-reads the chapter's
		// current blueprint, re-serializes it (internal/store/recovery.go:158-190)
		// and compares that hash and the captured draft identity with the row.
		if candidate["sourceCurrent"] != true {
			t.Fatalf("db:recovery-candidate-list did not re-prove the source: %+v", candidate)
		}
		if candidate["sourceHash"] != productionSingletonDigest(productionSingletonSource) {
			t.Fatalf("db:recovery-candidate-list returned a rewritten source hash: %+v", candidate)
		}
		for _, row := range candidates {
			if row["candidateId"] == productionSingletonResolvedID {
				t.Fatalf("db:recovery-candidate-list listed the resolved candidate: %+v", row)
			}
		}
	})

	// skills:list-user: the installed skill from the committed generation, under
	// the managed:// display identifiers, never the private root.
	t.Run("skills:list-user", func(t *testing.T) {
		response := author.call(t, productionSingletonSkillsListName, []any{})
		if response.Code != http.StatusOK {
			t.Fatalf("skills:list-user: HTTP %d %s", response.Code, response.Body.String())
		}
		var wire struct {
			Result []map[string]any `json:"result"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &wire); err != nil {
			t.Fatalf("skills:list-user: %v %s", err, response.Body.String())
		}
		if len(wire.Result) != 1 {
			t.Fatalf("skills:list-user returned %d skills; want the one installed skill: %s",
				len(wire.Result), response.Body.String())
		}
		skill := wire.Result[0]
		base := "managed://skills/" + productionSingletonSkillName
		if skill["name"] != productionSingletonSkillName ||
			skill["content"] != productionSingletonSkillBody ||
			skill["baseDir"] != base ||
			skill["filePath"] != base+"/SKILL.md" {
			t.Fatalf("skills:list-user projected the wrong skill: %+v", skill)
		}
		bundle, ok := skill["bundle"].(map[string]any)
		if !ok || bundle["status"] != "none" {
			t.Fatalf("skills:list-user projected the wrong bundle state: %+v", skill["bundle"])
		}
		// Host paths are not browser capabilities.
		body := response.Body.String()
		if strings.Contains(body, root) || strings.Contains(body, skillDirectory) {
			t.Fatalf("skills:list-user leaked the private skill root: %s", body)
		}
	})

	// fs:list-dir stays refused: the name is unbound, so every shape the family
	// handler would have accepted - an absolute host path, a ".." traversal and a
	// path through a planted escaping symlink - is refused at the registry, before
	// any directory is opened, and nothing is written.
	t.Run("fs:list-dir", func(t *testing.T) {
		attempts := []struct {
			label string
			path  string
		}{
			{"absolute path", filepath.Join(outside, "outside-secret.txt")},
			{"traversal", "../outside-secret.txt"},
			{"escaping symlink", filepath.Join("escape-dir", "outside-secret.txt")},
			{"absolute project path", projectPath},
		}
		for _, attempt := range attempts {
			response := author.call(t, "fs:list-dir", []any{attempt.path, projectPath, frozen})
			if response.Code != http.StatusNotImplemented ||
				!strings.Contains(response.Body.String(), "GO_OPERATION_NOT_MIGRATED") {
				t.Fatalf("fs:list-dir accepted a %s: %d %s", attempt.label, response.Code, response.Body.String())
			}
			body := response.Body.String()
			if strings.Contains(body, productionSingletonOutsideBody) ||
				strings.Contains(body, projectPath) || strings.Contains(body, productionSingletonSkillBody) {
				t.Fatalf("fs:list-dir returned tree content for a %s: %s", attempt.label, body)
			}
		}
		if after := productionFSSnapshot(t, projectPath); !equalSingletonSnapshots(before, after) {
			t.Fatal("an fs:list-dir refusal changed the admitted project tree")
		}
		if after := productionFSSnapshot(t, outside); !equalSingletonSnapshots(outsideBefore, after) {
			t.Fatal("an fs:list-dir refusal wrote into the tree outside the project")
		}
	})

	// One stale-lease refusal: a lease this server never issued reads nothing,
	// and the refusal is the fence's own code rather than a row.
	t.Run("stale-lease", func(t *testing.T) {
		stale := map[string]any{
			"projectId": frozen["projectId"], "projectPath": projectPath, "leaseId": "stale-lease",
		}
		productionSingletonRefused(t, author, productionSingletonReviewLatestName, "stale lease",
			[]any{int64(1), projectPath, stale}, "PROJECT_SESSION_STALE", "PROJECT_SESSION_NOT_OWNED")
		productionSingletonRefused(t, author, productionSingletonRecoveryListName, "stale lease",
			[]any{projectPath, stale}, "PROJECT_SESSION_STALE", "PROJECT_SESSION_NOT_OWNED")
	})

	// Every read above was a read: the author database, the committed generation
	// and the outside tree are byte-identical.
	t.Run("database-byte-identity", func(t *testing.T) {
		after, err := os.ReadFile(database)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(after, seededDatabase) {
			t.Fatal("a singleton read changed the admitted author database bytes")
		}
		if current := productionFSSnapshot(t, projectPath); !equalSingletonSnapshots(before, current) {
			t.Fatalf("a singleton read changed the admitted project tree: %v then %v", before, current)
		}
		if current := productionFSSnapshot(t, generationRoot); !equalSingletonSnapshots(generationBefore, current) {
			t.Fatalf("a singleton read changed the committed global generation: %v then %v", generationBefore, current)
		}
		if current := productionFSSnapshot(t, outside); !equalSingletonSnapshots(outsideBefore, current) {
			t.Fatalf("a singleton read changed the tree outside the project: %v then %v", outsideBefore, current)
		}
	})
}

// equalSingletonSnapshots compares two productionFSSnapshot maps without
// reflect.DeepEqual's nil-versus-empty distinction getting in the way.
func equalSingletonSnapshots(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for name, digest := range left {
		if right[name] != digest {
			return false
		}
	}
	return true
}
