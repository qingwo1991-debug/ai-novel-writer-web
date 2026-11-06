//go:build linux

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3" // Registers the database/sql driver "sqlite3".
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/admission"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
)

const (
	// productionCreateTab is this test's own browser tab, so the lease it takes
	// cannot be confused with any other test's.
	productionCreateTab = "client_create_1234567890"
	// productionCreateRawName is a name the reference rewrites: surrounding
	// whitespace is trimmed, and ':' and '?' become '_'
	// (electron/services/project-access.ts:104-117). The duplicate-create case
	// re-spells this same name with different whitespace, so a refusal there
	// proves the duplicate check runs on the sanitized path, not on the raw
	// string the browser typed.
	productionCreateRawName   = "  第一本 书:测试?  "
	productionCreateCleanName = "第一本 书_测试_"
	productionCreateSecondRaw = "第二本*书"
	productionCreateSecond    = "第二本_书"
)

// productionCreateDataRoot is syntheticProductionDataDir with an EMPTY library:
// the shipped pinned contract admits none of that fixture's synthetic projects,
// and a create needs a library that starts empty so the duplicate refusal really
// is the only thing standing between two calls and one directory.
func productionCreateDataRoot(t *testing.T) string {
	t.Helper()
	root := syntheticProductionDataDir(t)
	library := filepath.Join(root, productionLibraryDirectory)
	entries, err := os.ReadDir(library)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if err := os.RemoveAll(filepath.Join(library, entry.Name())); err != nil {
			t.Fatal(err)
		}
	}
	if remaining, err := os.ReadDir(library); err != nil || len(remaining) != 0 {
		t.Fatalf("the create fixture library is not empty: %v %v", remaining, err)
	}
	return root
}

// productionCreateCall invokes project:create and returns the decoded result plus
// the raw envelope, so a test can also assert what the wire does NOT contain.
func productionCreateCall(t *testing.T, author *productionDurableClient, args []any) (productionProjectCreateResult, string) {
	t.Helper()
	response := author.call(t, "project:create", args)
	if response.Code != http.StatusOK {
		t.Fatalf("project:create: %d %s", response.Code, response.Body.String())
	}
	var wire struct {
		Result productionProjectCreateResult `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &wire); err != nil {
		t.Fatalf("project:create: %v %s", err, response.Body.String())
	}
	return wire.Result, response.Body.String()
}

// productionCreateConfig is the CreateProjectConfig the renderer sends
// (src/shared/ipc-channels.ts:246-252). parentGrantId is carried and never
// obeyed: this lane's only writable parent is its own library root.
func productionCreateConfig(name string) map[string]any {
	return map[string]any{
		"name": name, "parentGrantId": "renderer-directory-grant",
		"genre": "玄幻", "targetAudience": "男频", "writingLanguage": "en-US",
	}
}

// productionCreateReadOnly opens a project's database the way an auditor would:
// mode=ro, never the lane's own handle.
func productionCreateReadOnly(t *testing.T, database string) *sql.DB {
	t.Helper()
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(database)}
	query := url.Values{}
	query.Set("mode", "ro")
	query.Set("_busy_timeout", "5000")
	uri.RawQuery = query.Encode()
	db, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// productionCreateSameSnapshot fails unless no byte under root changed. SQLite
// sidecars are excluded by productionFSSnapshot, so a read-write open that
// legitimately created one is not a difference, while a created, removed or
// replaced file always is.
func productionCreateSameSnapshot(t *testing.T, label string, before, after map[string]string) {
	t.Helper()
	if reflect.DeepEqual(before, after) {
		return
	}
	differences := make([]string, 0, 8)
	for path, digest := range after {
		if before[path] != digest {
			differences = append(differences, "added or changed: "+path)
		}
	}
	for path := range before {
		if _, exists := after[path]; !exists {
			differences = append(differences, "removed: "+path)
		}
	}
	t.Fatalf("%s changed %d entr(y/ies) under the data root: %v", label, len(differences), differences)
}

// TestProductionProjectCreateServesTheCreateContract is the whole point of this
// file: a browser POSTs project:create, and the project it gets back is one the
// same process then opens, reads and admits - proven through the lane's own
// operations, never by looking at the directory.
func TestProductionProjectCreateServesTheCreateContract(t *testing.T) {
	root := productionCreateDataRoot(t)
	// The REAL pinned v7 contract, not the synthetic test one: create mints v7,
	// and this lane has to admit it.
	contract, err := admission.PinnedSchemaContract()
	if err != nil {
		t.Fatal(err)
	}
	handler, cleanup, err := productionHandlerWithContract(migrationRegistry(t), productionDurableAddress, "", root,
		productionConfig{Schema: contract, MaxDatabaseBytes: productionMaxDatabaseBytes})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	author := newProductionDurableClient(t, handler, productionCreateTab)
	library := filepath.Join(root, productionLibraryDirectory)
	projectPath := filepath.Join(library, productionCreateCleanName)
	database := filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName)

	// The lease the first project:open produces. The last subtest proves a later
	// create neither rotates nor breaks it.
	var openedLease, openedPath string

	t.Run("create answers the reference payload", func(t *testing.T) {
		result, body := productionCreateCall(t, author, []any{
			productionCreateConfig(productionCreateRawName), "create-token-1", nil,
		})
		if !result.Success || result.Error != "" {
			t.Fatalf("project:create refused an empty library: %+v %s", result, body)
		}
		if result.ProjectPath != projectPath {
			t.Fatalf("created %q; want the sanitized path %q (the reference trims and rewrites :?)", result.ProjectPath, projectPath)
		}
		if result.RequestToken != "create-token-1" {
			t.Fatalf("create did not echo the request token: %+v", result)
		}
		// src/stores/project-store.ts:487-495 treats a false in either field as a
		// failed create, so a truthful success must carry both.
		if !result.DatabaseRestored || !result.DBReady {
			t.Fatalf("create reported an unusable database state: %+v", result)
		}
		if result.Warning != "" {
			t.Fatalf("create answered a warning for a project it registered: %q", result.Warning)
		}
		if projectID := productionCreateManifestProjectID(t, projectPath); projectID == "" || result.ProjectID != projectID {
			t.Fatalf("create answered projectId %q but the manifest names %q", result.ProjectID, projectID)
		}
		// There is no rollback boundary in this lane, so activeProjectPath is the
		// caller's own live lease - absent here, because nothing is open yet.
		if result.ActiveProjectPath != nil {
			t.Fatalf("create reported an active project for a caller holding no lease: %q", *result.ActiveProjectPath)
		}
		// `stale` belongs to the desktop's serialized open/create queue
		// (electron/controllers/project-controller.ts:474-490). project:create is a
		// engine-scoped operation here (web/operations.json:298) carrying no project
		// session, so it is not lease-fenced and must never claim staleness.
		if strings.Contains(body, `"stale":true`) {
			t.Fatalf("create claimed staleness in a lane with no serialized open queue: %s", body)
		}
	})

	t.Run("the directory layout is the reference layout", func(t *testing.T) {
		storage := filepath.Join(projectPath, projectfs.StorageDirectory)
		for _, directory := range []string{projectPath, storage, filepath.Join(storage, productionProjectCreatePromptDirectory)} {
			info, err := os.Lstat(directory)
			if err != nil || !info.IsDir() {
				t.Fatalf("missing project directory %s: %v", directory, err)
			}
			// projectfs admits no group/other-writable directory in a project.
			if info.Mode().Perm()&0o022 != 0 {
				t.Fatalf("%s is %v; want owner-only", directory, info.Mode().Perm())
			}
		}
		for _, name := range []string{projectfs.ManifestName, projectfs.DatabaseName} {
			path := filepath.Join(storage, name)
			info, err := os.Lstat(path)
			if err != nil || !info.Mode().IsRegular() {
				t.Fatalf("missing project file %s: %v", path, err)
			}
			if info.Mode().Perm() != 0o600 || info.Mode()&os.ModeSymlink != 0 {
				t.Fatalf("%s is %v; want a 0600 regular file", path, info.Mode())
			}
		}
		// The canonical project carries no legacy storage directory at all.
		if _, err := os.Lstat(filepath.Join(projectPath, ".vela")); !os.IsNotExist(err) {
			t.Fatalf("create wrote a legacy storage directory: %v", err)
		}
	})

	t.Run("the manifest is the canonical six-key document", func(t *testing.T) {
		raw, err := os.ReadFile(filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.ManifestName))
		if err != nil {
			t.Fatal(err)
		}
		// writeManifest writes JSON.stringify(manifest, null, 2) plus a newline
		// (electron/services/project-access.ts:345-353); no other shape is signed.
		if !strings.HasSuffix(string(raw), "\n") || !strings.Contains(string(raw), "\n  \"") {
			t.Fatalf("manifest is not the two-space indented document with a trailing newline:\n%s", raw)
		}
		var document map[string]any
		if err := json.Unmarshal(raw, &document); err != nil {
			t.Fatalf("manifest is not JSON: %v\n%s", err, raw)
		}
		want := []string{"schemaVersion", "kind", "projectId", "createdAt", "storageFormat", "storageVersion"}
		if len(document) != len(want) {
			t.Fatalf("manifest has %d keys; want exactly %v: %s", len(document), want, raw)
		}
		for _, key := range want {
			if _, exists := document[key]; !exists {
				t.Fatalf("manifest is missing %q: %s", key, raw)
			}
		}
		if document["schemaVersion"] != float64(1) || document["storageVersion"] != float64(1) ||
			document["kind"] != productionProjectCreateKind || document["storageFormat"] != productionProjectCreateStorageFormat {
			t.Fatalf("manifest literals are not the canonical ones: %s", raw)
		}
		projectID, _ := document["projectId"].(string)
		if len(projectID) != 36 || strings.Count(projectID, "-") != 4 {
			t.Fatalf("manifest projectId is not a UUID: %q", projectID)
		}
		createdAt, _ := document["createdAt"].(string)
		if len(createdAt) != len("2006-01-02T15:04:05.000Z") || !strings.HasSuffix(createdAt, "Z") {
			t.Fatalf("manifest createdAt is not new Date().toISOString(): %q", createdAt)
		}
		// The library's own reader is the only authority on the manifest, so the
		// document must also pass projectfs.ParseManifest.
		manifest, err := projectfs.ParseManifest(raw)
		if err != nil {
			t.Fatalf("projectfs.ParseManifest refused the created manifest: %v", err)
		}
		if manifest.ProjectID != projectID {
			t.Fatalf("ParseManifest read projectId %q; the document says %q", manifest.ProjectID, projectID)
		}
	})

	t.Run("the database is the pinned v7 schema", func(t *testing.T) {
		// The strongest single assertion in this file: the created file verifies
		// against the SHIPPED contract - integrity, foreign keys, user_version and
		// the sqlite_master fingerprint - through the same verifier the startup
		// harvest and projectaccess.AdmitAuthor use. The fingerprint list is read
		// from internal/admission/pinned_contract.go (PinnedSchemaContract), i.e.
		// the CI-exported TypeScript oracle, never from this test's own DDL.
		evidence, err := admission.VerifySnapshot(context.Background(), database, contract)
		if err != nil {
			t.Fatalf("the created database is not the pinned v7 schema: %v", err)
		}
		if evidence.UserVersion != admission.CurrentSchemaVersion {
			t.Fatalf("created database is user_version %d; want the pinned %d", evidence.UserVersion, admission.CurrentSchemaVersion)
		}
		if evidence.SchemaFingerprint == "" {
			t.Fatal("the verifier returned no schema fingerprint")
		}
		pinned := false
		for _, entry := range contract {
			if entry.Version != admission.CurrentSchemaVersion {
				continue
			}
			for _, fingerprint := range entry.Fingerprints {
				if fingerprint == evidence.SchemaFingerprint {
					pinned = true
				}
			}
		}
		if !pinned {
			t.Fatalf("the created schema fingerprints to %q, which the pinned v7 contract does not list", evidence.SchemaFingerprint)
		}
		// The two rows `project:create` is responsible for are present, and the
		// asset's own DDL is what defines the tables they live in.
		if evidence.TableCounts["project_core"] != 1 || evidence.TableCounts["character_identity_meta"] != 1 {
			t.Fatalf("created database seeded the wrong rows: %v", evidence.TableCounts)
		}
	})

	t.Run("the shelf serves the new project without a restart", func(t *testing.T) {
		response := author.call(t, "project:recent-list", []any{})
		var wire struct {
			Result []map[string]any `json:"result"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &wire); err != nil || response.Code != http.StatusOK {
			t.Fatalf("project:recent-list: %d %s %v", response.Code, response.Body.String(), err)
		}
		if len(wire.Result) != 1 {
			t.Fatalf("project:recent-list returned %d projects; want only the created one: %s", len(wire.Result), response.Body.String())
		}
		if wire.Result[0]["name"] != productionCreateCleanName || wire.Result[0]["path"] != projectPath {
			t.Fatalf("the shelf did not pick up the created project: %v", wire.Result[0])
		}
	})

	t.Run("project:open opens the created project", func(t *testing.T) {
		var opened productionDurableOpenWire
		author.decode(t, "project:open", []any{projectPath, "create-open-token", projectPath}, &opened)
		if !opened.Success || !opened.DBReady || opened.Error != "" {
			t.Fatalf("project:open refused the project create just minted: %+v", opened)
		}
		if opened.Project["path"] != projectPath {
			t.Fatalf("project:open returned the wrong path: %v", opened.Project)
		}
		if opened.Project["name"] != productionCreateCleanName {
			t.Fatalf("project:open read project_name %v; want %q", opened.Project["name"], productionCreateCleanName)
		}
		lease, _ := opened.Project["sessionLease"].(string)
		if lease == "" {
			t.Fatalf("project:open returned no session lease: %v", opened.Project)
		}
		openedLease, openedPath = lease, projectPath
		// The projectId the browser stores must be the manifest's, not a second
		// identifier minted by the database: project:open reads it from the
		// manifest through projectfs (internal/authorruntime/project_open.go:106-124).
		if projectID := productionCreateManifestProjectID(t, projectPath); opened.Project["id"] != projectID {
			t.Fatalf("project:open answered id %v; the manifest names %q", opened.Project["id"], projectID)
		}
		config, _ := opened.Project["novelConfig"].(map[string]any)
		if config["writingLanguage"] != "en-US" {
			t.Fatalf("project:open did not read the created writing language: %v", config)
		}
	})

	t.Run("a separate read-only connection reads the seeded rows", func(t *testing.T) {
		db := productionCreateReadOnly(t, database)
		var version int
		if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
			t.Fatal(err)
		}
		if version != admission.CurrentSchemaVersion {
			t.Fatalf("user_version is %d; want %d", version, admission.CurrentSchemaVersion)
		}
		var name, genre, audience, language string
		if err := db.QueryRow(`SELECT project_name, genre, target_audience, writing_language
			FROM project_core WHERE id = 'main'`).Scan(&name, &genre, &audience, &language); err != nil {
			t.Fatalf("the created database has no readable project_core row: %v", err)
		}
		if name != productionCreateCleanName || genre != "玄幻" || audience != "男频" || language != "en-US" {
			t.Fatalf("project_core row is %q/%q/%q/%q; want the request's values", name, genre, audience, language)
		}
		// The row the domain invariant requires (internal/admission/identity.go:23-25).
		var revision, legacyCount int
		if err := db.QueryRow(`SELECT revision, legacy_count FROM character_identity_meta WHERE id = 'main'`).
			Scan(&revision, &legacyCount); err != nil {
			t.Fatalf("the created database has no character_identity_meta singleton: %v", err)
		}
		if revision != 0 || legacyCount != 0 {
			t.Fatalf("character_identity_meta is (%d, %d); want the migration's (0, 0)", revision, legacyCount)
		}
	})

	t.Run("a duplicate name is refused and changes no byte", func(t *testing.T) {
		before := productionFSSnapshot(t, root)
		// The same project, re-spelled: different surrounding whitespace, so only
		// the sanitized path can decide this.
		result, body := productionCreateCall(t, author, []any{
			productionCreateConfig(" " + productionCreateRawName + " "), "create-token-duplicate", nil,
		})
		if result.Success || result.Error != productionProjectCreateDirectoryExists {
			t.Fatalf("a duplicate name was not refused with %s: %+v %s",
				productionProjectCreateDirectoryExists, result, body)
		}
		if result.ProjectPath != "" || result.ProjectID != "" {
			t.Fatalf("a refused create answered a project identity: %+v", result)
		}
		productionCreateSameSnapshot(t, "the duplicate refusal", before, productionFSSnapshot(t, root))
	})

	t.Run("a malformed request is refused before any write", func(t *testing.T) {
		before := productionFSSnapshot(t, root)
		// Two arguments instead of three: the wire shape is fixed by
		// src/shared/ipc-channels.ts:342-346, so this is a protocol error, not a
		// project request.
		result, body := productionCreateCall(t, author, []any{productionCreateConfig(productionCreateSecondRaw)})
		if result.Success || result.Error != productionProjectCreateInvalidArguments {
			t.Fatalf("a malformed create was not refused with %s: %+v %s",
				productionProjectCreateInvalidArguments, result, body)
		}
		productionCreateSameSnapshot(t, "the malformed request", before, productionFSSnapshot(t, root))
	})

	t.Run("a parent this lane never vouched for is refused and changes no byte", func(t *testing.T) {
		before := productionFSSnapshot(t, root)
		// The browser names a directory that exists inside the data root but is
		// not a project and not this lane's library root. The desktop would have
		// resolved it from a one-shot grant (project-controller.ts:493-496); this
		// lane accepts only null, its own library root, or an admitted project.
		foreign := filepath.Join(root, productionLegacyDirectory)
		result, body := productionCreateCall(t, author, []any{
			productionCreateConfig(productionCreateSecondRaw), "create-token-foreign-parent", foreign,
		})
		if result.Success || result.Error != productionProjectCreateTargetRefused {
			t.Fatalf("a foreign parent was not refused with %s: %+v %s",
				productionProjectCreateTargetRefused, result, body)
		}
		if _, err := os.Lstat(filepath.Join(filepath.Dir(foreign), productionCreateSecond)); !os.IsNotExist(err) {
			t.Fatalf("the refusal created something outside the library: %v", err)
		}
		productionCreateSameSnapshot(t, "the foreign-parent refusal", before, productionFSSnapshot(t, root))
	})

	t.Run("a live lease survives another create", func(t *testing.T) {
		// This is the lease-fencing question. project:create carries no project
		// session and is not lease-fenced (web/operations.json:298 scopes it
		// globally), so there is no stale-lease refusal to assert - and this lane
		// has no serialized open queue that could produce one. What it must never
		// do is disturb the lease a caller already holds.
		if openedLease == "" || openedPath == "" {
			t.Fatal("the earlier project:open subtest did not establish a lease")
		}
		result, body := productionCreateCall(t, author, []any{
			productionCreateConfig(productionCreateSecondRaw), "create-token-with-lease", openedPath,
		})
		if !result.Success {
			t.Fatalf("create refused while a lease was live: %+v %s", result, body)
		}
		if result.ActiveProjectPath == nil || *result.ActiveProjectPath != openedPath {
			t.Fatalf("create did not report the caller's live lease: %+v", result)
		}
		// The opened project is still readable through its own database, and it
		// still opens: a create that had rotated, closed or stolen the lease would
		// fail here.
		db := productionCreateReadOnly(t, database)
		var name string
		if err := db.QueryRow(`SELECT project_name FROM project_core WHERE id = 'main'`).Scan(&name); err != nil {
			t.Fatalf("the previously opened project stopped being readable: %v", err)
		}
		if name != productionCreateCleanName {
			t.Fatalf("the previously opened project reads %q", name)
		}
		// A second `project:open` for the same project with a fresh request token is
		// NOT checked here. The claim this subtest makes is that a create elsewhere
		// does not disturb the lease a caller already holds, and that is proved by
		// the four checks around this comment: the create succeeded, it reported the
		// caller's live lease instead of a new one, the opened project's database is
		// still readable, and it is still listed. Reopening is a separate question
		// with its own contract (a refresh must resolve to the existing lease), and
		// this lane has not settled it - asserting it here would pin a behaviour
		// nobody has verified against the reference.
		response := author.call(t, "project:recent-list", []any{})
		var wire struct {
			Result []map[string]any `json:"result"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &wire); err != nil || len(wire.Result) != 2 {
			t.Fatalf("project:recent-list: %d %s %v", response.Code, response.Body.String(), err)
		}
		// Both projects are on the shelf: the second create is a new entry, not a
		// replacement. The order is the library's own (database mtime, newest
		// first), and two creates can land inside one millisecond, so this asserts
		// membership rather than order.
		shelf := map[any]bool{}
		for _, entry := range wire.Result {
			shelf[entry["path"]] = true
		}
		for _, want := range []string{projectPath, filepath.Join(library, productionCreateSecond)} {
			if !shelf[want] {
				t.Fatalf("the shelf lost %q after a second create: %v", want, wire.Result)
			}
		}
	})
}

// productionCreateManifestProjectID reads the projectId the manifest names. It is
// a helper rather than a second source of truth: every other assertion compares
// against it.
func productionCreateManifestProjectID(t *testing.T, projectPath string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.ManifestName))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var document struct {
		ProjectID string `json:"projectId"`
	}
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatalf("parse manifest: %v %s", err, raw)
	}
	return document.ProjectID
}
