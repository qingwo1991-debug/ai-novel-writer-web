package projectfs

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/writerlock"
)

// LockedSnapshot binds a complete private SQLite/WAL/journal copy to the
// original gateway writer lock and its source-project proof. It is evidence for
// a future rollback plan; it does not expose a writable source descriptor or
// provide restore/publish authority. The caller owns the project, destination,
// and lock, and must keep all three alive through VerifyUnchanged.
type LockedSnapshot struct {
	proof    *SnapshotProof
	lock     *writerlock.Lock
	dataRoot string
}

// StageDatabaseUnderLegacyLock copies the complete canonical database set
// while the exact legacy gateway lock remains held. dataRoot is trusted server
// configuration; the project must lie beneath that root. destination must be
// a caller-owned private scratch root outside the author tree, with no other
// writer. In particular, this copy is not itself a safe author rollback: a
// future publisher must still prove transaction consistency, durable restore,
// and all knowledge-asset authority before enabling source writes.
func (p *Project) StageDatabaseUnderLegacyLock(ctx context.Context, held *writerlock.Lock, dataRoot string, destination *os.Root, maxBytes int64) (*LockedSnapshot, error) {
	if ctx == nil || p == nil || p.library == nil || held == nil || destination == nil {
		return nil, ErrInvalidPath
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if dataRoot == "/" || !canonicalAbsolute(dataRoot) || !beneathDataRoot(dataRoot, p.library.path) {
		return nil, ErrInvalidPath
	}
	if err := held.VerifyLegacy(dataRoot); err != nil {
		return nil, err
	}
	if err := p.VerifyNoPortableAuthority(ctx); err != nil {
		return nil, err
	}
	proof, err := p.StageDatabase(ctx, destination, maxBytes)
	if err != nil {
		return nil, err
	}
	result := &LockedSnapshot{proof: proof, lock: held, dataRoot: dataRoot}
	if err := result.VerifyUnchanged(ctx); err != nil {
		return nil, err
	}
	return result, nil
}

func beneathDataRoot(dataRoot, child string) bool {
	rel, err := filepath.Rel(dataRoot, child)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// VerifyUnchanged rehashes the source after checking the same live legacy
// lock on both sides. A closed/replaced lock or changed source fails closed.
func (snapshot *LockedSnapshot) VerifyUnchanged(ctx context.Context) error {
	if snapshot == nil || snapshot.proof == nil || snapshot.lock == nil || ctx == nil {
		return ErrInvalidPath
	}
	if err := snapshot.lock.VerifyLegacy(snapshot.dataRoot); err != nil {
		return err
	}
	if err := snapshot.proof.VerifyUnchanged(ctx); err != nil {
		return err
	}
	return snapshot.lock.VerifyLegacy(snapshot.dataRoot)
}
