//go:build linux

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/uploadstore"
)

// These tests walk the whole upload journey a browser walks, in order: POST the
// bytes to the mounted route, then present the returned handle to a served
// operation over the frozen session. The lane builds the production mux the way
// production_open_linux.go builds it - the two upload routes beside the operation
// server - so the 501 this slice removes and the answers it adds are both observed
// on the same surface.
//
// The names under test are the three families that could not be used end to end:
//
//	kb:import-document           consumes one staged text file
//	kb:import-folder             consumes one staged folder
//	character-avatar:choose      consumes one staged image
//	dialog:select-knowledge-files verifies one staged file for the page
//
// The admitted project is the editor-reads fixture (the real v7 baseline with one
// live character), so the import and the avatar both write through the same store
// the served readers already decode.

const (
	// productionUploadTestNonce opens the one project session this file uses.
	productionUploadTestNonce = "upload-route-open-1"
	// productionUploadTestOtherTab is a second browser tab on its own session, so
	// "another tab's upload id" is a real refusal and not a fabricated one.
	productionUploadTestOtherTab = "client_upload_other_1"
	// productionUploadTestCharacter is the character the editor-reads baseline seeds.
	productionUploadTestCharacter = "ci-character-1"
	// productionUploadTestPNG is a real PNG signature; choose decides by magic bytes.
	productionUploadTestPNG = "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"
	// productionUploadTestDocument is the manuscript a browser import stages.
	productionUploadTestDocument = "第一章 门\n\n他推开门，风从走廊尽头灌进来。\n"
)

// productionUploadTestLane is one open production lane with the upload route
// mounted, the three consuming families bound, and the two callers the authority
// cases need.
type productionUploadTestLane struct {
	server  *transport.Server
	mux     http.Handler
	open    *productionOpen
	route   *productionUploadRoute
	staging string
	project string
	frozen  map[string]any
	author  *productionDurableClient
	other   *productionDurableClient
}

func newProductionUploadTestLane(t *testing.T, limits uploadstore.Limits) *productionUploadTestLane {
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
		"go-production-upload-route")
	// The production wiring under test: one value that is both the mounted route and
	// the seam the three lanes read.
	route, err := newProductionUploadRouteWith(open, server, productionDurableAddress, limits)
	if err != nil {
		_ = open.Close()
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	if mountErr := route.Mount(mux); mountErr != nil {
		_ = route.Close()
		_ = open.Close()
		t.Fatal(mountErr)
	}
	// The rest of the production mux, in production_open_linux.go's own order.
	mux.Handle("/api/", server)
	mux.Handle("/api", server)
	mux.Handle("/healthz", server)
	mux.Handle("/", server)
	t.Cleanup(func() {
		server.Close()
		if closeErr := route.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		if closeErr := open.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if bindErr := server.BindProjects(open.durable); bindErr != nil {
		t.Fatal(bindErr)
	}
	imports := newProductionKnowledgeImports(open.durable)
	if bindErr := imports.Bind(server); bindErr != nil {
		t.Fatal(bindErr)
	}
	if bindErr := newProductionKnowledgeUploadsFrom(imports, route).Bind(server); bindErr != nil {
		t.Fatalf("bind the knowledge upload names: %v", bindErr)
	}
	if bindErr := newProductionCharacterAvatarsFrom(open.durable, route).Bind(server); bindErr != nil {
		t.Fatalf("bind the character avatar names: %v", bindErr)
	}
	if bindErr := newProductionBrowserPickersFrom(open, route).Bind(server); bindErr != nil {
		t.Fatalf("bind the browser picker names: %v", bindErr)
	}
	author, frozen := productionExecuteSession(t, mux, projectPath, productionUploadTestNonce)
	return &productionUploadTestLane{
		server:  server,
		mux:     mux,
		open:    open,
		route:   route,
		staging: filepath.Join(layout.dataRoot, productionUploadStagingDirectory),
		project: projectPath,
		frozen:  frozen,
		author:  author,
		other:   newProductionDurableClient(t, mux, productionUploadTestOtherTab),
	}
}

// sibling is a second tab on the SAME cookie session, which is the only way to
// prove the tab half of the staging key and the shared tab cap.
func (l *productionUploadTestLane) sibling(tab string) *productionDurableClient {
	return l.author.siblingTab(tab)
}

// send is one upload request with exactly the headers the renderer sets
// (src/components/BrowserDialogs.tsx:92-95). The name and the relative label are
// URI-encoded the way encodeURIComponent encodes them; url.PathEscape agrees on
// every character these cases use.
func (l *productionUploadTestLane) send(handler http.Handler, client *productionDurableClient,
	route, name string, body []byte, extra map[string]string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, route, bytes.NewReader(body))
	request.Header.Set("X-Novel-User", "production-author")
	request.Header.Set("X-Client-Id", client.tab)
	request.Header.Set("X-CSRF-Token", client.csrf)
	request.Header.Set("Origin", "http://"+productionDurableAddress)
	request.Header.Set("Content-Type", "application/octet-stream")
	// The session cookie, so the staging key is the same session the operations
	// below run under. Without it the gateway would mint a second, unreachable one.
	request.AddCookie(client.cookie)
	if name != "" {
		request.Header.Set("X-Upload-Name", url.PathEscape(name))
	}
	for key, value := range extra {
		request.Header.Set(key, value)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

// upload stages one file and returns the handle the answer carried.
func (l *productionUploadTestLane) upload(t *testing.T, client *productionDurableClient, name string,
	data []byte) string {
	t.Helper()
	return productionUploadTestCreated(t,
		l.send(l.mux, client, uploadstore.RouteFile, name, data, nil), "uploadId")
}

// folder stages one folder and returns its handle.
func (l *productionUploadTestLane) folder(t *testing.T, client *productionDurableClient) string {
	t.Helper()
	return productionUploadTestCreated(t,
		l.send(l.mux, client, uploadstore.RouteFolder, "", []byte("{}"),
			map[string]string{"Content-Type": "application/json"}), "folderUploadId")
}

// member stages one file inside a staged folder.
func (l *productionUploadTestLane) member(t *testing.T, client *productionDurableClient, folder,
	relative string, data []byte) *httptest.ResponseRecorder {
	t.Helper()
	return l.send(l.mux, client, uploadstore.RouteFile, "member.txt", data,
		map[string]string{"X-Upload-Folder": folder, "X-Upload-Relative": url.PathEscape(relative)})
}

// staged is what is actually on disk under the staging root, so "a refused upload
// left nothing" is a comparison against an empty directory.
func (l *productionUploadTestLane) staged(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(l.staging)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func productionUploadTestCreated(t *testing.T, response *httptest.ResponseRecorder, key string) string {
	t.Helper()
	if response.Code != http.StatusCreated {
		t.Fatalf("upload refused: %d %s", response.Code, response.Body.String())
	}
	var answer map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &answer); err != nil {
		t.Fatalf("unreadable upload answer: %v %s", err, response.Body.String())
	}
	id, ok := answer[key].(string)
	if !ok || id == "" {
		t.Fatalf("no %s in the upload answer: %s", key, response.Body.String())
	}
	return id
}

func productionUploadTestRefused(t *testing.T, response *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	var answer struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &answer); err != nil {
		t.Fatalf("unreadable refusal: %v %s", err, response.Body.String())
	}
	if response.Code != status || answer.Error.Code != code {
		t.Fatalf("got %d %s, want %d %s", response.Code, answer.Error.Code, status, code)
	}
}

// productionUploadTestSnapshot reads the canonical knowledge snapshot, so an import
// is asserted against the store the served readers decode, not against its receipt.
func productionUploadTestSnapshot(t *testing.T, projectPath string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(projectPath, projectfs.StorageDirectory, productionKnowledgeIndexName))
	if err != nil {
		t.Fatalf("read the knowledge snapshot: %v", err)
	}
	return string(raw)
}

// TestProductionUploadRouteIsMountedAndChecksOrigin proves the structural claim:
// the transport still answers 501 for an unmounted upload route, and the mounted
// route answers the renderer's real request form while refusing a wrong Origin
// through the shared CSRF/Origin path.
func TestProductionUploadRouteIsMountedAndChecksOrigin(t *testing.T) {
	lane := newProductionUploadTestLane(t, uploadstore.DefaultLimits())
	body := []byte("synthetic manuscript; no author data")
	// The transport's own answer, unchanged by this slice: this is what the browser
	// used to receive from the mux.
	productionUploadTestRefused(t, lane.send(lane.server, lane.author, uploadstore.RouteFile,
		"notes.txt", body, nil), http.StatusNotImplemented, "GO_ROUTE_NOT_MIGRATED")
	productionUploadTestRefused(t, lane.send(lane.server, lane.author, uploadstore.RouteFolder,
		"", []byte("{}"), map[string]string{"Content-Type": "application/json"}),
		http.StatusNotImplemented, "GO_ROUTE_NOT_MIGRATED")
	// The mounted route answers the real form: an id plus the metadata a caller needs.
	response := lane.send(lane.mux, lane.author, uploadstore.RouteFile, "第一章 门.txt", body, nil)
	if response.Code != http.StatusCreated {
		t.Fatalf("the mounted route refused a valid upload: %d %s", response.Code, response.Body.String())
	}
	var answer struct {
		UploadID string `json:"uploadId"`
		Name     string `json:"name"`
		Size     int64  `json:"size"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &answer); err != nil {
		t.Fatal(err)
	}
	if answer.UploadID == "" || answer.Name != "第一章 门.txt" || answer.Size != int64(len(body)) {
		t.Fatalf("the upload answer lost its metadata: %s", response.Body.String())
	}
	// The same request with an origin the gateway does not allow is refused by the
	// shared path, before any byte is staged.
	before := len(lane.staged(t))
	productionUploadTestRefused(t, lane.send(lane.mux, lane.author, uploadstore.RouteFile, "notes.txt",
		body, map[string]string{"Origin": "https://untrusted.example"}), http.StatusForbidden, "ORIGIN_DENIED")
	if after := len(lane.staged(t)); after != before {
		t.Fatalf("a denied-origin upload staged a blob: %d -> %d", before, after)
	}
}

// TestKnowledgeImportConsumesAStagedUpload is the first of the three journeys: the
// bytes come in over the route and leave through a served import name.
func TestKnowledgeImportConsumesAStagedUpload(t *testing.T) {
	lane := newProductionUploadTestLane(t, uploadstore.DefaultLimits())
	uploadID := lane.upload(t, lane.author, "第一章 门.txt", []byte(productionUploadTestDocument))
	var answer map[string]any
	lane.author.decode(t, productionKnowledgeImportDocumentName,
		[]any{uploadID, lane.project, lane.frozen}, &answer)
	if answer["success"] != true {
		t.Fatalf("the import refused a staged document: %v", answer)
	}
	documentID, ok := answer["docId"].(string)
	if !ok || documentID == "" {
		t.Fatalf("the import answered no document id: %v", answer)
	}
	chunks, ok := answer["chunkCount"].(float64)
	if !ok || chunks < 1 {
		t.Fatalf("the import indexed no chunk: %v", answer)
	}
	if snapshot := productionUploadTestSnapshot(t, lane.project); !strings.Contains(snapshot, documentID) {
		t.Fatalf("the canonical snapshot does not hold the imported document")
	}
}

// TestKnowledgeImportConsumesAStagedFolder is the same journey for a folder: every
// text member the route staged arrives as its own document, in label order, and a
// member that is not a text file is skipped exactly as the reference skips it
// (production_knowledge_uploads_linux.go:461-465).
func TestKnowledgeImportConsumesAStagedFolder(t *testing.T) {
	lane := newProductionUploadTestLane(t, uploadstore.DefaultLimits())
	folderID := lane.folder(t, lane.author)
	productionUploadTestCreated(t, lane.member(t, lane.author, folderID, "卷一/第一章.txt",
		[]byte(productionUploadTestDocument)), "uploadId")
	productionUploadTestCreated(t, lane.member(t, lane.author, folderID, "卷一/第二章.md",
		[]byte("第二章 灯\n\n灯芯烧到最后，屋里只剩一线光。\n")), "uploadId")
	productionUploadTestCreated(t, lane.member(t, lane.author, folderID, "封面.png",
		[]byte(productionUploadTestPNG)), "uploadId")
	var answer map[string]any
	lane.author.decode(t, productionKnowledgeImportFolderName,
		[]any{folderID, lane.project, lane.frozen}, &answer)
	if answer["importedCount"] != float64(2) {
		t.Fatalf("the folder import counted %v members, want 2: %v", answer["importedCount"], answer)
	}
	if failed, ok := answer["failedFiles"].([]any); !ok || len(failed) != 0 {
		t.Fatalf("the folder import reported a failed member: %v", answer)
	}
	snapshot := productionUploadTestSnapshot(t, lane.project)
	if !strings.Contains(snapshot, "第一章 门") || !strings.Contains(snapshot, "第二章 灯") {
		t.Fatalf("the canonical snapshot does not hold both imported folder members")
	}
}

// TestCharacterAvatarChooseConsumesAStagedUpload is the second journey: the preview
// the page shows comes from the bytes the route staged, decided by magic bytes.
func TestCharacterAvatarChooseConsumesAStagedUpload(t *testing.T) {
	lane := newProductionUploadTestLane(t, uploadstore.DefaultLimits())
	uploadID := lane.upload(t, lane.author, "avatar.png", []byte(productionUploadTestPNG))
	var answer map[string]any
	lane.author.decode(t, productionAvatarChooseName,
		[]any{productionUploadTestCharacter, uploadID, lane.frozen}, &answer)
	if answer["success"] != true {
		t.Fatalf("choose refused a staged png: %v", answer)
	}
	image, ok := answer["image"].(map[string]any)
	if !ok || image["mime"] != "image/png" || image["base64"] == "" {
		t.Fatalf("choose answered no png preview: %v", answer)
	}
	// The same route, text named as an image: the magic bytes decide, not the name.
	textID := lane.upload(t, lane.author, "avatar.png", []byte("这不是一张图片\n"))
	var refused map[string]any
	lane.author.decode(t, productionAvatarChooseName,
		[]any{productionUploadTestCharacter, textID, lane.frozen}, &refused)
	if refused["success"] != false {
		t.Fatalf("choose accepted text as an image: %v", refused)
	}
	detail, ok := refused["error"].(map[string]any)
	if !ok || detail["code"] != productionAvatarCodeFormatInvalid {
		t.Fatalf("choose answered %v, want %s", refused, productionAvatarCodeFormatInvalid)
	}
}

// TestKnowledgePickerVerifiesAStagedUpload is the third journey: the picker name the
// page calls first now answers the verified identity of an upload instead of an
// unavailable slot.
func TestKnowledgePickerVerifiesAStagedUpload(t *testing.T) {
	lane := newProductionUploadTestLane(t, uploadstore.DefaultLimits())
	uploadID := lane.upload(t, lane.author, "稿件.txt", []byte(productionUploadTestDocument))
	var answer map[string]any
	lane.author.decode(t, productionPickerKnowledgeFiles,
		[]any{map[string]any{"uploadIds": []any{uploadID}}}, &answer)
	if answer["success"] != true {
		t.Fatalf("the picker refused a staged document: %v", answer)
	}
	files, ok := answer["files"].([]any)
	if !ok || len(files) != 1 {
		t.Fatalf("the picker verified %v files, want 1: %v", answer["files"], answer)
	}
	verified, ok := files[0].(map[string]any)
	if !ok || verified["uploadId"] != uploadID || verified["readable"] != false {
		t.Fatalf("the picker answered %v, want the staged id and no grant", files[0])
	}
}

// TestOversizeUploadIsRefusedBeforeAnythingIsWritten uses a small budget so the
// bound is observable: the refusal is the body bound and the staging root stays
// empty.
func TestOversizeUploadIsRefusedBeforeAnythingIsWritten(t *testing.T) {
	limits := uploadstore.DefaultLimits()
	limits.MaxFileBytes = 32
	lane := newProductionUploadTestLane(t, limits)
	productionUploadTestRefused(t, lane.send(lane.mux, lane.author, uploadstore.RouteFile, "big.txt",
		bytes.Repeat([]byte("x"), 64), nil), http.StatusRequestEntityTooLarge, "BODY_TOO_LARGE")
	if staged := lane.staged(t); len(staged) != 0 {
		t.Fatalf("a refused oversize upload left %d blobs behind: %v", len(staged), staged)
	}
	// Nothing was staged, so the id a refused upload never returned cannot be used.
	var answer map[string]any
	lane.author.decode(t, productionKnowledgeImportDocumentName,
		[]any{"00000000-0000-4000-8000-000000000000", lane.project, lane.frozen}, &answer)
	if answer["success"] != false || answer["error"] != productionKnowledgeCodeUploadUnknown {
		t.Fatalf("an unminted upload id answered %v, want %s", answer, productionKnowledgeCodeUploadUnknown)
	}
}

// TestUnsafeFolderMemberAndForeignFolderAreRefused covers the two folder refusals a
// browser can provoke: a member label that could address something outside the
// upload, and a folder staged by another tab.
func TestUnsafeFolderMemberAndForeignFolderAreRefused(t *testing.T) {
	lane := newProductionUploadTestLane(t, uploadstore.DefaultLimits())
	folderID := lane.folder(t, lane.author)
	for _, relative := range []string{"../escape.txt", "/etc/passwd", `nested\escape.txt`,
		"a/b/c/d/e/f/g/h/i.txt"} {
		productionUploadTestRefused(t, lane.send(lane.mux, lane.author, uploadstore.RouteFile,
			"member.txt", []byte("data"), map[string]string{"X-Upload-Folder": folderID,
				"X-Upload-Relative": url.PathEscape(relative)}),
			http.StatusBadRequest, "INVALID_PATH")
	}
	if staged := lane.staged(t); len(staged) != 0 {
		t.Fatalf("a refused member left blobs behind: %v", staged)
	}
	// A folder staged by another session and tab is not addressable, so its member
	// cannot be staged here either.
	foreign := lane.folder(t, lane.other)
	productionUploadTestRefused(t, lane.send(lane.mux, lane.author, uploadstore.RouteFile,
		"member.txt", []byte("data"), map[string]string{"X-Upload-Folder": foreign,
			"X-Upload-Relative": url.PathEscape("notes.txt")}),
		http.StatusForbidden, "UPLOAD_FOLDER_NOT_OWNED")
}

// TestStaleAndForeignUploadIDsAreRefusedAfterTheRouteAcceptedThem is the case that
// matters most: an id that was really minted, then is no longer redeemable.
func TestStaleAndForeignUploadIDsAreRefusedAfterTheRouteAcceptedThem(t *testing.T) {
	limits := uploadstore.DefaultLimits()
	limits.Lifetime = 20 * time.Millisecond
	lane := newProductionUploadTestLane(t, limits)
	// Foreign: another session staged it, so the author's tab cannot import it.
	foreignID := lane.upload(t, lane.other, "稿件.txt", []byte(productionUploadTestDocument))
	var foreign map[string]any
	lane.author.decode(t, productionKnowledgeImportDocumentName,
		[]any{foreignID, lane.project, lane.frozen}, &foreign)
	if foreign["success"] != false || foreign["error"] != productionKnowledgeCodeUploadUnknown {
		t.Fatalf("a foreign upload id answered %v, want %s", foreign, productionKnowledgeCodeUploadUnknown)
	}
	var avatar map[string]any
	lane.author.decode(t, productionAvatarChooseName,
		[]any{productionUploadTestCharacter, foreignID, lane.frozen}, &avatar)
	detail, ok := avatar["error"].(map[string]any)
	if avatar["success"] != false || !ok || detail["code"] != productionAvatarCodeUploadUnknown {
		t.Fatalf("a foreign upload id answered %v, want %s", avatar, productionAvatarCodeUploadUnknown)
	}
	// Stale: the author's own id, past its lifetime, is refused the same way.
	staleID := lane.upload(t, lane.author, "稿件.txt", []byte(productionUploadTestDocument))
	time.Sleep(80 * time.Millisecond)
	var stale map[string]any
	lane.author.decode(t, productionKnowledgeImportDocumentName,
		[]any{staleID, lane.project, lane.frozen}, &stale)
	if stale["success"] != false || stale["error"] != productionKnowledgeCodeUploadUnknown {
		t.Fatalf("a stale upload id answered %v, want %s", stale, productionKnowledgeCodeUploadUnknown)
	}
}

// TestUploadFromATabOverTheCapIsRefused proves the upload route shares the gateway's
// 32-tab admission instead of opening a second, uncapped door.
func TestUploadFromATabOverTheCapIsRefused(t *testing.T) {
	lane := newProductionUploadTestLane(t, uploadstore.DefaultLimits())
	// The author's tab already holds one slot; fill the rest on the same session.
	for index := 0; index < 31; index++ {
		lane.sibling(fmt.Sprintf("client_upload_cap_%03d", index)).call(t, "project:recent-list", []any{})
	}
	over := lane.sibling("client_upload_cap_999")
	productionUploadTestRefused(t, lane.send(lane.mux, over, uploadstore.RouteFile, "cap.txt",
		[]byte("data"), nil), http.StatusTooManyRequests, "TAB_LIMIT")
	if staged := lane.staged(t); len(staged) != 0 {
		t.Fatalf("a tab over the cap staged a blob: %v", staged)
	}
}
