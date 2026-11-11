//go:build linux

package main

// The legacy copy's decisive evidence. One disposable data root, one lane built
// exactly as production_open_linux.go builds it (the pickers mint, the
// portability family answers), and the one name driven over the frozen HTTP
// session the browser uses. The tests below prove, in order:
//
//   - a granted legacy directory is copied into the library byte for byte: the
//     two trees' digests, the entry lists and one carried file all agree, the
//     source is untouched (this lane copies, it never moves), and the answer
//     names the copy by its symbolic identity, never by a host path;
//   - a source that was never granted - an unminted token, a token minted for
//     another tab, and a token whose target is a file - is refused by name;
//   - an existing destination is refused by name;
//   - a source that carries this lane's own project files is refused by name,
//     because copying it would duplicate the library's own project;
//   - a destination that is the source itself, and a name one level below the
//     library, are both refused by name;
//   - a grant naming a tree outside the roots is refused by name, and a symlink
//     that points out of the granted directory is refused instead of followed -
//     the planted marker's bytes never reach the copy;
//   - every one of those refusals leaves the library tree byte-identical;
//   - the binder line and the served inventory still say so.
//
// Nothing here reaches outside the fixture: every path is created by the test, and
// the one path outside the data root is a t.TempDir the marker test plants itself.

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/librarypath"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

const (
	productionLegacyImportFixtureTab       = "client_legacy_import_tab1"
	productionLegacyImportFixtureForeign   = "client_legacy_import_foreign1"
	productionLegacyImportFixtureSource    = "旧版样书"
	productionLegacyImportFixtureCopy      = "旧版样书-新版副本"
	productionLegacyImportFixtureExisting  = "已存在的目录"
	productionLegacyImportFixtureProjectID = "3b0e6f4c-9a1d-4a52-8c3e-1d7b2f5a6c90"
)

// productionLegacyImportLane is one open production lane over a disposable data
// root, with the two families the import needs mounted where the browser mounts
// them: the pickers mint both grants, and the portability family answers the name.
type productionLegacyImportLane struct {
	mux     http.Handler
	server  *transport.Server
	library string
	client  *productionDurableClient
}

func newProductionLegacyImportLane(t *testing.T) *productionLegacyImportLane {
	t.Helper()
	root := syntheticProductionDataDir(t)
	layout, layoutErr := productionLayoutFor(root)
	if layoutErr != nil {
		t.Fatal(layoutErr)
	}
	startup, cancel := context.WithTimeout(context.Background(), productionStartupTimeout)
	defer cancel()
	config := productionConfig{Schema: productionTestContract(), MaxDatabaseBytes: productionMaxDatabaseBytes}
	open, openErr := openProduction(startup, layout, config)
	if openErr != nil {
		t.Fatal(openErr)
	}
	server := transport.New(migrationRegistry(t), security.NewOrigins("http://"+productionDurableAddress),
		"go-production-legacy-import")
	mux := http.NewServeMux()
	mux.Handle("/api/", server)
	mux.Handle("/api", server)
	mux.Handle("/healthz", server)
	mux.Handle("/", server)
	t.Cleanup(func() {
		server.Close()
		if closeErr := open.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if bindErr := newProductionBrowserPickers(open).Bind(server); bindErr != nil {
		t.Fatalf("bind the picker names: %v", bindErr)
	}
	if bindErr := newProductionPortability(open, config).Bind(server); bindErr != nil {
		t.Fatalf("bind the portability family: %v", bindErr)
	}
	return &productionLegacyImportLane{
		mux:     mux,
		server:  server,
		library: layout.libraryRoot,
		client:  newProductionDurableClient(t, mux, productionLegacyImportFixtureTab),
	}
}

// productionLegacyImportAnswer is the channel's own union
// (src/shared/ipc-channels.ts:329-334), decoded as the page sees it.
type productionLegacyImportAnswer struct {
	State      string `json:"state"`
	Code       string `json:"code"`
	ProjectID  string `json:"projectId"`
	TargetRoot string `json:"targetRoot"`
}

// invoke drives the name over HTTP through the frozen session and requires an
// HTTP 200 answer, because the shipped renderer reads `result.state` and would
// treat a thrown error as an unexpected failure instead of a refusal.
func (l *productionLegacyImportLane) invoke(t *testing.T, client *productionDurableClient, source, target string) productionLegacyImportAnswer {
	t.Helper()
	var answer productionLegacyImportAnswer
	client.decode(t, productionPortabilityLegacyCopyName, []any{source, target}, &answer)
	return answer
}

// pick mints one grant the way the page does: a symbolic root, a relative path,
// and the answer's grantId.
func productionLegacyImportPick(t *testing.T, client *productionDurableClient, operation, relative string) string {
	t.Helper()
	var answer struct {
		Success bool   `json:"success"`
		GrantID string `json:"grantId"`
		Error   string `json:"error"`
	}
	query := map[string]any{"rootId": librarypath.LibraryRootID, "relativePath": relative}
	client.decode(t, operation, []any{query}, &answer)
	if !answer.Success || answer.GrantID == "" {
		t.Fatalf("%v refused to mint a grant for %v: %v", operation, relative, answer.Error)
	}
	return answer.GrantID
}

// productionLegacyImportMint mints a token for a target this test resolved or
// built itself, for the cases no picker would ever mint: a file instead of a
// directory, and a tree outside every root.
func productionLegacyImportMint(t *testing.T, client *productionDurableClient, target librarypath.Target) string {
	t.Helper()
	registry, registryErr := productionFSGrantsRegistry()
	if registryErr != nil {
		t.Fatalf("the process-wide grant registry is unavailable: %v", registryErr)
	}
	if !target.Valid() {
		t.Fatalf("the test target for %v is not well formed", target.Relative)
	}
	token, mintErr := registry.Mint(productionFSGrantBinding(client), target, librarypath.OpRead,
		librarypath.MintOptions{MaxUses: productionPickerReadUses})
	if mintErr != nil {
		t.Fatalf("mint a test grant for %v: %v", target.Relative, mintErr)
	}
	return token
}

// productionLegacyImportSeed writes one legacy project tree - the old `.vela`
// storage with its manifest, its database and an avatar, plus two loose
// directories and an empty one - and answers its root.
func productionLegacyImportSeed(t *testing.T, library, name string) string {
	t.Helper()
	root := filepath.Join(library, name)
	files := map[string]string{
		".vela/project.json": `{"schemaVersion":1,"kind":"ai-novel-project","projectId":"` +
			productionLegacyImportFixtureProjectID + `","createdAt":"2024-05-06T07:08:09.000Z"}`,
		".vela/vela.db":          "SQLite format 3\x00" + strings.Repeat("旧版数据库正文", 8),
		".vela/avatars/主角.png":   "\x89PNG\r\n\x1a\nlegacy avatar bytes",
		"正文/第一章.md":             "# 第一章\n\n他推开门，风从走廊尽头灌进来，带着雨的气味。\n",
		"设定/世界观.md":             "## 世界观\n\n潮汐历，一年三百二十日。\n",
		".封面.png":               "封面字节",
	}
	for _, relative := range []string{".vela", ".vela/avatars", "正文", "设定", "空目录"} {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(relative)), 0o700); err != nil {
			t.Fatalf("create %v: %v", relative, err)
		}
	}
	names := make([]string, 0, len(files))
	for relative := range files {
		names = append(names, relative)
	}
	sort.Strings(names)
	for _, relative := range names {
		path := filepath.Join(root, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatalf("create the parent of %v: %v", relative, err)
		}
		if err := os.WriteFile(path, []byte(files[relative]), 0o600); err != nil {
			t.Fatalf("write %v: %v", relative, err)
		}
	}
	return root
}

// productionLegacyImportNames lists a tree's relative names, sorted, so two trees
// can be compared by what they carry and not only by their bytes.
func productionLegacyImportNames(t *testing.T, root string) []string {
	t.Helper()
	names := make([]string, 0, 16)
	walkErr := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		names = append(names, relative)
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk %v: %v", root, walkErr)
	}
	sort.Strings(names)
	return names
}

// productionLegacyImportCarries reports whether any file below root holds exactly
// want. It is how "the copy never read the marker" is measured rather than assumed.
func productionLegacyImportCarries(t *testing.T, root string, want []byte) bool {
	t.Helper()
	carried := false
	walkErr := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if bytes.Equal(data, want) {
			carried = true
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk %v: %v", root, walkErr)
	}
	return carried
}

// productionLegacyImportRefused requires one named refusal and nothing else.
func productionLegacyImportRefused(t *testing.T, answer productionLegacyImportAnswer, want string) {
	t.Helper()
	if answer.State != productionLegacyImportBlockedState || answer.Code != want {
		t.Fatalf("the import answered state %v code %v; want blocked with %v", answer.State, answer.Code, want)
	}
}

// productionLegacyImportAbsent requires that a name was not created.
func productionLegacyImportAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the refused import left %v behind: %v", filepath.Base(path), err)
	}
}

// TestProductionLegacyImportCopiesGrantedDirectoryByteForByte is the whole happy
// path: two grants minted by the pickers, one copy inside the library, and the
// source still exactly where and what it was.
func TestProductionLegacyImportCopiesGrantedDirectoryByteForByte(t *testing.T) {
	lane := newProductionLegacyImportLane(t)
	bound := false
	for _, name := range lane.server.BoundOperations() {
		if name == productionPortabilityLegacyCopyName {
			bound = true
		}
	}
	if !bound {
		t.Fatalf("%v is not bound by the built lane", productionPortabilityLegacyCopyName)
	}

	source := productionLegacyImportSeed(t, lane.library, productionLegacyImportFixtureSource)
	sourceDigest := productionCloudBackupTreeDigest(t, source)
	sourceNames := productionLegacyImportNames(t, source)
	copyPath := filepath.Join(lane.library, productionLegacyImportFixtureCopy)
	productionLegacyImportAbsent(t, copyPath)

	sourceGrant := productionLegacyImportPick(t, lane.client, productionPickerLegacyName, productionLegacyImportFixtureSource)
	// The destination does not exist yet: that is what the restore-target picker's
	// create permission is for, and it is the name the author composed.
	targetGrant := productionLegacyImportPick(t, lane.client, productionPickerRestoreTargetName, productionLegacyImportFixtureCopy)

	answer := lane.invoke(t, lane.client, sourceGrant, targetGrant)
	if answer.State != productionLegacyImportReadyState || answer.Code != "" {
		t.Fatalf("the import answered state %v code %v", answer.State, answer.Code)
	}
	if answer.ProjectID != productionLegacyImportFixtureProjectID {
		t.Fatalf("the import reported projectId %v; want the legacy manifest's own", answer.ProjectID)
	}
	identity := librarypath.LibraryRootID + "/" + productionLegacyImportFixtureCopy
	if answer.TargetRoot != identity {
		t.Fatalf("the import reported targetRoot %v; want the symbolic identity %v", answer.TargetRoot, identity)
	}

	// Byte for byte: the same digest, the same names, and one file read back.
	if copiedDigest := productionCloudBackupTreeDigest(t, copyPath); copiedDigest != sourceDigest {
		t.Fatalf("the copy is not the source's bytes: %v became %v", sourceDigest, copiedDigest)
	}
	copiedNames := productionLegacyImportNames(t, copyPath)
	if strings.Join(copiedNames, ",") != strings.Join(sourceNames, ",") {
		t.Fatalf("the copy carries different names: %v", strings.Join(copiedNames, ","))
	}
	original, readErr := os.ReadFile(filepath.Join(source, ".vela", "vela.db"))
	if readErr != nil {
		t.Fatalf("read the legacy database: %v", readErr)
	}
	copied, copiedErr := os.ReadFile(filepath.Join(copyPath, ".vela", "vela.db"))
	if copiedErr != nil {
		t.Fatalf("read the copied legacy database: %v", copiedErr)
	}
	if !bytes.Equal(original, copied) {
		t.Fatalf("the legacy database did not travel byte for byte")
	}
	// It copied; it did not move.
	if after := productionCloudBackupTreeDigest(t, source); after != sourceDigest {
		t.Fatalf("the source changed, so the import moved instead of copied")
	}
}

// TestProductionLegacyImportRefusesUngrantedSource proves a source has to be a
// capability this caller holds for a directory, not a string naming one.
func TestProductionLegacyImportRefusesUngrantedSource(t *testing.T) {
	lane := newProductionLegacyImportLane(t)
	productionLegacyImportSeed(t, lane.library, productionLegacyImportFixtureSource)
	before := productionCloudBackupTreeDigest(t, lane.library)
	copyPath := filepath.Join(lane.library, productionLegacyImportFixtureCopy)
	targetGrant := productionLegacyImportPick(t, lane.client, productionPickerRestoreTargetName, productionLegacyImportFixtureCopy)

	// (a) A token this process never minted.
	productionLegacyImportRefused(t, lane.invoke(t, lane.client, "legacy-import-never-granted", targetGrant),
		productionLegacyImportPathInvalidCode)
	if after := productionCloudBackupTreeDigest(t, lane.library); after != before {
		t.Fatalf("a refusal changed the library tree")
	}
	productionLegacyImportAbsent(t, copyPath)

	// (b) A real grant, minted for another tab of the same cookie session. The
	//     binding is the session and the tab, so this one is somebody else's.
	other := lane.client.siblingTab(productionLegacyImportFixtureForeign)
	foreign := productionLegacyImportPick(t, other, productionPickerLegacyName, productionLegacyImportFixtureSource)
	productionLegacyImportRefused(t, lane.invoke(t, lane.client, foreign, targetGrant),
		productionLegacyImportPathInvalidCode)
	if after := productionCloudBackupTreeDigest(t, lane.library); after != before {
		t.Fatalf("a foreign grant's refusal changed the library tree")
	}
	productionLegacyImportAbsent(t, copyPath)

	// (c) A live token whose target is a file: a source has to be a directory.
	filePath := filepath.Join(lane.library, "散稿.txt")
	if err := os.WriteFile(filePath, []byte("这是一份散稿。\n"), 0o600); err != nil {
		t.Fatalf("seed a loose file: %v", err)
	}
	roots, rootsErr := librarypath.NewRoots([]librarypath.Root{{
		ID: librarypath.LibraryRootID, Kind: librarypath.KindLibrary, Path: lane.library,
	}})
	if rootsErr != nil {
		t.Fatalf("build the library allowlist: %v", rootsErr)
	}
	loose, resolveErr := roots.Resolve(librarypath.LibraryRootID, filepath.Base(filePath))
	if resolveErr != nil {
		t.Fatalf("resolve the loose file: %v", resolveErr)
	}
	fileGrant := productionLegacyImportMint(t, lane.client, loose)
	productionLegacyImportRefused(t, lane.invoke(t, lane.client, fileGrant, targetGrant),
		productionLegacyImportPathInvalidCode)
	productionLegacyImportAbsent(t, copyPath)
}

// TestProductionLegacyImportRefusesExistingDestination proves a copy never
// replaces author data: the destination is one new name or it is refused, and the
// refusal touches nothing.
func TestProductionLegacyImportRefusesExistingDestination(t *testing.T) {
	lane := newProductionLegacyImportLane(t)
	productionLegacyImportSeed(t, lane.library, productionLegacyImportFixtureSource)
	existing := filepath.Join(lane.library, productionLegacyImportFixtureExisting)
	if err := os.MkdirAll(existing, 0o700); err != nil {
		t.Fatalf("seed the existing destination: %v", err)
	}
	if err := os.WriteFile(filepath.Join(existing, "已有.txt"), []byte("现有作品数据\n"), 0o600); err != nil {
		t.Fatalf("seed the existing destination's file: %v", err)
	}
	before := productionCloudBackupTreeDigest(t, lane.library)
	existingDigest := productionCloudBackupTreeDigest(t, existing)

	sourceGrant := productionLegacyImportPick(t, lane.client, productionPickerLegacyName, productionLegacyImportFixtureSource)
	targetGrant := productionLegacyImportPick(t, lane.client, productionPickerRestoreTargetName, productionLegacyImportFixtureExisting)
	productionLegacyImportRefused(t, lane.invoke(t, lane.client, sourceGrant, targetGrant),
		productionLegacyImportTargetExistsCode)

	if after := productionCloudBackupTreeDigest(t, lane.library); after != before {
		t.Fatalf("the refusal changed the library tree")
	}
	if after := productionCloudBackupTreeDigest(t, existing); after != existingDigest {
		t.Fatalf("the refusal changed the existing destination")
	}
}

// TestProductionLegacyImportRefusesLaneProjectSource is the self-copy refusal: a
// source that already carries this lane's own project files is one of the
// library's projects, not a legacy import.
func TestProductionLegacyImportRefusesLaneProjectSource(t *testing.T) {
	lane := newProductionLegacyImportLane(t)
	before := productionCloudBackupTreeDigest(t, lane.library)
	copyPath := filepath.Join(lane.library, productionLegacyImportFixtureCopy)

	// The admitted project the synthetic data root seeds, with its own
	// `.ai-novel/project.json` and database.
	sourceGrant := productionLegacyImportPick(t, lane.client, productionPickerLegacyName, "current-novel")
	targetGrant := productionLegacyImportPick(t, lane.client, productionPickerRestoreTargetName, productionLegacyImportFixtureCopy)
	productionLegacyImportRefused(t, lane.invoke(t, lane.client, sourceGrant, targetGrant),
		productionLegacyImportOwnProjectCode)

	if after := productionCloudBackupTreeDigest(t, lane.library); after != before {
		t.Fatalf("the self-copy refusal changed the library tree")
	}
	productionLegacyImportAbsent(t, copyPath)
}

// TestProductionLegacyImportRefusesOverlappingDestination proves the two trees may
// not contain each other - a destination that IS the source is the self-copy the
// desktop refuses first (legacy-project-copy-import.ts:320) - and that a nested
// name is refused too, because a project is an immediate child of the library and
// nothing else is scanned for one.
func TestProductionLegacyImportRefusesOverlappingDestination(t *testing.T) {
	lane := newProductionLegacyImportLane(t)
	source := productionLegacyImportSeed(t, lane.library, productionLegacyImportFixtureSource)
	before := productionCloudBackupTreeDigest(t, lane.library)
	sourceDigest := productionCloudBackupTreeDigest(t, source)

	sourceGrant := productionLegacyImportPick(t, lane.client, productionPickerLegacyName, productionLegacyImportFixtureSource)
	// The source's own name: it exists, so only the overlap rule can refuse it
	// (the existence rule runs after it).
	selfGrant := productionLegacyImportPick(t, lane.client, productionPickerRestoreTargetName, productionLegacyImportFixtureSource)
	productionLegacyImportRefused(t, lane.invoke(t, lane.client, sourceGrant, selfGrant),
		productionLegacyImportOverlapCode)
	if after := productionCloudBackupTreeDigest(t, lane.library); after != before {
		t.Fatalf("the overlap refusal changed the library tree")
	}
	if after := productionCloudBackupTreeDigest(t, source); after != sourceDigest {
		t.Fatalf("the overlap refusal changed the source tree")
	}

	// A name one level down: its parent exists, so only the depth-1 rule can
	// refuse it, and a copy written there would be invisible to the shelf.
	nestedGrant := productionLegacyImportPick(t, lane.client, productionPickerRestoreTargetName,
		productionLegacyImportFixtureSource+"/副本")
	productionLegacyImportRefused(t, lane.invoke(t, lane.client, sourceGrant, nestedGrant),
		productionLegacyImportTargetUnsafeCode)
	if after := productionCloudBackupTreeDigest(t, lane.library); after != before {
		t.Fatalf("the nested-name refusal changed the library tree")
	}
	productionLegacyImportAbsent(t, filepath.Join(source, "副本"))
}

// TestProductionLegacyImportNeverReadsOutsideTheRoots is the confinement proof. A
// marker is planted outside every root and another beside the source, and a symlink
// inside the source points at the first one. The first import refuses it by name
// and changes nothing; the second, with the symlink gone, copies the source and
// carries neither marker's bytes.
func TestProductionLegacyImportNeverReadsOutsideTheRoots(t *testing.T) {
	lane := newProductionLegacyImportLane(t)
	outside := t.TempDir()
	markerBytes := []byte("这是数据根之外的标记，任何副本都不得携带。\n")
	marker := filepath.Join(outside, "库外标记.txt")
	if err := os.WriteFile(marker, markerBytes, 0o600); err != nil {
		t.Fatalf("plant the outside marker: %v", err)
	}
	// A second marker beside the source: inside the library, outside the granted
	// directory, so a walk that knew only "under the library" would still see it.
	beside := filepath.Join(lane.library, "旁置")
	if err := os.MkdirAll(beside, 0o700); err != nil {
		t.Fatalf("seed the beside directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(beside, "旁置标记.txt"), markerBytes, 0o600); err != nil {
		t.Fatalf("plant the beside marker: %v", err)
	}
	source := productionLegacyImportSeed(t, lane.library, productionLegacyImportFixtureSource)
	leak := filepath.Join(source, "泄露.txt")
	if err := os.Symlink(marker, leak); err != nil {
		t.Fatalf("plant the escaping symlink: %v", err)
	}
	before := productionCloudBackupTreeDigest(t, lane.library)
	copyPath := filepath.Join(lane.library, productionLegacyImportFixtureCopy)

	sourceGrant := productionLegacyImportPick(t, lane.client, productionPickerLegacyName, productionLegacyImportFixtureSource)
	targetGrant := productionLegacyImportPick(t, lane.client, productionPickerRestoreTargetName, productionLegacyImportFixtureCopy)
	// A symlink is refused by Lstat before it is ever opened, so the outside
	// marker's bytes are never read, let alone written.
	productionLegacyImportRefused(t, lane.invoke(t, lane.client, sourceGrant, targetGrant),
		productionLegacyImportUnsafeCode)
	if after := productionCloudBackupTreeDigest(t, lane.library); after != before {
		t.Fatalf("the symlink refusal changed the library tree")
	}
	productionLegacyImportAbsent(t, copyPath)

	// The same two grants, once the source is a tree this lane may copy.
	if err := os.Remove(leak); err != nil {
		t.Fatalf("remove the escaping symlink: %v", err)
	}
	sourceDigest := productionCloudBackupTreeDigest(t, source)
	sourceNames := productionLegacyImportNames(t, source)
	answer := lane.invoke(t, lane.client, sourceGrant, targetGrant)
	if answer.State != productionLegacyImportReadyState || answer.Code != "" {
		t.Fatalf("the second import answered state %v code %v", answer.State, answer.Code)
	}
	if copiedDigest := productionCloudBackupTreeDigest(t, copyPath); copiedDigest != sourceDigest {
		t.Fatalf("the copy is not the source's bytes: %v became %v", sourceDigest, copiedDigest)
	}
	if copiedNames := productionLegacyImportNames(t, copyPath); strings.Join(copiedNames, ",") != strings.Join(sourceNames, ",") {
		t.Fatalf("the copy carries names the source does not: %v", strings.Join(copiedNames, ","))
	}
	if productionLegacyImportCarries(t, copyPath, markerBytes) {
		t.Fatalf("the copy carries the marker's bytes, so the walk read outside the granted directory")
	}
	productionLegacyImportAbsent(t, filepath.Join(copyPath, "泄露.txt"))
	productionLegacyImportAbsent(t, filepath.Join(copyPath, "旁置标记.txt"))
}

// TestProductionLegacyImportRefusesGrantOutsideTheRoots proves a grant is only a
// symbolic root plus a relative path: a token naming a tree this request's
// allowlist does not hold is refused by name, never opened.
func TestProductionLegacyImportRefusesGrantOutsideTheRoots(t *testing.T) {
	lane := newProductionLegacyImportLane(t)
	offshore := t.TempDir()
	productionLegacyImportSeed(t, offshore, "库外旧项目")
	before := productionCloudBackupTreeDigest(t, lane.library)
	copyPath := filepath.Join(lane.library, productionLegacyImportFixtureCopy)

	// A target the registry can mint - it is well formed and bound to this caller -
	// whose root is this lane's allowlist does not have.
	outsideTarget := librarypath.Target{
		RootID:   "offshore",
		Kind:     librarypath.KindLibrary,
		Root:     offshore,
		Relative: "库外旧项目",
		Path:     filepath.Join(offshore, "库外旧项目"),
	}
	outsideGrant := productionLegacyImportMint(t, lane.client, outsideTarget)
	targetGrant := productionLegacyImportPick(t, lane.client, productionPickerRestoreTargetName, productionLegacyImportFixtureCopy)
	productionLegacyImportRefused(t, lane.invoke(t, lane.client, outsideGrant, targetGrant),
		productionLegacyImportPathInvalidCode)

	if after := productionCloudBackupTreeDigest(t, lane.library); after != before {
		t.Fatalf("the outside-root refusal changed the library tree")
	}
	productionLegacyImportAbsent(t, copyPath)
}

// TestProductionLegacyImportBinderIsWired pins the one line that serves the name,
// the handler behind it, and the reference it is a port of. CI formats the source
// in place, so every assertion is compacted text (productionSeamContains) rather
// than a line number or a byte-for-byte match.
func TestProductionLegacyImportBinderIsWired(t *testing.T) {
	const binder = `if err := server.Bind(productionPortabilityLegacyCopyName, b.importLegacyCopy); err != nil {`
	portability := productionLegacyImportReadSource(t, "production_project_portability_linux.go")
	if !productionSeamContains(portability, binder) {
		t.Fatalf("the binder no longer installs the legacy copy: %s",
			productionSeamDiagnosis(portability, binder, 0))
	}
	handler := productionLegacyImportReadSource(t, "production_project_import_legacy_linux.go")
	for _, marker := range []string{
		"func (b *productionPortability) importLegacyCopy(ctx context.Context, caller transport.Caller, args []any) (any, error) {",
		"roots, binding, rootsErr := b.grants.roots(ctx, caller)",
		"source, sourceErr := roots.Resolve(sourceGrant.Target.RootID, sourceGrant.Target.Relative)",
		"if copied != before {",
		"return deny(productionLegacyImportFail(productionLegacyImportCopyChangedCode))",
	} {
		if !productionSeamContains(handler, marker) {
			t.Fatalf("the served handler no longer contains %v, so this binder is guessing", marker)
		}
	}
	if _, held := productionProjectPortabilityUnbound()[productionPortabilityLegacyCopyName]; held {
		t.Fatalf("%v is served but still reported unbound", productionPortabilityLegacyCopyName)
	}
	// The reference, so a change on the desktop side is a failing test and not a
	// silently stale port.
	controller := productionLegacyImportReadSource(t, "../../../electron/controllers/project-controller.ts")
	for _, marker := range []string{
		"'project:import-legacy-copy'",
		"sourceRoot = externalFileGrants.resolveDirectoryPath({",
		"targetRoot = externalFileGrants.resolveExactPath({",
		"code: 'LEGACY_IMPORT_PATH_INVALID'",
	} {
		if !productionSeamContains(controller, marker) {
			t.Fatalf("the controller no longer says %v, so this port is quoting nothing", marker)
		}
	}
	service := productionLegacyImportReadSource(t, "../../../electron/services/legacy-project-copy-import.ts")
	for _, marker := range []string{
		"if (exists(targetRoot)) fail('LEGACY_IMPORT_TARGET_EXISTS')",
		"fail('LEGACY_IMPORT_UNSUPPORTED_SOURCE')",
		"if (treeHash(sourceRoot) !== before) fail('LEGACY_IMPORT_SOURCE_CHANGED')",
		"fs.renameSync(builtRoot, targetRoot)",
	} {
		if !productionSeamContains(service, marker) {
			t.Fatalf("the copy service no longer says %v, so this port is quoting nothing", marker)
		}
	}
}

// productionLegacyImportReadSource reads one source file for a seam assertion.
func productionLegacyImportReadSource(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v: %v", path, err)
	}
	return string(raw)
}
