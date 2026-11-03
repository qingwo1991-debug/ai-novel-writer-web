//go:build linux

package projectpublish

import (
	"context"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"unsafe"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/writerlock"
)

// ErrExchangeOutcomeUnknown means a namespace syscall was attempted. The
// caller must classify the journal and retain both trees; it must not retry.
var ErrExchangeOutcomeUnknown = errors.New("PROJECT_PUBLICATION_EXCHANGE_OUTCOME_UNKNOWN")

// CurrentDraftFacts are server-owned inputs to the active session owner. The
// callback must verify the exact caller, tab, lease and project generation;
// a browser response or a stale project path is not an authority.
type CurrentDraftFacts struct {
	ProjectID      string
	ProjectPath    string
	DraftID        int64
	OperationHash  string
	SourceTreeHash string
	EditedTreeHash string
}

type CurrentDraftAuthority interface {
	VerifyCurrentDraft(context.Context, CurrentDraftFacts) error
}

type CurrentDraftAuthorityFunc func(context.Context, CurrentDraftFacts) error

func (f CurrentDraftAuthorityFunc) VerifyCurrentDraft(ctx context.Context, facts CurrentDraftFacts) error {
	if f == nil {
		return ErrPublishUnavailable
	}
	return f(ctx, facts)
}

// EditedExchangePlan binds one private CAS readback to one durable PREPARED
// journal. It deliberately has no source mutation method. The pre-edit
// ExchangePlan cannot validate an edited stage and is never used here.
type EditedExchangePlan struct {
	mu        sync.Mutex
	attempted bool
	snapshot  *Snapshot
	journal   *ExchangeJournal
	evidence  DraftEditEvidence
	current   CurrentDraftAuthority
	drain     HandleDrain
	stageHash string
}

// PrepareEditedExchangePlan independently rechecks the source, the edited
// SQLite-only tree, the PREPARED marker and current browser/session authority.
// The callbacks must belong to trusted internal owners, not transport input.
func PrepareEditedExchangePlan(ctx context.Context, snapshot *Snapshot, evidence DraftEditEvidence,
	journal *ExchangeJournal, current CurrentDraftAuthority, drain HandleDrain) (*EditedExchangePlan, error) {
	if ctx == nil || snapshot == nil || journal == nil || current == nil || drain == nil {
		return nil, ErrInvalid
	}
	plan := &EditedExchangePlan{snapshot: snapshot, journal: journal, evidence: evidence,
		current: current, drain: drain, stageHash: hex.EncodeToString(evidence.AfterStageTreeSHA256[:])}
	if err := plan.Verify(ctx); err != nil {
		return nil, err
	}
	return plan, nil
}

// Verify repeats the entire edited-stage and authority check. A prior success
// does not survive a changed source, stage, journal, lease or handle set.
func (p *EditedExchangePlan) Verify(ctx context.Context) error {
	if p == nil || p.snapshot == nil || p.journal == nil || p.current == nil || p.drain == nil || ctx == nil {
		return ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := p.verifyPhysical(ctx); err != nil {
		return err
	}
	facts := CurrentDraftFacts{ProjectID: p.evidence.projectID, ProjectPath: p.evidence.projectPath,
		DraftID: p.evidence.DraftID, OperationHash: hex.EncodeToString(p.evidence.operationHash[:]),
		SourceTreeHash: hex.EncodeToString(p.evidence.SourceTreeSHA256[:]),
		EditedTreeHash: p.stageHash}
	if err := p.current.VerifyCurrentDraft(ctx, facts); err != nil {
		return err
	}
	if err := p.drain.ProveDrained(ctx, facts.ProjectPath); err != nil {
		return err
	}
	// Both callbacks may wait, close handles or trigger state changes. Repeat
	// physical and currentness checks before this plan can be observed.
	if err := p.verifyPhysical(ctx); err != nil {
		return err
	}
	return p.current.VerifyCurrentDraft(ctx, facts)
}

func (p *EditedExchangePlan) verifyPhysical(ctx context.Context) error {
	s := p.snapshot
	e := p.evidence
	j := p.journal
	if e.DraftID <= 0 || e.projectID == "" || e.projectPath == "" || e.stagePath == "" ||
		p.stageHash == "" || !validJournalName(j.name) || j.lock == nil || j.root == nil {
		return ErrInvalid
	}
	if err := s.VerifySource(ctx); err != nil {
		return err
	}
	observed, err := captureEditedStage(ctx, s)
	if err != nil {
		return err
	}
	s.mu.Lock()
	if s.closed || s.removed || s.parent == nil || s.lock == nil || s.projectID != e.projectID ||
		s.projectPath != e.projectPath || s.stage != e.stagePath ||
		!sameDevice(s.storageInfo, s.stageInfo) || !sameDevice(s.storageInfo, s.parentInfo) ||
		!os.SameFile(s.stageInfo, observed.inode) || verifyExchangeRootsPrivate(s) != nil ||
		hashTreeEntries(s.entries) != hex.EncodeToString(e.SourceTreeSHA256[:]) {
		s.mu.Unlock()
		return ErrChanged
	}
	dataRoot, projectID, projectPath, stageParent := s.dataRoot, s.projectID, s.projectPath, s.parentPath
	stageName, sourceInfo, stageInfo, parentInfo := s.stageName, s.storageInfo, s.stageInfo, s.parentInfo
	s.mu.Unlock()
	if observed.digest != p.stageHash || !sameChangedPaths(observed.changedPaths, e.changedSQLite) {
		return ErrChanged
	}
	proof, err := e.VerifyEditedDraftCAS(ctx, EditedStageFacts{ProjectID: projectID, ProjectPath: projectPath,
		StagePath: filepath.Join(stageParent, stageName), SourceTreeHash: hex.EncodeToString(e.SourceTreeSHA256[:]),
		EditedTreeHash: observed.digest, ChangedSQLite: observed.changedPaths})
	if err != nil {
		return err
	}
	prepared := j.prepared
	sourceID, sourceOK := inodeOf(sourceInfo)
	stageID, stageOK := inodeOf(stageInfo)
	parentID, parentOK := inodeOf(parentInfo)
	if !sourceOK || !stageOK || !parentOK || prepared.Source != sourceID || prepared.Stage != stageID ||
		prepared.Parent != parentID || prepared.DataRoot != dataRoot || prepared.ProjectID != projectID ||
		prepared.ProjectPath != projectPath || prepared.StageParent != stageParent || prepared.StageName != stageName ||
		prepared.SourceTreeHash != hex.EncodeToString(e.SourceTreeSHA256[:]) || prepared.StageTreeHash != observed.digest ||
		prepared.EditedDraftID != e.DraftID || prepared.EditedOperationHash != proof.OperationHash ||
		prepared.EditedProofHash != proof.ReadbackHash || prepared.EntryCount != observed.entryCount ||
		prepared.TotalBytes != observed.totalBytes || !validJournalHash(j.rawHash) {
		return ErrChanged
	}
	journalRaw, err := readJournalFile(j.root, j.name)
	if err != nil || journalDigest(journalRaw) != j.rawHash {
		return ErrJournalInvalid
	}
	var onDisk journalPrepared
	if err := strictJournalJSON(journalRaw, &onDisk); err != nil || onDisk != prepared {
		return ErrJournalInvalid
	}
	recovery, err := RecoverExchangeJournal(ctx, s.lock, dataRoot, projectID, projectPath, stageParent, j.name)
	if err != nil {
		return err
	}
	if recovery.Class != JournalPrepared || recovery.JournalID != prepared.ID ||
		recovery.EditedDraftID != e.DraftID || recovery.EditedOperationHash != proof.OperationHash ||
		recovery.EditedProofHash != proof.ReadbackHash {
		return ErrChanged
	}
	return nil
}

// Exchange remains unavailable. A PREPARED marker and edited-stage proof do
// not establish post-swap re-admission, rollback or unknown-outcome handling.
func (*EditedExchangePlan) Exchange(context.Context) error { return ErrPublishUnavailable }

// EditedExchangeClaim ties one physical exchange to the coordinator's live,
// project-scoped commit fence and the exact legacy lock used by the snapshot.
// Its fields are caller-supplied labels, not authority; VerifyCommit validates
// them against the coordinator's private state immediately before the syscall.
type EditedExchangeClaim struct {
	Held     *writerlock.Lock
	Fence    *projectsession.PublishFence
	Owner    projectsession.Owner
	Expected projectsession.Context
	OldKey   string
}

// ExchangeWithFence performs only the atomic namespace exchange. It does not
// certify durability or acknowledge a save. The caller must retain the old
// tree, observe both inodes, persist SWITCHED, re-admit and read back the new
// source, then clear the catalog hold before any acknowledgement. A syscall
// attempt is one-shot even when the kernel reports an error: the result may be
// ambiguous, so recovery must classify it from the PREPARED journal.
func (p *EditedExchangePlan) ExchangeWithFence(ctx context.Context, claim EditedExchangeClaim) error {
	if p == nil || ctx == nil || claim.Held == nil || claim.Fence == nil || claim.OldKey == "" {
		return ErrInvalid
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.attempted {
		return ErrExchangeOutcomeUnknown
	}
	if err := claim.Fence.VerifyCommit(ctx, claim.Held, claim.Owner, claim.Expected, claim.OldKey); err != nil {
		return err
	}
	if err := p.Verify(ctx); err != nil {
		return err
	}
	if err := claim.Fence.VerifyCommit(ctx, claim.Held, claim.Owner, claim.Expected, claim.OldKey); err != nil {
		return err
	}
	s := p.snapshot
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.removed || s.parent == nil || s.lock != claim.Held ||
		s.projectID != claim.Expected.ProjectID || s.projectPath != claim.Expected.ProjectPath ||
		!s.journalOwned || p.journal.lock != claim.Held {
		return ErrChanged
	}
	if err := verifyExchangeRootsPrivate(s); err != nil {
		return err
	}
	sourceParent, sourceParentInfo, err := openPinnedDirectory(s.projectPath, s.stageInfo)
	if err != nil {
		return err
	}
	defer sourceParent.Close()
	if !os.SameFile(sourceParentInfo, s.projectRootInfo) {
		return ErrChanged
	}
	sourceNow, sourceErr := sourceParent.Lstat(projectfs.StorageDirectory)
	stageNow, stageErr := s.parent.Lstat(s.stageName)
	if sourceErr != nil || stageErr != nil || !os.SameFile(sourceNow, s.storageInfo) ||
		!os.SameFile(stageNow, s.stageInfo) {
		return ErrChanged
	}
	journalRaw, err := readJournalFile(p.journal.root, p.journal.name)
	if err != nil || journalDigest(journalRaw) != p.journal.rawHash {
		return ErrJournalInvalid
	}
	var prepared journalPrepared
	if err := strictJournalJSON(journalRaw, &prepared); err != nil || prepared != p.journal.prepared {
		return ErrJournalInvalid
	}
	sourceFD, err := sourceParent.Open(".")
	if err != nil {
		return err
	}
	defer sourceFD.Close()
	stageFD, err := s.parent.Open(".")
	if err != nil {
		return err
	}
	defer stageFD.Close()
	callNumber, ok := renameat2SyscallNumber()
	if !ok {
		return ErrPublishUnavailable
	}
	sourceName, err := syscall.BytePtrFromString(projectfs.StorageDirectory)
	if err != nil {
		return ErrInvalid
	}
	stageName, err := syscall.BytePtrFromString(s.stageName)
	if err != nil {
		return ErrInvalid
	}
	if err := claim.Fence.VerifyCommit(ctx, claim.Held, claim.Owner, claim.Expected, claim.OldKey); err != nil {
		return err
	}
	// Nothing that may return an error is permitted between this assignment
	// and the syscall. The journal owns the stage even on an uncertain result.
	p.attempted = true
	const renameExchange = 2
	_, _, callErr := syscall.RawSyscall6(callNumber,
		sourceFD.Fd(), uintptr(unsafe.Pointer(sourceName)),
		stageFD.Fd(), uintptr(unsafe.Pointer(stageName)), renameExchange, 0)
	runtime.KeepAlive(sourceName)
	runtime.KeepAlive(stageName)
	if callErr != 0 {
		return errors.Join(ErrExchangeOutcomeUnknown, callErr)
	}
	return nil
}

func renameat2SyscallNumber() (uintptr, bool) {
	switch runtime.GOARCH {
	case "amd64":
		return 316, true
	case "arm64":
		return 276, true
	default:
		return 0, false
	}
}
