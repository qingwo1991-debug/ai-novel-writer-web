//go:build linux

package projectfs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func listedFixture(t *testing.T, library, name, id string, modified time.Time) string {
	t.Helper()
	storage := filepath.Join(library, name, StorageDirectory)
	if err := os.MkdirAll(storage, 0700); err != nil {
		t.Fatal(err)
	}
	put(t, filepath.Join(storage, ManifestName), []byte(strings.Replace(fixtureManifest, fixtureID, id, 1)))
	database := filepath.Join(storage, DatabaseName)
	put(t, database, []byte("synthetic DB bytes"))
	if err := os.Chtimes(database, modified, modified); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(library, name)
}

func TestListCanonicalProjectsIncludesHiddenAndSortsDatabaseTime(t *testing.T) {
	library := t.TempDir()
	older := time.Date(2026, 8, 1, 9, 1, 2, 123000000, time.UTC)
	newer := older.Add(time.Hour)
	visible := listedFixture(t, library, "Visible", fixtureID, older)
	hiddenID := "12345678-1234-4234-8234-aaaaaaaaaaaa"
	hidden := listedFixture(t, library, ".Hidden", hiddenID, newer)
	// Library navigation includes a canonical project even if migration work
	// prevents opening it as an admitted author database.
	put(t, filepath.Join(hidden, ".vela"), []byte("legacy marker"))
	_ = listedFixture(t, library, "Broken", "not-a-uuid", newer)
	if err := os.Mkdir(filepath.Join(library, "Incomplete"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(visible, filepath.Join(library, "Alias")); err != nil {
		t.Fatal(err)
	}
	l, err := OpenLibrary(library)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	projects, err := l.ListCanonicalProjects(context.Background(), 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 2 || projects[0].Name != ".Hidden" || projects[0].Path != hidden || projects[0].ProjectID != hiddenID || projects[0].UpdatedAt != "2026-08-01T10:01:02.123Z" || projects[1].Name != "Visible" || projects[1].Path != visible || projects[1].UpdatedAt != "2026-08-01T09:01:02.123Z" {
		t.Fatalf("inventory mismatch: %#v", projects)
	}
}

func TestListCanonicalProjectsBoundsAndDetectsReplacedLibrary(t *testing.T) {
	library := t.TempDir()
	listedFixture(t, library, "One", fixtureID, time.Now())
	listedFixture(t, library, "Two", "12345678-1234-4234-8234-aaaaaaaaaaaa", time.Now())
	l, err := OpenLibrary(library)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if _, err := l.ListCanonicalProjects(context.Background(), 1); !errors.Is(err, ErrLibraryLimit) {
		t.Fatalf("unbounded directory accepted: %v", err)
	}
	if _, err := l.ListCanonicalProjects(context.Background(), 0); !errors.Is(err, ErrInvalidPath) {
		t.Fatalf("zero bound accepted: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := l.ListCanonicalProjects(ctx, 10); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled inventory accepted: %v", err)
	}
	if err := os.Rename(library, library+"-original"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(library, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := l.ListCanonicalProjects(context.Background(), 10); !errors.Is(err, ErrChanged) {
		t.Fatalf("replaced library accepted: %v", err)
	}
}
