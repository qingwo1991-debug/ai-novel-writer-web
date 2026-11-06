//go:build linux

package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// These tests travel the real HTTP surface: the frozen three-key session, the
// admitted v7 database, and the four bound names over the durable lane. They bind
// their own transport.Server - project lifecycle plus this one slice - because the
// lane's own binder block is what the production handler test walks, and a name is
// only served once its binder is installed there.
//
// The database is the editor-reads fixture: the real v7 baseline with one live
// character (`ci-character-1`), so CHARACTER_NOT_FOUND, the avatar row and the
// roster read are exercised against the schema the lane actually admits.

const (
	productionAvatarTestNonce     = "avatar-open-1"
	productionAvatarTestCharacter = "ci-character-1"
	// productionAvatarTestAbsent is a well-formed identity no seeded character
	// holds, so it reaches the CHARACTER_NOT_FOUND branch instead of the shape
	// refusal.
	productionAvatarTestAbsent = "ci-character-9"
	// productionAvatarTestOther is the second tab: it stages an upload of its own
	// and never opens the project, so both authorities are proved absent.
	productionAvatarTestOther = "client_avatar_other"
)

// The three payloads every case is built from: a real PNG signature, a real JPEG
// magic prefix, and text that a lying file name would call a png.
var (
	productionAvatarTestPNG = []byte{
		0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a,
		0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
	}
	productionAvatarTestJPEG = []byte{0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10, 0x4a, 0x46, 0x49, 0x46}
	productionAvatarTestText = []byte("这不是一张图片 - this is not an image\n")
)

// productionAvatarTestView is the wire avatar object, decoded with its types, so
// an assertion never has to guess what encoding/json did to a number.
type productionAvatarTestView struct {
	CharacterID   string `json:"characterId"`
	AssetRevision int64  `json:"assetRevision"`
	Mime          string `json:"mime"`
	Base64        string `json:"base64"`
}

// productionAvatarTestAnswer is the whole envelope: the two success shapes and the
// one failure shape.
type productionAvatarTestAnswer struct {
	Success   bool                      `json:"success"`
	Cancelled bool                      `json:"cancelled"`
	Avatar    productionAvatarTestView  `json:"avatar"`
	Image     productionAvatarTestView  `json:"image"`
	Avatars   []productionAvatarTestView  `json:"avatars"`
	Error     productionAvatarTestError  `json:"error"`
}

type productionAvatarTestError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// productionAvatarTestLane is one open production lane with the four names bound,
// the upload owner installed (or not), and the two callers the authority tests need.
type productionAvatarTestLane struct {
	server      *transport.Server
	open        *productionOpen
	uploads     *productionPickerFixtureUploads
	author      *productionDurableClient
	other       *productionDurableClient
	projectPath string
	database    string
	frozen      map[string]any
	readback    *sql.DB
}

// newProductionAvatarTestLane opens the admitted fixture project over the real
// lane. uploads is the seam under test: a staged owner, or nil to prove the honest
// refusal this process answers while the lane installs no upload route.
func newProductionAvatarTestLane(t *testing.T, uploads productionPickerUploads) *productionAvatarTestLane {
	t.Helper()
	root, projectPath, contract, _, _ := productionEditorReadsRoot(t)
	layout, err := productionLayoutFor(root)
	if err != nil {
		t.Fatal(err)
	}
	startup, cancel := context.WithTimeout(context.Background(), productionStartupTimeout)
	defer cancel()
	open, err := openProduction(startup, layout, productionConfig{
		Schema: contract, MaxDatabaseBytes: productionMaxDatabaseBytes,
	})
	if err != nil {
		t.Fatal(err)
	}
	server := transport.New(migrationRegistry(t), security.NewOrigins("http://"+productionDurableAddress),
		"go-production-character-avatar")
	t.Cleanup(func() {
		server.Close()
		if closeErr := open.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if bindErr := server.BindProjects(open.durable); bindErr != nil {
		t.Fatal(bindErr)
	}
	if bindErr := newProductionCharacterAvatars(open.durable, uploads).Bind(server); bindErr != nil {
		t.Fatalf("bind the character avatar names: %v", bindErr)
	}
	author, frozen := productionExecuteSession(t, server, projectPath, productionAvatarTestNonce)
	lane := &productionAvatarTestLane{
		server:      server,
		open:        open,
		author:      author,
		other:       newProductionDurableClient(t, server, productionAvatarTestOther),
		projectPath: projectPath,
		database:    filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName),
		frozen:      frozen,
	}
	if staged, ok := uploads.(*productionPickerFixtureUploads); ok {
		lane.uploads = staged
	}
	// The read-only connection is opened before any digest is taken: a reader can
	// create its own sidecar, and that must not look like the lane writing.
	lane.readback = productionAvatarTestReadback(t, lane.database)
	return lane
}

// productionAvatarTestReadback is a separate mode=ro connection, so an assertion
// about the committed row is never an assertion about the serving connection's
// cache.
func productionAvatarTestReadback(t *testing.T, database string) *sql.DB {
	t.Helper()
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(database)}
	uri.RawQuery = "mode=ro"
	db, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("the read-only avatar connection is unusable: %v", err)
	}
	return db
}

// productionAvatarTestCall is one operation call through the frozen session, and
// the failure report carries the body rather than a decoded pointer.
func productionAvatarTestCall(t *testing.T, client *productionDurableClient, operation string,
	args []any) (productionAvatarTestAnswer, string) {
	t.Helper()
	response := client.call(t, operation, args)
	if response.Code != http.StatusOK {
		t.Fatalf("%s: %d %s", operation, response.Code, response.Body.String())
	}
	body := response.Body.String()
	var envelope struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil || len(envelope.Result) == 0 {
		t.Fatalf("%s: %v %s", operation, err, body)
	}
	var answer productionAvatarTestAnswer
	if err := json.Unmarshal(envelope.Result, &answer); err != nil {
		t.Fatalf("%s: %v %s", operation, err, body)
	}
	return answer, body
}

// productionAvatarTestAccepts requires a successful answer.
func productionAvatarTestAccepts(t *testing.T, client *productionDurableClient, operation string,
	args []any) productionAvatarTestAnswer {
	t.Helper()
	answer, body := productionAvatarTestCall(t, client, operation, args)
	if !answer.Success {
		t.Fatalf("%s refused a valid request: %s", operation, body)
	}
	return answer
}

// productionAvatarTestRefuses requires exactly one symbolic refusal, with the code
// repeated as the message the renderer would throw.
func productionAvatarTestRefuses(t *testing.T, client *productionDurableClient, operation string,
	args []any, code string) {
	t.Helper()
	answer, body := productionAvatarTestCall(t, client, operation, args)
	if answer.Success || answer.Error.Code != code || answer.Error.Message != code {
		t.Fatalf("%s: expected %s, got %s", operation, code, body)
	}
}

// productionAvatarTestRaw is the HTTP status of a call that is expected to be
// refused before it reaches the handler - a tab without the project lease - so the
// assertion is about the transport's own authority, not about an envelope.
func productionAvatarTestRaw(t *testing.T, client *productionDurableClient, operation string, args []any) int {
	t.Helper()
	return client.call(t, operation, args).Code
}

// productionAvatarTestAsset is the one committed avatar row.
type productionAvatarTestAsset struct {
	Relative string
	Digest   string
	Mime     string
	Size     int64
	Revision int64
}

// productionAvatarTestAssets reads the roster's committed row over the read-only
// connection: zero rows is the empty roster, one row is the current avatar.
func productionAvatarTestAssets(t *testing.T, db *sql.DB, characterID string) []productionAvatarTestAsset {
	t.Helper()
	rows, err := db.Query(`SELECT relative_path, content_hash, mime, byte_size, asset_revision
 FROM character_avatar_assets WHERE character_id=? ORDER BY asset_revision`, characterID)
	if err != nil {
		t.Fatalf("read the avatar rows: %v", err)
	}
	defer func() { _ = rows.Close() }()
	assets := make([]productionAvatarTestAsset, 0, 1)
	for rows.Next() {
		var asset productionAvatarTestAsset
		if err := rows.Scan(&asset.Relative, &asset.Digest, &asset.Mime, &asset.Size,
			&asset.Revision); err != nil {
			t.Fatalf("scan the avatar row: %v", err)
		}
		assets = append(assets, asset)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate the avatar rows: %v", err)
	}
	return assets
}

// productionAvatarTestTree digests every file the project holds, so "a refusal
// wrote nothing" is one comparison instead of a list of files a test hoped were the
// only ones. The avatar lane writes in exactly two places under the project - the
// admitted database (.ai-novel/project.db) and the avatar store beside it
// (.ai-novel/avatars/<bucket>/<file>) - and both are inside this digest.
//
// Directories carry no bytes, so they are skipped: a file's own relative path is
// what the digest keys on, and a lane cannot add or remove a directory without a
// file. Everything that is not a directory or a SQLite write-ahead sidecar must be
// a regular file; anything else is a lane bug this digest refuses to read.
func productionAvatarTestTree(t *testing.T, projectPath string) []byte {
	t.Helper()
	relative := make([]string, 0, 8)
	walkErr := filepath.WalkDir(projectPath, func(path string, entry fs.DirEntry, failure error) error {
		if failure != nil {
			return failure
		}
		if path == projectPath || entry.IsDir() {
			return nil
		}
		name := filepath.Base(path)
		for _, suffix := range []string{"-wal", "-shm", "-journal"} {
			if strings.HasSuffix(name, suffix) {
				return nil
			}
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("%s is not a regular file", path)
		}
		rel, err := filepath.Rel(projectPath, path)
		if err != nil {
			return err
		}
		relative = append(relative, rel)
		return nil
	})
	if walkErr != nil {
		t.Fatalf("the project tree digest cannot walk %s: %v", projectPath, walkErr)
	}
	sort.Strings(relative)
	digest := sha256.New()
	for _, rel := range relative {
		body, err := os.ReadFile(filepath.Join(projectPath, rel))
		if err != nil {
			t.Fatalf("the project tree digest cannot read %s: %v", rel, err)
		}
		digest.Write([]byte(rel))
		digest.Write([]byte{0})
		digest.Write(body)
		digest.Write([]byte{0})
	}
	return digest.Sum(nil)
}

// productionAvatarTestPath is the asset path the reference derives
// (character-asset-service.ts:141), recomputed here so the assertion does not read
// the lane's own helper.
func productionAvatarTestPath(characterID string, revision int64, data []byte, extension string) string {
	bucket := sha256.Sum256([]byte(characterID))
	content := sha256.Sum256(data)
	return "avatars/" + hex.EncodeToString(bucket[:]) + "/" +
		strconv.FormatInt(revision, 10) + "-" + hex.EncodeToString(content[:]) + "." + extension
}

// productionAvatarTestStage stages one upload for the tab that opened the project.
func (l *productionAvatarTestLane) stage(id, name string, data []byte) {
	l.uploads.stage(l.author.cookie.Value, l.author.tab, id, name, data)
}

// TestProductionCharacterAvatarCommitAnswersTheRosterAvatar is the happy path of
// commit plus read-batch: the recorded row, the file it names, and the base64 the
// renderer turns into an object URL all agree.
func TestProductionCharacterAvatarCommitAnswersTheRosterAvatar(t *testing.T) {
	lane := newProductionAvatarTestLane(t, newProductionPickerFixtureUploads())
	encoded := base64.StdEncoding.EncodeToString(productionAvatarTestPNG)
	answer := productionAvatarTestAccepts(t, lane.author, productionAvatarCommitName,
		[]any{productionAvatarTestCharacter, encoded, lane.frozen})
	if answer.Avatar.CharacterID != productionAvatarTestCharacter || answer.Avatar.AssetRevision != 1 ||
		answer.Avatar.Mime != productionAvatarMimePNG || answer.Avatar.Base64 != encoded {
		t.Fatalf("commit answered %+v", answer.Avatar)
	}
	expected := productionAvatarTestPath(productionAvatarTestCharacter, 1, productionAvatarTestPNG, "png")
	assets := productionAvatarTestAssets(t, lane.readback, productionAvatarTestCharacter)
	if len(assets) != 1 {
		t.Fatalf("the roster holds %d avatar rows, expected 1", len(assets))
	}
	if assets[0].Relative != expected || assets[0].Mime != productionAvatarMimePNG ||
		assets[0].Revision != 1 || assets[0].Size != int64(len(productionAvatarTestPNG)) {
		t.Fatalf("the committed row is %+v", assets[0])
	}
	stored, err := os.ReadFile(filepath.Join(lane.projectPath, projectfs.StorageDirectory, expected))
	if err != nil {
		t.Fatalf("read the stored avatar: %v", err)
	}
	if string(stored) != string(productionAvatarTestPNG) {
		t.Fatalf("the stored avatar holds %d bytes, expected %d", len(stored), len(productionAvatarTestPNG))
	}
	roster := productionAvatarTestAccepts(t, lane.author, productionAvatarReadName,
		[]any{[]any{productionAvatarTestCharacter, productionAvatarTestAbsent}, lane.frozen})
	if len(roster.Avatars) != 1 || roster.Avatars[0].Base64 != encoded ||
		roster.Avatars[0].AssetRevision != 1 || roster.Avatars[0].Mime != productionAvatarMimePNG {
		t.Fatalf("read-batch answered %+v", roster.Avatars)
	}
}

// TestProductionCharacterAvatarCommitKeepsOneRecordPerCharacter proves a second
// commit replaces the first: one row, one file, and the superseded asset is
// reclaimed rather than left behind.
func TestProductionCharacterAvatarCommitKeepsOneRecordPerCharacter(t *testing.T) {
	lane := newProductionAvatarTestLane(t, newProductionPickerFixtureUploads())
	productionAvatarTestAccepts(t, lane.author, productionAvatarCommitName,
		[]any{productionAvatarTestCharacter, base64.StdEncoding.EncodeToString(productionAvatarTestPNG), lane.frozen})
	answer := productionAvatarTestAccepts(t, lane.author, productionAvatarCommitName,
		[]any{productionAvatarTestCharacter, base64.StdEncoding.EncodeToString(productionAvatarTestJPEG), lane.frozen})
	if answer.Avatar.AssetRevision != 2 || answer.Avatar.Mime != productionAvatarMimeJPEG {
		t.Fatalf("the second commit answered %+v", answer.Avatar)
	}
	expected := productionAvatarTestPath(productionAvatarTestCharacter, 2, productionAvatarTestJPEG, "jpg")
	assets := productionAvatarTestAssets(t, lane.readback, productionAvatarTestCharacter)
	if len(assets) != 1 || assets[0].Relative != expected || assets[0].Revision != 2 {
		t.Fatalf("the roster holds %+v", assets)
	}
	bucket := filepath.Join(lane.projectPath, projectfs.StorageDirectory, filepath.Dir(expected))
	entries, err := os.ReadDir(bucket)
	if err != nil {
		t.Fatalf("read the avatar bucket: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(expected) {
		t.Fatalf("the avatar bucket holds %d files, expected the current one only", len(entries))
	}
}

// TestProductionCharacterAvatarChoosePreviewsTheStagedImage proves choose answers
// the preview the desktop's dialog answered, without writing anything: the upload
// is read through the seam the browser's staging route satisfies.
func TestProductionCharacterAvatarChoosePreviewsTheStagedImage(t *testing.T) {
	lane := newProductionAvatarTestLane(t, newProductionPickerFixtureUploads())
	lane.stage("upload-avatar-1", "avatar.png", productionAvatarTestPNG)
	tree, assets := productionAvatarTestTree(t, lane.projectPath),
		len(productionAvatarTestAssets(t, lane.readback, productionAvatarTestCharacter))
	answer := productionAvatarTestAccepts(t, lane.author, productionAvatarChooseName,
		[]any{productionAvatarTestCharacter, "upload-avatar-1", lane.frozen})
	if answer.Cancelled || answer.Image.CharacterID != productionAvatarTestCharacter ||
		answer.Image.Mime != productionAvatarMimePNG ||
		answer.Image.Base64 != base64.StdEncoding.EncodeToString(productionAvatarTestPNG) {
		t.Fatalf("choose answered %+v", answer.Image)
	}
	if after := productionAvatarTestTree(t, lane.projectPath); string(after) != string(tree) {
		t.Fatal("choose changed the project tree")
	}
	if len(productionAvatarTestAssets(t, lane.readback, productionAvatarTestCharacter)) != assets {
		t.Fatal("choose wrote an avatar row")
	}
}

// TestProductionCharacterAvatarChooseRefusesALyingMime is the magic-byte decision:
// the staged file is named avatar.png and holds text, and the name is not evidence.
func TestProductionCharacterAvatarChooseRefusesALyingMime(t *testing.T) {
	lane := newProductionAvatarTestLane(t, newProductionPickerFixtureUploads())
	lane.stage("upload-avatar-2", "avatar.png", productionAvatarTestText)
	tree := productionAvatarTestTree(t, lane.projectPath)
	productionAvatarTestRefuses(t, lane.author, productionAvatarChooseName,
		[]any{productionAvatarTestCharacter, "upload-avatar-2", lane.frozen},
		productionAvatarCodeFormatInvalid)
	if after := productionAvatarTestTree(t, lane.projectPath); string(after) != string(tree) {
		t.Fatal("a refused choose changed the project tree")
	}
	if assets := productionAvatarTestAssets(t, lane.readback, productionAvatarTestCharacter); len(assets) != 0 {
		t.Fatalf("a refused choose wrote %d avatar rows", len(assets))
	}
}

// TestProductionCharacterAvatarCommitRefusesDamagedAndOversizeUpload proves the
// two byte rules on the recording path: text that claims to be a png, and a payload
// one byte over the reference's four-megabyte ceiling. Neither may touch the store.
func TestProductionCharacterAvatarCommitRefusesDamagedAndOversizeUpload(t *testing.T) {
	lane := newProductionAvatarTestLane(t, newProductionPickerFixtureUploads())
	tree := productionAvatarTestTree(t, lane.projectPath)
	productionAvatarTestRefuses(t, lane.author, productionAvatarCommitName,
		[]any{productionAvatarTestCharacter, base64.StdEncoding.EncodeToString(productionAvatarTestText), lane.frozen},
		productionAvatarCodeFormatInvalid)
	oversize := make([]byte, productionAvatarMaxInputBytes+1)
	copy(oversize, productionAvatarTestPNG)
	productionAvatarTestRefuses(t, lane.author, productionAvatarCommitName,
		[]any{productionAvatarTestCharacter, base64.StdEncoding.EncodeToString(oversize), lane.frozen},
		productionAvatarCodeTooLarge)
	if after := productionAvatarTestTree(t, lane.projectPath); string(after) != string(tree) {
		t.Fatal("a refused commit changed the project tree")
	}
	if assets := productionAvatarTestAssets(t, lane.readback, productionAvatarTestCharacter); len(assets) != 0 {
		t.Fatalf("a refused commit wrote %d avatar rows", len(assets))
	}
}

// TestProductionCharacterAvatarRemoveIsIdempotent proves remove drops the row and
// the file once, and that a second remove is a success that writes nothing - while
// an unknown character is still CHARACTER_NOT_FOUND rather than a silent success.
func TestProductionCharacterAvatarRemoveIsIdempotent(t *testing.T) {
	lane := newProductionAvatarTestLane(t, newProductionPickerFixtureUploads())
	productionAvatarTestAccepts(t, lane.author, productionAvatarCommitName,
		[]any{productionAvatarTestCharacter, base64.StdEncoding.EncodeToString(productionAvatarTestPNG), lane.frozen})
	productionAvatarTestAccepts(t, lane.author, productionAvatarRemoveName,
		[]any{productionAvatarTestCharacter, lane.frozen})
	if assets := productionAvatarTestAssets(t, lane.readback, productionAvatarTestCharacter); len(assets) != 0 {
		t.Fatalf("remove left %d avatar rows", len(assets))
	}
	roster := productionAvatarTestAccepts(t, lane.author, productionAvatarReadName,
		[]any{[]any{productionAvatarTestCharacter}, lane.frozen})
	if len(roster.Avatars) != 0 {
		t.Fatalf("the roster still answers %+v", roster.Avatars)
	}
	tree := productionAvatarTestTree(t, lane.projectPath)
	productionAvatarTestAccepts(t, lane.author, productionAvatarRemoveName,
		[]any{productionAvatarTestCharacter, lane.frozen})
	if after := productionAvatarTestTree(t, lane.projectPath); string(after) != string(tree) {
		t.Fatal("an idempotent remove changed the project tree")
	}
	productionAvatarTestRefuses(t, lane.author, productionAvatarRemoveName,
		[]any{productionAvatarTestAbsent, lane.frozen}, productionAvatarCodeNotFound)
}

// TestProductionCharacterAvatarRefusesForeignCallers proves the two authorities a
// browser cannot forge: a tab with no lease cannot read the roster at all, and an
// upload id staged for another tab is unknown here.
func TestProductionCharacterAvatarRefusesForeignCallers(t *testing.T) {
	lane := newProductionAvatarTestLane(t, newProductionPickerFixtureUploads())
	if code := productionAvatarTestRaw(t, lane.other, productionAvatarReadName,
		[]any{[]any{productionAvatarTestCharacter}, lane.frozen}); code == http.StatusOK {
		t.Fatal("a tab without the project lease read the avatar roster")
	}
	lane.uploads.stage(lane.other.cookie.Value, lane.other.tab, "upload-foreign", "avatar.png",
		productionAvatarTestPNG)
	productionAvatarTestRefuses(t, lane.author, productionAvatarChooseName,
		[]any{productionAvatarTestCharacter, "upload-foreign", lane.frozen},
		productionAvatarCodeUploadUnknown)
}

// TestProductionCharacterAvatarChooseNamesTheMissingUploadOwner is the honest
// answer this process gives while the lane installs no upload route: the name is
// served, and it refuses with the port it is missing instead of pretending.
func TestProductionCharacterAvatarChooseNamesTheMissingUploadOwner(t *testing.T) {
	lane := newProductionAvatarTestLane(t, nil)
	productionAvatarTestRefuses(t, lane.author, productionAvatarChooseName,
		[]any{productionAvatarTestCharacter, "upload-avatar-3", lane.frozen},
		productionAvatarCodeUploadUnavailable)
	productionAvatarTestRefuses(t, lane.author, productionAvatarChooseName,
		[]any{productionAvatarTestCharacter, lane.frozen},
		productionAvatarCodeUploadRequired)
}

// TestProductionCharacterAvatarReadBatchOmitsAMissingFile is the case an author
// meets after a restore: the row survived and the file did not. read-batch must then
// answer the roster without that character, exactly as the reference's readMany
// skips a missing asset (character-asset-service.ts:159-169), rather than answering
// an empty base64 the renderer would turn into an empty image.
func TestProductionCharacterAvatarReadBatchOmitsAMissingFile(t *testing.T) {
	lane := newProductionAvatarTestLane(t, newProductionPickerFixtureUploads())
	encoded := base64.StdEncoding.EncodeToString(productionAvatarTestPNG)
	productionAvatarTestAccepts(t, lane.author, productionAvatarCommitName,
		[]any{productionAvatarTestCharacter, encoded, lane.frozen})
	expected := productionAvatarTestPath(productionAvatarTestCharacter, 1, productionAvatarTestPNG, "png")
	stored := filepath.Join(lane.projectPath, projectfs.StorageDirectory, expected)
	if err := os.Remove(stored); err != nil {
		t.Fatalf("remove the asset to imitate a restore: %v", err)
	}
	roster := productionAvatarTestAccepts(t, lane.author, productionAvatarReadName,
		[]any{[]any{productionAvatarTestCharacter}, lane.frozen})
	if len(roster.Avatars) != 0 {
		t.Fatalf("read-batch answered %+v for an asset whose file is gone", roster.Avatars)
	}
	if assets := productionAvatarTestAssets(t, lane.readback, productionAvatarTestCharacter); len(assets) != 1 {
		t.Fatalf("the restored row is gone too: %d rows", len(assets))
	}
	if empty := productionAvatarTestAccepts(t, lane.author, productionAvatarReadName,
		[]any{[]any{productionAvatarTestAbsent}, lane.frozen}); len(empty.Avatars) != 0 {
		t.Fatalf("read-batch answered %+v for a character with no avatar", empty.Avatars)
	}
}
