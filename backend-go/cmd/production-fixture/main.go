// Command production-fixture builds the disposable production data root that the
// container journey in .github/workflows/go-backend.yml starts from.
//
// This command is CI-only. It creates a synthetic data root under the runner's
// temporary directory and refuses to reuse an existing one, so it can never
// point at an author's mount. It duplicates no schema by hand: the project
// database it seeds is the original TypeScript migration lane's baseline-v7.db,
// the same file the admission and author-runtime tests already open, and the
// seeded fixture is re-proved against the shipped pinned contract with
// admission.PinnedSchemaContract before any container is started. A fixture that
// does not pass that contract fails here instead of failing silently later.
package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	_ "github.com/mattn/go-sqlite3" // Registers the database/sql driver "sqlite3".
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/admission"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
)

// The committed model configuration and the one generation run the journey's
// read assertions name. They are constants because the workflow asserts the same
// values: a fixture that drifted from them would fail the journey instead of
// quietly proving nothing.
const (
	fixtureModelID          = "ci-model-1"
	fixtureEmbeddingModelID = "ci-embedding-model-1"
	fixtureGenerationEpoch  = "lease-0000000001-history"
	fixtureGenerationOp     = "chapter-draft"
	fixtureGenerationNonce  = "ci-generation-nonce-1"
	fixtureGenerationText   = "草稿候选：门后有人。"
)

// fixtureGeneration is the identity of the settled run the fixture commits, so
// the journey can name the same run the fixture wrote.
type fixtureGeneration struct {
	RootActionID string
	RunID        string
	AttemptID    string
	Epoch        string
	Text         string
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "PRODUCTION_FIXTURE_FAILED:", err)
		os.Exit(1)
	}
}

func run() error {
	root := flag.String("root", "", "disposable absolute data root to create")
	baseline := flag.String("baseline", "", "original TypeScript baseline-v7.db the project database is copied from")
	workspace := flag.String("workspace", "ci-novel", "workspace directory name under workspaces/")
	projectID := flag.String("project-id", "", "canonical project id written to .ai-novel/project.json")
	generation := flag.String("generation", "", "committed global generation id recorded in the migration receipt")
	seed := flag.String("seed", "", "synthetic draft body seeded into the project database")
	flag.Parse()
	if *root == "" {
		return errors.New("-root is required")
	}
	if *baseline == "" {
		return errors.New("-baseline is required")
	}
	if *workspace == "" || *workspace == "." || *workspace == ".." || strings.ContainsAny(*workspace, "/\\") {
		return errors.New("-workspace must be one plain directory name")
	}
	if *projectID == "" {
		return errors.New("-project-id is required")
	}
	if len(*generation) != 36 {
		return errors.New("-generation must be the 36 character committed generation id")
	}
	if !utf8.ValidString(*seed) || strings.TrimSpace(*seed) == "" {
		return errors.New("-seed is required")
	}
	rootValue := filepath.Clean(*root)
	if !filepath.IsAbs(rootValue) || rootValue == string(filepath.Separator) {
		return errors.New("-root must be an absolute disposable directory")
	}
	if _, err := os.Lstat(rootValue); err == nil {
		return errors.New("the fixture root already exists; it must be disposable")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	info, err := os.Stat(*baseline)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() < 100 {
		return errors.New("the baseline project database is not a usable SQLite file")
	}

	// The fixed production layout, mirrored from the unexported constants in
	// cmd/novel-server/production_open_linux.go. A drift here is caught by the
	// container startup itself, which refuses a data root it does not recognise.
	canonicalTarget := filepath.Join(rootValue, "config", "ai-novel-writer")
	generationDirectory := filepath.Join(canonicalTarget, "generations", *generation)
	storage := filepath.Join(rootValue, "workspaces", *workspace, projectfs.StorageDirectory)
	for _, directory := range []string{
		filepath.Join(canonicalTarget, ".migration"),
		generationDirectory,
		filepath.Join(rootValue, "config", "ai-novel-writer-web-runtime"),
		filepath.Join(rootValue, "legacy-global"),
		storage,
	} {
		if err := os.MkdirAll(directory, 0700); err != nil {
			return err
		}
	}

	// The completed-migration receipt the shipped gateway leaves behind. The
	// generation holds config.json; nothing else is required by the receipt.
	receipt := fmt.Sprintf(`{"version":1,"generation":%q,"completed":true,"preservedUnknownCount":2,"requiredObjects":["config.json"]}`, *generation)
	if err := writeFile(filepath.Join(canonicalTarget, ".migration", "receipt.json"), receipt); err != nil {
		return err
	}
	if err := writeFile(filepath.Join(generationDirectory, "config.json"),
		fmt.Sprintf(`{"theme":"light","editorFontSize":17,"defaultModelId":%[1]q,"defaultEmbeddingModelId":%[2]q,"stageModelIds":{"drafting":%[1]q}}`,
			fixtureModelID, fixtureEmbeddingModelID)); err != nil {
		return err
	}
	// The private model store of the committed generation. The journey's
	// llm:list-models assertion needs a profile it can recognise and a credential
	// that must never come back, and the two routing reads need the ids above to
	// resolve, exactly as they must on a real author machine.
	if err := writeFile(filepath.Join(generationDirectory, "models.json"),
		fmt.Sprintf(`[{"id":%[1]q,"provider":"custom","protocol":"openai","baseUrl":"https://synthetic.invalid/v1","apiKey":"synthetic-ci-model-key","name":"CI 正文模型","modelName":"ci-story-model","temperature":0.7,"maxTokens":1024},`+
			`{"id":%[2]q,"provider":"custom","protocol":"openai","baseUrl":"https://synthetic.invalid/v1","apiKey":"synthetic-ci-embedding-key","name":"CI 向量模型","modelName":"ci-embedding-model"}]`,
			fixtureModelID, fixtureEmbeddingModelID)); err != nil {
		return err
	}
	// The six-key canonical project identity contract.
	manifest := fmt.Sprintf(`{"schemaVersion":1,"kind":"ai-novel-project","projectId":%q,"createdAt":"2024-05-06T07:08:09.000Z","storageFormat":"ai-novel","storageVersion":1}`, *projectID)
	if err := writeFile(filepath.Join(storage, projectfs.ManifestName), manifest); err != nil {
		return err
	}
	database := filepath.Join(storage, projectfs.DatabaseName)
	if err := copyFile(*baseline, database); err != nil {
		return err
	}
	generationSeed, err := seedProjectDatabase(database, *projectID, "CI Journey Novel", *seed)
	if err != nil {
		return err
	}
	evidence, digest, err := verifyAgainstPinnedContract(context.Background(), database)
	if err != nil {
		return err
	}
	fmt.Printf("fixture root: %s\n", rootValue)
	fmt.Printf("pinned contract: user_version=%d fingerprint=%s tables=%d database_sha256=%s\n",
		evidence.UserVersion, evidence.SchemaFingerprint, len(evidence.TableCounts), digest)
	fmt.Printf("seeded rows: project_core=%d contents=%d drafts=%d characters=%d blueprints=%d\n",
		evidence.TableCounts["project_core"], evidence.TableCounts["contents"], evidence.TableCounts["drafts"],
		evidence.TableCounts["characters"], evidence.TableCounts["blueprints"])
	fmt.Printf("library project: %s\n", filepath.Join(rootValue, "workspaces", *workspace))
	fmt.Printf("project id: %s\n", *projectID)
	fmt.Printf("seeded draft body: %s\n", *seed)
	fmt.Printf("seeded generation run: root=%s run=%s attempt=%s epoch=%s\n",
		generationSeed.RootActionID, generationSeed.RunID, generationSeed.AttemptID, generationSeed.Epoch)
	fmt.Printf("committed models: default=%s embedding=%s stage_drafting=%s\n",
		fixtureModelID, fixtureEmbeddingModelID, fixtureModelID)
	return nil
}

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content+"\n"), 0600)
}

func copyFile(source, target string) error {
	data, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	return os.WriteFile(target, data, 0600)
}

func sha256File(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	digest := sha256.New()
	_, copyErr := io.Copy(digest, file)
	closeErr := file.Close()
	if copyErr != nil {
		return "", copyErr
	}
	if closeErr != nil {
		return "", closeErr
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

// verifyAgainstPinnedContract opens a private byte copy, exactly as the
// production lane stages one, and proves the fixture against the compiled
// authority the container will use at startup.
func verifyAgainstPinnedContract(ctx context.Context, database string) (admission.Evidence, string, error) {
	var zero admission.Evidence
	contract, err := admission.PinnedSchemaContract()
	if err != nil {
		return zero, "", err
	}
	digest, err := sha256File(database)
	if err != nil {
		return zero, "", err
	}
	directory, err := os.MkdirTemp("", "novel-production-fixture-")
	if err != nil {
		return zero, "", err
	}
	defer func() { _ = os.RemoveAll(directory) }()
	snapshot := filepath.Join(directory, "snapshot.db")
	if err := copyFile(database, snapshot); err != nil {
		return zero, "", err
	}
	evidence, err := admission.VerifySnapshot(ctx, snapshot, contract)
	if err != nil {
		return zero, "", fmt.Errorf("the seeded project database does not pass the shipped pinned contract: %w", err)
	}
	if evidence.UserVersion != admission.CurrentSchemaVersion {
		return zero, "", fmt.Errorf("the seeded project database is schema version %d", evidence.UserVersion)
	}
	if evidence.TableCounts["project_core"] != 1 || evidence.TableCounts["contents"] != 1 || evidence.TableCounts["drafts"] != 1 ||
		evidence.TableCounts["characters"] != 1 || evidence.TableCounts["blueprints"] != 1 {
		return zero, "", fmt.Errorf("the seeded author rows were not committed: %v", evidence.TableCounts)
	}
	return evidence, digest, nil
}

// seedProjectDatabase replaces the oracle's synthetic contents row with one
// working draft the browser journey can list, save and read back, and commits one
// settled generation run through the shipped ledger so the journey's
// generation:list/generation:read assertions read real ledger rows.
func seedProjectDatabase(database, projectID, projectName, seed string) (fixtureGeneration, error) {
	var zero fixtureGeneration
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(database)}
	uri.RawQuery = "mode=rw&_foreign_keys=on"
	db, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		return zero, err
	}
	db.SetMaxOpenConns(1)
	if err := insertAuthorRows(db, projectName, seed); err != nil {
		_ = db.Close()
		return zero, err
	}
	generation, err := seedGenerationLedger(db, projectID)
	if err != nil {
		_ = db.Close()
		return zero, err
	}
	// The container copies the database set into its own scratch directory, so
	// the fixture leaves no un-checkpointed write-ahead log behind. A database
	// that is not in WAL mode reports no checkpoint row and is left untouched.
	var backfill, logFrames, checkpointed int64
	if err := db.QueryRow("PRAGMA wal_checkpoint(TRUNCATE)").Scan(&backfill, &logFrames, &checkpointed); err != nil && !errors.Is(err, sql.ErrNoRows) {
		_ = db.Close()
		return zero, err
	}
	if err := db.Close(); err != nil {
		return zero, err
	}
	for _, suffix := range []string{"-shm", "-wal", "-journal"} {
		sidecar := database + suffix
		info, err := os.Lstat(sidecar)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return zero, err
		}
		if info.Size() != 0 {
			return zero, fmt.Errorf("the fixture database still has a non-empty %s sidecar", suffix)
		}
		if err := os.Remove(sidecar); err != nil {
			return zero, err
		}
	}
	return generation, nil
}

// fixtureGenerationRequest is the frozen binding of the fixture's run: the same
// nine hashes, manifest and budget shape a real drafting request carries, written
// as JSON text so the ledger parses it with its order-preserving decoder.
func fixtureGenerationRequest(projectID string) string {
	hash := strings.Repeat("a", 64)
	return `{"projectId":` + quote(projectID) + `,"epoch":` + quote(fixtureGenerationEpoch) +
		`,"operation":` + quote(fixtureGenerationOp) + `,"uiActionNonce":` + quote(fixtureGenerationNonce) +
		`,"frozenInputHash":` + quote(hash) + `,"fingerprint":{"chapterBriefHash":` + quote(hash) +
		`,"authorGuidanceHash":` + quote(hash) + `,"dependencyHash":` + quote(hash) +
		`,"contextSnapshotHash":` + quote(hash) + `,"templateHash":` + quote(hash) +
		`,"skillSnapshotHash":` + quote(hash) + `,"modelLeaseRevision":` + quote(hash) +
		`,"policyHash":` + quote(hash) + `,"outputContractHash":` + quote(hash) +
		`},"contextSnapshotId":"ci-context-snapshot-1","sourceManifest":{"operation":` + quote(fixtureGenerationOp) +
		`},"sourceRefs":[],"budget":{"maxPhysicalRequests":3,"maxTokenLiability":4000,` +
		`"maxOutputPerRequest":1000,"maxActiveElapsedMs":600000}}`
}

// quote renders a Go string as a JSON string literal.
func quote(value string) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return `""`
	}
	return string(encoded)
}

// seedGenerationLedger drives the shipped ledger through one complete settlement
// chain - open, reserve, dispatch, snapshot, settle - so the fixture's committed
// history has exactly the shape a real one does.
func seedGenerationLedger(db *sql.DB, projectID string) (fixtureGeneration, error) {
	var zero fixtureGeneration
	ordinal := 0
	ledger := store.NewGenerationLedger(db, func() int64 { return 1700000000000 }, func() string {
		ordinal++
		return fmt.Sprintf("fixture-ledger-%02d", ordinal)
	})
	ctx := context.Background()
	value, err := ledger.CallJSON(ctx, "open", json.RawMessage("["+fixtureGenerationRequest(projectID)+"]"))
	if err != nil {
		return zero, fmt.Errorf("generation open: %w", err)
	}
	run, ok := value.(map[string]any)
	if !ok {
		return zero, errors.New("generation open returned no run")
	}
	runID, _ := run["runId"].(string)
	rootID, _ := run["rootActionId"].(string)
	if runID == "" || rootID == "" {
		return zero, errors.New("generation open returned no run identity")
	}
	reserved, err := ledger.Call(ctx, "reserve", []any{runID, fixtureGenerationNonce, strings.Repeat("b", 64), 200, 100})
	if err != nil {
		return zero, fmt.Errorf("generation reserve: %w", err)
	}
	receipt, _ := reserved.(map[string]any)
	attempt, _ := receipt["attempt"].(map[string]any)
	attemptID, _ := attempt["attemptId"].(string)
	if attemptID == "" {
		return zero, errors.New("generation reserve returned no attempt")
	}
	if _, err := ledger.Call(ctx, "markDispatched", []any{attemptID}); err != nil {
		return zero, fmt.Errorf("generation markDispatched: %w", err)
	}
	if _, err := ledger.Call(ctx, "snapshot", []any{attemptID, 0, fixtureGenerationText}); err != nil {
		return zero, fmt.Errorf("generation snapshot: %w", err)
	}
	if _, err := ledger.Call(ctx, "settle", []any{attemptID, map[string]any{"trusted": true, "actualTokens": 120}, "stop"}); err != nil {
		return zero, fmt.Errorf("generation settle: %w", err)
	}
	return fixtureGeneration{
		RootActionID: rootID,
		RunID:        runID,
		AttemptID:    attemptID,
		Epoch:        fixtureGenerationEpoch,
		Text:         fixtureGenerationText,
	}, nil
}

// insertAuthorRows writes the smallest author history the current-schema
// runtime reads: one project_core row, one contents row, one working draft, one
// character card and one blueprint row. The column set matches the admitted
// runtime's own queries instead of a private test schema, and every row is
// synthetic: the character and the blueprint exist so the container journey can
// prove the editor's read surface returns real rows instead of an empty list or
// a default shape. The roster and identity metadata rows are the migration
// lane's own (M02 and the roster schema insert them); this fixture never
// fabricates a hash receipt it cannot compute.
func insertAuthorRows(db *sql.DB, projectName, seed string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, statement := range []string{
		"DELETE FROM finalization_outbox",
		"DELETE FROM drafts",
		"DELETE FROM contents",
		"DELETE FROM project_core",
		"DELETE FROM blueprints",
		"DELETE FROM characters",
	} {
		if _, err := tx.Exec(statement); err != nil {
			return fmt.Errorf("%s: %w", statement, err)
		}
	}
	const core = `INSERT INTO project_core (id, project_name, writing_language, genre,
 sub_genre, target_audience, total_chapters, words_per_chapter, creative_strategy,
 narrative_thread_dormant_threshold, plot_structure, narrative_pov, core_outline,
 world_setting, golden_finger, protagonist_profile, global_guidance, writing_style,
 reference_works, character_states, created_at, updated_at)
 VALUES ('main', ?, 'zh-CN', 'fantasy', '', '', 12, 3000, 'auto', 3,
 '', '', '', '', '', '', '', '', '', '{}', '2024-05-06 07:08:09', '2024-05-06 07:08:09')`
	if _, err := tx.Exec(core, projectName); err != nil {
		return err
	}
	if _, err := tx.Exec("INSERT INTO contents (id, body) VALUES (1, ?)", seed); err != nil {
		return err
	}
	const draft = `INSERT INTO drafts (id, chapter_number, version, status, source,
 content_id, word_count, source_dependencies, created_at, updated_at)
 VALUES (1, 1, 1, 'draft', 'write', 1, ?, '[]', '2024-05-06 07:08:09', '2024-05-06 07:08:09')`
	if _, err := tx.Exec(draft, utf8.RuneCountInString(seed)); err != nil {
		return err
	}
	// One active character card and one chapter blueprint: the two rows the
	// editor's character and chapter/volume reads must return. Only columns the
	// admitted runtime itself selects are written; everything else keeps the
	// schema's own defaults.
	const character = `INSERT INTO characters (character_id, name, role, retired, created_at, updated_at)
 VALUES ('ci-character-1', '甲', 'protagonist', 0, '2024-05-06 07:08:09', '2024-05-06 07:08:09')`
	if _, err := tx.Exec(character); err != nil {
		return err
	}
	const blueprint = `INSERT INTO blueprints (chapter_number, title, role, purpose,
 key_events, characters, suspense_hook, user_guidance, notes, notes_updated_at, created_at, updated_at)
 VALUES (1, '第一章 门', 'setup', '开篇', '["他推开门"]', '["甲"]', '风声', '', '', '', '2024-05-06 07:08:09', '2024-05-06 07:08:09')`
	if _, err := tx.Exec(blueprint); err != nil {
		return err
	}
	return tx.Commit()
}
