package projectfs

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Library is opened only from trusted configuration, never request input.
// The descriptor is private: callers cannot obtain write-capable os.Root methods.
type Library struct {
	mu sync.Mutex
	root *os.Root
	path string
	info fs.FileInfo
	closed bool
}

// Project pins the physical project/storage directories and original manifest.
// OpenProject grants filesystem access, NOT SQLite/data admission: the caller
// must separately verify a private owned SnapshotDatabase result.
type Project struct {
	mu sync.Mutex
	library *Library
	root *os.Root
	storage *os.Root
	path string
	name string
	info fs.FileInfo
	storageInfo fs.FileInfo
	manifestInfo fs.FileInfo
	manifestHash [32]byte
	manifest Manifest
	closed bool
}

func canonicalAbsolute(path string) bool {
	return path != "" && !strings.ContainsAny(path, "\x00\\") && filepath.IsAbs(path) && filepath.Clean(path) == path
}

func validRelative(path string) bool {
	if path == "" || strings.ContainsAny(path, "\x00\\") || filepath.IsAbs(path) || !fs.ValidPath(path) || path == "." {
		return false
	}
	return filepath.Clean(path) == path
}

func directory(info fs.FileInfo) bool {
	return info != nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0
}

// openChild pins one directory at a time; paths are never reopened as host paths.
// The before/opened/after identities reject observed swaps and symlinks. os.Root
// enforces escape confinement even if a swap races these checks. This is not an
// atomic no-symlink proof against an actor swapping an ancestor away and back;
// filesystem ownership/exclusion of hostile local writers is still required.
func openChild(parent *os.Root, name string) (*os.Root, fs.FileInfo, error) {
	before, err := parent.Lstat(name)
	if err != nil {
		return nil, nil, err
	}
	if !directory(before) {
		return nil, nil, ErrUnsafeDirectory
	}
	child, err := parent.OpenRoot(name)
	if err != nil {
		return nil, nil, err
	}
	opened, openErr := child.Stat(".")
	after, afterErr := parent.Lstat(name)
	if openErr != nil || afterErr != nil || !directory(opened) || !directory(after) || !os.SameFile(before, opened) || !os.SameFile(before, after) {
		child.Close()
		return nil, nil, ErrChanged
	}
	return child, opened, nil
}

// openPhysicalRoot starts at / and rejects symlink ancestors in trusted root
// configuration. It does not use EvalSymlinks or silently normalize input.
func openPhysicalRoot(path string) (*os.Root, fs.FileInfo, error) {
	if !canonicalAbsolute(path) {
		return nil, nil, ErrInvalidPath
	}
	current, err := os.OpenRoot("/")
	if err != nil {
		return nil, nil, err
	}
	if path == "/" {
		info, err := current.Stat(".")
		if err != nil {
			current.Close()
			return nil, nil, err
		}
		return current, info, nil
	}
	var info fs.FileInfo
	for _, part := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		next, nextInfo, err := openChild(current, part)
		current.Close()
		if err != nil {
			return nil, nil, err
		}
		current, info = next, nextInfo
	}
	return current, info, nil
}

func OpenLibrary(path string) (*Library, error) {
	if !platformSupported() {
		return nil, ErrUnsupported
	}
	root, info, err := openPhysicalRoot(path)
	if err != nil {
		return nil, err
	}
	return &Library{root: root, path: path, info: info}, nil
}

func (l *Library) Path() string { return l.path }

func (l *Library) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	l.closed = true
	return l.root.Close()
}

func (l *Library) checkLocked() error {
	if l.closed {
		return ErrClosed
	}
	root, info, err := openPhysicalRoot(l.path)
	if err != nil {
		return err
	}
	root.Close()
	if !os.SameFile(l.info, info) {
		return ErrChanged
	}
	return nil
}

func (l *Library) OpenProject(path string) (*Project, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.checkLocked(); err != nil {
		return nil, err
	}
	if !canonicalAbsolute(path) || filepath.Dir(path) != l.path || path == l.path {
		return nil, ErrInvalidPath
	}
	name := filepath.Base(path)
	root, info, err := openChild(l.root, name)
	if err != nil {
		return nil, err
	}
	if _, err := root.Lstat(".vela"); err == nil {
		root.Close()
		return nil, ErrMigrationRequired
	} else if !errors.Is(err, fs.ErrNotExist) {
		root.Close()
		return nil, err
	}
	storage, storageInfo, err := openChild(root, StorageDirectory)
	if err != nil {
		root.Close()
		return nil, err
	}
	p := &Project{library: l, root: root, storage: storage, path: path, name: name, info: info, storageInfo: storageInfo}
	manifest, manifestInfo, hash, err := p.readManifest()
	if err == nil {
		p.manifest, p.manifestInfo, p.manifestHash = manifest, manifestInfo, hash
		err = p.checkMigration()
	}
	if err == nil {
		var file *os.File
		file, _, err = openRegular(storage, DatabaseName)
		if file != nil {
			file.Close()
		}
	}
	if err == nil {
		err = p.checkIdentityLocked()
	}
	if err != nil {
		storage.Close()
		root.Close()
		return nil, err
	}
	return p, nil
}

func (p *Project) Path() string { return p.path }
func (p *Project) ProjectID() string { return p.manifest.ProjectID }
func (p *Project) Manifest() Manifest { return p.manifest }

func (p *Project) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	p.closed = true
	return errors.Join(p.storage.Close(), p.root.Close())
}

// openRegular only accepts a single final component in an already pinned root.
// It uses Linux O_NOFOLLOW|O_NONBLOCK and checks the descriptor rather than
// trusting a pathname's earlier metadata. All readable files require nlink=1.
func openRegular(root *os.Root, name string) (*os.File, fs.FileInfo, error) {
	before, err := root.Lstat(name)
	if err != nil {
		return nil, nil, err
	}
	if !before.Mode().IsRegular() || !singleLink(before) {
		return nil, nil, ErrUnsafeFile
	}
	file, err := root.OpenFile(name, readFlags(), 0)
	if err != nil {
		return nil, nil, err
	}
	opened, statErr := file.Stat()
	after, afterErr := root.Lstat(name)
	if statErr != nil || afterErr != nil || !opened.Mode().IsRegular() || !singleLink(opened) || !after.Mode().IsRegular() || !singleLink(after) || !os.SameFile(before, opened) || !os.SameFile(opened, after) {
		file.Close()
		return nil, nil, ErrChanged
	}
	return file, opened, nil
}

func sameVersion(a, b fs.FileInfo) bool {
	return os.SameFile(a, b) && a.Size() == b.Size() && a.Mode() == b.Mode() && a.ModTime().Equal(b.ModTime()) && sameChangeTime(a, b) && singleLink(a) && singleLink(b)
}

func readBounded(root *os.Root, name string, limit int64) ([]byte, fs.FileInfo, error) {
	file, before, err := openRegular(root, name)
	if err != nil {
		return nil, nil, err
	}
	defer file.Close()
	if limit < 0 || before.Size() > limit {
		return nil, nil, ErrTooLarge
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, nil, err
	}
	if int64(len(data)) > limit {
		return nil, nil, ErrTooLarge
	}
	after, statErr := file.Stat()
	named, nameErr := root.Lstat(name)
	if statErr != nil || nameErr != nil || !sameVersion(before, after) || !sameVersion(after, named) || int64(len(data)) != after.Size() {
		return nil, nil, ErrChanged
	}
	return data, after, nil
}

func (p *Project) readManifest() (Manifest, fs.FileInfo, [32]byte, error) {
	var hash [32]byte
	data, info, err := readBounded(p.storage, ManifestName, manifestLimit)
	if err != nil {
		return Manifest{}, nil, hash, err
	}
	manifest, err := ParseManifest(data)
	if err != nil {
		return Manifest{}, nil, hash, err
	}
	return manifest, info, sha256.Sum256(data), nil
}

func (p *Project) checkMigration() error {
	// Lstat detects dangling symlinks too; any .vela presence blocks admission.
	if _, err := p.root.Lstat(".vela"); err == nil {
		return ErrMigrationRequired
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if _, err := p.root.Lstat(".ai-novel-migration"); errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return ErrRecoveryRequired
	}
	control, _, err := openChild(p.root, ".ai-novel-migration")
	if err != nil {
		return ErrRecoveryRequired
	}
	defer control.Close()
	if _, err := control.Lstat("journal.json"); errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return ErrRecoveryRequired
	}
	data, _, err := readBounded(control, "journal.json", journalLimit)
	if err != nil {
		return ErrRecoveryRequired
	}
	obj, err := strictObject(data)
	if err != nil || !numberOne(obj["version"]) {
		return ErrRecoveryRequired
	}
	phase, phaseOK := stringField(obj, "phase")
	id, idOK := stringField(obj, "projectId")
	if !phaseOK || phase != "switched" || !idOK || id != p.manifest.ProjectID {
		return ErrRecoveryRequired
	}
	return nil
}

// checkIdentityLocked requires the library lock (and, after construction, p.mu).
func (p *Project) checkIdentityLocked() error {
	if p.closed || p.library.closed {
		return ErrClosed
	}
	if err := p.library.checkLocked(); err != nil {
		return err
	}
	root, info, err := openChild(p.library.root, p.name)
	if err != nil {
		return err
	}
	defer root.Close()
	if !os.SameFile(info, p.info) {
		return ErrChanged
	}
	storage, info, err := openChild(root, StorageDirectory)
	if err != nil {
		return err
	}
	defer storage.Close()
	if !os.SameFile(info, p.storageInfo) {
		return ErrChanged
	}
	manifest, manifestInfo, hash, err := p.readManifest()
	if err != nil {
		return err
	}
	if !os.SameFile(manifestInfo, p.manifestInfo) || hash != p.manifestHash || manifest != p.manifest {
		return ErrManifestChanged
	}
	return p.checkMigration()
}

// OpenFileRead returns a caller-owned, read-only descriptor relative to canonical
// storage. No absolute paths, empty/dot/traversal components, symlinks, special
// files or hardlinks are accepted. SQLite and sidecars are snapshot-only. This
// is not a frozen content snapshot; callers must bound their descriptor reads.
func (p *Project) OpenFileRead(path string) (*os.File, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.library.mu.Lock()
	defer p.library.mu.Unlock()
	if err := p.checkIdentityLocked(); err != nil {
		return nil, err
	}
	if !validRelative(path) {
		return nil, ErrInvalidPath
	}
	for _, part := range strings.Split(path, "/") {
		if part == DatabaseName || strings.HasPrefix(part, DatabaseName+"-") {
			return nil, ErrDatabaseSnapshotRequired
		}
	}
	parts := strings.Split(path, "/")
	current := p.storage
	var opened []*os.Root
	defer func() {
		for _, root := range opened {
			root.Close()
		}
	}()
	for _, part := range parts[:len(parts)-1] {
		next, _, err := openChild(current, part)
		if err != nil {
			return nil, err
		}
		opened = append(opened, next)
		current = next
	}
	file, _, err := openRegular(current, parts[len(parts)-1])
	if err != nil {
		return nil, err
	}
	if err := p.checkIdentityLocked(); err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}

// WriteStorageFile atomically replaces (or creates) one storage-relative file
// through the pinned storage tree, bypassing the browser-file allowlist. It is
// for the lane's own fixed-name documents - the architecture checkpoint - not
// for renderer-chosen paths, which must go through WriteBrowserFile.
func (p *Project) WriteStorageFile(ctx context.Context, path string, data []byte) error {
	if p == nil || ctx == nil {
		return ErrInvalidPath
	}
	if !validRelative(path) {
		return ErrInvalidPath
	}
	for _, part := range strings.Split(path, "/") {
		if part == DatabaseName || strings.HasPrefix(part, DatabaseName+"-") {
			return ErrDatabaseSnapshotRequired
		}
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
	parts := strings.Split(path, "/")
	current := p.storage
	var opened []*os.Root
	defer func() {
		for _, root := range opened {
			root.Close()
		}
	}()
	for _, part := range parts[:len(parts)-1] {
		next, _, err := openChild(current, part)
		if err != nil {
			return err
		}
		opened = append(opened, next)
		current = next
	}
	if err := replaceBrowserFile(current, parts[len(parts)-1], data); err != nil {
		return err
	}
	return p.checkIdentityLocked()
}
