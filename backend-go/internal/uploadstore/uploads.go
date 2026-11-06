// Package uploadstore owns bounded, private staging of bytes a browser sends now
// and a LATER request consumes.
//
// # The request form is the shipped renderer's, not a new one
//
// src/components/BrowserDialogs.tsx:88-99 is the only browser uploader in this
// repo, and it sends exactly two shapes:
//
//	POST /api/uploads         body = the raw file bytes, Content-Type
//	                          application/octet-stream, and the URI-encoded name
//	                          in X-Upload-Name plus, for a folder member,
//	                          X-Upload-Folder and X-Upload-Relative
//	POST /api/upload-folders  body = a JSON object, answering {folderUploadId}
//
// The 201 answers it decodes are {uploadId}, {uploadId,name,size} and
// {folderUploadId}; the reference gateway answers the same
// (web/server.mjs:478-509). Nothing here accepts a multipart form, a query
// parameter, or a host path: the bytes are the body and the only metadata is the
// three headers above.
//
// # Why this store exists beside internal/transferhttp
//
// internal/transferhttp already stages browser uploads with this route, these
// headers and the same coarse bounds, and the synthetic lane mounts it
// (cmd/novel-server/main.go:425-437). It is not the owner this migration needs for
// one structural reason: a staged FOLDER is not enumerable outside that package.
// Its tree and its index are unexported (internal/transferhttp/uploads.go:44-70)
// and it lends one file by id (OpenUploadFile, :386-415), which answers
// UPLOAD_NOT_OWNED for a directory. Three served operations take a folder id and
// must read its members - kb:import-folder, dialog:select-knowledge-folder and
// skills:pick-local - so none of them can be served from it.
//
// # What is deliberately different, and smaller
//
// transferhttp materialises the request's relative name as nested directories
// under uploads/<id>/ and then defends that tree (symlink refusals, per-component
// inode checks, path-exists collisions). No consumer in this repo ever reads a
// staged file by path: they receive member names as LABELS and a knowledge
// document is named by one component. So every staged file here is ONE opaque blob
// named by a random id, and the member's relative name is an index field. No
// request value becomes a filesystem path at all, which removes the traversal and
// symlink surface instead of defending it.
//
// # Durable enough for the second request, bounded enough for the disk
//
// An upload id is durable across the two requests because the bytes live in a
// private directory on disk and the id -> (owner, name, relative, size) row lives
// in memory beside the cookie session that minted it. A process restart ends both
// halves together - security.NewSessions is in-memory too - so an id can never
// outlive its session and become authority (cf. internal/transport/server.go:53-57).
//
// The eviction rule, enforced in this order by reserveLocked:
//
//  1. every entry older than Limits.Lifetime is dropped first (expired staging is
//     worthless, so it is never the reason a live upload is refused);
//  2. then the OLDEST remaining entry is dropped, repeatedly, until the new bytes
//     fit under Limits.MaxTotalBytes and the new row fits under MaxEntries;
//  3. if the store cannot make room - one request larger than the whole budget, or
//     nothing left to drop - the request is refused with UPLOAD_STORAGE_LIMIT
//     before a byte is written.
//
// Eviction is global and oldest-first across callers, because the disk is one
// resource; a caller's own ceiling is the strict per-caller quota
// (MaxFiles/MaxCallerBytes), which is checked before anything else and is never
// evicted away to satisfy someone else. A dropped folder drops its members with
// it, so no member can outlive the folder it was staged under.
package uploadstore

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
)

// The two routes this store serves, exactly as the renderer requests them.
const (
	RouteFile   = "/api/uploads"
	RouteFolder = "/api/upload-folders"
)

// The request headers the renderer sets. The name and the relative label are
// encodeURIComponent output, so they are decoded with url.PathUnescape - the
// inverse of encodeURIComponent, and the same decode the reference gateway does.
const (
	headerName     = "X-Upload-Name"
	headerFolder   = "X-Upload-Folder"
	headerRelative = "X-Upload-Relative"
)

// The refusal codes this package answers. Each is symbolic and is also the
// message, because the renderer's own sentences are its job
// (src/components/BrowserDialogs.tsx:139-140).
const (
	codeUnavailable    = "UPLOAD_UNAVAILABLE"
	codeClientID       = "CLIENT_ID_REQUIRED"
	codeInvalidPath    = "INVALID_PATH"
	codeFolderNotOwned = "UPLOAD_FOLDER_NOT_OWNED"
	codeFileLimit      = "UPLOAD_FILE_LIMIT"
	codeFolderLimit    = "UPLOAD_FOLDER_LIMIT"
	codeSessionLimit   = "UPLOAD_SESSION_LIMIT"
	codeBodyTooLarge   = "BODY_TOO_LARGE"
	codeStorageLimit   = "UPLOAD_STORAGE_LIMIT"
	codeStorageFailed  = "UPLOAD_STORAGE_FAILED"
	codeRouteNotFound  = "API_ROUTE_NOT_FOUND"
)

// The two consumption refusals. An id staged for another cookie session or tab,
// an id evicted by the lifetime, and an id this process never minted are ONE
// answer: telling them apart would let a caller probe another tab's staging.
// ErrUnavailable is the different thing - the store itself cannot be read, which
// is what a closed store or a swapped staging directory answers.
var (
	ErrUnknown     = errors.New("UPLOAD_STORE_UNKNOWN")
	ErrUnavailable = errors.New("UPLOAD_STORE_UNAVAILABLE")
)

// Limits are the bounds one store enforces. Every field is a maximum; the zero
// value is unusable and DefaultLimits is the shipped set.
type Limits struct {
	// MaxFileBytes bounds one request body, and one staged blob.
	MaxFileBytes int64
	// MaxCallerBytes bounds everything one cookie session and tab may hold.
	MaxCallerBytes int64
	// MaxFiles and MaxFolders bound the rows one caller may hold.
	MaxFiles   int
	MaxFolders int
	// MaxFolderMembers and MaxFolderBytes bound one staged folder.
	MaxFolderMembers int
	MaxFolderBytes   int64
	// MaxDepth, MaxNameBytes and MaxRelativeBytes bound a member label: the
	// same shape productionPickerUploadMember accepts, so this store can never
	// mint a folder the picker then refuses.
	MaxDepth         int
	MaxNameBytes     int
	MaxRelativeBytes int
	// MaxTotalBytes and MaxEntries bound the whole store: the disk guard.
	MaxTotalBytes int64
	MaxEntries    int
	// Lifetime is when a staged upload stops being readable.
	Lifetime time.Duration
}

// DefaultLimits is the shipped browser's budget: the same per-file, per-caller and
// per-folder bounds the pickers advertise (production_browser_pickers_linux.go:
// 227-243), plus the global disk guard those per-caller bounds cannot express (32
// tabs x 256 MiB would otherwise be 8 GiB of disposable data).
func DefaultLimits() Limits {
	return Limits{
		MaxFileBytes:     64 << 20,
		MaxCallerBytes:   256 << 20,
		MaxFiles:         1024,
		MaxFolders:       32,
		MaxFolderMembers: 1024,
		MaxFolderBytes:   32 << 20,
		MaxDepth:         8,
		MaxNameBytes:     255,
		MaxRelativeBytes: 1024,
		MaxTotalBytes:    512 << 20,
		MaxEntries:       4096,
		Lifetime:         time.Hour,
	}
}

// valid reports whether the limits can be enforced at all. A zero or negative
// value would turn a bound into "refuse everything" or "bound nothing", so it is
// a construction error instead.
func (l Limits) valid() bool {
	return l.MaxFileBytes > 0 && l.MaxCallerBytes > 0 && l.MaxFiles > 0 && l.MaxFolders > 0 &&
		l.MaxFolderMembers > 0 && l.MaxFolderBytes > 0 && l.MaxDepth > 0 && l.MaxNameBytes > 0 &&
		l.MaxRelativeBytes > 0 && l.MaxTotalBytes > 0 && l.MaxEntries > 0 && l.Lifetime > 0
}

// ResolveClient must return the cookie session and the browser-tab identity the
// operation gateway uses, and must enforce its tab cap. The production lane passes
// transport.Server.ResolveTransferClient (internal/transport/server.go:61-69), so
// an upload is admitted by the same credential path as every operation and never
// by a second one.
type ResolveClient func(http.ResponseWriter, *http.Request) (*security.Session, string, error)

// Member is one file of a staged folder. Relative is a LABEL - already validated,
// never resolved against any filesystem.
type Member struct {
	Relative string
	Data     []byte
}

// entry is one staged row. It is not a path: id names one opaque blob under the
// store's private root, and nothing else here ever reaches a filesystem.
type entry struct {
	id        string
	owner     string
	name      string
	relative  string
	folder    string
	directory bool
	size      int64
	created   time.Time
}

// Store is bounded, private staging under a directory the caller owns. No request
// value can choose this root; New refuses one that is not already a private
// directory owned by this process.
type Store struct {
	mu       sync.Mutex
	limits   Limits
	root     *os.Root
	rootPath string
	info     os.FileInfo
	sessions *security.Sessions
	origins  security.Origins
	resolve  ResolveClient
	entries  map[string]*entry
	now      func() time.Time
	closed   bool
}

// New opens already-private staging at directory. It creates nothing: the owner
// decides where disposable bytes may live, and a directory that does not exist,
// is a symlink, or is readable by anyone else is refused rather than repaired.
func New(directory string, sessions *security.Sessions, origins security.Origins,
	resolve ResolveClient, limits Limits) (*Store, error) {
	if sessions == nil || resolve == nil || !limits.valid() ||
		!filepath.IsAbs(directory) || filepath.Clean(directory) != directory {
		return nil, errors.New("UPLOAD_STORE_CONFIGURATION_INVALID")
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
	return &Store{limits: limits, root: root, rootPath: directory, info: info, sessions: sessions,
		origins: origins, resolve: resolve, entries: make(map[string]*entry), now: time.Now}, nil
}

// privateDirectory is one owner-private directory that is not a symlink. The same
// rule the staging root of internal/transferhttp uses (:101-107).
func privateDirectory(info os.FileInfo) bool {
	if info == nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 || info.Mode()&os.ModeSymlink != 0 {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid())
}

// Close releases the pinned root and drops every staged row. The blobs are
// disposable, so the store removes them: a restart begins with an empty staging
// directory, never with bytes no session can ever claim.
func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	s.entries = make(map[string]*entry)
	return s.root.Close()
}

// ServeHTTP serves the two upload routes.
func (s *Store) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := s.serve(w, r); err != nil {
		_ = security.WriteError(w, err)
	}
}

func (s *Store) serve(w http.ResponseWriter, r *http.Request) error {
	if s == nil || r == nil {
		return security.NewHTTPError(http.StatusServiceUnavailable, codeUnavailable)
	}
	if r.Method != http.MethodPost || (r.URL.Path != RouteFile && r.URL.Path != RouteFolder) {
		return security.NewHTTPError(http.StatusNotFound, codeRouteNotFound)
	}
	// The credential and CSRF path is the gateway's own, borrowed rather than
	// reimplemented: the resolver admits the cookie session and the tab (with its
	// 32-tab cap) and Verify then enforces Origin, Sec-Fetch-Site and X-CSRF-Token.
	session, client, err := s.resolve(w, r)
	if err != nil {
		return err
	}
	owner, ok := ownerKey(session, client)
	if !ok {
		return security.NewHTTPError(http.StatusBadRequest, codeClientID)
	}
	if err := s.sessions.Verify(r, session, s.origins); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || !s.rootUnchanged() {
		return security.NewHTTPError(http.StatusServiceUnavailable, codeUnavailable)
	}
	s.expireLocked(s.now())
	if r.URL.Path == RouteFolder {
		return s.createFolder(w, r, owner)
	}
	return s.createFile(w, r, owner)
}

// ownerKey is the staging key: the cookie session and the tab, exactly as the
// operation gateway names its caller (transport.Caller{SessionID, ClientID}). Both
// halves are required, and neither may carry the separator.
func ownerKey(session *security.Session, client string) (string, bool) {
	if session == nil {
		return "", false
	}
	return ownerKeyByID(session.ID(), client)
}

// ownerKeyByID is ownerKey over the two strings a consumer carries, so a seam that
// never sees a *security.Session can still name its own key.
func ownerKeyByID(sessionID, clientID string) (string, bool) {
	if sessionID == "" || strings.ContainsRune(sessionID, 0) || !clientToken.MatchString(clientID) {
		return "", false
	}
	return sessionID + "\x00" + clientID, true
}

// clientToken is the gateway's own browser-tab format
// (internal/transport/server.go:78). It is repeated because it is unexported
// there; a client id this store accepted and that server refused would be a tab
// cap with a hole in it.
var clientToken = regexp.MustCompile(`^[a-zA-Z0-9_-]{16,128}$`)

// uploadID is the only shape a staged id can have, so a read never has to treat a
// request value as a path component.
var uploadID = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)

// rootUnchanged re-proves the staging directory after every request: a directory
// replaced under a running store must not accept or lend a byte.
func (s *Store) rootUnchanged() bool {
	now, err := os.Lstat(s.rootPath)
	if err != nil || !os.SameFile(now, s.info) || !privateDirectory(now) {
		return false
	}
	opened, err := s.root.Stat(".")
	return err == nil && os.SameFile(opened, s.info) && privateDirectory(opened)
}

// createFolder stages one folder row. It writes no bytes and creates nothing on
// disk: a folder is the label its members are grouped by, so a folder that is
// never used costs one bounded index row.
func (s *Store) createFolder(w http.ResponseWriter, r *http.Request, owner string) error {
	folders, _, _ := s.countsLocked(owner)
	if folders >= s.limits.MaxFolders {
		return security.NewHTTPError(http.StatusTooManyRequests, codeFolderLimit)
	}
	label, err := folderRequest(r, s.limits)
	if err != nil {
		return err
	}
	if err := s.reserveLocked(0); err != nil {
		return err
	}
	id, err := newID()
	if err != nil {
		return security.NewHTTPError(http.StatusServiceUnavailable, codeStorageFailed)
	}
	if label == "" {
		label = id
	}
	s.entries[id] = &entry{id: id, owner: owner, name: label, directory: true, created: s.now()}
	return security.WriteJSON(w, http.StatusCreated, map[string]any{"folderUploadId": id})
}

// folderRequest reads the folder route's JSON body and its optional label. The
// renderer sends {}, so the label is normally absent and the id is the display
// name; a client that does send one gets a sanitised single component, never a
// path.
func folderRequest(r *http.Request, limits Limits) (string, error) {
	value, err := security.JSONBody(r, 8<<20)
	if err != nil {
		return "", err
	}
	object, ok := value.(map[string]any)
	if !ok {
		return "", nil
	}
	raw, present := object["name"]
	if !present {
		return "", nil
	}
	text, ok := raw.(string)
	if !ok || len(text) > 4*limits.MaxNameBytes {
		return "", security.NewHTTPError(http.StatusBadRequest, codeInvalidPath)
	}
	name := strings.TrimSpace(text)
	if name == "" {
		return "", nil
	}
	return safeName(name, limits.MaxNameBytes), nil
}

// createFile stages one file: one blob plus one row. Every count, label, depth and
// quota bound runs before the first byte is written, and the byte bound is enforced
// by the reader itself, so a refused upload leaves no blob and no row.
func (s *Store) createFile(w http.ResponseWriter, r *http.Request, owner string) error {
	_, files, used := s.countsLocked(owner)
	if files >= s.limits.MaxFiles {
		return security.NewHTTPError(http.StatusTooManyRequests, codeFileLimit)
	}
	if used >= s.limits.MaxCallerBytes {
		return security.NewHTTPError(http.StatusRequestEntityTooLarge, codeSessionLimit)
	}
	name, err := requestName(r, s.limits)
	if err != nil {
		return err
	}
	folderID := ""
	if raw := r.Header.Get(headerFolder); raw != "" {
		folder := s.entries[raw]
		if folder == nil || !folder.directory || folder.owner != owner {
			return security.NewHTTPError(http.StatusForbidden, codeFolderNotOwned)
		}
		members, bytes := s.folderUsageLocked(folder.id)
		if members >= s.limits.MaxFolderMembers || bytes >= s.limits.MaxFolderBytes {
			return security.NewHTTPError(http.StatusTooManyRequests, codeFolderLimit)
		}
		folderID = folder.id
	}
	relative := name
	if folderID != "" {
		if raw := r.Header.Get(headerRelative); raw != "" {
			decoded, decodeErr := url.PathUnescape(raw)
			if decodeErr != nil || !utf8.ValidString(decoded) {
				return security.NewHTTPError(http.StatusBadRequest, codeInvalidPath)
			}
			relative = decoded
		}
	}
	if !memberLabel(relative, s.limits) {
		return security.NewHTTPError(http.StatusBadRequest, codeInvalidPath)
	}
	// Known size: the whole bound is decided before the store is touched.
	if r.ContentLength > s.limits.MaxFileBytes {
		return security.NewHTTPError(http.StatusRequestEntityTooLarge, codeBodyTooLarge)
	}
	if r.ContentLength > 0 {
		if used+r.ContentLength > s.limits.MaxCallerBytes {
			return security.NewHTTPError(http.StatusRequestEntityTooLarge, codeSessionLimit)
		}
		if folderID != "" {
			if _, bytes := s.folderUsageLocked(folderID); bytes+r.ContentLength > s.limits.MaxFolderBytes {
				return security.NewHTTPError(http.StatusTooManyRequests, codeFolderLimit)
			}
		}
	}
	reserve := r.ContentLength
	if reserve <= 0 {
		// A chunked body declares nothing, so the worst case is reserved: the
		// disk guard must hold for an upload that turns out to be the full bound.
		reserve = s.limits.MaxFileBytes
	}
	if err := s.reserveLocked(reserve); err != nil {
		return err
	}
	// Reservation may have dropped older rows of this same caller, so the per-caller
	// total is re-read rather than reused: the quota below must bound what is stored
	// now, not what was stored before the disk guard ran.
	_, _, used = s.countsLocked(owner)
	if folderID != "" {
		// The disk guard can evict the oldest row, which may be this very folder. A
		// member whose folder is gone would be unreachable, so the upload is refused
		// and the page stages the folder again.
		if folder := s.entries[folderID]; folder == nil || !folder.directory || folder.owner != owner {
			return security.NewHTTPError(http.StatusForbidden, codeFolderNotOwned)
		}
	}
	id, err := newID()
	if err != nil {
		return security.NewHTTPError(http.StatusServiceUnavailable, codeStorageFailed)
	}
	// A blob is written under a part name and renamed only once it is complete and
	// verified, so an interrupted upload can never be read as a staged one.
	part := id + ".part"
	file, err := s.root.OpenFile(part, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return security.NewHTTPError(http.StatusServiceUnavailable, codeStorageFailed)
	}
	n, copyErr := io.Copy(file, io.LimitReader(r.Body, s.limits.MaxFileBytes+1))
	if copyErr == nil {
		copyErr = file.Sync()
	}
	if closeErr := file.Close(); copyErr == nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		_ = s.root.Remove(part)
		return security.NewHTTPError(http.StatusServiceUnavailable, codeStorageFailed)
	}
	refuse := func(code string, status int) error {
		_ = s.root.Remove(part)
		return security.NewHTTPError(status, code)
	}
	if n > s.limits.MaxFileBytes {
		return refuse(codeBodyTooLarge, http.StatusRequestEntityTooLarge)
	}
	if used+n > s.limits.MaxCallerBytes {
		return refuse(codeSessionLimit, http.StatusRequestEntityTooLarge)
	}
	if folderID != "" {
		if _, bytes := s.folderUsageLocked(folderID); bytes+n > s.limits.MaxFolderBytes {
			return refuse(codeFolderLimit, http.StatusTooManyRequests)
		}
	}
	if s.totalBytesLocked()+n > s.limits.MaxTotalBytes {
		return refuse(codeStorageLimit, http.StatusInsufficientStorage)
	}
	if !s.rootUnchanged() {
		return refuse(codeUnavailable, http.StatusServiceUnavailable)
	}
	if err := s.root.Rename(part, id); err != nil {
		return refuse(codeStorageFailed, http.StatusServiceUnavailable)
	}
	info, err := s.root.Lstat(id)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() != n {
		_ = s.root.Remove(id)
		return security.NewHTTPError(http.StatusServiceUnavailable, codeStorageFailed)
	}
	s.entries[id] = &entry{id: id, owner: owner, name: name, relative: relative,
		folder: folderID, size: n, created: s.now()}
	return security.WriteJSON(w, http.StatusCreated, map[string]any{
		"uploadId": id, "name": name, "size": n,
	})
}

// Read lends one staged file's display name and bytes to an already authenticated
// operation. sessionID and clientID are the caller's own cookie session and tab,
// never a request argument, so another tab's staging is not addressable.
func (s *Store) Read(sessionID, clientID, id string) (string, []byte, error) {
	if s == nil {
		return "", nil, ErrUnavailable
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	staged, err := s.lookupLocked(sessionID, clientID, id, false)
	if err != nil {
		return "", nil, err
	}
	data, err := s.blobLocked(staged)
	if err != nil {
		return "", nil, err
	}
	return staged.name, data, nil
}

// ReadFolder lends one staged folder's display label and its members by label,
// sorted, with the same ownership rule as Read. A folder whose members exceed the
// byte budget on read is refused rather than partially delivered.
func (s *Store) ReadFolder(sessionID, clientID, folderID string) (string, []Member, error) {
	if s == nil {
		return "", nil, ErrUnavailable
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	folder, err := s.lookupLocked(sessionID, clientID, folderID, true)
	if err != nil {
		return "", nil, err
	}
	members := make([]*entry, 0, 8)
	for _, item := range s.entries {
		if item.folder == folder.id && item.owner == folder.owner {
			members = append(members, item)
		}
	}
	if len(members) > s.limits.MaxFolderMembers {
		return "", nil, ErrUnknown
	}
	sort.SliceStable(members, func(left, right int) bool {
		return members[left].relative < members[right].relative
	})
	result := make([]Member, 0, len(members))
	total := int64(0)
	for _, item := range members {
		data, readErr := s.blobLocked(item)
		if readErr != nil {
			return "", nil, readErr
		}
		total += int64(len(data))
		if total > s.limits.MaxFolderBytes {
			return "", nil, ErrUnknown
		}
		result = append(result, Member{Relative: item.relative, Data: data})
	}
	return folder.name, result, nil
}

// lookupLocked is the one ownership test both readers share.
func (s *Store) lookupLocked(sessionID, clientID, id string, folder bool) (*entry, error) {
	owner, ok := ownerKeyByID(sessionID, clientID)
	if !ok || !uploadID.MatchString(id) {
		return nil, ErrUnknown
	}
	if s.closed || !s.rootUnchanged() {
		return nil, ErrUnavailable
	}
	s.expireLocked(s.now())
	staged := s.entries[id]
	if staged == nil || staged.owner != owner || staged.directory != folder {
		return nil, ErrUnknown
	}
	return staged, nil
}

// blobLocked reads one staged blob after proving it is still the file this store
// wrote: a regular, non-symlink entry of exactly the recorded size.
func (s *Store) blobLocked(staged *entry) ([]byte, error) {
	before, err := s.root.Lstat(staged.id)
	if err != nil || !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 || before.Size() != staged.size {
		return nil, ErrUnknown
	}
	file, err := s.root.OpenFile(staged.id, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, ErrUnknown
	}
	data, readErr := io.ReadAll(io.LimitReader(file, s.limits.MaxFileBytes+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		return nil, ErrUnavailable
	}
	if int64(len(data)) != staged.size {
		return nil, ErrUnknown
	}
	return data, nil
}

// countsLocked is one caller's usage: its folders, its files and its bytes.
func (s *Store) countsLocked(owner string) (folders, files int, bytes int64) {
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
	return folders, files, bytes
}

// folderUsageLocked is one folder's member count and byte total. It is recomputed
// from the rows rather than cached on the folder, so it cannot drift.
func (s *Store) folderUsageLocked(folderID string) (members int, bytes int64) {
	for _, item := range s.entries {
		if item.folder == folderID {
			members++
			bytes += item.size
		}
	}
	return members, bytes
}

func (s *Store) totalBytesLocked() int64 {
	total := int64(0)
	for _, item := range s.entries {
		if !item.directory {
			total += item.size
		}
	}
	return total
}

// expireLocked applies rule 1 of the eviction rule: everything past its lifetime
// stops being readable, and its bytes are reclaimed. The expired rows are collected
// before any of them is dropped, because dropping a folder drops its members from
// the same map.
func (s *Store) expireLocked(now time.Time) {
	expired := make([]*entry, 0, 4)
	for _, item := range s.entries {
		if now.Sub(item.created) > s.limits.Lifetime {
			expired = append(expired, item)
		}
	}
	for _, item := range expired {
		if _, live := s.entries[item.id]; live {
			s.dropLocked(item)
		}
	}
}

// reserveLocked applies rules 2 and 3: make room for want bytes and one row, or
// refuse before anything is written.
func (s *Store) reserveLocked(want int64) error {
	if want < 0 {
		want = 0
	}
	if want > s.limits.MaxTotalBytes {
		return security.NewHTTPError(http.StatusInsufficientStorage, codeStorageLimit)
	}
	for {
		if s.totalBytesLocked()+want <= s.limits.MaxTotalBytes && len(s.entries)+1 <= s.limits.MaxEntries {
			return nil
		}
		oldest := s.oldestRootLocked()
		if oldest == nil {
			return security.NewHTTPError(http.StatusInsufficientStorage, codeStorageLimit)
		}
		s.dropLocked(oldest)
	}
}

// oldestRootLocked is the eviction candidate: the oldest row that is not a folder
// member, so dropping it never leaves an orphaned member behind.
func (s *Store) oldestRootLocked() *entry {
	var oldest *entry
	for _, item := range s.entries {
		if item.folder != "" {
			continue
		}
		if oldest == nil || item.created.Before(oldest.created) ||
			(item.created.Equal(oldest.created) && item.id < oldest.id) {
			oldest = item
		}
	}
	return oldest
}

// dropLocked removes one row and its blob; a folder takes its members with it.
func (s *Store) dropLocked(item *entry) {
	if item == nil {
		return
	}
	if item.directory {
		for id, member := range s.entries {
			if member.folder == item.id {
				_ = s.root.Remove(member.id)
				delete(s.entries, id)
			}
		}
	} else {
		_ = s.root.Remove(item.id)
	}
	delete(s.entries, item.id)
}

// requestName decodes and sanitises the one display name a file upload carries.
// The default is the reference gateway's default (web/server.mjs:489).
func requestName(r *http.Request, limits Limits) (string, error) {
	raw := r.Header.Get(headerName)
	if raw == "" {
		raw = "document.txt"
	}
	decoded, err := url.PathUnescape(raw)
	if err != nil || !utf8.ValidString(decoded) {
		return "", security.NewHTTPError(http.StatusBadRequest, codeInvalidPath)
	}
	return safeName(decoded, limits.MaxNameBytes), nil
}

// safeName is one filename component: NFC, no separator, no control byte, no
// leading or trailing dot or space, bounded in bytes. The same rule the staging
// root of internal/transferhttp applies (:429-453), repeated because it is
// unexported there.
func safeName(value string, maxBytes int) string {
	value = norm.NFC.String(value)
	var out strings.Builder
	for _, char := range value {
		if char < 32 || char == 127 || strings.ContainsRune(`<>:"/\|?*`, char) {
			char = '_'
		}
		out.WriteRune(char)
	}
	clean := strings.Trim(out.String(), ". ")
	out.Reset()
	written := 0
	for _, char := range clean {
		width := utf8.RuneLen(char)
		if width < 0 {
			continue
		}
		if written+width > maxBytes {
			break
		}
		out.WriteRune(char)
		written += width
	}
	result := strings.Trim(out.String(), ". ")
	if result == "" {
		return "document.txt"
	}
	return result
}

// memberLabel bounds a folder member's relative name: valid UTF-8, relative, no
// backslash or NUL, bounded depth, bounded name bytes, bounded total bytes, and no
// empty, "." or ".." component. This is productionPickerUploadMember's rule
// (production_browser_pickers_linux.go:1581-1603), so the store and the picker
// cannot disagree about what an acceptable member is.
func memberLabel(value string, limits Limits) bool {
	if value == "" || len(value) > limits.MaxRelativeBytes || !utf8.ValidString(value) {
		return false
	}
	if strings.HasPrefix(value, "/") || strings.ContainsAny(value, "\\\x00") {
		return false
	}
	parts := strings.Split(value, "/")
	if len(parts) > limits.MaxDepth {
		return false
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || len(part) > limits.MaxNameBytes {
			return false
		}
		for _, char := range part {
			if char < 0x20 || char == 0x7f {
				return false
			}
		}
	}
	return true
}

// newID is the staged handle: 128 bits from the kernel, formatted as a UUID v4.
func newID() (string, error) {
	var raw [16]byte
	if _, err := io.ReadFull(rand.Reader, raw[:]); err != nil {
		return "", err
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	text := hex.EncodeToString(raw[:])
	return text[:8] + "-" + text[8:12] + "-" + text[12:16] + "-" + text[16:20] + "-" + text[20:], nil
}
