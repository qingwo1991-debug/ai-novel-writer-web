//go:build linux

package projectgrant

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
)

const syntheticManifest = `{"schemaVersion":1,"kind":"ai-novel-project","projectId":"12345678-1234-4234-8234-123456789abc","createdAt":"2026-01-02T03:04:05.123Z","storageFormat":"ai-novel","storageVersion":1}`

func putProject(t *testing.T, path string) {
	t.Helper()
	storage := filepath.Join(path, projectfs.StorageDirectory)
	if err := os.MkdirAll(storage, 0700); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		projectfs.ManifestName: syntheticManifest,
		projectfs.DatabaseName: "synthetic only; no SQLite",
	} {
		if err := os.WriteFile(filepath.Join(storage, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func fixture(t *testing.T) (*Registry, *projectfs.Project, string) {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, "book")
	putProject(t, path)
	library, err := projectfs.OpenLibrary(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = library.Close() })
	project, err := library.OpenProject(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = project.Close() })
	registry, err := New(library)
	if err != nil {
		t.Fatal(err)
	}
	return registry, project, path
}

func TestProjectOpenGrantBindsOwnerPurposeAndConsumesOnce(t *testing.T) {
	registry, project, path := fixture(t)
	owner := projectsession.Owner{SessionID: "cookie-a", ClientID: "tab-a"}
	token, err := registry.Issue(context.Background(), owner, PurposeProjectOpen, project, time.Minute)
	if err != nil || token == "" {
		t.Fatal("trusted project was not issued a grant", err)
	}
	for _, foreign := range []projectsession.Owner{
		{SessionID: "cookie-b", ClientID: owner.ClientID},
		{SessionID: owner.SessionID, ClientID: "tab-b"},
	} {
		if opened, err := registry.Resolve(context.Background(), foreign, PurposeProjectOpen, token); !errors.Is(err, ErrInvalidGrant) || opened != nil {
			t.Fatal("foreign caller resolved grant", err)
		}
	}
	if opened, err := registry.Resolve(context.Background(), owner, "other-purpose", token); !errors.Is(err, ErrInvalidGrant) || opened != nil {
		t.Fatal("wrong purpose resolved grant", err)
	}
	if opened, err := registry.Resolve(context.Background(), owner, PurposeProjectOpen, "forged"); !errors.Is(err, ErrInvalidGrant) || opened != nil {
		t.Fatal("forged grant resolved", err)
	}
	opened, err := registry.Resolve(context.Background(), owner, PurposeProjectOpen, token)
	if err != nil || opened.Path() != path || !opened.Identity().Same(project.Identity()) {
		t.Fatal("matching grant failed", err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	if reopened, err := registry.Resolve(context.Background(), owner, PurposeProjectOpen, token); !errors.Is(err, ErrInvalidGrant) || reopened != nil {
		t.Fatal("consumed grant reused", err)
	}
}

func TestProjectOpenGrantExpiresAndRejectsUnsafeTTL(t *testing.T) {
	registry, project, _ := fixture(t)
	owner := projectsession.Owner{SessionID: "cookie", ClientID: "tab"}
	for _, ttl := range []time.Duration{0, -time.Second, MaxTTL + time.Nanosecond} {
		if token, err := registry.Issue(context.Background(), owner, PurposeProjectOpen, project, ttl); !errors.Is(err, ErrInvalidGrant) || token != "" {
			t.Fatal("unsafe TTL accepted", ttl, err)
		}
	}
	clock := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	registry.now = func() time.Time { return clock }
	token, err := registry.Issue(context.Background(), owner, PurposeProjectOpen, project, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(time.Second)
	if opened, err := registry.Resolve(context.Background(), owner, PurposeProjectOpen, token); !errors.Is(err, ErrInvalidGrant) || opened != nil {
		t.Fatal("expired grant resolved", err)
	}
}

func TestProjectOpenGrantRejectsReplacedProjectAndUntrustedLibrary(t *testing.T) {
	registry, project, path := fixture(t)
	owner := projectsession.Owner{SessionID: "cookie", ClientID: "tab"}
	otherRoot := t.TempDir()
	otherPath := filepath.Join(otherRoot, "book")
	putProject(t, otherPath)
	otherLibrary, err := projectfs.OpenLibrary(otherRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer otherLibrary.Close()
	otherProject, err := otherLibrary.OpenProject(otherPath)
	if err != nil {
		t.Fatal(err)
	}
	defer otherProject.Close()
	if token, err := registry.Issue(context.Background(), owner, PurposeProjectOpen, otherProject, time.Minute); !errors.Is(err, ErrInvalidGrant) || token != "" {
		t.Fatal("foreign library project issued grant", err)
	}
	token, err := registry.Issue(context.Background(), owner, PurposeProjectOpen, project, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, path+"-old"); err != nil {
		t.Fatal(err)
	}
	putProject(t, path)
	if opened, err := registry.Resolve(context.Background(), owner, PurposeProjectOpen, token); !errors.Is(err, ErrInvalidGrant) || opened != nil {
		t.Fatal("same-ID replacement resolved grant", err)
	}
	if opened, err := registry.Resolve(context.Background(), owner, PurposeProjectOpen, token); !errors.Is(err, ErrInvalidGrant) || opened != nil {
		t.Fatal("failed replacement grant was not consumed", err)
	}
}

func TestProjectOpenGrantConcurrentResolveHasOneWinner(t *testing.T) {
	registry, project, _ := fixture(t)
	owner := projectsession.Owner{SessionID: "cookie", ClientID: "tab"}
	token, err := registry.Issue(context.Background(), owner, PurposeProjectOpen, project, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	var wait sync.WaitGroup
	var mu sync.Mutex
	success := 0
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			opened, err := registry.Resolve(context.Background(), owner, PurposeProjectOpen, token)
			if err == nil {
				mu.Lock()
				success++
				mu.Unlock()
				_ = opened.Close()
			}
		}()
	}
	wait.Wait()
	if success != 1 {
		t.Fatalf("one-use grant had %d winners", success)
	}
}
