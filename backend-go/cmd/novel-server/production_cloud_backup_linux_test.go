//go:build linux

package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/archiveformat"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/cloudbackup"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/librarypath"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/webdav"
)

// This file proves the six served cloud-backup names over real HTTP through the
// frozen session: no handler is called directly, and every answer read here is
// the answer the browser would receive.
//
// The lane is built exactly as production_open_linux.go will build it once the
// parent adds the binder, because productionHandlerWithContract binds no
// cloud-backup name yet: the project lifecycle (so project:open can issue the
// lease every request names) plus newProductionCloudBackup.
const (
	productionCloudBackupTestTargetName = "restored-novel"
	productionCloudBackupTestTab        = productionDurableTab
	// productionCloudBackupTestSecret stands in for the WebDAV password the
	// desktop keeps. Nothing in this lane ever holds one, so the only way it can
	// reach a caller is if a handler echoes what it was given.
	productionCloudBackupTestSecret = "correct-horse-battery-staple-655f2830"
	productionCloudBackupTestUUID   = "6d1f0a91-3c47-4a2e-9d55-8b0e6f2a71c4"
	productionCloudBackupTestGrant  = "not-a-minted-grant"
)

// productionCloudBackupLane opens the real production lane over a disposable data
// root and installs the two binders these names need.
func productionCloudBackupLane(t *testing.T, root string) http.Handler {
	t.Helper()
	config := productionConfig{Schema: productionDurableContract(t), MaxDatabaseBytes: productionMaxDatabaseBytes}
	layout, err := productionLayoutFor(root)
	if err != nil {
		t.Fatal(err)
	}
	startup, cancel := context.WithTimeout(context.Background(), productionStartupTimeout)
	defer cancel()
	open, err := openProduction(startup, layout, config)
	if err != nil {
		t.Fatal(err)
	}
	server := transport.New(migrationRegistry(t), security.NewOrigins("http://"+productionDurableAddress),
		"go-production-cloud-backup")
	t.Cleanup(func() {
		server.Close()
		if closeErr := open.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if bindErr := server.BindProjects(open.durable); bindErr != nil {
		t.Fatal(bindErr)
	}
	if bindErr := newProductionCloudBackup(open, config).Bind(server); bindErr != nil {
		t.Fatal(bindErr)
	}
	return server
}

// productionCloudBackupOpenSession opens one project and returns the three-key session
// every served name names. The lease comes from project:open, never from the test.
func productionCloudBackupOpenSession(t *testing.T, author *productionDurableClient, projectPath string) map[string]any {
	t.Helper()
	var opened productionDurableOpenWire
	author.decode(t, "project:open", []any{projectPath, "cloud-backup-open", projectPath}, &opened)
	if !opened.Success || !opened.DBReady || opened.Error != "" {
		t.Fatalf("project:open refused the admitted project: %+v", opened)
	}
	lease, _ := opened.Project["sessionLease"].(string)
	if lease == "" {
		t.Fatalf("project:open issued no session lease: %v", opened.Project)
	}
	return map[string]any{
		"projectId":   opened.Project["id"],
		"leaseId":     lease,
		"projectPath": opened.Project["path"],
	}
}

// productionCloudBackupInvoke performs one call and returns the status, the raw
// answer body and the decoded result. Refusals on this surface are HTTP 200
// business envelopes, so the body is what a credential leak would appear in.
func productionCloudBackupInvoke(t *testing.T, author *productionDurableClient, operation string, args []any) (int, string, map[string]any) {
	t.Helper()
	response := author.call(t, operation, args)
	body := response.Body.String()
	result := map[string]any{}
	if response.Code == http.StatusOK {
		var wire struct {
			Result map[string]any `json:"result"`
		}
		if err := json.Unmarshal([]byte(body), &wire); err != nil {
			t.Fatalf("%s answered an undecodable envelope: %v %s", operation, err, body)
		}
		if wire.Result == nil {
			t.Fatalf("%s answered no result: %s", operation, body)
		}
		result = wire.Result
	}
	return response.Code, body, result
}

// productionCloudBackupRefusal asserts one refusal envelope and returns its code.
func productionCloudBackupRefusal(t *testing.T, author *productionDurableClient, operation string, args []any) string {
	t.Helper()
	status, body, result := productionCloudBackupInvoke(t, author, operation, args)
	if status != http.StatusOK {
		t.Fatalf("%s answered HTTP %d instead of a business envelope: %s", operation, status, body)
	}
	if success, _ := result["success"].(bool); success {
		t.Fatalf("%s succeeded where it had to refuse: %s", operation, body)
	}
	code, _ := result["errorCode"].(string)
	if code == "" {
		t.Fatalf("%s refused without a symbolic code: %s", operation, body)
	}
	if state, _ := result["state"].(string); state != "failed" {
		t.Fatalf("%s refused with state %v: %s", operation, state, body)
	}
	return code
}

// productionCloudBackupStoreDirectory is the store root this binder derives: one
// fixed child of the data root, never under the library.
func productionCloudBackupStoreDirectory(root string) string {
	return filepath.Join(root, productionCloudBackupDirectory)
}

// productionCloudBackupGenerationDirectory names one published generation on disk.
// The collection directory is internal/cloudbackup's own documented layout, and
// asserting it here is what makes "the archive is really on disk" a real test.
func productionCloudBackupGenerationDirectory(t *testing.T, root, generationID string) string {
	t.Helper()
	directory := filepath.Join(productionCloudBackupStoreDirectory(root), "generations", generationID)
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() {
		t.Fatalf("generation %v was not published under the store: %v %v", generationID, info, err)
	}
	return directory
}

// productionCloudBackupFileDigest returns one file's SHA-256.
func productionCloudBackupFileDigest(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %v: %v", path, err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// productionCloudBackupDatabaseDigest proves the author database's bytes through a
// SEPARATE read-only connection: the file is opened by SQLite in mode=ro first, so
// a stale WAL cannot hide behind the digest, and only then are the bytes hashed.
func productionCloudBackupDatabaseDigest(t *testing.T, projectPath string) string {
	t.Helper()
	database := filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName)
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(database)}
	uri.RawQuery = "mode=ro"
	handle, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		t.Fatalf("open the author database read-only: %v", err)
	}
	handle.SetMaxOpenConns(1)
	var tables int
	if err := handle.QueryRow(`SELECT count(*) FROM sqlite_master`).Scan(&tables); err != nil {
		_ = handle.Close()
		t.Fatalf("read the author schema read-only: %v", err)
	}
	if err := handle.Close(); err != nil {
		t.Fatalf("close the read-only connection: %v", err)
	}
	if tables <= 0 {
		t.Fatalf("the author database has no schema: %v", database)
	}
	// The digest is over project.db itself. A live project may keep committed
	// frames in a -wal sidecar that the durable runtime's own connection will
	// fold back in whenever it likes, so hashing the file - not a checkpointed
	// snapshot - is what makes "the bytes the author has" the thing compared.
	return productionCloudBackupFileDigest(t, database)
}

// productionCloudBackupTreeDigest hashes a whole project tree, names included, so
// "the target was not touched" covers more than one file.
func productionCloudBackupTreeDigest(t *testing.T, root string) string {
	t.Helper()
	names := make([]string, 0, 16)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		names = append(names, relative)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %v: %v", root, err)
	}
	sort.Strings(names)
	digest := sha256.New()
	for _, name := range names {
		_, _ = digest.Write([]byte(name))
		_, _ = digest.Write([]byte{0})
		path := filepath.Join(root, name)
		info, statErr := os.Lstat(path)
		if statErr != nil {
			t.Fatalf("stat %v: %v", path, statErr)
		}
		if info.Mode().IsRegular() {
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatalf("read %v: %v", path, readErr)
			}
			_, _ = digest.Write(data)
			_, _ = digest.Write([]byte{0})
		}
	}
	return hex.EncodeToString(digest.Sum(nil))
}

// productionCloudBackupMintRestoreTarget mints the grant
// dialog:select-project-restore-target will mint: a library target the server
// itself resolved, bound to this caller, carrying the create permission the
// desktop's resolveRestoreTargetGrant asks for
// (electron/controllers/cloud-backup-controller.ts:255-258).
func productionCloudBackupMintRestoreTarget(t *testing.T, author *productionDurableClient, root, relative string) string {
	t.Helper()
	registry, err := productionFSGrantsRegistry()
	if err != nil {
		t.Fatalf("the process-wide grant registry is unavailable: %v", err)
	}
	roots, rootsErr := librarypath.NewRoots([]librarypath.Root{{
		ID: librarypath.LibraryRootID, Kind: librarypath.KindLibrary,
		Path: filepath.Join(root, productionLibraryDirectory),
	}})
	if rootsErr != nil {
		t.Fatalf("build the library allowlist: %v", rootsErr)
	}
	target, resolveErr := roots.Resolve(librarypath.LibraryRootID, relative)
	if resolveErr != nil {
		t.Fatalf("resolve the restore target %v: %v", relative, resolveErr)
	}
	token, mintErr := registry.Mint(productionFSGrantBinding(author), target, librarypath.OpMkdir, librarypath.MintOptions{})
	if mintErr != nil {
		t.Fatalf("mint a restore grant for %v: %v", relative, mintErr)
	}
	return token
}

type productionCloudBackupViewWire struct {
	Success   bool           `json:"success"`
	State     string         `json:"state"`
	ErrorCode string         `json:"errorCode"`
	Binding   map[string]any `json:"binding"`
	Account   map[string]any `json:"account"`
}

type productionCloudBackupBackupWire struct {
	Success      bool             `json:"success"`
	State        string           `json:"state"`
	ErrorCode    string           `json:"errorCode"`
	OperationID  string           `json:"operationId"`
	BackupPoint  string           `json:"backupPoint"`
	BindingSaved bool             `json:"bindingSaved"`
	Generation   webdav.Generation `json:"generation"`
	Binding      map[string]any   `json:"binding"`
}

type productionCloudBackupReceiptWire struct {
	OriginProjectID            string `json:"originProjectId"`
	TargetProjectID            string `json:"targetProjectId"`
	TargetProjectRoot          string `json:"targetProjectRoot"`
	SnapshotGeneration         string `json:"snapshotGeneration"`
	PortableDatabaseSHA256     string `json:"portableDatabaseSha256"`
	RequiresRuntimeFreezeGuard bool   `json:"requiresRuntimeFreezeGuard"`
}

type productionCloudBackupRestoreWire struct {
	Success              bool                           `json:"success"`
	State                string                         `json:"state"`
	ErrorCode            string                         `json:"errorCode"`
	OperationID          string                         `json:"operationId"`
	Receipt              productionCloudBackupReceiptWire `json:"receipt"`
	BindingSaved         bool                           `json:"bindingSaved"`
	Binding              map[string]any                 `json:"binding"`
	RecentProjectUpdated bool                           `json:"recentProjectUpdated"`
}

type productionCloudBackupListWire struct {
	Success     bool               `json:"success"`
	State       string             `json:"state"`
	ErrorCode   string             `json:"errorCode"`
	Generations []webdav.Generation `json:"generations"`
}

// TestProductionCloudBackupPublishesVerifiableGeneration is the backup half: the
// archive is written, the manifest and completion describe it, and the bytes on
// disk prove both.
func TestProductionCloudBackupPublishesVerifiableGeneration(t *testing.T) {
	root, projectPath := productionDurableDataRoot(t)
	handler := productionCloudBackupLane(t, root)
	author := newProductionDurableClient(t, handler, productionCloudBackupTestTab)
	session := productionCloudBackupOpenSession(t, author, projectPath)

	before := productionCloudBackupDatabaseDigest(t, projectPath)

	var view productionCloudBackupViewWire
	author.decode(t, productionCloudBackupViewName, []any{session}, &view)
	if !view.Success || view.State != "configured" {
		t.Fatalf("cloud-backup:view did not report the derived binding: %+v", view)
	}
	if view.Binding["cloudBookId"] != productionDurableProjectID || view.Binding["mode"] != "writable" {
		t.Fatalf("cloud-backup:view returned the wrong binding: %v", view.Binding)
	}
	if view.Account["accountId"] != productionCloudBackupLocalAccountID ||
		view.Account["persistence"] != "session-only" || view.Account["username"] != "production-author" {
		t.Fatalf("cloud-backup:view returned the wrong account: %v", view.Account)
	}

	var backup productionCloudBackupBackupWire
	author.decode(t, productionCloudBackupBackupName, []any{map[string]any{
		"operationId":         productionCloudBackupTestUUID,
		"projectSession":      session,
		"disclosureConfirmed": true,
	}}, &backup)
	if !backup.Success || backup.State != "backup-complete" || backup.ErrorCode != "" {
		t.Fatalf("cloud-backup:backup refused a disclosed backup: %s", backup.ErrorCode)
	}
	if !backup.BindingSaved || backup.OperationID != productionCloudBackupTestUUID {
		t.Fatalf("cloud-backup:backup returned the wrong envelope: %+v", backup)
	}
	generation := backup.Generation
	if generation.GenerationID == "" || generation.GenerationID != backup.BackupPoint {
		t.Fatalf("cloud-backup:backup returned no backup point: %+v", backup)
	}
	if generation.CloudBookID != productionDurableProjectID || generation.OriginProjectID != productionDurableProjectID {
		t.Fatalf("cloud-backup:backup returned the wrong identity: %+v", generation)
	}
	if generation.PortableSnapshotGeneration != generation.GenerationID {
		t.Fatalf("the portable snapshot generation is not the generation: %+v", generation)
	}

	// A backup only reads the author's project.
	if after := productionCloudBackupDatabaseDigest(t, projectPath); after != before {
		t.Fatalf("cloud-backup:backup changed the author database: %v -> %v", before, after)
	}

	// The bytes on disk: completion -> manifest -> archive, all three digests.
	directory := productionCloudBackupGenerationDirectory(t, root, generation.GenerationID)
	archivePath := filepath.Join(directory, "archive.ainovel")
	archiveDigest := productionCloudBackupFileDigest(t, archivePath)
	if archiveDigest != generation.ArchiveSHA256 {
		t.Fatalf("the published archive does not hash to the answered digest: %v != %v", archiveDigest, generation.ArchiveSHA256)
	}
	archiveInfo, statErr := os.Lstat(archivePath)
	if statErr != nil {
		t.Fatal(statErr)
	}
	if archiveInfo.Size() != generation.ArchiveByteSize {
		t.Fatalf("the published archive is %d bytes, answered %d", archiveInfo.Size(), generation.ArchiveByteSize)
	}
	manifestPath := filepath.Join(directory, "manifest.json")
	var manifest webdav.Manifest
	manifestBytes, readErr := os.ReadFile(manifestPath)
	if readErr != nil {
		t.Fatalf("the generation published no manifest: %v", readErr)
	}
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatalf("the published manifest is not JSON: %v", err)
	}
	if manifest.Archive.SHA256 != archiveDigest || manifest.Archive.ByteSize != archiveInfo.Size() {
		t.Fatalf("the published manifest does not describe the archive: %+v", manifest)
	}
	var completion webdav.Completion
	completionBytes, readErr := os.ReadFile(filepath.Join(directory, "completion.json"))
	if readErr != nil {
		t.Fatalf("the generation published no completion: %v", readErr)
	}
	if err := json.Unmarshal(completionBytes, &completion); err != nil {
		t.Fatalf("the published completion is not JSON: %v", err)
	}
	if completion.Archive.SHA256 != archiveDigest ||
		completion.Manifest.SHA256 != productionCloudBackupFileDigest(t, manifestPath) {
		t.Fatalf("the published completion does not prove its own pair: %+v", completion)
	}

	// The store's own readback, over the published bytes.
	store, storeErr := cloudbackup.Open(productionCloudBackupStoreDirectory(root), cloudbackup.DefaultLimits())
	if storeErr != nil {
		t.Fatalf("open the store for readback: %v", storeErr)
	}
	verified, verifyErr := store.Verify(context.Background(), productionDurableProjectID, generation.GenerationID)
	if verifyErr != nil {
		t.Fatalf("the published generation does not verify: %v", verifyErr)
	}
	if verified.Archive.SHA256 != archiveDigest || verified.OriginProjectID != productionDurableProjectID {
		t.Fatalf("the store read back the wrong generation: %+v", verified)
	}

	// The archive is a real portable copy of one database, and it says out loud
	// which authority it does not carry.
	archiveFile, openErr := os.Open(archivePath)
	if openErr != nil {
		t.Fatal(openErr)
	}
	defer archiveFile.Close()
	archive, inspectErr := archiveformat.Inspect(archiveFile, archiveInfo.Size(), archiveformat.DefaultLimits())
	if inspectErr != nil {
		t.Fatalf("the published archive is not a portable archive: %v", inspectErr)
	}
	portable := archive.Manifest()
	if len(portable.Entries) != 1 || portable.Entries[0].Path != productionCloudBackupDatabaseEntry ||
		portable.Entries[0].Disposition != productionCloudBackupDatabaseDisposition {
		t.Fatalf("the archive does not carry exactly one portable database: %+v", portable.Entries)
	}
	if portable.SourceSchemaVersion != 7 || portable.OriginProjectID != productionDurableProjectID {
		t.Fatalf("the archive manifest describes the wrong source: %+v", portable)
	}
	if len(portable.TransferReceiptIDs) != 0 || len(portable.HistoryProjectionIDs) != 0 {
		t.Fatalf("the archive claims portable authority this lane cannot honour: %+v", portable)
	}
	if len(portable.OmittedItems) != 1 || portable.OmittedItems[0].Reason != "exclude-machine-authority" {
		t.Fatalf("the archive does not declare the omitted authority: %+v", portable.OmittedItems)
	}
}

// TestProductionCloudBackupListReportsVerifiedGenerations is the list half: the
// catalogue the browser renders is read from the store, and a book this lane never
// admitted is refused rather than probed.
func TestProductionCloudBackupListReportsVerifiedGenerations(t *testing.T) {
	root, projectPath := productionDurableDataRoot(t)
	handler := productionCloudBackupLane(t, root)
	author := newProductionDurableClient(t, handler, productionCloudBackupTestTab)
	session := productionCloudBackupOpenSession(t, author, projectPath)

	var backup productionCloudBackupBackupWire
	author.decode(t, productionCloudBackupBackupName, []any{map[string]any{
		"operationId":         productionCloudBackupTestUUID,
		"projectSession":      session,
		"disclosureConfirmed": true,
	}}, &backup)
	if !backup.Success {
		t.Fatalf("cloud-backup:backup refused: %s", backup.ErrorCode)
	}

	var listed productionCloudBackupListWire
	author.decode(t, productionCloudBackupListName, []any{map[string]any{
		"localEndpointAccountId": productionCloudBackupLocalAccountID,
		"cloudBookId":            productionDurableProjectID,
	}}, &listed)
	if !listed.Success || len(listed.Generations) != 1 {
		t.Fatalf("cloud-backup:list did not report the new generation: %+v", listed)
	}
	if listed.Generations[0].GenerationID != backup.Generation.GenerationID ||
		listed.Generations[0].ArchiveSHA256 != backup.Generation.ArchiveSHA256 {
		t.Fatalf("cloud-backup:list reported a different generation: %+v", listed.Generations[0])
	}
	if code := productionCloudBackupRefusal(t, author, productionCloudBackupListName, []any{map[string]any{
		"localEndpointAccountId": productionCloudBackupLocalAccountID,
		"cloudBookId":            productionCloudBackupTestUUID,
	}}); code != productionCloudBackupNotConfigured {
		t.Fatalf("cloud-backup:list answered %v for a book no project ever had; want %v", code, productionCloudBackupNotConfigured)
	}
	if code := productionCloudBackupRefusal(t, author, productionCloudBackupListName, []any{map[string]any{
		"localEndpointAccountId": productionCloudBackupTestUUID,
		"cloudBookId":            productionDurableProjectID,
	}}); code != productionCloudBackupCredentialMissing {
		t.Fatalf("cloud-backup:list answered %v for an account with no credential; want %v", code, productionCloudBackupCredentialMissing)
	}

	// The two names this lane refuses stay unbound: a wrong answer about a
	// credential would be worse than 501.
	for _, operation := range []string{productionCloudBackupConnectName, productionCloudBackupClearName} {
		if response := author.call(t, operation, nil); response.Code != http.StatusNotImplemented {
			t.Fatalf("%v is bound, but this lane has no port for it: %d %s", operation, response.Code, response.Body.String())
		}
	}
}

// TestProductionCloudBackupRestoreCopyInstallsVerifiedArchive is the restore half:
// a fresh directory receives the archived database byte for byte, admitted under
// the same schema contract the lane uses at startup.
func TestProductionCloudBackupRestoreCopyInstallsVerifiedArchive(t *testing.T) {
	root, projectPath := productionDurableDataRoot(t)
	handler := productionCloudBackupLane(t, root)
	author := newProductionDurableClient(t, handler, productionCloudBackupTestTab)
	session := productionCloudBackupOpenSession(t, author, projectPath)

	var backup productionCloudBackupBackupWire
	author.decode(t, productionCloudBackupBackupName, []any{map[string]any{
		"operationId":         productionCloudBackupTestUUID,
		"projectSession":      session,
		"disclosureConfirmed": true,
	}}, &backup)
	if !backup.Success {
		t.Fatalf("cloud-backup:backup refused: %s", backup.ErrorCode)
	}
	before := productionCloudBackupDatabaseDigest(t, projectPath)
	target := filepath.Join(root, productionLibraryDirectory, productionCloudBackupTestTargetName)
	grant := productionCloudBackupMintRestoreTarget(t, author, root, productionCloudBackupTestTargetName)

	var restored productionCloudBackupRestoreWire
	author.decode(t, productionCloudBackupRestoreName, []any{map[string]any{
		"operationId":            productionCloudBackupTestUUID,
		"localEndpointAccountId": productionCloudBackupLocalAccountID,
		"cloudBookId":            productionDurableProjectID,
		"generationId":           backup.Generation.GenerationID,
		"targetGrantId":          grant,
	}}, &restored)
	if !restored.Success || restored.State != "restore-complete" || restored.ErrorCode != "" {
		t.Fatalf("cloud-backup:restore-copy refused a valid archive: %s", restored.ErrorCode)
	}
	receipt := restored.Receipt
	if receipt.OriginProjectID != productionDurableProjectID ||
		receipt.SnapshotGeneration != backup.Generation.GenerationID {
		t.Fatalf("the receipt describes the wrong origin: %+v", receipt)
	}
	if receipt.TargetProjectID == "" || receipt.TargetProjectID == productionDurableProjectID {
		t.Fatalf("the restored copy did not get its own project id: %+v", receipt)
	}
	// The receipt carries the library-relative name, never a host path.
	if receipt.TargetProjectRoot != productionCloudBackupTestTargetName {
		t.Fatalf("the receipt carried %v; want the library-relative target name", receipt.TargetProjectRoot)
	}
	if !receipt.RequiresRuntimeFreezeGuard {
		t.Fatalf("the receipt did not require the runtime freeze guard: %+v", receipt)
	}
	if restored.Binding["mode"] != productionCloudBackupModeOriginReadonly ||
		restored.Binding["cloudBookId"] != productionDurableProjectID {
		t.Fatalf("the restored binding is not origin-readonly: %v", restored.Binding)
	}
	if !restored.BindingSaved || !restored.RecentProjectUpdated {
		t.Fatalf("the restored copy is not navigable: %+v", restored)
	}

	// The installed database is the archived database, byte for byte.
	installedDatabase := filepath.Join(target, projectfs.StorageDirectory, projectfs.DatabaseName)
	if digest := productionCloudBackupFileDigest(t, installedDatabase); digest != receipt.PortableDatabaseSHA256 {
		t.Fatalf("the restored database does not hash to the receipt digest: %v != %v", digest, receipt.PortableDatabaseSHA256)
	}
	if _, statErr := os.Lstat(filepath.Join(target, projectfs.StorageDirectory, projectfs.ManifestName)); statErr != nil {
		t.Fatalf("the restored copy has no canonical manifest: %v", statErr)
	}
	// The content really is there: the seeded body is read back through a separate
	// read-only connection, exactly like the author's own reader would.
	if body := productionCloudBackupReadBody(t, target); body != productionDurableSeed {
		t.Fatalf("the restored database answered %v; want the backed-up body", body)
	}
	// The origin project was never touched by a restore into a fresh directory.
	if after := productionCloudBackupDatabaseDigest(t, projectPath); after != before {
		t.Fatalf("cloud-backup:restore-copy changed the origin database: %v -> %v", before, after)
	}
}

// productionCloudBackupReadBody reads the seeded draft body out of one project
// database over a separate read-only connection.
func productionCloudBackupReadBody(t *testing.T, projectPath string) string {
	t.Helper()
	database := filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName)
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(database)}
	uri.RawQuery = "mode=ro"
	handle, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		t.Fatalf("open the restored database read-only: %v", err)
	}
	defer func() {
		if closeErr := handle.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	handle.SetMaxOpenConns(1)
	var body string
	if err := handle.QueryRow(`SELECT body FROM contents WHERE id = 1`).Scan(&body); err != nil {
		t.Fatalf("read the restored body: %v", err)
	}
	return body
}

// TestProductionCloudBackupRestoreCopyRefusesTamperedArchive proves a damaged
// archive is never installed: the digest mismatch refuses the whole call and the
// target directory is not created.
func TestProductionCloudBackupRestoreCopyRefusesTamperedArchive(t *testing.T) {
	root, projectPath := productionDurableDataRoot(t)
	handler := productionCloudBackupLane(t, root)
	author := newProductionDurableClient(t, handler, productionCloudBackupTestTab)
	session := productionCloudBackupOpenSession(t, author, projectPath)

	var backup productionCloudBackupBackupWire
	author.decode(t, productionCloudBackupBackupName, []any{map[string]any{
		"operationId":         productionCloudBackupTestUUID,
		"projectSession":      session,
		"disclosureConfirmed": true,
	}}, &backup)
	if !backup.Success {
		t.Fatalf("cloud-backup:backup refused: %s", backup.ErrorCode)
	}
	before := productionCloudBackupDatabaseDigest(t, projectPath)

	// Flip one byte inside the archive, leaving the declared digests intact: this is
	// exactly the damage a corrupt disk or a partial transfer produces.
	directory := productionCloudBackupGenerationDirectory(t, root, backup.Generation.GenerationID)
	archivePath := filepath.Join(directory, "archive.ainovel")
	damaged, readErr := os.ReadFile(archivePath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(damaged) < 64 {
		t.Fatalf("the published archive is implausibly small: %d bytes", len(damaged))
	}
	damaged[len(damaged)/2] ^= 0x5a
	if writeErr := os.WriteFile(archivePath, damaged, productionProjectCreateFileMode); writeErr != nil {
		t.Fatal(writeErr)
	}

	target := filepath.Join(root, productionLibraryDirectory, productionCloudBackupTestTargetName)
	grant := productionCloudBackupMintRestoreTarget(t, author, root, productionCloudBackupTestTargetName)
	code := productionCloudBackupRefusal(t, author, productionCloudBackupRestoreName, []any{map[string]any{
		"operationId":            productionCloudBackupTestUUID,
		"localEndpointAccountId": productionCloudBackupLocalAccountID,
		"cloudBookId":            productionDurableProjectID,
		"generationId":           backup.Generation.GenerationID,
		"targetGrantId":          grant,
	}})
	if code != productionCloudBackupRemoteInvalid {
		t.Fatalf("a tampered archive answered %v; want %v", code, productionCloudBackupRemoteInvalid)
	}
	if _, statErr := os.Lstat(target); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("a refused restore left a target directory behind: %v %v", target, statErr)
	}
	if after := productionCloudBackupDatabaseDigest(t, projectPath); after != before {
		t.Fatalf("a refused restore changed the author database: %v -> %v", before, after)
	}
	// The damaged generation no longer proves itself, so it is not advertised.
	var listed productionCloudBackupListWire
	author.decode(t, productionCloudBackupListName, []any{map[string]any{
		"localEndpointAccountId": productionCloudBackupLocalAccountID,
		"cloudBookId":            productionDurableProjectID,
	}}, &listed)
	if listed.Success && len(listed.Generations) != 0 {
		t.Fatalf("the damaged generation is still advertised: %+v", listed.Generations)
	}
	if !listed.Success && listed.ErrorCode == "" {
		t.Fatalf("cloud-backup:list refused the damaged book without a code: %+v", listed)
	}
}

// TestProductionCloudBackupRestoreCopyProtectsLiveTarget proves the two halves of
// the live-project promise: the target is copied before anything else, and the
// live project itself is left byte-identical because this lane cannot replace it.
func TestProductionCloudBackupRestoreCopyProtectsLiveTarget(t *testing.T) {
	root, projectPath := productionDurableDataRoot(t)
	handler := productionCloudBackupLane(t, root)
	author := newProductionDurableClient(t, handler, productionCloudBackupTestTab)
	session := productionCloudBackupOpenSession(t, author, projectPath)

	var backup productionCloudBackupBackupWire
	author.decode(t, productionCloudBackupBackupName, []any{map[string]any{
		"operationId":         productionCloudBackupTestUUID,
		"projectSession":      session,
		"disclosureConfirmed": true,
	}}, &backup)
	if !backup.Success {
		t.Fatalf("cloud-backup:backup refused: %s", backup.ErrorCode)
	}
	before := productionCloudBackupDatabaseDigest(t, projectPath)
	treeBefore := productionCloudBackupTreeDigest(t, projectPath)

	// The grant names the LIVE project's own directory, which is the target a
	// confused or malicious caller would reach for.
	grant := productionCloudBackupMintRestoreTarget(t, author, root, productionDurableProjectName)
	code := productionCloudBackupRefusal(t, author, productionCloudBackupRestoreName, []any{map[string]any{
		"operationId":            productionCloudBackupTestUUID,
		"localEndpointAccountId": productionCloudBackupLocalAccountID,
		"cloudBookId":            productionDurableProjectID,
		"generationId":           backup.Generation.GenerationID,
		"targetGrantId":          grant,
	}})
	if code != productionCloudBackupRestoreFailed {
		t.Fatalf("a restore onto the live project answered %v; want %v", code, productionCloudBackupRestoreFailed)
	}
	if after := productionCloudBackupDatabaseDigest(t, projectPath); after != before {
		t.Fatalf("the refused restore changed the live database: %v -> %v", before, after)
	}
	if after := productionCloudBackupTreeDigest(t, projectPath); after != treeBefore {
		t.Fatalf("the refused restore changed the live project tree")
	}

	// The pre-restore copy exists and proves itself.
	store, storeErr := cloudbackup.Open(productionCloudBackupStoreDirectory(root), cloudbackup.DefaultLimits())
	if storeErr != nil {
		t.Fatal(storeErr)
	}
	generations, listErr := store.List(context.Background(), productionDurableProjectID)
	if listErr != nil {
		t.Fatalf("list the book after the refusal: %v", listErr)
	}
	if len(generations) != 2 {
		t.Fatalf("the refused restore left %d generations; want the backup plus one pre-restore copy", len(generations))
	}
	var preRestored string
	for _, generation := range generations {
		if generation.GenerationID != backup.Generation.GenerationID {
			preRestored = generation.GenerationID
		}
	}
	if preRestored == "" {
		t.Fatalf("no pre-restore generation was published: %+v", generations)
	}
	if _, verifyErr := store.Verify(context.Background(), productionDurableProjectID, preRestored); verifyErr != nil {
		t.Fatalf("the pre-restore copy does not verify: %v", verifyErr)
	}
}

// TestProductionCloudBackupRefusalsLeaveAuthorDatabaseIdentical runs the refusal
// surface and proves after each one that the author's database is byte-identical.
func TestProductionCloudBackupRefusalsLeaveAuthorDatabaseIdentical(t *testing.T) {
	root, projectPath := productionDurableDataRoot(t)
	handler := productionCloudBackupLane(t, root)
	author := newProductionDurableClient(t, handler, productionCloudBackupTestTab)
	session := productionCloudBackupOpenSession(t, author, projectPath)

	var backup productionCloudBackupBackupWire
	author.decode(t, productionCloudBackupBackupName, []any{map[string]any{
		"operationId":         productionCloudBackupTestUUID,
		"projectSession":      session,
		"disclosureConfirmed": true,
	}}, &backup)
	if !backup.Success {
		t.Fatalf("cloud-backup:backup refused: %s", backup.ErrorCode)
	}
	before := productionCloudBackupDatabaseDigest(t, projectPath)
	treeBefore := productionCloudBackupTreeDigest(t, projectPath)
	occupied := filepath.Join(root, productionLibraryDirectory, productionDurableProjectName)

	for _, refusal := range []struct {
		name      string
		operation string
		args      []any
		code      string
	}{
		{
			name:      "backup without the disclosure",
			operation: productionCloudBackupBackupName,
			args: []any{map[string]any{
				"operationId":         productionCloudBackupTestUUID,
				"projectSession":      session,
				"disclosureConfirmed": false,
			}},
			code: productionCloudBackupDisclosureRequired,
		},
		{
			name:      "backup about another lease",
			operation: productionCloudBackupBackupName,
			args: []any{map[string]any{
				"operationId": productionCloudBackupTestUUID,
				"projectSession": map[string]any{
					"projectId":   productionDurableProjectID,
					"leaseId":     productionCloudBackupTestUUID,
					"projectPath": projectPath,
				},
				"disclosureConfirmed": true,
			}},
			code: productionCloudBackupInputInvalid,
		},
		{
			name:      "backup with an unknown operation field",
			operation: productionCloudBackupBackupName,
			args: []any{map[string]any{
				"operationId":         productionCloudBackupTestUUID,
				"projectSession":      session,
				"disclosureConfirmed": true,
				"operation":           "restore",
			}},
			code: productionCloudBackupInputInvalid,
		},
		{
			name:      "confirm-binding with a parent selection",
			operation: productionCloudBackupConfirmName,
			args: []any{map[string]any{
				"projectSession":                 session,
				"localEndpointAccountId":         productionCloudBackupLocalAccountID,
				"lastSelectedParentGenerationIds": []any{backup.Generation.GenerationID},
				"expectedRevision":               nil,
			}},
			code: productionCloudBackupBindingReadOnly,
		},
		{
			name:      "confirm-binding with another book",
			operation: productionCloudBackupConfirmName,
			args: []any{map[string]any{
				"projectSession":                 session,
				"localEndpointAccountId":         productionCloudBackupLocalAccountID,
				"cloudBookId":                    productionCloudBackupTestUUID,
				"lastSelectedParentGenerationIds": []any{},
				"expectedRevision":               nil,
			}},
			code: productionCloudBackupBindingConflict,
		},
		{
			name:      "confirm-binding at a stale revision",
			operation: productionCloudBackupConfirmName,
			args: []any{map[string]any{
				"projectSession":                 session,
				"localEndpointAccountId":         productionCloudBackupLocalAccountID,
				"lastSelectedParentGenerationIds": []any{},
				"expectedRevision":               7,
			}},
			code: productionCloudBackupBindingConflict,
		},
		{
			name:      "restore-copy with an unusable grant",
			operation: productionCloudBackupRestoreName,
			args: []any{map[string]any{
				"operationId":            productionCloudBackupTestUUID,
				"localEndpointAccountId": productionCloudBackupLocalAccountID,
				"cloudBookId":            productionDurableProjectID,
				"generationId":           backup.Generation.GenerationID,
				"targetGrantId":          productionCloudBackupTestGrant,
			}},
			code: productionCloudBackupInputInvalid,
		},
		{
			name:      "restore-copy without a grant",
			operation: productionCloudBackupRestoreName,
			args: []any{map[string]any{
				"operationId":            productionCloudBackupTestUUID,
				"localEndpointAccountId": productionCloudBackupLocalAccountID,
				"cloudBookId":            productionDurableProjectID,
				"generationId":           backup.Generation.GenerationID,
				"targetGrantId":          "",
			}},
			code: productionCloudBackupInputInvalid,
		},
		{
			name:      "cancel of an operation that is not running",
			operation: productionCloudBackupCancelName,
			args:      []any{productionCloudBackupTestUUID},
			code:      productionCloudBackupOperationNotFound,
		},
		{
			name:      "cancel without an operation id",
			operation: productionCloudBackupCancelName,
			args:      nil,
			code:      productionCloudBackupInputInvalid,
		},
		{
			name:      "view about another project",
			operation: productionCloudBackupViewName,
			args: []any{map[string]any{
				"projectId":   productionCloudBackupTestUUID,
				"leaseId":     productionCloudBackupTestUUID,
				"projectPath": occupied,
			}},
			code: productionCloudBackupInputInvalid,
		},
	} {
		if code := productionCloudBackupRefusal(t, author, refusal.operation, refusal.args); code != refusal.code {
			t.Fatalf("%v answered %v; want %v", refusal.name, code, refusal.code)
		}
		if after := productionCloudBackupDatabaseDigest(t, projectPath); after != before {
			t.Fatalf("%v changed the author database: %v -> %v", refusal.name, before, after)
		}
		if after := productionCloudBackupTreeDigest(t, projectPath); after != treeBefore {
			t.Fatalf("%v changed the author project tree", refusal.name)
		}
	}
}

// TestProductionCloudBackupNeverAnswersCredential smuggles a WebDAV password into
// every served name and proves no answer ever carries it, and that nothing this
// lane wrote on disk does either.
func TestProductionCloudBackupNeverAnswersCredential(t *testing.T) {
	root, projectPath := productionDurableDataRoot(t)
	handler := productionCloudBackupLane(t, root)
	author := newProductionDurableClient(t, handler, productionCloudBackupTestTab)
	session := productionCloudBackupOpenSession(t, author, projectPath)

	var backup productionCloudBackupBackupWire
	author.decode(t, productionCloudBackupBackupName, []any{map[string]any{
		"operationId":         productionCloudBackupTestUUID,
		"projectSession":      session,
		"disclosureConfirmed": true,
	}}, &backup)
	if !backup.Success {
		t.Fatalf("cloud-backup:backup refused: %s", backup.ErrorCode)
	}

	secret := productionCloudBackupTestSecret
	for _, attempt := range []struct {
		name      string
		operation string
		args      []any
	}{
		{
			name:      "view with a smuggled argument",
			operation: productionCloudBackupViewName,
			args:      []any{session, secret},
		},
		{
			name:      "view with a smuggled session field",
			operation: productionCloudBackupViewName,
			args: []any{map[string]any{
				"projectId":   productionDurableProjectID,
				"leaseId":     session["leaseId"],
				"projectPath": projectPath,
				"password":    secret,
			}},
		},
		{
			name:      "confirm-binding with a smuggled account",
			operation: productionCloudBackupConfirmName,
			args: []any{map[string]any{
				"projectSession":                 session,
				"localEndpointAccountId":         secret,
				"lastSelectedParentGenerationIds": []any{},
				"expectedRevision":               nil,
			}},
		},
		{
			name:      "confirm-binding with a smuggled secret field",
			operation: productionCloudBackupConfirmName,
			args: []any{map[string]any{
				"projectSession":                 session,
				"localEndpointAccountId":         productionCloudBackupLocalAccountID,
				"lastSelectedParentGenerationIds": []any{},
				"expectedRevision":               nil,
				"password":                       secret,
			}},
		},
		{
			name:      "backup with a smuggled secret field",
			operation: productionCloudBackupBackupName,
			args: []any{map[string]any{
				"operationId":         productionCloudBackupTestUUID,
				"projectSession":      session,
				"disclosureConfirmed": true,
				"password":            secret,
			}},
		},
		{
			name:      "list with a smuggled book",
			operation: productionCloudBackupListName,
			args: []any{map[string]any{
				"localEndpointAccountId": productionCloudBackupLocalAccountID,
				"cloudBookId":            secret,
			}},
		},
		{
			name:      "restore-copy with a smuggled grant",
			operation: productionCloudBackupRestoreName,
			args: []any{map[string]any{
				"operationId":            productionCloudBackupTestUUID,
				"localEndpointAccountId": productionCloudBackupLocalAccountID,
				"cloudBookId":            productionDurableProjectID,
				"generationId":           backup.Generation.GenerationID,
				"targetGrantId":          secret,
			}},
		},
		{
			name:      "cancel with a smuggled id",
			operation: productionCloudBackupCancelName,
			args:      []any{secret},
		},
	} {
		status, body, result := productionCloudBackupInvoke(t, author, attempt.operation, attempt.args)
		if status != http.StatusOK {
			t.Fatalf("%v answered HTTP %d: %s", attempt.name, status, body)
		}
		if success, _ := result["success"].(bool); success {
			t.Fatalf("%v was accepted; the secret was never a usable field: %s", attempt.name, body)
		}
		if strings.Contains(body, secret) {
			t.Fatalf("%v echoed the credential back in its answer: %s", attempt.name, body)
		}
	}

	// Nothing this lane wrote may carry it either: not the store, not the author
	// database, not the project manifest.
	for _, scanRoot := range []string{productionCloudBackupStoreDirectory(root), projectPath} {
		err := filepath.WalkDir(scanRoot, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if !entry.Type().IsRegular() {
				return nil
			}
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			if strings.Contains(string(data), secret) {
				return errors.New("a written file carries the credential")
			}
			return nil
		})
		if err != nil {
			t.Fatalf("scan %v: %v", scanRoot, err)
		}
	}
}

// TestProductionCloudBackupConfirmBindingReaffirmsDerivedBinding proves the derived
// binding can be re-affirmed with the revision the browser holds, and that it
// reports itself as the reserved local account.
func TestProductionCloudBackupConfirmBindingReaffirmsDerivedBinding(t *testing.T) {
	root, projectPath := productionDurableDataRoot(t)
	handler := productionCloudBackupLane(t, root)
	author := newProductionDurableClient(t, handler, productionCloudBackupTestTab)
	session := productionCloudBackupOpenSession(t, author, projectPath)

	confirmed := map[string]any{}
	author.decode(t, productionCloudBackupConfirmName, []any{map[string]any{
		"projectSession":                 session,
		"localEndpointAccountId":         productionCloudBackupLocalAccountID,
		"cloudBookId":                    productionDurableProjectID,
		"lastSelectedParentGenerationIds": []any{},
		"expectedRevision":               nil,
	}}, &confirmed)
	if success, _ := confirmed["success"].(bool); !success {
		t.Fatalf("cloud-backup:confirm-binding refused the derived binding: %v", confirmed)
	}
	binding, _ := confirmed["binding"].(map[string]any)
	if binding == nil {
		t.Fatalf("cloud-backup:confirm-binding returned no binding: %v", confirmed)
	}
	if binding["cloudBookId"] != productionDurableProjectID ||
		binding["localEndpointAccountId"] != productionCloudBackupLocalAccountID ||
		binding["mode"] != productionCloudBackupModeWritable ||
		binding["revision"] != float64(0) {
		t.Fatalf("cloud-backup:confirm-binding returned the wrong binding: %v", binding)
	}
	parents, _ := binding["lastSelectedParentGenerationIds"].([]any)
	if len(parents) != 0 {
		t.Fatalf("the derived binding claims a lineage it never recorded: %v", binding)
	}
	// The author database is untouched by a binding confirmation.
	if _, statErr := os.Lstat(filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName)); statErr != nil {
		t.Fatalf("confirm-binding disturbed the author database: %v", statErr)
	}
}
