//go:build linux

package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/importsource"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/uploadstore"
)

// These tests walk the import journey a browser walks, in order: POST the
// manuscripts to the mounted upload route, then send dialog:select-novel-files
// with the frozen three-key project session and read the inspection the page shows
// the author.
//
// What is proven, and how:
//
//	the answer names what was uploaded   the two staged manuscripts come back by
//	                                     display name, in upload order
//	another tab's upload is not a source  a manuscript staged by a different tab of
//	                                     the same cookie session never appears
//	a non-manuscript is not a source      a staged PNG in the same inbox is skipped
//	the selection refuses when empty      an empty pending set is a refusal, not an
//	                                     empty manuscript
//	the receipt is the caller's          one live receipt per caller, replaced by a
//	                                     re-selection
//	a stale lease cannot import          a lease id or project path that is not the
//	                                     runtime's current one is refused by name
//	nothing was written                  the project tree and a whole-file digest of
//	                                     project.db are unchanged after every
//	                                     refusal, including the ones that only the
//	                                     provider sees
const (
	// productionImportTestNonce opens the one project session this file uses.
	productionImportTestNonce = "production-import-open-1"
	// productionImportTestForeignTab stages a manuscript that the selecting caller
	// must never be offered: staging is keyed by cookie session AND tab.
	productionImportTestForeignTab = "client_import_staged_elsewhere_1"
	// productionImportTestFirst and productionImportTestSecond are the two
	// manuscripts the author stages, with the chapter headings the parser reads.
	productionImportTestFirst  = "第一章 门.txt"
	productionImportTestSecond = "第二章 灯.md"
)

// productionImportTestLane is one open production lane with the upload route
// mounted, project:open bound and dialog:select-novel-files bound through the real
// transport binder over this file's provider.
type productionImportTestLane struct {
	open      *productionOpen
	route     *productionUploadRoute
	mux       http.Handler
	server    *transport.Server
	selection *productionImportSelection
	project   string
	frozen    map[string]any
	author    *productionDurableClient
}

func newProductionImportTestLane(t *testing.T) *productionImportTestLane {
	t.Helper()
	root, projectPath := productionDurableDataRoot(t)
	layout, err := productionLayoutFor(root)
	if err != nil {
		t.Fatal(err)
	}
	startup, cancel := context.WithTimeout(context.Background(), productionStartupTimeout)
	defer cancel()
	open, err := openProduction(startup, layout, productionConfig{
		Schema: productionDurableContract(t), MaxDatabaseBytes: productionMaxDatabaseBytes,
	})
	if err != nil {
		t.Fatal(err)
	}
	server := transport.New(migrationRegistry(t), security.NewOrigins("http://"+productionDurableAddress),
		"go-production-import-selection")
	route, err := newProductionUploadRoute(open, server, productionDurableAddress)
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
	selection, err := newProductionImportSelection(open, route)
	if err != nil {
		t.Fatalf("build the import selection: %v", err)
	}
	if bindErr := selection.Bind(server); bindErr != nil {
		t.Fatalf("bind dialog:select-novel-files: %v", bindErr)
	}
	author, frozen := productionExecuteSession(t, mux, projectPath, productionImportTestNonce)
	return &productionImportTestLane{
		open:      open,
		route:     route,
		mux:       mux,
		server:    server,
		selection: selection,
		project:   projectPath,
		frozen:    frozen,
		author:    author,
	}
}

// send is one upload request with the headers the renderer sets
// (src/components/BrowserDialogs.tsx:92-95) plus the session cookie, so the staged
// id belongs to the cookie session the operations below run under.
func (l *productionImportTestLane) send(client *productionDurableClient, name string,
	data []byte) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, uploadstore.RouteFile, bytes.NewReader(data))
	request.Header.Set("X-Novel-User", "production-author")
	request.Header.Set("X-Client-Id", client.tab)
	request.Header.Set("X-CSRF-Token", client.csrf)
	request.Header.Set("Origin", "http://"+productionDurableAddress)
	request.Header.Set("Content-Type", "application/octet-stream")
	request.Header.Set("X-Upload-Name", url.PathEscape(name))
	request.AddCookie(client.cookie)
	response := httptest.NewRecorder()
	l.mux.ServeHTTP(response, request)
	return response
}

// stage uploads one manuscript and returns its id.
func (l *productionImportTestLane) stage(t *testing.T, client *productionDurableClient,
	name, body string) string {
	t.Helper()
	return productionUploadTestCreated(t, l.send(client, name, []byte(body)), "uploadId")
}

// request is the four-key display intent the renderer sends
// (src/components/dialogs/ImportNovelDialog.tsx:214-219). It cannot name a file.
func (l *productionImportTestLane) request() map[string]any {
	return map[string]any{
		"runId":               "production-import-run-1",
		"purpose":             string(importsource.AuthorManuscript),
		"locale":              "zh-CN",
		// The path the operation opened, so a fixture path and the runtime's own
		// spelling of it can never disagree here.
		"expectedProjectPath": l.frozen["projectPath"],
	}
}

// caller is the transport identity the served request carries, rebuilt for the
// direct provider calls below. The session is the cookie session, so an upload
// staged by another tab of it is still not this caller's.
func (l *productionImportTestLane) caller(client *productionDurableClient) transport.Caller {
	return transport.Caller{Principal: "production-author", SessionID: client.cookie.Value, ClientID: client.tab}
}

// productionImportInspection is the inspection the page renders.
type productionImportInspection struct {
	InspectionID       string   `json:"inspectionId"`
	Purpose            string   `json:"purpose"`
	SourceCount        int      `json:"sourceCount"`
	SourceDisplayNames []string `json:"sourceDisplayNames"`
	ChapterCount       int      `json:"chapterCount"`
	TotalWords         int      `json:"totalWords"`
	TotalBytes         int64    `json:"totalBytes"`
}

// productionImportWire is the answer of dialog:select-novel-files.
type productionImportWire struct {
	Success    bool                       `json:"success"`
	Inspection productionImportInspection `json:"inspection"`
}

// TestProductionImportSelectionNamesWhatWasUploaded is the whole picker journey:
// the manuscripts arrive over the mounted route and the selection answers with the
// names the author chose, with nothing else from the shared inbox admitted.
func TestProductionImportSelectionNamesWhatWasUploaded(t *testing.T) {
	lane := newProductionImportTestLane(t)
	// A second tab on the SAME cookie session, which is the only honest way to prove
	// that the staging key's tab half keeps another tab's upload out of a selection.
	other := &productionDurableClient{handler: lane.mux, tab: productionImportTestForeignTab,
		csrf: lane.author.csrf, cookie: lane.author.cookie}
	treeBefore := productionCloudBackupTreeDigest(t, lane.project)
	databaseBefore := productionCloudBackupDatabaseDigest(t, lane.project)

	// A manuscript staged by a DIFFERENT tab of the same cookie session. The
	// selecting caller holds the project lease, so it is the one that would import;
	// the foreign bytes must never become one of its sources.
	lane.stage(t, other, "外来的稿.txt", "第一章 外\n\n这是另一个标签页上传的。\n")
	refused := lane.author.call(t, "dialog:select-novel-files", []any{lane.request(), lane.frozen})
	productionUploadTestRefused(t, refused, http.StatusInternalServerError, "IMPORT_SELECTION_FAILED")
	// The same refusal, read from the provider itself: the selection is empty because
	// nothing in the inbox is redeemable by THIS caller, not because the lane is
	// missing anything.
	selection, release, providerErr := lane.selection.BeginImportSelection(context.Background(),
		lane.caller(lane.author), transport.ImportSelectionRequest{RunID: "production-import-run-1",
			Purpose: importsource.AuthorManuscript, Locale: "zh-CN"}, copyProductionImportContext(lane.frozen))
	if release != nil {
		release()
	}
	if providerErr == nil || providerErr.Error() != productionImportSelectionSourcesCode {
		t.Fatalf("a foreign staging produced a selection: %v", providerErr)
	}
	if len(selection.Sources) != 0 {
		t.Fatalf("a foreign staging produced sources: %d", len(selection.Sources))
	}
	if active := lane.selection.store.ActiveCount(); active != 0 {
		t.Fatalf("a refused selection retained a receipt: %d", active)
	}
	productionImportUnchanged(t, lane.project, treeBefore, databaseBefore)

	// The author's own manuscripts, plus one upload that is not a manuscript. The
	// PNG stages fine and is simply not offered as prose.
	lane.stage(t, lane.author, productionImportTestFirst, "第一章 门\n\n他推开门，风从走廊尽头灌进来。\n")
	lane.stage(t, lane.author, "封面.png", "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")
	lane.stage(t, lane.author, productionImportTestSecond, "第二章 灯\n\n楼道的灯亮着，没有人上楼。\n")
	var answer productionImportWire
	lane.author.decode(t, "dialog:select-novel-files", []any{lane.request(), lane.frozen}, &answer)
	if !answer.Success {
		t.Fatalf("the selection refused the author's own manuscripts: %+v", answer)
	}
	if answer.Inspection.InspectionID == "" || answer.Inspection.Purpose != string(importsource.AuthorManuscript) {
		t.Fatalf("the inspection is not a usable receipt: %+v", answer.Inspection)
	}
	if answer.Inspection.SourceCount != 2 || len(answer.Inspection.SourceDisplayNames) != 2 {
		t.Fatalf("the selection admitted %d sources: %+v", answer.Inspection.SourceCount, answer.Inspection.SourceDisplayNames)
	}
	want := []string{productionImportTestFirst, productionImportTestSecond}
	for index, name := range want {
		if answer.Inspection.SourceDisplayNames[index] != name {
			t.Fatalf("source %d is %v, want %v", index, answer.Inspection.SourceDisplayNames, want)
		}
	}
	if answer.Inspection.ChapterCount != 2 || answer.Inspection.TotalWords <= 0 || answer.Inspection.TotalBytes <= 0 {
		t.Fatalf("the inspection did not parse the manuscripts: %+v", answer.Inspection)
	}
	for _, name := range answer.Inspection.SourceDisplayNames {
		if name == "封面.png" || name == "外来的稿.txt" {
			t.Fatalf("the selection admitted a file it must not: %v", answer.Inspection.SourceDisplayNames)
		}
	}
	// One receipt for one caller: the store's own count, not the answer's.
	if active := lane.selection.store.ActiveCount(); active != 1 {
		t.Fatalf("the store kept %d receipts for one caller", active)
	}

	// A second selection re-reads the same pending set and replaces the caller's
	// receipt instead of accumulating one per run.
	var again productionImportWire
	lane.author.decode(t, "dialog:select-novel-files", []any{lane.request(), lane.frozen}, &again)
	if !again.Success || again.Inspection.SourceCount != 2 {
		t.Fatalf("a re-selection changed the pending set: %+v", again.Inspection)
	}
	if again.Inspection.InspectionID == "" || again.Inspection.InspectionID == answer.Inspection.InspectionID {
		t.Fatalf("a re-selection reused the previous receipt: %v", again.Inspection.InspectionID)
	}
	if active := lane.selection.store.ActiveCount(); active != 1 {
		t.Fatalf("a re-selection kept %d receipts", active)
	}
	// Selecting is a read: the project tree and the author database are untouched.
	productionImportUnchanged(t, lane.project, treeBefore, databaseBefore)
}

// TestProductionImportSelectionRefusesStaleAndForeignContext covers the refusals
// the wire folds into IMPORT_SELECTION_FAILED, so each one is proven by its own
// symbolic name and by the tree and database it leaves alone.
func TestProductionImportSelectionRefusesStaleAndForeignContext(t *testing.T) {
	lane := newProductionImportTestLane(t)
	treeBefore := productionCloudBackupTreeDigest(t, lane.project)
	databaseBefore := productionCloudBackupDatabaseDigest(t, lane.project)
	lane.stage(t, lane.author, productionImportTestFirst, "第一章 门\n\n他推开门。\n")
	request := transport.ImportSelectionRequest{RunID: "production-import-run-2",
		Purpose: importsource.AuthorManuscript, Locale: "zh-CN"}

	// The runtime's lease, proved by the provider: a context that differs from the
	// live one in any part is refused before any bytes are read.
	for _, refused := range []struct {
		name     string
		expected map[string]any
		want     string
	}{
		{name: "another lease id", expected: productionImportContext(lane.frozen, "leaseId", "lease-0000"),
			want: productionImportSelectionStaleCode},
		{name: "another project path", expected: productionImportContext(lane.frozen, "projectPath", lane.project+"-elsewhere"),
			want: productionImportSelectionStaleCode},
		{name: "no lease id", expected: productionImportContext(lane.frozen, "leaseId", ""),
			want: productionImportSelectionProjectCode},
		{name: "no project id", expected: productionImportContext(lane.frozen, "projectId", ""),
			want: productionImportSelectionProjectCode},
	} {
		selection, release, err := lane.selection.BeginImportSelection(context.Background(),
			lane.caller(lane.author), request, refused.expected)
		if release != nil {
			release()
		}
		if err == nil || err.Error() != refused.want {
			t.Fatalf("%s was admitted: %v", refused.name, err)
		}
		if len(selection.Sources) != 0 {
			t.Fatalf("%s produced sources: %d", refused.name, len(selection.Sources))
		}
		productionImportUnchanged(t, lane.project, treeBefore, databaseBefore)
	}

	// A caller with no lease at all cannot import, even though the project is open
	// and something is staged: the proof is per caller, not per project.
	stranger := &productionDurableClient{handler: lane.mux, tab: productionImportTestForeignTab,
		csrf: lane.author.csrf, cookie: lane.author.cookie}
	if _, release, err := lane.selection.BeginImportSelection(context.Background(),
		lane.caller(stranger), request, copyProductionImportContext(lane.frozen)); err == nil {
		if release != nil {
			release()
		}
		t.Fatal("a caller without a lease imported through another caller's context")
	}

	// Only the author-manuscript purpose reaches a provider; a reference selection is
	// the durable parsing run this lane does not claim.
	if _, release, err := lane.selection.BeginImportSelection(context.Background(),
		lane.caller(lane.author), transport.ImportSelectionRequest{RunID: "production-import-run-3",
			Purpose: importsource.Reference, Locale: "zh-CN"}, copyProductionImportContext(lane.frozen)); err == nil {
		if release != nil {
			release()
		}
		t.Fatal("a reference selection was admitted")
	}
	// The wire refuses a reference selection with the transport's own code, before
	// any provider runs.
	referenceRequest := lane.request()
	referenceRequest["purpose"] = string(importsource.Reference)
	productionUploadTestRefused(t,
		lane.author.call(t, "dialog:select-novel-files", []any{referenceRequest, lane.frozen}),
		http.StatusNotImplemented, "IMPORT_DURABLE_PARSING_REQUIRED")
	// And a request that would name an upload id is refused as INVALID_ARGUMENTS: the
	// frozen four-key shape is the whole attack surface, and it names nothing.
	smuggled := lane.request()
	smuggled["uploadIds"] = []any{"00000000-0000-4000-8000-000000000000"}
	productionUploadTestRefused(t,
		lane.author.call(t, "dialog:select-novel-files", []any{smuggled, lane.frozen}),
		http.StatusBadRequest, "INVALID_ARGUMENTS")
	productionImportUnchanged(t, lane.project, treeBefore, databaseBefore)
}

// copyProductionImportContext copies the frozen three-key context, so a test can
// change one part of it without changing the caller's own copy.
func copyProductionImportContext(frozen map[string]any) map[string]any {
	copied := make(map[string]any, len(frozen))
	for key, value := range frozen {
		copied[key] = value
	}
	return copied
}

// productionImportContext copies the frozen context with one key replaced.
func productionImportContext(frozen map[string]any, key, value string) map[string]any {
	copied := copyProductionImportContext(frozen)
	copied[key] = value
	return copied
}

// productionImportUnchanged fails the test if the project tree or the author
// database changed. The database digest is a whole-file digest taken over a
// separate read-only connection, so it observes the bytes the author has.
func productionImportUnchanged(t *testing.T, projectPath, tree, database string) {
	t.Helper()
	if got := productionCloudBackupTreeDigest(t, projectPath); got != tree {
		t.Fatalf("a selection changed the project tree: %s != %s", got, tree)
	}
	if got := productionCloudBackupDatabaseDigest(t, projectPath); got != database {
		t.Fatalf("a selection changed the author database: %s != %s", got, database)
	}
}
