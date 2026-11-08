//go:build linux

package main

import (
	"net/http"
	"net/url"
	"sort"
	"strings"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// productionServedOperations is the number of operations this lane actually
// installs a handler for: of the inventory the browser is allowed to ask for,
// how many the production lane serves rather than refuses at
// GO_OPERATION_NOT_MIGRATED. It is the one authoritative figure behind the
// report, because the number is asserted below against the handlers the
// finished lane really bound instead of being recounted by hand. A batch that
// adds an operation has to move this number in the same change. -1 means "not
// measured yet", which fails on purpose and prints both the real count and the
// served names.
const productionServedOperations = 273

// TestProductionLaneServedOperationCount measures that number. The fixture is
// the editor-read root: it skips without GO_SQLITE_CONTRACT exactly like its
// siblings, and it opens the whole lane, because which operations are bound is
// decided by productionHandlerWithContract and not by the rows the fixture
// seeds.
func TestProductionLaneServedOperationCount(t *testing.T) {
	root, _, contract, _, _ := productionEditorReadsRoot(t)
	registry := migrationRegistry(t)
	handler, cleanup, err := productionHandlerWithContract(registry, productionDurableAddress, "", root,
		productionConfig{Schema: contract, MaxDatabaseBytes: productionMaxDatabaseBytes})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	served := productionLaneBoundOperations(t, handler)
	// Count first so a truncated annotation still carries the number, names last
	// and comma separated so one CI run reports the whole truth without a second
	// compile and without any per-family list pinned in this file.
	// The unserved list is logged on every run, not only on a mismatch: the next
	// pass has to know which names are left without recounting by hand, and a
	// hand-kept list is the thing this test exists to replace.
	bound := make(map[string]bool, len(served))
	for _, name := range served {
		bound[name] = true
	}
	var unserved []string
	for name := range registry.Operations {
		if !bound[name] {
			unserved = append(unserved, name)
		}
	}
	sort.Strings(unserved)
	// Logged in chunks because the workflow truncates a long log line, and a
	// truncated list is the same problem as no list.
	t.Logf("%d served, %d unserved", len(served), len(unserved))
	for start := 0; start < len(unserved); start += 6 {
		end := start + 6
		if end > len(unserved) {
			end = len(unserved)
		}
		t.Logf("unserved %v-%v: %s", start+1, end, strings.Join(unserved[start:end], ","))
	}
	if len(served) != productionServedOperations {
		t.Fatalf("%d of %d inventoried operations are served (productionServedOperations=%d); served=%s",
			len(served), len(registry.Operations), productionServedOperations, strings.Join(served, ","))
	}
}

// productionLaneBoundOperations returns the operation names the built lane
// installed. The route table productionHandlerWithContract returns mounts the
// operation server itself at /api/ (production_open_linux.go:631), so asking the
// route table for that path hands back the transport.Server, and BoundOperations
// (internal/transport/server.go:41) is the accessor the package already exposes
// for the handlers it bound. Nothing here reaches the unexported map or
// reimplements the wiring.
func productionLaneBoundOperations(t *testing.T, handler http.Handler) []string {
	t.Helper()
	mux, ok := handler.(*http.ServeMux)
	if !ok {
		t.Fatalf("the production lane returned %T, not the route table it builds", handler)
	}
	mounted, pattern := mux.Handler(&http.Request{Method: http.MethodPost, URL: &url.URL{Path: "/api/"}})
	server, ok := mounted.(*transport.Server)
	if !ok {
		t.Fatalf("the production route table mounts %T at %q, not the operation server", mounted, pattern)
	}
	// BoundOperations returns the names sorted, so neither the count nor the
	// reported list depends on the order the binders were installed in.
	return server.BoundOperations()
}

