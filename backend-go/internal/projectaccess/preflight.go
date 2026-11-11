// Package projectaccess composes an isolated read-only author admission preflight.
// It never grants a writable original-project or repository capability. Config is
// trusted administrator input, not renderer/RPC data. Programs ignoring flock
// (notably the legacy desktop) must be stopped externally for the entire check.
package projectaccess

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/admission"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/writerlock"
)

var (
	ErrConfiguration = errors.New("AUTHOR_PREFLIGHT_CONFIGURATION_INVALID")
	ErrFilesystem    = errors.New("AUTHOR_PREFLIGHT_FILESYSTEM_UNSAFE")
	ErrChanged       = errors.New("AUTHOR_PREFLIGHT_SOURCE_CHANGED")
	ErrCleanup       = errors.New("AUTHOR_PREFLIGHT_CLEANUP_FAILED")
)

// Config roots must already exist and be administrator-owned. DataRoot is the
// exact legacy gateway NOVEL_DATA_ROOT, not the library or selected project root.
// ScratchParent must be private0700, outside DataRoot, never an author directory.
type Config struct {
	DataRoot, LibraryRoot, ProjectPath, ScratchParent string
	MaxDatabaseBytes                                  int64
	Schema                                            admission.SchemaContract
	Knowledge                                         *KnowledgeConfig
}

// Report contains integrity evidence only, not a path, source descriptor, prose,
// key or mutation handle. An optional native summary proves only this bounded
// copied-table domain, not arbitrary author compatibility, writable authority,
// portable restore support, populated legacy migration or production readiness.
//
// CanonicalKnowledgeProved reports that .ai-novel holds this lane's own knowledge
// store: a decodable canonical knowledge-snapshot.json whose reference-format
// copies name documents of that snapshot
// (internal/projectfs.ProveCanonicalKnowledge). It is the one fact that separates
// knowledge this lane wrote and can manage from a desktop asset it cannot, so a
// knowledge-copy footprint is refused only when this is false.
type Report struct {
	ProjectID                string                    `json:"projectId"`
	SQLite                   admission.Evidence        `json:"sqlite"`
	LegacyGatewayExcluded    bool                      `json:"legacyGatewayExcluded"`
	CanonicalAssets          projectfs.CanonicalAssets `json:"canonicalAssetEntries"`
	Knowledge                *KnowledgeEvidence        `json:"knowledge,omitempty"`
	CanonicalKnowledgeProved bool                      `json:"canonicalKnowledgeProved"`
}

func canonical(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && !strings.ContainsAny(path, "\x00\\")
}
func beneath(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}
func validate(config Config) error {
	for _, path := range []string{config.DataRoot, config.LibraryRoot, config.ProjectPath, config.ScratchParent} {
		if !canonical(path) {
			return ErrConfiguration
		}
	}
	if config.DataRoot == string(filepath.Separator) || config.MaxDatabaseBytes <= 0 || !beneath(config.DataRoot, config.LibraryRoot) || filepath.Dir(config.ProjectPath) != config.LibraryRoot || config.ScratchParent == config.DataRoot || beneath(config.DataRoot, config.ScratchParent) || beneath(config.ScratchParent, config.DataRoot) {
		return ErrConfiguration
	}
	if k := config.Knowledge; k != nil {
		if !validKnowledgeConfig(*k) {
			return ErrConfiguration
		}
	}
	return nil
}

// afterStage is a private test seam; exported Preflight cannot accept callbacks
// or caller-selected verification routines/schema from project metadata.
type stageHook func(*projectfs.Project) error

func Preflight(ctx context.Context, config Config) (Report, error) {
	return preflight(ctx, config, nil)
}

// PreflightUnderLock performs the same read-only admission while the caller
// retains the exact legacy gateway lock. This lets a future durable runtime
// keep one exclusion across admission and the subsequent database lifetime.
// The caller must keep the lock open, exclude non-cooperating writers, and
// close all database handles before releasing it. This does not grant a write
// capability or imply that a copied snapshot can be committed to author data.
func PreflightUnderLock(ctx context.Context, config Config, held *writerlock.Lock) (Report, error) {
	if held == nil {
		return Report{}, writerlock.ErrUnsafe
	}
	return preflightReadWithLock(ctx, config, nil, nil, held)
}

func preflight(ctx context.Context, config Config, afterStage stageHook) (Report, error) {
	return preflightRead(ctx, config, afterStage, nil)
}
func preflightRead(ctx context.Context, config Config, afterStage stageHook, capture *KnowledgeRead) (report Report, err error) {
	return preflightReadWithLock(ctx, config, afterStage, capture, nil)
}
func preflightReadWithLock(ctx context.Context, config Config, afterStage stageHook, capture *KnowledgeRead, held *writerlock.Lock) (report Report, err error) {
	defer func() {
		if err != nil && capture != nil {
			*capture = KnowledgeRead{}
		}
	}()
	if ctx == nil {
		return report, ErrConfiguration
	}
	if err := ctx.Err(); err != nil {
		return report, err
	}
	if err := validate(config); err != nil {
		return report, err
	}
	guard := held
	if guard == nil {
		guard, err = writerlock.AcquireLegacy(config.DataRoot)
		if err != nil {
			return report, err
		}
		defer func() {
			if closeErr := guard.Close(); closeErr != nil {
				report = Report{}
				err = errors.Join(err, ErrCleanup)
			}
		}()
	}
	if err := guard.VerifyLegacy(config.DataRoot); err != nil {
		return report, err
	}
	if err := ctx.Err(); err != nil {
		return report, err
	}
	library, err := projectfs.OpenLibrary(config.LibraryRoot)
	if err != nil {
		return report, ErrFilesystem
	}
	defer library.Close()
	project, err := library.OpenProject(config.ProjectPath)
	if err != nil {
		return report, ErrFilesystem
	}
	defer project.Close()
	if err := project.VerifyNoPortableAuthority(ctx); err != nil {
		return report, err
	}
	if err := guard.Verify(); err != nil {
		return report, err
	}
	assets, err := project.InventoryCanonicalAssets(ctx)
	if err != nil {
		return report, redact(err)
	}
	// OpenLibrary validates every physical ancestor before any scratch creation.
	scratchAnchor, err := projectfs.OpenLibrary(config.ScratchParent)
	if err != nil {
		return report, ErrFilesystem
	}
	defer scratchAnchor.Close()
	parentBefore, err := os.Lstat(config.ScratchParent)
	if err != nil || !safeScratch(config.ScratchParent) {
		return report, ErrFilesystem
	}
	temporary, err := os.MkdirTemp(config.ScratchParent, "author-preflight-")
	if err != nil {
		return report, ErrFilesystem
	}
	owned, err := os.Lstat(temporary)
	if err != nil {
		return report, ErrCleanup
	}
	var root *os.Root
	var knowledge *stagedKnowledge
	defer func() {
		// Clear fixed snapshot names via the pinned private directory even on rename.
		// Never remove a substituted host-path directory or unknown names.
		failed := false
		if knowledge != nil {
			if cleanupErr := knowledge.cleanup(); cleanupErr != nil {
				failed = true
			}
		}
		if root != nil {
			for _, name := range []string{"snapshot.db", "snapshot.db-wal", "snapshot.db-shm", "snapshot.db-journal"} {
				if removeErr := root.Remove(name); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
					failed = true
				}
			}
			if closeErr := root.Close(); closeErr != nil {
				failed = true
			}
		}
		named, statErr := os.Lstat(temporary)
		if statErr != nil || !os.SameFile(owned, named) {
			failed = true
		} else if removeErr := os.Remove(temporary); removeErr != nil {
			failed = true
		}
		if failed {
			report = Report{}
			err = errors.Join(err, ErrCleanup)
		}
	}()
	root, err = os.OpenRoot(temporary)
	if err != nil {
		return report, ErrFilesystem
	}
	proof, err := project.StageDatabase(ctx, root, config.MaxDatabaseBytes)
	if err != nil {
		return report, redact(err)
	}
	if config.Knowledge != nil {
		knowledge, err = stageKnowledge(ctx, project, root, *config.Knowledge)
		if err != nil {
			return report, err
		}
	}
	if err := guard.Verify(); err != nil {
		return report, err
	}
	if afterStage != nil {
		if err := afterStage(project); err != nil {
			return report, err
		}
	}
	evidence, err := admission.VerifySnapshot(ctx, filepath.Join(temporary, "snapshot.db"), config.Schema)
	if err != nil {
		return report, err
	}
	if capture != nil {
		if knowledge == nil || config.Knowledge == nil || !config.Knowledge.Native {
			return report, ErrConfiguration
		}
		if err := captureKnowledgeRead(ctx, knowledge, filepath.Join(temporary, "snapshot.db"), config.Knowledge.Metadata, capture); err != nil {
			return report, err
		}
	}
	if err := guard.Verify(); err != nil {
		return report, err
	}
	if err := project.VerifyNoPortableAuthority(ctx); err != nil {
		return report, err
	}
	if err := proof.VerifyUnchanged(ctx); err != nil {
		return report, redact(err)
	}
	if err := assets.VerifyUnchanged(ctx); err != nil {
		return report, redact(err)
	}
	if knowledge != nil {
		if err := knowledge.verify(ctx); err != nil {
			return report, err
		}
	}
	parentAfter, err := os.Lstat(config.ScratchParent)
	if err != nil || !os.SameFile(parentBefore, parentAfter) || !safeScratch(config.ScratchParent) {
		return report, ErrChanged
	}
	if err := guard.Verify(); err != nil {
		return report, err
	}
	if err := ctx.Err(); err != nil {
		return report, err
	}
	// The canonical knowledge proof is taken last, after every earlier check has
	// verified the source, so the decision rests on the freshest state this
	// preflight can observe under the continuously held legacy lock.
	canonicalKnowledge, err := project.ProveCanonicalKnowledge(ctx)
	if err != nil {
		return report, redact(err)
	}
	report = Report{
		ProjectID:                project.ProjectID(),
		SQLite:                   evidence,
		LegacyGatewayExcluded:    true,
		CanonicalAssets:          assets.Summary(),
		CanonicalKnowledgeProved: canonicalKnowledge.Proved(),
	}
	if knowledge != nil {
		value := knowledge.evidence
		report.Knowledge = &value
	}
	return report, nil
}
func redact(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		code := error(context.Canceled)
		if errors.Is(err, context.DeadlineExceeded) {
			code = context.DeadlineExceeded
		}
		if errors.Is(err, projectfs.ErrKnowledgeCleanup) {
			return errors.Join(code, ErrCleanup)
		}
		return code
	}
	for _, code := range []error{projectfs.ErrChanged, projectfs.ErrManifestChanged} {
		if errors.Is(err, code) {
			return ErrChanged
		}
	}
	return ErrFilesystem
}
