package librarypath

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// Bounds and modes for the three filesystem effects a grant may cause. MaxFileBytes
// is the same 64 MiB budget projectfs.BrowserFileMaxBytes applies to a browser
// read (internal/projectfs/browser_files.go:18). An output file is created
// owner-only and published with 0644, which is what an export needs to be.
const (
	MaxFileBytes        = int64(64 << 20)
	OutputFileMode      = 0o644
	OutputDirectoryMode = 0o755
	tempNameBytes       = 16
)

// The refusal vocabulary of this file. ErrAuthorData is the important one: it is
// how a write or a mkdir into author data is refused, and it is why a grant is
// not a write capability (see the package doc).
var (
	// ErrAuthorData refuses a write or a mkdir whose target is author data: a
	// project root, anything below one, or anything below a directory holding a
	// project manifest. The caller must use the operation that owns that data and
	// the author database's single-writer lease instead.
	ErrAuthorData = errors.New("LIBRARY_PATH_AUTHOR_DATA")
	// ErrTooLarge refuses a body or a file above MaxFileBytes, or a read limit
	// outside the bounds this package is willing to hold in memory.
	ErrTooLarge = errors.New("LIBRARY_PATH_TOO_LARGE")
	// ErrNotText refuses bytes that are not valid UTF-8. Both the read and the
	// write side of the renderer treat the payload as text.
	ErrNotText = errors.New("LIBRARY_PATH_NOT_TEXT")
	// ErrUnsafeEntry refuses a path that is a directory where a file was needed,
	// a special file, or a hardlink-shaped entry this package will not serve.
	ErrUnsafeEntry = errors.New("LIBRARY_PATH_UNSAFE_ENTRY")
	// ErrCommitUnknown reports an atomic replacement that may have landed but
	// could not be re-verified. A caller must not report it as a plain failure.
	ErrCommitUnknown = errors.New("LIBRARY_PATH_COMMIT_UNKNOWN")
)

// ReadBytes reads one resolved regular file, bounded by limit, and refuses
// anything that is not text. The target's parent is re-pinned and the final
// component is opened through that root, so a symlink planted after the resolve
// is refused instead of followed.
func (t Target) ReadBytes(limit int64) ([]byte, error) {
	if !t.Valid() {
		return nil, ErrPathInvalid
	}
	if limit <= 0 || limit > MaxFileBytes {
		return nil, ErrTooLarge
	}
	parent, name, err := t.split()
	if err != nil {
		return nil, err
	}
	root, err := openPinned(parent)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	existing, statErr := root.Lstat(name)
	if statErr != nil {
		if errors.Is(statErr, fs.ErrNotExist) {
			return nil, fs.ErrNotExist
		}
		return nil, ErrPathUnavailable
	}
	if existing.Mode()&os.ModeSymlink != 0 {
		return nil, ErrSymlinkEscape
	}
	if !existing.Mode().IsRegular() {
		return nil, ErrUnsafeEntry
	}
	if existing.Size() > limit {
		return nil, ErrTooLarge
	}
	file, err := root.Open(name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fs.ErrNotExist
		}
		return nil, ErrPathUnavailable
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, ErrPathUnavailable
	}
	if int64(len(data)) > limit {
		return nil, ErrTooLarge
	}
	if !utf8.Valid(data) {
		return nil, ErrNotText
	}
	return data, nil
}

// WriteOutput atomically replaces (or, when create is set, creates) one output
// file. It refuses author data before touching anything: a target under a project
// root, and a target under any directory holding a project manifest, is
// ErrAuthorData and writes nothing. The replacement is a temporary file plus one
// rename, so a concurrent reader sees the old bytes or the new bytes.
func (t Target) WriteOutput(data []byte, create bool) error {
	if int64(len(data)) > MaxFileBytes {
		return ErrTooLarge
	}
	if err := t.requireOutput(); err != nil {
		return err
	}
	parent, name, err := t.split()
	if err != nil {
		return err
	}
	root, err := openPinned(parent)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	existing, statErr := root.Lstat(name)
	switch {
	case statErr == nil:
		if existing.Mode()&os.ModeSymlink != 0 {
			return ErrSymlinkEscape
		}
		if !existing.Mode().IsRegular() {
			return ErrUnsafeEntry
		}
	case errors.Is(statErr, fs.ErrNotExist):
		// A grant that only allowed "write an existing file" must not create one.
		if !create {
			return fs.ErrNotExist
		}
	default:
		return ErrPathUnavailable
	}
	return replaceOutputFile(root, name, data)
}

// MakeDirectory creates the missing components of one output path, one component
// at a time, each through its own pinned parent. An existing directory is
// success, so a repeated export does not fail; anything that exists and is not a
// directory is refused rather than replaced. Author data is refused first, on the
// same rule as WriteOutput.
func (t Target) MakeDirectory() error {
	if err := t.requireOutput(); err != nil {
		return err
	}
	if t.Relative == "" {
		return ErrPathInvalid
	}
	current := t.Root
	for _, part := range strings.Split(t.Relative, "/") {
		root, err := openPinned(current)
		if err != nil {
			return err
		}
		existing, statErr := root.Lstat(part)
		switch {
		case statErr == nil:
			if existing.Mode()&os.ModeSymlink != 0 {
				_ = root.Close()
				return ErrSymlinkEscape
			}
			if !existing.IsDir() {
				_ = root.Close()
				return ErrUnsafeEntry
			}
		case errors.Is(statErr, fs.ErrNotExist):
			if mkdirErr := root.Mkdir(part, OutputDirectoryMode); mkdirErr != nil && !errors.Is(mkdirErr, fs.ErrExist) {
				_ = root.Close()
				return ErrPathUnavailable
			}
		default:
			_ = root.Close()
			return ErrPathUnavailable
		}
		_ = root.Close()
		current = filepath.Join(current, part)
	}
	return nil
}

// requireOutput is the non-negotiable, expressed as code: a target is writable
// only when it is not author data. KindProject is the admitted project tree the
// caller's lease names; the manifest walk catches every other project directory
// inside the library, whose projects this server may not have admitted at all.
func (t Target) requireOutput() error {
	if !t.Valid() {
		return ErrPathInvalid
	}
	if t.Kind == KindProject {
		return ErrAuthorData
	}
	inside, err := holdsProjectManifest(t.Root, t.Relative)
	if err != nil {
		return err
	}
	if inside {
		return ErrAuthorData
	}
	return nil
}

// holdsProjectManifest walks the root and every ancestor of the target's final
// component and reports a directory that holds a project manifest. It does not
// inspect contents and does not recurse.
func holdsProjectManifest(root, relative string) (bool, error) {
	directories := []string{root}
	parts := strings.Split(relative, "/")
	for index := 0; index < len(parts)-1; index++ {
		directories = append(directories, filepath.Join(directories[len(directories)-1], parts[index]))
	}
	for _, directory := range directories {
		// The directory is inspected before the manifest inside it: Lstat on
		// "<file>/project.json" answers ENOTDIR, which is not ErrNotExist, and a
		// non-directory ancestor is not a project tree anyway - the caller's own
		// per-component walk refuses it with ErrUnsafeEntry.
		info, err := os.Lstat(directory)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return false, nil
			}
			return false, ErrPathUnavailable
		}
		if !info.IsDir() {
			return false, nil
		}
		if _, err := os.Lstat(filepath.Join(directory, ProjectManifestName)); err == nil {
			return true, nil
		} else if !errors.Is(err, fs.ErrNotExist) {
			return false, ErrPathUnavailable
		}
	}
	return false, nil
}

// split returns the resolved target's parent directory and final component. The
// root itself is not a file, so a target with no relative path is refused.
func (t Target) split() (string, string, error) {
	if !t.Valid() {
		return "", "", ErrPathInvalid
	}
	if t.Relative == "" {
		return "", "", ErrPathInvalid
	}
	return filepath.Dir(t.Path), filepath.Base(t.Path), nil
}

// openPinned opens one directory as an os.Root and proves the descriptor really
// is that directory: Lstat before, the root's own Stat, and an Lstat after it is
// opened must all name the same inode. This is the discipline
// internal/projectfs/project.go:62-81 applies to a project component, and it is
// what makes a swapped ancestor a refusal instead of a write into an unexpected
// place.
func openPinned(directory string) (*os.Root, error) {
	before, err := os.Lstat(directory)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fs.ErrNotExist
		}
		return nil, ErrPathUnavailable
	}
	if before.Mode()&os.ModeSymlink != 0 {
		return nil, ErrSymlinkEscape
	}
	if !before.IsDir() {
		return nil, ErrUnsafeEntry
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, ErrPathUnavailable
	}
	opened, openErr := root.Stat(".")
	after, afterErr := os.Lstat(directory)
	if openErr != nil || afterErr != nil || !opened.IsDir() ||
		after.Mode()&os.ModeSymlink != 0 ||
		!os.SameFile(before, opened) || !os.SameFile(before, after) {
		_ = root.Close()
		return nil, ErrChanged
	}
	return root, nil
}

// replaceOutputFile writes data to a fresh temporary file in the pinned parent and
// renames it over the final name, then reads it back through the same pinned
// parent. A readback that does not match is ErrCommitUnknown and not a plain
// failure, because the bytes may already be the file's content. The temporary
// name starts with a dot, so cleanRelative can never address it and a listing can
// never offer it.
func replaceOutputFile(parent *os.Root, name string, data []byte) error {
	temp, err := outputTempName()
	if err != nil {
		return err
	}
	file, err := parent.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return ErrPathUnavailable
	}
	discard := true
	defer func() {
		if discard {
			_ = parent.Remove(temp)
		}
	}()
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return ErrPathUnavailable
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return ErrPathUnavailable
	}
	if err := file.Close(); err != nil {
		return ErrPathUnavailable
	}
	written, statErr := parent.Lstat(temp)
	if statErr != nil || written.Mode()&os.ModeSymlink != 0 || !written.Mode().IsRegular() {
		return ErrUnsafeEntry
	}
	if err := parent.Rename(temp, name); err != nil {
		return ErrPathUnavailable
	}
	discard = false
	readback, err := parent.Open(name)
	if err != nil {
		return ErrCommitUnknown
	}
	defer func() { _ = readback.Close() }()
	contents, err := io.ReadAll(io.LimitReader(readback, MaxFileBytes+1))
	if err != nil || !bytes.Equal(contents, data) {
		return ErrCommitUnknown
	}
	return nil
}

// outputTempName is the temporary name of one replacement. It is unguessable so
// two concurrent exports cannot collide, and dot-prefixed so it stays inside the
// namespace the browser contract already refuses.
func outputTempName() (string, error) {
	var raw [tempNameBytes]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return ".write-" + hex.EncodeToString(raw[:]) + ".tmp", nil
}
