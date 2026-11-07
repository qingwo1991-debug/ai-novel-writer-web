//go:build linux

package main

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/promptskills"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/skillegress"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// These tests pin the serving half of skills:inspect-github and
// skills:install-github: real HTTP through the frozen cookie session, with the
// network stubbed at the narrow client's own socket seam
// (skillegress.NewWithTransport) - never a real fetch to GitHub. The only
// round trips here are the loopback operation envelope the renderer speaks, and
// the stub transport that stands in for the two GitHub origins.
//
// What the tests prove between them:
//
//	valid           a self-contained package inspects compatible, installs, and
//	                is listed through a separate service over the same root
//	traversal/hook  the same inspect core the local install applies refuses the
//	                package, install publishes nothing, the root is unchanged
//	foreign         a non-GitHub URL is refused by ParseGitHubURL before any
//	                socket: the stub records zero dials
//	redirect        a 3xx is refused without following to a second host, and the
//	                second host is never dialed
//	oversize        a body over the bound is refused as too large
//	seam            the dispatcher, authority and binder declarations still
//	                resolve in this file's source

const (
	// productionSkillGitHubInstallSource is the one argument both names take.
	productionSkillGitHubInstallSource = "https://github.com/acme/scene-craft"
	// productionSkillGitHubInstallForeignSource is a host ParseGitHubURL refuses
	// before any dial (internal/promptskills/inspection.go:111).
	productionSkillGitHubInstallForeignSource = "https://evil.example/acme/scene-craft"
	// productionSkillGitHubInstallSkillName is the frontmatter name every
	// package below declares, so the name gate and the inspect gate are not
	// confused.
	productionSkillGitHubInstallSkillName = "scene-craft"
	// productionSkillGitHubInstallValidBody is a self-contained package.
	productionSkillGitHubInstallValidBody = "---\nname: scene-craft\ndescription: 逐章复核草稿\n---\n\n# 场景打磨\n\n按章节顺序读一遍，指出前后矛盾，再改一处。\n"
	// productionSkillGitHubInstallTraversalBody references a file above the skill.
	productionSkillGitHubInstallTraversalBody = "---\nname: scene-craft\ndescription: 逐章复核草稿\n---\n\n先读 [模板](../outside/template.md) 再改一处。\n"
	// productionSkillGitHubInstallHookBody ships a hook path in its prose.
	productionSkillGitHubInstallHookBody = "---\nname: scene-craft\ndescription: 逐章复核草稿\n---\n\n安装前执行 hooks/pre-commit.sh。\n"
)

// productionSkillGitHubInstallStub is the narrow client's socket seam: it stands
// in for api.github.com and raw.githubusercontent.com and records every dial.
type productionSkillGitHubInstallStub struct {
	mu    sync.Mutex
	dials []string
	repo  func() *http.Response
	raw   func() *http.Response
}

func (s *productionSkillGitHubInstallStub) RoundTrip(req *http.Request) (*http.Response, error) {
	s.mu.Lock()
	s.dials = append(s.dials, req.URL.Scheme+"://"+req.URL.Host+req.URL.Path)
	s.mu.Unlock()
	var handler func() *http.Response
	switch req.URL.Host {
	case "api.github.com":
		handler = s.repo
	case "raw.githubusercontent.com":
		handler = s.raw
	}
	if handler == nil {
		return productionSkillGitHubInstallResponse(http.StatusInternalServerError, ""), nil
	}
	return handler(), nil
}

func (s *productionSkillGitHubInstallStub) dialsSnapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.dials))
	copy(out, s.dials)
	return out
}

func productionSkillGitHubInstallResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func productionSkillGitHubInstallRedirectResponse(target string) *http.Response {
	header := make(http.Header)
	header.Set("Location", target)
	return &http.Response{
		StatusCode: http.StatusFound,
		Status:     http.StatusText(http.StatusFound),
		Header:     header,
		Body:       io.NopCloser(strings.NewReader("")),
	}
}

func productionSkillGitHubInstallValidStub(rawBody string) *productionSkillGitHubInstallStub {
	return &productionSkillGitHubInstallStub{
		repo: func() *http.Response {
			return productionSkillGitHubInstallResponse(http.StatusOK, `{"default_branch":"main"}`)
		},
		raw: func() *http.Response {
			return productionSkillGitHubInstallResponse(http.StatusOK, rawBody)
		},
	}
}

func productionSkillGitHubInstallRedirectStub(target string) *productionSkillGitHubInstallStub {
	return &productionSkillGitHubInstallStub{
		repo: func() *http.Response {
			return productionSkillGitHubInstallRedirectResponse(target)
		},
	}
}

// productionSkillGitHubInstallServer opens the whole serving lane over a real
// committed generation, with the narrow client's socket stubbed. It returns the
// bound operation server, the stub for dial inspection, and the committed
// generation root.
func productionSkillGitHubInstallServer(t *testing.T, stub *productionSkillGitHubInstallStub) (*transport.Server, *productionSkillGitHubInstallStub, string) {
	t.Helper()
	root, _ := productionSingletonDataRoot(t)
	layout, err := productionLayoutFor(root)
	if err != nil {
		t.Fatal(err)
	}
	startup, cancel := context.WithTimeout(context.Background(), productionStartupTimeout)
	defer cancel()
	open, err := openProduction(startup, layout, productionConfig{
		Schema: productionSingletonContract(t), MaxDatabaseBytes: productionMaxDatabaseBytes,
	})
	if err != nil {
		t.Fatal(err)
	}
	client, err := skillegress.NewWithTransport(stub)
	if err != nil {
		_ = open.Close()
		t.Fatal(err)
	}
	server := transport.New(migrationRegistry(t), security.NewOrigins("http://"+productionDurableAddress),
		"go-production-skill-github-install")
	lane := newProductionSkillGitHubInstallWithClient(open, client)
	if err := bindProductionSkillGitHubInstall(server, lane); err != nil {
		_ = open.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		lane.Close()
		server.Close()
		if closeErr := open.Close(); closeErr != nil {
			t.Errorf("close the production lane: %v", closeErr)
		}
	})
	return server, stub, productionWriteGeneration(root)
}

// productionSkillGitHubInstallRefused decodes one refusal envelope and returns
// its error code.
func productionSkillGitHubInstallRefused(t *testing.T, author *productionDurableClient, operation, source string) string {
	t.Helper()
	var refused struct {
		Success bool   `json:"success"`
		Error   string `json:"error"`
	}
	author.decode(t, operation, []any{source}, &refused)
	if refused.Success {
		t.Fatalf("%s answered success for %s", operation, source)
	}
	if refused.Error == "" {
		t.Fatalf("%s returned an empty refusal", operation)
	}
	return refused.Error
}

// TestProductionSkillGitHubInstallServesValidPackage proves the full journey:
// inspect returns a compatible inspection, install publishes the re-verified
// bytes, and a separate service over the same root lists the installed skill.
func TestProductionSkillGitHubInstallServesValidPackage(t *testing.T) {
	stub := productionSkillGitHubInstallValidStub(productionSkillGitHubInstallValidBody)
	server, _, dataRoot := productionSkillGitHubInstallServer(t, stub)
	author := newProductionDurableClient(t, server, productionDurableTab)

	var inspected struct {
		Success    bool `json:"success"`
		Inspection struct {
			Compatible bool `json:"compatible"`
			Metadata   struct {
				Name string `json:"name"`
			} `json:"metadata"`
		} `json:"inspection"`
	}
	author.decode(t, productionSkillGitHubInspectName, []any{productionSkillGitHubInstallSource}, &inspected)
	if !inspected.Success || !inspected.Inspection.Compatible ||
		inspected.Inspection.Metadata.Name != productionSkillGitHubInstallSkillName {
		t.Fatalf("a self-contained package was not inspected: success=%v compatible=%v name=%s",
			inspected.Success, inspected.Inspection.Compatible, inspected.Inspection.Metadata.Name)
	}

	var installed struct {
		Success bool `json:"success"`
		Skill   struct {
			Name       string `json:"name"`
			Source     string `json:"source"`
			Compatible bool   `json:"compatible"`
		} `json:"skill"`
	}
	author.decode(t, productionSkillGitHubInstallName, []any{productionSkillGitHubInstallSource}, &installed)
	if !installed.Success || installed.Skill.Name != productionSkillGitHubInstallSkillName ||
		installed.Skill.Source != "user" || !installed.Skill.Compatible {
		t.Fatalf("the install receipt is wrong: success=%v name=%s source=%s compatible=%v",
			installed.Success, installed.Skill.Name, installed.Skill.Source, installed.Skill.Compatible)
	}

	fresh, err := promptskills.New(dataRoot)
	if err != nil {
		t.Fatalf("open a fresh skill service: %v", err)
	}
	defer func() {
		if closeErr := fresh.Close(); closeErr != nil {
			t.Errorf("close the fresh skill service: %v", closeErr)
		}
	}()
	list, err := fresh.ListUser()
	if err != nil {
		t.Fatalf("list installed skills: %v", err)
	}
	if len(list) != 1 || list[0].Name != productionSkillGitHubInstallSkillName ||
		list[0].Content != productionSkillGitHubInstallValidBody {
		t.Fatalf("the published skill is not the package that was installed: %d entries", len(list))
	}
}

// TestProductionSkillGitHubInstallRefusesIncompatiblePackage proves a traversal
// and a hook package inspect as incompatible and install nothing, leaving the
// skill root byte-identical.
func TestProductionSkillGitHubInstallRefusesIncompatiblePackage(t *testing.T) {
	for _, fixture := range []struct {
		label string
		body  string
	}{
		{"traversal", productionSkillGitHubInstallTraversalBody},
		{"hook", productionSkillGitHubInstallHookBody},
	} {
		t.Run(fixture.label, func(t *testing.T) {
			stub := productionSkillGitHubInstallValidStub(fixture.body)
			server, _, dataRoot := productionSkillGitHubInstallServer(t, stub)
			author := newProductionDurableClient(t, server, productionDurableTab)

			before := productionFSSnapshot(t, dataRoot)
			var inspected struct {
				Success    bool `json:"success"`
				Inspection struct {
					Compatible bool `json:"compatible"`
				} `json:"inspection"`
			}
			author.decode(t, productionSkillGitHubInspectName, []any{productionSkillGitHubInstallSource}, &inspected)
			if !inspected.Success || inspected.Inspection.Compatible {
				t.Fatalf("the %s package was not judged incompatible: success=%v compatible=%v",
					fixture.label, inspected.Success, inspected.Inspection.Compatible)
			}
			productionSkillGitHubInstallRefused(t, author, productionSkillGitHubInstallName, productionSkillGitHubInstallSource)
			if after := productionFSSnapshot(t, dataRoot); !reflect.DeepEqual(before, after) {
				t.Fatalf("the %s refusal changed the skill root", fixture.label)
			}
		})
	}
}

// TestProductionSkillGitHubInstallRefusesForeignURLBeforeDial proves a
// non-GitHub URL is refused by ParseGitHubURL before any socket: the stub
// records zero dials.
func TestProductionSkillGitHubInstallRefusesForeignURLBeforeDial(t *testing.T) {
	stub := &productionSkillGitHubInstallStub{}
	server, _, dataRoot := productionSkillGitHubInstallServer(t, stub)
	author := newProductionDurableClient(t, server, productionDurableTab)

	before := productionFSSnapshot(t, dataRoot)
	// ParseGitHubURL refuses the host before the narrow client is asked to dial,
	// so the exact code is the generic refusal; the property that matters is the
	// zero-dial assertion below.
	productionSkillGitHubInstallRefused(t, author, productionSkillGitHubInspectName, productionSkillGitHubInstallForeignSource)
	if dials := stub.dialsSnapshot(); len(dials) != 0 {
		t.Fatalf("a foreign source dialed the network: %v", dials)
	}
	if after := productionFSSnapshot(t, dataRoot); !reflect.DeepEqual(before, after) {
		t.Fatal("the foreign refusal changed the skill root")
	}
}

// TestProductionSkillGitHubInstallRefusesRedirect proves a 3xx is refused before
// the Location is read and the second host is never dialed.
func TestProductionSkillGitHubInstallRefusesRedirect(t *testing.T) {
	stub := productionSkillGitHubInstallRedirectStub("https://evil.example/elsewhere/SKILL.md")
	server, _, dataRoot := productionSkillGitHubInstallServer(t, stub)
	author := newProductionDurableClient(t, server, productionDurableTab)

	before := productionFSSnapshot(t, dataRoot)
	code := productionSkillGitHubInstallRefused(t, author, productionSkillGitHubInspectName, productionSkillGitHubInstallSource)
	if code != "SKILLS_GITHUB_REDIRECT_FORBIDDEN" {
		t.Fatalf("a redirect refused with <%s>; want the redirect refusal", code)
	}
	dials := stub.dialsSnapshot()
	if len(dials) != 1 || !strings.HasPrefix(dials[0], "https://api.github.com/") {
		t.Fatalf("a redirect was followed or mis-routed: %v", dials)
	}
	for _, dial := range dials {
		if strings.Contains(dial, "evil.example") {
			t.Fatalf("the redirect target was dialed: %v", dials)
		}
	}
	if after := productionFSSnapshot(t, dataRoot); !reflect.DeepEqual(before, after) {
		t.Fatal("the redirect refusal changed the skill root")
	}
}

// TestProductionSkillGitHubInstallRefusesOversize proves a body over the bound
// is refused as too large and the root stays byte-identical.
func TestProductionSkillGitHubInstallRefusesOversize(t *testing.T) {
	oversize := "---\nname: scene-craft\n---\n\n" + strings.Repeat("a", 64*1024)
	stub := productionSkillGitHubInstallValidStub(oversize)
	server, _, dataRoot := productionSkillGitHubInstallServer(t, stub)
	author := newProductionDurableClient(t, server, productionDurableTab)

	before := productionFSSnapshot(t, dataRoot)
	code := productionSkillGitHubInstallRefused(t, author, productionSkillGitHubInspectName, productionSkillGitHubInstallSource)
	if code != "SKILLS_GITHUB_TOO_LARGE" {
		t.Fatalf("an oversize body refused with <%s>; want the size refusal", code)
	}
	if after := productionFSSnapshot(t, dataRoot); !reflect.DeepEqual(before, after) {
		t.Fatal("the oversize refusal changed the skill root")
	}
}

// TestProductionSkillGitHubInstallSeamResolves re-reads this file's source and
// checks that the dispatcher, authority and binder declarations still resolve.
// CI formats the source before running tests, so the check compares without
// white space via productionSeamContains.
func TestProductionSkillGitHubInstallSeamResolves(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("..", "..", "cmd", "novel-server", "production_skill_github_install_linux.go"))
	if err != nil {
		t.Fatalf("read the seam source: %v", err)
	}
	for _, symbol := range []string{
		"skillegress.Client",
		"productionSkillGitHubAuthority",
		"newProductionSkillGitHubInstall",
		"bindProductionSkillGitHubInstall",
		"AcquireCurrent",
		"SetRemoteAuthority",
	} {
		if !productionSeamContains(string(source), symbol) {
			t.Fatalf("the seam source no longer declares <%s>", symbol)
		}
	}
}
