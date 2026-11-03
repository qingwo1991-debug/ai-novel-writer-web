//go:build linux

package projectpublish

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/writerlock"
)

var ErrJournalInvalid = errors.New("PROJECT_PUBLICATION_JOURNAL_INVALID")

const maxJournalBytes = 64 << 10

// JournalClass is a restart observation, never permission to restore or delete.
type JournalClass string

const (
	JournalPrepared            JournalClass = "prepared-unchanged"
	JournalSwitchedUnconfirmed JournalClass = "switched-unconfirmed"
	JournalSwitched            JournalClass = "switched"
	JournalUnknown             JournalClass = "unknown"
)

type journalInode struct {
	Device uint64 `json:"device"`
	Number uint64 `json:"number"`
}

func inodeOf(info fs.FileInfo) (journalInode, bool) {
	if info == nil {
		return journalInode{}, false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return journalInode{}, false
	}
	return journalInode{Device: uint64(stat.Dev), Number: stat.Ino}, true
}

type journalPrepared struct {
	Format              int          `json:"format"`
	ID                  string       `json:"id"`
	DataRoot            string       `json:"dataRoot"`
	ProjectID           string       `json:"projectId"`
	ProjectPath         string       `json:"projectPath"`
	StageParent         string       `json:"stageParent"`
	StageName           string       `json:"stageName"`
	Source              journalInode `json:"source"`
	Stage               journalInode `json:"stage"`
	Parent              journalInode `json:"parent"`
	SourceTreeHash      string       `json:"sourceTreeHash"`
	StageTreeHash       string       `json:"stageTreeHash"`
	EntryCount          int          `json:"entryCount"`
	TotalBytes          int64        `json:"totalBytes"`
	MaxFiles            int          `json:"maxFiles"`
	MaxBytes            int64        `json:"maxBytes"`
	MaxDepth            int          `json:"maxDepth"`
	EditedDraftID       int64        `json:"editedDraftId,omitempty"`
	EditedOperationHash string       `json:"editedOperationHash,omitempty"`
	EditedProofHash     string       `json:"editedProofHash,omitempty"`
}

type journalSwitched struct {
	Format       int    `json:"format"`
	PreparedHash string `json:"preparedHash"`
}

// ExchangeJournal is a durable PREPARED marker tied to a still-held legacy
// lock and an immutable private stage. It cannot perform the exchange.
type ExchangeJournal struct {
	name     string
	prepared journalPrepared
	root     *os.Root
	lock     *writerlock.Lock
	rawHash  string
}

func (j *ExchangeJournal) Name() string {
	if j == nil {
		return ""
	}
	return j.name
}

// JournalRecovery only classifies visible state. A caller must retain both
// directories and seek explicit recovery authority for every nontrivial case.
type JournalRecovery struct {
	Class               JournalClass
	JournalID           string
	ProjectPath         string
	StagePath           string
	PreparedMarked      bool
	SwitchedMarked      bool
	EditedDraftID       int64
	EditedOperationHash string
	EditedProofHash     string
}

func validJournalName(name string) bool {
	if !strings.HasPrefix(name, "novel-exchange-") || !strings.HasSuffix(name, ".prepared.json") || strings.ToLower(name) != name || len(name) != len("novel-exchange-")+32+len(".prepared.json") {
		return false
	}
	_, err := hex.DecodeString(strings.TrimSuffix(strings.TrimPrefix(name, "novel-exchange-"), ".prepared.json"))
	return err == nil
}

func validJournalHash(value string) bool {
	if len(value) != sha256.Size*2 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func journalDigest(raw []byte) string {
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}

func hashTreeEntries(items []entry) string {
	// Use the exact field order and byte encoding of ExchangePlan's source
	// digest, so the journal binds the same evidence without exposing entries.
	h := sha256.New()
	var length [8]byte
	for _, item := range items {
		putJournalUint64(length[:], uint64(len(item.path)))
		_, _ = h.Write(length[:])
		_, _ = h.Write([]byte(item.path))
		putJournalUint64(length[:], uint64(item.size))
		_, _ = h.Write(length[:])
		putJournalUint64(length[:], uint64(item.mode))
		_, _ = h.Write(length[:])
		_, _ = h.Write(item.hash[:])
	}
	return hex.EncodeToString(h.Sum(nil))
}

func putJournalUint64(dst []byte, value uint64) {
	for i := 7; i >= 0; i-- {
		dst[i] = byte(value)
		value >>= 8
	}
}

func writeJournalFile(root *os.Root, name string, raw []byte) error {
	if root == nil || len(raw) == 0 || len(raw) > maxJournalBytes {
		return ErrJournalInvalid
	}
	file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	// O_CREATE applies the process umask. Normalize and verify the mode before
	// acknowledging a marker; otherwise a restrictive umask could create a
	// durable file that the strict recovery reader cannot open later.
	if err := file.Chmod(0600); err != nil {
		_ = file.Close()
		return err
	}
	created, err := file.Stat()
	if err != nil || created.Mode().Perm() != 0600 {
		_ = file.Close()
		return ErrJournalInvalid
	}
	owner, ok := created.Sys().(*syscall.Stat_t)
	if !ok || owner.Uid != uint32(os.Geteuid()) {
		_ = file.Close()
		return ErrJournalInvalid
	}
	writeErr := error(nil)
	if n, err := file.Write(raw); err != nil || n != len(raw) {
		writeErr = ErrJournalInvalid
	} else if err := file.Sync(); err != nil {
		writeErr = err
	}
	if err := file.Close(); err != nil {
		writeErr = errors.Join(writeErr, err)
	}
	if writeErr != nil {
		// A partial marker is intentionally retained for manual review.
		return writeErr
	}
	return syncRoot(root)
}

func readJournalFile(root *os.Root, name string) ([]byte, error) {
	info, err := root.Lstat(name)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || !oneLink(info) || info.Size() <= 0 || info.Size() > maxJournalBytes {
		return nil, ErrJournalInvalid
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return nil, ErrJournalInvalid
	}
	file, err := root.Open(name)
	if err != nil {
		return nil, ErrJournalInvalid
	}
	opened, err := file.Stat()
	if err != nil || !sameVersion(info, opened) {
		_ = file.Close()
		return nil, ErrJournalInvalid
	}
	raw, readErr := io.ReadAll(io.LimitReader(file, maxJournalBytes+1))
	after, statErr := file.Stat()
	closeErr := file.Close()
	if readErr != nil || statErr != nil || closeErr != nil || !sameVersion(info, after) || len(raw) == 0 || len(raw) > maxJournalBytes {
		return nil, ErrJournalInvalid
	}
	return raw, nil
}

func strictJournalJSON(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return ErrJournalInvalid
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return ErrJournalInvalid
	}
	canonical, err := json.Marshal(target)
	if err != nil || !bytes.Equal(canonical, raw) {
		return ErrJournalInvalid
	}
	return nil
}

// PrepareExchangeJournal persists a PREPARED marker outside the source tree.
// It repeats the plan proof, records both directory inodes and complete tree
// digests, fsyncs the marker and its parent, and leaves all source bytes alone.
// This is deliberately a pre-edit journal: ExchangePlan requires the private
// tree to equal the source. It cannot authorize an edited candidate or publish.
func PrepareExchangeJournal(ctx context.Context, plan *ExchangePlan) (*ExchangeJournal, error) {
	if ctx == nil || plan == nil || plan.snapshot == nil {
		return nil, ErrInvalid
	}
	if err := plan.Verify(ctx); err != nil {
		return nil, err
	}
	s := plan.snapshot
	s.mu.Lock()
	locked := true
	defer func() {
		if locked {
			s.mu.Unlock()
		}
	}()
	if s.closed || s.removed || s.journalOwned || s.parent == nil || s.lock == nil || s.projectPath != plan.projectPath || !os.SameFile(s.storageInfo, plan.sourceInfo) || !os.SameFile(s.stageInfo, plan.stageInfo) {
		return nil, ErrChanged
	}
	source, sourceOK := inodeOf(s.storageInfo)
	stage, stageOK := inodeOf(s.stageInfo)
	parent, parentOK := inodeOf(s.parentInfo)
	if !sourceOK || !stageOK || !parentOK || source == stage || source.Device != stage.Device || source.Device != parent.Device {
		return nil, ErrInvalid
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	id := hex.EncodeToString(nonce[:])
	prepared := journalPrepared{Format: 1, ID: id, DataRoot: s.dataRoot, ProjectID: s.projectID, ProjectPath: s.projectPath,
		StageParent: s.parentPath, StageName: s.stageName, Source: source, Stage: stage, Parent: parent,
		SourceTreeHash: hex.EncodeToString(plan.sourceTreeSHA256[:]), StageTreeHash: hashTreeEntries(s.entries),
		EntryCount: len(s.entries), TotalBytes: plan.totalBytes,
		MaxFiles: s.limits.MaxFiles, MaxBytes: s.limits.MaxBytes, MaxDepth: s.limits.MaxDepth}
	if prepared.SourceTreeHash != prepared.StageTreeHash {
		return nil, ErrChanged
	}
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
	// The snapshot owns the root; journal methods must not outlive it.
	journal := &ExchangeJournal{name: name, prepared: prepared, root: s.parent, lock: s.lock, rawHash: journalDigest(raw)}
	s.mu.Unlock()
	locked = false
	if err := plan.Verify(ctx); err != nil {
		// The PREPARED marker remains for conservative restart review.
		return nil, err
	}
	return journal, nil
}

func validPrepared(p journalPrepared, dataRoot, projectID, projectPath, stageParent, name string) bool {
	base := p.Format == 1 && p.ID != "" && name == "novel-exchange-"+p.ID+".prepared.json" &&
		canonical(dataRoot) && canonical(projectPath) && canonical(stageParent) &&
		p.DataRoot == dataRoot && p.ProjectID == projectID && projectID != "" && p.ProjectPath == projectPath && p.StageParent == stageParent &&
		strings.HasPrefix(p.StageName, "novel-private-publish-") && filepath.Base(p.StageName) == p.StageName &&
		p.Source.Device != 0 && p.Source.Number != 0 && p.Stage.Device == p.Source.Device && p.Stage.Number != 0 &&
		p.Parent.Device == p.Source.Device && p.Parent.Number != 0 && p.Source != p.Stage &&
		validJournalHash(p.SourceTreeHash) && validJournalHash(p.StageTreeHash) &&
		p.EntryCount > 0 && p.TotalBytes >= 0 && p.MaxFiles >= p.EntryCount && p.MaxFiles <= maxTreeFiles &&
		p.MaxBytes > 0 && p.MaxBytes <= maxTreeBytes && p.MaxDepth > 0 && p.MaxDepth <= maxTreeDepth
	if !base {
		return false
	}
	if p.EditedDraftID == 0 && p.EditedOperationHash == "" && p.EditedProofHash == "" {
		return p.SourceTreeHash == p.StageTreeHash
	}
	return p.EditedDraftID > 0 && validJournalHash(p.EditedOperationHash) && validJournalHash(p.EditedProofHash) && p.SourceTreeHash != p.StageTreeHash
}

func inspectJournalTree(ctx context.Context, path string, limits Limits) (journalInode, string, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return journalInode{}, "", ErrChanged
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return journalInode{}, "", ErrChanged
	}
	opened, statErr := root.Stat(".")
	if statErr != nil || !os.SameFile(info, opened) {
		_ = root.Close()
		return journalInode{}, "", ErrChanged
	}
	items, scanErr := scan(ctx, root, limits)
	closeErr := root.Close()
	if errors.Is(scanErr, context.Canceled) || errors.Is(scanErr, context.DeadlineExceeded) {
		return journalInode{}, "", scanErr
	}
	if scanErr != nil || closeErr != nil {
		return journalInode{}, "", ErrChanged
	}
	id, ok := inodeOf(info)
	if !ok {
		return journalInode{}, "", ErrChanged
	}
	return id, hashTreeEntries(items), nil
}

// RecoverExchangeJournal classifies a journal after restart under a freshly
// acquired legacy lock. All paths are trusted server arguments and must match
// the persisted marker exactly; a marker can never supply its own target.
func RecoverExchangeJournal(ctx context.Context, held *writerlock.Lock, dataRoot, projectID, projectPath, stageParent, name string) (JournalRecovery, error) {
	var zero JournalRecovery
	if ctx == nil || held == nil || !validJournalName(name) || projectID == "" || !canonical(dataRoot) || !canonical(projectPath) || !canonical(stageParent) || !childOf(dataRoot, projectPath) {
		return zero, ErrJournalInvalid
	}
	if err := held.VerifyLegacy(dataRoot); err != nil {
		return zero, err
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	storagePath := filepath.Join(projectPath, projectfs.StorageDirectory)
	if stageParent == storagePath || childOf(storagePath, stageParent) || childOf(stageParent, storagePath) {
		return zero, ErrJournalInvalid
	}
	parentInfo, err := privateDirectory(stageParent)
	if err != nil {
		return zero, ErrJournalInvalid
	}
	visibleSource, sourceErr := os.Lstat(storagePath)
	if sourceErr != nil || !visibleSource.IsDir() || visibleSource.Mode()&os.ModeSymlink != 0 {
		visibleSource = nil // Still validate the journal, then classify unknown.
	}
	root, pinnedInfo, err := openPinnedDirectory(stageParent, visibleSource)
	if err != nil {
		return zero, ErrJournalInvalid
	}
	defer root.Close()
	if !os.SameFile(parentInfo, pinnedInfo) {
		return zero, ErrJournalInvalid
	}
	raw, err := readJournalFile(root, name)
	if err != nil {
		return zero, err
	}
	var prepared journalPrepared
	if err := strictJournalJSON(raw, &prepared); err != nil || !validPrepared(prepared, dataRoot, projectID, projectPath, stageParent, name) {
		return zero, ErrJournalInvalid
	}
	parentID, ok := inodeOf(pinnedInfo)
	if !ok || parentID != prepared.Parent {
		return zero, ErrJournalInvalid
	}
	markerName := name + ".switched"
	markerRaw, markerErr := readJournalFile(root, markerName)
	marked := markerErr == nil
	if markerErr != nil && !errors.Is(markerErr, ErrJournalInvalid) {
		return zero, markerErr
	}
	// A missing marker is normal; an existing malformed one is not. Lstat
	// distinguishes those cases without allowing a symlink to be followed.
	if markerErr != nil {
		if _, statErr := root.Lstat(markerName); !errors.Is(statErr, fs.ErrNotExist) {
			return zero, ErrJournalInvalid
		}
	} else {
		var marker journalSwitched
		if err := strictJournalJSON(markerRaw, &marker); err != nil || marker.Format != 1 || marker.PreparedHash != journalDigest(raw) {
			return zero, ErrJournalInvalid
		}
	}
	result := JournalRecovery{Class: JournalUnknown, JournalID: prepared.ID, ProjectPath: projectPath,
		StagePath: filepath.Join(stageParent, prepared.StageName), PreparedMarked: true, SwitchedMarked: marked,
		EditedDraftID: prepared.EditedDraftID, EditedOperationHash: prepared.EditedOperationHash,
		EditedProofHash: prepared.EditedProofHash}
	limits := Limits{MaxFiles: prepared.MaxFiles, MaxBytes: prepared.MaxBytes, MaxDepth: prepared.MaxDepth}
	sourceID, sourceHash, sourceErr := inspectJournalTree(ctx, storagePath, limits)
	stageID, stageHash, stageErr := inspectJournalTree(ctx, result.StagePath, limits)
	if errors.Is(sourceErr, context.Canceled) || errors.Is(sourceErr, context.DeadlineExceeded) {
		return zero, sourceErr
	}
	if errors.Is(stageErr, context.Canceled) || errors.Is(stageErr, context.DeadlineExceeded) {
		return zero, stageErr
	}
	if sourceErr == nil && stageErr == nil {
		unchanged := sourceID == prepared.Source && stageID == prepared.Stage && sourceHash == prepared.SourceTreeHash && stageHash == prepared.StageTreeHash
		switched := sourceID == prepared.Stage && stageID == prepared.Source && sourceHash == prepared.StageTreeHash && stageHash == prepared.SourceTreeHash
		switch {
		case unchanged && !marked:
			result.Class = JournalPrepared
		case switched && !marked:
			result.Class = JournalSwitchedUnconfirmed
		case switched && marked:
			result.Class = JournalSwitched
		}
	}
	if err := held.VerifyLegacy(dataRoot); err != nil {
		return zero, err
	}
	return result, nil
}

// MarkSwitched durably records an observed switch after a future exchange.
// It cannot cause a namespace mutation and refuses a marker unless the exact
// source/stage inodes and tree digests appear at their exchanged paths.
func (j *ExchangeJournal) MarkSwitched(ctx context.Context) error {
	if j == nil || j.root == nil || j.lock == nil || ctx == nil || !validJournalName(j.name) {
		return ErrJournalInvalid
	}
	if err := j.lock.VerifyLegacy(j.prepared.DataRoot); err != nil {
		return err
	}
	parentInfo, err := os.Lstat(j.prepared.StageParent)
	if err != nil {
		return ErrChanged
	}
	parentID, ok := inodeOf(parentInfo)
	if !ok || parentID != j.prepared.Parent {
		return ErrChanged
	}
	raw, err := readJournalFile(j.root, j.name)
	if err != nil || journalDigest(raw) != j.rawHash {
		return ErrJournalInvalid
	}
	var prepared journalPrepared
	if err := strictJournalJSON(raw, &prepared); err != nil || prepared != j.prepared {
		return ErrJournalInvalid
	}
	limits := Limits{MaxFiles: prepared.MaxFiles, MaxBytes: prepared.MaxBytes, MaxDepth: prepared.MaxDepth}
	sourceID, sourceHash, sourceErr := inspectJournalTree(ctx, filepath.Join(prepared.ProjectPath, projectfs.StorageDirectory), limits)
	stageID, stageHash, stageErr := inspectJournalTree(ctx, filepath.Join(prepared.StageParent, prepared.StageName), limits)
	if sourceErr != nil || stageErr != nil || sourceID != prepared.Stage || stageID != prepared.Source || sourceHash != prepared.StageTreeHash || stageHash != prepared.SourceTreeHash {
		return ErrChanged
	}
	// A durable marker must not get ahead of the namespace exchange. Fsync both
	// directories whose entries would change, then recheck the observed swap.
	// The project root is opened component by component so an alias cannot make
	// the fsync target differ from the trusted project directory.
	projectRoot, _, err := openPinnedDirectory(prepared.ProjectPath, nil)
	if err != nil {
		return ErrChanged
	}
	projectSyncErr := syncRoot(projectRoot)
	projectCloseErr := projectRoot.Close()
	if err := errors.Join(projectSyncErr, projectCloseErr); err != nil {
		return err
	}
	if err := syncRoot(j.root); err != nil {
		return err
	}
	sourceAfter, sourceHashAfter, sourceErr := inspectJournalTree(ctx, filepath.Join(prepared.ProjectPath, projectfs.StorageDirectory), limits)
	stageAfter, stageHashAfter, stageErr := inspectJournalTree(ctx, filepath.Join(prepared.StageParent, prepared.StageName), limits)
	if sourceErr != nil || stageErr != nil || sourceAfter != prepared.Stage || stageAfter != prepared.Source || sourceHashAfter != prepared.StageTreeHash || stageHashAfter != prepared.SourceTreeHash {
		return ErrChanged
	}
	marker, err := json.Marshal(journalSwitched{Format: 1, PreparedHash: j.rawHash})
	if err != nil {
		return err
	}
	if err := writeJournalFile(j.root, j.name+".switched", marker); err != nil {
		if errors.Is(err, fs.ErrExist) {
			existing, readErr := readJournalFile(j.root, j.name+".switched")
			if readErr == nil && bytes.Equal(existing, marker) {
				return j.lock.VerifyLegacy(prepared.DataRoot)
			}
		}
		return fmt.Errorf("durable switched marker: %w", err)
	}
	return j.lock.VerifyLegacy(prepared.DataRoot)
}
