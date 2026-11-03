//go:build linux

package authorruntime

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectcatalog"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectgrant"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
)

func putGrantTestProject(t *testing.T, path string, database []byte) {
	t.Helper()
	storage := filepath.Join(path, projectfs.StorageDirectory)
	if err := os.MkdirAll(storage, 0700); err != nil {
		t.Fatal(err)
	}
	manifest := strings.Replace(fixtureManifest, "8b5929e6-cda4-43e2-a15c-f232571f0036", "e4f0edbb-0c38-45b4-a720-449b208037c9", 1)
	for name, content := range map[string][]byte{projectfs.ManifestName: []byte(manifest), projectfs.DatabaseName: database} {
		if err := os.WriteFile(filepath.Join(storage, name), content, 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestGrantedReadOpenPreservesActiveReaderOnRejectedGrantAndProject(t *testing.T) {
	catalog, keyA, config, source, original := readFixture(t)
	seed, err := sql.Open("sqlite3", source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Exec("INSERT INTO project_core(id, project_name) VALUES('main', 'Synthetic novel')"); err != nil {
		_ = seed.Close()
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
	config.ProjectPath = ""
	lifecycle, err := NewReadLifecycle(catalog, config)
	if err != nil {
		t.Fatal(err)
	}
	defer lifecycle.Shutdown()
	owner := projectsession.Owner{SessionID: "cookie", ClientID: "tab"}
	openedA, err := lifecycle.OpenProjectReadOnly(context.Background(), owner, keyA, "open-a")
	if err != nil || !openedA.Success {
		t.Fatalf("project A failed to open: %+v %v", openedA, err)
	}
	activeA, present, err := lifecycle.Current(context.Background(), owner)
	if err != nil || !present {
		t.Fatalf("project A has no active lease: %v", err)
	}
	library, err := projectfs.OpenLibrary(config.LibraryRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer library.Close()
	grants, err := projectgrant.New(library)
	if err != nil {
		t.Fatal(err)
	}
	issue := func(path string) string {
		t.Helper()
		project, err := library.OpenProject(path)
		if err != nil {
			t.Fatal(err)
		}
		defer project.Close()
		grant, err := grants.Issue(context.Background(), owner, projectgrant.PurposeProjectOpen, project, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		return grant
	}
	pathA := filepath.Dir(filepath.Dir(source))
	grantA := issue(pathA)
	if _, err := lifecycle.OpenGrantedProjectReadOnly(context.Background(), projectsession.Owner{SessionID: "other", ClientID: "tab"}, grants, grantA, "foreign"); !errors.Is(err, projectgrant.ErrInvalidGrant) {
		t.Fatalf("foreign caller used grant: %v", err)
	}
	if _, err := lifecycle.OpenGrantedProjectReadOnly(context.Background(), owner, grants, "forged", "forged"); !errors.Is(err, projectgrant.ErrInvalidGrant) {
		t.Fatalf("forged grant opened project: %v", err)
	}
	if current, present, err := lifecycle.Current(context.Background(), owner); err != nil || !present || current != activeA {
		t.Fatalf("invalid grant disturbed project A: %+v %t %v", current, present, err)
	}
	refreshed, err := lifecycle.OpenGrantedProjectReadOnly(context.Background(), owner, grants, grantA, "refresh-a")
	if err != nil || !refreshed.Success || refreshed.Project["sessionLease"] != openedA.Project["sessionLease"] {
		t.Fatalf("same-key grant did not refresh A: %+v %v", refreshed, err)
	}
	if _, err := lifecycle.OpenGrantedProjectReadOnly(context.Background(), owner, grants, grantA, "replay"); !errors.Is(err, projectgrant.ErrInvalidGrant) {
		t.Fatalf("consumed grant replayed: %v", err)
	}

	// B is registered but lacks project_core. Its failed DB admission must not
	// revoke A's key or replace A's healthy lease.
	_ = secondProjectKey(t, catalog, config, original)
	grantB := issue(filepath.Join(config.LibraryRoot, "synthetic-author-b"))
	failedB, err := lifecycle.OpenGrantedProjectReadOnly(context.Background(), owner, grants, grantB, "open-b")
	if err != nil || failedB.Success || failedB.ActiveProjectPath == nil || *failedB.ActiveProjectPath != pathA {
		t.Fatalf("failed B changed A: %+v %v", failedB, err)
	}
	if current, present, err := lifecycle.Current(context.Background(), owner); err != nil || !present || current != activeA {
		t.Fatalf("failed B changed active lease: %+v %t %v", current, present, err)
	}
	if project, err := catalog.Resolve(keyA); err != nil {
		t.Fatalf("failed B revoked A's catalog key: %v", err)
	} else if err := project.Close(); err != nil {
		t.Fatal(err)
	}

	// A valid server-issued grant for an unregistered project cannot register
	// itself through this path.
	pathC := filepath.Join(config.LibraryRoot, "synthetic-author-c")
	putGrantTestProject(t, pathC, original)
	grantC := issue(pathC)
	if _, err := lifecycle.OpenGrantedProjectReadOnly(context.Background(), owner, grants, grantC, "open-c"); !errors.Is(err, projectcatalog.ErrCatalogKey) {
		t.Fatalf("unregistered project gained a catalog key: %v", err)
	}
}
