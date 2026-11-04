package transport

import (
	"reflect"
	"testing"
)

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
