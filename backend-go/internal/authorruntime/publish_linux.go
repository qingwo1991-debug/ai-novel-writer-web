//go:build linux

package authorruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
)

var ErrSnapshotUnsafe = errors.New("AUTHOR_SNAPSHOT_UNSAFE")

const sealedSnapshotManifest = "database-snapshot.json"

// DatabaseMember records exact bytes and membership of a private database copy.
// A WAL or rollback journal is preserved as a separate member when present.
type DatabaseMember struct {
	Name   string `json:"name"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

type DatabaseSnapshotManifest struct {
	Version   int              `json:"version"`
	ProjectID string           `json:"projectId"`
	Members   []DatabaseMember `json:"members"`
}

// SealedDatabaseSnapshot is a durable, private copy of the SQLite file set.
// It is not a restorable author backup: knowledge assets, global credentials,
// transaction publication and rollback authority are deliberately absent.
// The caller owns the directory after Close and must retain/delete it under
// its own private backup policy. Verify requires the originating Reader's
// legacy lock to remain held and its source project to remain unchanged.
type SealedDatabaseSnapshot struct {
	mu       sync.Mutex
	reader   *Reader
	proof    *projectfs.LockedSnapshot
	root     *os.Root
	dir      string
	dirInfo  os.FileInfo
	manifest DatabaseSnapshotManifest
}

func (s *SealedDatabaseSnapshot) Directory() string {
	if s == nil {
		return ""
	}
	return s.dir
}

func (s *SealedDatabaseSnapshot) Manifest() DatabaseSnapshotManifest {
	if s == nil {
		return DatabaseSnapshotManifest{}
	}
	copy := s.manifest
	copy.Members = append([]DatabaseMember(nil), copy.Members...)
	return copy
}

// SealDatabaseSnapshot stages the full source DB/WAL/SHM/journal set under
// the existing legacy lock, hashes every copied member, fsyncs each file and
// its private directory and parent, then rechecks the source. It does not
// open the author database for writing or publish any new author bytes.
func (reader *Reader) SealDatabaseSnapshot(ctx context.Context, privateParent string, maxBytes int64) (_ *SealedDatabaseSnapshot, err error) {
	if reader == nil || ctx == nil || maxBytes <= 0 {
		return nil, ErrInvalid
	}
	reader.mu.Lock()
	defer reader.mu.Unlock()
	if err := reader.verifyLocked(ctx); err != nil {
		return nil, err
	}
	parentInfo, err := privateSnapshotParent(privateParent, reader.dataRoot)
	if err != nil {
		return nil, err
	}
	parent, err := os.Open(privateParent)
	if err != nil {
		return nil, ErrSnapshotUnsafe
	}
	defer parent.Close()
	openedParent, err := parent.Stat()
	if err != nil || !os.SameFile(parentInfo, openedParent) {
		return nil, ErrSnapshotUnsafe
	}
	directory, err := os.MkdirTemp(privateParent, "novel-go-db-snapshot-")
	if err != nil {
		return nil, ErrSnapshotUnsafe
	}
	var root *os.Root
	directoryInfo, _ := os.Lstat(directory)
	defer func() {
		if err == nil {
			return
		}
		if root != nil {
			for _, name := range []string{"snapshot.db", "snapshot.db-wal", "snapshot.db-shm", "snapshot.db-journal", sealedSnapshotManifest} {
				_ = root.Remove(name)
			}
			_ = root.Close()
		}
		if current, statErr := os.Lstat(directory); statErr == nil && directoryInfo != nil && os.SameFile(current, directoryInfo) {
			_ = os.Remove(directory)
		}
	}()
	if err := os.Chmod(directory, 0700); err != nil {
		return nil, ErrSnapshotUnsafe
	}
	directoryInfo, err = os.Lstat(directory)
	if err != nil || !privateSnapshotDirectory(directoryInfo) {
		return nil, ErrSnapshotUnsafe
	}
	root, err = os.OpenRoot(directory)
	if err != nil {
		return nil, ErrSnapshotUnsafe
	}
	openedDir, err := root.Stat(".")
	if err != nil || !os.SameFile(directoryInfo, openedDir) {
		return nil, ErrSnapshotUnsafe
	}
	proof, err := reader.project.StageDatabaseUnderLegacyLock(ctx, reader.lock, reader.dataRoot, root, maxBytes)
	if err != nil {
		return nil, err
	}
	manifest := DatabaseSnapshotManifest{Version: 1, ProjectID: reader.report.ProjectID}
	for _, name := range []string{"snapshot.db", "snapshot.db-wal", "snapshot.db-shm", "snapshot.db-journal"} {
		member, memberErr := syncAndHashSnapshotMember(ctx, root, name)
		if errors.Is(memberErr, fs.ErrNotExist) {
			continue
		}
		if memberErr != nil {
			return nil, memberErr
		}
		manifest.Members = append(manifest.Members, member)
	}
	if len(manifest.Members) == 0 || manifest.Members[0].Name != "snapshot.db" {
		return nil, ErrSnapshotUnsafe
	}
	if err := writeSealedManifest(root, manifest); err != nil {
		return nil, err
	}
	if err := syncSnapshotDirectory(root); err != nil {
		return nil, err
	}
	if err := parent.Sync(); err != nil {
		return nil, ErrSnapshotUnsafe
	}
	if currentParent, statErr := os.Lstat(privateParent); statErr != nil || !os.SameFile(parentInfo, currentParent) {
		return nil, ErrChanged
	}
	if err := proof.VerifyUnchanged(ctx); err != nil {
		return nil, err
	}
	if err := reader.verifyLocked(ctx); err != nil {
		return nil, err
	}
	if current, statErr := os.Lstat(directory); statErr != nil || !os.SameFile(directoryInfo, current) {
		return nil, ErrChanged
	}
	return &SealedDatabaseSnapshot{reader: reader, proof: proof, root: root, dir: directory, dirInfo: directoryInfo, manifest: manifest}, nil
}

func privateSnapshotParent(path, dataRoot string) (os.FileInfo, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == dataRoot {
		return nil, ErrSnapshotUnsafe
	}
	if rel, err := filepath.Rel(dataRoot, path); err == nil && rel != ".." && rel != "." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel) {
		return nil, ErrSnapshotUnsafe
	}
	info, err := os.Lstat(path)
	if err != nil || !privateSnapshotDirectory(info) {
		return nil, ErrSnapshotUnsafe
	}
	return info, nil
}

func privateSnapshotDirectory(info os.FileInfo) bool {
	if info == nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid())
}

func syncAndHashSnapshotMember(ctx context.Context, root *os.Root, name string) (DatabaseMember, error) {
	before, err := root.Lstat(name)
	if err != nil {
		return DatabaseMember{}, err
	}
	if !privateSnapshotFile(before) {
		return DatabaseMember{}, ErrSnapshotUnsafe
	}
	file, err := root.Open(name)
	if err != nil {
		return DatabaseMember{}, ErrSnapshotUnsafe
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return DatabaseMember{}, ErrSnapshotUnsafe
	}
	hash := sha256.New()
	buffer := make([]byte, 128*1024)
	var size int64
	for {
		if err := ctx.Err(); err != nil {
			return DatabaseMember{}, err
		}
		n, readErr := file.Read(buffer)
		if n > 0 {
			_, _ = hash.Write(buffer[:n])
			size += int64(n)
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil || n == 0 {
			return DatabaseMember{}, ErrSnapshotUnsafe
		}
	}
	if err := file.Sync(); err != nil {
		return DatabaseMember{}, ErrSnapshotUnsafe
	}
	after, err := file.Stat()
	named, nameErr := root.Lstat(name)
	if err != nil || nameErr != nil || !os.SameFile(opened, after) || !os.SameFile(after, named) || size != after.Size() {
		return DatabaseMember{}, ErrSnapshotUnsafe
	}
	return DatabaseMember{Name: name, Bytes: size, SHA256: hex.EncodeToString(hash.Sum(nil))}, nil
}

func privateSnapshotFile(info os.FileInfo) bool {
	if info == nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid()) && stat.Nlink == 1
}

func writeSealedManifest(root *os.Root, value DatabaseSnapshotManifest) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return ErrSnapshotUnsafe
	}
	file, err := root.OpenFile(sealedSnapshotManifest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return ErrSnapshotUnsafe
	}
	if n, err := file.Write(raw); err != nil || n != len(raw) {
		_ = file.Close()
		return ErrSnapshotUnsafe
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return ErrSnapshotUnsafe
	}
	if err := file.Close(); err != nil {
		return ErrSnapshotUnsafe
	}
	return nil
}

func syncSnapshotDirectory(root *os.Root) error {
	directory, err := root.Open(".")
	if err != nil {
		return ErrSnapshotUnsafe
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return ErrSnapshotUnsafe
	}
	return nil
}

// Verify compares sealed bytes to the private manifest and checks that the
// source project still matches the same live legacy lock. It does not infer
// that restoring these SQLite files alone can roll back an author project.
func (s *SealedDatabaseSnapshot) Verify(ctx context.Context) error {
	if s == nil || s.reader == nil || ctx == nil {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.root == nil {
		return ErrClosed
	}
	s.reader.mu.Lock()
	defer s.reader.mu.Unlock()
	if err := s.reader.verifyLocked(ctx); err != nil {
		return err
	}
	if info, err := os.Lstat(s.dir); err != nil || !os.SameFile(s.dirInfo, info) {
		return ErrChanged
	}
	if err := s.proof.VerifyUnchanged(ctx); err != nil {
		return err
	}
	for _, member := range s.manifest.Members {
		current, err := syncAndHashSnapshotMember(ctx, s.root, member.Name)
		if err != nil || current != member {
			return ErrChanged
		}
	}
	known := make(map[string]bool, len(s.manifest.Members))
	for _, member := range s.manifest.Members {
		known[member.Name] = true
	}
	for _, name := range []string{"snapshot.db", "snapshot.db-wal", "snapshot.db-shm", "snapshot.db-journal"} {
		_, statErr := s.root.Lstat(name)
		if known[name] && statErr != nil || !known[name] && !errors.Is(statErr, fs.ErrNotExist) {
			return ErrChanged
		}
	}
	data, err := s.root.Open(sealedSnapshotManifest)
	if err != nil {
		return ErrChanged
	}
	raw, readErr := io.ReadAll(io.LimitReader(data, 1<<20))
	closeErr := data.Close()
	if readErr != nil || closeErr != nil {
		return ErrChanged
	}
	encoded, err := json.Marshal(s.manifest)
	if err != nil || string(raw) != string(encoded) {
		return ErrChanged
	}
	return s.reader.verifyLocked(ctx)
}

// Close relinquishes only the pinned backup-directory handle. It deliberately
// retains the fsynced private files for an operator-controlled later restore.
func (s *SealedDatabaseSnapshot) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.root == nil {
		return nil
	}
	err := s.root.Close()
	s.root = nil
	return err
}
