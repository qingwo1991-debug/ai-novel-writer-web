//go:build linux

package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/librarypath"
)

// The fixture below lives entirely inside one disposable t.TempDir(): the
// production data root, one admitted project the lease names, the library root
// beside it, an output directory a browser export may write, and one import
// candidate a grant may read.
const (
	productionFSGrantImportPath  = "shared/input.md"
	productionFSGrantImportBody  = "外部导入的稿件：第一章，他推开门。"
	productionFSGrantOutputName  = "exports"
	productionFSGrantOutsideBody = "sealed outside bytes that no grant may serve"
	productionFSGrantChapterDir  = "第一卷"
	productionFSGrantChapterName = "第一章.md"
	productionFSGrantChapterBody = "第一章：门后有人。"
)

// productionFSGrantWire is the business envelope all three grant names answer.
type productionFSGrantWire struct {
	Success     bool   `json:"success"`
	Content     string `json:"content"`
	CommitState string `json:"commitState"`
	Error       string `json:"error"`
}

// productionFSGrantFixture is one open lane plus the trees the grant surface is
// measured against. Every path here is created through the fixture, never by an
// operation under test.
type productionFSGrantFixture struct {
	handler  http.Handler
	author   *productionDurableClient
	roots    librarypath.Roots
	library  string
	project  string
	outside  string
	registry *librarypath.Registry
}

// newProductionFSGrantFixture opens the production lane with the same fixture its
// siblings use, admits the project under this caller's lease, and then lays out
// the library side: the output directory, the import candidate, an outside tree
// and one planted link. The layout is created after the lane is open, so the
// startup catalog sees only what its own fixture seeded.
func newProductionFSGrantFixture(t *testing.T) productionFSGrantFixture {
	t.Helper()
	root, projectPath := productionDurableDataRoot(t)
	handler, cleanup, err := productionHandlerWithContract(migrationRegistry(t), productionDurableAddress, "", root,
		productionConfig{Schema: productionDurableContract(t), MaxDatabaseBytes: productionMaxDatabaseBytes})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	author := newProductionDurableClient(t, handler, productionDurableTab)
	var opened productionDurableOpenWire
	author.decode(t, "project:open", []any{projectPath, "fs-grant-open-1", projectPath}, &opened)
	if !opened.Success || !opened.DBReady || opened.Error != "" {
		t.Fatalf("project:open refused an admitted project: %+v", opened)
	}

	library := filepath.Join(root, productionLibraryDirectory)
	outside := t.TempDir()
	for _, directory := range []string{
		filepath.Join(library, productionFSGrantOutputName),
		filepath.Join(library, filepath.Dir(productionFSGrantImportPath)),
		outside,
	} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatalf("create %v: %v", directory, err)
		}
	}
	if err := os.WriteFile(filepath.Join(library, productionFSGrantImportPath),
		[]byte(productionFSGrantImportBody), 0o600); err != nil {
		t.Fatalf("seed the import candidate: %v", err)
	}
	if err := os.WriteFile(filepath.Join(outside, "outside-secret.txt"),
		[]byte(productionFSGrantOutsideBody), 0o600); err != nil {
		t.Fatalf("seed the outside file: %v", err)
	}
	// A link inside the library that points out of it: the escape every granted
	// write has to refuse, and it has to refuse it as a link and not as a path.
	if err := os.Symlink(outside, filepath.Join(library, productionFSGrantOutputName, "escape")); err != nil {
		t.Fatalf("plant the escape link: %v", err)
	}
	roots, err := librarypath.NewRoots([]librarypath.Root{
		{ID: librarypath.LibraryRootID, Kind: librarypath.KindLibrary, Path: library},
		{ID: librarypath.ProjectRootID, Kind: librarypath.KindProject, Path: projectPath},
	})
	if err != nil {
		t.Fatalf("build the allowlist the adapter should agree with: %v", err)
	}
	registry, err := productionFSGrantsRegistry()
	if err != nil {
		t.Fatalf("the process-wide grant registry is unavailable: %v", err)
	}
	return productionFSGrantFixture{
		handler:  handler,
		author:   author,
		roots:    roots,
		library:  library,
		project:  projectPath,
		outside:  outside,
		registry: registry,
	}
}

// mint issues one token for the caller that client represents, from the same
// allowlist the adapter rebuilds per request. This is the browser chooser's half
// of the contract: the server resolves the target, never the page.
func (f productionFSGrantFixture) mint(t *testing.T, client *productionDurableClient, rootID, relative string, ops librarypath.Op, options librarypath.MintOptions) string {
	t.Helper()
	target, err := f.roots.Resolve(rootID, relative)
	if err != nil {
		t.Fatalf("resolve the mint target %v/%v: %v", rootID, relative, err)
	}
	token, err := f.registry.Mint(productionFSGrantBinding(client), target, ops, options)
	if err != nil {
		t.Fatalf("mint a grant for %v: %v", relative, err)
	}
	return token
}

// productionFSGrantBinding is the caller identity a token is bound to: the cookie
// session the bootstrap issued and the tab header, exactly as projectsession
// spells an owner.
func productionFSGrantBinding(client *productionDurableClient) librarypath.Binding {
	return librarypath.Binding{SessionID: client.cookie.Value, ClientID: client.tab}
}

// invoke calls one grant name and decodes its business envelope. Every answer on
// this surface is HTTP 200 with `success` false and a symbolic code, because that
// is the shape the unchanged renderer branches on.
func productionFSGrantInvoke(t *testing.T, client *productionDurableClient, operation string, args []any) productionFSGrantWire {
	t.Helper()
	raw := productionEditorCall(t, client, operation, args)
	var wire productionFSGrantWire
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("%v: %v %s", operation, err, raw)
	}
	return wire
}

// productionFSGrantRefused asserts one symbolic refusal on one name.
func productionFSGrantRefused(t *testing.T, client *productionDurableClient, operation, label string, args []any, want string) {
	t.Helper()
	wire := productionFSGrantInvoke(t, client, operation, args)
	if wire.Success {
		t.Fatalf("%v accepted a %v", operation, label)
	}
	if wire.Error != want {
		t.Fatalf("%v refused a %v with %v; want %v", operation, label, wire.Error, want)
	}
}

// TestProductionFSGrantsServeAllowlistedPaths is the grant slice of the production
// lane. The refusal subtests run first, between two tree digests, so "a failed
// grant writes nothing" is measured rather than asserted; the acceptance subtests
// then prove the same three names really do read and write the paths they were
// granted.
func TestProductionFSGrantsServeAllowlistedPaths(t *testing.T) {
	fixture := newProductionFSGrantFixture(t)
	before := productionFSSnapshot(t, fixture.project)
	libraryBefore := productionFSSnapshot(t, fixture.library)
	outsideBefore := productionFSSnapshot(t, fixture.outside)

	outputDir := func() string { return filepath.Join(fixture.library, productionFSGrantOutputName) }

	// ---- refusals -----------------------------------------------------------
	t.Run("unknown-token", func(t *testing.T) {
		productionFSGrantRefused(t, fixture.author, productionFSGrantReadName, "token this server never minted",
			[]any{"never-minted"}, "FS_GRANT_UNKNOWN")
		productionFSGrantRefused(t, fixture.author, productionFSGrantMkdirName, "token this server never minted",
			[]any{"never-minted", "x"}, "FS_GRANT_UNKNOWN")
		productionFSGrantRefused(t, fixture.author, productionFSGrantWriteName, "token this server never minted",
			[]any{"never-minted", "x.md", "body"}, "FS_GRANT_UNKNOWN")
	})

	t.Run("malformed-request", func(t *testing.T) {
		productionFSGrantRefused(t, fixture.author, productionFSGrantReadName, "empty request",
			[]any{}, "INVALID_ARGUMENTS")
		productionFSGrantRefused(t, fixture.author, productionFSGrantWriteName, "short write",
			[]any{"token", "a.md"}, "INVALID_ARGUMENTS")
		token := fixture.mint(t, fixture.author, librarypath.LibraryRootID, productionFSGrantOutputName,
			librarypath.OpWrite|librarypath.OpMkdir, librarypath.MintOptions{})
		productionFSGrantRefused(t, fixture.author, productionFSGrantWriteName, "non-string body",
			[]any{token, "a.md", 7}, "INVALID_ARGUMENTS")
	})

	t.Run("traversal-refused", func(t *testing.T) {
		token := fixture.mint(t, fixture.author, librarypath.LibraryRootID, productionFSGrantOutputName,
			librarypath.OpWrite|librarypath.OpMkdir, librarypath.MintOptions{})
		productionFSGrantRefused(t, fixture.author, productionFSGrantWriteName, "traversal",
			[]any{token, "../escaped.md", "body"}, "FS_GRANT_PATH_REFUSED")
		productionFSGrantRefused(t, fixture.author, productionFSGrantMkdirName, "traversal",
			[]any{token, "../escaped"}, "FS_GRANT_PATH_REFUSED")
		if _, err := os.Lstat(filepath.Join(fixture.library, "escaped.md")); !os.IsNotExist(err) {
			t.Fatalf("a traversal write created %v", filepath.Join(fixture.library, "escaped.md"))
		}
	})

	t.Run("absolute-and-encoded-refused", func(t *testing.T) {
		token := fixture.mint(t, fixture.author, librarypath.LibraryRootID, productionFSGrantOutputName,
			librarypath.OpWrite|librarypath.OpMkdir, librarypath.MintOptions{})
		productionFSGrantRefused(t, fixture.author, productionFSGrantWriteName, "absolute path",
			[]any{token, "/etc/passwd", "body"}, "FS_GRANT_PATH_REFUSED")
		productionFSGrantRefused(t, fixture.author, productionFSGrantMkdirName, "absolute path",
			[]any{token, "/etc"}, "FS_GRANT_PATH_REFUSED")
		productionFSGrantRefused(t, fixture.author, productionFSGrantWriteName, "encoded separator",
			[]any{token, "a%2fb.md", "body"}, "FS_GRANT_PATH_REFUSED")
		productionFSGrantRefused(t, fixture.author, productionFSGrantWriteName, "hidden component",
			[]any{token, ".storage/x.md", "body"}, "FS_GRANT_PATH_REFUSED")
	})

	t.Run("symlink-escape-refused", func(t *testing.T) {
		token := fixture.mint(t, fixture.author, librarypath.LibraryRootID, productionFSGrantOutputName,
			librarypath.OpWrite|librarypath.OpMkdir, librarypath.MintOptions{})
		productionFSGrantRefused(t, fixture.author, productionFSGrantWriteName, "link out of the root",
			[]any{token, "escape/leak.md", "body"}, "FS_GRANT_PATH_ESCAPE")
		productionFSGrantRefused(t, fixture.author, productionFSGrantMkdirName, "link out of the root",
			[]any{token, "escape/nested"}, "FS_GRANT_PATH_ESCAPE")
		// The read side reaches the same link only through an extent, because the
		// resolve that mints a token refuses a symlinked path in the first place.
		reader := fixture.mint(t, fixture.author, librarypath.LibraryRootID, productionFSGrantOutputName,
			librarypath.OpRead, librarypath.MintOptions{})
		productionFSGrantRefused(t, fixture.author, productionFSGrantReadName, "link out of the root",
			[]any{reader, "escape"}, "FS_GRANT_PATH_ESCAPE")
	})

	t.Run("foreign-caller-refused", func(t *testing.T) {
		token := fixture.mint(t, fixture.author, librarypath.LibraryRootID, productionFSGrantImportPath,
			librarypath.OpRead, librarypath.MintOptions{})
		other := newProductionDurableClient(t, fixture.handler, "production-grant-other-tab")
		productionFSGrantRefused(t, other, productionFSGrantReadName, "another caller's token",
			[]any{token}, "FS_GRANT_CALLER_MISMATCH")
		// The same tab with a session it did not open the project under is still a
		// different caller.
		if productionFSGrantBinding(other) == productionFSGrantBinding(fixture.author) {
			t.Fatal("the second client shares the first client's session")
		}
	})

	t.Run("expired-refused", func(t *testing.T) {
		token := fixture.mint(t, fixture.author, librarypath.LibraryRootID, productionFSGrantImportPath,
			librarypath.OpRead, librarypath.MintOptions{TTL: time.Millisecond})
		time.Sleep(3 * time.Millisecond)
		productionFSGrantRefused(t, fixture.author, productionFSGrantReadName, "expired token",
			[]any{token}, "FS_GRANT_EXPIRED")
	})

	t.Run("revoked-refused", func(t *testing.T) {
		token := fixture.mint(t, fixture.author, librarypath.LibraryRootID, productionFSGrantImportPath,
			librarypath.OpRead, librarypath.MintOptions{})
		if !fixture.registry.Revoke(token) {
			t.Fatal("Revoke reported nothing to revoke")
		}
		productionFSGrantRefused(t, fixture.author, productionFSGrantReadName, "revoked token",
			[]any{token}, "FS_GRANT_REVOKED")
	})

	t.Run("read-only-grant-refuses-mkdir", func(t *testing.T) {
		token := fixture.mint(t, fixture.author, librarypath.LibraryRootID, productionFSGrantOutputName,
			librarypath.OpRead, librarypath.MintOptions{})
		productionFSGrantRefused(t, fixture.author, productionFSGrantMkdirName, "mkdir under a read-only grant",
			[]any{token, "读only"}, "FS_GRANT_OPERATION_DENIED")
		productionFSGrantRefused(t, fixture.author, productionFSGrantWriteName, "write under a read-only grant",
			[]any{token, "读only.md", "body"}, "FS_GRANT_OPERATION_DENIED")
		if _, err := os.Lstat(filepath.Join(outputDir(), "读only")); !os.IsNotExist(err) {
			t.Fatal("a read-only grant created a directory")
		}
	})

	t.Run("author-data-refused", func(t *testing.T) {
		// A grant that carries every operation still may not write author data: the
		// operation that owns the project tree, under the author database's
		// single-writer lease, is the only writer there.
		token := fixture.mint(t, fixture.author, librarypath.ProjectRootID, "",
			librarypath.OpRead|librarypath.OpWrite|librarypath.OpMkdir, librarypath.MintOptions{})
		productionFSGrantRefused(t, fixture.author, productionFSGrantWriteName, "write into the project",
			[]any{token, "manuscript/grant.md", "body"}, "FS_GRANT_AUTHOR_DATA")
		productionFSGrantRefused(t, fixture.author, productionFSGrantMkdirName, "mkdir in the project",
			[]any{token, "manuscript/grant-dir"}, "FS_GRANT_AUTHOR_DATA")
		if _, err := os.Lstat(filepath.Join(fixture.project, "manuscript", "grant.md")); !os.IsNotExist(err) {
			t.Fatal("a grant wrote into the admitted project tree")
		}
	})

	t.Run("write-without-create-refused", func(t *testing.T) {
		token := fixture.mint(t, fixture.author, librarypath.LibraryRootID, productionFSGrantOutputName,
			librarypath.OpWrite, librarypath.MintOptions{})
		productionFSGrantRefused(t, fixture.author, productionFSGrantWriteName, "a new file without create permission",
			[]any{token, "new.md", "body"}, "FS_GRANT_FILE_NOT_FOUND")
		if _, err := os.Lstat(filepath.Join(outputDir(), "new.md")); !os.IsNotExist(err) {
			t.Fatal("a write-only grant created a new file")
		}
	})

	// Nothing above wrote a byte, in the project tree, in the library or outside
	// it: every refusal was decided before the filesystem was touched.
	if after := productionFSSnapshot(t, fixture.project); !reflect.DeepEqual(before, after) {
		t.Fatal("a refused grant changed the admitted project tree")
	}
	if after := productionFSSnapshot(t, fixture.library); !reflect.DeepEqual(libraryBefore, after) {
		t.Fatal("a refused grant changed the library tree")
	}
	if after := productionFSSnapshot(t, fixture.outside); !reflect.DeepEqual(outsideBefore, after) {
		t.Fatal("a refused grant wrote into the tree outside the library")
	}

	// ---- acceptance ---------------------------------------------------------
	t.Run("read", func(t *testing.T) {
		token := fixture.mint(t, fixture.author, librarypath.LibraryRootID, productionFSGrantImportPath,
			librarypath.OpRead, librarypath.MintOptions{MaxUses: 2})
		wire := productionFSGrantInvoke(t, fixture.author, productionFSGrantReadName, []any{token})
		if !wire.Success || wire.Error != "" {
			t.Fatalf("the grant read refused an allowlisted file: %+v", wire)
		}
		if wire.Content != productionFSGrantImportBody {
			t.Fatalf("the grant read returned %d bytes; want the seeded body", len(wire.Content))
		}
		// The second use goes through the extent form the wire declares optional.
		wire = productionFSGrantInvoke(t, fixture.author, productionFSGrantReadName, []any{token, ""})
		if !wire.Success || wire.Content != productionFSGrantImportBody {
			t.Fatalf("the grant read refused an empty extent: %+v", wire)
		}
		wire = productionFSGrantInvoke(t, fixture.author, productionFSGrantReadName, []any{token})
		if wire.Success || wire.Error != "FS_GRANT_EXHAUSTED" {
			t.Fatalf("a spent grant answered %+v; want FS_GRANT_EXHAUSTED", wire)
		}
	})

	t.Run("mkdir-and-write", func(t *testing.T) {
		token := fixture.mint(t, fixture.author, librarypath.LibraryRootID, productionFSGrantOutputName,
			librarypath.OpWrite|librarypath.OpMkdir, librarypath.MintOptions{MaxUses: 8})
		wire := productionFSGrantInvoke(t, fixture.author, productionFSGrantMkdirName,
			[]any{token, productionFSGrantChapterDir})
		if !wire.Success || wire.Error != "" {
			t.Fatalf("the grant mkdir refused an output directory: %+v", wire)
		}
		chapter := filepath.Join(outputDir(), productionFSGrantChapterDir, productionFSGrantChapterName)
		wire = productionFSGrantInvoke(t, fixture.author, productionFSGrantWriteName,
			[]any{token, productionFSGrantChapterDir + "/" + productionFSGrantChapterName, productionFSGrantChapterBody})
		if !wire.Success || wire.Error != "" || wire.CommitState != "" {
			t.Fatalf("the grant write refused an output file: %+v", wire)
		}
		body, err := os.ReadFile(chapter)
		if err != nil || string(body) != productionFSGrantChapterBody {
			t.Fatalf("the granted write did not publish the bytes: %v", err)
		}
		// Replacing an existing file needs only the write permission.
		wire = productionFSGrantInvoke(t, fixture.author, productionFSGrantWriteName,
			[]any{token, productionFSGrantChapterDir + "/" + productionFSGrantChapterName, productionFSGrantChapterBody + "二"})
		if !wire.Success {
			t.Fatalf("the grant write refused a replacement: %+v", wire)
		}
		replaced, err := os.ReadFile(chapter)
		if err != nil || string(replaced) != productionFSGrantChapterBody+"二" {
			t.Fatalf("the granted replacement was not published: %v", err)
		}
		// The project tree is untouched by every success above.
		after := productionFSSnapshot(t, fixture.project)
		if !reflect.DeepEqual(before, after) {
			t.Fatal("the export lane changed the admitted project tree")
		}
	})
}

// TestProductionFSGrantNamesAreBoundOnce is the wiring guard: the three names are
// bound exactly once by this binder on a real lane, and a second Bind is the
// transport's OPERATION_ALREADY_BOUND rather than a silent replacement.
func TestProductionFSGrantNamesAreBoundOnce(t *testing.T) {
	root, _ := productionDurableDataRoot(t)
	handler, cleanup, err := productionHandlerWithContract(migrationRegistry(t), productionDurableAddress, "", root,
		productionConfig{Schema: productionDurableContract(t), MaxDatabaseBytes: productionMaxDatabaseBytes})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	bound := productionLaneBoundOperations(t, handler)
	for _, name := range []string{
		productionFSGrantReadName,
		productionFSGrantWriteName,
		productionFSGrantMkdirName,
	} {
		count := 0
		for _, candidate := range bound {
			if candidate == name {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("%v is bound %d times; want exactly 1", name, count)
		}
	}
}

// TestProductionFSGrantFilesStayClosedWithoutASession pins the one refusal outside
// the business envelope: a request with no session never reaches the adapter, so
// no code path can mint or redeem a token for it. The request carries everything
// else the neighbours send - the user, the origin, the content type - so only the
// missing cookie can be what refuses it.
func TestProductionFSGrantFilesStayClosedWithoutASession(t *testing.T) {
	root, _ := productionDurableDataRoot(t)
	handler, cleanup, err := productionHandlerWithContract(migrationRegistry(t), productionDurableAddress, "", root,
		productionConfig{Schema: productionDurableContract(t), MaxDatabaseBytes: productionMaxDatabaseBytes})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	payload, err := json.Marshal(map[string]any{"args": []any{"never-minted"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, operation := range []string{
		productionFSGrantReadName,
		productionFSGrantWriteName,
		productionFSGrantMkdirName,
	} {
		request := httptest.NewRequest(http.MethodPost, "/api/operations/"+operation, bytes.NewReader(payload))
		request.Header.Set("X-Novel-User", "production-author")
		request.Header.Set("X-Client-Id", productionDurableTab)
		request.Header.Set("Origin", "http://"+productionDurableAddress)
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code == http.StatusOK {
			t.Fatalf("%v answered 200 without a session: %s", operation, response.Body.String())
		}
		if strings.Contains(response.Body.String(), productionFSGrantImportBody) {
			t.Fatalf("%v answered a session-less request with seeded bytes", operation)
		}
	}
}
