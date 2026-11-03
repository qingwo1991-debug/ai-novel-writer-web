//go:build linux

package projectfs

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"sync"
	"syscall"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/writerlock"
)

// SQLiteWriteCandidate pins a write-capable descriptor for the existing
// project.db under the legacy gateway lock. The descriptor is deliberately
// private: SQLite also opens WAL/journal sidecars by path, so handing this fd
// to a caller would not grant a confined SQLite transaction. This is only a
// source identity and filesystem safety gate for a future publisher.
type SQLiteWriteCandidate struct {
	mu       sync.Mutex
	project  *Project
	lock     *writerlock.Lock
	dataRoot string
	file     *os.File
	identity fs.FileInfo
}

// OpenSQLiteWriteCandidate does not create or change an author file. The
// caller must retain the exact legacy lock and Project until Close. DataRoot
// is trusted configuration, never a browser path.
func (p *Project) OpenSQLiteWriteCandidate(ctx context.Context, held *writerlock.Lock, dataRoot string) (*SQLiteWriteCandidate, error) {
	if ctx == nil || p == nil || p.library == nil || held == nil || dataRoot == "/" || !canonicalAbsolute(dataRoot) || !beneathDataRoot(dataRoot, p.library.path) {
		return nil, ErrInvalidPath
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := held.VerifyLegacy(dataRoot); err != nil {
		return nil, err
	}
	if err := p.VerifyNoPortableAuthority(ctx); err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.library.mu.Lock()
	defer p.library.mu.Unlock()
	if err := p.checkIdentityLocked(); err != nil {
		return nil, err
	}
	if err := p.checkWritableDirectoriesLocked(); err != nil {
		return nil, err
	}
	before, err := p.storage.Lstat(DatabaseName)
	if err != nil || !ownedWriteFile(before) {
		return nil, ErrUnsafeFile
	}
	// Use the pinned storage root as the openat parent. Root.OpenFile itself
	// may resolve a confined symlink, even when O_NOFOLLOW is requested.
	directory, err := p.storage.OpenFile(".", os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, ErrUnsafeDirectory
	}
	defer directory.Close()
	fd, err := syscall.Openat(int(directory.Fd()), DatabaseName, syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, ErrUnsafeFile
	}
	file := os.NewFile(uintptr(fd), DatabaseName)
	keep := false
	defer func() {
		if !keep {
			_ = file.Close()
		}
	}()
	opened, statErr := file.Stat()
	entry, entryErr := p.storage.Lstat(DatabaseName)
	if statErr != nil || entryErr != nil || !ownedWriteFile(opened) || !ownedWriteFile(entry) || !sameVersion(before, opened) || !sameVersion(opened, entry) {
		return nil, ErrChanged
	}
	if err := p.checkIdentityLocked(); err != nil {
		return nil, err
	}
	if err := p.checkWritableDirectoriesLocked(); err != nil {
		return nil, err
	}
	if err := held.VerifyLegacy(dataRoot); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	keep = true
	return &SQLiteWriteCandidate{project: p, lock: held, dataRoot: dataRoot, file: file, identity: opened}, nil
}

func ownedWriteFile(info fs.FileInfo) bool {
	if info == nil || !info.Mode().IsRegular() || !singleLink(info) || info.Mode().Perm()&0o022 != 0 || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid())
}

func ownedWriteDirectory(info fs.FileInfo) bool {
	if !directory(info) || info.Mode().Perm()&0o022 != 0 || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid())
}

// Caller holds Project.mu followed by Library.mu.
func (p *Project) checkWritableDirectoriesLocked() error {
	for _, root := range []*os.Root{p.library.root, p.root, p.storage} {
		info, err := root.Stat(".")
		if err != nil || !ownedWriteDirectory(info) {
			return ErrUnsafeDirectory
		}
	}
	return nil
}

// Verify proves the same descriptor, name, project and legacy lock still agree.
// It is a pre-write proof; file content changes intentionally invalidate it.
func (c *SQLiteWriteCandidate) Verify(ctx context.Context) error {
	if c == nil || ctx == nil {
		return ErrInvalidPath
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.file == nil {
		return ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := c.lock.VerifyLegacy(c.dataRoot); err != nil {
		return err
	}
	p := c.project
	p.mu.Lock()
	defer p.mu.Unlock()
	p.library.mu.Lock()
	defer p.library.mu.Unlock()
	if err := p.checkIdentityLocked(); err != nil {
		return err
	}
	if err := p.checkWritableDirectoriesLocked(); err != nil {
		return err
	}
	opened, err := c.file.Stat()
	entry, entryErr := p.storage.Lstat(DatabaseName)
	if err != nil || entryErr != nil || !ownedWriteFile(opened) || !ownedWriteFile(entry) || !sameVersion(c.identity, opened) || !sameVersion(opened, entry) {
		return ErrChanged
	}
	return c.lock.VerifyLegacy(c.dataRoot)
}

// Close closes only the pinned descriptor; it never changes a source file or
// releases the caller's gateway lock.
func (c *SQLiteWriteCandidate) Close() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.file == nil {
		return nil
	}
	err := c.file.Close()
	c.file = nil
	if err != nil {
		return errors.Join(ErrChanged, err)
	}
	return nil
}

// SourcePath is deliberately absent. A future SQLite connector must preserve
// the pinned inode and safely handle sidecars before it may write to source.
