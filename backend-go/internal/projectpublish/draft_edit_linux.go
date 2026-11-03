//go:build linux

package projectpublish

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"unicode/utf8"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
)

const maxPrivateDraftContentBytes = 16 << 20
const maxPrivateDraftSafeInteger int64 = 9007199254740991

// DraftEditEvidence describes a CAS committed only to a private staged copy.
// It cannot authorize replacing the author's storage directory.
type DraftEditEvidence struct {
	DraftID               int64
	WordCount             int64
	SourceTreeSHA256      [32]byte
	BeforeStageTreeSHA256 [32]byte
	AfterStageTreeSHA256  [32]byte
	BeforeContentSHA256   [32]byte
	AfterContentSHA256    [32]byte
	projectID             string
	projectPath           string
	stagePath             string
	changedSQLite         []string
	operationHash         [32]byte
	readbackHash          [32]byte
}

func (DraftEditEvidence) PublicationAuthority(context.Context) error { return ErrPublishUnavailable }

// VerifyEditedDraftCAS is a trusted internal proof adapter for a journal that
// independently scans this same private stage. The proof is derived from the
// CAS inputs and a fresh SQLite readback, never from browser-provided hashes.
func (e DraftEditEvidence) VerifyEditedDraftCAS(ctx context.Context, facts EditedStageFacts) (EditedDraftCASProof, error) {
	if ctx == nil || e.DraftID <= 0 || e.projectID == "" || e.projectPath == "" || e.stagePath == "" {
		return EditedDraftCASProof{}, ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return EditedDraftCASProof{}, err
	}
	if facts.ProjectID != e.projectID || facts.ProjectPath != e.projectPath || facts.StagePath != e.stagePath ||
		facts.SourceTreeHash != hex.EncodeToString(e.SourceTreeSHA256[:]) ||
		facts.EditedTreeHash != hex.EncodeToString(e.AfterStageTreeSHA256[:]) ||
		!sameChangedPaths(facts.ChangedSQLite, e.changedSQLite) {
		return EditedDraftCASProof{}, ErrChanged
	}
	return EditedDraftCASProof{DraftID: e.DraftID,
		OperationHash: hex.EncodeToString(e.operationHash[:]),
		ReadbackHash:  hex.EncodeToString(e.readbackHash[:])}, nil
}

// EditDraftCASPrivate applies the existing transactional draft CAS to the
// disposable private copy only. The caller must retain the legacy lock and
// Snapshot. A failure can leave an uncertain private stage, which must be
// discarded rather than published or retried as if it were pristine.
func (s *Snapshot) EditDraftCASPrivate(ctx context.Context, draftID int64, content string, wordCount int64, expectedContent *string) (DraftEditEvidence, error) {
	var empty DraftEditEvidence
	if s == nil || ctx == nil || draftID <= 0 || draftID > maxPrivateDraftSafeInteger ||
		wordCount < 0 || wordCount > maxPrivateDraftSafeInteger || expectedContent == nil ||
		!utf8.ValidString(content) || !utf8.ValidString(*expectedContent) ||
		len(content) > maxPrivateDraftContentBytes || len(*expectedContent) > maxPrivateDraftContentBytes {
		return empty, ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.removed || s.lock == nil || s.parent == nil || s.library == nil {
		return empty, ErrInvalid
	}
	if err := verifyExchangeRootsPrivate(s); err != nil {
		return empty, err
	}
	if err := s.verifySourceForDraftEditLocked(ctx); err != nil {
		return empty, err
	}
	stageRoot, before, err := s.scanPrivateForDraftEditLocked(ctx)
	if err != nil {
		return empty, err
	}
	defer stageRoot.Close()
	if !sameEntries(s.entries, before) {
		return empty, ErrChanged
	}
	beforeDB, err := stageRoot.Lstat(projectfs.DatabaseName)
	if err != nil || !beforeDB.Mode().IsRegular() || !oneLink(beforeDB) {
		return empty, ErrInvalid
	}
	beforeDigest := digestDraftTree(before)
	dbPath := filepath.Join(s.stage, projectfs.DatabaseName)
	if err := draftStageDBPathCurrent(stageRoot, dbPath, beforeDB); err != nil {
		return empty, err
	}
	writeDB, err := openPrivateDraftDB(dbPath, "rw")
	if err != nil {
		return empty, err
	}
	writeErr := store.NewDrafts(writeDB).UpdateContentIfCurrent(ctx, draftID, content, wordCount, expectedContent)
	closeErr := writeDB.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return empty, err
	}
	if err := draftStageDBPathCurrent(stageRoot, dbPath, beforeDB); err != nil {
		return empty, err
	}
	// Open a fresh connection after commit and close, so a transaction-local
	// cache cannot masquerade as persisted private-copy content.
	readDB, err := openPrivateDraftDB(dbPath, "ro")
	if err != nil {
		return empty, err
	}
	var actual string
	var actualWords int64
	readErr := readDB.QueryRowContext(ctx, `SELECT c.body, d.word_count FROM drafts d JOIN contents c ON c.id = d.content_id WHERE d.id = ?`, draftID).Scan(&actual, &actualWords)
	readCloseErr := readDB.Close()
	if err := errors.Join(readErr, readCloseErr); err != nil {
		return empty, err
	}
	if actual != content || actualWords != wordCount {
		return empty, ErrChanged
	}
	if err := draftStageDBPathCurrent(stageRoot, dbPath, beforeDB); err != nil {
		return empty, err
	}
	after, err := scan(ctx, stageRoot, s.limits)
	if err != nil {
		return empty, err
	}
	changed, err := editedSQLiteOnly(before, after)
	if err != nil {
		return empty, err
	}
	if err := verifyExchangeRootsPrivate(s); err != nil {
		return empty, err
	}
	if err := s.verifySourceForDraftEditLocked(ctx); err != nil {
		return empty, err
	}
	afterDigest := digestDraftTree(after)
	sourceDigest := digestDraftTree(s.entries)
	return DraftEditEvidence{
		DraftID: draftID, WordCount: wordCount,
		SourceTreeSHA256:      sourceDigest,
		BeforeStageTreeSHA256: beforeDigest,
		AfterStageTreeSHA256:  afterDigest,
		BeforeContentSHA256:   sha256.Sum256([]byte(*expectedContent)),
		AfterContentSHA256:    sha256.Sum256([]byte(content)),
		projectID:             s.projectID, projectPath: s.projectPath, stagePath: s.stage,
		changedSQLite: append([]string(nil), changed...),
		operationHash: privateDraftOperationHash(draftID, wordCount, *expectedContent, content, sourceDigest, beforeDigest),
		readbackHash:  privateDraftReadbackHash(draftID, actualWords, actual, afterDigest),
	}, nil
}

func (s *Snapshot) verifySourceForDraftEditLocked(ctx context.Context) error {
	if err := s.lock.VerifyLegacy(s.dataRoot); err != nil {
		return err
	}
	project, err := s.library.OpenProject(s.projectPath)
	if err != nil {
		return err
	}
	defer project.Close()
	if project.ProjectID() != s.projectID || !project.Identity().Same(s.identity) {
		return ErrChanged
	}
	if err := project.VerifyNoPortableAuthority(ctx); err != nil {
		return err
	}
	root, err := os.OpenRoot(filepath.Join(s.projectPath, projectfs.StorageDirectory))
	if err != nil {
		return err
	}
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(opened, s.storageInfo) {
		_ = root.Close()
		return ErrChanged
	}
	entries, scanErr := scan(ctx, root, s.limits)
	closeErr := root.Close()
	if err := errors.Join(scanErr, closeErr); err != nil {
		return err
	}
	if !sameEntries(entries, s.entries) {
		return ErrChanged
	}
	return s.lock.VerifyLegacy(s.dataRoot)
}

func (s *Snapshot) scanPrivateForDraftEditLocked(ctx context.Context) (*os.Root, []entry, error) {
	parent, err := os.Lstat(s.parentPath)
	if err != nil || !os.SameFile(parent, s.parentInfo) {
		return nil, nil, ErrChanged
	}
	root, err := s.parent.OpenRoot(s.stageName)
	if err != nil {
		return nil, nil, err
	}
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(opened, s.stageInfo) {
		_ = root.Close()
		return nil, nil, ErrChanged
	}
	entries, err := scan(ctx, root, s.limits)
	if err != nil {
		_ = root.Close()
		return nil, nil, err
	}
	return root, entries, nil
}

func draftStageDBPathCurrent(root *os.Root, path string, pinned fs.FileInfo) error {
	inside, err := root.Lstat(projectfs.DatabaseName)
	if err != nil || !inside.Mode().IsRegular() || !os.SameFile(inside, pinned) {
		return ErrChanged
	}
	visible, err := os.Lstat(path)
	if err != nil || !visible.Mode().IsRegular() || !os.SameFile(visible, pinned) {
		return ErrChanged
	}
	return nil
}

func openPrivateDraftDB(path, mode string) (*sql.DB, error) {
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	query := url.Values{}
	query.Set("mode", mode)
	query.Set("_foreign_keys", "on")
	uri.RawQuery = query.Encode()
	db, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	return db, nil
}

func privateDraftOperationHash(id, count int64, before, after string, source, stage [32]byte) [32]byte {
	h := sha256.New()
	_, _ = h.Write([]byte("private-draft-cas-operation:v1\x00"))
	writeDraftNumber(h, id)
	writeDraftNumber(h, count)
	writeDraftText(h, before)
	writeDraftText(h, after)
	_, _ = h.Write(source[:])
	_, _ = h.Write(stage[:])
	var result [32]byte
	copy(result[:], h.Sum(nil))
	return result
}

func privateDraftReadbackHash(id, count int64, actual string, stage [32]byte) [32]byte {
	h := sha256.New()
	_, _ = h.Write([]byte("private-draft-cas-readback:v1\x00"))
	writeDraftNumber(h, id)
	writeDraftNumber(h, count)
	writeDraftText(h, actual)
	_, _ = h.Write(stage[:])
	var result [32]byte
	copy(result[:], h.Sum(nil))
	return result
}

func writeDraftNumber(h interface{ Write([]byte) (int, error) }, value int64) {
	var buffer [8]byte
	binary.BigEndian.PutUint64(buffer[:], uint64(value))
	_, _ = h.Write(buffer[:])
}

func writeDraftText(h interface{ Write([]byte) (int, error) }, value string) {
	writeDraftNumber(h, int64(len(value)))
	_, _ = h.Write([]byte(value))
}

func digestDraftTree(entries []entry) [32]byte {
	h := sha256.New()
	var length [8]byte
	for _, item := range entries {
		binary.BigEndian.PutUint64(length[:], uint64(len(item.path)))
		_, _ = h.Write(length[:])
		_, _ = h.Write([]byte(item.path))
		binary.BigEndian.PutUint64(length[:], uint64(item.size))
		_, _ = h.Write(length[:])
		binary.BigEndian.PutUint64(length[:], uint64(item.mode))
		_, _ = h.Write(length[:])
		_, _ = h.Write(item.hash[:])
	}
	var result [32]byte
	copy(result[:], h.Sum(nil))
	return result
}
