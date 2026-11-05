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
)

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
		`{"theme":"light","editorFontSize":17,"defaultModelId":null}`); err != nil {
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
	if err := seedProjectDatabase(database, "CI Journey Novel", *seed); err != nil {
		return err
	}
	evidence, digest, err := verifyAgainstPinnedContract(context.Background(), database)
	if err != nil {
		return err
	}
	fmt.Printf("fixture root: %s\n", rootValue)
	fmt.Printf("pinned contract: user_version=%d fingerprint=%s tables=%d database_sha256=%s\n",
		evidence.UserVersion, evidence.SchemaFingerprint, len(evidence.TableCounts), digest)
	fmt.Printf("seeded rows: project_core=%d contents=%d drafts=%d\n",
		evidence.TableCounts["project_core"], evidence.TableCounts["contents"], evidence.TableCounts["drafts"])
	fmt.Printf("library project: %s\n", filepath.Join(rootValue, "workspaces", *workspace))
	fmt.Printf("project id: %s\n", *projectID)
	fmt.Printf("seeded draft body: %s\n", *seed)
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
	if evidence.TableCounts["project_core"] != 1 || evidence.TableCounts["contents"] != 1 || evidence.TableCounts["drafts"] != 1 {
		return zero, "", fmt.Errorf("the seeded author rows were not committed: %v", evidence.TableCounts)
	}
	return evidence, digest, nil
}

// seedProjectDatabase replaces the oracle's synthetic contents row with one
// working draft the browser journey can list, save and read back.
func seedProjectDatabase(database, projectName, seed string) error {
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(database)}
	uri.RawQuery = "mode=rw&_foreign_keys=on"
	db, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(1)
	if err := insertAuthorRows(db, projectName, seed); err != nil {
		_ = db.Close()
		return err
	}
	// The container copies the database set into its own scratch directory, so
	// the fixture leaves no un-checkpointed write-ahead log behind. A database
	// that is not in WAL mode reports no checkpoint row and is left untouched.
	var backfill, logFrames, checkpointed int64
	if err := db.QueryRow("PRAGMA wal_checkpoint(TRUNCATE)").Scan(&backfill, &logFrames, &checkpointed); err != nil && !errors.Is(err, sql.ErrNoRows) {
		_ = db.Close()
		return err
	}
	if err := db.Close(); err != nil {
		return err
	}
	for _, suffix := range []string{"-shm", "-wal", "-journal"} {
		sidecar := database + suffix
		info, err := os.Lstat(sidecar)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if info.Size() != 0 {
			return fmt.Errorf("the fixture database still has a non-empty %s sidecar", suffix)
		}
		if err := os.Remove(sidecar); err != nil {
			return err
		}
	}
	return nil
}

// insertAuthorRows writes the smallest author history the current-schema
// runtime reads: one project_core row, one contents row and one working draft.
// The column set matches the admitted runtime's own queries instead of a
// private test schema.
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
	return tx.Commit()
}
