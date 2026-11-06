//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/librarypath"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// The fixture below lives entirely inside one disposable t.TempDir(): the
// production data root, one admitted project the lease names, a library with one
// book directory, one archive file, one output directory and one planted link out
// of the library, plus an outside tree nothing may reach. Every path is created by
// the fixture, never by an operation under test.
const (
	productionPickerFixtureTab       = "client_picker_author1"
	productionPickerFixtureOtherTab  = "client_picker_other1"
	productionPickerFixtureBook      = "第一部"
	productionPickerFixtureChapter   = "第一章.md"
	productionPickerFixtureChapter2  = "第一章：他推开门。"
	productionPickerFixtureArchive   = "novel.anovel"
	productionPickerFixtureArchive2  = "archive-bytes-not-a-real-zip"
	productionPickerFixtureOutput    = "exports"
	productionPickerFixtureOutside   = "outside-secret.txt"
	productionPickerFixtureOutside2  = "sealed outside bytes no picker may serve"
	productionPickerFixtureMissing   = "尚未存在的书"
	productionPickerFixtureManifest  = "旧项目"
	productionPickerFixtureUploadA   = "upload-author-1"
	productionPickerFixtureUploadB   = "upload-author-2"
	productionPickerFixtureFolderA   = "folder-author-1"
	productionPickerFixtureFolderBad = "folder-author-2"
	productionPickerFixtureFolderPkg = "folder-author-3"
	productionPickerFixtureKnowledge = "素材/世界观.md"
	productionPickerFixtureSkillName = "picker-skill"
	productionPickerFixtureSkillBody = "---\nname: picker-skill\ndescription: 本地写作技能夹具。\nlanguage: zh-CN\n---\n\n写一段开门见山的开头。\n"
	productionPickerFixtureSkillHard = "---\nname: picker-script\ndescription: 需要执行脚本的技能。\nlanguage: zh-CN\n---\n\n先运行 scripts/build.sh 再动笔。\n"
	productionPickerFixtureSkillBad  = "---\nname: picker-bundle\ndescription: 带附件的技能。\nlanguage: zh-CN\n---\n\n先读 [笔记](notes.md) 再动笔。\n"
	productionPickerFixtureNotes     = "笔记：先写冲突。\n"
	productionPickerFixtureRelease   = "release notes that are never part of a skill\n"
)

// productionPickerEntry is one drawn child of a chooser answer.
type productionPickerEntry struct {
	Name        string `json:"name"`
	IsDirectory bool   `json:"isDirectory"`
	Size        int64  `json:"size"`
}

// productionPickerAnswer is the single object every `dialog:select-*` picker
// answers, decoded exactly as the renderer reads it.
type productionPickerAnswer struct {
	Success       bool                    `json:"success"`
	GrantID       string                  `json:"grantId"`
	DisplayName   string                  `json:"displayName"`
	RootID        string                  `json:"rootId"`
	RelativePath  string                  `json:"relativePath"`
	Identity      string                  `json:"identity"`
	Kind          string                  `json:"kind"`
	Exists        bool                    `json:"exists"`
	Creatable     bool                    `json:"creatable"`
	Path          string                  `json:"path"`
	Truncated     bool                    `json:"truncated"`
	MaxEntries    int                     `json:"maxEntries"`
	SuggestedName string                  `json:"suggestedName"`
	Entries       []productionPickerEntry `json:"entries"`
}

// productionPickerFixtureUploads is the upload seam as the existing staging owner
// would satisfy it: every id is keyed by the cookie session and tab that staged it,
// so a foreign id is indistinguishable from one that was never staged.
type productionPickerFixtureUploads struct {
	mu      sync.Mutex
	files   map[string]productionPickerFixtureFile
	folders map[string]productionPickerFixtureFolder
}

type productionPickerFixtureFile struct {
	name string
	data []byte
}

type productionPickerFixtureFolder struct {
	name  string
	files []productionPickerUploaded
}

func newProductionPickerFixtureUploads() *productionPickerFixtureUploads {
	return &productionPickerFixtureUploads{
		files:   map[string]productionPickerFixtureFile{},
		folders: map[string]productionPickerFixtureFolder{},
	}
}

func productionPickerFixtureUploadKey(session, client, id string) string {
	return session + "\x00" + client + "\x00" + id
}

func (u *productionPickerFixtureUploads) stage(session, client, id, name string, data []byte) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.files[productionPickerFixtureUploadKey(session, client, id)] = productionPickerFixtureFile{name: name, data: data}
}

func (u *productionPickerFixtureUploads) stageFolder(session, client, id, name string, files []productionPickerUploaded) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.folders[productionPickerFixtureUploadKey(session, client, id)] = productionPickerFixtureFolder{name: name, files: files}
}

func (u *productionPickerFixtureUploads) ReadUpload(session, client, id string) (string, []byte, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	staged, ok := u.files[productionPickerFixtureUploadKey(session, client, id)]
	if !ok {
		return "", nil, errors.New("PICKER_FIXTURE_UPLOAD_UNKNOWN")
	}
	return staged.name, staged.data, nil
}

func (u *productionPickerFixtureUploads) ReadFolder(session, client, id string) (string, []productionPickerUploaded, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	staged, ok := u.folders[productionPickerFixtureUploadKey(session, client, id)]
	if !ok {
		return "", nil, errors.New("PICKER_FIXTURE_UPLOAD_UNKNOWN")
	}
	return staged.name, staged.files, nil
}

// productionPickerFixtureInstalled is one publication the fake owner recorded.
type productionPickerFixtureInstalled struct {
	name string
	raw  string
}

// productionPickerFixture is one open lane with the ten names bound, the two seams
// installed, and two callers: the one that opened the project and one other tab.
type productionPickerFixture struct {
	server    *transport.Server
	open      *productionOpen
	pickers   *productionBrowserPickers
	uploads   *productionPickerFixtureUploads
	installed []productionPickerFixtureInstalled
	author    *productionDurableClient
	other     *productionDurableClient
	library   string
	project   string
	outside   string
}

// newProductionPickerFixture opens the real production lane over the disposable
// data root and installs exactly two binders on it: the project lifecycle (so
// project:open can issue the lease the pickers resolve against) and this slice's
// ten names, with the upload and publication owners the production lane does not
// yet have. The rest of the lane is untouched.
func newProductionPickerFixture(t *testing.T) *productionPickerFixture {
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
		"go-production-browser-pickers")
	t.Cleanup(func() {
		server.Close()
		if closeErr := open.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if bindErr := server.BindProjects(open.durable); bindErr != nil {
		t.Fatal(bindErr)
	}
	fixture := &productionPickerFixture{
		server:  server,
		open:    open,
		uploads: newProductionPickerFixtureUploads(),
		library: filepath.Join(root, productionLibraryDirectory),
		project: projectPath,
		outside: t.TempDir(),
	}
	pickers := newProductionBrowserPickers(open)
	pickers.uploads = fixture.uploads
	pickers.publish = func(name, raw string) error {
		fixture.installed = append(fixture.installed, productionPickerFixtureInstalled{name: name, raw: raw})
		return nil
	}
	if bindErr := pickers.Bind(server); bindErr != nil {
		t.Fatalf("bind the picker names: %v", bindErr)
	}
	fixture.pickers = pickers
	// The library side, laid out after the lane is open so the startup catalog sees
	// only what its own fixture seeded.
	for _, directory := range []string{
		filepath.Join(fixture.library, productionPickerFixtureBook),
		filepath.Join(fixture.library, productionPickerFixtureOutput),
		filepath.Join(fixture.library, "素材"),
		filepath.Join(fixture.library, productionPickerFixtureManifest),
		fixture.outside,
	} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatalf("create %v: %v", directory, err)
		}
	}
	// One file per line, as pairs rather than a map: the seeding order then cannot
	// matter and no alignment column has to be kept in step with a longer path.
	for _, seed := range []struct{ path, body string }{
		{filepath.Join(fixture.library, productionPickerFixtureBook, productionPickerFixtureChapter), productionPickerFixtureChapter2},
		{filepath.Join(fixture.library, productionPickerFixtureBook, "第二章.md"), productionPickerFixtureChapter2},
		{filepath.Join(fixture.library, productionPickerFixtureArchive), productionPickerFixtureArchive2},
		{filepath.Join(fixture.library, "素材", "世界观.md"), productionPickerFixtureNotes},
		{filepath.Join(fixture.library, productionPickerFixtureManifest, librarypath.ProjectManifestName), "{}"},
		{filepath.Join(fixture.outside, productionPickerFixtureOutside), productionPickerFixtureOutside2},
	} {
		if err := os.WriteFile(seed.path, []byte(seed.body), 0o600); err != nil {
			t.Fatalf("seed %v: %v", seed.path, err)
		}
	}
	// A link inside the library that points out of it: the escape every picker has
	// to refuse, and to refuse as a link rather than as a path.
	if err := os.Symlink(fixture.outside, filepath.Join(fixture.library, "逃走")); err != nil {
		t.Fatalf("plant the escape link: %v", err)
	}
	fixture.author = newProductionDurableClient(t, server, productionPickerFixtureTab)
	var opened productionDurableOpenWire
	fixture.author.decode(t, "project:open", []any{projectPath, "picker-open-1", projectPath}, &opened)
	if !opened.Success || !opened.DBReady || opened.Error != "" {
		t.Fatalf("project:open refused the admitted project: %+v", opened)
	}
	fixture.other = newProductionDurableClient(t, server, productionPickerFixtureOtherTab)
	return fixture
}

// productionPickerReply is one call's status, symbolic error code and raw result,
// so a test can assert a transport refusal and a business envelope alike.
type productionPickerReply struct {
	Status int
	Code   string
	Result json.RawMessage
}

// call posts one operation through the frozen session and splits the answer.
func (f *productionPickerFixture) call(t *testing.T, client *productionDurableClient, operation string, args []any) productionPickerReply {
	t.Helper()
	response := client.call(t, operation, args)
	if response.Code == http.StatusNotImplemented {
		t.Fatalf("%v is still GO_OPERATION_NOT_MIGRATED in the production lane: %s", operation, response.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("%v: %v %s", operation, err, response.Body.String())
	}
	reply := productionPickerReply{Status: response.Code}
	if failure, ok := body["error"].(map[string]any); ok {
		if code, ok := failure["code"].(string); ok {
			reply.Code = code
		}
	}
	if raw, ok := body["result"]; ok {
		encoded, err := json.Marshal(raw)
		if err != nil {
			t.Fatalf("%v: %v", operation, err)
		}
		reply.Result = encoded
	}
	return reply
}

// answer decodes one `dialog:select-*` acceptance.
func (r productionPickerReply) answer(t *testing.T) productionPickerAnswer {
	t.Helper()
	if r.Status != http.StatusOK {
		t.Fatalf("a picker refused with HTTP %d %s; want an answer", r.Status, r.Code)
	}
	var answer productionPickerAnswer
	if err := json.Unmarshal(r.Result, &answer); err != nil {
		t.Fatalf("decode the picker answer: %v %s", err, r.Result)
	}
	if !answer.Success || answer.GrantID == "" {
		t.Fatalf("the picker answered without a success and a grant: %s", r.Result)
	}
	return answer
}

// business decodes one knowledge or skills envelope, which is HTTP 200 whether it
// succeeded or refused by name.
func (r productionPickerReply) business(t *testing.T) map[string]any {
	t.Helper()
	if r.Status != http.StatusOK {
		t.Fatalf("an envelope name refused with HTTP %d %s; want a business answer", r.Status, r.Code)
	}
	envelope := map[string]any{}
	if err := json.Unmarshal(r.Result, &envelope); err != nil {
		t.Fatalf("decode the envelope: %v %s", err, r.Result)
	}
	return envelope
}

// refused asserts one transport refusal by status and symbolic code.
func (f *productionPickerFixture) refused(t *testing.T, client *productionDurableClient, operation, label string, args []any, status int, code string) {
	t.Helper()
	reply := f.call(t, client, operation, args)
	if reply.Status != status {
		t.Fatalf("%v accepted a %v with HTTP %d %s; want HTTP %d", operation, label, reply.Status, reply.Code, status)
	}
	if reply.Code != code {
		t.Fatalf("%v refused a %v with %v; want %v", operation, label, reply.Code, code)
	}
}

// businessRefused asserts one envelope refusal by name.
func (f *productionPickerFixture) businessRefused(t *testing.T, client *productionDurableClient, operation, label string, args []any, code string) map[string]any {
	t.Helper()
	envelope := f.call(t, client, operation, args).business(t)
	if success, _ := envelope["success"].(bool); success {
		t.Fatalf("%v accepted a %v: %v", operation, label, envelope)
	}
	if text, _ := envelope["error"].(string); text != code {
		t.Fatalf("%v refused a %v with %v; want %v", operation, label, text, code)
	}
	return envelope
}

// productions is the ten names this slice binds.
func productionPickerFixtureNames() []string {
	return []string{
		productionPickerFolderName,
		productionPickerExportName,
		productionPickerArchiveExportName,
		productionPickerRestoreTargetName,
		productionPickerArchiveName,
		productionPickerLegacyName,
		productionPickerKnowledgeFiles,
		productionPickerKnowledgeFolder,
		productionPickerSkillPick,
		productionPickerSkillInstall,
	}
}

// entriesOf reads a chooser answer's drawn children.
func productionPickerFixtureEntries(t *testing.T, answer productionPickerAnswer) map[string]productionPickerEntry {
	t.Helper()
	drawn := map[string]productionPickerEntry{}
	for _, entry := range answer.Entries {
		drawn[entry.Name] = entry
	}
	return drawn
}

// textOf reads one string field of an envelope.
func productionPickerFixtureText(t *testing.T, value map[string]any, key string) string {
	t.Helper()
	text, ok := value[key].(string)
	if !ok {
		t.Fatalf("the envelope carries no string field %v: %v", key, value)
	}
	return text
}

// slotOf reads the upload slot a knowledge or skills envelope carries.
func productionPickerFixtureSlot(t *testing.T, value map[string]any) map[string]any {
	t.Helper()
	slot, ok := value["upload"].(map[string]any)
	if !ok {
		t.Fatalf("the envelope carries no upload slot: %v", value)
	}
	return slot
}

// TestProductionBrowserPickersBindTheTenNames proves the ten names are bound on a
// real server, sorted as the transport reports them, and that nothing else was
// added to the lane by this slice.
func TestProductionBrowserPickersBindTheTenNames(t *testing.T) {
	fixture := newProductionPickerFixture(t)
	bound := map[string]bool{}
	for _, name := range fixture.server.BoundOperations() {
		bound[name] = true
	}
	for _, name := range productionPickerFixtureNames() {
		if !bound[name] {
			t.Fatalf("%v is not bound on the production lane", name)
		}
	}
	if len(productionPickerFixtureNames()) != 10 {
		t.Fatalf("this slice binds %d names; want 10", len(productionPickerFixtureNames()))
	}
	// The eight `dialog:select-*` and the two `skills:*` names must be exactly the
	// set that became 501-free: a name bound here that the renderer never sends, or
	// a renderer name left out, is the bug this assertion is for.
	want := []string{
		productionPickerArchiveExportName,
		productionPickerArchiveName,
		productionPickerExportName,
		productionPickerFolderName,
		productionPickerKnowledgeFiles,
		productionPickerKnowledgeFolder,
		productionPickerLegacyName,
		productionPickerRestoreTargetName,
		productionPickerSkillInstall,
		productionPickerSkillPick,
	}
	sort.Strings(want)
	got := append([]string{}, productionPickerFixtureNames()...)
	sort.Strings(got)
	for index := range want {
		if want[index] != got[index] {
			t.Fatalf("the bound set is %v; want %v", got, want)
		}
	}
}

// TestProductionBrowserPickersAnswerChooserQueries is the acceptance half: each of
// the six chooser names answers the directory or file it resolved, with the
// identity the server can re-resolve and the grant the shipped consumer spends.
func TestProductionBrowserPickersAnswerChooserQueries(t *testing.T) {
	fixture := newProductionPickerFixture(t)
	registry := fixture.pickers.grants.Grants()
	if registry == nil {
		t.Fatal("the process-wide grant registry is unavailable")
	}

	t.Run("select-folder-opens-the-library-root", func(t *testing.T) {
		answer := fixture.call(t, fixture.author, productionPickerFolderName, []any{"project-open"}).answer(t)
		if answer.RootID != librarypath.LibraryRootID || answer.RelativePath != "" {
			t.Fatalf("the chooser answered %v/%v; want the library root", answer.RootID, answer.RelativePath)
		}
		if answer.Identity != librarypath.LibraryRootID {
			t.Fatalf("the identity is %v; want %v", answer.Identity, librarypath.LibraryRootID)
		}
		if answer.Kind != "directory" {
			t.Fatalf("the answer kind is %v; want directory", answer.Kind)
		}
		// The one path reveal: project:open's string form consumes it, so a consumer
		// forwarding this answer whole is never handed a value it cannot use. Only
		// this name reveals it, and only the root this lane resolved.
		if !filepath.IsAbs(answer.Path) || filepath.Base(answer.Path) != productionLibraryDirectory {
			t.Fatalf("the resolved path is %v; want the library root", answer.Path)
		}
		drawn := productionPickerFixtureEntries(t, answer)
		if !drawn[productionPickerFixtureBook].IsDirectory {
			t.Fatalf("the listing does not carry the book directory: %v", answer.Entries)
		}
		if drawn[productionPickerFixtureArchive].IsDirectory {
			t.Fatalf("the listing carries the archive as a directory: %v", answer.Entries)
		}
		if answer.MaxEntries != librarypath.DefaultListEntries {
			t.Fatalf("the answer capped the listing at %d; want the default", answer.MaxEntries)
		}
		// The token it minted is read-only and belongs to this caller alone.
		if _, err := registry.Redeem(answer.GrantID, productionFSGrantBinding(fixture.author), librarypath.OpRead); err != nil {
			t.Fatalf("the picker's read grant did not redeem: %v", err)
		}
		foreign := fixture.call(t, fixture.author, productionPickerFolderName, []any{"project-open"}).answer(t)
		if _, err := registry.Redeem(foreign.GrantID, productionFSGrantBinding(fixture.other), librarypath.OpRead); !errors.Is(err, librarypath.ErrCallerMismatch) {
			t.Fatalf("another tab redeemed this tab's picker grant: %v", err)
		}
	})

	t.Run("select-folder-descends-with-a-bounded-listing", func(t *testing.T) {
		answer := fixture.call(t, fixture.author, productionPickerFolderName, []any{map[string]any{
			"rootId":       librarypath.LibraryRootID,
			"relativePath": productionPickerFixtureBook,
			"maxEntries":   float64(1),
			"purpose":      productionPickerPurposeCreate,
		}}).answer(t)
		if answer.RelativePath != productionPickerFixtureBook || answer.Identity != librarypath.LibraryRootID+"/"+productionPickerFixtureBook {
			t.Fatalf("the chooser answered %v; want the book", answer.Identity)
		}
		if answer.Path != filepath.Join(fixture.library, productionPickerFixtureBook) {
			t.Fatalf("the resolved path is %v; want the book directory", answer.Path)
		}
		if len(answer.Entries) != 1 || !answer.Truncated || answer.MaxEntries != 1 {
			t.Fatalf("the bounded listing answered %d entries (truncated %v); want one of more", len(answer.Entries), answer.Truncated)
		}
		if answer.DisplayName != productionPickerFixtureBook {
			t.Fatalf("the display name is %v; want %v", answer.DisplayName, productionPickerFixtureBook)
		}
		if !answer.Exists || answer.Creatable {
			t.Fatalf("an existing book says exists=%v creatable=%v; want exists=true creatable=false", answer.Exists, answer.Creatable)
		}
	})

	t.Run("select-folder-creates-a-new-book-name", func(t *testing.T) {
		answer := fixture.call(t, fixture.author, productionPickerFolderName, []any{map[string]any{
			"rootId":       librarypath.LibraryRootID,
			"relativePath": productionPickerFixtureMissing,
			"purpose":      productionPickerPurposeCreate,
		}}).answer(t)
		// An unborn name is classified by the request form, not by the failed stat:
		// the answer says it may be created instead of claiming it is a file.
		if answer.Kind != "directory" {
			t.Fatalf("a new book name answered as %v; want the directory the caller may create", answer.Kind)
		}
		if answer.Exists || !answer.Creatable {
			t.Fatalf("a new book name says exists=%v creatable=%v; want exists=false creatable=true", answer.Exists, answer.Creatable)
		}
		if len(answer.Entries) != 0 || answer.Truncated {
			t.Fatalf("a new book name answered %v entries (truncated %v); want an empty listing", len(answer.Entries), answer.Truncated)
		}
	})

	t.Run("select-export-directory-carries-write-and-mkdir", func(t *testing.T) {
		answer := fixture.call(t, fixture.author, productionPickerExportName, []any{map[string]any{
			"rootId": librarypath.LibraryRootID, "relativePath": productionPickerFixtureOutput,
		}}).answer(t)
		if answer.Path != "" {
			// Only dialog:select-folder reveals a resolved path; a consumer of this name
			// reads the grant, so the answer must not carry a host path at all.
			t.Fatalf("the export picker revealed a host path: %v", answer.Path)
		}
		binding := productionFSGrantBinding(fixture.author)
		if _, err := registry.Redeem(answer.GrantID, binding, librarypath.OpWrite); err != nil {
			t.Fatalf("the export grant cannot write: %v", err)
		}
		if _, err := registry.Redeem(answer.GrantID, binding, librarypath.OpMkdir); err != nil {
			t.Fatalf("the export grant cannot mkdir: %v", err)
		}
	})

	t.Run("select-project-archive-export-echoes-the-suggested-name", func(t *testing.T) {
		answer := fixture.call(t, fixture.author, productionPickerArchiveExportName, []any{map[string]any{
			"rootId":        librarypath.LibraryRootID,
			"relativePath":  productionPickerFixtureOutput,
			"suggestedName": productionPickerFixtureArchive,
		}}).answer(t)
		if answer.SuggestedName != productionPickerFixtureArchive {
			t.Fatalf("the answer dropped the suggested name: %v", answer.SuggestedName)
		}
		if answer.Kind != "directory" {
			t.Fatalf("the archive destination answered as %v; want a directory", answer.Kind)
		}
	})

	t.Run("select-project-restore-target-is-read-only", func(t *testing.T) {
		answer := fixture.call(t, fixture.author, productionPickerRestoreTargetName, []any{map[string]any{
			"rootId": librarypath.LibraryRootID, "relativePath": productionPickerFixtureOutput,
		}}).answer(t)
		binding := productionFSGrantBinding(fixture.author)
		if _, err := registry.Redeem(answer.GrantID, binding, librarypath.OpRead); err != nil {
			t.Fatalf("the restore target grant cannot read: %v", err)
		}
		// A restored copy is author data: the token it hands back must not be able to
		// write it, even though the caller asked for a destination.
		if _, err := registry.Redeem(answer.GrantID, binding, librarypath.OpWrite); !errors.Is(err, librarypath.ErrGrantOperationDenied) {
			t.Fatalf("the restore target grant could write: %v", err)
		}
	})

	t.Run("select-project-archive-picks-a-file", func(t *testing.T) {
		answer := fixture.call(t, fixture.author, productionPickerArchiveName, []any{map[string]any{
			"rootId": librarypath.LibraryRootID, "relativePath": productionPickerFixtureArchive,
		}}).answer(t)
		if answer.Kind != "file" || len(answer.Entries) != 0 {
			t.Fatalf("the archive pick answered %v with %d entries; want one file", answer.Kind, len(answer.Entries))
		}
		if answer.RelativePath != productionPickerFixtureArchive {
			t.Fatalf("the archive identity is %v; want %v", answer.RelativePath, productionPickerFixtureArchive)
		}
	})

	t.Run("select-project-archive-browses-a-directory", func(t *testing.T) {
		answer := fixture.call(t, fixture.author, productionPickerArchiveName, []any{map[string]any{
			"rootId": librarypath.LibraryRootID,
		}}).answer(t)
		drawn := productionPickerFixtureEntries(t, answer)
		if _, listed := drawn[productionPickerFixtureArchive]; !listed {
			t.Fatalf("the browse step did not list the archive next to it: %v", answer.Entries)
		}
		if answer.Kind != "directory" {
			t.Fatalf("a browse step answered as %v; want the directory being browsed", answer.Kind)
		}
	})

	t.Run("select-legacy-project-reads-a-tree", func(t *testing.T) {
		answer := fixture.call(t, fixture.author, productionPickerLegacyName, []any{map[string]any{
			"rootId": librarypath.LibraryRootID, "relativePath": productionPickerFixtureBook,
		}}).answer(t)
		if answer.Identity != librarypath.LibraryRootID+"/"+productionPickerFixtureBook {
			t.Fatalf("the legacy pick identity is %v", answer.Identity)
		}
		if _, err := registry.Redeem(answer.GrantID, productionFSGrantBinding(fixture.author), librarypath.OpRead); err != nil {
			t.Fatalf("the legacy import grant cannot read: %v", err)
		}
	})
}

// TestProductionBrowserPickersRefuseWithoutWriting runs every refusal between two
// tree digests, so "a refused picker writes nothing" is measured rather than
// asserted.
func TestProductionBrowserPickersRefuseWithoutWriting(t *testing.T) {
	fixture := newProductionPickerFixture(t)
	libraryBefore := productionFSSnapshot(t, fixture.library)
	projectBefore := productionFSSnapshot(t, fixture.project)
	outsideBefore := productionFSSnapshot(t, fixture.outside)

	for _, name := range []string{
		productionPickerFolderName,
		productionPickerExportName,
		productionPickerArchiveExportName,
		productionPickerRestoreTargetName,
		productionPickerArchiveName,
		productionPickerLegacyName,
	} {
		operation := name
		t.Run("traversal-"+strings.NewReplacer(":", "-").Replace(operation), func(t *testing.T) {
			fixture.refused(t, fixture.author, operation, "traversal", []any{map[string]any{
				"rootId": librarypath.LibraryRootID, "relativePath": "../" + productionPickerFixtureOutside,
			}}, http.StatusForbidden, productionPickerPathRefusedCode)
			fixture.refused(t, fixture.author, operation, "nested traversal", []any{map[string]any{
				"rootId": librarypath.LibraryRootID, "relativePath": productionPickerFixtureBook + "/../" + productionPickerFixtureOutput,
			}}, http.StatusForbidden, productionPickerPathRefusedCode)
			fixture.refused(t, fixture.author, operation, "an absolute path", []any{map[string]any{
				"rootId": librarypath.LibraryRootID, "relativePath": fixture.outside,
			}}, http.StatusForbidden, productionPickerPathRefusedCode)
			fixture.refused(t, fixture.author, operation, "a hidden name", []any{map[string]any{
				"rootId": librarypath.LibraryRootID, "relativePath": ".ssh",
			}}, http.StatusForbidden, productionPickerPathRefusedCode)
		})
		t.Run("symlink-escape-"+strings.NewReplacer(":", "-").Replace(operation), func(t *testing.T) {
			fixture.refused(t, fixture.author, operation, "a link out of the library", []any{map[string]any{
				"rootId": librarypath.LibraryRootID, "relativePath": "逃走/" + productionPickerFixtureOutside,
			}}, http.StatusForbidden, productionPickerEscapeCode)
		})
	}

	t.Run("a-host-path-is-not-a-field", func(t *testing.T) {
		// The refusal that matters most: a body naming a host path is refused as
		// arguments, not interpreted as a path.
		for _, name := range []string{productionPickerFolderName, productionPickerKnowledgeFiles} {
			fixture.refused(t, fixture.author, name, "a body path", []any{map[string]any{
				"path": fixture.outside,
			}}, http.StatusBadRequest, productionPickerInvalidArguments)
			fixture.refused(t, fixture.author, name, "a grant id in the body", []any{map[string]any{
				"grantId": "not-a-grant",
			}}, http.StatusBadRequest, productionPickerInvalidArguments)
			fixture.refused(t, fixture.author, name, "two arguments", []any{"project-open", "project-open"},
				http.StatusBadRequest, productionPickerInvalidArguments)
		}
	})

	t.Run("an-unknown-root-is-not-addressable", func(t *testing.T) {
		fixture.refused(t, fixture.author, productionPickerFolderName, "a root this lane never had", []any{map[string]any{
			"rootId": "downloads",
		}}, http.StatusForbidden, productionPickerRootUnknownCode)
	})

	t.Run("a-missing-pick-is-refused-by-name", func(t *testing.T) {
		fixture.refused(t, fixture.author, productionPickerLegacyName, "a directory that does not exist", []any{map[string]any{
			"rootId": librarypath.LibraryRootID, "relativePath": productionPickerFixtureMissing,
		}}, http.StatusNotFound, productionPickerNotFoundCode)
		fixture.refused(t, fixture.author, productionPickerArchiveName, "a file that does not exist", []any{map[string]any{
			"rootId":       librarypath.LibraryRootID,
			"relativePath": "novel-missing.anovel",
		}}, http.StatusNotFound, productionPickerNotFoundCode)
		fixture.refused(t, fixture.author, productionPickerFolderName, "a book that does not exist for opening", []any{map[string]any{
			"rootId":       librarypath.LibraryRootID,
			"relativePath": productionPickerFixtureMissing,
			"purpose":      productionPickerPurposeOpen,
		}}, http.StatusNotFound, productionPickerNotFoundCode)
	})

	t.Run("an-output-pick-never-names-author-data", func(t *testing.T) {
		// A project tree is not an export destination: the refusal happens before the
		// mint, so the page never holds a grant that could try to write it.
		fixture.refused(t, fixture.author, productionPickerExportName, "the admitted project root", []any{map[string]any{
			"rootId": librarypath.ProjectRootID,
		}}, http.StatusForbidden, productionPickerAuthorDataCode)
		// The real production project tree: its manifest sits under the hidden
		// storage directory, so no allowlist resolution can address it. It is
		// refused because this lane admitted it as a project, open or not.
		fixture.refused(t, fixture.author, productionPickerExportName, "an admitted project tree", []any{map[string]any{
			"rootId":       librarypath.LibraryRootID,
			"relativePath": productionDurableProjectName,
		}}, http.StatusForbidden, productionPickerAuthorDataCode)
		// And a directory holding the marker at the name the detector probes,
		// which this lane never admitted: the visible check has to stand alone.
		fixture.refused(t, fixture.author, productionPickerExportName, "a directory holding a project manifest", []any{map[string]any{
			"rootId":       librarypath.LibraryRootID,
			"relativePath": productionPickerFixtureManifest,
		}}, http.StatusForbidden, productionPickerAuthorDataCode)
		fixture.refused(t, fixture.author, productionPickerArchiveExportName, "a directory holding a project manifest", []any{map[string]any{
			"rootId":       librarypath.LibraryRootID,
			"relativePath": productionPickerFixtureManifest,
		}}, http.StatusForbidden, productionPickerAuthorDataCode)
		// An output directory that is merely a sibling of a project is fine, so the
		// refusal is about author data and not about the library.
		answer := fixture.call(t, fixture.author, productionPickerExportName, []any{map[string]any{
			"rootId":       librarypath.LibraryRootID,
			"relativePath": productionPickerFixtureOutput,
		}}).answer(t)
		if !strings.HasPrefix(answer.Identity, librarypath.LibraryRootID) {
			t.Fatalf("the sibling export destination answered %v", answer.Identity)
		}
		fixture.refused(t, fixture.author, productionPickerArchiveExportName, "the admitted project root", []any{map[string]any{
			"rootId": librarypath.ProjectRootID,
		}}, http.StatusForbidden, productionPickerAuthorDataCode)
	})

	productionPickerUnchanged(t, "library", libraryBefore, productionFSSnapshot(t, fixture.library))
	productionPickerUnchanged(t, "project", projectBefore, productionFSSnapshot(t, fixture.project))
	productionPickerUnchanged(t, "outside", outsideBefore, productionFSSnapshot(t, fixture.outside))
}

// productionPickerUnchanged asserts one tree digest pair describes the same tree:
// no entry added, none removed, none rewritten.
func productionPickerUnchanged(t *testing.T, label string, before, after map[string]string) {
	t.Helper()
	for path, digest := range before {
		if after[path] != digest {
			t.Fatalf("a refused picker changed %s: %v", label, path)
		}
	}
	for path := range after {
		if _, existed := before[path]; !existed {
			t.Fatalf("a refused picker created %s: %v", label, path)
		}
	}
}

// TestProductionBrowserPickersRequireUploadsForKnowledge is the knowledge half: the
// answer is the upload slot until bytes arrive, and then it is the verified identity
// of exactly those bytes - never a grant for a file the server cannot see.
func TestProductionBrowserPickersRequireUploadsForKnowledge(t *testing.T) {
	fixture := newProductionPickerFixture(t)
	libraryBefore := productionFSSnapshot(t, fixture.library)

	t.Run("answers-the-slot-before-any-upload", func(t *testing.T) {
		envelope := fixture.call(t, fixture.author, productionPickerKnowledgeFiles, []any{map[string]any{}}).business(t)
		if success, _ := envelope["success"].(bool); !success {
			t.Fatalf("the knowledge pick refused before any upload: %v", envelope)
		}
		files, ok := envelope["files"].([]any)
		if !ok || len(files) != 0 {
			t.Fatalf("the slot answer carries files: %v", envelope["files"])
		}
		if grants, ok := envelope["grants"].([]any); !ok || len(grants) != 0 {
			t.Fatalf("the slot answer carries grants: %v", envelope["grants"])
		}
		slot := productionPickerFixtureSlot(t, envelope)
		if productionPickerFixtureText(t, slot, "url") != productionPickerUploadRoute {
			t.Fatalf("the slot points at %v; want the staging route", slot["url"])
		}
		resume, ok := slot["resume"].(map[string]any)
		if !ok {
			t.Fatalf("the slot carries no resume step: %v", slot)
		}
		if productionPickerFixtureText(t, resume, "operation") != productionPickerKnowledgeFiles {
			t.Fatalf("the resume names %v; want this name again", resume["operation"])
		}
		if productionPickerFixtureText(t, resume, "idField") != "uploadIds" {
			t.Fatalf("the resume field is %v; want uploadIds", resume["idField"])
		}
	})

	t.Run("refuses-an-id-that-was-never-staged", func(t *testing.T) {
		fixture.businessRefused(t, fixture.author, productionPickerKnowledgeFiles, "an id this server never minted",
			[]any{map[string]any{"uploadIds": []any{"never-staged"}}}, productionPickerUploadUnknownCode)
	})

	t.Run("refuses-another-tabs-upload", func(t *testing.T) {
		fixture.uploads.stage(fixture.other.cookie.Value, fixture.other.tab, "shared-upload-id",
			"世界观.md", []byte(productionPickerFixtureNotes))
		fixture.businessRefused(t, fixture.author, productionPickerKnowledgeFiles, "another tab's upload",
			[]any{map[string]any{"uploadIds": []any{"shared-upload-id"}}}, productionPickerUploadUnknownCode)
	})

	t.Run("verifies-a-staged-file-without-granting-it", func(t *testing.T) {
		fixture.uploads.stage(fixture.author.cookie.Value, fixture.author.tab, productionPickerFixtureUploadA,
			"世界观.md", []byte(productionPickerFixtureNotes))
		envelope := fixture.call(t, fixture.author, productionPickerKnowledgeFiles,
			[]any{map[string]any{"uploadIds": []any{productionPickerFixtureUploadA}}}).business(t)
		if success, _ := envelope["success"].(bool); !success {
			t.Fatalf("the knowledge pick refused a staged upload: %v", envelope)
		}
		files, ok := envelope["files"].([]any)
		if !ok || len(files) != 1 {
			t.Fatalf("the answer verified %v files; want one", envelope["files"])
		}
		file, ok := files[0].(map[string]any)
		if !ok {
			t.Fatalf("the verified file is not an object: %v", files[0])
		}
		if productionPickerFixtureText(t, file, "uploadId") != productionPickerFixtureUploadA {
			t.Fatalf("the verified file carries the wrong id: %v", file)
		}
		if productionPickerFixtureText(t, file, "grantId") != "" {
			t.Fatalf("the verified file carries a grant: %v", file)
		}
		if readable, _ := file["readable"].(bool); readable {
			t.Fatalf("the verified file claims to be readable by the server: %v", file)
		}
		if productionPickerFixtureText(t, file, "displayName") != "世界观.md" {
			t.Fatalf("the verified file dropped its display name: %v", file)
		}
		if productionPickerFixtureText(t, file, "contentSha256") == "" {
			t.Fatalf("the verified file carries no digest: %v", file)
		}
		// The continuation the page needs, and the one import name this lane binds.
		if productionPickerFixtureText(t, envelope, "next") != "kb:import-planning-text" {
			t.Fatalf("the answer names no continuation: %v", envelope)
		}
	})

	t.Run("refuses-bytes-that-are-not-text", func(t *testing.T) {
		fixture.uploads.stage(fixture.author.cookie.Value, fixture.author.tab, productionPickerFixtureUploadB,
			"binary.md", []byte{0xff, 0xfe, 0x00})
		fixture.businessRefused(t, fixture.author, productionPickerKnowledgeFiles, "non-UTF-8 bytes",
			[]any{map[string]any{"uploadIds": []any{productionPickerFixtureUploadB}}}, productionPickerUploadNotTextCode)
	})

	t.Run("folds-a-folder-upload", func(t *testing.T) {
		// What the browser's folder picker actually forwards: the author's own tree.
		// A member name is a label for bytes that were already uploaded, so it may be
		// Chinese, carry a subdirectory or start with a dot - none of that is a host
		// path, and none of it may be refused as one.
		members := []string{"世界观.md", "人物.md", "设定/人物小传.md", ".笔记.md"}
		staged := make([]productionPickerUploaded, 0, len(members))
		for _, member := range members {
			staged = append(staged, productionPickerUploaded{Relative: member, Data: []byte(productionPickerFixtureNotes)})
		}
		fixture.uploads.stageFolder(fixture.author.cookie.Value, fixture.author.tab, productionPickerFixtureFolderA,
			"素材", staged)
		envelope := fixture.call(t, fixture.author, productionPickerKnowledgeFolder,
			[]any{map[string]any{"folderUploadId": productionPickerFixtureFolderA}}).business(t)
		if success, _ := envelope["success"].(bool); !success {
			t.Fatalf("the folder pick refused a staged folder: %v", envelope)
		}
		files, ok := envelope["files"].([]any)
		if !ok || len(files) != len(members) {
			t.Fatalf("the folder answer verified %v files; want %v", envelope["files"], len(members))
		}
		// The answer names every member exactly as it was staged: a label is
		// echoed, never rewritten. Order is the owner's business, so compare as a set.
		answered := map[string]bool{}
		for _, item := range files {
			file, ok := item.(map[string]any)
			if !ok {
				t.Fatalf("a verified file is not an object: %v", item)
			}
			answered[productionPickerFixtureText(t, file, "displayName")] = true
		}
		for _, member := range members {
			if !answered[member] {
				t.Fatalf("the folder answer lost the member %v: %v", member, answered)
			}
		}
		if len(answered) != len(members) {
			t.Fatalf("the folder answer named %v members; want %v", len(answered), len(members))
		}
		slot := productionPickerFixtureSlot(t, envelope)
		resume, ok := slot["resume"].(map[string]any)
		if !ok || productionPickerFixtureText(t, resume, "idField") != "folderUploadId" {
			t.Fatalf("the folder slot does not resume on folderUploadId: %v", slot)
		}
	})

	t.Run("refuses-a-folder-member-that-is-not-a-name", func(t *testing.T) {
		// The same walk refuses what is not a name at all: a member that leaves the
		// upload, one that is absolute, one that uses the other separator, and one
		// nested past the bound. Each is refused for the folder as a whole, because
		// answering with part of the folder would answer a question nobody asked.
		deep := ""
		for index := 0; index < productionPickerMaxUploadDepth+1; index++ {
			deep += "层/"
		}
		for _, member := range []string{"../世界观.md", "/etc/passwd", "设定\\人物.md", deep + "世界观.md"} {
			fixture.uploads.stageFolder(fixture.author.cookie.Value, fixture.author.tab, productionPickerFixtureFolderBad,
				"素材", []productionPickerUploaded{{Relative: member, Data: []byte(productionPickerFixtureNotes)}})
			fixture.businessRefused(t, fixture.author, productionPickerKnowledgeFolder, member,
				[]any{map[string]any{"folderUploadId": productionPickerFixtureFolderBad}}, productionPickerPathRefusedCode)
		}
	})

	t.Run("refuses-when-the-lane-has-no-upload-owner", func(t *testing.T) {
		uploads := fixture.pickers.uploads
		fixture.pickers.uploads = nil
		t.Cleanup(func() { fixture.pickers.uploads = uploads })
		fixture.businessRefused(t, fixture.author, productionPickerKnowledgeFiles, "no staging owner",
			[]any{map[string]any{"uploadIds": []any{"any-id"}}}, productionPickerUploadUnavailCode)
		// The slot itself still answers: a page can prepare the upload it will retry.
		envelope := fixture.call(t, fixture.author, productionPickerKnowledgeFolder, []any{map[string]any{}}).business(t)
		if slot := productionPickerFixtureSlot(t, envelope); productionPickerFixtureText(t, slot, "folderUploadUrl") != productionPickerUploadFolderRoute {
			t.Fatalf("the slot lost its folder route: %v", slot)
		}
	})

	productionPickerUnchanged(t, "library", libraryBefore, productionFSSnapshot(t, fixture.library))
}

// TestProductionBrowserPickersInstallUploadedSkills is the skills half: the pick
// inspects uploaded bytes and mints one single-use candidate, and the install
// publishes exactly the bytes it inspected - never a server path, and never over a
// package that arrived damaged.
func TestProductionBrowserPickersInstallUploadedSkills(t *testing.T) {
	fixture := newProductionPickerFixture(t)
	libraryBefore := productionFSSnapshot(t, fixture.library)
	digestOf := func(body string) string { return productionPickerSHA256([]byte(body)) }

	pick := func(t *testing.T, client *productionDurableClient, args map[string]any) map[string]any {
		t.Helper()
		return fixture.call(t, client, productionPickerSkillPick, []any{args}).business(t)
	}
	install := func(t *testing.T, client *productionDurableClient, candidate string) map[string]any {
		t.Helper()
		return fixture.call(t, client, productionPickerSkillInstall, []any{candidate}).business(t)
	}
	candidateOf := func(t *testing.T, envelope map[string]any) string {
		t.Helper()
		if success, _ := envelope["success"].(bool); !success {
			t.Fatalf("the skill pick refused a valid package: %v", envelope)
		}
		inspection, ok := envelope["inspection"].(map[string]any)
		if !ok {
			t.Fatalf("the pick answered no inspection: %v", envelope)
		}
		if _, carried := inspection["content"]; carried {
			t.Fatalf("the inspection shipped the body the author chose: %v", inspection)
		}
		candidate := productionPickerFixtureText(t, inspection, "candidateId")
		if candidate == "" {
			t.Fatalf("the pick minted no candidate id: %v", inspection)
		}
		return candidate
	}

	t.Run("asks-for-an-upload-before-inspecting", func(t *testing.T) {
		envelope := fixture.businessRefused(t, fixture.author, productionPickerSkillPick, "a pick with no upload",
			[]any{map[string]any{"kind": productionPickerSkillFile}}, productionPickerUploadRequiredCode)
		slot := productionPickerFixtureSlot(t, envelope)
		if productionPickerFixtureText(t, slot, "nameHeader") != productionPickerUploadNameHeader {
			t.Fatalf("the skill slot does not name the upload header: %v", slot)
		}
	})

	t.Run("picks-and-installs-an-uploaded-skill-md", func(t *testing.T) {
		fixture.uploads.stage(fixture.author.cookie.Value, fixture.author.tab, productionPickerFixtureUploadB,
			"SKILL.md", []byte(productionPickerFixtureSkillBody))
		envelope := pick(t, fixture.author, map[string]any{
			"kind": productionPickerSkillFile, "uploadIds": []any{productionPickerFixtureUploadB},
		})
		candidate := candidateOf(t, envelope)
		inspection, _ := envelope["inspection"].(map[string]any)
		metadata, ok := inspection["metadata"].(map[string]any)
		if !ok {
			t.Fatalf("the inspection carries no metadata: %v", inspection)
		}
		if productionPickerFixtureText(t, metadata, "name") != productionPickerFixtureSkillName {
			t.Fatalf("the inspection read the name as %v; want %v", metadata["name"], productionPickerFixtureSkillName)
		}
		if compatible, _ := inspection["compatible"].(bool); !compatible {
			t.Fatalf("the fixture skill was judged incompatible: %v", inspection["reasons"])
		}
		installed := install(t, fixture.author, candidate)
		if success, _ := installed["success"].(bool); !success {
			t.Fatalf("the install refused the candidate it minted: %v", installed)
		}
		skill, ok := installed["skill"].(map[string]any)
		if !ok {
			t.Fatalf("the install answered no skill: %v", installed)
		}
		if productionPickerFixtureText(t, skill, "name") != productionPickerFixtureSkillName {
			t.Fatalf("the install published %v", skill["name"])
		}
		if len(fixture.installed) != 1 {
			t.Fatalf("the publisher ran %d times; want one", len(fixture.installed))
		}
		if fixture.installed[0].raw != productionPickerFixtureSkillBody {
			t.Fatalf("the publisher received other bytes than the pick inspected: %v", fixture.installed[0].name)
		}
	})

	t.Run("consumes-the-candidate-once", func(t *testing.T) {
		fixture.uploads.stage(fixture.author.cookie.Value, fixture.author.tab, productionPickerFixtureUploadA,
			"SKILL.md", []byte(productionPickerFixtureSkillBody))
		candidate := candidateOf(t, pick(t, fixture.author, map[string]any{
			"kind": productionPickerSkillFile, "uploadIds": []any{productionPickerFixtureUploadA},
		}))
		if success, _ := install(t, fixture.author, candidate)["success"].(bool); !success {
			t.Fatal("the first install of a fresh candidate was refused")
		}
		fixture.businessRefused(t, fixture.author, productionPickerSkillInstall, "a candidate already consumed",
			[]any{candidate}, productionPickerSkillCandidateCode)
	})

	t.Run("refuses-another-tabs-candidate", func(t *testing.T) {
		fixture.uploads.stage(fixture.author.cookie.Value, fixture.author.tab, productionPickerFixtureUploadB,
			"SKILL.md", []byte(productionPickerFixtureSkillBody))
		candidate := candidateOf(t, pick(t, fixture.author, map[string]any{
			"kind": productionPickerSkillFile, "uploadIds": []any{productionPickerFixtureUploadB},
		}))
		fixture.businessRefused(t, fixture.other, productionPickerSkillInstall, "another tab's candidate",
			[]any{candidate}, productionPickerSkillCandidateCode)
		// The refusal did not consume the owner's candidate: the author can still
		// confirm, which is what a shared registry with a caller key is for.
		if success, _ := install(t, fixture.author, candidate)["success"].(bool); !success {
			t.Fatal("a foreign refusal consumed the owner's candidate")
		}
	})

	t.Run("refuses-a-damaged-archive-without-installing", func(t *testing.T) {
		before := len(fixture.installed)
		fixture.uploads.stageFolder(fixture.author.cookie.Value, fixture.author.tab, productionPickerFixtureFolderBad,
			"picker-bundle", []productionPickerUploaded{
				{Relative: "SKILL.md", Data: []byte(productionPickerFixtureSkillBad)},
				{Relative: "notes.md", Data: []byte(productionPickerFixtureNotes)},
				// The manifest claims bytes that did not arrive: a damaged archive.
				{Relative: ".pagekin-bundle.json", Data: []byte(`{"version":1,"files":[{"path":"notes.md","sha256":"` +
					digestOf(productionPickerFixtureRelease) + `"}]}`)},
			})
		fixture.businessRefused(t, fixture.author, productionPickerSkillPick, "a damaged archive",
			[]any{map[string]any{"kind": productionPickerSkillFolder, "folderUploadId": productionPickerFixtureFolderBad}},
			productionPickerSkillDamagedCode)
		// Nothing was minted, so nothing can be installed: the confirm step has no
		// candidate to consume and the publisher never runs.
		fixture.businessRefused(t, fixture.author, productionPickerSkillInstall, "a candidate a damaged archive never minted",
			[]any{"never-minted-candidate"}, productionPickerSkillCandidateCode)
		if len(fixture.installed) != before {
			t.Fatalf("a damaged archive published %d skills", len(fixture.installed)-before)
		}
	})

	t.Run("refuses-an-intact-bundle-this-lane-cannot-carry", func(t *testing.T) {
		fixture.uploads.stageFolder(fixture.author.cookie.Value, fixture.author.tab, productionPickerFixtureFolderPkg,
			"picker-bundle", []productionPickerUploaded{
				{Relative: "SKILL.md", Data: []byte(productionPickerFixtureSkillBad)},
				{Relative: "notes.md", Data: []byte(productionPickerFixtureNotes)},
				{Relative: ".pagekin-bundle.json", Data: []byte(`{"version":1,"files":[{"path":"notes.md","sha256":"` +
					digestOf(productionPickerFixtureNotes) + `"}]}`)},
			})
		fixture.businessRefused(t, fixture.author, productionPickerSkillPick, "a package with bundled files",
			[]any{map[string]any{"kind": productionPickerSkillFolder, "folderUploadId": productionPickerFixtureFolderPkg}},
			productionPickerSkillUnsupportedCode)
	})

	t.Run("refuses-a-folder-member-that-names-a-path", func(t *testing.T) {
		fixture.uploads.stageFolder(fixture.author.cookie.Value, fixture.author.tab, productionPickerFixtureFolderA,
			"picker-bundle", []productionPickerUploaded{
				{Relative: "../SKILL.md", Data: []byte(productionPickerFixtureSkillBody)},
			})
		fixture.businessRefused(t, fixture.author, productionPickerSkillPick, "a traversal member",
			[]any{map[string]any{"kind": productionPickerSkillFolder, "folderUploadId": productionPickerFixtureFolderA}},
			productionPickerSkillPathCode)
	})

	t.Run("refuses-an-incompatible-skill-at-the-confirm", func(t *testing.T) {
		fixture.uploads.stage(fixture.author.cookie.Value, fixture.author.tab, productionPickerFixtureUploadA,
			"SKILL.md", []byte(productionPickerFixtureSkillHard))
		envelope := pick(t, fixture.author, map[string]any{
			"kind": productionPickerSkillFile, "uploadIds": []any{productionPickerFixtureUploadA},
		})
		if success, _ := envelope["success"].(bool); !success {
			t.Fatalf("the pick refused an inspectable package: %v", envelope)
		}
		inspection, _ := envelope["inspection"].(map[string]any)
		if compatible, _ := inspection["compatible"].(bool); compatible {
			t.Fatalf("a script-dependent skill was judged compatible: %v", inspection)
		}
		candidate := productionPickerFixtureText(t, inspection, "candidateId")
		fixture.businessRefused(t, fixture.author, productionPickerSkillInstall, "a script-dependent skill",
			[]any{candidate}, productionPickerSkillIncompatibleCode)
	})

	t.Run("refuses-a-source-that-is-not-markdown", func(t *testing.T) {
		fixture.uploads.stage(fixture.author.cookie.Value, fixture.author.tab, productionPickerFixtureUploadB,
			"notes.txt", []byte(productionPickerFixtureNotes))
		fixture.businessRefused(t, fixture.author, productionPickerSkillPick, "a non-markdown source",
			[]any{map[string]any{"kind": productionPickerSkillFile, "uploadIds": []any{productionPickerFixtureUploadB}}},
			productionPickerSkillMissingCode)
	})

	t.Run("refuses-when-the-lane-has-no-publisher", func(t *testing.T) {
		publish := fixture.pickers.publish
		fixture.pickers.publish = nil
		t.Cleanup(func() { fixture.pickers.publish = publish })
		fixture.uploads.stage(fixture.author.cookie.Value, fixture.author.tab, productionPickerFixtureUploadB,
			"SKILL.md", []byte(productionPickerFixtureSkillBody))
		candidate := candidateOf(t, pick(t, fixture.author, map[string]any{
			"kind": productionPickerSkillFile, "uploadIds": []any{productionPickerFixtureUploadB},
		}))
		fixture.businessRefused(t, fixture.author, productionPickerSkillInstall, "a lane with no publication owner",
			[]any{candidate}, productionPickerSkillPublishCode)
	})

	t.Run("refuses-when-the-lane-has-no-upload-owner", func(t *testing.T) {
		uploads := fixture.pickers.uploads
		fixture.pickers.uploads = nil
		t.Cleanup(func() { fixture.pickers.uploads = uploads })
		fixture.businessRefused(t, fixture.author, productionPickerSkillPick, "a lane with no staging owner",
			[]any{map[string]any{"kind": productionPickerSkillFile, "uploadIds": []any{"any-id"}}},
			productionPickerUploadUnavailCode)
	})

	productionPickerUnchanged(t, "library", libraryBefore, productionFSSnapshot(t, fixture.library))
}
