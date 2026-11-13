//go:build linux

package projectaccess

import (
	"context"
	"errors"
	"path/filepath"
	"sync"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/admission"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/writerlock"
)

var (
	// ErrAuthorSchemaUnsupported means the project is not a current schema whose
	// writer semantics are proven. It fails closed instead of writing.
	ErrAuthorSchemaUnsupported = errors.New("AUTHOR_WRITE_SCHEMA_UNSUPPORTED")
	// ErrAuthorKnowledgeUnproved means a knowledge asset is present without a
	// complete native write authority. It fails closed instead of degrading it.
	ErrAuthorKnowledgeUnproved = errors.New("AUTHOR_WRITE_KNOWLEDGE_UNPROVED")
	// ErrAuthorStale means the original database content identity no longer
	// matches the admit-time digest; the project must be re-admitted.
	ErrAuthorStale = errors.New("AUTHOR_WRITE_STALE")
)

// AdmittedAuthor is an admission permit for one exact author project. It
// implements projectfs.AuthorPermit so the confined write handle can re-prove
// authority before and after opening the original database. It never releases
// the caller's legacy writer lock; the caller owns the lock for the whole
// runtime and must keep it held until every handle is closed.
type AdmittedAuthor struct {
	mu       sync.Mutex
	config   Config
	project  *projectfs.Project
	held     *writerlock.Lock
	evidence admission.Evidence
	closed   bool
}

// AdmitAuthor runs the complete preflight (schema fingerprint, row integrity,
// content identity, canonical asset membership and, when the caller opts in,
// knowledge staging) while the caller's legacy writer lock remains held, then
// requires:
//
//   - the schema to be exactly the current supported version;
//   - the legacy gateway to be excluded from cooperating writers;
//   - no present LanceDB or embedding-registry canonical asset, and no
//     knowledge-copy footprint that does not belong to a decodable canonical
//     knowledge snapshot this lane wrote — a durable native writer for the
//     desktop's assets is not yet proven, and degrading them would destroy author
//     data, while the lane's own store must stay openable;
//   - when knowledge staging was configured, no legacy knowledge receipt or
//     registry footprint either.
//
// It does not open the database, write any source file, or grant a lease; those
// are the caller's next steps through projectfs.OpenAuthorDatabase.
func AdmitAuthor(ctx context.Context, config Config, project *projectfs.Project, held *writerlock.Lock) (*AdmittedAuthor, error) {
	if ctx == nil || project == nil || held == nil {
		return nil, ErrConfiguration
	}
	if config.ProjectPath != project.Path() {
		// The permit must prove one exact project; never a mistyped wrapper.
		return nil, ErrConfiguration
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	report, err := PreflightUnderLock(ctx, config, held)
	if err != nil {
		return nil, err
	}
	evidence := report.SQLite
	if evidence.UserVersion != admission.CurrentSchemaVersion {
		return nil, ErrAuthorSchemaUnsupported
	}
	if !report.LegacyGatewayExcluded {
		return nil, ErrAuthorSchemaUnsupported
	}
	assets := report.CanonicalAssets
	// An asset that is present but EMPTY is not author data: projectfs reports a
	// fixed asset directory as absent when it holds no entries, because the shipped
	// gateway leaves an empty .ai-novel/lancedb behind after the knowledge base is
	// cleared. Anything with bytes in it still refuses here.
	//
	// LanceDB and the embedding-space registry have no Go writer at all, so they
	// refuse unconditionally. A knowledge-copy footprint is different: this lane's
	// own knowledge operations write copies beside a canonical
	// knowledge-snapshot.json that its served reads decode, so copies that belong
	// to that snapshot are knowledge this writer can leave alone rather than
	// degrade. Copies WITHOUT such a snapshot (the desktop footprint, which has no
	// canonical JSON store) stay exactly as unproved as before.
	if assets.LanceDB || assets.EmbeddingRegistry {
		return nil, ErrAuthorKnowledgeUnproved
	}
	if assets.KnowledgeCopies && !report.CanonicalKnowledgeProved {
		return nil, ErrAuthorKnowledgeUnproved
	}
	if knowledge := report.Knowledge; knowledge != nil {
		if knowledge.Physical.LanceDB || knowledge.Physical.EmbeddingRegistry ||
			knowledge.Physical.LegacyReceipt || knowledge.Metadata.RegistryPresent {
			return nil, ErrAuthorKnowledgeUnproved
		}
		if (knowledge.Physical.KnowledgeCopies || knowledge.Metadata.CopiesPresent) &&
			!report.CanonicalKnowledgeProved {
			return nil, ErrAuthorKnowledgeUnproved
		}
	}
	return &AdmittedAuthor{config: config, project: project, held: held, evidence: evidence}, nil
}

// Verify re-proves, against the exact same project, that the legacy writer lock
// is still held for the configured data root, that no portable authority has
// appeared, and that the original database is still the hashable author file
// this permit admits.
//
// A changed content identity is refused, because a caller that froze a source
// under this permit must not be handed a file it can no longer prove it owns.
// Callers that serve an unfrozen view use VerifyAdopt.
func (a *AdmittedAuthor) Verify(ctx context.Context, project *projectfs.Project) error {
	return a.verify(ctx, project, false)
}

// VerifyAdopt is Verify for a borrow that does not depend on a frozen source: it
// proves the same live invariants (legacy lock still held, no portable
// authority, the database still the hashable author file) and then ADOPTS the
// content identity it read instead of refusing a change.
//
// The exclusive legacy lock is what protects the database: while this process
// holds it, every content change on disk came from this process's own committed
// write. Refusing the change is what answered concurrent reads with
// AUTHOR_WRITE_STALE while a generation was mid-write, hiding rows that were
// already durably committed.
func (a *AdmittedAuthor) VerifyAdopt(ctx context.Context, project *projectfs.Project) error {
	return a.verify(ctx, project, true)
}

func (a *AdmittedAuthor) verify(ctx context.Context, project *projectfs.Project, adopt bool) error {
	if a == nil {
		return ErrConfiguration
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return projectfs.ErrClosed
	}
	if ctx == nil {
		return ErrConfiguration
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if project != a.project {
		return ErrConfiguration
	}
	if err := a.held.VerifyLegacy(a.config.DataRoot); err != nil {
		return err
	}
	if err := project.VerifyNoPortableAuthority(ctx); err != nil {
		return err
	}
	identity, err := admission.ContentIdentityOf(ctx, filepath.Join(a.config.ProjectPath, projectfs.StorageDirectory, projectfs.DatabaseName))
	if err != nil {
		// A database that cannot be re-hashed (missing, replaced by junk,
		// unsafe entry) is by definition not provably the admitted one.
		return ErrAuthorStale
	}
	if !adopt && identity != a.evidence.ContentIdentity {
		return ErrAuthorStale
	}
	a.evidence.ContentIdentity = identity
	return a.held.VerifyLegacy(a.config.DataRoot)
}

// Refresh adopts the current on-disk content identity after a write that this
// permit authorized, so later Verifies compare against the new committed state.
// It re-checks the retained lock first and re-reads the digest; it never
// accepts a digest change while the lock is not held.
func (a *AdmittedAuthor) Refresh(ctx context.Context) error {
	if a == nil {
		return ErrConfiguration
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return projectfs.ErrClosed
	}
	if ctx == nil {
		return ErrConfiguration
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := a.held.VerifyLegacy(a.config.DataRoot); err != nil {
		return err
	}
	identity, err := admission.ContentIdentityOf(ctx, filepath.Join(a.config.ProjectPath, projectfs.StorageDirectory, projectfs.DatabaseName))
	if err != nil {
		return ErrAuthorStale
	}
	a.evidence.ContentIdentity = identity
	return a.held.VerifyLegacy(a.config.DataRoot)
}

// Close invalidates the permit for future Verifies. It does not close the
// caller's legacy lock, the project, or any open database handle; the caller
// owns those.
func (a *AdmittedAuthor) Close() error {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.closed = true
	return nil
}
