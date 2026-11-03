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
// content identity, canonical asset membership and optional native knowledge
// capture) while the caller's legacy writer lock remains held, then requires:
//
//   - the schema to be exactly the current supported version;
//   - the legacy gateway to be excluded from cooperating writers;
//   - the mandatory knowledge preflight to have run (config.Knowledge required);
//   - no present LanceDB/embedding-registry/knowledge-copy asset and no legacy
//     knowledge receipt, because a durable native writer for those is not yet
//     proven and degrading them would destroy author data.
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
	if config.Knowledge == nil {
		// The write gate cannot claim knowledge is absent without running the
		// mandatory knowledge preflight, exactly like the existing write evidence.
		return nil, ErrConfiguration
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Run the knowledge-capturing preflight so the permit can prove knowledge
	// assets are genuinely absent rather than merely unclaimed. The captured
	// evidence is kept on the permit for the caller's audit.
	var captured KnowledgeRead
	report, err := PreflightUnderLockWithKnowledge(ctx, config, held, &captured)
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
	knowledge := report.Knowledge
	if assets.LanceDB || assets.EmbeddingRegistry || assets.KnowledgeCopies {
		return nil, ErrAuthorKnowledgeUnproved
	}
	if knowledge == nil {
		return nil, ErrAuthorKnowledgeUnproved
	}
	if knowledge.Physical.LanceDB || knowledge.Physical.EmbeddingRegistry || knowledge.Physical.KnowledgeCopies ||
		knowledge.Physical.LegacyReceipt || knowledge.Metadata.RegistryPresent || knowledge.Metadata.CopiesPresent {
		return nil, ErrAuthorKnowledgeUnproved
	}
	return &AdmittedAuthor{config: config, project: project, held: held, evidence: evidence}, nil
}

// Verify re-proves, against the exact same project, that the legacy writer lock
// is still held for the configured data root, that no portable authority has
// appeared, and that the original database's on-disk content identity still
// matches admit time. Content hashing is the same bounded read used by the
// snapshot verifier; it is intentionally costed like the read path.
func (a *AdmittedAuthor) Verify(ctx context.Context, project *projectfs.Project) error {
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
		return err
	}
	if identity != a.evidence.ContentIdentity {
		return ErrAuthorStale
	}
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