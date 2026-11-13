package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// Used only when the original TS schema oracle is unavailable outside CI, or
// for legacy NULL-language cases prohibited by the current schema's NOT NULL.
// Never alters any existing store fixture helper or production database.
const projectCoreFallbackSchema = `CREATE TABLE project_core (
 id TEXT PRIMARY KEY DEFAULT 'main', project_name TEXT NOT NULL DEFAULT '',
 genre TEXT DEFAULT '', sub_genre TEXT DEFAULT '', target_audience TEXT DEFAULT '',
 total_chapters INTEGER DEFAULT 100, words_per_chapter INTEGER DEFAULT 3000,
 writing_language TEXT DEFAULT 'zh-CN', creative_strategy TEXT DEFAULT 'auto',
 narrative_thread_dormant_threshold INTEGER DEFAULT 3,
 plot_structure TEXT DEFAULT 'three_act', narrative_pov TEXT DEFAULT 'third_limited',
 writing_style TEXT DEFAULT '', reference_works TEXT DEFAULT '', global_guidance TEXT DEFAULT '',
 golden_finger TEXT DEFAULT '', core_outline TEXT DEFAULT '', world_setting TEXT DEFAULT '',
 protagonist_profile TEXT DEFAULT '', premise TEXT DEFAULT '', worldbuilding TEXT DEFAULT '',
 characters_arch TEXT DEFAULT '', synopsis TEXT DEFAULT '', character_states TEXT DEFAULT '',
 plot_tree_snapshot TEXT NOT NULL DEFAULT '', created_at TEXT DEFAULT (datetime('now')),
 updated_at TEXT DEFAULT (datetime('now'))
);`

func projectCoreFixture(t *testing.T, synthetic bool) (*sql.DB, *ProjectCore, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "project core #? 中文.db")
	contract := os.Getenv("GO_SQLITE_CONTRACT")
	if contract == "" && os.Getenv("GITHUB_ACTIONS") == "true" {
		t.Fatal("GO_SQLITE_CONTRACT original TypeScript schema is mandatory in GitHub Actions")
	}
	if contract != "" && !synthetic {
		index, err := os.ReadFile(contract)
		if err != nil {
			t.Fatal(err)
		}
		var manifest struct {
			Fixtures []struct {
				Name string `json:"name"`
				File string `json:"file"`
			} `json:"fixtures"`
		}
		if err := json.Unmarshal(index, &manifest); err != nil {
			t.Fatal(err)
		}
		name := "baseline-v7"
		if strings.Contains(t.Name(), "donor") {
			name = "donor-v7"
		}
		var source string
		for _, fixture := range manifest.Fixtures {
			if fixture.Name == name {
				source = filepath.Join(filepath.Dir(contract), fixture.File)
				break
			}
		}
		if source == "" {
			t.Fatalf("original TypeScript oracle has no %s fixture", name)
		}
		original, err := os.ReadFile(source)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, original, 0600); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			after, err := os.ReadFile(source)
			if err != nil || !bytes.Equal(original, after) {
				t.Errorf("original TS schema fixture changed: %v", err)
			}
		})
	}
	uri := url.URL{Scheme: "file", Path: path}
	uri.RawQuery = "mode=rwc&_busy_timeout=5000"
	db, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(4)
	t.Cleanup(func() { _ = db.Close() })
	if synthetic || contract == "" {
		projectCoreExec(t, db, projectCoreFallbackSchema)
	}
	projectCoreExec(t, db, "DELETE FROM project_core")
	projectCoreExec(t, db, "INSERT INTO project_core(id,project_name,created_at,updated_at) VALUES('main','Original','created-original','before')")
	return db, NewProjectCore(db), path
}

func projectCoreExec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}

func projectCoreRead(t *testing.T, repository *ProjectCore) map[string]any {
	t.Helper()
	data, err := repository.Get(context.Background())
	if err != nil || data == nil {
		t.Fatalf("Get = %#v, %v", data, err)
	}
	return data
}

func projectCoreTimestamp(t *testing.T, db *sql.DB) string {
	t.Helper()
	var timestamp string
	if err := db.QueryRow("SELECT updated_at FROM project_core WHERE id='main'").Scan(&timestamp); err != nil {
		t.Fatal(err)
	}
	return timestamp
}

func projectCoreExpected(t *testing.T, db *sql.DB) map[string]any {
	t.Helper()
	projectCoreExec(t, db, `UPDATE project_core SET
 synopsis='Original outline', premise='Premise v1', characters_arch='Characters v1',
 worldbuilding='World v1', genre='mystery', total_chapters=80, words_per_chapter=2500,
 writing_language='zh-CN', plot_structure='three_act', narrative_pov='third_limited',
 global_guidance='Keep the reveal private.', updated_at='before' WHERE id='main'`)
	return map[string]any{
		"synopsis": "Original outline", "premise": "Premise v1", "charactersArch": "Characters v1",
		"worldbuilding": "World v1", "genre": "mystery", "totalChapters": int64(80), "wordsPerChapter": int64(2500),
		"writingLanguage": "zh-CN", "plotStructure": "three_act", "narrativePov": "third_limited",
		"globalGuidance": "Keep the reveal private.",
	}
}

func TestProjectCoreExactFieldMapping(t *testing.T) {
	for _, layout := range []string{"baseline", "donor"} {
		t.Run(layout, func(t *testing.T) {
			db, repository, _ := projectCoreFixture(t, false)
			data := map[string]any{
				"projectName": "中文 😀\r\n\x00", "genre": "genre-value", "subGenre": "sub-value", "targetAudience": "audience",
				"totalChapters": int64(81), "wordsPerChapter": int64(2600), "writingLanguage": "en-US",
				"creativeStrategy": "deep-planning", "narrativeThreadDormantChapterThreshold": int64(9),
				"plotStructure": "multi_thread", "narrativePov": "first_person", "writingStyle": "style",
				"referenceWorks": "reference", "globalGuidance": "guidance", "goldenFinger": "golden",
				"coreOutline": "core", "worldSetting": "setting", "protagonistProfile": "protagonist",
				"premise": "premise", "worldbuilding": "world", "synopsis": "synopsis", "characterStates": "states",
			}
			projectCoreExec(t, db, "UPDATE project_core SET characters_arch='roster', plot_tree_snapshot='snapshot'")
			if err := repository.Update(context.Background(), data); err != nil {
				t.Fatal(err)
			}
			// Independently written SQL column names catch accidental mapping errors
			// even if both Update and Get share the same incorrect field table.
			columns := []string{"project_name", "genre", "sub_genre", "target_audience", "total_chapters", "words_per_chapter", "writing_language", "creative_strategy", "narrative_thread_dormant_threshold", "plot_structure", "narrative_pov", "writing_style", "reference_works", "global_guidance", "golden_finger", "core_outline", "world_setting", "protagonist_profile", "premise", "worldbuilding", "synopsis", "character_states"}
			keys := []string{"projectName", "genre", "subGenre", "targetAudience", "totalChapters", "wordsPerChapter", "writingLanguage", "creativeStrategy", "narrativeThreadDormantChapterThreshold", "plotStructure", "narrativePov", "writingStyle", "referenceWorks", "globalGuidance", "goldenFinger", "coreOutline", "worldSetting", "protagonistProfile", "premise", "worldbuilding", "synopsis", "characterStates"}
			for i, column := range columns {
				var actual any
				if err := db.QueryRow("SELECT " + column + " FROM project_core WHERE id='main'").Scan(&actual); err != nil || !reflect.DeepEqual(actual, data[keys[i]]) {
					t.Fatalf("%s = %#v, want %#v; err=%v", column, actual, data[keys[i]], err)
				}
			}
			data["charactersArch"] = "roster"
			if actual := projectCoreRead(t, repository); !reflect.DeepEqual(actual, data) {
				t.Fatalf("Get = %#v, want %#v", actual, data)
			}
			var created, snapshot string
			if err := db.QueryRow("SELECT created_at,plot_tree_snapshot FROM project_core WHERE id='main'").Scan(&created, &snapshot); err != nil || created != "created-original" || snapshot != "snapshot" {
				t.Fatal("unrelated columns changed", created, snapshot, err)
			}
			if timestamp := projectCoreTimestamp(t, db); timestamp == "before" {
				t.Fatal("Update did not timestamp")
			}
		})
	}
}

func TestProjectCoreNullableRawValuesAndReadNormalizers(t *testing.T) {
	db, repository, _ := projectCoreFixture(t, false)
	projectCoreExec(t, db, `UPDATE project_core SET genre=NULL, total_chapters=NULL,
 synopsis=?, premise=?, writing_language='unknown', creative_strategy='unknown',
 narrative_thread_dormant_threshold=8.9`, []byte{0, 255}, float64(1.5))
	data := projectCoreRead(t, repository)
	if data["genre"] != nil || data["totalChapters"] != nil || !reflect.DeepEqual(data["synopsis"], []byte{0, 255}) || data["premise"] != "1.5" {
		t.Fatalf("raw SQL values coerced: %#v", data)
	}
	if data["writingLanguage"] != "zh-CN" || data["creativeStrategy"] != "auto" || data["narrativeThreadDormantChapterThreshold"] != int64(8) {
		t.Fatalf("normalization mismatch: %#v", data)
	}
	for _, strategy := range []string{"auto", "fluent-drafting", "consistency-first", "deep-planning"} {
		projectCoreExec(t, db, "UPDATE project_core SET creative_strategy=?", strategy)
		if projectCoreRead(t, repository)["creativeStrategy"] != strategy {
			t.Fatal("valid strategy was not preserved", strategy)
		}
	}
	for _, language := range []string{"en-US", "zh-CN", "EN-US", " en-US", "en-US ", ""} {
		projectCoreExec(t, db, "UPDATE project_core SET writing_language=?", language)
		want := "zh-CN"
		if language == "en-US" {
			want = "en-US"
		}
		if projectCoreRead(t, repository)["writingLanguage"] != want {
			t.Fatal("language normalization mismatch", language)
		}
	}
}

func TestProjectCoreThresholdUpdateOnlyNormalization(t *testing.T) {
	db, repository, _ := projectCoreFixture(t, false)
	cases := []struct {
		value any
		want  int64
	}{
		{nil, 3}, {"7", 3}, {true, 3}, {map[string]any{}, 3}, {[]any{4}, 3},
		{math.NaN(), 3}, {math.Inf(1), 3}, {math.Inf(-1), 3},
		{float64(8.9), 8}, {float64(-8.9), 1}, {float64(0), 1}, {float64(50.9), 50},
		{int64(17), 17}, {float64(1e100), 50},
	}
	for _, test := range cases {
		if err := repository.Update(context.Background(), map[string]any{"narrativeThreadDormantChapterThreshold": test.value}); err != nil {
			t.Fatal(err)
		}
		var stored int64
		if err := db.QueryRow("SELECT narrative_thread_dormant_threshold FROM project_core").Scan(&stored); err != nil || stored != test.want {
			t.Fatalf("threshold(%#v) = %d, want %d, err=%v", test.value, stored, test.want, err)
		}
	}
	if err := repository.Update(context.Background(), map[string]any{"writingLanguage": "raw-language", "creativeStrategy": "raw-strategy"}); err != nil {
		t.Fatal(err)
	}
	var language, strategy string
	if err := db.QueryRow("SELECT writing_language,creative_strategy FROM project_core").Scan(&language, &strategy); err != nil || language != "raw-language" || strategy != "raw-strategy" {
		t.Fatal("Update must not apply read normalizers", language, strategy, err)
	}
	projectCoreExec(t, db, "UPDATE project_core SET narrative_thread_dormant_threshold='invalid'")
	if projectCoreRead(t, repository)["narrativeThreadDormantChapterThreshold"] != int64(3) {
		t.Fatal("non-numeric persisted threshold must default to 3")
	}
}

func TestProjectCoreReadOnlyUnknownAndInvalidBindingsNoMutation(t *testing.T) {
	db, repository, _ := projectCoreFixture(t, false)
	ctx := context.Background()
	before := projectCoreRead(t, repository)
	for _, value := range []any{nil, "replacement", false, map[string]any{}} {
		err := repository.Update(ctx, map[string]any{"charactersArch": value, "projectName": "forbidden", "genre": map[string]any{}})
		if !errors.Is(err, ErrProjectCoreCharactersArchReadOnly) {
			t.Fatalf("read-only key presence: %v", err)
		}
	}
	if err := repository.Update(ctx, map[string]any{"id": "other", "updated_at": "forged", "createdAt": "forged", "plotTreeSnapshot": "forged", "characters_arch": "forged", "unknown": true}); err != nil {
		t.Fatal(err)
	}
	if err := repository.Update(ctx, nil); err != nil {
		t.Fatal(err)
	}
	for _, value := range []any{true, map[string]any{}, []any{}, struct{}{}} {
		if err := repository.Update(ctx, map[string]any{"projectName": "must-not-write", "genre": value}); !errors.Is(err, ErrProjectCoreInvalidBinding) {
			t.Fatalf("bad binding accepted: %#v, %v", value, err)
		}
	}
	if got := projectCoreRead(t, repository); !reflect.DeepEqual(got, before) || projectCoreTimestamp(t, db) != "before" {
		t.Fatalf("rejected/no-op update mutated data: %#v", got)
	}
	if err := repository.Update(ctx, map[string]any{"genre": nil}); err != nil || projectCoreRead(t, repository)["genre"] != nil {
		t.Fatal("explicit NULL must be preserved", err)
	}
}

func TestProjectCoreSynopsisCASEverySourceField(t *testing.T) {
	columns := []string{"synopsis", "premise", "characters_arch", "worldbuilding", "genre", "total_chapters", "words_per_chapter", "writing_language", "plot_structure", "narrative_pov", "global_guidance"}
	for _, column := range columns {
		t.Run(column, func(t *testing.T) {
			db, repository, _ := projectCoreFixture(t, false)
			expected := projectCoreExpected(t, db)
			var changed any = "changed é\r\n😀"
			if column == "total_chapters" || column == "words_per_chapter" {
				changed = int64(999)
			}
			if column == "writing_language" {
				changed = "en-US"
			}
			projectCoreExec(t, db, "UPDATE project_core SET "+column+"=? WHERE id='main'", changed)
			before := projectCoreRead(t, repository)
			projectCoreExec(t, db, `CREATE TRIGGER reject_stale_synopsis BEFORE UPDATE ON project_core
 BEGIN SELECT RAISE(ABORT,'stale CAS attempted mutation'); END`)
			ok, err := repository.CommitSynopsis(context.Background(), map[string]any{"synopsis": "must not write", "expected": expected})
			if err != nil || ok || !reflect.DeepEqual(projectCoreRead(t, repository), before) || projectCoreTimestamp(t, db) != "before" {
				t.Fatalf("stale %s: ok=%v err=%v", column, ok, err)
			}
		})
	}
}

func TestProjectCoreSynopsisCASSuccessIgnoresUnrelatedChurn(t *testing.T) {
	for _, layout := range []string{"baseline", "donor"} {
		t.Run(layout, func(t *testing.T) {
			db, repository, _ := projectCoreFixture(t, false)
			expected := projectCoreExpected(t, db)
			projectCoreExec(t, db, `UPDATE project_core SET updated_at='unrelated', project_name='new name',
 sub_genre='new subgenre', target_audience='new audience', writing_style='new style',
 reference_works='new reference', golden_finger='new power', core_outline='new core',
 world_setting='new setting', protagonist_profile='new protagonist',
 creative_strategy='deep-planning', narrative_thread_dormant_threshold=10,
 character_states='new states', plot_tree_snapshot='new snapshot'`)
			before := projectCoreRead(t, repository)
			ok, err := repository.CommitSynopsis(context.Background(), map[string]any{"synopsis": "Replacement\x00\r\n😀", "expected": expected, "ignored": true})
			if err != nil || !ok {
				t.Fatal("matching CAS rejected", ok, err)
			}
			before["synopsis"] = "Replacement\x00\r\n😀"
			if !reflect.DeepEqual(projectCoreRead(t, repository), before) || projectCoreTimestamp(t, db) == "unrelated" {
				t.Fatal("CAS mutated unrelated data or missed timestamp")
			}
		})
	}
}

func TestProjectCoreSynopsisCASLegacyLanguageAndSQLNull(t *testing.T) {
	for _, language := range []any{nil, "", "unknown", "EN-US", []byte("en-US"), "en-US"} {
		t.Run(fmt.Sprintf("%T-%v", language, language), func(t *testing.T) {
			db, repository, _ := projectCoreFixture(t, true)
			expected := projectCoreExpected(t, db)
			projectCoreExec(t, db, "UPDATE project_core SET writing_language=?", language)
			expected["writingLanguage"] = projectCoreRead(t, repository)["writingLanguage"]
			ok, err := repository.CommitSynopsis(context.Background(), map[string]any{"synopsis": "replacement", "expected": expected})
			if err != nil || !ok {
				t.Fatal("normalized language CAS", language, ok, err)
			}
		})
	}
	db, repository, _ := projectCoreFixture(t, false)
	expected := projectCoreExpected(t, db)
	projectCoreExec(t, db, "UPDATE project_core SET premise=NULL")
	expected["premise"] = nil
	if ok, err := repository.CommitSynopsis(context.Background(), map[string]any{"synopsis": "must not write", "expected": expected}); err != nil || ok || projectCoreTimestamp(t, db) != "before" {
		t.Fatal("NULL equality must not become IS/COALESCE", ok, err)
	}
}

func TestProjectCoreSynopsisCASEmptyExactAndNoOp(t *testing.T) {
	db, repository, _ := projectCoreFixture(t, false)
	expected := projectCoreExpected(t, db)
	for _, value := range []string{"", "é\r\n😀 "} {
		projectCoreExec(t, db, "UPDATE project_core SET synopsis=?,updated_at='before'", value)
		expected["synopsis"] = value
		ok, err := repository.CommitSynopsis(context.Background(), map[string]any{"synopsis": value, "expected": expected})
		if err != nil || !ok || projectCoreTimestamp(t, db) == "before" {
			t.Fatal("matching no-op body must count one updated row", ok, err)
		}
	}
	for _, value := range []string{"e\u0301\r\n😀 ", "é\n😀 ", "é\r\n😀"} {
		expected["synopsis"] = value
		if ok, err := repository.CommitSynopsis(context.Background(), map[string]any{"synopsis": "forbidden", "expected": expected}); err != nil || ok {
			t.Fatal("body must compare without trim/Unicode/newline normalization", ok, err)
		}
	}
}

func TestProjectCoreSynopsisCASOwnerAndMalformedFailClosed(t *testing.T) {
	db, repository, _ := projectCoreFixture(t, false)
	expected := projectCoreExpected(t, db)
	for _, key := range []string{"generationRunHandle", "authorRecovery"} {
		for _, value := range []any{nil, false, "", map[string]any{}} {
			ok, err := repository.CommitSynopsis(context.Background(), map[string]any{"synopsis": "forbidden", "expected": expected, key: value})
			if ok || !errors.Is(err, ErrGenerationOwnerRequired) {
				t.Fatalf("owner key presence must reject: %s %#v %v", key, value, err)
			}
		}
	}
	for _, request := range []map[string]any{
		nil, {}, {"synopsis": "new"}, {"synopsis": "new", "expected": nil},
		{"synopsis": "new", "expected": map[string]any{}}, {"expected": expected},
		{"synopsis": true, "expected": expected},
	} {
		if ok, err := repository.CommitSynopsis(context.Background(), request); ok || !errors.Is(err, ErrProjectCoreInvalidBinding) {
			t.Fatalf("malformed request accepted: %#v %v", request, err)
		}
	}
	for _, key := range []string{"synopsis", "premise", "charactersArch", "worldbuilding", "genre", "totalChapters", "wordsPerChapter", "writingLanguage", "plotStructure", "narrativePov", "globalGuidance"} {
		value := expected[key]
		delete(expected, key)
		if ok, err := repository.CommitSynopsis(context.Background(), map[string]any{"synopsis": "forbidden", "expected": expected}); ok || !errors.Is(err, ErrProjectCoreInvalidBinding) {
			t.Fatal("missing expected field accepted", key, ok, err)
		}
		expected[key] = true
		if ok, err := repository.CommitSynopsis(context.Background(), map[string]any{"synopsis": "forbidden", "expected": expected}); ok || !errors.Is(err, ErrProjectCoreInvalidBinding) {
			t.Fatal("invalid expected binding accepted", key, ok, err)
		}
		expected[key] = value
	}
	if projectCoreRead(t, repository)["synopsis"] != "Original outline" || projectCoreTimestamp(t, db) != "before" {
		t.Fatal("failed request mutated row")
	}
}

func TestProjectCoreSynopsisCASConcurrentIndependentPools(t *testing.T) {
	db, repository, path := projectCoreFixture(t, false)
	expected := projectCoreExpected(t, db)
	dsn, err := ExistingDSN(path)
	if err != nil {
		t.Fatal(err)
	}
	other, err := sql.Open("sqlite3", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	other.SetMaxOpenConns(1)
	start := make(chan struct{})
	results := make(chan bool, 2)
	failures := make(chan error, 2)
	var ready sync.WaitGroup
	ready.Add(2)
	for i, repo := range []*ProjectCore{repository, NewProjectCore(other)} {
		go func(index int, r *ProjectCore) {
			ready.Done()
			<-start
			ok, err := r.CommitSynopsis(context.Background(), map[string]any{"synopsis": fmt.Sprintf("winner-%d", index), "expected": expected})
			results <- ok
			failures <- err
		}(i, repo)
	}
	ready.Wait()
	close(start)
	wins := 0
	for range 2 {
		if <-results {
			wins++
		}
		if err := <-failures; err != nil {
			t.Fatal("concurrent CAS error", err)
		}
	}
	if wins != 1 {
		t.Fatalf("CAS winners=%d, want exactly one", wins)
	}
	if value := projectCoreRead(t, repository)["synopsis"]; value != "winner-0" && value != "winner-1" {
		t.Fatal("unexpected final synopsis", value)
	}
}

func TestProjectCoreMissingUnopenedCancelledAndNoMigrations(t *testing.T) {
	ctx := context.Background()
	unopened := NewProjectCore(nil)
	if data, err := unopened.Get(ctx); err != nil || data != nil {
		t.Fatal("unopened Get", data, err)
	}
	if err := unopened.Update(ctx, nil); !errors.Is(err, ErrProjectNotOpen) {
		t.Fatal("unopened Update", err)
	}
	if ok, err := unopened.CommitSynopsis(ctx, nil); ok || !errors.Is(err, ErrProjectNotOpen) {
		t.Fatal("unopened CAS", ok, err)
	}
	db, repository, _ := projectCoreFixture(t, false)
	expected := projectCoreExpected(t, db)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := repository.Get(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatal("Get ignored cancellation", err)
	}
	if err := repository.Update(cancelled, map[string]any{"genre": "new"}); !errors.Is(err, context.Canceled) {
		t.Fatal("Update ignored cancellation", err)
	}
	if ok, err := repository.CommitSynopsis(cancelled, map[string]any{"synopsis": "new", "expected": expected}); ok || !errors.Is(err, context.Canceled) {
		t.Fatal("CAS ignored cancellation", ok, err)
	}
	projectCoreExec(t, db, "DELETE FROM project_core")
	if data, err := repository.Get(ctx); err != nil || data != nil {
		t.Fatal("missing main", data, err)
	}
	if err := repository.Update(ctx, map[string]any{"genre": "not an upsert"}); err != nil {
		t.Fatal("missing row update must be a no-op", err)
	}
	if ok, err := repository.CommitSynopsis(ctx, map[string]any{"synopsis": "new", "expected": expected}); err != nil || ok {
		t.Fatal("missing row CAS", ok, err)
	}
	projectCoreExec(t, db, "DROP TABLE project_core; PRAGMA user_version=99")
	repository = NewProjectCore(db)
	if _, err := repository.Get(ctx); err == nil {
		t.Fatal("missing schema must error")
	}
	var count, version int
	if err := db.QueryRow("SELECT count(*) FROM sqlite_master WHERE name='project_core'").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || count != 0 || version != 99 {
		t.Fatal("repository silently initialized/migrated", count, version, err)
	}
}

func TestProjectCoreSQLFailureIsAtomic(t *testing.T) {
	db, repository, _ := projectCoreFixture(t, false)
	expected := projectCoreExpected(t, db)
	projectCoreExec(t, db, `CREATE TRIGGER reject_core BEFORE UPDATE ON project_core
 BEGIN SELECT RAISE(ABORT,'core mutation rejected'); END`)
	if err := repository.Update(context.Background(), map[string]any{"genre": "new", "synopsis": "new"}); err == nil || !strings.Contains(err.Error(), "core mutation rejected") {
		t.Fatal("SQL update failure lost", err)
	}
	if ok, err := repository.CommitSynopsis(context.Background(), map[string]any{"synopsis": "new", "expected": expected}); ok || err == nil || !strings.Contains(err.Error(), "core mutation rejected") {
		t.Fatal("SQL CAS failure lost", ok, err)
	}
	if projectCoreRead(t, repository)["synopsis"] != "Original outline" || projectCoreTimestamp(t, db) != "before" {
		t.Fatal("SQL error partially mutated data")
	}
}
