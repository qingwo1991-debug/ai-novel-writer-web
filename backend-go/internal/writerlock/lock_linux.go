//go:build linux

// Package writerlock provides an advisory, process-wide writer exclusion lock.
// The configured root must be an existing canonical absolute physical directory
// in an administrator-controlled tree, never a request-controlled path. A private
// 0700 root is recommended; an owned root with no group/world write bits (for
// example 0755) is accepted. Root-owned sticky temporary ancestors are allowed.
//
// This is not a defense against a hostile administrator, same-UID writer, mount
// changes, or programs that ignore flock. Do not rename the configured tree or
// remove/replace the lock entry while a server is running. Anchors are checked
// before and after acquisition and on explicit Verify calls, not continuously.
// The lock file is persistent: neither failed acquisition nor Close unlinks,
// truncates, repairs, or changes permissions on an existing entry.
package writerlock

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

var (
	// ErrLocked means another open file description holds the writer lock.
	ErrLocked = errors.New("writerlock_busy")
	// ErrUnsafe means a path, file, or physical anchor failed validation.
	ErrUnsafe = errors.New("writerlock_unsafe")
	// ErrIO reports an operating-system failure without exposing configured paths.
	ErrIO = errors.New("writerlock_io")
	// ErrRetained means an unresolved reader cleanup requires this process to
	// keep the advisory exclusion until it exits.
	ErrRetained = errors.New("writerlock_retained")
	// ErrUnsupported is returned on platforms without this implementation.
	ErrUnsupported = errors.New("writerlock_unsupported")
)

const (
	lockName       = ".novel-writer.lock"
	legacyLockName = ".native-web.gateway.lock"
)

// Policies are private and bound to fixed entry names, never request input.
// The zero policy preserves Acquire's original exact-0600 requirements.
type lockPolicy uint8

const (
	privateLockPolicy lockPolicy = iota
	legacyLockPolicy
)

func (policy lockPolicy) name() string {
	if policy == legacyLockPolicy {
		return legacyLockName
	}
	return lockName
}

// Lock holds a Linux flock and its physical directory anchors. It must not be
// copied. Close is idempotent and safe for concurrent calls. Every independent
// Acquire, including in the same process, opens its own file description.
type Lock struct {
	mu       sync.Mutex
	file     *os.File
	anchor   *directoryAnchor
	policy   lockPolicy
	identity fs.FileInfo
	closeErr error
	retained bool
}

// A retained lock must remain reachable even if its owner aborts startup or
// discards the last local reference. Otherwise os.File's finalizer could close
// its descriptor and silently release flock before process exit.
var retainedLocks sync.Map // map[*Lock]struct{}

// Acquire exclusively locks the fixed .novel-writer.lock entry without waiting.
// It creates only a missing lock entry, with O_EXCL and mode 0600; it never
// creates the configured directory. Existing entries must be empty, regular,
// exactly 0600, current-effective-UID-owned and single-link. Valid existing
// entries are reused because flock ownership is tied to their inode. A valid
// same-UID precreation cannot be distinguished from a previous legitimate run;
// protecting this namespace is the administrator's responsibility.
//
// A restrictive umask that removes owner read/write bits causes fail-closed
// rejection, not chmod or deletion. All returned errors are symbolic and contain
// no input paths or raw OS error strings.
func Acquire(directory string) (*Lock, error) {
	return acquire(directory, privateLockPolicy)
}

// AcquireLegacy joins the util-linux flock used by docker/web-entrypoint.sh on
// the fixed .native-web.gateway.lock entry. Existing entries may be exactly 0600
// or 0644 (the historical shell default umask); all other Acquire safety rules
// apply. Missing entries are created exactly as for Acquire, with mode 0600.
// Existing entries are never chmodded, truncated, unlinked, or recreated.
// This coordinates cooperating native web writers only, not desktop writers
// or any other process that ignores flock.
func AcquireLegacy(directory string) (*Lock, error) {
	return acquire(directory, legacyLockPolicy)
}

func acquire(directory string, policy lockPolicy) (*Lock, error) {
	anchor, err := openDirectory(directory)
	if err != nil {
		return nil, err
	}
	lock, err := acquirePinnedPolicy(anchor, policy)
	if err != nil {
		anchor.close()
		return nil, err
	}
	return lock, nil
}

func acquirePinned(anchor *directoryAnchor) (*Lock, error) {
	return acquirePinnedPolicy(anchor, privateLockPolicy)
}

func acquirePinnedPolicy(anchor *directoryAnchor, policy lockPolicy) (*Lock, error) {
	if !anchor.valid() {
		return nil, ErrUnsafe
	}
	file, identity, err := openLockPolicy(anchor, policy)
	if err != nil {
		return nil, err
	}
	keep := false
	defer func() {
		if !keep {
			// Closing the only descriptor also releases any acquired flock.
			file.Close()
		}
	}()
	if !anchor.valid() || !sameEntryPolicy(anchor, file, identity, policy) {
		return nil, ErrUnsafe
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, ErrLocked
		}
		return nil, ErrIO
	}
	if !anchor.valid() || !sameEntryPolicy(anchor, file, identity, policy) {
		return nil, ErrUnsafe
	}
	keep = true
	return &Lock{file: file, anchor: anchor, policy: policy, identity: identity}, nil
}

// Verify rechecks every pinned parent and the held lock entry's identity,
// permissions, ownership, link count, size, ctime and mtime. It is serialized
// with Close. Nil, zero and closed locks return the stable ErrUnsafe sentinel.
// Verification is an explicit point-in-time check, not continuous monitoring
// or a defense against hostile same-UID writers or administrators.
func (lock *Lock) Verify() error {
	if lock == nil {
		return ErrUnsafe
	}
	lock.mu.Lock()
	defer lock.mu.Unlock()
	if lock.file == nil || !lock.anchor.valid() || !sameEntryPolicy(lock.anchor, lock.file, lock.identity, lock.policy) {
		return ErrUnsafe
	}
	return nil
}

// Close releases flock and closes descriptors only. The persistent lock entry
// is never removed, even if it or an ancestor was replaced after acquisition.
func (lock *Lock) Close() error {
	if lock == nil {
		return nil
	}
	lock.mu.Lock()
	defer lock.mu.Unlock()
	if lock.retained {
		return ErrRetained
	}
	if lock.file == nil {
		return lock.closeErr
	}
	if err := syscall.Flock(int(lock.file.Fd()), syscall.LOCK_UN); err != nil {
		lock.closeErr = ErrIO
	}
	if err := lock.file.Close(); err != nil {
		lock.closeErr = ErrIO
	}
	if err := lock.anchor.close(); err != nil {
		lock.closeErr = ErrIO
	}
	lock.file = nil
	lock.anchor = nil
	return lock.closeErr
}

// Retain permanently refuses Close after cleanup of a borrowed reader has
// failed. The owner may still inspect the held lock, but must restart the
// process before another writer can acquire it. The caller must own the live
// lock through the readback; it must not race Retain against Close. It is safe
// to call repeatedly.
func (lock *Lock) Retain() error {
	if lock == nil {
		return ErrUnsafe
	}
	lock.mu.Lock()
	defer lock.mu.Unlock()
	if lock.file == nil {
		return ErrUnsafe
	}
	lock.retained = true
	retainedLocks.Store(lock, struct{}{})
	return nil
}

type directoryPart struct {
	root     *os.Root
	name     string
	identity fs.FileInfo
}

// Every parent is retained, rather than validating the path once and dropping
// its ancestors. Rechecking the complete chain detects detached/replaced roots.
type directoryAnchor struct {
	parts []directoryPart
	dir   *os.File
}

func openDirectory(directory string) (*directoryAnchor, error) {
	if directory == "" || directory == "/" || !filepath.IsAbs(directory) || filepath.Clean(directory) != directory || strings.ContainsAny(directory, "\x00\\") {
		return nil, ErrUnsafe
	}
	root, err := os.OpenRoot("/")
	if err != nil {
		return nil, ErrIO
	}
	anchor := &directoryAnchor{}
	keep := false
	defer func() {
		if !keep {
			anchor.close()
		}
	}()
	identity, err := root.Stat(".")
	anchor.parts = append(anchor.parts, directoryPart{root: root, identity: identity})
	if err != nil || !safeDirectory(identity, false) {
		return nil, ErrUnsafe
	}
	components := strings.Split(strings.TrimPrefix(directory, "/"), "/")
	for i, name := range components {
		final := i == len(components)-1
		before, err := root.Lstat(name)
		if err != nil || !safeDirectory(before, final) {
			return nil, ErrUnsafe
		}
		// os.Root confines resolution, but can follow symlinks. Comparing both
		// sides of every open, then rechecking every pinned parent, rejects
		// symlink substitution or directory replacement during admission.
		next, err := root.OpenRoot(name)
		if err != nil {
			return nil, ErrUnsafe
		}
		opened, statErr := next.Stat(".")
		after, entryErr := root.Lstat(name)
		anchor.parts = append(anchor.parts, directoryPart{root: next, name: name, identity: before})
		if statErr != nil || entryErr != nil || !sameDirectory(before, opened, final) || !sameDirectory(before, after, final) {
			return nil, ErrUnsafe
		}
		root = next
	}
	// The file descriptor is used only for real syscall.Openat below; using
	// Root.OpenFile(name, O_NOFOLLOW) would still follow confined symlinks.
	anchor.dir, err = root.OpenFile(".", os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, ErrIO
	}
	if !anchor.valid() {
		return nil, ErrUnsafe
	}
	keep = true
	return anchor, nil
}

func safeDirectory(info fs.FileInfo, final bool) bool {
	if info == nil || !info.IsDir() || info.Mode()&(os.ModeSymlink|os.ModeSetuid|os.ModeSetgid) != 0 {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || (stat.Uid != uint32(os.Geteuid()) && stat.Uid != 0) {
		return false
	}
	if final {
		return stat.Uid == uint32(os.Geteuid()) && info.Mode().Perm()&0o022 == 0
	}
	return info.Mode().Perm()&0o022 == 0 || (stat.Uid == 0 && info.Mode()&os.ModeSticky != 0)
}

func sameDirectory(a, b fs.FileInfo, final bool) bool {
	return safeDirectory(a, final) && safeDirectory(b, final) && os.SameFile(a, b)
}

func (anchor *directoryAnchor) valid() bool {
	if anchor == nil || len(anchor.parts) == 0 || anchor.dir == nil {
		return false
	}
	for i, part := range anchor.parts {
		final := i == len(anchor.parts)-1
		opened, err := part.root.Stat(".")
		if err != nil || !sameDirectory(part.identity, opened, final) {
			return false
		}
		if i > 0 {
			entry, err := anchor.parts[i-1].root.Lstat(part.name)
			if err != nil || !sameDirectory(part.identity, entry, final) {
				return false
			}
		}
	}
	opened, err := anchor.dir.Stat()
	return err == nil && sameDirectory(anchor.parts[len(anchor.parts)-1].identity, opened, true)
}

func (anchor *directoryAnchor) close() error {
	var result error
	if anchor.dir != nil {
		if err := anchor.dir.Close(); err != nil {
			result = ErrIO
		}
		anchor.dir = nil
	}
	for i := len(anchor.parts) - 1; i >= 0; i-- {
		if err := anchor.parts[i].root.Close(); err != nil {
			result = ErrIO
		}
	}
	anchor.parts = nil
	return result
}

func safeLock(info fs.FileInfo) bool {
	return safeLockPolicy(info, privateLockPolicy)
}

func safeLockPolicy(info fs.FileInfo, policy lockPolicy) bool {
	if info == nil || !info.Mode().IsRegular() || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 || info.Size() != 0 {
		return false
	}
	mode := info.Mode().Perm()
	if mode != 0o600 && (policy != legacyLockPolicy || mode != 0o644) {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid()) && stat.Nlink == 1
}

func sameLock(a, b fs.FileInfo) bool {
	return sameLockPolicy(a, b, privateLockPolicy)
}

func sameLockPolicy(a, b fs.FileInfo, policy lockPolicy) bool {
	if !safeLockPolicy(a, policy) || !safeLockPolicy(b, policy) || !os.SameFile(a, b) || a.Mode() != b.Mode() {
		return false
	}
	x := a.Sys().(*syscall.Stat_t)
	y := b.Sys().(*syscall.Stat_t)
	return x.Ctim == y.Ctim && x.Mtim == y.Mtim
}

func openLock(anchor *directoryAnchor) (*os.File, fs.FileInfo, error) {
	return openLockPolicy(anchor, privateLockPolicy)
}

func openLockPolicy(anchor *directoryAnchor, policy lockPolicy) (*os.File, fs.FileInfo, error) {
	root := anchor.parts[len(anchor.parts)-1].root
	name := policy.name()
	before, err := root.Lstat(name)
	flags := syscall.O_RDWR | syscall.O_NOFOLLOW | syscall.O_NONBLOCK | syscall.O_CLOEXEC
	fd := -1
	created := false
	if errors.Is(err, fs.ErrNotExist) {
		fd, err = syscall.Openat(int(anchor.dir.Fd()), name, flags|syscall.O_CREAT|syscall.O_EXCL, 0o600)
		if errors.Is(err, syscall.EEXIST) {
			// A concurrent creator won. Inspect it, but never retry creation
			// after observing an existing entry, including if it disappears.
			before, err = root.Lstat(name)
		} else if err != nil {
			return nil, nil, ErrIO
		} else {
			created = true
		}
	}
	if fd < 0 {
		if err != nil || !safeLockPolicy(before, policy) {
			return nil, nil, ErrUnsafe
		}
		fd, err = syscall.Openat(int(anchor.dir.Fd()), name, flags, 0)
		if err != nil {
			return nil, nil, ErrUnsafe
		}
	}
	file := os.NewFile(uintptr(fd), name)
	opened, err := file.Stat()
	entry, entryErr := root.Lstat(name)
	if err != nil || entryErr != nil || !safeLockPolicy(opened, policy) || (created && !safeLock(opened)) || !sameLockPolicy(opened, entry, policy) || (before != nil && !sameLockPolicy(before, opened, policy)) {
		file.Close()
		return nil, nil, ErrUnsafe
	}
	return file, opened, nil
}

func sameEntry(anchor *directoryAnchor, file *os.File, identity fs.FileInfo) bool {
	return sameEntryPolicy(anchor, file, identity, privateLockPolicy)
}

func sameEntryPolicy(anchor *directoryAnchor, file *os.File, identity fs.FileInfo, policy lockPolicy) bool {
	opened, err := file.Stat()
	entry, entryErr := anchor.parts[len(anchor.parts)-1].root.Lstat(policy.name())
	return err == nil && entryErr == nil && sameLockPolicy(identity, opened, policy) && sameLockPolicy(identity, entry, policy)
}
