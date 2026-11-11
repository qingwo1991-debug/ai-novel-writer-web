//go:build linux

package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/promptskills"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/uploadstore"
)

// These tests walk the skills journey a browser walks, in order, over the real
// upload route and the real operation server: POST the package bytes, present the
// handle to skills:pick-local, then present the candidate to skills:install-local
// and read the published file back through internal/promptskills itself.
//
// What the two tests prove between them:
//
//	a damaged upload         pick refuses it by name, nothing is published
//	an archive with members  pick refuses it by name, nothing is published
//	a self-contained package pick mints one candidate; install publishes it, the
//	                         package's own ListUser reports it, and the file on
//	                         disk is the staged byte string at mode 0600
//	a consumed candidate     install refuses it by name, the tree is unchanged
//	another tab's candidate  install refuses it by name, the owner can still spend
//	                         it, which is what "caller-bound" has to mean
//	the publisher's own gate a package that is not self-contained, a package whose
//	                         frontmatter names another skill, an unsafe name and an
//	                         oversized body are all refused by Publish itself,
//	                         with the generation byte-identical afterwards
const (
	// productionSkillInstallBody is a self-contained package: one frontmatter
	// name, one description and prose. It names nothing, runs nothing and
	// references no file, so the verdict the GitHub path uses accepts it.
	productionSkillInstallBody = "---\nname: daily-review\ndescription: 逐章复核草稿\n---\n\n# 日更复核\n\n按章节顺序读一遍，指出前后矛盾，再改一处。\n"
	// productionSkillInstallForeignTab is a second tab on the same cookie session:
	// the candidate key is the session AND the tab, so a candidate minted by one
	// tab is not installable by the other.
	productionSkillInstallForeignTab = "client_skill_install_other_1"
)

// productionSkillInstallLane is one open production lane with the upload route
// mounted and the picker family bound the way production_open_linux.go binds it
// once the parent adds the publisher line, plus the data root the published skill
// is read back from.
type productionSkillInstallLane struct {
	root   string
	open   *productionOpen
	route  *productionUploadRoute
	mux    http.Handler
	server *transport.Server
}

func newProductionSkillInstallLane(t *testing.T) *productionSkillInstallLane {
	t.Helper()
	root, _ := productionDurableDataRoot(t)
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
		"go-production-skill-install")
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
	// The wiring under test: the canonical picker constructor, then the publish
	// port, then the family's own Bind.
	pickers := newProductionBrowserPickersFrom(open, route)
	if bindErr := bindProductionSkillInstall(pickers, newProductionSkillInstalls(open)); bindErr != nil {
		t.Fatal(bindErr)
	}
	if bindErr := pickers.Bind(server); bindErr != nil {
		t.Fatal(bindErr)
	}
	return &productionSkillInstallLane{root: root, open: open, route: route, mux: mux, server: server}
}

// generation is where the prompt templates and the installed skills live.
func (l *productionSkillInstallLane) generation() string {
	return productionWriteGeneration(l.root)
}

// send is one upload request with the headers the renderer sets
// (src/components/BrowserDialogs.tsx:92-95), including the session cookie so the
// staged id belongs to the same caller the operations run as.
func (l *productionSkillInstallLane) send(client *productionDurableClient, route, name string,
	body []byte, extra map[string]string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, route, bytes.NewReader(body))
	request.Header.Set("X-Novel-User", "production-author")
	request.Header.Set("X-Client-Id", client.tab)
	request.Header.Set("X-CSRF-Token", client.csrf)
	request.Header.Set("Origin", "http://"+productionDurableAddress)
	request.Header.Set("Content-Type", "application/octet-stream")
	request.AddCookie(client.cookie)
	if name != "" {
		request.Header.Set("X-Upload-Name", url.PathEscape(name))
	}
	for key, value := range extra {
		request.Header.Set(key, value)
	}
	response := httptest.NewRecorder()
	l.mux.ServeHTTP(response, request)
	return response
}

// stage uploads one file and returns its id.
func (l *productionSkillInstallLane) stage(t *testing.T, client *productionDurableClient,
	name string, data []byte) string {
	t.Helper()
	return productionUploadTestCreated(t, l.send(client, uploadstore.RouteFile, name, data, nil), "uploadId")
}

// archive stages one folder upload and its members, which is how a browser
// presents a package that carries more than one file.
func (l *productionSkillInstallLane) archive(t *testing.T, client *productionDurableClient,
	members map[string]string) string {
	t.Helper()
	response := l.send(client, uploadstore.RouteFolder, "", []byte("{}"),
		map[string]string{"Content-Type": "application/json"})
	folder := productionUploadTestCreated(t, response, "folderUploadId")
	for relative, body := range members {
		member := l.send(client, uploadstore.RouteFile, "SKILL.md", []byte(body),
			map[string]string{"X-Upload-Folder": folder, "X-Upload-Relative": url.PathEscape(relative)})
		productionUploadTestCreated(t, member, "uploadId")
	}
	return folder
}

// productionSkillPickInspection is the inspected half of the pick answer.
type productionSkillPickInspection struct {
	CandidateID   string   `json:"candidateId"`
	SourceKind    string   `json:"sourceKind"`
	SourceName    string   `json:"sourceName"`
	ContentSHA256 string   `json:"contentSha256"`
	BundledFiles  []string `json:"bundledFiles"`
}

// productionSkillPickWire is the skills:pick-local answer.
type productionSkillPickWire struct {
	Success    bool                         `json:"success"`
	Cancelled  bool                         `json:"cancelled"`
	Error      string                       `json:"error"`
	Inspection productionSkillPickInspection `json:"inspection"`
}

// productionSkillInstalled is the published half of the install answer.
type productionSkillInstalled struct {
	Name       string `json:"name"`
	Source     string `json:"source"`
	Version    string `json:"version"`
	Language   string `json:"language"`
	Compatible bool   `json:"compatible"`
	UTF8Bytes  int    `json:"utf8Bytes"`
}

// productionSkillInstallWire is the skills:install-local answer.
type productionSkillInstallWire struct {
	Success   bool                     `json:"success"`
	Cancelled bool                     `json:"cancelled"`
	Error     string                   `json:"error"`
	Skill     productionSkillInstalled `json:"skill"`
}

func (l *productionSkillInstallLane) pickFile(t *testing.T, client *productionDurableClient,
	uploadID string) productionSkillPickWire {
	t.Helper()
	var answer productionSkillPickWire
	client.decode(t, productionPickerSkillPick, []any{map[string]any{
		"kind": "file", "uploadIds": []any{uploadID},
	}}, &answer)
	return answer
}

func (l *productionSkillInstallLane) pickFolder(t *testing.T, client *productionDurableClient,
	folderID string) productionSkillPickWire {
	t.Helper()
	var answer productionSkillPickWire
	client.decode(t, productionPickerSkillPick, []any{map[string]any{
		"kind": "folder", "folderUploadId": folderID,
	}}, &answer)
	return answer
}

func (l *productionSkillInstallLane) install(t *testing.T, client *productionDurableClient,
	candidateID string) productionSkillInstallWire {
	t.Helper()
	var answer productionSkillInstallWire
	client.decode(t, productionPickerSkillInstall, []any{map[string]any{
		"candidateId": candidateID,
	}}, &answer)
	return answer
}

// unchanged fails the test if the committed generation - the prompt templates, the
// installed skills, models.json and config.json - is not byte-identical to want.
func (l *productionSkillInstallLane) unchanged(t *testing.T, want string) {
	t.Helper()
	if got := productionCloudBackupTreeDigest(t, l.generation()); got != want {
		t.Fatalf("a refusal changed the committed generation: %s != %s", got, want)
	}
}

// TestProductionSkillInstallPublishesOnlyStagedVerifiedPackages is the whole local
// install journey: the bytes arrive over the mounted route, the picker mints a
// caller-bound candidate, and the publisher writes the package into the committed
// generation's skills tree - or refuses by name and writes nothing.
func TestProductionSkillInstallPublishesOnlyStagedVerifiedPackages(t *testing.T) {
	lane := newProductionSkillInstallLane(t)
	author := newProductionDurableClient(t, lane.mux, productionEditorTab)
	empty := productionCloudBackupTreeDigest(t, lane.generation())

	// A damaged package: invalid UTF-8 is not a skill, so the pick refuses it and
	// no candidate exists to install.
	broken := lane.stage(t, author, "broken.md", []byte("# broke\xff\xfe\x00n"))
	if answer := lane.pickFile(t, author, broken); answer.Success || answer.Error != productionPickerSkillInvalidCode {
		t.Fatalf("a damaged upload was inspected: %+v", answer)
	}
	lane.unchanged(t, empty)

	// An archive that carries its own members: the GitHub install path publishes
	// SKILL.md alone, so the pick refuses the package instead of promising an
	// install that would silently drop notes.md.
	archive := lane.archive(t, author, map[string]string{
		"SKILL.md": "---\nname: bundled-review\n---\n\n# 拆文\n\n先读 [说明](notes.md)。\n",
		"notes.md": "# 说明\n\n细节在这里。\n",
	})
	if answer := lane.pickFolder(t, author, archive); answer.Success || answer.Error != productionPickerSkillUnsupportedCode {
		t.Fatalf("an archive with bundled files was accepted: %+v", answer)
	}
	lane.unchanged(t, empty)

	// A self-contained package: pick scores it, install publishes it.
	upload := lane.stage(t, author, "daily-review.md", []byte(productionSkillInstallBody))
	picked := lane.pickFile(t, author, upload)
	if !picked.Success || picked.Error != "" {
		t.Fatalf("pick refused a self-contained package: %+v", picked)
	}
	// The pick reports the name the author chose (the staged display name); the
	// installed name is the package's own frontmatter name, asserted below.
	if picked.Inspection.CandidateID == "" || picked.Inspection.SourceName != "daily-review.md" ||
		picked.Inspection.SourceKind != "file" || len(picked.Inspection.BundledFiles) != 0 {
		t.Fatalf("pick minted an unexpected candidate: %+v", picked.Inspection)
	}
	installed := lane.install(t, author, picked.Inspection.CandidateID)
	if !installed.Success || installed.Error != "" {
		t.Fatalf("install refused a verified package: %+v", installed)
	}
	if installed.Skill.Name != "daily-review" || installed.Skill.Source != "user" ||
		!installed.Skill.Compatible || installed.Skill.UTF8Bytes <= 0 {
		t.Fatalf("install reported an unexpected skill: %+v", installed.Skill)
	}
	// The published package is read back through the package's own reader, not
	// through the file this test wrote: the operation and the reader must agree.
	service, err := promptskills.New(lane.generation())
	if err != nil {
		t.Fatalf("open the installed skill root: %v", err)
	}
	list, listErr := service.ListUser()
	if closeErr := service.Close(); closeErr != nil {
		t.Fatalf("close the installed skill root: %v", closeErr)
	}
	if listErr != nil {
		t.Fatalf("list the installed skills: %v", listErr)
	}
	if len(list) != 1 || list[0].Name != "daily-review" || list[0].Content != productionSkillInstallBody {
		t.Fatalf("the published skill is not listed as written: %+v", list)
	}
	published := filepath.Join(lane.generation(), "skills", "daily-review", "SKILL.md")
	onDisk, readErr := os.ReadFile(published)
	if readErr != nil {
		t.Fatalf("read the published skill: %v", readErr)
	}
	if string(onDisk) != productionSkillInstallBody {
		t.Fatalf("the published skill is not the staged bytes: %d bytes", len(onDisk))
	}
	info, statErr := os.Lstat(published)
	if statErr != nil {
		t.Fatalf("stat the published skill: %v", statErr)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("the published skill is not a private regular file: %v", info.Mode())
	}
	// Exactly one skill directory: the staging directory of the atomic install is
	// gone, so a half-written package cannot be mistaken for an installed one.
	entries, entryErr := os.ReadDir(filepath.Join(lane.generation(), "skills"))
	if entryErr != nil {
		t.Fatalf("read the skills tree: %v", entryErr)
	}
	if len(entries) != 1 || entries[0].Name() != "daily-review" {
		t.Fatalf("the skills tree holds more than the published skill: %v", entries)
	}
	afterPublish := productionCloudBackupTreeDigest(t, lane.generation())

	// The candidate is single use: the id that published cannot publish again.
	if replay := lane.install(t, author, picked.Inspection.CandidateID); replay.Success ||
		replay.Error != productionPickerSkillCandidateCode {
		t.Fatalf("a consumed candidate installed a second time: %+v", replay)
	}
	lane.unchanged(t, afterPublish)

	// A candidate is bound to the caller that minted it: another tab on the same
	// session is refused, and its attempt does not consume the owner's candidate.
	second := lane.stage(t, author, "voice-pass.md",
		[]byte("---\nname: voice-pass\n---\n\n# 语感\n\n保持同一语气。\n"))
	minted := lane.pickFile(t, author, second)
	if !minted.Success || minted.Inspection.CandidateID == "" {
		t.Fatalf("pick refused a second self-contained package: %+v", minted)
	}
	foreign := author.siblingTab(productionSkillInstallForeignTab)
	if stolen := lane.install(t, foreign, minted.Inspection.CandidateID); stolen.Success ||
		stolen.Error != productionPickerSkillCandidateCode {
		t.Fatalf("another tab installed this caller's candidate: %+v", stolen)
	}
	lane.unchanged(t, afterPublish)
	if owner := lane.install(t, author, minted.Inspection.CandidateID); !owner.Success {
		t.Fatalf("the owner could not spend its own candidate after a foreign attempt: %+v", owner)
	}
	if _, statErr := os.Lstat(filepath.Join(lane.generation(), "skills", "voice-pass", "SKILL.md")); statErr != nil {
		t.Fatalf("the second published skill is missing: %v", statErr)
	}
}

// TestProductionSkillInstallAppliesTheSharedGate keeps the publisher honest about
// which gate it applies. A readable but not self-contained package still gets a
// candidate from the pick (a single upload cannot see the files it references), and
// the INSTALL refuses it with the same verdict the GitHub path reaches; the
// publisher itself refuses the same shapes when it is called directly, and the
// committed generation is byte-identical after every refusal.
func TestProductionSkillInstallAppliesTheSharedGate(t *testing.T) {
	lane := newProductionSkillInstallLane(t)
	author := newProductionDurableClient(t, lane.mux, productionEditorTab)
	empty := productionCloudBackupTreeDigest(t, lane.generation())
	// A single file that references a file it does not carry.
	needsNotes := "---\nname: needs-notes\n---\n\n# 复核\n\n按 [说明](references/notes.md) 执行。\n"
	upload := lane.stage(t, author, "needs-notes.md", []byte(needsNotes))
	picked := lane.pickFile(t, author, upload)
	if !picked.Success || picked.Inspection.CandidateID == "" {
		t.Fatalf("pick refused a readable package instead of deferring to install: %+v", picked)
	}
	refused := lane.install(t, author, picked.Inspection.CandidateID)
	if refused.Success || refused.Error != productionPickerSkillIncompatibleCode {
		t.Fatalf("install published a package that is not self-contained: %+v", refused)
	}
	if _, statErr := os.Lstat(filepath.Join(lane.generation(), "skills", "needs-notes")); !os.IsNotExist(statErr) {
		t.Fatalf("a refused install left a skill directory behind: %v", statErr)
	}
	lane.unchanged(t, empty)
	// The candidate was consumed by the attempt, so a second attempt - even with the
	// same id - is the same refusal: the author picks again, exactly as the desktop's
	// single pending candidate behaves.
	if retry := lane.install(t, author, picked.Inspection.CandidateID); retry.Success ||
		retry.Error != productionPickerSkillCandidateCode {
		t.Fatalf("a refused install left its candidate spendable: %+v", retry)
	}
	lane.unchanged(t, empty)

	// The publisher's own gate, called directly: each of these is a package the
	// GitHub install path would refuse, and none of them may reach the root.
	installs := newProductionSkillInstalls(lane.open)
	for _, rejected := range []struct {
		name string
		raw  string
	}{
		// Not self-contained: the reference leaves the package.
		{name: "needs-notes", raw: needsNotes},
		// The frontmatter names a different skill than the one being installed.
		{name: "other-name", raw: productionSkillInstallBody},
		// A name that is not one path component.
		{name: "../../escape", raw: productionSkillInstallBody},
		// Past the package size bound the GitHub lane enforces.
		{name: "daily-review", raw: strings.Repeat("x", promptskills.MaxSkillBytes+1)},
	} {
		publishErr := installs.Publish(rejected.name, rejected.raw)
		if publishErr == nil || publishErr.Error() != productionSkillInstallPackageCode {
			t.Fatalf("publish accepted %s: %v", rejected.name, publishErr)
		}
		lane.unchanged(t, empty)
	}
	// The same publisher accepts the portable package, which is what makes the
	// refusals above refusals of the package and not of the port.
	if publishErr := installs.Publish("daily-review", productionSkillInstallBody); publishErr != nil {
		t.Fatalf("publish refused a portable package: %v", publishErr)
	}
	if _, statErr := os.Lstat(filepath.Join(lane.generation(), "skills", "daily-review", "SKILL.md")); statErr != nil {
		t.Fatalf("the publisher wrote no skill: %v", statErr)
	}
	// Installing the same name again is a refusal, not an overwrite.
	if publishErr := installs.Publish("daily-review", productionSkillInstallBody); publishErr == nil {
		t.Fatal("publish overwrote an installed skill")
	}
}
