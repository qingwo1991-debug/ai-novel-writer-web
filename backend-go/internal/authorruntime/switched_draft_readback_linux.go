//go:build linux

package authorruntime

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"path/filepath"
	"unicode/utf8"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectaccess"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectpublish"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/writerlock"
)

var ErrSwitchedDraftUnverified = errors.New("AUTHOR_SWITCHED_DRAFT_UNVERIFIED")

const switchedDraftMaxSafeInteger int64 = 9007199254740991

// SwitchedReadbackCleanupError retains the unclosed private Reader so the
// coordinator can inspect cleanup while the legacy lock is retained. Losing
// this handle would make it unsafe to release the lock after a failed DB close.
type SwitchedReadbackCleanupError struct {
	Reader *Reader
	Cause  error
}

func (e *SwitchedReadbackCleanupError) Error() string { return "AUTHOR_SWITCHED_DRAFT_CLEANUP_FAILED" }
func (e *SwitchedReadbackCleanupError) Unwrap() error { return e.Cause }

// SwitchedDraftExpectation comes from the server-owned private CAS evidence,
// never from a browser or the journal being checked. The full body remains in
// memory only until this readback has finished.
type SwitchedDraftExpectation struct {
	ProjectID           string
	DraftID             int64
	Content             string
	WordCount           int64
	EditedTreeHash      string
	EditedOperationHash string
}

// SwitchedDraftReadback is an observation, not source publication or catalog
// clearance. The publisher must still durably confirm the exchange, install a
// new lease and handle crash recovery before acknowledging the browser save.
type SwitchedDraftReadback struct {
	ProjectID       string
	ProjectPath     string
	DraftID         int64
	WordCount       int64
	ContentSHA256   string
	EditedTreeHash  string
	ReadbackSHA256  string
	NewSourceDevice uint64
	NewSourceInode  uint64
}

// VerifySwitchedDraftUnderLock independently classifies the persisted journal,
// observes both exchanged trees and reads the newly visible draft through a
// fresh, admitted private SQLite copy. It deliberately does not call catalog
// methods: a recovery checker holds the catalog mutex and refuses to mint a
// new key until this readback is complete. No source database is opened through
// SQLite and no author data is written. The snapshot, library and held lock
// remain caller-owned; the temporary pinned project and Reader are closed here.
func VerifySwitchedDraftUnderLock(ctx context.Context, held *writerlock.Lock,
	library *projectfs.Library, snapshot *projectpublish.Snapshot,
	projectPath, stageParent, journalName string, config projectaccess.Config,
	expected SwitchedDraftExpectation) (result SwitchedDraftReadback, retErr error) {
	if ctx == nil || held == nil || library == nil || snapshot == nil ||
		expected.ProjectID == "" || expected.DraftID <= 0 || expected.DraftID > switchedDraftMaxSafeInteger ||
		expected.WordCount < 0 || expected.WordCount > switchedDraftMaxSafeInteger ||
		!utf8.ValidString(expected.Content) || len(expected.Content) > 16<<20 ||
		!readbackHashValid(expected.EditedTreeHash) || !readbackHashValid(expected.EditedOperationHash) ||
		config.ProjectPath != projectPath || config.LibraryRoot != library.Path() ||
		filepath.Dir(projectPath) != library.Path() {
		return result, ErrSwitchedDraftUnverified
	}
	if err := held.VerifyLegacy(config.DataRoot); err != nil {
		return result, errors.Join(ErrSwitchedDraftUnverified, err)
	}
	recovery, err := projectpublish.RecoverExchangeJournal(ctx, held, config.DataRoot,
		expected.ProjectID, projectPath, stageParent, journalName)
	if err != nil || recovery.Class != projectpublish.JournalSwitched ||
		recovery.EditedDraftID != expected.DraftID ||
		recovery.EditedOperationHash != expected.EditedOperationHash ||
		!readbackHashValid(recovery.EditedProofHash) {
		return result, errors.Join(ErrSwitchedDraftUnverified, err)
	}
	first, err := snapshot.RetainSwitched(ctx, expected.EditedTreeHash)
	if err != nil || first == nil || first.NewSourcePath != filepath.Join(projectPath, projectfs.StorageDirectory) ||
		first.OldSourcePath != recovery.StagePath || first.NewSourceDevice == 0 || first.NewSourceInode == 0 {
		return result, errors.Join(ErrSwitchedDraftUnverified, err)
	}
	project, err := library.OpenProject(projectPath)
	if err != nil {
		return result, errors.Join(ErrSwitchedDraftUnverified, err)
	}
	if project.ProjectID() != expected.ProjectID {
		_ = project.Close()
		return result, ErrSwitchedDraftUnverified
	}
	reader, err := openReadOnlyPinnedProjectUnderLock(ctx, held, project, config)
	if err != nil {
		return result, errors.Join(ErrSwitchedDraftUnverified, err)
	}
	defer func() {
		if err := reader.Close(); err != nil {
			retainErr := held.Retain()
			result = SwitchedDraftReadback{}
			retErr = errors.Join(retErr, ErrSwitchedDraftUnverified,
				&SwitchedReadbackCleanupError{Reader: reader, Cause: err}, retainErr)
		}
	}()
	report, err := reader.Report()
	if err != nil || report.ProjectID != expected.ProjectID || !report.LegacyGatewayExcluded {
		return result, errors.Join(ErrSwitchedDraftUnverified, err)
	}
	var body string
	var words int64
	err = reader.Read(ctx, func(tx *sql.Tx) error {
		var rawBody, rawWords any
		if err := tx.QueryRowContext(ctx, `SELECT c.body, d.word_count
 FROM drafts d JOIN contents c ON c.id = d.content_id WHERE d.id = ?`, expected.DraftID).Scan(&rawBody, &rawWords); err != nil {
			return err
		}
		var ok bool
		body, ok = rawBody.(string)
		if !ok {
			return ErrSwitchedDraftUnverified
		}
		words, ok = rawWords.(int64)
		if !ok || words != expected.WordCount || body != expected.Content {
			return ErrSwitchedDraftUnverified
		}
		return nil
	})
	if err != nil {
		return result, errors.Join(ErrSwitchedDraftUnverified, err)
	}
	var treeDigest [sha256.Size]byte
	decoded, err := hex.DecodeString(expected.EditedTreeHash)
	if err != nil || len(decoded) != len(treeDigest) {
		return result, ErrSwitchedDraftUnverified
	}
	copy(treeDigest[:], decoded)
	proof := switchedDraftReadbackHash(expected.DraftID, words, body, treeDigest)
	if hex.EncodeToString(proof[:]) != recovery.EditedProofHash {
		return result, ErrSwitchedDraftUnverified
	}
	last, err := snapshot.RetainSwitched(ctx, expected.EditedTreeHash)
	if err != nil || last == nil || *last != *first {
		return result, errors.Join(ErrSwitchedDraftUnverified, err)
	}
	again, err := projectpublish.RecoverExchangeJournal(ctx, held, config.DataRoot,
		expected.ProjectID, projectPath, stageParent, journalName)
	if err != nil || again != recovery {
		return result, errors.Join(ErrSwitchedDraftUnverified, err)
	}
	if err := held.VerifyLegacy(config.DataRoot); err != nil {
		return result, errors.Join(ErrSwitchedDraftUnverified, err)
	}
	contentDigest := sha256.Sum256([]byte(body))
	return SwitchedDraftReadback{ProjectID: expected.ProjectID, ProjectPath: projectPath,
		DraftID: expected.DraftID, WordCount: words,
		ContentSHA256: hex.EncodeToString(contentDigest[:]), EditedTreeHash: first.NewSourceTreeHash,
		ReadbackSHA256:  recovery.EditedProofHash,
		NewSourceDevice: first.NewSourceDevice, NewSourceInode: first.NewSourceInode}, nil
}

func readbackHashValid(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && hex.EncodeToString(decoded) == value
}

// This is the versioned private-stage proof encoding from projectpublish.
// The journal carries its digest; a different post-swap body, count or tree
// cannot satisfy it without changing the durable PREPARED record.
func switchedDraftReadbackHash(id, count int64, content string, tree [sha256.Size]byte) [sha256.Size]byte {
	h := sha256.New()
	_, _ = h.Write([]byte("private-draft-cas-readback:v1\x00"))
	var number [8]byte
	for _, value := range []int64{id, count, int64(len(content))} {
		binary.BigEndian.PutUint64(number[:], uint64(value))
		_, _ = h.Write(number[:])
	}
	_, _ = h.Write([]byte(content))
	_, _ = h.Write(tree[:])
	var digest [sha256.Size]byte
	copy(digest[:], h.Sum(nil))
	return digest
}
