package librarypath

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// treeSnapshot records every entry under root by relative path: a regular file by
// the digest of its bytes, a symlink by its target text. It is how "a failed grant
// writes nothing" is measured - a refusal that created, replaced or removed one
// entry fails the comparison.
func treeSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	snapshot := make(map[string]string)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if relative == "." || entry.IsDir() {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			snapshot[relative] = "symlink->" + target
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		digest := sha256.Sum256(data)
		snapshot[relative] = hex.EncodeToString(digest[:])
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot %v: %v", root, err)
	}
	return snapshot
}

// TestReadBytesIsConfinedAndBounded is the read half of the io layer: the bytes a
// grant's target really holds, a symlink planted there refused instead of
// followed, and the size and encoding bounds enforced before anything is served.
func TestReadBytesIsConfinedAndBounded(t *testing.T) {
	fixture := newPathFixture(t)
	chapter, err := fixture.roots.Resolve(ProjectRootID, "manuscript/chapter-1.md")
	if err != nil {
		t.Fatalf("resolve the chapter: %v", err)
	}
	data, err := chapter.ReadBytes(MaxFileBytes)
	if err != nil {
		t.Fatalf("ReadBytes: %v", err)
	}
	if string(data) != "第一章" {
		t.Fatalf("ReadBytes returned %v bytes; want the seeded chapter", len(data))
	}
	if _, err := chapter.ReadBytes(2); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("ReadBytes(limit 2) = %v; want %v", err, ErrTooLarge)
	}
	if _, err := chapter.ReadBytes(0); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("ReadBytes(limit 0) = %v; want %v", err, ErrTooLarge)
	}
	if _, err := chapter.ReadBytes(MaxFileBytes + 1); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("ReadBytes(over the ceiling) = %v; want %v", err, ErrTooLarge)
	}

	// A target that resolve would refuse, reconstructed to prove the io layer does
	// not trust a target it was merely handed: the final component is a link.
	link := Target{
		RootID:   ProjectRootID,
		Kind:     KindProject,
		Root:     fixture.project,
		Relative: "escape.md",
		Path:     filepath.Join(fixture.project, "escape.md"),
	}
	if !link.Valid() {
		t.Fatal("the escape target should be a well formed identity")
	}
	if _, err := link.ReadBytes(MaxFileBytes); !errors.Is(err, ErrSymlinkEscape) {
		t.Fatalf("ReadBytes on a symlink = %v; want %v", err, ErrSymlinkEscape)
	}

	missing, err := fixture.roots.Resolve(ProjectRootID, "manuscript/chapter-9.md")
	if err != nil {
		t.Fatalf("resolve the missing chapter: %v", err)
	}
	if _, err := missing.ReadBytes(MaxFileBytes); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("ReadBytes on a missing file = %v; want %v", err, fs.ErrNotExist)
	}

	directory, err := fixture.roots.Resolve(ProjectRootID, "manuscript")
	if err != nil {
		t.Fatalf("resolve the directory: %v", err)
	}
	if _, err := directory.ReadBytes(MaxFileBytes); !errors.Is(err, ErrUnsafeEntry) {
		t.Fatalf("ReadBytes on a directory = %v; want %v", err, ErrUnsafeEntry)
	}

	binary := filepath.Join(fixture.project, "manuscript", "scan.bin")
	if err := os.WriteFile(binary, []byte{0xff, 0xfe, 0x00, 0x01}, 0o600); err != nil {
		t.Fatalf("seed the binary file: %v", err)
	}
	binaryTarget, err := fixture.roots.Resolve(ProjectRootID, "manuscript/scan.bin")
	if err != nil {
		t.Fatalf("resolve the binary file: %v", err)
	}
	if _, err := binaryTarget.ReadBytes(MaxFileBytes); !errors.Is(err, ErrNotText) {
		t.Fatalf("ReadBytes on non-text = %v; want %v", err, ErrNotText)
	}
}

// TestWriteOutputRefusesAuthorData is the non-negotiable as a test: a write whose
// target is the admitted project tree, or any directory below the library that
// holds a project manifest, is refused and leaves the tree byte for byte as it
// was.
func TestWriteOutputRefusesAuthorData(t *testing.T) {
	fixture := newPathFixture(t)
	if err := os.MkdirAll(filepath.Join(fixture.library, "second-book"), 0o700); err != nil {
		t.Fatalf("create the second project: %v", err)
	}
	if err := os.WriteFile(filepath.Join(fixture.library, "second-book", ProjectManifestName), []byte("{}"), 0o600); err != nil {
		t.Fatalf("seed the second manifest: %v", err)
	}
	before := treeSnapshot(t, fixture.base)

	admitting, err := fixture.roots.Resolve(ProjectRootID, "manuscript")
	if err != nil {
		t.Fatalf("resolve the project directory: %v", err)
	}
	if err := admitting.WriteOutput([]byte("author data"), true); !errors.Is(err, ErrAuthorData) {
		t.Fatalf("WriteOutput into the admitted project = %v; want %v", err, ErrAuthorData)
	}
	// The library root is addressable, but a child project directory is not an
	// output: the manifest walk catches it even though this server never admitted
	// that project.
	sibling, err := fixture.roots.Resolve(LibraryRootID, "second-book/manuscript")
	if err != nil {
		t.Fatalf("resolve the sibling project: %v", err)
	}
	if err := sibling.WriteOutput([]byte("author data"), true); !errors.Is(err, ErrAuthorData) {
		t.Fatalf("WriteOutput into a sibling project = %v; want %v", err, ErrAuthorData)
	}
	if err := sibling.MakeDirectory(); !errors.Is(err, ErrAuthorData) {
		t.Fatalf("MakeDirectory in a sibling project = %v; want %v", err, ErrAuthorData)
	}
	if after := treeSnapshot(t, fixture.base); !reflect.DeepEqual(before, after) {
		t.Fatal("a refused output write changed the tree")
	}
}

// TestWriteOutputPublishesOneOutputFile is the accepted half: a real output
// directory under the library root is written atomically, a replacement is what a
// reader sees next, and a grant without create permission cannot bring a new file
// into being.
func TestWriteOutputPublishesOneOutputFile(t *testing.T) {
	fixture := newPathFixture(t)
	chapter := filepath.Join(fixture.library, "exports", "chapters", "one.md")
	target, err := fixture.roots.Resolve(LibraryRootID, "exports/chapters/one.md")
	if err != nil {
		t.Fatalf("resolve the output path: %v", err)
	}
	if err := target.WriteOutput([]byte("first"), false); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("WriteOutput without create = %v; want %v", err, fs.ErrNotExist)
	}
	if _, err := os.Stat(chapter); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a write without create permission created the file: %v", err)
	}
	if err := target.WriteOutput([]byte("first"), true); err != nil {
		t.Fatalf("WriteOutput: %v", err)
	}
	if body, err := os.ReadFile(chapter); err != nil || string(body) != "first" {
		t.Fatalf("the output file does not hold the written bytes: %v", err)
	}
	if err := target.WriteOutput([]byte("second"), false); err != nil {
		t.Fatalf("WriteOutput over an existing file: %v", err)
	}
	if body, err := os.ReadFile(chapter); err != nil || string(body) != "second" {
		t.Fatalf("the replacement was not published: %v", err)
	}
	if err := target.WriteOutput(make([]byte, MaxFileBytes+1), true); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("WriteOutput over the ceiling = %v; want %v", err, ErrTooLarge)
	}

	// A target whose parent is a link is refused before anything is created.
	escaped := Target{
		RootID:   LibraryRootID,
		Kind:     KindLibrary,
		Root:     fixture.library,
		Relative: "exports/link",
		Path:     filepath.Join(fixture.library, "exports", "link"),
	}
	if err := os.Symlink(filepath.Join(fixture.library, "exports", "chapters"),
		filepath.Join(fixture.library, "exports", "link")); err != nil {
		t.Fatalf("plant the directory link: %v", err)
	}
	if err := escaped.WriteOutput([]byte("body"), true); !errors.Is(err, ErrSymlinkEscape) {
		t.Fatalf("WriteOutput through a linked parent = %v; want %v", err, ErrSymlinkEscape)
	}
	if err := escaped.MakeDirectory(); !errors.Is(err, ErrSymlinkEscape) {
		t.Fatalf("MakeDirectory through a linked parent = %v; want %v", err, ErrSymlinkEscape)
	}
}

// TestMakeDirectoryCreatesOneOutputTree covers the mkdir half: missing components
// are created one at a time, an existing directory is success, and anything that
// exists and is not a directory is refused rather than replaced.
func TestMakeDirectoryCreatesOneOutputTree(t *testing.T) {
	fixture := newPathFixture(t)
	nested, err := fixture.roots.Resolve(LibraryRootID, "exports/two/three")
	if err != nil {
		t.Fatalf("resolve the nested output: %v", err)
	}
	if err := nested.MakeDirectory(); err != nil {
		t.Fatalf("MakeDirectory: %v", err)
	}
	info, err := os.Lstat(filepath.Join(fixture.library, "exports", "two", "three"))
	if err != nil || !info.IsDir() {
		t.Fatalf("MakeDirectory did not create the nested directory: %v", err)
	}
	if err := nested.MakeDirectory(); err != nil {
		t.Fatalf("MakeDirectory on an existing directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(fixture.library, "exports", "file.txt"), []byte("body"), 0o600); err != nil {
		t.Fatalf("seed the output file: %v", err)
	}
	// Resolve refuses a path that walks through a file, so the only way to hand
	// MakeDirectory that shape is to construct the identity directly - which is
	// exactly what a forged target would look like.
	blocked := Target{
		RootID:   LibraryRootID,
		Kind:     KindLibrary,
		Root:     fixture.library,
		Relative: "exports/file.txt/deeper",
		Path:     filepath.Join(fixture.library, "exports", "file.txt", "deeper"),
	}
	if err := blocked.MakeDirectory(); !errors.Is(err, ErrUnsafeEntry) {
		t.Fatalf("MakeDirectory through a file = %v; want %v", err, ErrUnsafeEntry)
	}
	rootTarget, err := fixture.roots.Resolve(LibraryRootID, "")
	if err != nil {
		t.Fatalf("resolve the library root: %v", err)
	}
	if err := rootTarget.MakeDirectory(); !errors.Is(err, ErrPathInvalid) {
		t.Fatalf("MakeDirectory on the root itself = %v; want %v", err, ErrPathInvalid)
	}
}

// TestProjectDirectoryIsNotAnOutput pins the manifest rule at the boundary: a
// directory that merely looks like a project (no manifest) is an output, the one
// beside it that holds a manifest is not.
func TestProjectDirectoryIsNotAnOutput(t *testing.T) {
	fixture := newPathFixture(t)
	plain, err := fixture.roots.Resolve(LibraryRootID, "exports/plain")
	if err != nil {
		t.Fatalf("resolve the plain output: %v", err)
	}
	if err := plain.MakeDirectory(); err != nil {
		t.Fatalf("MakeDirectory beside a project: %v", err)
	}
	plainFile, err := fixture.roots.Resolve(LibraryRootID, "exports/plain/out.md")
	if err != nil {
		t.Fatalf("resolve the plain output file: %v", err)
	}
	if err := plainFile.WriteOutput([]byte("exported"), true); err != nil {
		t.Fatalf("WriteOutput beside a project: %v", err)
	}
	inside, err := fixture.roots.Resolve(LibraryRootID, "book/exports")
	if err != nil {
		t.Fatalf("resolve the project child: %v", err)
	}
	if err := inside.MakeDirectory(); !errors.Is(err, ErrAuthorData) {
		t.Fatalf("MakeDirectory inside the project = %v; want %v", err, ErrAuthorData)
	}
	if err := inside.WriteOutput([]byte("body"), true); !errors.Is(err, ErrAuthorData) {
		t.Fatalf("WriteOutput inside the project = %v; want %v", err, ErrAuthorData)
	}
}
