package librarypath

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
)

// physicalTempDir is one disposable directory whose path is already physical, so
// it can serve as a configured root. A test root that is reached through a symlink
// (a temporary-directory layout that resolves elsewhere) would be refused by
// NewRoots for the right reason but would test nothing.
func physicalTempDir(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	resolved, err := filepath.EvalSymlinks(directory)
	if err != nil {
		t.Fatalf("resolve the temporary directory: %v", err)
	}
	return resolved
}

// pathFixture is the whole allowlist grammar in one tree: an admitted project
// inside a configured library, an export directory beside it, a project manifest,
// a hidden storage directory, and three planted links - one to an outside file,
// one to an outside directory and one that points back inside its own root.
type pathFixture struct {
	base    string
	library string
	project string
	outside string
	roots   Roots
}

func newPathFixture(t *testing.T) pathFixture {
	t.Helper()
	base := physicalTempDir(t)
	library := filepath.Join(base, "library")
	project := filepath.Join(library, "book")
	outside := filepath.Join(base, "outside")
	for _, directory := range []string{
		filepath.Join(project, "manuscript"),
		filepath.Join(project, ".ai-novel"),
		filepath.Join(library, "exports", "chapters"),
		outside,
	} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatalf("create %v: %v", directory, err)
		}
	}
	for path, body := range map[string]string{
		filepath.Join(project, "manuscript", "chapter-1.md"): "第一章",
		filepath.Join(project, ProjectManifestName):          "{}",
		filepath.Join(outside, "secret.txt"):                 "outside",
	} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("seed %v: %v", path, err)
		}
	}
	for _, link := range []struct{ target, name string }{
		{filepath.Join(outside, "secret.txt"), filepath.Join(project, "escape.md")},
		{outside, filepath.Join(project, "escape-dir")},
		{filepath.Join(project, "manuscript"), filepath.Join(project, "alias")},
	} {
		if err := os.Symlink(link.target, link.name); err != nil {
			t.Fatalf("plant %v: %v", link.name, err)
		}
	}
	roots, err := NewRoots([]Root{
		{ID: ProjectRootID, Kind: KindProject, Path: project},
		{ID: LibraryRootID, Kind: KindLibrary, Path: library},
	})
	if err != nil {
		t.Fatalf("build the allowlist: %v", err)
	}
	return pathFixture{base: base, library: library, project: project, outside: outside, roots: roots}
}

// TestResolveConfinesToAllowlistedRoots is the allowlist grammar: what is inside a
// configured root resolves to a physical identity, what is outside is refused by
// name before anything is opened, and a symlink is a refusal rather than a path
// even when it points back inside its own root.
func TestResolveConfinesToAllowlistedRoots(t *testing.T) {
	fixture := newPathFixture(t)
	overlong := strings.Repeat("n", maxNameBytes+1)
	cases := []struct {
		name         string
		rootID       string
		requested    string
		want         error
		wantRelative string
		wantExists   bool
		wantDir      bool
	}{
		{name: "root-itself", rootID: ProjectRootID, requested: "",
			wantRelative: "", wantExists: true, wantDir: true},
		{name: "admitted-file", rootID: ProjectRootID, requested: "manuscript/chapter-1.md",
			wantRelative: "manuscript/chapter-1.md", wantExists: true},
		{name: "missing-child", rootID: ProjectRootID, requested: "manuscript/chapter-2.md",
			wantRelative: "manuscript/chapter-2.md", wantExists: false},
		{name: "library-relative", rootID: LibraryRootID, requested: "book/manuscript/chapter-1.md",
			wantRelative: "book/manuscript/chapter-1.md", wantExists: true},
		{name: "library-export-directory", rootID: LibraryRootID, requested: "exports/chapters",
			wantRelative: "exports/chapters", wantExists: true, wantDir: true},

		{name: "traversal", rootID: ProjectRootID, requested: "..", want: ErrPathTraversal},
		{name: "traversal-inside", rootID: ProjectRootID, requested: "manuscript/../chapter-1.md",
			want: ErrPathTraversal},
		{name: "traversal-to-library-sibling", rootID: LibraryRootID, requested: "../outside/secret.txt",
			want: ErrPathTraversal},
		{name: "absolute", rootID: ProjectRootID, requested: "/etc/passwd", want: ErrPathAbsolute},
		{name: "absolute-outside-root", rootID: ProjectRootID, requested: fixture.outside,
			want: ErrPathAbsolute},
		{name: "drive-letter", rootID: ProjectRootID, requested: "C:/Windows/win.ini",
			want: ErrPathAbsolute},
		{name: "backslash", rootID: ProjectRootID, requested: `manuscript\chapter-1.md`,
			want: ErrPathSeparator},
		{name: "encoded-separator", rootID: ProjectRootID, requested: "manuscript%2Fchapter-1.md",
			want: ErrPathEncoded},
		{name: "encoded-backslash", rootID: ProjectRootID, requested: "manuscript%5Cchapter-1.md",
			want: ErrPathEncoded},
		{name: "encoded-traversal", rootID: ProjectRootID, requested: "%2e%2e/outside/secret.txt",
			want: ErrPathEncoded},
		{name: "symlink-file-escape", rootID: ProjectRootID, requested: "escape.md",
			want: ErrSymlinkEscape},
		{name: "symlink-directory-escape", rootID: ProjectRootID, requested: "escape-dir/secret.txt",
			want: ErrSymlinkEscape},
		{name: "symlink-inside-root", rootID: ProjectRootID, requested: "alias/chapter-1.md",
			want: ErrSymlinkEscape},
		{name: "hidden-storage", rootID: ProjectRootID, requested: ".ai-novel/project.db",
			want: ErrPathHidden},
		{name: "hidden-dotfile", rootID: ProjectRootID, requested: ".env", want: ErrPathHidden},
		{name: "empty-component", rootID: ProjectRootID, requested: "manuscript//chapter-1.md",
			want: ErrPathInvalid},
		{name: "dot-component", rootID: ProjectRootID, requested: "./manuscript/chapter-1.md",
			want: ErrPathInvalid},
		{name: "overlong-component", rootID: ProjectRootID, requested: overlong,
			want: ErrPathInvalid},
		{name: "nul-byte", rootID: ProjectRootID, requested: "manuscript/\x00chapter-1.md",
			want: ErrPathInvalid},
		{name: "file-as-parent", rootID: ProjectRootID, requested: "manuscript/chapter-1.md/x.md",
			want: ErrPathNotDirectory},
		{name: "unknown-root", rootID: "elsewhere", requested: "", want: ErrRootUnknown},
		{name: "empty-root-id", rootID: "", requested: "", want: ErrRootUnknown},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			target, err := fixture.roots.Resolve(testCase.rootID, testCase.requested)
			if testCase.want != nil {
				if !errors.Is(err, testCase.want) {
					t.Fatalf("Resolve(%v, %v) = %v; want %v", testCase.rootID, testCase.requested, err, testCase.want)
				}
				return
			}
			if err != nil {
				t.Fatalf("Resolve(%v, %v): %v", testCase.rootID, testCase.requested, err)
			}
			if target.Relative != testCase.wantRelative {
				t.Fatalf("Resolve(%v, %v) relative = %v; want %v",
					testCase.rootID, testCase.requested, target.Relative, testCase.wantRelative)
			}
			if target.Exists != testCase.wantExists || target.IsDir != testCase.wantDir {
				t.Fatalf("Resolve(%v, %v) exists=%v dir=%v; want exists=%v dir=%v",
					testCase.rootID, testCase.requested, target.Exists, target.IsDir,
					testCase.wantExists, testCase.wantDir)
			}
			// The resolved identity is recomputed from the allowlisted root: the
			// request's string is never echoed back as a path.
			if !target.Valid() {
				t.Fatalf("Resolve(%v, %v) produced an invalid target", testCase.rootID, testCase.requested)
			}
			if !strings.HasPrefix(target.Path, target.Root) {
				t.Fatalf("Resolve(%v, %v) resolved outside its root", testCase.rootID, testCase.requested)
			}
		})
	}
}

// TestResolveRefusesASymlinkedRoot pins the second half of the escape rule: a root
// that is itself a symlink would make every descendant check meaningless, so the
// allowlist refuses it instead of resolving through it.
func TestResolveRefusesASymlinkedRoot(t *testing.T) {
	fixture := newPathFixture(t)
	link := filepath.Join(fixture.base, "library-link")
	if err := os.Symlink(fixture.library, link); err != nil {
		t.Fatalf("plant the root link: %v", err)
	}
	if _, err := NewRoots([]Root{{ID: LibraryRootID, Kind: KindLibrary, Path: link}}); !errors.Is(err, ErrRootInvalid) {
		t.Fatalf("NewRoots accepted a symlinked root: %v", err)
	}
}

// TestNewRootsRefusesUnusableCandidates covers the whole validation table, because
// a partially valid allowlist would answer questions about a tree the operator
// never configured.
func TestNewRootsRefusesUnusableCandidates(t *testing.T) {
	fixture := newPathFixture(t)
	file := filepath.Join(fixture.project, "manuscript", "chapter-1.md")
	cases := []struct {
		name       string
		candidates []Root
	}{
		{name: "no-candidates"},
		{name: "relative-path", candidates: []Root{{ID: LibraryRootID, Kind: KindLibrary, Path: "library"}}},
		{name: "unclean-path", candidates: []Root{{
			ID: LibraryRootID, Kind: KindLibrary, Path: fixture.library + "/./book/..",
		}}},
		{name: "missing-path", candidates: []Root{{
			ID: LibraryRootID, Kind: KindLibrary, Path: filepath.Join(fixture.base, "absent"),
		}}},
		{name: "file-path", candidates: []Root{{ID: LibraryRootID, Kind: KindLibrary, Path: file}}},
		{name: "empty-id", candidates: []Root{{ID: "", Kind: KindLibrary, Path: fixture.library}}},
		{name: "path-in-id", candidates: []Root{{ID: "library/x", Kind: KindLibrary, Path: fixture.library}}},
		{name: "unknown-kind", candidates: []Root{{ID: LibraryRootID, Kind: Kind(9), Path: fixture.library}}},
		{name: "zero-kind", candidates: []Root{{ID: LibraryRootID, Kind: 0, Path: fixture.library}}},
		{name: "duplicate-id", candidates: []Root{
			{ID: LibraryRootID, Kind: KindLibrary, Path: fixture.library},
			{ID: LibraryRootID, Kind: KindProject, Path: fixture.project},
		}},
		{name: "one-bad-candidate", candidates: []Root{
			{ID: LibraryRootID, Kind: KindLibrary, Path: fixture.library},
			{ID: ProjectRootID, Kind: KindProject, Path: "relative"},
		}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := NewRoots(testCase.candidates); !errors.Is(err, ErrRootInvalid) {
				t.Fatalf("NewRoots accepted %v: %v", testCase.name, err)
			}
		})
	}
}

// TestJoinRelativeRefusesEscapes is the grammar a grant's extent is validated
// with: a grant for a directory may be extended by one more relative request, and
// that request gets exactly the same refusals as the path that minted the grant.
func TestJoinRelativeRefusesEscapes(t *testing.T) {
	cases := []struct {
		name      string
		base      string
		requested string
		want      error
		wantPath  string
	}{
		{name: "empty-request", base: "exports", requested: "", wantPath: "exports"},
		{name: "empty-base", base: "", requested: "exports/chapters", wantPath: "exports/chapters"},
		{name: "both", base: "exports", requested: "chapters/one.md", wantPath: "exports/chapters/one.md"},
		{name: "base-traversal", base: "../outside", requested: "x", want: ErrPathTraversal},
		{name: "request-traversal", base: "exports", requested: "../outside/x", want: ErrPathTraversal},
		{name: "request-absolute", base: "exports", requested: "/etc/passwd", want: ErrPathAbsolute},
		{name: "request-separator", base: "exports", requested: `a\b`, want: ErrPathSeparator},
		{name: "request-encoded", base: "exports", requested: "a%2fb", want: ErrPathEncoded},
		{name: "request-hidden", base: "exports", requested: ".git/config", want: ErrPathHidden},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			joined, err := JoinRelative(testCase.base, testCase.requested)
			if testCase.want != nil {
				if !errors.Is(err, testCase.want) {
					t.Fatalf("JoinRelative(%v, %v) = %v; want %v",
						testCase.base, testCase.requested, err, testCase.want)
				}
				return
			}
			if err != nil {
				t.Fatalf("JoinRelative(%v, %v): %v", testCase.base, testCase.requested, err)
			}
			if joined != testCase.wantPath {
				t.Fatalf("JoinRelative(%v, %v) = %v; want %v",
					testCase.base, testCase.requested, joined, testCase.wantPath)
			}
		})
	}
}

// TestTargetValidRefusesAForgedIdentity is the rule that keeps the token store
// honest: a Target is only acceptable when its Path is exactly its Relative joined
// onto its Root, so a caller cannot mint a token for a path the allowlist never
// produced.
func TestTargetValidRefusesAForgedIdentity(t *testing.T) {
	fixture := newPathFixture(t)
	good, err := fixture.roots.Resolve(LibraryRootID, "exports/chapters")
	if err != nil {
		t.Fatalf("resolve the acceptance case: %v", err)
	}
	if !good.Valid() {
		t.Fatal("a resolved target reports itself invalid")
	}
	forged := []Target{
		{},
		{RootID: LibraryRootID, Kind: KindLibrary, Root: fixture.library, Relative: "", Path: filepath.Join(fixture.library, "exports")},
		{RootID: "", Kind: KindLibrary, Root: fixture.library, Relative: "exports", Path: filepath.Join(fixture.library, "exports")},
		{RootID: LibraryRootID, Kind: Kind(7), Root: fixture.library, Relative: "exports", Path: filepath.Join(fixture.library, "exports")},
		{RootID: LibraryRootID, Kind: KindLibrary, Root: fixture.library, Relative: "../outside", Path: filepath.Join(fixture.base, "outside")},
		{RootID: LibraryRootID, Kind: KindLibrary, Root: fixture.library, Relative: "exports", Path: filepath.Join(fixture.outside, "exports")},
		{RootID: LibraryRootID, Kind: KindLibrary, Root: "relative", Relative: "exports", Path: "relative/exports"},
	}
	for index, candidate := range forged {
		if candidate.Valid() {
			t.Fatalf("forged target %d reports itself valid", index)
		}
	}
}

// TestProjectManifestNameMatchesProjectFS keeps this package's copy of the marker
// name honest: the layering forbids importing projectfs, so a rename there has to
// fail here instead of silently disabling the output rule in io.go.
func TestProjectManifestNameMatchesProjectFS(t *testing.T) {
	if ProjectManifestName != projectfs.ManifestName {
		t.Fatalf("ProjectManifestName = %v; projectfs.ManifestName = %v",
			ProjectManifestName, projectfs.ManifestName)
	}
}
