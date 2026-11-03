package transport

import (
	"context"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/archiveformat"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
)

// ArchiveExportAttempt owns a private unpublished staging writer and an exact
// one-use target grant. Commit must atomically make a completed archive
// available under a caller-owned download URL. Abort removes only attempt-owned
// bytes and is safe after Commit. The provider must independently prove the
// caller's exact session, tab, frozen project context and grant purpose.
type ArchiveExportAttempt struct {
	Snapshot archiveformat.ExportSnapshot
	Writer   io.WriteCloser
	Commit   func(archiveformat.ExportReceipt) (downloadURL, downloadName string, err error)
	Abort    func() error
}

type ArchiveExportProvider interface {
	BeginArchiveExport(context.Context, Caller, map[string]string, string) (*ArchiveExportAttempt, error)
}

type ArchiveExportProviderFunc func(context.Context, Caller, map[string]string, string) (*ArchiveExportAttempt, error)

var archiveDownloadURL = regexp.MustCompile(`^/api/downloads/[a-zA-Z0-9_-]{16,256}$`)

func (f ArchiveExportProviderFunc) BeginArchiveExport(ctx context.Context, caller Caller, expected map[string]string, grantID string) (*ArchiveExportAttempt, error) {
	if f == nil {
		return nil, errors.New("PORTABLE_EXPORT_AUTHORITY_REQUIRED")
	}
	return f(ctx, caller, expected, grantID)
}

// BindArchiveExport retains the original project:archive-export RPC envelope.
// There is intentionally no production binding until a durable project owner,
// schema-7 projector, asset snapshot and one-use download grant exist. A
// provider cannot obtain success by returning only ZIP bytes or a path.
func (s *Server) BindArchiveExport(provider ArchiveExportProvider, limits archiveformat.Limits) error {
	if provider == nil {
		return errors.New("PORTABLE_EXPORT_AUTHORITY_REQUIRED")
	}
	return s.Bind("project:archive-export", func(ctx context.Context, caller Caller, args []any) (any, error) {
		fail := func(code string) (any, error) {
			return map[string]any{"success": false, "error": code, "errorCode": code}, nil
		}
		if len(args) != 1 {
			return fail("PORTABLE_CONTEXT_INVALID")
		}
		request, ok := args[0].(map[string]any)
		if !ok || len(request) != 2 {
			return fail("PORTABLE_CONTEXT_INVALID")
		}
		grantID, ok := request["targetArchiveGrantId"].(string)
		if !ok || grantID == "" || len(grantID) > 512 || !utf8.ValidString(grantID) {
			return fail("PORTABLE_GRANT_INVALID")
		}
		contextValue, ok := request["projectSession"].(map[string]any)
		if !ok || len(contextValue) != 3 {
			return fail("PORTABLE_CONTEXT_INVALID")
		}
		expected := make(map[string]string, 3)
		for _, key := range []string{"projectId", "leaseId", "projectPath"} {
			value, ok := contextValue[key].(string)
			if !ok || value == "" || !utf8.ValidString(value) {
				return fail("PORTABLE_CONTEXT_INVALID")
			}
			expected[key] = value
		}
		attempt, err := provider.BeginArchiveExport(ctx, caller, expected, grantID)
		if err != nil {
			return fail("PORTABLE_CONTEXT_INVALID")
		}
		if attempt == nil {
			return fail("PORTABLE_EXPORT_AUTHORITY_REQUIRED")
		}
		if attempt.Writer == nil || attempt.Commit == nil || attempt.Abort == nil {
			if attempt.Abort != nil {
				_ = attempt.Abort()
			}
			return fail("PORTABLE_EXPORT_AUTHORITY_REQUIRED")
		}
		committed := false
		defer func() {
			if !committed {
				_ = attempt.Abort()
			}
		}()
		receipt, err := archiveformat.ExportPrivate(ctx, attempt.Writer, attempt.Snapshot, limits)
		closeErr := attempt.Writer.Close()
		if err != nil {
			if archiveformat.IsExportAuthorityFailure(err) {
				return fail(archiveformat.Code(err))
			}
			return fail("PORTABLE_ARCHIVE_INVALID")
		}
		if closeErr != nil {
			return fail("PORTABLE_ARCHIVE_INVALID")
		}
		url, name, err := attempt.Commit(receipt)
		if err != nil || !archiveDownloadURL.MatchString(url) || name == "" || !utf8.ValidString(name) || strings.ContainsAny(name, "/\\\x00\r\n") {
			return fail("PORTABLE_ARCHIVE_PUBLISH_UNSUPPORTED")
		}
		committed = true
		if err := s.Publish(caller, "browser:download-ready", []any{map[string]any{"url": url, "name": name}}); err != nil {
			return nil, security.NewHTTPError(http.StatusConflict, "DOWNLOAD_CLIENT_NOT_OWNED")
		}
		return map[string]any{"success": true, "receipt": receipt}, nil
	})
}
