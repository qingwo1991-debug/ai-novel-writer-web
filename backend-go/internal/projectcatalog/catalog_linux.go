//go:build linux

// Package projectcatalog stores server-owned recent navigation metadata and
// issues process-local keys for an already admitted project capability. A key
// never grants database admission or durable write authority.
package projectcatalog

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
)

const fileName = "recent-projects.go.json"
const maxFileBytes = 64 << 10
const maxRecent = 20

var ErrCatalogUnsafe = errors.New("PROJECT_CATALOG_UNSAFE")
var ErrCatalogInvalid = errors.New("PROJECT_CATALOG_INVALID")
var ErrCatalogKey = errors.New("PROJECT_CATALOG_KEY_INVALID")
var ErrCatalogRecoveryPending = errors.New("PROJECT_CATALOG_RECOVERY_PENDING")

// RecoveryCheck is supplied by the trusted startup/publisher integration. It
// must reject an unresolved exchange for the exact path and project ID. It is
// called under the Store mutex and must not call back into the Store. Returning
// nil is appropriate only after journal classification and post-swap readback
// have established that issuing a new capability is safe.
type RecoveryCheck func(path, projectID string) error

type Recent struct {
	Key       string `json:"key,omitempty"`
	Name      string `json:"name"`
	Path      string `json:"path"`
	ProjectID string `json:"projectId"`
	UpdatedAt string `json:"updatedAt"`
}

type record struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	ProjectID string `json:"projectId"`
	UpdatedAt string `json:"updatedAt"`
}

type document struct {
	Version int      `json:"version"`
	Recent  []record `json:"recent"`
}

type capability struct {
	path     string
	id       string
	identity projectfs.Identity
}

type Store struct {
	mu         sync.Mutex
	library    *projectfs.Library
	directory  string
	recent     []record
	byPath     map[string]string
	keys       map[string]capability
	removeKeys map[string]string
	recovery   RecoveryCheck
	holds      map[string]string
}

// Open takes a trusted, private metadata directory and a trusted library.
// A corrupt existing file fails closed; it is never silently reset.
func Open(library *projectfs.Library, directory string) (*Store, error) {
	return open(library, directory, nil)
}

// OpenWithRecoveryCheck fences catalog capability issuance and resolution
// against an external, trusted exchange-recovery decision. The legacy Open
// constructor has no recovery integration and must not be used by a publisher.
func OpenWithRecoveryCheck(library *projectfs.Library, directory string, check RecoveryCheck) (*Store, error) {
	if check == nil {
		return nil, ErrCatalogInvalid
	}
	return open(library, directory, check)
}

func open(library *projectfs.Library, directory string, check RecoveryCheck) (*Store, error) {
	if library == nil || !filepath.IsAbs(directory) || filepath.Clean(directory) != directory {
		return nil, ErrCatalogInvalid
	}
	if err := privateDirectory(directory); err != nil {
		return nil, err
	}
	entries, err := readDocument(filepath.Join(directory, fileName))
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if filepath.Dir(entry.Path) != library.Path() {
			return nil, ErrCatalogInvalid
		}
	}
	return &Store{library: library, directory: directory, recent: entries, byPath: make(map[string]string), keys: make(map[string]capability), removeKeys: make(map[string]string), recovery: check, holds: make(map[string]string)}, nil
}

func (s *Store) checkRecoveryLocked(path, projectID string) error {
	if s.holds[path] != "" {
		return ErrCatalogRecoveryPending
	}
	if s.recovery == nil {
		return nil
	}
	if err := s.recovery(path, projectID); err != nil {
		return errors.Join(ErrCatalogRecoveryPending, err)
	}
	return nil
}

// HoldRecovery synchronizes an in-process exchange with List, Resolve and
// Register. Once it returns, no catalog operation can issue or resolve a key
// for this project until a verified release or promotion succeeds. The trusted
// publisher must acquire this hold before changing the source tree. Holds are
// process-local; restart safety requires OpenWithRecoveryCheck to consult the
// persisted exchange journal before any catalog operation.
func (s *Store) HoldRecovery(path, projectID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if path == "" || projectID == "" || s.holds[path] != "" {
		return ErrCatalogInvalid
	}
	found := false
	for _, entry := range s.recent {
		if entry.Path == path && entry.ProjectID == projectID {
			found = true
			break
		}
	}
	if !found {
		return ErrCatalogInvalid
	}
	s.holds[path] = projectID
	return nil
}

// ReleaseAfterVerifiedRecovery accepts a freshly opened project capability
// only after the trusted recovery checker has confirmed a durable SWITCHED
// marker and post-swap readback. It revokes all keys for the old inode before
// making the new project available. A store without a checker cannot release.
// Coordinators that need a new key must use PromoteVerifiedRecovery instead.
func (s *Store) ReleaseAfterVerifiedRecovery(project *projectfs.Project) error {
	if project == nil {
		return ErrCatalogInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	path, projectID := project.Path(), project.ProjectID()
	if s.holds[path] != projectID || s.recovery == nil {
		return ErrCatalogRecoveryPending
	}
	if err := s.recovery(path, projectID); err != nil {
		return errors.Join(ErrCatalogRecoveryPending, err)
	}
	current, err := s.library.OpenProject(path)
	if err != nil {
		return err
	}
	defer current.Close()
	if current.ProjectID() != projectID || !current.Identity().Same(project.Identity()) {
		return projectfs.ErrChanged
	}
	s.revokeLocked(path)
	delete(s.holds, path)
	return nil
}

// PromoteVerifiedRecovery publishes a replacement catalog key without a
// reader binding. Coordinators that publish a Reader must use the binding
// variant so readers cannot observe a key before its Reader is installed.
func (s *Store) PromoteVerifiedRecovery(ctx context.Context, project *projectfs.Project) (Recent, error) {
	return s.PromoteVerifiedRecoveryWithBinding(ctx, project, nil)
}

// PromoteVerifiedRecoveryWithBinding binds a fenced replacement reader and
// publishes its key in one catalog critical section. The binder receives the
// caller's pinned project and generated key. An independently opened copy is
// verified against that project and closed before binding. The binder must not
// call back into this Store or close the pinned project. On success it returns
// a no-fail unlock closure, holding its reader lock until that closure runs.
// On error it must leave the reader fenced
// and release any lock it acquired. The prior Register and HoldRecovery keep
// navigation metadata durable, so promotion only changes process-local keys.
// The caller must own source writer exclusion throughout.
func (s *Store) PromoteVerifiedRecoveryWithBinding(ctx context.Context, project *projectfs.Project, bind func(key string, verified *projectfs.Project) (unlock func(), err error)) (Recent, error) {
	if ctx == nil || project == nil {
		return Recent{}, ErrCatalogInvalid
	}
	if err := ctx.Err(); err != nil {
		return Recent{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	path, projectID, identity := project.Path(), project.ProjectID(), project.Identity()
	if s.holds[path] != projectID || s.recovery == nil {
		return Recent{}, ErrCatalogRecoveryPending
	}
	var saved *record
	for i := range s.recent {
		if s.recent[i].Path == path && s.recent[i].ProjectID == projectID {
			saved = &s.recent[i]
			break
		}
	}
	if saved == nil {
		return Recent{}, ErrCatalogInvalid
	}
	if err := s.recovery(path, projectID); err != nil {
		return Recent{}, errors.Join(ErrCatalogRecoveryPending, err)
	}
	if err := project.VerifyNoPortableAuthority(ctx); err != nil {
		return Recent{}, err
	}
	current, err := s.library.OpenProject(path)
	if err != nil {
		return Recent{}, err
	}
	if current.ProjectID() != projectID || !current.Identity().Same(identity) {
		current.Close()
		return Recent{}, projectfs.ErrChanged
	}
	if err := current.VerifyNoPortableAuthority(ctx); err != nil {
		current.Close()
		return Recent{}, err
	}
	if err := ctx.Err(); err != nil {
		current.Close()
		return Recent{}, err
	}
	var key string
	for {
		key, err = newKey()
		if err != nil {
			current.Close()
			return Recent{}, err
		}
		if _, exists := s.keys[key]; exists {
			continue
		}
		if _, exists := s.removeKeys[key]; !exists {
			break
		}
	}
	// Close and check cancellation before binding. Once a reader is bound to
	// this key, publication must not encounter any further fallible step.
	if err := current.Close(); err != nil {
		return Recent{}, err
	}
	if err := ctx.Err(); err != nil {
		return Recent{}, err
	}
	var unlock func()
	if bind != nil {
		unlock, err = bind(key, project)
		if err != nil {
			return Recent{}, err
		}
		if unlock == nil {
			return Recent{}, ErrCatalogInvalid
		}
	}
	// No fallible operation follows. Catalog readers cannot observe an
	// intermediate state because the Store mutex remains held throughout.
	s.revokeLocked(path)
	s.byPath[path] = key
	s.keys[key] = capability{path: path, id: projectID, identity: identity}
	delete(s.holds, path)
	if unlock != nil {
		unlock()
	}
	return Recent{Key: key, Name: saved.Name, Path: path, ProjectID: projectID, UpdatedAt: saved.UpdatedAt}, nil
}

func privateDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return ErrCatalogUnsafe
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return ErrCatalogUnsafe
	}
	opened, err := os.Open(path)
	if err != nil {
		return err
	}
	defer opened.Close()
	actual, err := opened.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(info, actual) {
		return ErrCatalogUnsafe
	}
	return nil
}

func readDocument(path string) ([]record, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > maxFileBytes {
		return nil, ErrCatalogUnsafe
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) || stat.Nlink != 1 {
		return nil, ErrCatalogUnsafe
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	actual, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(info, actual) {
		return nil, ErrCatalogUnsafe
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxFileBytes {
		return nil, ErrCatalogUnsafe
	}
	var data document
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&data); err != nil {
		return nil, ErrCatalogInvalid
	}
	if decoder.Decode(new(any)) != io.EOF || data.Version != 1 || len(data.Recent) > maxRecent {
		return nil, ErrCatalogInvalid
	}
	seen := make(map[string]bool)
	for _, entry := range data.Recent {
		if !validRecord(entry) || seen[entry.Path] {
			return nil, ErrCatalogInvalid
		}
		seen[entry.Path] = true
	}
	return data.Recent, nil
}

func validRecord(entry record) bool {
	if entry.Name == "" || len(entry.Name) > 256 || strings.ContainsAny(entry.Name, "\x00/\\") || len(entry.Path) > 4096 || strings.ContainsAny(entry.Path, "\x00\\") || !filepath.IsAbs(entry.Path) || filepath.Clean(entry.Path) != entry.Path || entry.ProjectID == "" {
		return false
	}
	_, err := time.Parse(time.RFC3339Nano, entry.UpdatedAt)
	return err == nil
}

func (s *Store) writeLocked(entries []record) error {
	if err := privateDirectory(s.directory); err != nil {
		return err
	}
	path := filepath.Join(s.directory, fileName)
	if _, err := readDocument(path); err != nil {
		return err
	}
	raw, err := json.Marshal(document{Version: 1, Recent: entries})
	if err != nil {
		return err
	}
	if len(raw) > maxFileBytes {
		return ErrCatalogInvalid
	}
	tmp, err := os.CreateTemp(s.directory, ".recent-projects-go-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(s.directory)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// Register requires a trusted projectfs capability. Its path and UUID are
// re-admitted against the pinned library before metadata is replaced.
func (s *Store) Register(project *projectfs.Project) (Recent, error) {
	if project == nil {
		return Recent{}, ErrCatalogInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkRecoveryLocked(project.Path(), project.ProjectID()); err != nil {
		return Recent{}, err
	}
	current, err := s.library.OpenProject(project.Path())
	if err != nil {
		return Recent{}, err
	}
	defer current.Close()
	if current.ProjectID() != project.ProjectID() || !current.Identity().Same(project.Identity()) {
		return Recent{}, projectfs.ErrChanged
	}
	entry := record{Name: filepath.Base(project.Path()), Path: project.Path(), ProjectID: project.ProjectID(), UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	entries := make([]record, 0, maxRecent)
	entries = append(entries, entry)
	for _, old := range s.recent {
		if old.Path != entry.Path && len(entries) < maxRecent {
			entries = append(entries, old)
		}
	}
	if err := s.writeLocked(entries); err != nil {
		return Recent{}, err
	}
	s.recent = entries
	s.revokeLocked(entry.Path)
	key, err := s.issueLocked(current)
	if err != nil {
		return Recent{}, err
	}
	return Recent{Key: key, Name: entry.Name, Path: entry.Path, ProjectID: entry.ProjectID, UpdatedAt: entry.UpdatedAt}, nil
}

func (s *Store) revokeLocked(path string) {
	if key := s.byPath[path]; key != "" {
		delete(s.keys, key)
		delete(s.removeKeys, key)
		delete(s.byPath, path)
	}
}

func newKey() (string, error) {
	bytes := make([]byte, 24)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(bytes), nil
}

func (s *Store) issueLocked(project *projectfs.Project) (string, error) {
	if key := s.byPath[project.Path()]; key != "" {
		if _, ok := s.keys[key]; ok {
			return key, nil
		}
		s.revokeLocked(project.Path())
	}
	key, err := newKey()
	if err != nil {
		return "", err
	}
	s.byPath[project.Path()] = key
	s.keys[key] = capability{path: project.Path(), id: project.ProjectID(), identity: project.Identity()}
	return key, nil
}

func (s *Store) issueRemovalLocked(path string) (string, error) {
	if key := s.byPath[path]; key != "" {
		if _, ok := s.removeKeys[key]; ok {
			return key, nil
		}
		s.revokeLocked(path)
	}
	key, err := newKey()
	if err != nil {
		return "", err
	}
	s.byPath[path] = key
	s.removeKeys[key] = path
	return key, nil
}

// List revalidates every stored path against the configured library. Missing
// projects remain navigation metadata with a removal-only key.
func (s *Store) List() ([]Recent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]Recent, 0, len(s.recent))
	for _, entry := range s.recent {
		if err := s.checkRecoveryLocked(entry.Path, entry.ProjectID); err != nil {
			return nil, err
		}
		item := Recent{Name: entry.Name, Path: entry.Path, ProjectID: entry.ProjectID, UpdatedAt: entry.UpdatedAt}
		project, err := s.library.OpenProject(entry.Path)
		if err == nil {
			if project.ProjectID() == entry.ProjectID {
				if key := s.byPath[entry.Path]; key != "" && !s.keys[key].identity.Same(project.Identity()) {
					s.revokeLocked(entry.Path)
				}
				item.Key, err = s.issueLocked(project)
			} else {
				s.revokeLocked(entry.Path)
			}
			project.Close()
		} else {
			if errors.Is(err, projectfs.ErrClosed) {
				return nil, err
			}
			s.revokeLocked(entry.Path)
			err = nil
		}
		if err != nil {
			return nil, err
		}
		if item.Key == "" {
			item.Key, err = s.issueRemovalLocked(entry.Path)
			if err != nil {
				return nil, err
			}
		}
		result = append(result, item)
	}
	return result, nil
}

// Resolve returns a fresh projectfs capability only for a key issued in this
// process. The caller owns Close and must perform separate data admission.
func (s *Store) Resolve(key string) (*projectfs.Project, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cap, ok := s.keys[key]
	if !ok || key == "" {
		return nil, ErrCatalogKey
	}
	if err := s.checkRecoveryLocked(cap.path, cap.id); err != nil {
		return nil, err
	}
	current := false
	for _, entry := range s.recent {
		if entry.Path == cap.path && entry.ProjectID == cap.id {
			current = true
			break
		}
	}
	if !current {
		s.revokeLocked(cap.path)
		return nil, ErrCatalogKey
	}
	project, err := s.library.OpenProject(cap.path)
	if err != nil {
		s.revokeLocked(cap.path)
		return nil, err
	}
	if project.ProjectID() != cap.id || !project.Identity().Same(cap.identity) {
		project.Close()
		s.revokeLocked(cap.path)
		return nil, projectfs.ErrChanged
	}
	return project, nil
}

// Remove accepts only a live catalog key, never a client-selected filesystem
// path. It does not delete the project directory.
func (s *Store) Remove(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cap, ok := s.keys[key]
	path := cap.path
	if !ok {
		path, ok = s.removeKeys[key]
	}
	if !ok || key == "" {
		return ErrCatalogKey
	}
	projectID := cap.id
	if projectID == "" {
		for _, entry := range s.recent {
			if entry.Path == path {
				projectID = entry.ProjectID
				break
			}
		}
	}
	if err := s.checkRecoveryLocked(path, projectID); err != nil {
		return err
	}
	entries := make([]record, 0, len(s.recent))
	for _, entry := range s.recent {
		if entry.Path != path {
			entries = append(entries, entry)
		}
	}
	if err := s.writeLocked(entries); err != nil {
		return err
	}
	s.recent = entries
	s.revokeLocked(path)
	return nil
}
