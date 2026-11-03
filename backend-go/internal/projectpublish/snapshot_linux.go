//go:build linux

package projectpublish

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/writerlock"
)

var ErrInvalid = errors.New("PROJECT_PUBLICATION_SNAPSHOT_INVALID")
var ErrChanged = errors.New("PROJECT_PUBLICATION_SOURCE_CHANGED")
var ErrLimit = errors.New("PROJECT_PUBLICATION_SNAPSHOT_LIMIT")
var ErrPublishUnavailable = errors.New("PROJECT_PUBLICATION_EXCHANGE_UNAVAILABLE")

// Limits bound a complete private copy of .ai-novel. Zero is never unlimited.
type Limits struct {
	MaxFiles int
	MaxBytes int64
	MaxDepth int
}

type entry struct {
	path string
	mode fs.FileMode
	size int64
	hash [32]byte
}

// Snapshot is a private, complete storage-tree copy under the legacy writer
// lock. It grants no right to replace the source tree. The owner must retain
// the lock, library and snapshot until the copy has been discarded.
type Snapshot struct {
	mu          sync.Mutex
	lock        *writerlock.Lock
	dataRoot    string
	library     *projectfs.Library
	projectPath string
	projectID   string
	identity    projectfs.Identity
	storageInfo fs.FileInfo
	parent      *os.Root
	parentPath  string
	parentInfo  fs.FileInfo
	stageName   string
	stage       string
	stageInfo   fs.FileInfo
	entries     []entry
	limits      Limits
	removed     bool
	closed      bool
}

const maxTreeFiles = 100000
const maxTreeDepth = 128
const maxTreeBytes int64 = 1 << 40

func canonical(path string) bool {
	return path != "/" && filepath.IsAbs(path) && filepath.Clean(path) == path && !strings.ContainsAny(path, "\\\x00")
}

func childOf(parent, path string) bool {
	rel, err := filepath.Rel(parent, path)
	return err == nil && rel != "." && rel != ".." && !filepath.IsAbs(rel) && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func privateDirectory(path string) (fs.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0700 {
		return nil, ErrInvalid
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return nil, ErrInvalid
	}
	return info, nil
}

func sameDevice(a, b fs.FileInfo) bool {
	as, aok := a.Sys().(*syscall.Stat_t)
	bs, bok := b.Sys().(*syscall.Stat_t)
	return aok && bok && as.Dev == bs.Dev
}

// openPinnedDirectory walks every ancestor from a fixed root. Symlink aliases
// are rejected before a stage name can be created. Comparing each opened inode
// also detects replacement between Lstat and OpenRoot. A bind mount into the
// source storage is rejected when its inode appears in this ancestry.
func openPinnedDirectory(path string, forbidden fs.FileInfo) (*os.Root, fs.FileInfo, error) {
	current, err := os.OpenRoot("/")
	if err != nil {
		return nil, nil, err
	}
	for _, name := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		if name == "" || name == "." || name == ".." {
			_ = current.Close()
			return nil, nil, ErrInvalid
		}
		before, err := current.Lstat(name)
		if err != nil || !before.IsDir() || before.Mode()&os.ModeSymlink != 0 || os.SameFile(before, forbidden) {
			_ = current.Close()
			return nil, nil, ErrInvalid
		}
		next, err := current.OpenRoot(name)
		if err != nil {
			_ = current.Close()
			return nil, nil, err
		}
		opened, statErr := next.Stat(".")
		after, afterErr := current.Lstat(name)
		if statErr != nil || afterErr != nil || !os.SameFile(before, opened) || !os.SameFile(before, after) {
			_ = next.Close()
			_ = current.Close()
			return nil, nil, ErrChanged
		}
		if err := current.Close(); err != nil {
			_ = next.Close()
			return nil, nil, err
		}
		current = next
	}
	info, err := current.Stat(".")
	if err != nil {
		_ = current.Close()
		return nil, nil, err
	}
	return current, info, nil
}

func syncRoot(root *os.Root) error {
	file, err := root.Open(".")
	if err != nil {
		return err
	}
	return errors.Join(file.Sync(), file.Close())
}

func createStage(parent *os.Root) (string, *os.Root, fs.FileInfo, error) {
	for attempt := 0; attempt < 8; attempt++ {
		var nonce [16]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return "", nil, nil, err
		}
		name := "novel-private-publish-" + hex.EncodeToString(nonce[:])
		if err := parent.Mkdir(name, 0700); errors.Is(err, fs.ErrExist) {
			continue
		} else if err != nil {
			return "", nil, nil, err
		}
		root, err := parent.OpenRoot(name)
		if err != nil {
			_ = parent.RemoveAll(name)
			return "", nil, nil, err
		}
		info, err := root.Stat(".")
		if err != nil {
			_ = root.Close()
			_ = parent.RemoveAll(name)
			return "", nil, nil, err
		}
		return name, root, info, nil
	}
	return "", nil, nil, ErrInvalid
}

// Stage makes a bounded copy of every file and directory, including unknown
// assets, DB sidecars, Lance data and copies. It rejects links and special
// files. Both source and copy are scanned after writing, under the same lock.
// stageParent must be a caller-owned 0700 directory on the source filesystem,
// outside the source storage tree. It is trusted server configuration.
func Stage(ctx context.Context, held *writerlock.Lock, dataRoot string, library *projectfs.Library, projectPath, stageParent string, limits Limits) (*Snapshot, error) {
	if ctx == nil || held == nil || library == nil || !canonical(dataRoot) || !canonical(projectPath) || !canonical(stageParent) ||
		!childOf(dataRoot, projectPath) || limits.MaxFiles < 2 || limits.MaxFiles > maxTreeFiles ||
		limits.MaxBytes < 1 || limits.MaxBytes > maxTreeBytes || limits.MaxDepth < 1 || limits.MaxDepth > maxTreeDepth {
		return nil, ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := held.VerifyLegacy(dataRoot); err != nil {
		return nil, err
	}
	project, err := library.OpenProject(projectPath)
	if err != nil {
		return nil, err
	}
	defer project.Close()
	if err := project.VerifyNoPortableAuthority(ctx); err != nil {
		return nil, err
	}
	storagePath := filepath.Join(projectPath, projectfs.StorageDirectory)
	if childOf(storagePath, stageParent) || stageParent == storagePath || childOf(stageParent, storagePath) {
		return nil, ErrInvalid
	}
	storageInfo, err := privateDirectory(storagePath)
	if err != nil {
		return nil, err
	}
	pathParentInfo, err := privateDirectory(stageParent)
	if err != nil || !sameDevice(storageInfo, pathParentInfo) {
		return nil, ErrInvalid
	}
	parent, parentInfo, err := openPinnedDirectory(stageParent, storageInfo)
	if err != nil {
		return nil, err
	}
	ownedParent := true
	defer func() {
		if ownedParent {
			_ = parent.Close()
		}
	}()
	if !os.SameFile(pathParentInfo, parentInfo) {
		return nil, ErrChanged
	}
	source, err := os.OpenRoot(storagePath)
	if err != nil {
		return nil, err
	}
	defer source.Close()
	openedStorage, err := source.Stat(".")
	if err != nil || !os.SameFile(storageInfo, openedStorage) {
		return nil, ErrChanged
	}
	aliasInsideSource, err := containsDirectoryInode(ctx, source, parentInfo, limits)
	if err != nil {
		return nil, err
	}
	if aliasInsideSource {
		return nil, ErrInvalid
	}
	stageName, copyRoot, stageInfo, err := createStage(parent)
	if err != nil {
		return nil, err
	}
	stage := filepath.Join(stageParent, stageName)
	cleanup := true
	defer func() {
		if cleanup {
			_ = parent.RemoveAll(stageName)
			_ = syncRoot(parent)
		}
	}()
	if stageInfo.Mode().Perm() != 0700 || !sameDevice(storageInfo, stageInfo) {
		_ = copyRoot.Close()
		return nil, ErrInvalid
	}
	if err := copyTree(ctx, source, copyRoot, "", 0, limits, new(int), new(int64)); err != nil {
		_ = copyRoot.Close()
		return nil, err
	}
	if err := syncRoot(copyRoot); err != nil {
		_ = copyRoot.Close()
		return nil, err
	}
	if err := copyRoot.Close(); err != nil {
		return nil, err
	}
	if err := syncRoot(parent); err != nil {
		return nil, err
	}
	before, err := scan(ctx, source, limits)
	if err != nil {
		return nil, err
	}
	staged, err := parent.OpenRoot(stageName)
	if err != nil {
		return nil, err
	}
	copied, scanErr := scan(ctx, staged, limits)
	closeErr := staged.Close()
	if scanErr != nil {
		return nil, scanErr
	}
	if closeErr != nil || !sameEntries(before, copied) {
		return nil, ErrChanged
	}
	currentStage, err := parent.Lstat(stageName)
	if err != nil || !os.SameFile(stageInfo, currentStage) {
		return nil, ErrChanged
	}
	currentParent, err := os.Lstat(stageParent)
	if err != nil || !os.SameFile(parentInfo, currentParent) {
		return nil, ErrChanged
	}
	currentStorage, err := os.Lstat(storagePath)
	if err != nil || !os.SameFile(storageInfo, currentStorage) {
		return nil, ErrChanged
	}
	snapshot := &Snapshot{lock: held, dataRoot: dataRoot, library: library, projectPath: projectPath,
		projectID: project.ProjectID(), identity: project.Identity(), storageInfo: storageInfo,
		parent: parent, parentPath: stageParent, parentInfo: parentInfo,
		stageName: stageName, stage: stage, stageInfo: stageInfo, entries: before, limits: limits}
	if err := snapshot.VerifySource(ctx); err != nil {
		return nil, err
	}
	cleanup = false
	ownedParent = false
	return snapshot, nil
}

func copyTree(ctx context.Context, source, target *os.Root, prefix string, depth int, limits Limits, files *int, bytes *int64) error {
	if depth > limits.MaxDepth {
		return ErrLimit
	}
	names, err := boundedDirEntries(source, limits.MaxFiles-*files)
	if err != nil {
		return err
	}
	for _, de := range names {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := de.Name()
		if name == "." || name == ".." || strings.ContainsAny(name, "/\\\x00") {
			return ErrInvalid
		}
		before, err := source.Lstat(name)
		if err != nil {
			return err
		}
		if before.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
			return ErrInvalid
		}
		path := name
		if prefix != "" {
			path = prefix + "/" + name
		}
		*files++
		if *files > limits.MaxFiles {
			return ErrLimit
		}
		switch {
		case before.IsDir() && before.Mode()&os.ModeSymlink == 0:
			if depth == limits.MaxDepth {
				return ErrLimit
			}
			if err := target.Mkdir(name, 0700); err != nil {
				return err
			}
			nextSource, err := source.OpenRoot(name)
			if err != nil {
				return err
			}
			nextTarget, err := target.OpenRoot(name)
			if err != nil {
				_ = nextSource.Close()
				return err
			}
			err = copyTree(ctx, nextSource, nextTarget, path, depth+1, limits, files, bytes)
			if err == nil {
				directory, openErr := nextTarget.Open(".")
				if openErr != nil {
					err = openErr
				} else {
					err = directory.Chmod(before.Mode().Perm())
					if err == nil {
						err = directory.Sync()
					}
					err = errors.Join(err, directory.Close())
				}
			}
			closeTarget := nextTarget.Close()
			closeSource := nextSource.Close()
			if err != nil {
				return err
			}
			if closeTarget != nil || closeSource != nil {
				return ErrChanged
			}
		case before.Mode().IsRegular() && before.Mode()&os.ModeSymlink == 0:
			if !oneLink(before) || before.Size() < 0 || before.Size() > limits.MaxBytes-*bytes {
				return ErrLimit
			}
			input, err := source.Open(name)
			if err != nil {
				return err
			}
			opened, statErr := input.Stat()
			if statErr != nil || !sameVersion(before, opened) {
				_ = input.Close()
				return ErrChanged
			}
			output, err := target.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
			if err != nil {
				_ = input.Close()
				return err
			}
			n, copyErr := io.CopyN(output, input, before.Size())
			var extra [1]byte
			extraN, extraErr := input.Read(extra[:])
			if copyErr == nil && (n != before.Size() || extraN != 0 || extraErr != io.EOF) {
				copyErr = ErrChanged
			}
			if copyErr == nil {
				copyErr = output.Chmod(before.Mode().Perm())
			}
			if copyErr == nil {
				copyErr = output.Sync()
			}
			closeOutput := output.Close()
			closeInput := input.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeOutput != nil || closeInput != nil {
				return ErrChanged
			}
			*bytes += n
		default:
			return ErrInvalid
		}
		after, err := source.Lstat(name)
		if err != nil || !sameVersion(before, after) {
			return ErrChanged
		}
	}
	return nil
}

// boundedDirEntries reads in fixed batches and never buffers more names than
// the remaining tree budget. Sorting only the bounded slice keeps traversal
// deterministic without an unbounded ReadDir(-1) allocation.
func boundedDirEntries(root *os.Root, remaining int) ([]fs.DirEntry, error) {
	if remaining < 0 || remaining > maxTreeFiles {
		return nil, ErrLimit
	}
	listing, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	entries := make([]fs.DirEntry, 0, min(remaining, 64))
	for {
		batch, readErr := listing.ReadDir(64)
		if len(batch) > remaining-len(entries) {
			_ = listing.Close()
			return nil, ErrLimit
		}
		entries = append(entries, batch...)
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			_ = listing.Close()
			return nil, readErr
		}
	}
	if err := listing.Close(); err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	return entries, nil
}

// containsDirectoryInode catches bind-mounted or otherwise aliased stage
// parents that point into a nested source directory. It runs before creating
// anything under the proposed parent and honors the same tree budget.
func containsDirectoryInode(ctx context.Context, root *os.Root, candidate fs.FileInfo, limits Limits) (bool, error) {
	seen := 0
	var walk func(*os.Root, int) (bool, error)
	walk = func(current *os.Root, depth int) (bool, error) {
		items, err := boundedDirEntries(current, limits.MaxFiles-seen)
		if err != nil {
			return false, err
		}
		for _, item := range items {
			if err := ctx.Err(); err != nil {
				return false, err
			}
			seen++
			if seen > limits.MaxFiles {
				return false, ErrLimit
			}
			info, err := current.Lstat(item.Name())
			if err != nil {
				return false, err
			}
			if info.Mode()&os.ModeSymlink != 0 {
				return false, ErrInvalid
			}
			if info.IsDir() {
				if os.SameFile(info, candidate) {
					return true, nil
				}
				if depth >= limits.MaxDepth {
					return false, ErrLimit
				}
				child, err := current.OpenRoot(item.Name())
				if err != nil {
					return false, err
				}
				opened, statErr := child.Stat(".")
				if statErr != nil || !os.SameFile(info, opened) {
					_ = child.Close()
					return false, ErrChanged
				}
				found, walkErr := walk(child, depth+1)
				closeErr := child.Close()
				if walkErr != nil {
					return false, walkErr
				}
				if closeErr != nil {
					return false, closeErr
				}
				if found {
					return true, nil
				}
			} else if !info.Mode().IsRegular() || !oneLink(info) {
				return false, ErrInvalid
			}
		}
		return false, nil
	}
	return walk(root, 0)
}

func oneLink(info fs.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Nlink == 1
}

func sameVersion(a, b fs.FileInfo) bool {
	if a == nil || b == nil || !os.SameFile(a, b) || a.Size() != b.Size() || a.Mode() != b.Mode() || !a.ModTime().Equal(b.ModTime()) {
		return false
	}
	as, aok := a.Sys().(*syscall.Stat_t)
	bs, bok := b.Sys().(*syscall.Stat_t)
	return aok && bok && as.Ctim == bs.Ctim && as.Nlink == bs.Nlink
}

func scan(ctx context.Context, root *os.Root, limits Limits) ([]entry, error) {
	result := make([]entry, 0)
	var bytes int64
	var walk func(*os.Root, string, int) error
	walk = func(current *os.Root, prefix string, depth int) error {
		if depth > limits.MaxDepth {
			return ErrLimit
		}
		items, err := boundedDirEntries(current, limits.MaxFiles-len(result))
		if err != nil {
			return err
		}
		for _, de := range items {
			if err := ctx.Err(); err != nil {
				return err
			}
			name := de.Name()
			if name == "." || name == ".." || strings.ContainsAny(name, "/\\\x00") {
				return ErrInvalid
			}
			info, err := current.Lstat(name)
			if err != nil {
				return err
			}
			if info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
				return ErrInvalid
			}
			path := name
			if prefix != "" {
				path = prefix + "/" + name
			}
			if len(result) >= limits.MaxFiles {
				return ErrLimit
			}
			e := entry{path: path, mode: info.Mode(), size: info.Size()}
			switch {
			case info.IsDir() && info.Mode()&os.ModeSymlink == 0:
				if depth == limits.MaxDepth {
					return ErrLimit
				}
				e.size = 0
				result = append(result, e)
				child, err := current.OpenRoot(name)
				if err != nil {
					return err
				}
				err = walk(child, path, depth+1)
				closeErr := child.Close()
				if err != nil {
					return err
				}
				if closeErr != nil {
					return closeErr
				}
			case info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 && oneLink(info):
				if info.Size() < 0 || info.Size() > limits.MaxBytes-bytes {
					return ErrLimit
				}
				file, err := current.Open(name)
				if err != nil {
					return err
				}
				opened, err := file.Stat()
				if err != nil || !sameVersion(info, opened) {
					_ = file.Close()
					return ErrChanged
				}
				h := sha256.New()
				n, err := io.CopyN(h, file, info.Size())
				var extra [1]byte
				extraN, extraErr := file.Read(extra[:])
				after, afterErr := file.Stat()
				closeErr := file.Close()
				if err != nil || n != info.Size() || extraN != 0 || extraErr != io.EOF || afterErr != nil || !sameVersion(info, after) || closeErr != nil {
					return ErrChanged
				}
				copy(e.hash[:], h.Sum(nil))
				bytes += n
				result = append(result, e)
			default:
				return ErrInvalid
			}
			now, err := current.Lstat(name)
			if err != nil || !sameVersion(info, now) {
				return ErrChanged
			}
		}
		return nil
	}
	if err := walk(root, "", 0); err != nil {
		return nil, err
	}
	return result, nil
}

func sameEntries(a, b []entry) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].path != b[i].path || a[i].size != b[i].size || a[i].hash != b[i].hash || a[i].mode.Type() != b[i].mode.Type() || a[i].mode.Perm() != b[i].mode.Perm() {
			return false
		}
	}
	return true
}

// VerifySource checks the original source and live lock. It does not grant
// author write authority, even if it returns nil.
func (s *Snapshot) VerifySource(ctx context.Context) error {
	if s == nil || ctx == nil {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrInvalid
	}
	if err := s.lock.VerifyLegacy(s.dataRoot); err != nil {
		return err
	}
	p, err := s.library.OpenProject(s.projectPath)
	if err != nil {
		return err
	}
	defer p.Close()
	if p.ProjectID() != s.projectID || !p.Identity().Same(s.identity) {
		return ErrChanged
	}
	if err := p.VerifyNoPortableAuthority(ctx); err != nil {
		return err
	}
	root, err := os.OpenRoot(filepath.Join(s.projectPath, projectfs.StorageDirectory))
	if err != nil {
		return err
	}
	opened, openErr := root.Stat(".")
	if openErr != nil || !os.SameFile(s.storageInfo, opened) {
		_ = root.Close()
		return ErrChanged
	}
	entries, scanErr := scan(ctx, root, s.limits)
	closeErr := root.Close()
	if scanErr != nil {
		return scanErr
	}
	if closeErr != nil {
		return closeErr
	}
	if !sameEntries(s.entries, entries) {
		return ErrChanged
	}
	return s.lock.VerifyLegacy(s.dataRoot)
}

// VerifyPrivateCopy detects mutation of the staged tree before a caller uses
// it as an immutable source. It does not validate any later SQLite edits.
func (s *Snapshot) VerifyPrivateCopy(ctx context.Context) error {
	if s == nil || ctx == nil {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrInvalid
	}
	if err := s.lock.VerifyLegacy(s.dataRoot); err != nil {
		return err
	}
	currentParent, err := os.Lstat(s.parentPath)
	if err != nil || !os.SameFile(s.parentInfo, currentParent) {
		return ErrChanged
	}
	root, err := s.parent.OpenRoot(s.stageName)
	if err != nil {
		return err
	}
	opened, openErr := root.Stat(".")
	if openErr != nil || !os.SameFile(s.stageInfo, opened) {
		_ = root.Close()
		return ErrChanged
	}
	entries, scanErr := scan(ctx, root, s.limits)
	closeErr := root.Close()
	if scanErr != nil {
		return scanErr
	}
	if closeErr != nil {
		return closeErr
	}
	if !sameEntries(s.entries, entries) {
		return ErrChanged
	}
	return s.lock.VerifyLegacy(s.dataRoot)
}

// PrivatePath is the private snapshot location. It is suitable for disposable
// tests and isolated parsing, not for source replacement or restoration.
// Ordinary permission bits are copied exactly inside a private 0700 parent.
// This snapshot is still not publish-ready: no transaction, journal, or
// namespace exchange authority is provided.
func (s *Snapshot) PrivatePath() (string, error) {
	if s == nil {
		return "", ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return "", ErrInvalid
	}
	if s.removed {
		return "", ErrChanged
	}
	currentParent, err := os.Lstat(s.parentPath)
	if err != nil || !os.SameFile(s.parentInfo, currentParent) {
		return "", ErrChanged
	}
	currentStage, err := s.parent.Lstat(s.stageName)
	if err != nil || !os.SameFile(s.stageInfo, currentStage) {
		return "", ErrChanged
	}
	return s.stage, nil
}

func (s *Snapshot) PublicationAuthority(context.Context) error { return ErrPublishUnavailable }

// Close removes the private copy. Any caller-created handles to it must have
// been closed before Close is called.
func (s *Snapshot) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	if !canonical(s.stage) || !strings.HasPrefix(s.stageName, "novel-private-publish-") || s.parent == nil {
		return ErrInvalid
	}
	if !s.removed {
		current, err := s.parent.Lstat(s.stageName)
		if err != nil || !os.SameFile(s.stageInfo, current) {
			return ErrChanged
		}
		if err := s.parent.RemoveAll(s.stageName); err != nil {
			return fmt.Errorf("remove private snapshot: %w", err)
		}
		s.removed = true
	}
	if err := syncRoot(s.parent); err != nil {
		return err
	}
	if err := s.parent.Close(); err != nil {
		return err
	}
	s.closed = true
	return nil
}
