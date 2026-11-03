package transport

import (
	"context"
	"errors"
	"net/http"
	"unicode/utf8"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/knowledgequery"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
)

// KnowledgeSearchProvider lends the FTS projection of the same native-admitted
// copied snapshot as the knowledge reads. The provider must fence the exact
// cookie session, tab, project identity and lease until release is called.
type KnowledgeSearchProvider interface {
	BeginKnowledgeSearch(context.Context, Caller, any) (*knowledgequery.SearchCapture, func(), error)
}

type KnowledgeSearchProviderFunc func(context.Context, Caller, any) (*knowledgequery.SearchCapture, func(), error)

func (f KnowledgeSearchProviderFunc) BeginKnowledgeSearch(ctx context.Context, caller Caller, expected any) (*knowledgequery.SearchCapture, func(), error) {
	if f == nil {
		return nil, nil, errors.New("KNOWLEDGE_PROVIDER_REQUIRED")
	}
	return f(ctx, caller, expected)
}

// BindKnowledgeSearch installs the original kb:search DTO for the bounded FTS
// fallback. Embedding dispatch requires separate native query-space admission.
func (s *Server) BindKnowledgeSearch(provider KnowledgeSearchProvider) error {
	if provider == nil {
		return errors.New("KNOWLEDGE_PROVIDER_REQUIRED")
	}
	return s.Bind("kb:search", func(ctx context.Context, caller Caller, args []any) (any, error) {
		fail := func(err error) (any, error) { return nil, knowledgeReadError(err) }
		if len(args) != 4 {
			return fail(security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS"))
		}
		expected, err := draftExpectedContext(args, 3)
		if err != nil {
			return fail(err)
		}
		query, ok := args[0].(string)
		if !ok || !utf8.ValidString(query) || len(query) > 32*1024 {
			return fail(security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS"))
		}
		topK := int64(5)
		if args[1] != nil {
			if topK, ok = draftSafeInteger(args[1], 0); !ok || topK > 100 {
				return fail(security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS"))
			}
		}
		path, ok := args[2].(string)
		if !ok || path == "" || !utf8.ValidString(path) || int64(len(path)) > security.DefaultBodyLimit {
			return fail(security.NewHTTPError(http.StatusBadRequest, "PROJECT_CONTEXT_REQUIRED"))
		}
		if path != expected["projectPath"] {
			return fail(security.NewHTTPError(http.StatusConflict, "PROJECT_CONTEXT_CHANGED"))
		}
		if ctx == nil {
			return fail(errors.New("KNOWLEDGE_READ_FAILED"))
		}
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		capture, release, err := provider.BeginKnowledgeSearch(ctx, caller, expected)
		if release != nil {
			defer release()
		}
		if err != nil {
			return fail(err)
		}
		if capture == nil || release == nil {
			return fail(errors.New("KNOWLEDGE_PROVIDER_UNAVAILABLE"))
		}
		result, err := capture.SearchFTS(ctx, query, int(topK), nil, nil)
		if err != nil {
			if errors.Is(err, knowledgequery.ErrInput) {
				return fail(security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS"))
			}
			return fail(err)
		}
		return result, nil
	})
}
