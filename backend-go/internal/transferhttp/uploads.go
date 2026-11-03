// Package transferhttp owns bounded browser upload staging. Staged files are
// disposable private data; an upload ID is not authority to edit an author
// project. A later dialog must independently bind the ID to its caller.
package transferhttp

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
)

const (
	maxUploadFolders = 32
	maxUploadFiles   = 1024
	maxUploadBytes   = int64(64 << 20)
	maxClientBytes   = int64(256 << 20)
	uploadLifetime   = time.Hour
)

var clientToken = regexp.MustCompile(`^[a-zA-Z0-9_-]{16,128}$`)

// ResolveClient must return the same Sessions-owned cookie and browser tab
// identity used by the RPC gateway. It must enforce the gateway's 32-tab cap.
// TransferStore calls Verify on that session before creating either upload.
type ResolveClient func(http.ResponseWriter, *http.Request) (*security.Session, string, error)

type staged struct {
	owner     string
	base      string
	baseInfo  os.FileInfo
	relative  string
	name      string
	created   time.Time
	size      int64
	count     int
	directory bool
}

// TransferStore holds private staging under an already private server-owned
// directory. No request value can choose this root or an author-project path.
type TransferStore struct {
	mu       sync.Mutex
	root     *os.Root
	rootPath string
	info     os.FileInfo
	uploads  os.FileInfo
	sessions *security.Sessions
	origins  security.Origins
	resolve  ResolveClient
	entries  map[string]*staged
	now      func() time.Time
	closed   bool
}

func NewTransferStore(directory string, sessions *security.Sessions, origins security.Origins, resolve ResolveClient) (*TransferStore, error) {
	if sessions == nil || resolve == nil || !filepath.IsAbs(directory) || filepath.Clean(directory) != directory {
		return nil, errors.New("UPLOAD_STORAGE_INVALID")
	}
	info, err := os.Lstat(directory)
	if err != nil || !privateDirectory(info) {
		return nil, errors.New("UPLOAD_STORAGE_UNSAFE")
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, errors.New("UPLOAD_STORAGE_UNSAFE")
	}
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		_ = root.Close()
		return nil, errors.New("UPLOAD_STORAGE_CHANGED")
	}
	if err := root.Mkdir("uploads", 0700); err != nil && !errors.Is(err, os.ErrExist) {
		_ = root.Close()
		return nil, err
	}
	child, err := root.Lstat("uploads")
	if err != nil || !privateDirectory(child) {
		_ = root.Close()
		return nil, errors.New("UPLOAD_STORAGE_UNSAFE")
	}
	return &TransferStore{root: root, rootPath: directory, info: info, uploads: child, sessions: sessions, origins: origins, resolve: resolve, entries: make(map[string]*staged), now: time.Now}, nil
}

func privateDirectory(info os.FileInfo) bool {
	if info == nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 || info.Mode()&os.ModeSymlink != 0 {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid())
}

func (s *TransferStore) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return s.root.Close()
}

func (s *TransferStore) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := s.serve(w, r); err != nil {
		_ = security.WriteError(w, err)
	}
}

func (s *TransferStore) serve(w http.ResponseWriter, r *http.Request) error {
	if s == nil || r == nil {
		return security.NewHTTPError(http.StatusServiceUnavailable, "UPLOAD_UNAVAILABLE")
	}
	if r.Method != http.MethodPost || (r.URL.Path != "/api/upload-folders" && r.URL.Path != "/api/uploads") {
		return security.NewHTTPError(http.StatusNotFound, "API_ROUTE_NOT_FOUND")
	}
	session, client, err := s.resolve(w, r)
	if err != nil {
		return err
	}
	if session == nil || !clientToken.MatchString(client) {
		return security.NewHTTPError(http.StatusBadRequest, "CLIENT_ID_REQUIRED")
	}
	if err := s.sessions.Verify(r, session, s.origins); err != nil {
		return err
	}
	owner := session.ID() + ":" + client
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || !s.rootUnchanged() {
		return security.NewHTTPError(http.StatusServiceUnavailable, "UPLOAD_UNAVAILABLE")
	}
	s.pruneLocked()
	if r.URL.Path == "/api/upload-folders" {
		return s.createFolder(w, r, owner)
	}
	return s.createFile(w, r, owner)
}

func (s *TransferStore) rootUnchanged() bool {
	now, err := os.Lstat(s.rootPath)
	if err != nil || !os.SameFile(now, s.info) || !privateDirectory(now) {
		return false
	}
	uploads, err := s.root.Lstat("uploads")
	return err == nil && os.SameFile(uploads, s.uploads) && privateDirectory(uploads)
}

func (s *TransferStore) pruneLocked() {
	now := s.now()
	expiredBases := make(map[string]bool)
	for _, item := range s.entries {
		if now.Sub(item.created) > uploadLifetime {
			expiredBases[item.base] = true
		}
	}
	for id, item := range s.entries {
		if expiredBases[item.base] {
			delete(s.entries, id)
		}
	}
	for base := range expiredBases {
		// Descriptor-relative removal remains confined if a pathname is swapped.
		_ = s.root.RemoveAll(base)
	}
}

func (s *TransferStore) countsLocked(owner string) (folders, files int, bytes int64) {
	for _, item := range s.entries {
		if item.owner != owner {
			continue
		}
		if item.directory {
			folders++
		} else {
			files++
			bytes += item.size
		}
	}
	return
}

func (s *TransferStore) createFolder(w http.ResponseWriter, r *http.Request, owner string) error {
	folders, _, _ := s.countsLocked(owner)
	if folders >= maxUploadFolders {
		return security.NewHTTPError(http.StatusTooManyRequests, "UPLOAD_FOLDER_LIMIT")
	}
	if err := readFolderJSON(r); err != nil {
		return err
	}
	id, err := randomID()
	if err != nil {
		return err
	}
	base := "uploads/" + id
	if err := s.root.Mkdir(base, 0700); err != nil {
		return err
	}
	baseInfo, err := s.root.Lstat(base)
	if err != nil || !privateDirectory(baseInfo) {
		return security.NewHTTPError(http.StatusServiceUnavailable, "UPLOAD_UNAVAILABLE")
	}
	s.entries[id] = &staged{owner: owner, base: base, baseInfo: baseInfo, directory: true, created: s.now()}
	return security.WriteJSON(w, http.StatusCreated, map[string]any{"folderUploadId": id})
}

func readFolderJSON(r *http.Request) error {
	if !strings.EqualFold(strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0]), "application/json") {
		return security.NewHTTPError(http.StatusUnsupportedMediaType, "JSON_REQUIRED")
	}
	limited := io.LimitReader(r.Body, (8<<20)+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return err
	}
	if len(data) > 8<<20 {
		return security.NewHTTPError(http.StatusRequestEntityTooLarge, "BODY_TOO_LARGE")
	}
	var value any
	if json.Unmarshal(data, &value) != nil {
		return security.NewHTTPError(http.StatusBadRequest, "INVALID_JSON")
	}
	return nil
}

func (s *TransferStore) createFile(w http.ResponseWriter, r *http.Request, owner string) error {
	_, files, used := s.countsLocked(owner)
	if files >= maxUploadFiles {
		return security.NewHTTPError(http.StatusTooManyRequests, "UPLOAD_FILE_LIMIT")
	}
	nameRaw := r.Header.Get("X-Upload-Name")
	if nameRaw == "" {
		nameRaw = "document.txt"
	}
	nameDecoded, err := url.PathUnescape(nameRaw)
	if err != nil || !utf8.ValidString(nameDecoded) {
		return security.NewHTTPError(http.StatusBadRequest, "INVALID_PATH")
	}
	name := safeFilename(nameDecoded)
	folderID := r.Header.Get("X-Upload-Folder")
	var folder *staged
	if folderID != "" {
		folder = s.entries[folderID]
		if folder == nil || !folder.directory || folder.owner != owner {
			return security.NewHTTPError(http.StatusForbidden, "UPLOAD_FOLDER_NOT_OWNED")
		}
	}
	relative := name
	if folder != nil {
		if raw := r.Header.Get("X-Upload-Relative"); raw != "" {
			relative, err = url.PathUnescape(raw)
			if err != nil || !utf8.ValidString(relative) {
				return security.NewHTTPError(http.StatusBadRequest, "INVALID_PATH")
			}
		}
	}
	if !validRelative(relative) {
		return security.NewHTTPError(http.StatusBadRequest, "INVALID_PATH")
	}
	id, err := randomID()
	if err != nil {
		return err
	}
	base := "uploads/" + id
	if folder != nil {
		base = folder.base
		if !s.baseUnchanged(folder) {
			return security.NewHTTPError(http.StatusForbidden, "UPLOAD_FOLDER_NOT_OWNED")
		}
	} else if err := s.root.Mkdir(base, 0700); err != nil {
		return err
	}
	baseInfo, err := s.root.Lstat(base)
	if err != nil || !privateDirectory(baseInfo) {
		return security.NewHTTPError(http.StatusServiceUnavailable, "UPLOAD_UNAVAILABLE")
	}
	full := base + "/" + relative
	if err := s.mkdirsLocked(base, relative); err != nil {
		if folder == nil {
			_ = s.root.RemoveAll(base)
		}
		return err
	}
	file, err := s.root.OpenFile(full, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		if folder == nil {
			_ = s.root.RemoveAll(base)
		}
		return security.NewHTTPError(http.StatusBadRequest, "UPLOAD_PATH_EXISTS")
	}
	remove := true
	defer func() {
		if remove {
			_ = s.root.Remove(full)
			if folder == nil {
				_ = s.root.RemoveAll(base)
			}
		}
	}()
	n, copyErr := io.Copy(file, io.LimitReader(r.Body, maxUploadBytes+1))
	if copyErr == nil {
		copyErr = file.Sync()
	}
	if closeErr := file.Close(); copyErr == nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		return copyErr
	}
	if n > maxUploadBytes {
		return security.NewHTTPError(http.StatusRequestEntityTooLarge, "BODY_TOO_LARGE")
	}
	if used+n > maxClientBytes {
		return security.NewHTTPError(http.StatusRequestEntityTooLarge, "UPLOAD_SESSION_LIMIT")
	}
	if !s.rootUnchanged() {
		return security.NewHTTPError(http.StatusServiceUnavailable, "UPLOAD_UNAVAILABLE")
	}
	remove = false
	s.entries[id] = &staged{owner: owner, base: base, baseInfo: baseInfo, relative: relative, name: name, size: n, created: s.now()}
	if folder != nil {
		folder.size += n
		folder.count++
	}
	return security.WriteJSON(w, http.StatusCreated, map[string]any{"uploadId": id, "name": name, "size": n})
}

func (s *TransferStore) mkdirsLocked(base, relative string) error {
	parts := strings.Split(relative, "/")
	current := base
	for _, part := range parts[:len(parts)-1] {
		current += "/" + part
		if err := s.root.Mkdir(current, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
		info, err := s.root.Lstat(current)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
			return security.NewHTTPError(http.StatusForbidden, "SYMLINK_DENIED")
		}
	}
	return nil
}

func (s *TransferStore) baseUnchanged(entry *staged) bool {
	if entry == nil || entry.baseInfo == nil {
		return false
	}
	info, err := s.root.Lstat(entry.base)
	return err == nil && os.SameFile(info, entry.baseInfo) && privateDirectory(info)
}

func (s *TransferStore) parentsSafe(base, relative string) bool {
	parts := strings.Split(relative, "/")
	current := base
	for _, part := range parts[:len(parts)-1] {
		current += "/" + part
		info, err := s.root.Lstat(current)
		if err != nil || !privateDirectory(info) {
			return false
		}
	}
	return true
}

// OpenUploadFile lends a staged regular file to an already authenticated
// dialog or import controller. The caller must supply the exact cookie/tab
// owner, and must close the returned descriptor. It never returns a path.
func (s *TransferStore) OpenUploadFile(session *security.Session, client, id string) (*os.File, error) {
	if s == nil || session == nil || !clientToken.MatchString(client) || id == "" {
		return nil, security.NewHTTPError(http.StatusForbidden, "UPLOAD_NOT_OWNED")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || !s.rootUnchanged() {
		return nil, security.NewHTTPError(http.StatusServiceUnavailable, "UPLOAD_UNAVAILABLE")
	}
	s.pruneLocked()
	entry := s.entries[id]
	if entry == nil || entry.directory || entry.owner != session.ID()+":"+client || !s.baseUnchanged(entry) || !s.parentsSafe(entry.base, entry.relative) {
		return nil, security.NewHTTPError(http.StatusForbidden, "UPLOAD_NOT_OWNED")
	}
	name := entry.base + "/" + entry.relative
	before, err := s.root.Lstat(name)
	if err != nil || !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 || before.Size() != entry.size {
		return nil, security.NewHTTPError(http.StatusForbidden, "UPLOAD_NOT_OWNED")
	}
	file, err := s.root.Open(name)
	if err != nil {
		return nil, security.NewHTTPError(http.StatusForbidden, "UPLOAD_NOT_OWNED")
	}
	actual, err := file.Stat()
	if err != nil || !os.SameFile(before, actual) || actual.Size() != entry.size {
		_ = file.Close()
		return nil, security.NewHTTPError(http.StatusForbidden, "UPLOAD_NOT_OWNED")
	}
	return file, nil
}

func validRelative(value string) bool {
	if value == "" || filepath.IsAbs(value) || strings.ContainsAny(value, "\\\x00") {
		return false
	}
	for _, part := range strings.Split(value, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

func safeFilename(value string) string {
	value = norm.NFC.String(value)
	var out strings.Builder
	for _, r := range value {
		if r < 32 || r == 127 || strings.ContainsRune(`<>:"/\\|?*`, r) {
			r = '_'
		}
		out.WriteRune(r)
	}
	clean := strings.Trim(out.String(), ". ")
	out.Reset()
	units := 0
	for _, r := range clean {
		width := len(utf16.Encode([]rune{r}))
		if units+width > 160 {
			break
		}
		out.WriteRune(r)
		units += width
	}
	if result := out.String(); result != "" {
		return result
	}
	return "document"
}

func randomID() (string, error) {
	var raw [16]byte
	if _, err := io.ReadFull(rand.Reader, raw[:]); err != nil {
		return "", err
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	hexID := hex.EncodeToString(raw[:])
	return hexID[:8] + "-" + hexID[8:12] + "-" + hexID[12:16] + "-" + hexID[16:20] + "-" + hexID[20:], nil
}
