//go:build linux

package secrets

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// LoadExistingKey reads a private key without creating directories or replacing
// anything. Paths must be canonical absolute physical paths from trusted admin
// configuration, never request input. The key must be current-UID-owned, exactly
// 0600, regular, 32 bytes and have exactly one hard link.
func LoadExistingKey(path string) ([]byte, error) {
	return loadKey(path, false)
}

// LoadOrCreateKey preserves every existing key, including invalid ones (which
// cause an error). Missing directories are created privately. Publication uses
// an exclusive temporary file, file fsync, exclusive hard-link publication,
// removal of the temporary link, and directory fsync. Existing destinations are
// never renamed over, truncated, chmodded, or silently regenerated.
//
// Directory descriptors pin every ancestor; raw destination paths are never
// reused after admission. This is for trusted administrator-owned trees, not a
// defense against administrators/mount changes or hostile same-UID writers.
func LoadOrCreateKey(path string) ([]byte, error) {
	return loadKey(path, true)
}

func loadKey(path string, create bool) ([]byte, error) {
	root, name, err := keyParent(path, create)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	// Serialize this implementation's link/unlink publication window. This
	// prevents readers rejecting a just-published key's transient second link.
	// flock is on the pinned directory, not on a second configurable lock path.
	dir, err := root.OpenFile(".", os.O_RDONLY | syscall.O_DIRECTORY | syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	if err := syscall.Flock(int(dir.Fd()), syscall.LOCK_EX); err != nil {
		return nil, err
	}
	defer syscall.Flock(int(dir.Fd()), syscall.LOCK_UN)
	key, err := readKeyAt(root, dir, name)
	if err == nil || !create || !errors.Is(err, fs.ErrNotExist) {
		return key, err
	}
	if err := createKeyAt(root, dir, name); err != nil {
		return nil, err
	}
	return readKeyAt(root, dir, name)
}

func invalidKey(reason string) error {
	return fmt.Errorf("%w: %s", ErrKeyInvalid, reason)
}

func keyParent(path string, create bool) (*os.Root, string, error) {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsAny(path, "\x00\\") || path == "/" {
		return nil, "", invalidKey("path must be canonical and absolute")
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	current, err := os.OpenRoot("/")
	if err != nil {
		return nil, "", err
	}
	for _, part := range parts[:len(parts)-1] {
		before, err := current.Lstat(part)
		if errors.Is(err, fs.ErrNotExist) && create {
			if err = current.Mkdir(part, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
				current.Close()
				return nil, "", err
			}
			if err = syncRoot(current); err != nil {
				current.Close()
				return nil, "", err
			}
			before, err = current.Lstat(part)
		}
		if err != nil {
			current.Close()
			return nil, "", err
		}
		if !safeDirectory(before, false) {
			current.Close()
			return nil, "", invalidKey("unsafe ancestor")
		}
		next, err := current.OpenRoot(part)
		if err != nil {
			current.Close()
			return nil, "", err
		}
		opened, openedErr := next.Stat(".")
		after, afterErr := current.Lstat(part)
		current.Close()
		if openedErr != nil || afterErr != nil || !safeDirectory(opened, false) || !safeDirectory(after, false) || !os.SameFile(before, opened) || !os.SameFile(before, after) {
			next.Close()
			return nil, "", invalidKey("ancestor changed while opening")
		}
		current = next
	}
	parent, err := current.Stat(".")
	if err != nil || !safeDirectory(parent, true) {
		current.Close()
		return nil, "", invalidKey("key directory must be owned and not writable by others")
	}
	return current, parts[len(parts)-1], nil
}

func safeDirectory(info fs.FileInfo, final bool) bool {
	if info == nil || !info.IsDir() || info.Mode() & os.ModeSymlink != 0 {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || (stat.Uid != uint32(os.Getuid()) && stat.Uid != 0) {
		return false
	}
	if final {
		return stat.Uid == uint32(os.Getuid()) && info.Mode().Perm() & 0o022 == 0
	}
	// Root-owned sticky temporary directories may contain a private owned child;
	// they may not themselves be the key's containing directory.
	return info.Mode().Perm() & 0o022 == 0 || (stat.Uid == 0 && info.Mode() & os.ModeSticky != 0)
}

func privateKey(info fs.FileInfo) bool {
	if info == nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Mode() & (os.ModeSetuid | os.ModeSetgid | os.ModeSticky) != 0 || info.Size() != KeyBytes {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Getuid()) && stat.Nlink == 1
}

func sameKey(a, b fs.FileInfo) bool {
	if !privateKey(a) || !privateKey(b) || !os.SameFile(a, b) {
		return false
	}
	x := a.Sys().(*syscall.Stat_t)
	y := b.Sys().(*syscall.Stat_t)
	return x.Ctim == y.Ctim && x.Mtim == y.Mtim
}

func readKeyAt(root *os.Root, dir *os.File, name string) ([]byte, error) {
	before, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !privateKey(before) {
		return nil, invalidKey("key metadata")
	}
	// Root.OpenFile follows confined symlinks, including when O_NOFOLLOW is
	// supplied. Openat on the pinned parent descriptor enforces Linux's actual
	// final-component nofollow semantics. name is one admitted basename.
	fd, err := syscall.Openat(int(dir.Fd()), name, os.O_RDONLY | syscall.O_NOFOLLOW | syscall.O_NONBLOCK | syscall.O_CLOEXEC, 0)
	if err != nil {
		// An observed existing destination must never become a create signal.
		return nil, invalidKey("existing key could not be opened")
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !sameKey(before, opened) {
		return nil, invalidKey("key changed while opening")
	}
	key, err := io.ReadAll(io.LimitReader(file, KeyBytes + 1))
	if err != nil {
		return nil, err
	}
	after, statErr := file.Stat()
	entry, entryErr := root.Lstat(name)
	if len(key) != KeyBytes || statErr != nil || entryErr != nil || !sameKey(opened, after) || !sameKey(opened, entry) {
		return nil, invalidKey("key changed while reading")
	}
	return key, nil
}

func syncRoot(root *os.Root) error {
	dir, err := root.OpenFile(".", os.O_RDONLY | syscall.O_DIRECTORY | syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func createKeyAt(root *os.Root, dir *os.File, name string) error {
	key := make([]byte, KeyBytes)
	if _, err := rand.Read(key); err != nil {
		return err
	}
	var suffix [16]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return err
	}
	temporary := ".anw-key-" + hex.EncodeToString(suffix[:]) + ".tmp"
	file, err := root.OpenFile(temporary, os.O_WRONLY | os.O_CREATE | os.O_EXCL | syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	defer root.Remove(temporary)
	// Set the newly owned descriptor exactly to 0600 even under restrictive umask.
	if err := file.Chmod(0o600); err != nil {
		return err
	}
	if n, err := file.Write(key); err != nil {
		return err
	} else if n != len(key) {
		return io.ErrShortWrite
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := root.Link(temporary, name); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	if err := root.Remove(temporary); err != nil {
		return err
	}
	return dir.Sync()
}
