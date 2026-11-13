// Package cloudbackup is the local, server-owned backup store for the
// cloud-backup family: an append-only collection of generations, each one a
// portable archive plus the two metadata documents that prove it.
//
// It is deliberately NOT a remote client. The wire protocol it implements is the
// same v1 generation protocol internal/webdav speaks over DAV - a manifest.json
// naming the archive's SHA-256 and byte size, and a completion.json sealing the
// manifest - so that a future remote destination is a second implementation of
// this interface rather than a second format. The metadata codec, the strict
// parsers and the sibling catalogue are internal/webdav's own, not copies.
//
// # What this package does not own
//
// No credential, no endpoint, no egress, no project, no author database and no
// publication authority. It receives an already verified portable archive as a
// stream and gives back bytes it has proved byte for byte. A caller that wants to
// install those bytes into an author project needs the project lane's own
// admission; this package never opens a project and never writes one.
//
// # Why a store failure is never a silent success
//
// Publish does not report a generation until it has re-read the published
// generation from disk and re-proved, from the published copies, the completion's
// manifest digest, the manifest's archive digest and the archive's byte count.
// A byte that cannot be re-proved is a refusal, not a receipt.
package cloudbackup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/webdav"
)

// Refusals are symbolic and finite: no host path, no remote body and no
// transport sentence is ever wrapped into one, because a caller may print it.
var (
	// ErrInput refuses a malformed id, a missing stream or an impossible size.
	ErrInput = errors.New("CLOUD_BACKUP_STORE_INPUT_INVALID")
	// ErrNotFound refuses a book or generation this store does not hold.
	ErrNotFound = errors.New("CLOUD_BACKUP_STORE_NOT_FOUND")
	// ErrCorrupt refuses a generation whose bytes do not prove its own metadata.
	ErrCorrupt = errors.New("CLOUD_BACKUP_STORE_CORRUPT")
	// ErrTooLarge refuses an archive or metadata document above the budget.
	ErrTooLarge = errors.New("CLOUD_BACKUP_STORE_TOO_LARGE")
	// ErrUnsafe refuses a store root or entry that is not the physical directory
	// this package created: a symlink, a device, a file where a directory belongs.
	ErrUnsafe = errors.New("CLOUD_BACKUP_STORE_UNSAFE")
	// ErrClosed refuses every call on a nil or closed store.
	ErrClosed = errors.New("CLOUD_BACKUP_STORE_CLOSED")
	// ErrCancelled refuses a call whose context ended.
	ErrCancelled = errors.New("CLOUD_BACKUP_STORE_CANCELLED")
)

const (
	// collectionDirectory is the one collection name inside a book directory.
	collectionDirectory = "generations"
	// archiveName, manifestName and completionName are the installed names of one
	// generation. The archive name is fixed because webdav.Manifest's own parser
	// admits only "archive.ainovel".
	archiveName    = "archive.ainovel"
	manifestName   = "manifest.json"
	completionName = "completion.json"
	// stagingPrefix marks the private, unpublished directory Publish writes into
	// before one atomic rename. A leftover is never listed.
	stagingPrefix = ".staging-"
	// directoryMode and fileMode are the store's own modes: the store is private
	// server state, not something a browser reads directly.
	directoryMode os.FileMode = 0o700
	fileMode      os.FileMode = 0o600
	// maxManifestBytes bounds one metadata document; the reference protocol's own
	// ceiling is 16 MiB (internal/webdav/client.go's MaxJSONBytes default is 1 MiB
	// and internal/archiveformat's manifest ceiling is 16 MiB). A generation
	// manifest is a handful of keys, so 1 MiB is generous and still finite.
	maxManifestBytes int64 = 1 << 20
	// maxGenerations bounds one listing, matching the browser chooser's own
	// library bound rather than an unbounded directory walk.
	maxGenerations = 4096
	// copyBufferBytes is the streaming buffer for both directions; it does not
	// scale with the archive.
	copyBufferBytes = 128 * 1024
)

// The two id grammars are internal/webdav's: a generation is a version-4 UUID and
// a book id is that package's own id pattern. Both are validated before an id is
// ever joined onto a path, so no id can name a parent directory.
var (
	generationPattern = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	bookPattern       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@+\-]{0,511}$`)
	digestPattern     = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

// Limits is the store's own budget. Zero fields select the defaults; a value
// outside the permitted range is refused at Open rather than clamped, because a
// silently widened budget is the failure mode this package exists to prevent.
type Limits struct {
	// MaxArchiveBytes bounds one archive.
	MaxArchiveBytes int64
	// MaxGenerations bounds one listing.
	MaxGenerations int
}

// DefaultLimits is the ceiling for a store that only ever holds portable copies
// of an author database: the same 64 MiB budget the production lane applies to
// one staged database (productionMaxDatabaseBytes).
func DefaultLimits() Limits { return Limits{MaxArchiveBytes: 64 << 20, MaxGenerations: maxGenerations} }

func (l Limits) resolved() (Limits, error) {
	defaults := DefaultLimits()
	if l.MaxArchiveBytes == 0 {
		l.MaxArchiveBytes = defaults.MaxArchiveBytes
	}
	if l.MaxGenerations == 0 {
		l.MaxGenerations = defaults.MaxGenerations
	}
	if l.MaxArchiveBytes <= 0 || l.MaxArchiveBytes > 16<<30 || l.MaxGenerations < 1 || l.MaxGenerations > maxGenerations {
		return Limits{}, ErrInput
	}
	return l, nil
}

// Store is one server-owned backup root. It is safe for concurrent use: every
// publish builds its own private staging directory and its one publication step is
// a rename, so two publishes of different generations never observe each other.
type Store struct {
	root   string
	limits Limits
}

// Open admits a store root. A missing root is created with mode 0700; an existing
// root must be a physical directory, never a symlink, because every path below it
// is derived from this one.
func Open(root string, limits Limits) (*Store, error) {
	if root == "" || strings.ContainsRune(root, '\x00') || !filepath.IsAbs(root) {
		return nil, ErrInput
	}
	resolved, err := limits.resolved()
	if err != nil {
		return nil, err
	}
	cleaned := filepath.Clean(root)
	if cleaned == string(filepath.Separator) {
		return nil, ErrUnsafe
	}
	info, statErr := os.Lstat(cleaned)
	switch {
	case errors.Is(statErr, os.ErrNotExist):
		if mkdirErr := os.MkdirAll(cleaned, directoryMode); mkdirErr != nil {
			return nil, mkdirErr
		}
		info, statErr = os.Lstat(cleaned)
	case statErr != nil:
		return nil, statErr
	}
	if statErr != nil {
		return nil, statErr
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, ErrUnsafe
	}
	return &Store{root: cleaned, limits: resolved}, nil
}

// Root reports the admitted store root. It is server-side only and must never be
// placed on a wire.
func (s *Store) Root() string {
	if s == nil {
		return ""
	}
	return s.root
}

// Directory reports the admitted directory one published generation lives in, so
// a caller inspecting the published files (a lane's durability proof, a diagnostic)
// asks this store instead of re-deriving the layout from Root. It refuses an id
// this store never published, and it is server-side only: like Root, the path must
// never be placed on a wire.
func (s *Store) Directory(bookID, generationID string) (string, error) {
	if s == nil {
		return "", ErrClosed
	}
	if generationID == "" {
		return "", ErrInput
	}
	return s.generationDirectory(bookID, generationID)
}

// PublishInput is one generation to write. The archive stream must be a stable
// snapshot: Publish hashes what it reads and refuses when the bytes do not match
// the declared digest and size, so a caller cannot publish a changing file.
type PublishInput struct {
	BookID              string
	GenerationID        string
	OriginProjectID     string
	SnapshotGeneration  string
	ParentGenerationIDs []string
	CreatedAt           string
	Archive             io.Reader
	ArchiveSHA256       string
	ArchiveByteSize     int64
}

// Publish writes one generation, proves it from the published bytes, and returns
// the catalogue entry. Nothing is reported until the readback succeeds.
func (s *Store) Publish(ctx context.Context, input PublishInput) (webdav.Generation, error) {
	var empty webdav.Generation
	if err := s.usable(ctx); err != nil {
		return empty, err
	}
	manifest := webdav.Manifest{
		Version:                    1,
		CloudBookID:                input.BookID,
		GenerationID:               input.GenerationID,
		ParentGenerationIDs:        append([]string{}, input.ParentGenerationIDs...),
		CreatedAt:                  input.CreatedAt,
		OriginProjectID:            input.OriginProjectID,
		PortableSnapshotGeneration: input.SnapshotGeneration,
		Archive: webdav.ArchiveDescriptor{
			File:     archiveName,
			SHA256:   input.ArchiveSHA256,
			ByteSize: input.ArchiveByteSize,
		},
	}
	if manifest.ParentGenerationIDs == nil {
		manifest.ParentGenerationIDs = []string{}
	}
	if input.Archive == nil || input.CreatedAt == "" {
		return empty, ErrInput
	}
	if input.ArchiveByteSize <= 0 || input.ArchiveByteSize > s.limits.MaxArchiveBytes {
		return empty, ErrTooLarge
	}
	// The manifest is validated by the protocol's own parser before a single byte
	// is written, so a malformed id or timestamp fails without touching the store.
	manifestBytes, encodeErr := json.Marshal(manifest)
	if encodeErr != nil {
		return empty, ErrInput
	}
	if int64(len(manifestBytes)) > maxManifestBytes {
		return empty, ErrTooLarge
	}
	parsed, parseErr := webdav.ParseManifest(manifestBytes)
	if parseErr != nil {
		return empty, ErrInput
	}
	bookDirectory, bookErr := s.bookDirectory(input.BookID, true)
	if bookErr != nil {
		return empty, bookErr
	}
	staging, stagingErr := os.MkdirTemp(bookDirectory, stagingPrefix)
	if stagingErr != nil {
		return empty, stagingErr
	}
	published := false
	defer func() {
		if !published {
			_ = os.RemoveAll(staging)
		}
	}()
	if err := ctx.Err(); err != nil {
		return empty, ErrCancelled
	}
	// 1. The archive, written exactly once, hashed while it is written.
	archivePath := filepath.Join(staging, archiveName)
	if err := s.writeStream(ctx, archivePath, input.Archive, parsed.Archive); err != nil {
		return empty, err
	}
	// 2. The manifest, whose digest the completion will seal.
	manifestPath := filepath.Join(staging, manifestName)
	if err := writeFile(manifestPath, manifestBytes); err != nil {
		return empty, err
	}
	manifestDigest := digestHex(manifestBytes)
	completion := webdav.Completion{
		Version:      1,
		CloudBookID:  input.BookID,
		GenerationID: input.GenerationID,
		Manifest: webdav.ManifestDescriptor{
			File:     manifestName,
			SHA256:   manifestDigest,
			ByteSize: int64(len(manifestBytes)),
		},
		Archive:     parsed.Archive,
		CompletedAt: input.CreatedAt,
	}
	completionBytes, encodeErr := json.Marshal(completion)
	if encodeErr != nil {
		return empty, ErrInput
	}
	if int64(len(completionBytes)) > maxManifestBytes {
		return empty, ErrTooLarge
	}
	if _, parseErr := webdav.ParseCompletion(completionBytes); parseErr != nil {
		return empty, ErrCorrupt
	}
	// 3. Completion LAST: it is the only object that claims the generation exists.
	if err := writeFile(filepath.Join(staging, completionName), completionBytes); err != nil {
		return empty, err
	}
	collection, collectionErr := s.collectionDirectory(input.BookID, true)
	if collectionErr != nil {
		return empty, collectionErr
	}
	target := filepath.Join(collection, input.GenerationID)
	if err := ctx.Err(); err != nil {
		return empty, ErrCancelled
	}
	if err := os.Rename(staging, target); err != nil {
		// A generation id that already exists is not overwritten: the append-only
		// collection keeps the first publication, and the caller's bytes were
		// already proved different or it would have reused the id.
		if _, statErr := os.Lstat(target); statErr == nil {
			return empty, ErrInput
		}
		return empty, err
	}
	published = true
	// 4. Prove the published copy. This is the readback: the same Verify a
	//    restore performs, over the renamed directory rather than the staging one.
	//    A published generation that cannot prove itself is removed again: the
	//    author must not be left holding a directory the store would later skip.
	if _, err := s.Verify(ctx, input.BookID, input.GenerationID); err != nil {
		_ = os.RemoveAll(target)
		return empty, err
	}
	return s.generation(ctx, input.BookID, input.GenerationID)
}

// Verify re-proves one generation from its published objects and returns its
// manifest. The order is the protocol's: completion, then manifest against the
// completion's digest, then the archive against the manifest's descriptor.
func (s *Store) Verify(ctx context.Context, bookID, generationID string) (webdav.Manifest, error) {
	var empty webdav.Manifest
	if err := s.usable(ctx); err != nil {
		return empty, err
	}
	if !bookPattern.MatchString(bookID) || !generationPattern.MatchString(generationID) {
		return empty, ErrInput
	}
	directory, directoryErr := s.generationDirectory(bookID, generationID)
	if directoryErr != nil {
		return empty, directoryErr
	}
	completionBytes, readErr := readBounded(filepath.Join(directory, completionName), maxManifestBytes)
	if readErr != nil {
		return empty, readErr
	}
	completion, parseErr := webdav.ParseCompletion(completionBytes)
	if parseErr != nil {
		return empty, ErrCorrupt
	}
	if completion.CloudBookID != bookID || completion.GenerationID != generationID ||
		completion.Archive.ByteSize > s.limits.MaxArchiveBytes ||
		completion.Manifest.ByteSize > maxManifestBytes {
		return empty, ErrCorrupt
	}
	manifestBytes, readErr := readBounded(filepath.Join(directory, manifestName), maxManifestBytes)
	if readErr != nil {
		return empty, readErr
	}
	if int64(len(manifestBytes)) != completion.Manifest.ByteSize || digestHex(manifestBytes) != completion.Manifest.SHA256 {
		return empty, ErrCorrupt
	}
	manifest, parseErr := webdav.ParseManifest(manifestBytes)
	if parseErr != nil {
		return empty, ErrCorrupt
	}
	if manifest.CloudBookID != bookID || manifest.GenerationID != generationID ||
		manifest.Archive.File != completion.Archive.File ||
		manifest.Archive.SHA256 != completion.Archive.SHA256 ||
		manifest.Archive.ByteSize != completion.Archive.ByteSize {
		return empty, ErrCorrupt
	}
	if err := s.verifyArchive(ctx, filepath.Join(directory, archiveName), manifest.Archive); err != nil {
		return empty, err
	}
	return manifest, nil
}

// CopyTo streams one verified archive into w. It proves the metadata first, then
// hashes the bytes as they leave the store: a caller that has received EOF and a
// nil error has received exactly the archive its manifest names. The caller still
// owns format validation and every publication decision; bytes on a stream are not
// authority to install them.
func (s *Store) CopyTo(ctx context.Context, bookID, generationID string, w io.Writer) (webdav.Manifest, error) {
	var empty webdav.Manifest
	if w == nil {
		return empty, ErrInput
	}
	manifest, err := s.Verify(ctx, bookID, generationID)
	if err != nil {
		return empty, err
	}
	directory, directoryErr := s.generationDirectory(bookID, generationID)
	if directoryErr != nil {
		return empty, directoryErr
	}
	file, openErr := openRegular(filepath.Join(directory, archiveName))
	if openErr != nil {
		return empty, openErr
	}
	defer file.Close()
	digest := sha256.New()
	checksum := &countingWriter{writer: io.MultiWriter(w, digest), limit: manifest.Archive.ByteSize}
	buffer := make([]byte, copyBufferBytes)
	for {
		if err := ctx.Err(); err != nil {
			return empty, ErrCancelled
		}
		read, readErr := file.Read(buffer)
		if read > 0 {
			if _, writeErr := checksum.Write(buffer[:read]); writeErr != nil {
				return empty, writeErr
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return empty, readErr
		}
	}
	if checksum.count != manifest.Archive.ByteSize || hex.EncodeToString(digest.Sum(nil)) != manifest.Archive.SHA256 {
		return empty, ErrCorrupt
	}
	return manifest, nil
}

// List returns the verified catalogue of one book. A generation that cannot prove
// its own bytes is omitted rather than reported: a listing that advertised an
// unprovable archive would invite the author to restore it.
func (s *Store) List(ctx context.Context, bookID string) ([]webdav.Generation, error) {
	if err := s.usable(ctx); err != nil {
		return nil, err
	}
	if !bookPattern.MatchString(bookID) {
		return nil, ErrInput
	}
	collection, err := s.collectionDirectory(bookID, false)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return []webdav.Generation{}, nil
		}
		return nil, err
	}
	directory, openErr := os.Open(collection)
	if openErr != nil {
		if errors.Is(openErr, os.ErrNotExist) {
			return []webdav.Generation{}, nil
		}
		return nil, openErr
	}
	names, readErr := directory.Readdirnames(s.limits.MaxGenerations + 1)
	closeErr := directory.Close()
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if len(names) > s.limits.MaxGenerations {
		return nil, ErrTooLarge
	}
	manifests := make([]webdav.Manifest, 0, len(names))
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return nil, ErrCancelled
		}
		if !generationPattern.MatchString(name) || strings.HasPrefix(name, stagingPrefix) {
			continue
		}
		info, statErr := os.Lstat(filepath.Join(collection, name))
		if statErr != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		manifest, verifyErr := s.Verify(ctx, bookID, name)
		if verifyErr != nil {
			if errors.Is(verifyErr, ErrCorrupt) || errors.Is(verifyErr, ErrNotFound) || errors.Is(verifyErr, ErrTooLarge) {
				continue
			}
			return nil, verifyErr
		}
		manifests = append(manifests, manifest)
	}
	return webdav.Catalog(manifests), nil
}

// generation returns the catalogue entry for one generation, with the siblings the
// rest of its parent set implies. It is a listing of one book over the already
// verified set, so it never reports a sibling it did not prove.
func (s *Store) generation(ctx context.Context, bookID, generationID string) (webdav.Generation, error) {
	var empty webdav.Generation
	entries, err := s.List(ctx, bookID)
	if err != nil {
		return empty, err
	}
	for _, entry := range entries {
		if entry.GenerationID == generationID {
			return entry, nil
		}
	}
	return empty, ErrNotFound
}

// usable refuses a nil or contextless call before any filesystem work.
func (s *Store) usable(ctx context.Context) error {
	if s == nil || s.root == "" {
		return ErrClosed
	}
	if ctx == nil {
		return ErrInput
	}
	if err := ctx.Err(); err != nil {
		return ErrCancelled
	}
	return nil
}

// bookDirectory admits (and optionally creates) one book directory, refusing any
// entry that is not a physical directory.
func (s *Store) bookDirectory(bookID string, create bool) (string, error) {
	if !bookPattern.MatchString(bookID) || strings.ContainsRune(bookID, os.PathSeparator) {
		return "", ErrInput
	}
	path := filepath.Join(s.root, bookID)
	return path, s.directory(path, create)
}

// collectionDirectory admits (and optionally creates) the generations collection.
func (s *Store) collectionDirectory(bookID string, create bool) (string, error) {
	book, err := s.bookDirectory(bookID, create)
	if err != nil {
		return "", err
	}
	path := filepath.Join(book, collectionDirectory)
	return path, s.directory(path, create)
}

// generationDirectory admits one existing generation directory.
func (s *Store) generationDirectory(bookID, generationID string) (string, error) {
	collection, err := s.collectionDirectory(bookID, false)
	if err != nil {
		return "", err
	}
	path := filepath.Join(collection, generationID)
	info, statErr := os.Lstat(path)
	if errors.Is(statErr, os.ErrNotExist) {
		return "", ErrNotFound
	}
	if statErr != nil {
		return "", statErr
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", ErrUnsafe
	}
	return path, nil
}

// directory creates a directory when asked and proves what is on disk either way.
func (s *Store) directory(path string, create bool) error {
	info, statErr := os.Lstat(path)
	if errors.Is(statErr, os.ErrNotExist) {
		if !create {
			return ErrNotFound
		}
		if mkdirErr := os.MkdirAll(path, directoryMode); mkdirErr != nil {
			return mkdirErr
		}
		info, statErr = os.Lstat(path)
	}
	if statErr != nil {
		return statErr
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return ErrUnsafe
	}
	return nil
}

// writeStream copies one stream to a new 0600 file and refuses bytes that do not
// match the descriptor the manifest declared.
func (s *Store) writeStream(ctx context.Context, path string, source io.Reader, expected webdav.ArchiveDescriptor) error {
	file, openErr := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fileMode)
	if openErr != nil {
		return openErr
	}
	digest := sha256.New()
	counter := &countingWriter{writer: io.MultiWriter(file, digest), limit: expected.ByteSize}
	buffer := make([]byte, copyBufferBytes)
	var copyErr error
	for {
		if err := ctx.Err(); err != nil {
			copyErr = ErrCancelled
			break
		}
		read, readErr := source.Read(buffer)
		if read > 0 {
			if _, writeErr := counter.Write(buffer[:read]); writeErr != nil {
				copyErr = writeErr
				break
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			copyErr = readErr
			break
		}
	}
	if syncErr := file.Sync(); copyErr == nil && syncErr != nil {
		copyErr = syncErr
	}
	if closeErr := file.Close(); copyErr == nil && closeErr != nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		return copyErr
	}
	if counter.count != expected.ByteSize || hex.EncodeToString(digest.Sum(nil)) != expected.SHA256 {
		return ErrCorrupt
	}
	return nil
}

// verifyArchive hashes one archive file from disk and compares it to the
// descriptor the manifest declared.
func (s *Store) verifyArchive(ctx context.Context, path string, expected webdav.ArchiveDescriptor) error {
	file, openErr := openRegular(path)
	if openErr != nil {
		return openErr
	}
	defer file.Close()
	digest := sha256.New()
	buffer := make([]byte, copyBufferBytes)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return ErrCancelled
		}
		read, readErr := file.Read(buffer)
		if read > 0 {
			total += int64(read)
			if total > expected.ByteSize || total > s.limits.MaxArchiveBytes {
				return ErrCorrupt
			}
			if _, writeErr := digest.Write(buffer[:read]); writeErr != nil {
				return writeErr
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return readErr
		}
	}
	if total != expected.ByteSize || hex.EncodeToString(digest.Sum(nil)) != expected.SHA256 {
		return ErrCorrupt
	}
	return nil
}

// countingWriter refuses to write past the declared size, so a stream that grew
// after its digest was taken is refused at the byte that proves it.
type countingWriter struct {
	writer io.Writer
	limit  int64
	count  int64
}

func (w *countingWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.limit-w.count {
		return 0, ErrTooLarge
	}
	written, err := w.writer.Write(p)
	w.count += int64(written)
	return written, err
}

// openRegular opens one existing regular file without following a symlink where
// the platform reports one, and refuses anything else.
func openRegular(path string) (*os.File, error) {
	info, statErr := os.Lstat(path)
	if errors.Is(statErr, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if statErr != nil {
		return nil, statErr
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, ErrUnsafe
	}
	return os.Open(path)
}

// readBounded reads one complete metadata document, refusing a file that is not a
// regular file or is larger than the protocol's ceiling.
func readBounded(path string, maximum int64) ([]byte, error) {
	file, openErr := openRegular(path)
	if openErr != nil {
		return nil, openErr
	}
	defer file.Close()
	data, readErr := io.ReadAll(io.LimitReader(file, maximum+1))
	if readErr != nil {
		return nil, readErr
	}
	if int64(len(data)) > maximum {
		return nil, ErrTooLarge
	}
	return data, nil
}

// writeFile creates one metadata document exclusively and durably.
func writeFile(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fileMode)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

// digestHex is the lower-case hexadecimal SHA-256 of one document.
func digestHex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
