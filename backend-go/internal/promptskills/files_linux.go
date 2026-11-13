//go:build linux

package promptskills

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

var ErrUnsafe = errors.New("PROMPT_SKILL_PATH_UNSAFE")
var ErrChanged = errors.New("PROMPT_SKILL_IDENTITY_CHANGED")
var ErrUnsupported = errors.New("PROMPT_SKILLS_PLATFORM_UNSUPPORTED")

type ownedDir struct{ file *os.File }

func (d *ownedDir) close() error { return d.file.Close() }
func fd(d *ownedDir) int         { return int(d.file.Fd()) }
func safeDir(info fs.FileInfo, final bool) bool {
	if info == nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return false
	}
	s, ok := info.Sys().(*syscall.Stat_t)
	if !ok || (s.Uid != uint32(os.Getuid()) && s.Uid != 0) {
		return false
	}
	if final {
		return s.Uid == uint32(os.Getuid()) && info.Mode().Perm()&0022 == 0
	}
	return info.Mode().Perm()&0022 == 0 || (s.Uid == 0 && info.Mode()&os.ModeSticky != 0)
}
func safeFile(info fs.FileInfo) bool {
	if info == nil || !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 {
		return false
	}
	s, ok := info.Sys().(*syscall.Stat_t)
	return ok && s.Uid == uint32(os.Getuid()) && s.Nlink == 1
}
func sameFile(a, b fs.FileInfo) bool {
	if !safeFile(a) || !safeFile(b) || !os.SameFile(a, b) {
		return false
	}
	x := a.Sys().(*syscall.Stat_t)
	y := b.Sys().(*syscall.Stat_t)
	return x.Ctim == y.Ctim && x.Mtim == y.Mtim && a.Size() == b.Size()
}
func openOwnedRoot(path string) (*ownedDir, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" || strings.ContainsAny(path, "\x00\\") {
		return nil, ErrUnsafe
	}
	rootFD, err := syscall.Open("/", syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	current := &ownedDir{os.NewFile(uintptr(rootFD), "/")}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for i, p := range parts {
		next, e := openChildDir(current, p, false, i == len(parts)-1)
		current.close()
		if e != nil {
			return nil, e
		}
		current = next
	}
	return current, nil
}
func statAt(d *ownedDir, name string) (fs.FileInfo, error) { // lstat through a pinned descriptor, never a request-controlled prefix.
	return os.Lstat(filepath.Join("/proc/self/fd", itoa(fd(d)), name))
}
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	b := []byte{}
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
func openChildDir(parent *ownedDir, name string, create, final bool) (*ownedDir, error) {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\\x00") {
		return nil, ErrUnsafe
	}
	before, err := statAt(parent, name)
	if errors.Is(err, fs.ErrNotExist) && create {
		if err = syscall.Mkdirat(fd(parent), name, 0700); err != nil && !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
		if err = parent.file.Sync(); err != nil {
			return nil, err
		}
		before, err = statAt(parent, name)
	}
	if err != nil {
		return nil, err
	}
	if !safeDir(before, final) {
		return nil, ErrUnsafe
	}
	childFD, err := syscall.Openat(fd(parent), name, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, ErrUnsafe
	}
	child := &ownedDir{os.NewFile(uintptr(childFD), name)}
	opened, e1 := child.file.Stat()
	after, e2 := statAt(parent, name)
	if e1 != nil || e2 != nil || !safeDir(opened, final) || !safeDir(after, final) || !os.SameFile(before, opened) || !os.SameFile(before, after) {
		child.close()
		return nil, ErrChanged
	}
	return child, nil
}
func readAt(parent *ownedDir, name string, limit int) ([]byte, error) {
	before, err := statAt(parent, name)
	if err != nil {
		return nil, err
	}
	if !safeFile(before) || before.Size() > int64(limit) {
		return nil, ErrUnsafe
	}
	fileFD, err := syscall.Openat(fd(parent), name, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, ErrUnsafe
	}
	file := os.NewFile(uintptr(fileFD), name)
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !sameFile(before, opened) {
		return nil, ErrChanged
	}
	bytes, err := io.ReadAll(io.LimitReader(file, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	after, e1 := file.Stat()
	entry, e2 := statAt(parent, name)
	if len(bytes) > limit || e1 != nil || e2 != nil || !sameFile(opened, after) || !sameFile(opened, entry) {
		return nil, ErrChanged
	}
	return bytes, nil
}
func readRelative(root *ownedDir, relative string, limit int) ([]byte, error) {
	parts := strings.Split(relative, "/")
	for _, p := range parts {
		if !segmentPattern.MatchString(p) || p == "." || p == ".." {
			return nil, ErrUnsafe
		}
	}
	cursor := root
	defer func() {
		if cursor != root {
			cursor.close()
		}
	}()
	for _, part := range parts[:len(parts)-1] {
		next, err := openChildDir(cursor, part, false, true)
		if err != nil {
			return nil, err
		}
		if cursor != root {
			cursor.close()
		}
		cursor = next
	}
	return readAt(cursor, parts[len(parts)-1], limit)
}
func randomName(prefix string) (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(bytes[:]), nil
}
func atomicWrite(parent *ownedDir, name string, bytes []byte, exclusive bool) error {
	before, err := statAt(parent, name)
	existed := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if existed && (!safeFile(before) || exclusive) {
		if exclusive {
			return fs.ErrExist
		}
		return ErrUnsafe
	}
	temp, err := randomName(".prompt-skill-")
	if err != nil {
		return err
	}
	fileFD, err := syscall.Openat(fd(parent), temp, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fileFD), temp)
	defer file.Close()
	defer syscall.Unlinkat(fd(parent), temp)
	if err = file.Chmod(0600); err != nil {
		return err
	}
	if n, e := file.Write(bytes); e != nil {
		return e
	} else if n != len(bytes) {
		return io.ErrShortWrite
	}
	if err = file.Sync(); err != nil {
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	current, e := statAt(parent, name)
	if existed {
		if e != nil || !sameFile(before, current) {
			return ErrChanged
		}
	} else if !errors.Is(e, fs.ErrNotExist) {
		if e == nil {
			return fs.ErrExist
		}
		return e
	}
	// Directory operations are serialized by Service. Same-UID administrators are
	// not an adversary; no writable-by-other-user directory is admitted.
	if err = syscall.Renameat(fd(parent), temp, fd(parent), name); err != nil {
		return err
	}
	return parent.file.Sync()
}
func removeFile(parent *ownedDir, name string) error {
	before, err := statAt(parent, name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !safeFile(before) {
		return ErrUnsafe
	}
	after, err := statAt(parent, name)
	if err != nil || !sameFile(before, after) {
		return ErrChanged
	}
	if err = syscall.Unlinkat(fd(parent), name); err != nil {
		return err
	}
	return parent.file.Sync()
}
func entries(d *ownedDir) ([]fs.DirEntry, error) {
	dup, err := syscall.Openat(fd(d), ".", syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(dup), ".")
	defer f.Close()
	return f.ReadDir(-1)
}
func unlinkDirectory(parent *ownedDir, name string) error {
	p, err := syscall.BytePtrFromString(name)
	if err != nil {
		return err
	}
	_, _, errno := syscall.Syscall6(syscall.SYS_UNLINKAT, uintptr(fd(parent)), uintptr(unsafe.Pointer(p)), uintptr(0x200), 0, 0, 0)
	if errno != 0 {
		return errno
	}
	return parent.file.Sync()
}
func removeTree(parent *ownedDir, name string) error {
	child, err := openChildDir(parent, name, false, true)
	if err != nil {
		return err
	}
	defer child.close()
	before, err := child.file.Stat()
	if err != nil {
		return err
	}
	items, err := entries(child)
	if err != nil {
		return err
	}
	for _, entry := range items {
		info, e := statAt(child, entry.Name())
		if e != nil {
			return e
		}
		if info.IsDir() {
			if e = removeTree(child, entry.Name()); e != nil {
				return e
			}
		} else {
			if e = removeFile(child, entry.Name()); e != nil {
				return e
			}
		}
	}
	after, e := statAt(parent, name)
	if e != nil || !os.SameFile(before, after) {
		return ErrChanged
	}
	return unlinkDirectory(parent, name)
}
