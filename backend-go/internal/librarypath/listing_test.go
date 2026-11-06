package librarypath

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// listingFixture is one library root with an export directory a chooser would
// offer: eight visible entries (four directories, four files), one hidden
// directory, one hidden file and one symlink to an outside file. None of the last
// three may appear in an answer.
type listingFixture struct {
	root  string
	roots Roots
}

func newListingFixture(t *testing.T) listingFixture {
	t.Helper()
	root := physicalTempDir(t)
	outside := filepath.Join(physicalTempDir(t), "secret.txt")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatalf("seed the outside file: %v", err)
	}
	for _, name := range []string{"chapters", "exports", "notes", "outline"} {
		if err := os.MkdirAll(filepath.Join(root, name), 0o700); err != nil {
			t.Fatalf("create %v: %v", name, err)
		}
	}
	for _, name := range []string{"draft.md", "notes.md", "readme.md", "todo.md"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("body"), 0o600); err != nil {
			t.Fatalf("seed %v: %v", name, err)
		}
	}
	if err := os.MkdirAll(filepath.Join(root, ".storage"), 0o700); err != nil {
		t.Fatalf("create the hidden directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, ".hidden.md"), []byte("hidden"), 0o600); err != nil {
		t.Fatalf("seed the hidden file: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape.md")); err != nil {
		t.Fatalf("plant the escape link: %v", err)
	}
	roots, err := NewRoots([]Root{{ID: LibraryRootID, Kind: KindLibrary, Path: root}})
	if err != nil {
		t.Fatalf("build the allowlist: %v", err)
	}
	return listingFixture{root: root, roots: roots}
}

// TestListDirectoryRespectsItsCap is the bounded listing: depth 1, at most the
// requested number of entries, hidden names and symlinks skipped, and Truncated
// telling the page that more exist instead of presenting a partial tree as
// complete.
func TestListDirectoryRespectsItsCap(t *testing.T) {
	fixture := newListingFixture(t)
	target, err := fixture.roots.Resolve(LibraryRootID, "")
	if err != nil {
		t.Fatalf("resolve the listing root: %v", err)
	}
	// Eight visible entries, so a cap of three must answer three and say more
	// exist, in the order the shipped listing draws them: the comparator is copied
	// from internal/projectfs/read_tree.go:169-177, so directories come first and
	// only then the names. With four directories the first three are all
	// directories - a name-first order would have put draft.md second.
	capped, err := ListDirectory(target, 3)
	if err != nil {
		t.Fatalf("ListDirectory(cap 3): %v", err)
	}
	if len(capped.Entries) != 3 {
		t.Fatalf("ListDirectory(cap 3) answered %d entries; want 3", len(capped.Entries))
	}
	if !capped.Truncated {
		t.Fatal("ListDirectory(cap 3) did not report the entries it dropped")
	}
	if capped.Relative != "" {
		t.Fatalf("ListDirectory(cap 3) relative = %v; want the root itself", capped.Relative)
	}
	want := []string{"chapters", "exports", "notes"}
	for index, name := range want {
		if capped.Entries[index].Name != name {
			t.Fatalf("entry %d = %v; want %v (read_tree.go:169-177 order)", index, capped.Entries[index].Name, name)
		}
		if !capped.Entries[index].Dir {
			t.Fatalf("entry %d = %v; want a directory", index, capped.Entries[index].Name)
		}
	}

	// A cap of five reaches into the files: the four directories still come first,
	// then the first file by name, and the file carries its size.
	five, err := ListDirectory(target, 5)
	if err != nil {
		t.Fatalf("ListDirectory(cap 5): %v", err)
	}
	wantFive := []string{"chapters", "exports", "notes", "outline", "draft.md"}
	if len(five.Entries) != len(wantFive) || !five.Truncated {
		t.Fatalf("ListDirectory(cap 5) answered %d entries truncated=%v; want 5 and true",
			len(five.Entries), five.Truncated)
	}
	for index, name := range wantFive {
		if five.Entries[index].Name != name {
			t.Fatalf("entry %d = %v; want %v (read_tree.go:169-177 order)", index, five.Entries[index].Name, name)
		}
	}
	if !five.Entries[3].Dir || five.Entries[4].Dir || five.Entries[4].Size != 4 {
		t.Fatalf("cap 5 boundary = %+v; want the last directory then a 4-byte file", five.Entries[4])
	}

	// The whole directory fits in eight, so a cap of exactly eight is complete.
	whole, err := ListDirectory(target, 8)
	if err != nil {
		t.Fatalf("ListDirectory(cap 8): %v", err)
	}
	if len(whole.Entries) != 8 {
		t.Fatalf("ListDirectory(cap 8) answered %d entries; want 8", len(whole.Entries))
	}
	if whole.Truncated {
		t.Fatal("ListDirectory(cap 8) truncated a directory that fits")
	}
	wantWhole := []string{"chapters", "exports", "notes", "outline",
		"draft.md", "notes.md", "readme.md", "todo.md"}
	for index, name := range wantWhole {
		if whole.Entries[index].Name != name {
			t.Fatalf("entry %d = %v; want %v (read_tree.go:169-177 order)", index, whole.Entries[index].Name, name)
		}
	}
	for _, entry := range whole.Entries {
		if entry.Name == "escape.md" || entry.Name == ".hidden.md" || entry.Name == ".storage" {
			t.Fatalf("ListDirectory offered %v", entry.Name)
		}
	}

	// A zero cap means the documented default, not "unbounded".
	byDefault, err := ListDirectory(target, 0)
	if err != nil {
		t.Fatalf("ListDirectory(default cap): %v", err)
	}
	if len(byDefault.Entries) != 8 || byDefault.Truncated {
		t.Fatalf("ListDirectory(default cap) answered %d entries truncated=%v",
			len(byDefault.Entries), byDefault.Truncated)
	}
}

// TestListDirectoryRefusesWhatIsNotADirectory covers the three ways a listing
// request can be wrong: a page size outside the bounds, a file instead of a
// directory, and a target that could not have come from the allowlist.
func TestListDirectoryRefusesWhatIsNotADirectory(t *testing.T) {
	fixture := newListingFixture(t)
	file, err := fixture.roots.Resolve(LibraryRootID, "draft.md")
	if err != nil {
		t.Fatalf("resolve the file target: %v", err)
	}
	overCap := MaxListEntries + 1
	if _, err := ListDirectory(file, 0); !errors.Is(err, ErrPathNotDirectory) {
		t.Fatalf("ListDirectory on a file = %v; want %v", err, ErrPathNotDirectory)
	}
	if _, err := ListDirectory(file, overCap); !errors.Is(err, ErrListingInvalid) {
		t.Fatalf("ListDirectory(cap %d) = %v; want %v", overCap, err, ErrListingInvalid)
	}
	if _, err := ListDirectory(file, -1); !errors.Is(err, ErrListingInvalid) {
		t.Fatalf("ListDirectory(cap -1) = %v; want %v", err, ErrListingInvalid)
	}
	if _, err := ListDirectory(Target{Root: fixture.root, Path: fixture.root}, 0); !errors.Is(err, ErrPathInvalid) {
		t.Fatalf("ListDirectory on a forged target = %v; want %v", err, ErrPathInvalid)
	}
	missing, err := fixture.roots.Resolve(LibraryRootID, "absent")
	if err != nil {
		t.Fatalf("resolve the missing target: %v", err)
	}
	if _, err := ListDirectory(missing, 0); !errors.Is(err, ErrPathNotDirectory) {
		t.Fatalf("ListDirectory on a missing directory = %v; want %v", err, ErrPathNotDirectory)
	}
}
