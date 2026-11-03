package transport

import (
	"context"
	"errors"
	"net/http"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
)

// LibraryInventory is supplied from a trusted, pinned server library. It
// enumerates canonical direct children, including projects hidden from the
// recent-project shelf. Enumeration never grants a project-open capability.
type LibraryInventory interface {
	ListCanonicalProjects(context.Context, int) ([]projectfs.ListedProject, error)
}

// LibraryRoute serves the existing browser chooser shape at GET /api/library.
// It shares Server's proxy authentication, cookie sessions, and tab limit. A
// future gateway must mount this exact route before Server's /api/ fallback.
// The optional previewCapabilityId is absent until a real peek service can
// issue a session-bound capability for each listed project.
type LibraryRoute struct {
	server     *Server
	inventory  LibraryInventory
	maxEntries int
}

func NewLibraryRoute(server *Server, inventory LibraryInventory, maxEntries int) (*LibraryRoute, error) {
	if server == nil || inventory == nil || maxEntries < 1 || maxEntries > 4096 {
		return nil, errors.New("LIBRARY_ROUTE_INVALID")
	}
	return &LibraryRoute{server: server, inventory: inventory, maxEntries: maxEntries}, nil
}

func (route *LibraryRoute) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := route.serve(w, r); err != nil {
		_ = route.server.writePublicError(w, err)
	}
}

func (route *LibraryRoute) serve(w http.ResponseWriter, r *http.Request) error {
	if r.URL.Path != "/api/library" {
		return security.NewHTTPError(http.StatusNotFound, "API_ROUTE_NOT_FOUND")
	}
	principal, err := security.Authenticate(r)
	if err != nil {
		return err
	}
	session, err := route.server.sessions.Get(r, w, principal)
	if err != nil {
		return err
	}
	// The original gateway obtains the browser client before dispatching any
	// /api/library method. A missing or invalid tab must not enumerate paths.
	client, err := route.server.getClient(session.ID(), r.Header.Get("X-Client-Id"))
	if err != nil {
		return err
	}
	defer route.server.unpin(client)
	if r.Method != http.MethodGet {
		return security.NewHTTPError(http.StatusNotFound, "API_ROUTE_NOT_FOUND")
	}
	projects, err := route.inventory.ListCanonicalProjects(r.Context(), route.maxEntries)
	if err != nil {
		return err
	}
	if projects == nil {
		projects = []projectfs.ListedProject{}
	}
	return security.WriteJSON(w, http.StatusOK, route.server.public.Sanitize(map[string]any{"projects": projects}))
}
