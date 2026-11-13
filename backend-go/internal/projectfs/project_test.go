//go:build linux

package projectfs

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// All fixtures are synthetic temporary files, not SQLite or author data.
func fixture(t *testing.T) (string, string, string) {
	t.Helper()
	library := t.TempDir()
	project := filepath.Join(library, "Synthetic project")
	storage := filepath.Join(project, StorageDirectory)
	if err := os.MkdirAll(storage, 0700); err != nil {
		t.Fatal(err)
	}
	put(t, filepath.Join(storage, ManifestName), []byte(fixtureManifest))
	put(t, filepath.Join(storage, DatabaseName), []byte("synthetic database bytes; not SQLite"))
	return library, project, storage
}

func put(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func remove(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
}

func link(t *testing.T, old, new string, symbolic bool) {
	t.Helper()
	var err error
	if symbolic {
		err = os.Symlink(old, new)
	} else {
		err = os.Link(old, new)
	}
	if err != nil {
		t.Fatal(err)
	}
}

func openFixture(t *testing.T, library, path string) (*Library, *Project) {
	t.Helper()
	l, err := OpenLibrary(library)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	p, err := l.OpenProject(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	return l, p
}

func TestCanonicalLibraryAndImmediateProjectPaths(t *testing.T) {
	library, path, _ := fixture(t)
	l, p := openFixture(t, library, path)
	if l.Path() != library || p.Path() != path || p.ProjectID() != fixtureID || p.Manifest().StorageFormat != "ai-novel" {
		t.Fatal("incorrect getters")
	}
	m := p.Manifest()
	m.ProjectID = "mutated"
	if p.ProjectID() != fixtureID {
		t.Fatal("manifest getter is not a value")
	}
	for _, bad := range []string{"", "relative", library + "/", library + "/.", library + "/../" + filepath.Base(library), library + "\x00", library + "\\child"} {
		if root, err := OpenLibrary(bad); err == nil {
			root.Close()
			t.Errorf("invalid library accepted: %q", bad)
		}
	}
	for _, bad := range []string{library, "Synthetic project", path + "/", path + "/.", path + "/child", path + "/../Synthetic project", filepath.Dir(library), path + "\x00", path + "\\other"} {
		if project, err := l.OpenProject(bad); err == nil {
			project.Close()
			t.Errorf("invalid project accepted: %q", bad)
		}
	}
}

func TestRejectLibrarySymlinkAncestors(t *testing.T) {
	library, _, _ := fixture(t)
	alias := filepath.Join(t.TempDir(), "alias")
	link(t, library, alias, true)
	for _, path := range []string{alias, filepath.Join(alias, "Synthetic project")} {
		if l, err := OpenLibrary(path); err == nil {
			l.Close()
			t.Fatalf("symlink root/ancestor accepted: %q", path)
		}
	}
}

func TestRejectProjectAndStorageSymlinks(t *testing.T) {
	for _, target := range []string{"project", "storage"} {
		t.Run(target, func(t *testing.T) {
			library, path, storage := fixture(t)
			l, err := OpenLibrary(library)
			if err != nil {
				t.Fatal(err)
			}
			defer l.Close()
			if target == "project" {
				alias := filepath.Join(library, "alias")
				link(t, "Synthetic project", alias, true)
				path = alias
			} else {
				moved := filepath.Join(path, "actual-storage")
				if err := os.Rename(storage, moved); err != nil {
					t.Fatal(err)
				}
				link(t, "actual-storage", storage, true)
			}
			if p, err := l.OpenProject(path); err == nil {
				p.Close()
				t.Fatal("symlink admitted")
			}
		})
	}
}

func TestRejectUnsafeManifestAndDatabase(t *testing.T) {
	for _, name := range []string{ManifestName, DatabaseName} {
		for _, kind := range []string{"symlink-inside", "symlink-outside", "hardlink", "directory", "fifo"} {
			t.Run(name+"/"+kind, func(t *testing.T) {
				library, path, storage := fixture(t)
				file := filepath.Join(storage, name)
				saved := filepath.Join(storage, "saved")
				if err := os.Rename(file, saved); err != nil {
					t.Fatal(err)
				}
				switch kind {
				case "symlink-inside":
					link(t, "saved", file, true)
				case "symlink-outside":
					outside := filepath.Join(t.TempDir(), "source")
					put(t, outside, []byte(fixtureManifest))
					link(t, outside, file, true)
				case "hardlink":
					link(t, saved, file, false)
				case "directory":
					if err := os.Mkdir(file, 0700); err != nil {
						t.Fatal(err)
					}
				case "fifo":
					if err := syscall.Mkfifo(file, 0600); err != nil {
						t.Fatal(err)
					}
				}
				l, err := OpenLibrary(library)
				if err != nil {
					t.Fatal(err)
				}
				defer l.Close()
				if p, err := l.OpenProject(path); err == nil {
					p.Close()
					t.Fatal("unsafe file admitted")
				}
			})
		}
	}
}

func TestOpenFileReadConfinement(t *testing.T) {
	library, path, storage := fixture(t)
	if err := os.Mkdir(filepath.Join(storage, "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	put(t, filepath.Join(storage, "nested", "safe.txt"), []byte("synthetic"))
	link(t, "nested", filepath.Join(storage, "alias"), true)
	link(t, "nested/safe.txt", filepath.Join(storage, "file-link"), true)
	outside := filepath.Join(t.TempDir(), "outside.txt")
	put(t, outside, []byte("outside"))
	link(t, outside, filepath.Join(storage, "escape"), true)
	put(t, filepath.Join(storage, "linked"), []byte("linked"))
	link(t, filepath.Join(storage, "linked"), filepath.Join(storage, "linked-again"), false)
	_, p := openFixture(t, library, path)
	f, err := p.OpenFileRead("nested/safe.txt")
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "synthetic" {
		t.Fatal("unexpected data")
	}
	if _, err := f.Write([]byte("not allowed")); err == nil {
		t.Fatal("descriptor writable")
	}
	f.Close()
	for _, bad := range []string{"", ".", "..", "../outside", "nested/../project.json", "nested/./safe.txt", "nested//safe.txt", "nested/safe.txt/", "/etc/passwd", "nested\\safe.txt", "nested/\x00", "alias/safe.txt", "file-link", "escape", "linked", "nested", DatabaseName, DatabaseName + "-wal", DatabaseName + "-journal"} {
		if f, err := p.OpenFileRead(bad); err == nil {
			f.Close()
			t.Errorf("unsafe read accepted: %q", bad)
		}
	}
}

func TestLegacyAndMigrationJournals(t *testing.T) {
	cases := []struct {
		name    string
		journal string
		setup   string
		allowed bool
	}{
		{"absent", "", "", true},
		{"empty control", "", "empty", true},
		{"switched", `{"version":1,"phase":"switched","projectId":"` + fixtureID + `","extra":{"allowed":true}}`, "", true},
		{"invalid json", `{`, "", false},
		{"duplicate", `{"version":1,"phase":"switched","phase":"switched","projectId":"` + fixtureID + `"}`, "", false},
		{"wrong phase", `{"version":1,"phase":"copied","projectId":"` + fixtureID + `"}`, "", false},
		{"wrong identity", `{"version":1,"phase":"switched","projectId":"12345678-1234-4234-8234-aaaaaaaaaaaa"}`, "", false},
		{"wrong version", `{"version":2,"phase":"switched","projectId":"` + fixtureID + `"}`, "", false},
		{"control file", "", "control-file", false},
		{"control symlink", "", "control-symlink", false},
		{"journal symlink", "", "journal-symlink", false},
		{"journal hardlink", "", "journal-hardlink", false},
		{"oversized journal", strings.Repeat(" ", int(journalLimit)+1), "", false},
		{"legacy directory", "", "legacy", false},
		{"legacy dangling link", "", "legacy-link", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			library, path, _ := fixture(t)
			control := filepath.Join(path, ".ai-novel-migration")
			switch tc.setup {
			case "legacy":
				if err := os.Mkdir(filepath.Join(path, ".vela"), 0700); err != nil {
					t.Fatal(err)
				}
			case "legacy-link":
				link(t, "does-not-exist", filepath.Join(path, ".vela"), true)
			case "control-file":
				put(t, control, []byte("file"))
			case "control-symlink":
				link(t, "does-not-exist", control, true)
			default:
				if tc.journal != "" || tc.setup != "" {
					if err := os.Mkdir(control, 0700); err != nil {
						t.Fatal(err)
					}
				}
			}
			if tc.journal != "" {
				put(t, filepath.Join(control, "journal.json"), []byte(tc.journal))
			}
			if tc.setup == "journal-symlink" {
				link(t, "missing", filepath.Join(control, "journal.json"), true)
			}
			if tc.setup == "journal-hardlink" {
				put(t, filepath.Join(control, "source"), []byte(`{"version":1,"phase":"switched","projectId":"`+fixtureID+`"}`))
				link(t, filepath.Join(control, "source"), filepath.Join(control, "journal.json"), false)
			}
			l, err := OpenLibrary(library)
			if err != nil {
				t.Fatal(err)
			}
			defer l.Close()
			p, err := l.OpenProject(path)
			if p != nil {
				p.Close()
			}
			if (err == nil) != tc.allowed {
				t.Fatalf("allowed=%v error=%v", tc.allowed, err)
			}
		})
	}
}

func TestManifestAndDirectoryReplacementInvalidatesHandle(t *testing.T) {
	for _, action := range []string{"new identity", "same bytes new inode", "storage replaced", "project replaced", "library replaced", "legacy added"} {
		t.Run(action, func(t *testing.T) {
			library, path, storage := fixture(t)
			_, p := openFixture(t, library, path)
			switch action {
			case "new identity":
				put(t, filepath.Join(storage, ManifestName), []byte(strings.Replace(fixtureManifest, fixtureID, "12345678-1234-4234-8234-aaaaaaaaaaaa", 1)))
			case "same bytes new inode":
				put(t, filepath.Join(storage, "replacement"), []byte(fixtureManifest))
				if err := os.Rename(filepath.Join(storage, "replacement"), filepath.Join(storage, ManifestName)); err != nil {
					t.Fatal(err)
				}
			case "storage replaced":
				if err := os.Rename(storage, storage+"-old"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(storage, 0700); err != nil {
					t.Fatal(err)
				}
			case "project replaced":
				if err := os.Rename(path, path+"-old"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			case "library replaced":
				moved := filepath.Join(t.TempDir(), "moved")
				if err := os.Rename(library, moved); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(library, 0700); err != nil {
					t.Fatal(err)
				}
			case "legacy added":
				put(t, filepath.Join(path, ".vela"), []byte("legacy marker"))
			}
			if _, err := p.SnapshotDatabase(1024); err == nil {
				t.Fatal("stale identity accepted")
			}
			if f, err := p.OpenFileRead(ManifestName); err == nil {
				f.Close()
				t.Fatal("stale read accepted")
			}
		})
	}
}

func TestClosedHandles(t *testing.T) {
	library, path, _ := fixture(t)
	l, p := openFixture(t, library, path)
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := p.SnapshotDatabase(1024); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed snapshot: %v", err)
	}
	if _, err := p.OpenFileRead(ManifestName); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed read: %v", err)
	}
	p2, err := l.OpenProject(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p2.Close()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := l.OpenProject(path); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed library: %v", err)
	}
	if _, err := p2.SnapshotDatabase(1024); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed library child: %v", err)
	}
}

func TestDatabaseSnapshotOwnedFullSetAndBudget(t *testing.T) {
	library, path, storage := fixture(t)
	for _, suffix := range databaseSuffixes[1:] {
		put(t, filepath.Join(storage, DatabaseName+suffix), []byte("synthetic"+suffix))
	}
	_, p := openFixture(t, library, path)
	snapshot, err := p.SnapshotDatabase(1024)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Files) != 4 {
		t.Fatalf("missing database set: %v", snapshot.Files)
	}
	var total int64
	for suffix, data := range snapshot.Files {
		source, err := os.ReadFile(filepath.Join(storage, DatabaseName+suffix))
		if err != nil || !bytes.Equal(source, data) {
			t.Fatalf("copy %q: %v", suffix, err)
		}
		total += int64(len(data))
	}
	if _, err := p.SnapshotDatabase(total); err != nil {
		t.Fatalf("exact budget: %v", err)
	}
	for _, limit := range []int64{0, -1, total - 1, MaxSnapshotBytes + 1} {
		if _, err := p.SnapshotDatabase(limit); !errors.Is(err, ErrTooLarge) {
			t.Fatalf("limit=%d: %v", limit, err)
		}
	}
	snapshot.Files[""][0] ^= 0xff
	source, err := os.ReadFile(filepath.Join(storage, DatabaseName))
	if err != nil {
		t.Fatal(err)
	}
	if source[0] == snapshot.Files[""][0] {
		t.Fatal("snapshot aliases source")
	}
	fresh, err := p.SnapshotDatabase(1024)
	if err != nil || !bytes.Equal(fresh.Files[""], source) {
		t.Fatalf("fresh independent snapshot: %v", err)
	}
}

func TestDatabaseSnapshotMissingSidecarsAndUnsafeSidecars(t *testing.T) {
	library, path, storage := fixture(t)
	_, p := openFixture(t, library, path)
	snapshot, err := p.SnapshotDatabase(1024)
	if err != nil || len(snapshot.Files) != 1 {
		t.Fatalf("absent sidecars: %#v %v", snapshot.Files, err)
	}
	for _, suffix := range databaseSuffixes[1:] {
		for _, kind := range []string{"dangling-symlink", "hardlink", "fifo", "directory"} {
			t.Run(suffix+kind, func(t *testing.T) {
				file := filepath.Join(storage, DatabaseName+suffix)
				switch kind {
				case "dangling-symlink":
					link(t, "missing", file, true)
				case "hardlink":
					link(t, filepath.Join(storage, DatabaseName), file, false)
				case "fifo":
					if err := syscall.Mkfifo(file, 0600); err != nil {
						t.Fatal(err)
					}
				case "directory":
					if err := os.Mkdir(file, 0700); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := p.SnapshotDatabase(1024); err == nil {
					t.Fatal("unsafe sidecar copied")
				}
				remove(t, file)
			})
		}
	}
}
