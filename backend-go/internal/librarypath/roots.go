package librarypath

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// Bounds. MaxPathBytes is the ceiling the transport lane already applies to a
// browser path (internal/transport/editor_nav_reads.go:110-113) and the one
// projectfs.BrowserRelativePathMaxBytes uses, so one refusal vocabulary covers
// both surfaces. maxNameBytes is the filesystem's own single-component limit.
const (
	MaxPathBytes = 4096
	maxNameBytes = 255
)

// ProjectManifestName is the marker that makes a directory an author project
// tree. It mirrors projectfs.ManifestName (internal/projectfs/manifest.go:18),
// which Roots cannot import without inverting the layering, and a test pins the
// two together. It is only used to refuse output writes into a project tree
// (io.go requireOutput).
const ProjectManifestName = "project.json"

// Symbolic root ids. They are constants rather than caller input so a token
// minted for a root stays usable across requests while the root's path is
// re-derived from configuration every time.
const (
	ProjectRootID = "project"
	LibraryRootID = "library"
)

// Kind is what an allowlisted root is. It decides one thing and one thing only:
// a KindProject root is author data, so a write or a mkdir into it is refused
// (io.go). Reads are allowed under both kinds.
type Kind uint8

const (
	// KindProject is the project tree this server admitted for the caller.
	KindProject Kind = 1
	// KindLibrary is the configured library directory: the root every project
	// directory is a child of, and the only place a browser export can land.
	KindLibrary Kind = 2
)

func (k Kind) valid() bool { return k == KindProject || k == KindLibrary }

func (k Kind) String() string {
	switch k {
	case KindProject:
		return "project"
	case KindLibrary:
		return "library"
	}
	return ""
}

// The refusal vocabulary. Every one is symbolic and carries no host path, so a
// refusal can travel to a browser through a finite code table without ever
// naming a file the page did not ask for.
var (
	// ErrRootInvalid refuses an allowlist candidate that cannot be a physical
	// configured root: a relative, unclean or symlinked path, a missing or
	// non-directory path, an empty or duplicated id, or an unknown kind.
	ErrRootInvalid = errors.New("LIBRARY_PATH_ROOT_INVALID")
	// ErrRootUnknown refuses a request for a root id this allowlist does not
	// hold. It is how "no root known to belong to the admitted project or
	// library" fails: the resolver never falls back to a path of its own.
	ErrRootUnknown = errors.New("LIBRARY_PATH_ROOT_UNKNOWN")
	// ErrPathInvalid refuses an unusable path string: empty and "." components,
	// an overlong component, an overlong path, invalid UTF-8, a NUL byte.
	ErrPathInvalid = errors.New("LIBRARY_PATH_INVALID")
	// ErrPathAbsolute refuses an absolute path, a leading separator or a drive
	// letter. A browser never names an absolute path.
	ErrPathAbsolute = errors.New("LIBRARY_PATH_ABSOLUTE")
	// ErrPathSeparator refuses a backslash component separator.
	ErrPathSeparator = errors.New("LIBRARY_PATH_SEPARATOR")
	// ErrPathEncoded refuses a separator, a dot or a traversal that reached this
	// layer inside a percent escape - "%2f", "%5c", "%2e" - which is how a
	// separator smuggled through one URL-decoding hop arrives.
	ErrPathEncoded = errors.New("LIBRARY_PATH_ENCODED")
	// ErrPathTraversal refuses a ".." component, anywhere in the path.
	ErrPathTraversal = errors.New("LIBRARY_PATH_TRAVERSAL")
	// ErrPathHidden refuses a component beginning with ".". The server's own
	// storage directory, its database and its temporary names live there, and a
	// browser has no business addressing any of them.
	ErrPathHidden = errors.New("LIBRARY_PATH_HIDDEN")
	// ErrPathNotDirectory refuses a path that walks through something which is
	// not a real directory.
	ErrPathNotDirectory = errors.New("LIBRARY_PATH_NOT_DIRECTORY")
	// ErrPathUnavailable refuses a path whose state could not be observed. Not
	// knowing is a refusal, never an assumption of absence.
	ErrPathUnavailable = errors.New("LIBRARY_PATH_UNAVAILABLE")
	// ErrSymlinkEscape refuses a symlink anywhere in a resolved path, including
	// one that points back inside its root: the server did not mint that name,
	// and it can be re-pointed after the check.
	ErrSymlinkEscape = errors.New("LIBRARY_PATH_SYMLINK_ESCAPE")
	// ErrChanged refuses a directory that was swapped between the checks that
	// were supposed to identify it.
	ErrChanged = errors.New("LIBRARY_PATH_CHANGED")
)

// Root is one allowlisted root. Path must already be an absolute, clean,
// physical directory: Roots refuses anything else rather than repairing it,
// because a root comes from trusted configuration and a repair would hide a
// configuration bug behind a path the operator never chose.
type Root struct {
	ID   string
	Kind Kind
	Path string
}

// Roots is the allowlist for one request. It is a value and must not be built
// once and reused across requests: the project root comes from the caller's live
// lease, so it is per-caller by construction.
type Roots struct {
	roots []Root
}

// Target is the resolved identity of one addressable path. Every field is
// derived by this package, never copied from a caller's string: Relative is the
// canonical form of the request, and Path is Relative joined onto an allowlisted
// root. Path is server-side only - it is what the io methods open - and must
// never be placed on a wire.
type Target struct {
	RootID   string
	Kind     Kind
	Root     string
	Relative string
	Path     string
	Exists   bool
	IsDir    bool
	Size     int64
}

// NewRoots validates an allowlist. It refuses the whole set if one candidate is
// unusable, because a partially valid allowlist would silently answer questions
// about a tree the operator did not configure. Duplicate ids are refused for the
// same reason: two roots under one id make a request ambiguous.
func NewRoots(candidates []Root) (Roots, error) {
	if len(candidates) == 0 {
		return Roots{}, ErrRootInvalid
	}
	seen := make(map[string]bool, len(candidates))
	roots := make([]Root, 0, len(candidates))
	for _, candidate := range candidates {
		if !validRootID(candidate.ID) || !candidate.Kind.valid() || seen[candidate.ID] {
			return Roots{}, ErrRootInvalid
		}
		if !canonicalAbsolute(candidate.Path) || !physicalRoot(candidate.Path) {
			return Roots{}, ErrRootInvalid
		}
		seen[candidate.ID] = true
		roots = append(roots, candidate)
	}
	return Roots{roots: roots}, nil
}

// Resolve is the one door from a request to a path. requested is relative to the
// named root, may be empty (the root itself), and is validated by cleanRelative
// before anything is touched. The returned Target is the resolved identity: its
// Path is recomputed from the allowlisted root, never taken from the request.
func (r Roots) Resolve(rootID, requested string) (Target, error) {
	root, ok := r.lookup(rootID)
	if !ok {
		return Target{}, ErrRootUnknown
	}
	relative, err := cleanRelative(requested)
	if err != nil {
		return Target{}, err
	}
	target := Target{RootID: root.ID, Kind: root.Kind, Root: root.Path, Relative: relative, Path: root.Path}
	if relative != "" {
		target.Path = filepath.Join(root.Path, filepath.FromSlash(relative))
	}
	info, exists, err := verifyChain(root.Path, relative)
	if err != nil {
		return Target{}, err
	}
	target.Exists = exists
	if exists {
		target.IsDir = info.IsDir()
		target.Size = info.Size()
	}
	return target, nil
}

// Valid reports whether a Target could have come from Resolve: a known root kind,
// a canonical relative path, and a Path that is exactly that relative path joined
// onto that root. The Registry refuses a Target that fails it, so a caller
// cannot hand the token store a path the allowlist never produced.
func (t Target) Valid() bool {
	if !validRootID(t.RootID) || !t.Kind.valid() || !canonicalAbsolute(t.Root) || !canonicalAbsolute(t.Path) {
		return false
	}
	relative, err := cleanRelative(t.Relative)
	if err != nil || relative != t.Relative {
		return false
	}
	expected := t.Root
	if t.Relative != "" {
		expected = filepath.Join(t.Root, filepath.FromSlash(t.Relative))
	}
	return t.Path == expected
}

// JoinRelative extends an already resolved target's relative path with one more
// relative request, which is how a grant for a directory names a child. Both
// halves are validated, so a traversal in either is refused here and again by
// the Resolve that follows.
func JoinRelative(base, requested string) (string, error) {
	cleanBase, err := cleanRelative(base)
	if err != nil {
		return "", err
	}
	cleanRequested, err := cleanRelative(requested)
	if err != nil {
		return "", err
	}
	switch {
	case cleanRequested == "":
		return cleanBase, nil
	case cleanBase == "":
		return cleanRequested, nil
	}
	joined := cleanBase + "/" + cleanRequested
	if len(joined) > MaxPathBytes {
		return "", ErrPathInvalid
	}
	return joined, nil
}

// lookup finds the root for an id. A nil Roots has no root at all.
func (r Roots) lookup(rootID string) (Root, bool) {
	for _, root := range r.roots {
		if root.ID == rootID {
			return root, true
		}
	}
	return Root{}, false
}

// cleanRelative validates a slash-separated path relative to a root and returns
// it unchanged, because every rule below also makes the canonical form identical
// to the input. The order matters: the separator and encoding refusals must
// precede the absolute check, so "\\etc\\passwd" and "%2Fetc%2Fpasswd" are
// refused as what they are instead of being reported as merely absolute.
func cleanRelative(requested string) (string, error) {
	if requested == "" {
		return "", nil
	}
	if len(requested) > MaxPathBytes || !utf8.ValidString(requested) || strings.ContainsRune(requested, 0) {
		return "", ErrPathInvalid
	}
	if strings.ContainsRune(requested, '\\') {
		return "", ErrPathSeparator
	}
	if smuggledPathEscape(requested) {
		return "", ErrPathEncoded
	}
	if strings.HasPrefix(requested, "/") || filepath.IsAbs(requested) || windowsDrive(requested) {
		return "", ErrPathAbsolute
	}
	// The shipped renderer lists and reads its own project skills and prompt
	// templates under the storage directory, so those two subtrees stay
	// addressable; every other hidden component - the database, the manifest and
	// the knowledge store - remains out of reach.
	authorVisible := authorVisibleLibraryPath(requested)
	for _, part := range strings.Split(requested, "/") {
		switch {
		case part == "..":
			return "", ErrPathTraversal
		case part == "" || part == ".":
			return "", ErrPathInvalid
		case strings.HasPrefix(part, ".") && !authorVisible:
			return "", ErrPathHidden
		case len(part) > maxNameBytes:
			return "", ErrPathInvalid
		}
	}
	return requested, nil
}

// authorVisibleLibraryPath reports whether a relative path names the two
// storage subtrees the shipped renderer addresses by path: the author's project
// skills and prompt templates. The literal must match projectfs.StorageDirectory.
func authorVisibleLibraryPath(relative string) bool {
	for _, name := range []string{"skills", "prompts"} {
		prefix := ".ai-novel/" + name
		if relative == prefix || strings.HasPrefix(relative, prefix+"/") {
			return true
		}
	}
	return false
}

// smuggledPathEscape reports a percent escape that decodes to a separator or a
// dot. The body is JSON, so a value that was URL-decoded one hop too early
// arrives here spelling "%2f" or "%2e%2e" literally; a filename has no business
// containing either.
func smuggledPathEscape(value string) bool {
	for index := 0; index+2 < len(value); index++ {
		if value[index] != '%' {
			continue
		}
		pair := value[index+1 : index+3]
		if strings.EqualFold(pair, "2f") || strings.EqualFold(pair, "5c") || strings.EqualFold(pair, "2e") {
			return true
		}
	}
	return false
}

// windowsDrive reports a "<letter>:" prefix. filepath.IsAbs is false for it on a
// POSIX filesystem, so the drive form is refused by name instead.
func windowsDrive(value string) bool {
	if len(value) < 2 || value[1] != ':' {
		return false
	}
	first := value[0]
	return first >= 'a' && first <= 'z' || first >= 'A' && first <= 'Z'
}

// verifyChain walks one component at a time with Lstat and refuses the first
// symlink, non-directory or unobservable entry. It returns the final component's
// info and whether it exists. A missing component is not an error: the rest of
// the path cannot exist either, and a write or mkdir grant may be minted for a
// name that does not exist yet.
func verifyChain(rootPath, relative string) (fs.FileInfo, bool, error) {
	if relative == "" {
		// A root that cannot be observed is unavailable, never absent: NewRoots
		// already proved it existed when the allowlist was built.
		info, err := os.Lstat(rootPath)
		if err != nil {
			return nil, false, ErrPathUnavailable
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, false, ErrSymlinkEscape
		}
		if !info.IsDir() {
			return nil, false, ErrRootInvalid
		}
		return info, true, nil
	}
	parts := strings.Split(relative, "/")
	current := rootPath
	for index, part := range parts {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil, false, nil
			}
			return nil, false, ErrPathUnavailable
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, false, ErrSymlinkEscape
		}
		final := index == len(parts)-1
		if !final && !info.IsDir() {
			return nil, false, ErrPathNotDirectory
		}
		if final {
			return info, true, nil
		}
	}
	return nil, false, ErrPathInvalid
}

// canonicalAbsolute is the path grammar a configured root must satisfy: absolute,
// already clean, bounded, valid UTF-8, and free of NUL and backslash.
func canonicalAbsolute(path string) bool {
	return path != "" && len(path) <= MaxPathBytes && utf8.ValidString(path) &&
		!strings.ContainsAny(path, "\x00\\") && filepath.IsAbs(path) && filepath.Clean(path) == path
}

// physicalRoot reports a configured root that really is the directory it names:
// a real directory, not a symlink, with no symlinked ancestor. A symlinked root
// would make every descendant check below meaningless.
func physicalRoot(path string) bool {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return false
	}
	resolved, err := filepath.EvalSymlinks(path)
	return err == nil && resolved == path
}

// validRootID bounds a symbolic root id. The id travels through a token record,
// so it may not contain a NUL or the separator this package's digests use.
func validRootID(id string) bool {
	return id != "" && len(id) <= 64 && utf8.ValidString(id) &&
		!strings.ContainsAny(id, "\x00/\\:")
}
