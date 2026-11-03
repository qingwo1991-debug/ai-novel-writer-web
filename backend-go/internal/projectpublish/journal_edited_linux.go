//go:build linux

package projectpublish

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
)

// EditedStageFacts are physical, immutable observations made under the legacy
// lock. The stage path is trusted server state, never browser input. A future
// author runtime must verify the actual draft CAS and SQLite readback before
// returning EditedDraftCASProof; this package cannot infer that from bytes.
type EditedStageFacts struct {
	ProjectID      string
	ProjectPath    string
	StagePath      string
	SourceTreeHash string
	EditedTreeHash string
	ChangedSQLite  []string
}

// EditedDraftCASProof is evidence from the owner that checked the exact
// transaction and readback on the private stage. Hashes are lowercase SHA256.
// The type is not publication authority and is never accepted from transport.
type EditedDraftCASProof struct {
	DraftID       int64
	OperationHash string
	ReadbackHash  string
}

type EditedDraftCASVerifier interface {
	VerifyEditedDraftCAS(context.Context, EditedStageFacts) (EditedDraftCASProof, error)
}

type EditedDraftCASVerifierFunc func(context.Context, EditedStageFacts) (EditedDraftCASProof, error)

func (f EditedDraftCASVerifierFunc) VerifyEditedDraftCAS(ctx context.Context, facts EditedStageFacts) (EditedDraftCASProof, error) {
	if f == nil {
		return EditedDraftCASProof{}, ErrPublishUnavailable
	}
	return f(ctx, facts)
}

type editedStageScan struct {
	inode        fs.FileInfo
	digest       string
	changedPaths []string
	entryCount   int
	totalBytes   int64
}

func sqliteSidecar(path string) bool {
	switch path {
	case projectfs.DatabaseName,
		projectfs.DatabaseName + "-wal",
		projectfs.DatabaseName + "-shm",
		projectfs.DatabaseName + "-journal":
		return true
	default:
		return false
	}
}

func samePhysicalEntry(a, b entry) bool {
	return a.path == b.path && a.mode == b.mode && a.size == b.size && a.hash == b.hash
}

// editedSQLiteOnly rejects every stage edit outside the exact SQLite file
// set. Unknown assets, registry/copy files and directories must stay byte- and
// mode-identical to the source snapshot. The main DB must remain present.
func editedSQLiteOnly(source, candidate []entry) ([]string, error) {
	before := make(map[string]entry, len(source))
	after := make(map[string]entry, len(candidate))
	for _, item := range source {
		before[item.path] = item
	}
	for _, item := range candidate {
		after[item.path] = item
	}
	mainBefore, beforeOK := before[projectfs.DatabaseName]
	mainAfter, afterOK := after[projectfs.DatabaseName]
	if !beforeOK || !afterOK || !mainBefore.mode.IsRegular() || !mainAfter.mode.IsRegular() {
		return nil, ErrPublishUnavailable
	}
	changed := make([]string, 0, 4)
	for path, old := range before {
		newEntry, exists := after[path]
		if exists && samePhysicalEntry(old, newEntry) {
			continue
		}
		if !sqliteSidecar(path) || !old.mode.IsRegular() {
			return nil, ErrPublishUnavailable
		}
		if exists && (!newEntry.mode.IsRegular() || newEntry.mode != old.mode) {
			return nil, ErrPublishUnavailable
		}
		if path == projectfs.DatabaseName && !exists {
			return nil, ErrPublishUnavailable
		}
		changed = append(changed, path)
	}
	for path, newEntry := range after {
		if _, existed := before[path]; existed {
			continue
		}
		if !sqliteSidecar(path) || path == projectfs.DatabaseName || !newEntry.mode.IsRegular() || newEntry.mode.Perm() != 0600 {
			return nil, ErrPublishUnavailable
		}
		changed = append(changed, path)
	}
	if len(changed) == 0 {
		return nil, ErrPublishUnavailable
	}
	sort.Strings(changed)
	return changed, nil
}

func captureEditedStage(ctx context.Context, s *Snapshot) (editedStageScan, error) {
	var zero editedStageScan
	if ctx == nil || s == nil {
		return zero, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.removed || s.parent == nil || s.lock == nil || s.stageName == "" {
		return zero, ErrInvalid
	}
	if err := s.lock.VerifyLegacy(s.dataRoot); err != nil {
		return zero, err
	}
	parentNow, err := os.Lstat(s.parentPath)
	if err != nil || !os.SameFile(parentNow, s.parentInfo) {
		return zero, ErrChanged
	}
	stageNow, err := s.parent.Lstat(s.stageName)
	if err != nil || !os.SameFile(stageNow, s.stageInfo) || !stageNow.IsDir() {
		return zero, ErrChanged
	}
	root, err := s.parent.OpenRoot(s.stageName)
	if err != nil {
		return zero, ErrChanged
	}
	opened, statErr := root.Stat(".")
	if statErr != nil || !os.SameFile(opened, s.stageInfo) {
		_ = root.Close()
		return zero, ErrChanged
	}
	items, scanErr := scan(ctx, root, s.limits)
	closeErr := root.Close()
	if scanErr != nil {
		return zero, scanErr
	}
	if closeErr != nil {
		return zero, closeErr
	}
	stageAfter, err := s.parent.Lstat(s.stageName)
	if err != nil || !os.SameFile(stageAfter, s.stageInfo) {
		return zero, ErrChanged
	}
	changed, err := editedSQLiteOnly(s.entries, items)
	if err != nil {
		return zero, err
	}
	var totalBytes int64
	for _, item := range items {
		if item.mode.IsRegular() {
			if item.size < 0 || item.size > s.limits.MaxBytes-totalBytes {
				return zero, ErrLimit
			}
			totalBytes += item.size
		}
	}
	return editedStageScan{inode: s.stageInfo, digest: hashTreeEntries(items), changedPaths: changed,
		entryCount: len(items), totalBytes: totalBytes}, nil
}

// syncEditedStage makes the already observed private CAS files and directory
// durable before PREPARED can be acknowledged. A removed SQLite sidecar has no
// file to sync; the stage-directory fsync persists that removal. Every opened
// file is checked against its pinned directory entry before and after sync.
func syncEditedStage(ctx context.Context, s *Snapshot, changed []string) error {
	if ctx == nil || s == nil || len(changed) == 0 {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.removed || s.parent == nil || s.lock == nil {
		return ErrInvalid
	}
	if err := s.lock.VerifyLegacy(s.dataRoot); err != nil {
		return err
	}
	parentNow, err := os.Lstat(s.parentPath)
	if err != nil || !os.SameFile(parentNow, s.parentInfo) {
		return ErrChanged
	}
	stageNow, err := s.parent.Lstat(s.stageName)
	if err != nil || !os.SameFile(stageNow, s.stageInfo) || !stageNow.IsDir() {
		return ErrChanged
	}
	root, err := s.parent.OpenRoot(s.stageName)
	if err != nil {
		return ErrChanged
	}
	defer root.Close()
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(opened, s.stageInfo) {
		return ErrChanged
	}
	for _, name := range changed {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !sqliteSidecar(name) {
			return ErrPublishUnavailable
		}
		info, err := root.Lstat(name)
		if errors.Is(err, fs.ErrNotExist) && name != projectfs.DatabaseName {
			continue // a removed sidecar is persisted by the directory fsync
		}
		if err != nil || !info.Mode().IsRegular() || !oneLink(info) {
			return ErrChanged
		}
		file, err := root.Open(name)
		if err != nil {
			return ErrChanged
		}
		fdInfo, statErr := file.Stat()
		if statErr != nil || !sameVersion(info, fdInfo) {
			_ = file.Close()
			return ErrChanged
		}
		syncErr := file.Sync()
		fdAfter, afterErr := file.Stat()
		closeErr := file.Close()
		visibleAfter, visibleErr := root.Lstat(name)
		if syncErr != nil || afterErr != nil || closeErr != nil || visibleErr != nil ||
			!sameVersion(info, fdAfter) || !sameVersion(info, visibleAfter) {
			return ErrChanged
		}
	}
	if err := syncRoot(root); err != nil {
		return err
	}
	stageAfter, err := s.parent.Lstat(s.stageName)
	if err != nil || !os.SameFile(stageAfter, s.stageInfo) {
		return ErrChanged
	}
	return s.lock.VerifyLegacy(s.dataRoot)
}

func sameChangedPaths(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// PrepareEditedExchangeJournal binds a *previously edited* private stage to
// an independently checked draft CAS readback. It never edits the stage, swaps
// names or resolves recovery. The caller must be a trusted internal owner that
// verifies the exact SQLite transaction; a browser-supplied proof is invalid.
func PrepareEditedExchangeJournal(ctx context.Context, snapshot *Snapshot, drain HandleDrain, verifier EditedDraftCASVerifier) (*ExchangeJournal, error) {
	if ctx == nil || snapshot == nil || drain == nil || verifier == nil {
		return nil, ErrInvalid
	}
	if err := snapshot.VerifySource(ctx); err != nil {
		return nil, err
	}
	first, err := captureEditedStage(ctx, snapshot)
	if err != nil {
		return nil, err
	}
	snapshot.mu.Lock()
	facts := EditedStageFacts{ProjectID: snapshot.projectID, ProjectPath: snapshot.projectPath,
		StagePath:      filepath.Join(snapshot.parentPath, snapshot.stageName),
		SourceTreeHash: hashTreeEntries(snapshot.entries), EditedTreeHash: first.digest,
		ChangedSQLite: append([]string(nil), first.changedPaths...)}
	snapshot.mu.Unlock()
	if facts.SourceTreeHash == facts.EditedTreeHash {
		return nil, ErrPublishUnavailable
	}
	proof, err := verifier.VerifyEditedDraftCAS(ctx, facts)
	if err != nil {
		return nil, err
	}
	if proof.DraftID <= 0 || !validJournalHash(proof.OperationHash) || !validJournalHash(proof.ReadbackHash) {
		return nil, ErrPublishUnavailable
	}
	if err := drain.ProveDrained(ctx, facts.ProjectPath); err != nil {
		return nil, err
	}
	if err := snapshot.VerifySource(ctx); err != nil {
		return nil, err
	}
	second, err := captureEditedStage(ctx, snapshot)
	if err != nil {
		return nil, err
	}
	if !os.SameFile(first.inode, second.inode) || first.digest != second.digest || !sameChangedPaths(first.changedPaths, second.changedPaths) {
		return nil, ErrChanged
	}
	if err := syncEditedStage(ctx, snapshot, second.changedPaths); err != nil {
		return nil, err
	}
	durable, err := captureEditedStage(ctx, snapshot)
	if err != nil || !os.SameFile(durable.inode, second.inode) || durable.digest != second.digest ||
		!sameChangedPaths(durable.changedPaths, second.changedPaths) {
		return nil, ErrChanged
	}
	s := snapshot
	s.mu.Lock()
	locked := true
	defer func() {
		if locked {
			s.mu.Unlock()
		}
	}()
	if s.closed || s.removed || s.journalOwned || s.parent == nil || s.lock == nil || s.projectID != facts.ProjectID || s.projectPath != facts.ProjectPath ||
		!os.SameFile(s.stageInfo, durable.inode) || hashTreeEntries(s.entries) != facts.SourceTreeHash ||
		!sameDevice(s.storageInfo, s.stageInfo) || !sameDevice(s.storageInfo, s.parentInfo) {
		return nil, ErrChanged
	}
	if err := s.lock.VerifyLegacy(s.dataRoot); err != nil {
		return nil, err
	}
	sourceID, sourceOK := inodeOf(s.storageInfo)
	stageID, stageOK := inodeOf(s.stageInfo)
	parentID, parentOK := inodeOf(s.parentInfo)
	if !sourceOK || !stageOK || !parentOK || sourceID == stageID || sourceID.Device != stageID.Device || sourceID.Device != parentID.Device {
		return nil, ErrInvalid
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	id := hex.EncodeToString(nonce[:])
	prepared := journalPrepared{Format: 1, ID: id, DataRoot: s.dataRoot, ProjectID: s.projectID,
		ProjectPath: s.projectPath, StageParent: s.parentPath, StageName: s.stageName,
		Source: sourceID, Stage: stageID, Parent: parentID,
		SourceTreeHash: facts.SourceTreeHash, StageTreeHash: durable.digest,
		EntryCount: durable.entryCount, TotalBytes: durable.totalBytes,
		MaxFiles: s.limits.MaxFiles, MaxBytes: s.limits.MaxBytes, MaxDepth: s.limits.MaxDepth,
		EditedDraftID: proof.DraftID, EditedOperationHash: proof.OperationHash, EditedProofHash: proof.ReadbackHash}
	raw, err := json.Marshal(prepared)
	if err != nil {
		return nil, err
	}
	name := "novel-exchange-" + id + ".prepared.json"
	// A failed write may still leave a durable marker. Transfer cleanup
	// ownership before attempting it, so a deferred Snapshot.Close cannot
	// destroy the stage referenced by restart recovery.
	s.journalOwned = true
	if err := writeJournalFile(s.parent, name, raw); err != nil {
		return nil, err
	}
	journal := &ExchangeJournal{name: name, prepared: prepared, root: s.parent, lock: s.lock, rawHash: journalDigest(raw)}
	s.mu.Unlock()
	locked = false
	if err := snapshot.VerifySource(ctx); err != nil {
		return nil, err // Durable marker remains for conservative recovery.
	}
	final, err := captureEditedStage(ctx, snapshot)
	if err != nil || final.digest != second.digest || !os.SameFile(final.inode, second.inode) || !sameChangedPaths(final.changedPaths, second.changedPaths) {
		return nil, ErrChanged
	}
	return journal, nil
}
