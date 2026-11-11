package projectfs

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// BrowserFileMaxBytes bounds one browser file read or write. The desktop lane
// read a manuscript straight off the host with no size ceiling because the
// author's own machine paid for it; a shared server must bound every body it
// holds in memory for one request.
const BrowserFileMaxBytes = int64(64 << 20)

// BrowserRelativePathMaxBytes bounds the path itself so a pathological name
// cannot be expanded component by component before it is refused.
const BrowserRelativePathMaxBytes = 4096

var (
	// ErrBrowserPathRefused is the refusal of the browser file contract below.
	// It is deliberately distinct from ErrInvalidPath so a caller can tell "the
	// browser named something the web surface does not accept" from "the pinned
	// project tree changed underneath us".
	ErrBrowserPathRefused = errors.New("PROJECT_FILE_PATH_REFUSED")
	// ErrBrowserCommitUnknown reports an atomic replacement that may have
	// landed but could not be re-verified. A caller must never report it as a
	// plain failure, because the bytes can already be the project's content.
	ErrBrowserCommitUnknown = errors.New("PROJECT_FILE_COMMIT_UNKNOWN")
)

// This file states the STRICTER web contract that replaces the desktop fs:*
// family (electron/controllers/fs-controller.ts). The desktop main process
// accepted an ABSOLUTE host path and confined it with a realpath comparison
// against the canonical project root, because that process already owned the
// host. A browser-facing server must not: an absolute-path route is a privilege
// escalation waiting for one missing comparison, and it lets a page address any
// path the server user can reach (/etc/passwd, another author's library entry,
// this lane's private config root).
//
// The web contract is therefore:
//
//  1. The browser names a path RELATIVE to the admitted project root. Absolute
//     paths, drive letters, backslashes, NUL bytes, empty/"."/".." components
//     and non-canonical spellings are refused before any file is opened
//     (BrowserRelativePath).
//  2. No component may begin with "." — hidden entries and the .ai-novel
//     storage directory belong to the server, never to a browser write.
//  3. Storage-reserved names (project.json, project.db and its sidecars) are
//     refused even at the root, so the file surface can never edit the
//     manifest or the author's SQLite database.
//  4. Every component is opened through an already pinned os.Root: the final
//     component must be a single-link regular file (or a real directory) with
//     no symlink, and each hop is re-verified. os.Root itself refuses an escape
//     even if an ancestor is swapped mid-walk, so a symlink cannot be used to
//     walk out of the project.
//  5. A write publishes through a temporary file in the pinned parent directory
//     and one rename, so a concurrent reader sees the old bytes or the new
//     bytes, never a truncated manuscript.
//
// None of this reuses the desktop external-file grant model: there is no
// grantId here and no client-named target outside the project root.

// BrowserRelativePath validates and returns the browser's path unchanged. The
// returned value is still relative; it is never joined onto a host path outside
// the pinned project tree.
func BrowserRelativePath(path string) (string, error) {
	if path == "" || len(path) > BrowserRelativePathMaxBytes || !utf8.ValidString(path) ||
		strings.ContainsAny(path, "\x00\\") || filepath.IsAbs(path) || !validRelative(path) {
		return "", ErrBrowserPathRefused
	}
	authorVisible := authorVisibleStoragePath(path)
	for _, part := range strings.Split(path, "/") {
		if reservedStorageName(part) || (strings.HasPrefix(part, ".") && !authorVisible) {
			return "", ErrBrowserPathRefused
		}
	}
	return path, nil
}

// authorVisibleStoragePath reports whether a project-relative path names the
// three things the shipped renderer keeps under the storage directory and reads
// back by path: the author's project skills, the project prompt templates and
// the writing-skill binding file. The storage directory itself, the author
// databases, the knowledge snapshot and every other hidden component stay out
// of the browser's reach.
func authorVisibleStoragePath(relative string) bool {
	for _, name := range []string{"skills", "prompts"} {
		prefix := StorageDirectory + "/" + name
		if relative == prefix || strings.HasPrefix(relative, prefix+"/") {
			return true
		}
	}
	return relative == StorageDirectory+"/writing-skills.json"
}

// reservedStorageName refuses the names the project storage owns. The dot rule
// above already keeps .ai-novel out of reach; this keeps the refusal true even
// if a future layout stores those files at the project root.
func reservedStorageName(part string) bool {
	return part == ManifestName || part == DatabaseName || strings.HasPrefix(part, DatabaseName+"-")
}

// withBrowserParent walks every parent component of an already validated
// browser path through pinned roots and hands the caller the pinned parent plus
// the final component. The pinned roots are released when the action returns.
func (p *Project) withBrowserParent(relative string, create bool, action func(parent *os.Root, name string) error) error {
	parts := strings.Split(relative, "/")
	current := p.root
	var opened []*os.Root
	defer func() {
		for _, root := range opened {
			_ = root.Close()
		}
	}()
	for _, part := range parts[:len(parts)-1] {
		next, err := openBrowserChild(current, part, create)
		if err != nil {
			return err
		}
		opened = append(opened, next)
		current = next
	}
	return action(current, parts[len(parts)-1])
}

// openBrowserChild pins one directory component. A missing component is created
// only for a writable operation, and it is then pinned exactly like a
// pre-existing one, so a symlink can never be substituted for a component this
// call just created.
func openBrowserChild(parent *os.Root, name string, create bool) (*os.Root, error) {
	if create {
		if _, err := parent.Lstat(name); errors.Is(err, fs.ErrNotExist) {
			if err := parent.Mkdir(name, 0700); err != nil && !errors.Is(err, fs.ErrExist) {
				return nil, err
			}
		} else if err != nil {
			return nil, err
		}
	}
	child, _, err := openChild(parent, name)
	return child, err
}

// ReadBrowserFile reads one browser-visible file through the pinned project
// tree. limit bounds the returned bytes; a larger file is ErrTooLarge rather
// than a truncated read.
func (p *Project) ReadBrowserFile(ctx context.Context, relative string, limit int64) ([]byte, error) {
	if p == nil || ctx == nil || limit < 0 || limit > BrowserFileMaxBytes {
		return nil, ErrInvalidPath
	}
	path, err := BrowserRelativePath(relative)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.library.mu.Lock()
	defer p.library.mu.Unlock()
	if err := p.checkIdentityLocked(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var contents []byte
	if err := p.withBrowserParent(path, false, func(parent *os.Root, name string) error {
		data, _, readErr := readBounded(parent, name, limit)
		contents = data
		return readErr
	}); err != nil {
		return nil, err
	}
	if err := p.checkIdentityLocked(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return contents, nil
}

// WriteBrowserFile atomically replaces (or creates) one browser-visible file.
// Missing parent directories are created, matching the desktop fs:write-file
// which made the parent before it wrote. The replacement is published with a
// single rename and then read back through the same pinned parent, so success
// means the project really holds these bytes.
func (p *Project) WriteBrowserFile(ctx context.Context, relative string, data []byte) error {
	if p == nil || ctx == nil {
		return ErrInvalidPath
	}
	if int64(len(data)) > BrowserFileMaxBytes {
		return ErrTooLarge
	}
	path, err := BrowserRelativePath(relative)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.library.mu.Lock()
	defer p.library.mu.Unlock()
	if err := p.checkIdentityLocked(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := p.withBrowserParent(path, true, func(parent *os.Root, name string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return replaceBrowserFile(parent, name, data)
	}); err != nil {
		return err
	}
	if err := p.checkIdentityLocked(); err != nil {
		return errors.Join(ErrBrowserCommitUnknown, err)
	}
	return nil
}

// replaceBrowserFile writes data to a fresh temporary file in the pinned parent
// and renames it over the final name. The temporary name starts with a dot, so
// it is invisible to the browser tree if the process dies before the rename.
func replaceBrowserFile(parent *os.Root, name string, data []byte) error {
	temp, err := browserTempName()
	if err != nil {
		return err
	}
	file, err := parent.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	discard := true
	defer func() {
		if discard {
			_ = parent.Remove(temp)
		}
	}()
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	written, err := parent.Lstat(temp)
	if err != nil || !written.Mode().IsRegular() || written.Mode()&os.ModeSymlink != 0 || written.Mode().Perm()&0077 != 0 {
		return ErrUnsafeFile
	}
	if err := parent.Rename(temp, name); err != nil {
		return err
	}
	discard = false
	contents, _, err := readBounded(parent, name, int64(len(data)))
	if err != nil || !bytes.Equal(contents, data) {
		return ErrBrowserCommitUnknown
	}
	return nil
}

// browserTempName returns a name that cannot collide with an author file and
// stays inside the browser contract's reserved (dot prefixed) namespace.
func browserTempName() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return ".write-" + hex.EncodeToString(raw[:]) + ".tmp", nil
}

// BrowserPathExists reports whether the browser path is a readable regular file
// or a real directory. A missing parent is a false answer, not an error: the
// desktop fs:check-exists asked the same question about a path that need not
// exist yet. A symlink or special file is refused instead of reported present,
// because the browser could neither read nor write it.
func (p *Project) BrowserPathExists(ctx context.Context, relative string) (bool, error) {
	if p == nil || ctx == nil {
		return false, ErrInvalidPath
	}
	path, err := BrowserRelativePath(relative)
	if err != nil {
		return false, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.library.mu.Lock()
	defer p.library.mu.Unlock()
	if err := p.checkIdentityLocked(); err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	present := false
	if err := p.withBrowserParent(path, false, func(parent *os.Root, name string) error {
		info, err := parent.Lstat(name)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if directory(info) || (info.Mode().IsRegular() && singleLink(info)) {
			present = true
			return nil
		}
		return ErrUnsafeFile
	}); err != nil {
		return false, err
	}
	if err := p.checkIdentityLocked(); err != nil {
		return false, err
	}
	return present, nil
}

// MakeBrowserDirectory creates one browser-visible directory and any missing
// parents, exactly like the desktop fs:mkdir which made the whole chain. Every
// created hop is pinned and verified afterwards, so a symlink planted as a
// component is refused rather than followed.
func (p *Project) MakeBrowserDirectory(ctx context.Context, relative string) error {
	if p == nil || ctx == nil {
		return ErrInvalidPath
	}
	path, err := BrowserRelativePath(relative)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.library.mu.Lock()
	defer p.library.mu.Unlock()
	if err := p.checkIdentityLocked(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := p.withBrowserParent(path, true, func(parent *os.Root, name string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if info, err := parent.Lstat(name); err == nil {
			if directory(info) {
				return nil
			}
			return ErrUnsafeDirectory
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err := parent.Mkdir(name, 0700); err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
		child, _, err := openChild(parent, name)
		if err != nil {
			return err
		}
		return child.Close()
	}); err != nil {
		return err
	}
	return p.checkIdentityLocked()
}
