package transport

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/promptskills"
)

// familyOptionalStub satisfies both optional seams the two newest family
// binders accept. The pin test only needs the binder to install its exact
// operation set, so every provider method is unreachable and refuses.
type familyOptionalStub struct{}

func (familyOptionalStub) BeginPromptSkills(context.Context, Caller) (*promptskills.Service, func(), error) {
	return nil, nil, errors.New("FAMILY_PIN_STUB_UNUSED")
}

func (familyOptionalStub) RemoveRecentNavigation(context.Context, Caller, string) (any, error) {
	return nil, errors.New("FAMILY_PIN_STUB_UNUSED")
}

// Each family binder is pinned to its exact operation set. The durable lane
// composes these binders, so a name silently disappearing from one of them is
// a migration regression even though every other test still passes.
func TestFamilyBindersPinTheirExactOperationSets(t *testing.T) {
	cases := []struct {
		name string
		bind func(*Server) error
		want []string
	}{
		{
			name: "generation",
			bind: func(s *Server) error { return s.BindGeneration(&GenerationProvider{}) },
			want: []string{
				"finalization-generation:cancel", "generation:begin", "generation:bind-material-decision",
				"generation:cancel", "generation:discard-candidate", "generation:execute", "generation:list",
				"generation:pause", "generation:read", "generation:restart", "generation:resume",
				"generation:retry-draft-short-outline", "graph-generation:cancel",
			},
		},
		{
			name: "graph-generation",
			bind: func(s *Server) error { return s.BindGraphGeneration(&GraphGenerationProvider{}) },
			want: []string{"graph-generation:read"},
		},
		{
			// The finalization slot read is bound only when the provider
			// implements it, so the family grows by exactly that one name.
			name: "graph-generation-with-finalization-read",
			bind: func(s *Server) error {
				return s.BindGraphGeneration(&GraphGenerationProvider{
					Read:             func(context.Context, Caller, map[string]any, any) (any, error) { return nil, nil },
					FinalizationRead: func(context.Context, Caller, map[string]any, any) (any, error) { return nil, nil },
				})
			},
			want: []string{"finalization-generation:read", "graph-generation:read"},
		},
		{
			name: "review-revisions",
			bind: func(s *Server) error {
				return s.BindReviewRevisions(ReviewRevisionProviderFunc(nil))
			},
			want: []string{
				"db:review-create", "db:review-get-full", "db:review-get-latest", "db:review-list",
				"db:review-next-index", "db:revision-create", "db:revision-get-full", "db:revision-get-pending",
				"db:revision-list", "db:revision-mark-discarded", "db:revision-mark-merged", "db:revision-merge",
				"db:revision-next-index", "db:revision-replace-pending",
			},
		},
		{
			name: "review-revision-remainder",
			bind: func(s *Server) error {
				return s.BindReviewRevisionRemainder(ReviewRevisionProviderFunc(nil))
			},
			want: []string{
				"db:review-create", "db:review-get-latest", "db:review-next-index",
				"db:revision-create", "db:revision-mark-discarded", "db:revision-mark-merged",
				"db:revision-merge", "db:revision-next-index", "db:revision-replace-pending",
			},
		},
		{
			name: "import-runs",
			bind: func(s *Server) error { return s.BindImportRuns(ImportRunProviderFunc(nil)) },
			want: []string{
				"db:import-run-advance-stage", "db:import-run-cancel-at-boundary",
				"db:import-run-complete", "db:import-run-complete-batch", "db:import-run-get",
				"db:import-run-list-chapters", "db:import-run-list-resumable",
				"db:import-run-renew-execution", "db:import-run-request-cancel",
				"db:import-run-start-resume",
			},
		},
		{
			name: "finalization",
			bind: func(s *Server) error {
				return s.BindFinalization(FinalizationProviderFunc(nil))
			},
			want: []string{
				"db:finalization-link-knowledge-document", "finalization:commit", "finalization:retry",
			},
		},
		{
			name: "prompt-skills-writes",
			bind: func(s *Server) error { return s.BindPromptSkillsWrites(familyOptionalStub{}) },
			want: []string{"prompt:delete-global", "prompt:save-global", "skills:uninstall-user"},
		},
		{
			name: "project-recent-remove",
			bind: func(s *Server) error { return s.BindProjectRecentRemove(familyOptionalStub{}) },
			want: []string{"project:recent-remove"},
		},
	}
	for _, c := range cases {
		server := fixture(t)
		if err := c.bind(server); err != nil {
			t.Fatalf("%s bind: %v", c.name, err)
		}
		got := server.BoundOperations()
		if !reflect.DeepEqual(got, c.want) {
			t.Fatalf("%s operations mismatch\n got %v\nwant %v", c.name, got, c.want)
		}
	}
}
