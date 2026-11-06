//go:build linux

package main

import (
	"context"
	"errors"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

const (
	// productionRecentRemoveName is the single name this file serves. It matches
	// the inventory key the binder resolves (internal/transport/projects.go:306).
	productionRecentRemoveName = "project:recent-remove"

	// productionRecentTargetUnknown is the symbolic refusal of a target that is
	// not an admitted shelf entry: an unknown id, another library's project, an
	// absolute path outside, a traversal spelling or a symlink. It names the
	// condition instead of the value, so no caller-named path is ever echoed and
	// no unknown id can be answered as a success.
	productionRecentTargetUnknown = "PROJECT_RECENT_TARGET_UNKNOWN"
)

// This file is the production lane's project:recent-remove. It publishes one
// name and nothing else, and it is the write half of the shelf
// production_portability_linux.go already serves through
// transport.BindEditorNavigation (production_project_portability_linux.go:
// 273-288, :313-321).
//
// # The reference it ports
//
// The desktop main process registered the name with one argument and no frozen
// context - dropping a shelf entry is navigation metadata, not an author-database
// write:
//
//	ipcMain.handle('project:recent-remove', async (_event, projectPath: string) => {
//	  try { removeRecentProject(projectPath); ...; return { success: true } }
//	  catch (error) { return { success: false, error: String(error) } }
//	})
//
// (electron/controllers/project-controller.ts:896-904). removeRecentProject
// itself only rewrites the navigation document (:343-347), so the reference can
// never delete a project, and its success is `{success: true}` with a refusal of
// `{success: false, error}` at HTTP 200 (:900-902). The name is routed as global
// in web/operations.json, which is why no lease is on the wire.
//
// The family's own binder is used verbatim: transport.BindProjectRecentRemove
// (internal/transport/projects.go:306-324) takes exactly one argument, refuses
// any other arity as INVALID_ARGUMENTS (:311-313), and answers a provider
// failure as `{success:false, error:"Error: PROJECT_RECENT_REMOVE_FAILED"}`
// (:319-321) instead of `String(error)`, because a catalog error can carry a
// host path. Nothing about the wire shape is reimplemented here.
//
// # What "the shelf" is on this lane, and what is NOT written
//
// project:recent-list is served by productionOpen.RecentProjects
// (production_open_linux.go:344-370): it projects the admitted inventory in
// `productionOpen.projects`, in the library's own newest-first order. That
// inventory - not projectcatalog.Store - is the structure the served list reads,
// so it is the one a removal has to write.
//
// The catalog is deliberately left alone, for three reasons that are all in the
// shipped code:
//
//  1. It is the OPEN authority, not the shelf: a live reader re-resolves its key
//     on every refresh (internal/authorruntime/read_linux.go:225-247), so
//     catalog.Remove for a document the author has open would turn a shelf
//     removal into a session revocation - the opposite of the reference, which
//     does not close the project the author is editing (:896-904).
//  2. The served shelf does not read it, so a catalog write would not remove
//     anything a browser can see: it would be the fabricated `{success:true}`
//     that production_config_write_linux.go:99-113 already refuses to answer.
//  3. Its navigation document is rebuilt from the admitted library on every
//     start (production_durable_linux.go:29-36, :112-129), so the record would
//     come straight back anyway.
//
// # The write, and why it cannot delete a project
//
// productionRecentRemoveShelve drops ONE entry whose path is exactly a string
// this lane admitted at startup (or in project:create's shelve,
// production_project_create_linux.go:752-770), under the same lane mutex that
// serves the list, and does nothing else: no filesystem call, no catalog call, no
// handle. A target that is not an admitted shelf entry - an unknown id, another
// library's project, an absolute path outside, a traversal spelling, a trailing
// slash, a symlink that resolves to an admitted project - is refused BY NAME with
// PROJECT_RECENT_TARGET_UNKNOWN, never answered as a success. Because the write
// is a string match against a trusted inventory and not a path resolution, no
// caller-named path is ever opened, followed or deleted.
//
// The scope of the removal is stated rather than hidden: it withdraws the entry
// from this process's admitted inventory. The project directory, its manifest and
// its database are untouched, it stays openable through the catalog key this lane
// issued, and a restart re-derives the shelf from the library and lists it again.
// The chooser at /api/library projects the same inventory
// (production_open_linux.go:319-342), so a removed entry leaves that list too
// for the lifetime of the process.
type productionRecentRemove struct {
	open *productionOpen
}

// newProductionRecentRemove returns the shelf-removal adapter for the lane built
// by productionHandlerWithContract. It resolves nothing and opens nothing here.
func newProductionRecentRemove(open *productionOpen) *productionRecentRemove {
	return &productionRecentRemove{open: open}
}

// Bind installs project:recent-remove through the family binder. A nil adapter is
// refused instead of being published as a handler that can only fail, and the
// binder's own server.Bind refuses an uninventoried or already bound name, so a
// double bind is a startup error rather than a silently different shelf.
func (r *productionRecentRemove) Bind(server *transport.Server) error {
	if r == nil || r.open == nil || server == nil {
		return errors.New("PRODUCTION_RECENT_REMOVE_BINDER_INVALID")
	}
	return server.BindProjectRecentRemove(r)
}

// RemoveRecentNavigation is the provider method the family binder drives. The
// target is the path project:recent-list handed the browser, never a new host
// path, and the only authority the request carries is the caller identity the
// transport itself admitted: the cookie session plus the browser tab, the same
// pair productionOwner validates for every leased name
// (production_durable_linux.go:157-164).
//
// A refusal is a named symbolic error, never a fabricated success. The binder
// turns it into the family's own failure envelope so the browser learns a stable
// code and no catalog or path text is reflected back.
func (r *productionRecentRemove) RemoveRecentNavigation(ctx context.Context, caller transport.Caller, target string) (any, error) {
	if r == nil || r.open == nil || ctx == nil {
		return nil, errors.New(productionFSClosedCode)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := productionOwner(caller); err != nil {
		return nil, errors.New("PROJECT_SESSION_REQUIRED")
	}
	return productionRecentRemoveShelve(r.open, target)
}

// productionRecentRemoveShelve drops one navigation entry from the shelf the
// served list projects. It is a string match against the admitted inventory and
// one marker under the lane mutex, so it holds no library, no project, no
// database and no catalog, and a lane whose project list is empty still answers
// exactly as it should: an unknown target, by name.
//
// It deliberately does NOT bound the reference's reach: the reference rewrites a
// navigation document, while this lane derives the shelf from what it admitted.
// Dropping the entry from that inventory would be a much larger act than the
// author asked for - the book would vanish from the library listing and from
// project:create's own proofs while still sitting on disk - so the removal is
// recorded next to the shelf and consulted only by project:recent-list.
// The consequence, stated because it is a real divergence: the shelf is derived,
// so a restart re-derives it and the removed entry comes back.
//
// The lane's own closed state is refused first, so a request that arrives after
// Close cannot mutate a list nothing serves any more.
func productionRecentRemoveShelve(open *productionOpen, target string) (any, error) {
	if open == nil {
		return nil, errors.New(productionFSClosedCode)
	}
	open.mu.Lock()
	defer open.mu.Unlock()
	if open.closed {
		return nil, errors.New(productionFSClosedCode)
	}
	if target == "" {
		return nil, errors.New(productionRecentTargetUnknown)
	}
	// Exact membership only. There is no filepath.Clean, no lexical containment
	// and no realpath here on purpose: the accepted value is the byte-for-byte
	// path this lane admitted, so a traversal spelling, a trailing slash, an
	// absolute path outside the library or a symlink that resolves to an admitted
	// project is a different string and therefore an unknown target.
	admitted := false
	for _, item := range open.projects {
		if item.listed.Path == target {
			admitted = true
			break
		}
	}
	if !admitted {
		return nil, errors.New(productionRecentTargetUnknown)
	}
	// The entry is dropped from the SHELF, not from the lane. Removing it from
	// open.projects would withdraw the project from admission: the library listing
	// would stop showing a book that is still on disk, and project:create's own
	// "the project I am leaving" proof would refuse a path the author is working in.
	// A shelf is navigation, so it keeps its own memory and nothing else changes.
	if open.recentRemoved == nil {
		open.recentRemoved = make(map[string]bool, len(open.projects))
	}
	open.recentRemoved[target] = true
	return map[string]any{"success": true}, nil
}
