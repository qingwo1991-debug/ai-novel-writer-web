//go:build linux

package projectpublish

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
)

// HandleDrain is evidence from the owner of every live SQLite, Lance and
// project file handle. A successful callback is only a prerequisite for an
// eventual exchange; this package cannot infer closed handles from paths.
type HandleDrain interface {
	ProveDrained(context.Context, string) error
}

type HandleDrainFunc func(context.Context, string) error

func (f HandleDrainFunc) ProveDrained(ctx context.Context, projectPath string) error {
	if f == nil {
		return ErrPublishUnavailable
	}
	return f(ctx, projectPath)
}

// ExchangePlan records the immutable evidence available from an isolated
// storage-tree snapshot. It deliberately has no publication authority: the
// namespace exchange, durable journal and restart resolver are not present.
type ExchangePlan struct {
	snapshot         *Snapshot
	sourceInfo       fs.FileInfo
	stageInfo        fs.FileInfo
	parentInfo       fs.FileInfo
	projectPath      string
	entryCount       int
	totalBytes       int64
	sourceTreeSHA256 [32]byte
}

func (p *ExchangePlan) EntryCount() int {
	if p == nil {
		return 0
	}
	return p.entryCount
}

func (p *ExchangePlan) TotalBytes() int64 {
	if p == nil {
		return 0
	}
	return p.totalBytes
}

func (p *ExchangePlan) SourceTreeSHA256() [32]byte {
	if p == nil {
		return [32]byte{}
	}
	return p.sourceTreeSHA256
}

// PrepareExchangePlan checks the available preconditions without mutating the
// source or claiming that the private copy is a publish-ready replacement.
// Stage preserves each ordinary file and directory mode inside a private
// 0700 parent, while the stage and source storage roots remain private.
func PrepareExchangePlan(ctx context.Context, snapshot *Snapshot, drain HandleDrain) (*ExchangePlan, error) {
	if ctx == nil || snapshot == nil || drain == nil {
		return nil, ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := snapshot.VerifySource(ctx); err != nil {
		return nil, err
	}
	if err := snapshot.VerifyPrivateCopy(ctx); err != nil {
		return nil, err
	}
	snapshot.mu.Lock()
	if snapshot.closed || snapshot.removed || snapshot.lock == nil || snapshot.parent == nil ||
		!sameDevice(snapshot.storageInfo, snapshot.stageInfo) || !sameDevice(snapshot.storageInfo, snapshot.parentInfo) {
		snapshot.mu.Unlock()
		return nil, ErrInvalid
	}
	projectPath := snapshot.projectPath
	snapshot.mu.Unlock()
	if err := drain.ProveDrained(ctx, projectPath); err != nil {
		return nil, err
	}
	// The callback may have closed or changed a handle or storage entry. Repeat
	// both proofs after it, then capture the immutable identities under mu.
	if err := snapshot.VerifySource(ctx); err != nil {
		return nil, err
	}
	if err := snapshot.VerifyPrivateCopy(ctx); err != nil {
		return nil, err
	}
	snapshot.mu.Lock()
	defer snapshot.mu.Unlock()
	if snapshot.closed || snapshot.removed || snapshot.parent == nil ||
		!sameDevice(snapshot.storageInfo, snapshot.stageInfo) || !sameDevice(snapshot.storageInfo, snapshot.parentInfo) {
		return nil, ErrInvalid
	}
	if err := verifyExchangeRootsPrivate(snapshot); err != nil {
		return nil, ErrChanged
	}
	var size int64
	h := sha256.New()
	var length [8]byte
	for _, item := range snapshot.entries {
		if item.mode.IsRegular() {
			if item.size < 0 || item.size > snapshot.limits.MaxBytes-size {
				return nil, ErrPublishUnavailable
			}
			size += item.size
		} else if item.mode.IsDir() {
			// The source mode is preserved by Stage and compared by
			// VerifyPrivateCopy; the outer 0700 parent controls exposure.
		} else {
			return nil, ErrPublishUnavailable
		}
		binary.BigEndian.PutUint64(length[:], uint64(len(item.path)))
		_, _ = h.Write(length[:])
		_, _ = h.Write([]byte(item.path))
		binary.BigEndian.PutUint64(length[:], uint64(item.size))
		_, _ = h.Write(length[:])
		binary.BigEndian.PutUint64(length[:], uint64(item.mode))
		_, _ = h.Write(length[:])
		_, _ = h.Write(item.hash[:])
	}
	var digest [32]byte
	copy(digest[:], h.Sum(nil))
	return &ExchangePlan{snapshot: snapshot, sourceInfo: snapshot.storageInfo, stageInfo: snapshot.stageInfo,
		parentInfo: snapshot.parentInfo, projectPath: projectPath, entryCount: len(snapshot.entries),
		totalBytes: size, sourceTreeSHA256: digest}, nil
}

// The source root, stage root and stage parent must retain their exact private
// inodes. File modes inside those roots are separately proven by Snapshot.
// Caller holds snapshot.mu.
func verifyExchangeRootsPrivate(snapshot *Snapshot) error {
	parentNow, err := privateDirectory(snapshot.parentPath)
	if err != nil || !os.SameFile(parentNow, snapshot.parentInfo) {
		return ErrChanged
	}
	stageNow, err := privateDirectory(snapshot.stage)
	if err != nil || !os.SameFile(stageNow, snapshot.stageInfo) {
		return ErrChanged
	}
	storageNow, err := privateDirectory(filepath.Join(snapshot.projectPath, projectfs.StorageDirectory))
	if err != nil || !os.SameFile(storageNow, snapshot.storageInfo) {
		return ErrChanged
	}
	return nil
}

// Verify keeps a prepared plan tied to the original source, stage and lock.
// It does not make the private copy writable or recoverable after a crash.
func (p *ExchangePlan) Verify(ctx context.Context) error {
	if p == nil || p.snapshot == nil || ctx == nil {
		return ErrInvalid
	}
	if err := p.snapshot.VerifySource(ctx); err != nil {
		return err
	}
	if err := p.snapshot.VerifyPrivateCopy(ctx); err != nil {
		return err
	}
	p.snapshot.mu.Lock()
	defer p.snapshot.mu.Unlock()
	if p.snapshot.closed || p.snapshot.removed || p.snapshot.parent == nil || p.snapshot.projectPath != p.projectPath ||
		!os.SameFile(p.sourceInfo, p.snapshot.storageInfo) || !os.SameFile(p.stageInfo, p.snapshot.stageInfo) ||
		!os.SameFile(p.parentInfo, p.snapshot.parentInfo) {
		return ErrChanged
	}
	if err := verifyExchangeRootsPrivate(p.snapshot); err != nil {
		return err
	}
	visibleStage, err := p.snapshot.parent.Lstat(p.snapshot.stageName)
	if err != nil || !os.SameFile(visibleStage, p.stageInfo) {
		return ErrChanged
	}
	visibleSource, err := os.Lstat(filepath.Join(p.projectPath, projectfs.StorageDirectory))
	if err != nil || !os.SameFile(visibleSource, p.sourceInfo) {
		return ErrChanged
	}
	return nil
}

// Exchange is explicitly unavailable until an independently qualified
// PREPARED/SWITCHED journal, handle drain, atomic rename, fsync and restart
// resolver are implemented. A readiness plan cannot authorize mutation.
func (p *ExchangePlan) Exchange(context.Context) error {
	return ErrPublishUnavailable
}

// RecoverExchange is a conservative placeholder. No exchange journal is
// emitted by this package, so no source mutation can be inferred or repaired.
func RecoverExchange(context.Context, string) error { return ErrPublishUnavailable }
