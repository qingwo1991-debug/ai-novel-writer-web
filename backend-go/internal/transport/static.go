package transport

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
)

// StaticAssets serves the retained React build. The caller supplies a trusted
// build directory and the same session store used by the API. Keeping the root
// descriptor open pins the admitted build even if its pathname is replaced.
// It is an isolated handler until the Go binary binds a complete web gateway.
type StaticAssets struct {
	root     *os.Root
	sessions *security.Sessions
	csp      string
}

var inlineScript = regexp.MustCompile(`(?is)<script(?:\s[^>]*)?>(.*?)</script>`)

// staticMIME covers every extension the Vite build actually emits, including
// the .ttf reading faces in public/fonts and the licence .txt files beside them.
var staticMIME = map[string]string{
	".html":        "text/html; charset=utf-8",
	".js":          "text/javascript; charset=utf-8",
	".css":         "text/css; charset=utf-8",
	".json":        "application/json",
	".map":         "application/json",
	".svg":         "image/svg+xml",
	".png":         "image/png",
	".jpg":         "image/jpeg",
	".jpeg":        "image/jpeg",
	".gif":         "image/gif",
	".avif":        "image/avif",
	".webp":        "image/webp",
	".ico":         "image/x-icon",
	".ttf":         "font/ttf",
	".otf":         "font/otf",
	".woff":        "font/woff",
	".woff2":       "font/woff2",
	".txt":         "text/plain; charset=utf-8",
	".webmanifest": "application/manifest+json",
}

// hashedAsset matches Vite's content-addressed output (assets/<name>-<hash>.ext).
var hashedAsset = regexp.MustCompile(`^assets/[^/]+-[A-Za-z0-9_-]{8}\.[a-z0-9]+$`)

// staticCacheControl keeps the shell revalidatable, lets the content-addressed
// build output live for a year, and keeps other public files short-lived.
func staticCacheControl(name string) string {
	switch {
	case name == "index.html":
		return "no-store"
	case hashedAsset.MatchString(name):
		return "public, max-age=31536000, immutable"
	default:
		return "public, max-age=3600"
	}
}

// spaRoute reports whether an unresolved path may be a client-side route. Only
// extensionless paths qualify, so a missing asset keeps its 404 instead of
// being answered with HTML.
func spaRoute(name string) bool {
	return filepath.Ext(filepath.Base(name)) == ""
}

func NewStaticAssets(directory string, sessions *security.Sessions) (*StaticAssets, error) {
	if sessions == nil || !filepath.IsAbs(directory) || filepath.Clean(directory) != directory {
		return nil, errors.New("STATIC_ROOT_INVALID")
	}
	before, err := os.Lstat(directory)
	if err != nil || !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("STATIC_ROOT_INVALID")
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, errors.New("STATIC_ROOT_INVALID")
	}
	actual, err := root.Stat(".")
	if err != nil || !os.SameFile(before, actual) {
		_ = root.Close()
		return nil, errors.New("STATIC_ROOT_CHANGED")
	}
	index, err := staticFile(root, "index.html")
	if err != nil {
		_ = root.Close()
		return nil, errors.New("STATIC_INDEX_INVALID")
	}
	defer index.Close()
	indexInfo, err := index.Stat()
	if err != nil || indexInfo.Size() > 1<<20 {
		_ = root.Close()
		return nil, errors.New("STATIC_INDEX_INVALID")
	}
	data, err := io.ReadAll(io.LimitReader(index, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		_ = root.Close()
		return nil, errors.New("STATIC_INDEX_INVALID")
	}
	var hashes []string
	for _, match := range inlineScript.FindAllSubmatch(data, -1) {
		if len(strings.TrimSpace(string(match[1]))) == 0 {
			continue
		}
		digest := sha256.Sum256(match[1])
		hashes = append(hashes, "'sha256-"+base64.StdEncoding.EncodeToString(digest[:])+"'")
	}
	policy := "default-src 'self'; script-src 'self' " + strings.Join(hashes, " ") + "; style-src 'self' 'unsafe-inline'; img-src 'self' data: blob:; font-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'self'; form-action 'self'"
	return &StaticAssets{root: root, sessions: sessions, csp: policy}, nil
}

func (s *StaticAssets) Close() error {
	if s == nil || s.root == nil {
		return nil
	}
	return s.root.Close()
}

func staticFile(root *os.Root, name string) (*os.File, error) {
	if name == "" || strings.ContainsAny(name, "\\\x00%") || strings.HasPrefix(name, "/") {
		return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_PATH")
	}
	parts := strings.Split(name, "/")
	prefix := ""
	var expected os.FileInfo
	for i, part := range parts {
		if part == "" || part == "." || part == ".." {
			return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_PATH")
		}
		if prefix == "" {
			prefix = part
		} else {
			prefix += "/" + part
		}
		info, err := root.Lstat(prefix)
		if err != nil {
			return nil, security.NewHTTPError(http.StatusNotFound, "NOT_FOUND")
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, security.NewHTTPError(http.StatusForbidden, "SYMLINK_DENIED")
		}
		if i < len(parts)-1 && !info.IsDir() || i == len(parts)-1 && !info.Mode().IsRegular() {
			return nil, security.NewHTTPError(http.StatusNotFound, "NOT_FOUND")
		}
		if i == len(parts)-1 {
			expected = info
		}
	}
	file, err := root.Open(name)
	if err != nil {
		return nil, security.NewHTTPError(http.StatusNotFound, "NOT_FOUND")
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || !os.SameFile(expected, info) {
		_ = file.Close()
		return nil, security.NewHTTPError(http.StatusNotFound, "NOT_FOUND")
	}
	return file, nil
}

func (s *StaticAssets) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := s.serve(w, r); err != nil {
		var public *security.HTTPError
		if !errors.As(err, &public) {
			public = security.NewHTTPError(http.StatusInternalServerError, "REQUEST_FAILED")
		}
		_ = security.WriteJSON(w, public.Status, map[string]any{"error": map[string]any{"code": public.Code, "message": public.Message}})
	}
}

func (s *StaticAssets) serve(w http.ResponseWriter, r *http.Request) error {
	principal, err := security.Authenticate(r)
	if err != nil {
		return err
	}
	if _, err := s.sessions.Get(r, w, principal); err != nil {
		return err
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return security.NewHTTPError(http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED")
	}
	if strings.HasPrefix(r.URL.Path, "/api/") {
		return security.NewHTTPError(http.StatusNotFound, "NOT_FOUND")
	}
	name := strings.TrimRight(strings.TrimLeft(r.URL.Path, "/"), "/")
	if name == "" {
		name = "index.html"
	}
	file, err := staticFile(s.root, name)
	if err != nil {
		var public *security.HTTPError
		// A single-page client route is served the app shell. A rejected path,
		// a denied symlink and a missing asset each keep their own status.
		if !errors.As(err, &public) || public.Status != http.StatusNotFound || !spaRoute(name) {
			return err
		}
		name = "index.html"
		if file, err = staticFile(s.root, name); err != nil {
			return err
		}
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	contentType := staticMIME[filepath.Ext(name)]
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	w.Header().Set("Cache-Control", staticCacheControl(name))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "same-origin")
	w.Header().Set("Content-Security-Policy", s.csp)
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return nil
	}
	_, _ = io.Copy(w, file)
	return nil
}
