package projectaccess

import (
	"context"
	"errors"
	"os"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/knowledgecopy"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/knowledgedomain"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
)

// KnowledgeConfig is trusted administrator input. Opting in adds a complete
// physical asset copy and bounded read-only metadata validation. Native additionally
// opts into bounded experimental copied-table domain inspection under the same
// legacy exclusion, only in a native build. No original author write access is granted.
type KnowledgeConfig struct {
	Copy     projectfs.KnowledgeBudget
	Metadata knowledgecopy.Budget
	Native   bool
}

// KnowledgeEvidence contains no paths, prose, IDs, registry values or handles.
type KnowledgeEvidence struct {
	Physical  projectfs.KnowledgeSummary `json:"physical"`
	Metadata  knowledgecopy.Summary      `json:"metadata"`
	Native    *knowledgedomain.Summary   `json:"native,omitempty"`
	CopyLinks *knowledgecopy.LinkSummary `json:"copyLinks,omitempty"`
}

type stagedKnowledge struct {
	parent       *os.Root
	root         *os.Root
	info         os.FileInfo
	proof        *projectfs.KnowledgeProof
	evidence     KnowledgeEvidence
	readSnapshot knowledgedomain.Snapshot
}

func validKnowledgeConfig(config KnowledgeConfig) bool {
	return config.Copy.MaxBytes > 0 && config.Copy.MaxBytes <= 1<<40 && config.Copy.MaxEntries > 0 && config.Copy.MaxEntries <= 10000 && config.Copy.MaxDepth > 0 && config.Copy.MaxDepth <= 64 && knowledgecopy.ValidateBudget(config.Metadata) == nil && (!config.Native || nativeKnowledgeSupported())
}
func stageKnowledge(ctx context.Context, project *projectfs.Project, parent *os.Root, config KnowledgeConfig) (stage *stagedKnowledge, err error) {
	if err := parent.Mkdir("knowledge", 0700); err != nil {
		return nil, ErrFilesystem
	}
	stage = &stagedKnowledge{parent: parent}
	ownedStage := stage
	defer func() {
		if err != nil {
			err = errors.Join(err, ownedStage.cleanup())
			stage = nil
		}
	}()
	stage.info, err = parent.Lstat("knowledge")
	if err != nil {
		return stage, ErrCleanup
	}
	stage.root, err = parent.OpenRoot("knowledge")
	if err != nil {
		return stage, ErrFilesystem
	}
	opened, err := stage.root.Stat(".")
	if err != nil || !os.SameFile(opened, stage.info) {
		return stage, ErrFilesystem
	}
	stage.proof, err = project.StageCanonicalKnowledge(ctx, stage.root, config.Copy)
	if err != nil {
		return stage, redact(err)
	}
	metadata, err := knowledgecopy.Validate(ctx, stage.root, config.Metadata)
	if err != nil {
		return stage, redact(err)
	}
	if err := stage.proof.VerifyCopy(ctx); err != nil {
		return stage, redact(err)
	}
	stage.evidence = KnowledgeEvidence{Physical: stage.proof.Summary(), Metadata: metadata}
	if config.Native {
		snapshot, summary, inspectErr := inspectNativeKnowledge(ctx, stage, config)
		if inspectErr != nil {
			return stage, inspectErr
		}
		refs := []knowledgecopy.DocumentRef{}
		for _, table := range snapshot.Tables {
			if table.Name == "documents" {
				for _, row := range table.Records {
					refs = append(refs, knowledgecopy.DocumentRef{ID: row["id"].(string), FilePath: row["filePath"].(string)})
				}
			}
		}
		links, linkErr := knowledgecopy.ValidateLinks(ctx, stage.root, refs, config.Metadata)
		if linkErr != nil {
			return stage, redact(linkErr)
		}
		stage.evidence.CopyLinks = &links
		stage.evidence.Native = &summary
		stage.readSnapshot = snapshot // Private bounded rows for optional immutable read capture only.
		if err := stage.verify(ctx); err != nil {
			return stage, err
		}
	}
	return stage, nil
}
func (s *stagedKnowledge) verify(ctx context.Context) error {
	if s == nil || s.proof == nil {
		return ErrFilesystem
	}
	if err := s.proof.VerifyUnchanged(ctx); err != nil {
		return redact(err)
	}
	if err := s.proof.VerifyCopy(ctx); err != nil {
		return redact(err)
	}
	return nil
}

// Descriptor-confined cleanup never recurses over arbitrary replacement content.
// The proof owns every created asset; the coordinator owns just the empty child.
func (s *stagedKnowledge) cleanup() error {
	if s == nil {
		return nil
	}
	failed := false
	if s.proof != nil {
		if err := s.proof.Cleanup(); err != nil {
			failed = true
		}
	}
	if s.root != nil {
		if err := s.root.Close(); err != nil {
			failed = true
		}
	}
	named, err := s.parent.Lstat("knowledge")
	if err != nil || s.info == nil || !os.SameFile(s.info, named) {
		failed = true
	} else if err := s.parent.Remove("knowledge"); err != nil {
		failed = true
	}
	if failed {
		return ErrCleanup
	}
	return nil
}
