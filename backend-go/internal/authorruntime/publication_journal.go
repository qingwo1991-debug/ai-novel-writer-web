//go:build linux

package authorruntime

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"strings"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/writerlock"
)

var ErrPublicationJournalInvalid = errors.New("AUTHOR_PUBLICATION_JOURNAL_INVALID")
var ErrPublicationJournalExists = errors.New("AUTHOR_PUBLICATION_JOURNAL_EXISTS")

const publicationJournalName = "publication-intent.json"
const maxPublicationJournalBytes = 1 << 20

// PublicationIntent is a durable PREPARED marker. It records the pre-write
// physical identity and a hash of the sealed backup manifest. No method in
// this package changes the author project. A future writer must create this
// marker before its first source mutation and retain the lock throughout its
// own commit/readback protocol.
type PublicationIntent struct {
	Version        int                      `json:"version"`
	IntentID       string                   `json:"intentId"`
	ProjectID      string                   `json:"projectId"`
	OperationHash  string                   `json:"operationHash"`
	ManifestSHA256 string                   `json:"manifestSha256"`
	Before         DatabaseSnapshotManifest `json:"before"`
}

// PublicationRecovery is a classification only. Changed or unreadable source
// data requires operator review; this API never restores a backup or treats an
// unknown outcome as a failed transaction.
type PublicationRecovery struct {
	Intent PublicationIntent
	Class  string // source-unchanged, source-changed, source-unreadable
}

// PreparePublicationIntent fsyncs a single exclusive marker inside the sealed
// private backup directory. A restart can inspect it under a newly acquired
// legacy lock before deciding how to handle an interrupted future write.
func PreparePublicationIntent(ctx context.Context, plan *SQLitePublicationPlan, operationHash string) (PublicationIntent, error) {
	if plan == nil || plan.backup == nil || ctx == nil || !validPublicationHash(operationHash) {
		return PublicationIntent{}, ErrPublicationJournalInvalid
	}
	if err := plan.Verify(ctx); err != nil {
		return PublicationIntent{}, err
	}
	backup := plan.backup
	backup.mu.Lock()
	if backup.root == nil {
		backup.mu.Unlock()
		return PublicationIntent{}, ErrClosed
	}
	manifestRaw, err := readPrivateJournalFile(backup.root, sealedSnapshotManifest, maxPublicationJournalBytes)
	if err != nil {
		backup.mu.Unlock()
		return PublicationIntent{}, err
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		backup.mu.Unlock()
		return PublicationIntent{}, ErrPublicationJournalInvalid
	}
	digest := sha256.Sum256(manifestRaw)
	intent := PublicationIntent{Version: 1, IntentID: hex.EncodeToString(nonce[:]), ProjectID: backup.manifest.ProjectID,
		OperationHash: operationHash, ManifestSHA256: hex.EncodeToString(digest[:]), Before: backup.Manifest()}
	raw, err := json.Marshal(intent)
	if err != nil || len(raw) > maxPublicationJournalBytes {
		backup.mu.Unlock()
		return PublicationIntent{}, ErrPublicationJournalInvalid
	}
	file, err := backup.root.OpenFile(publicationJournalName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if errors.Is(err, fs.ErrExist) {
		backup.mu.Unlock()
		return PublicationIntent{}, ErrPublicationJournalExists
	}
	if err != nil {
		backup.mu.Unlock()
		return PublicationIntent{}, ErrPublicationJournalInvalid
	}
	writeErr := error(nil)
	if n, err := file.Write(raw); err != nil || n != len(raw) {
		writeErr = ErrPublicationJournalInvalid
	} else if err := file.Sync(); err != nil {
		writeErr = ErrPublicationJournalInvalid
	}
	if err := file.Close(); err != nil {
		writeErr = ErrPublicationJournalInvalid
	}
	if writeErr == nil {
		writeErr = syncSnapshotDirectory(backup.root)
	}
	backup.mu.Unlock()
	if writeErr != nil {
		// Preserve a partially durable marker for conservative restart review.
		return PublicationIntent{}, writeErr
	}
	if err := plan.Verify(ctx); err != nil {
		// The marker remains on disk: an uncertain source outcome cannot be
		// silently discarded even when this caller did not perform a write.
		return PublicationIntent{}, err
	}
	return intent, nil
}

func validPublicationHash(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil && strings.ToLower(value) == value
}

// RecoverPublicationIntent reopens a private sealed backup and classifies its
// original SQLite file set against the current author project under a freshly
// held legacy lock. It does not require the pre-crash Reader to survive. An
// invalid backup or journal is an error; an unavailable or changed source is
// an explicit non-success classification. The library and projectPath are
// trusted server configuration, never values taken from the journal.
func RecoverPublicationIntent(ctx context.Context, held *writerlock.Lock, dataRoot string, library *projectfs.Library, projectPath, backupDirectory, scratchParent string, maxBytes int64) (PublicationRecovery, error) {
	var zero PublicationRecovery
	if ctx == nil || held == nil || library == nil || projectPath == "" || maxBytes <= 0 {
		return zero, ErrPublicationJournalInvalid
	}
	if err := held.VerifyLegacy(dataRoot); err != nil {
		return zero, err
	}
	if _, err := privateSnapshotParent(scratchParent, dataRoot); err != nil {
		return zero, err
	}
	if _, err := privateSnapshotParent(backupDirectory, dataRoot); err != nil {
		return zero, err
	}
	backupInfo, err := os.Lstat(backupDirectory)
	if err != nil || !privateSnapshotDirectory(backupInfo) {
		return zero, ErrPublicationJournalInvalid
	}
	backupRoot, err := os.OpenRoot(backupDirectory)
	if err != nil {
		return zero, ErrPublicationJournalInvalid
	}
	defer backupRoot.Close()
	opened, err := backupRoot.Stat(".")
	if err != nil || !os.SameFile(backupInfo, opened) {
		return zero, ErrPublicationJournalInvalid
	}
	manifestRaw, err := readPrivateJournalFile(backupRoot, sealedSnapshotManifest, maxPublicationJournalBytes)
	if err != nil {
		return zero, err
	}
	var manifest DatabaseSnapshotManifest
	if err := decodePublicationJSON(manifestRaw, &manifest); err != nil || !validPublicationManifest(manifest) {
		return zero, ErrPublicationJournalInvalid
	}
	intentRaw, err := readPrivateJournalFile(backupRoot, publicationJournalName, maxPublicationJournalBytes)
	if err != nil {
		return zero, err
	}
	var intent PublicationIntent
	if err := decodePublicationJSON(intentRaw, &intent); err != nil || intent.Version != 1 || len(intent.IntentID) != 32 || !validPublicationHash(intent.OperationHash) || !validPublicationHash(intent.ManifestSHA256) ||
		intent.ProjectID != manifest.ProjectID || !sameDatabaseManifest(intent.Before, manifest) {
		return zero, ErrPublicationJournalInvalid
	}
	if _, err := hex.DecodeString(intent.IntentID); err != nil {
		return zero, ErrPublicationJournalInvalid
	}
	digest := sha256.Sum256(manifestRaw)
	if intent.ManifestSHA256 != hex.EncodeToString(digest[:]) {
		return zero, ErrPublicationJournalInvalid
	}
	for _, member := range manifest.Members {
		current, err := syncAndHashSnapshotMember(ctx, backupRoot, member.Name)
		if err != nil || current != member {
			return zero, ErrPublicationJournalInvalid
		}
	}
	if !completePublicationMembership(backupRoot, manifest) {
		return zero, ErrPublicationJournalInvalid
	}
	if current, err := os.Lstat(backupDirectory); err != nil || !os.SameFile(backupInfo, current) {
		return zero, ErrPublicationJournalInvalid
	}
	if err := held.VerifyLegacy(dataRoot); err != nil {
		return zero, err
	}
	result := PublicationRecovery{Intent: intent, Class: "source-unreadable"}
	project, err := library.OpenProject(projectPath)
	if err != nil {
		if ctx.Err() != nil {
			return zero, ctx.Err()
		}
		if lockErr := held.VerifyLegacy(dataRoot); lockErr != nil {
			return zero, lockErr
		}
		return result, nil
	}
	defer project.Close()
	if project.ProjectID() != intent.ProjectID {
		result.Class = "source-changed"
		if lockErr := held.VerifyLegacy(dataRoot); lockErr != nil {
			return zero, lockErr
		}
		return result, nil
	}
	stage, err := stageRecoverySource(ctx, project, held, dataRoot, scratchParent, maxBytes)
	if err != nil {
		if ctx.Err() != nil {
			return zero, ctx.Err()
		}
		if lockErr := held.VerifyLegacy(dataRoot); lockErr != nil {
			return zero, lockErr
		}
		return result, nil
	}
	if sameDatabaseManifest(stage, manifest) {
		result.Class = "source-unchanged"
	} else {
		result.Class = "source-changed"
	}
	if err := held.VerifyLegacy(dataRoot); err != nil {
		return zero, err
	}
	return result, nil
}

func readPrivateJournalFile(root *os.Root, name string, limit int64) ([]byte, error) {
	before, err := root.Lstat(name)
	if err != nil || !privateSnapshotFile(before) || before.Size() < 1 || before.Size() > limit {
		return nil, ErrPublicationJournalInvalid
	}
	file, err := root.Open(name)
	if err != nil {
		return nil, ErrPublicationJournalInvalid
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return nil, ErrPublicationJournalInvalid
	}
	raw, err := io.ReadAll(io.LimitReader(file, limit+1))
	after, statErr := file.Stat()
	named, nameErr := root.Lstat(name)
	if err != nil || statErr != nil || nameErr != nil || len(raw) > int(limit) || !os.SameFile(opened, after) || !os.SameFile(after, named) || int64(len(raw)) != after.Size() {
		return nil, ErrPublicationJournalInvalid
	}
	return raw, nil
}

func decodePublicationJSON(raw []byte, output any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return ErrPublicationJournalInvalid
	}
	return nil
}

func validPublicationManifest(value DatabaseSnapshotManifest) bool {
	if value.Version != 1 || value.ProjectID == "" || len(value.Members) == 0 || len(value.Members) > 4 || value.Members[0].Name != "snapshot.db" || value.Members[0].Bytes <= 0 {
		return false
	}
	allowed := []string{"snapshot.db", "snapshot.db-wal", "snapshot.db-shm", "snapshot.db-journal"}
	position := -1
	for _, member := range value.Members {
		found := -1
		for i, name := range allowed {
			if member.Name == name {
				found = i
				break
			}
		}
		if found <= position || member.Bytes < 0 || !validPublicationHash(member.SHA256) {
			return false
		}
		position = found
	}
	return true
}

func completePublicationMembership(root *os.Root, manifest DatabaseSnapshotManifest) bool {
	known := make(map[string]bool, len(manifest.Members))
	for _, member := range manifest.Members {
		known[member.Name] = true
	}
	for _, name := range []string{"snapshot.db", "snapshot.db-wal", "snapshot.db-shm", "snapshot.db-journal"} {
		_, err := root.Lstat(name)
		if known[name] && err != nil || !known[name] && !errors.Is(err, fs.ErrNotExist) {
			return false
		}
	}
	return true
}

func stageRecoverySource(ctx context.Context, project *projectfs.Project, held *writerlock.Lock, dataRoot, scratchParent string, maxBytes int64) (DatabaseSnapshotManifest, error) {
	var zero DatabaseSnapshotManifest
	directory, err := os.MkdirTemp(scratchParent, "novel-go-recovery-inspect-")
	if err != nil {
		return zero, err
	}
	var root *os.Root
	info, _ := os.Lstat(directory)
	defer func() {
		if root != nil {
			for _, name := range []string{"snapshot.db", "snapshot.db-wal", "snapshot.db-shm", "snapshot.db-journal"} {
				_ = root.Remove(name)
			}
			_ = root.Close()
		}
		if current, statErr := os.Lstat(directory); statErr == nil && info != nil && os.SameFile(info, current) {
			_ = os.Remove(directory)
		}
	}()
	if err := os.Chmod(directory, 0700); err != nil {
		return zero, err
	}
	root, err = os.OpenRoot(directory)
	if err != nil {
		return zero, err
	}
	proof, err := project.StageDatabaseUnderLegacyLock(ctx, held, dataRoot, root, maxBytes)
	if err != nil {
		return zero, err
	}
	manifest := DatabaseSnapshotManifest{Version: 1, ProjectID: project.ProjectID()}
	for _, name := range []string{"snapshot.db", "snapshot.db-wal", "snapshot.db-shm", "snapshot.db-journal"} {
		member, err := syncAndHashSnapshotMember(ctx, root, name)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return zero, err
		}
		manifest.Members = append(manifest.Members, member)
	}
	if err := proof.VerifyUnchanged(ctx); err != nil {
		return zero, err
	}
	if !validPublicationManifest(manifest) {
		return zero, ErrPublicationJournalInvalid
	}
	return manifest, nil
}
