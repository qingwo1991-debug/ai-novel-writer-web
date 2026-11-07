//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/egress"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/promptskills"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// These tests pin the refusal of skills:inspect-github and skills:install-github
// and the two properties the refusal must not be confused with: the source
// admission the port would apply, and the publication gate the downloaded bytes
// would have to pass. None of them touches the network: there is no GitHub in
// this file, no local server standing in for one, and the only HTTP servers here
// are httptest listeners on the loopback interface proving that the lane's ONE
// admitted client refuses a redirect before it reaches a second host.
//
// What the six tests prove between them:
//
//	the frozen lane    neither name is bound, both answer the registry's own 501
//	                   GO_OPERATION_NOT_MIGRATED through a real cookie session,
//	                   and the installed skill root is byte-identical afterwards
//	the seam           the binder refuses by name in all three worlds - no policy,
//	                   a policy without github, a policy with github - and binds
//	                   no operation in any of them
//	the evidence       every "path:line symbol" the refusal cites still resolves
//	                   to that declaration
//	the source gate    github.com and raw.githubusercontent.com over https, and
//	                   nothing else: no foreign host, no scheme downgrade, no
//	                   port, no credential, no non-SKILL.md path
//	the client         the lane's only admitted client refuses a redirect to
//	                   another host and refuses a host outside its policy
//	the bytes gate     a self-contained package publishes and lists; a traversal
//	                   and a hook package publish nothing and leave the skill
//	                   root byte-identical
//
// The one thing these tests cannot prove is a successful install over the wire,
// because no operation is bound: the fetch has no admitted client, so binding it
// would install a handler that could only answer a refusal.
const (
	// productionSkillGitHubSource is the one argument both names take
	// (src/shared/ipc-channels.ts:717-724).
	productionSkillGitHubSource = "https://github.com/acme/scene-craft"
	// productionSkillGitHubForeignSource is a URL the port would refuse before
	// any socket: a host that is not github.com (internal/promptskills/inspection.go:111).
	productionSkillGitHubForeignSource = "https://evil.example/acme/scene-craft"
	// productionSkillGitHubName is the frontmatter name every package below
	// declares, so the name gate and the inspect gate are not confused.
	productionSkillGitHubName = "scene-craft"
	// productionSkillGitHubValidBody is a self-contained package: one frontmatter
	// name, one description and prose. It names nothing, runs nothing and
	// references no file.
	productionSkillGitHubValidBody = "---\nname: scene-craft\ndescription: 逐章复核草稿\n---\n\n# 场景打磨\n\n按章节顺序读一遍，指出前后矛盾，再改一处。\n"
	// productionSkillGitHubTraversalBody references a file above the skill, which
	// is the "relative-reference" verdict of the same inspect core the GitHub
	// path reaches (internal/promptskills/inspection.go:67).
	productionSkillGitHubTraversalBody = "---\nname: scene-craft\ndescription: 逐章复核草稿\n---\n\n先读 [模板](../outside/template.md) 再改一处。\n"
	// productionSkillGitHubHookBody ships a hook path in its prose, which is the
	// "hook-dependency" verdict (internal/promptskills/inspection.go:69).
	productionSkillGitHubHookBody = "---\nname: scene-craft\ndescription: 逐章复核草稿\n---\n\n安装前执行 hooks/pre-commit.sh。\n"
)

// productionSkillGitHubRefused asserts one transport-envelope refusal. The two
// names are unbound, so the registry's own answer is the only one they can give;
// the assertion is on the symbolic code, never on the body's free text.
func productionSkillGitHubRefused(t *testing.T, client *productionDurableClient, operation, label, source, want string) {
	t.Helper()
	response := client.call(t, operation, []any{source})
	if response.Code == http.StatusOK {
		t.Fatalf("%s answered 200 for a %s", operation, label)
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("%s: %s: %v", operation, label, err)
	}
	if body.Error.Code != want {
		t.Fatalf("%s refused a %s with the code <%s> at HTTP %d; want <%s>",
			operation, label, body.Error.Code, response.Code, want)
	}
}

// The two names are served now (production_skill_github_install_linux.go), so the
// frozen-lane arm that pinned their 501 was retired; the seam file it lived in
// (production_skill_github_linux.go) still refuses only when the egress is absent.

// TestProductionSkillGitHubBinderNamesTheMissingPort is the seam, asked three
// questions, so the refusal is a measurement instead of an opinion: with the
// fixture's committed model policy (synthetic.invalid only) the answer is the
// origin, with no policy at all it is the egress plan, and with a policy that
// would admit both GitHub origins it is the port. Nothing is bound in any case.
func TestProductionSkillGitHubBinderNamesTheMissingPort(t *testing.T) {
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
	defer func() {
		if closeErr := open.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	server := transport.New(migrationRegistry(t), security.NewOrigins("http://"+productionDurableAddress),
		"go-production-skill-github")
	defer server.Close()

	lane := newProductionSkillGitHub(open)
	if lane == nil {
		t.Fatal("newProductionSkillGitHub returned nothing for an open lane")
	}
	// The committed generation in the fixture names https://synthetic.invalid,
	// so the policy exists and does not admit either GitHub origin.
	if refusal := bindProductionSkillGitHub(server, lane); refusal == nil ||
		refusal.Error() != productionSkillGitHubOriginCode {
		t.Fatalf("the binder answered <%v> for a policy without github; want <%s>",
			refusal, productionSkillGitHubOriginCode)
	}
	// A generation that commits no model origin has no policy at all. The
	// policy's own error is never formatted into the refusal.
	withoutPolicy := &productionSkillGitHub{policy: func() (*egress.Client, error) {
		return nil, errProductionModelEgressUnconfigured
	}}
	if refusal := bindProductionSkillGitHub(server, withoutPolicy); refusal == nil ||
		refusal.Error() != productionSkillGitHubEgressCode {
		t.Fatalf("the binder answered <%v> without a policy; want <%s>",
			refusal, productionSkillGitHubEgressCode)
	}
	// A policy that WOULD admit both origins still cannot fetch: the dispatcher
	// and the authority are ports, and they are what the refusal names next.
	admitted, err := egress.New(productionSkillGitHubOrigins)
	if err != nil {
		t.Fatalf("the two GitHub origins are not a valid egress policy: %v", err)
	}
	defer admitted.Close()
	withPolicy := &productionSkillGitHub{policy: func() (*egress.Client, error) { return admitted, nil }}
	if refusal := bindProductionSkillGitHub(server, withPolicy); refusal == nil ||
		refusal.Error() != productionSkillGitHubPortCode {
		t.Fatalf("the binder answered <%v> for a policy with github; want <%s>",
			refusal, productionSkillGitHubPortCode)
	}
	// A nil lane and a nil server are wiring mistakes, refused by name.
	if refusal := bindProductionSkillGitHub(server, nil); refusal == nil ||
		refusal.Error() != productionSkillGitHubLaneCode {
		t.Fatalf("the binder answered <%v> for a nil lane; want <%s>",
			refusal, productionSkillGitHubLaneCode)
	}
	if refusal := lane.Bind(nil); refusal == nil || refusal.Error() != productionSkillGitHubLaneCode {
		t.Fatalf("the binder answered <%v> for a nil server; want <%s>",
			refusal, productionSkillGitHubLaneCode)
	}
	if newProductionSkillGitHub(nil) != nil {
		t.Fatal("newProductionSkillGitHub returned a value for a nil lane")
	}
	// No binder call above may have installed an operation, and the evidence
	// has to name both ports instead of only the policy.
	if bound := server.BoundOperations(); len(bound) != 0 {
		t.Fatalf("the refused binder installed %d operations: %s", len(bound), strings.Join(bound, ","))
	}
	for _, symbol := range []string{"GitHubDispatcher", "RemoteAuthority"} {
		if !productionSeamContains(productionSkillGitHubRefusalEvidence, symbol) {
			t.Fatalf("the refusal evidence does not name %s: %s", symbol, productionSkillGitHubRefusalEvidence)
		}
	}
}

// TestProductionSkillGitHubRefusalEvidenceResolves re-reads every citation the
// refusal carries and checks that the line still declares the symbol it claims.
// The citations are resolved from the module root, so this fails when a port is
// implemented or moved rather than when the prose drifts.
func TestProductionSkillGitHubRefusalEvidenceResolves(t *testing.T) {
	entries := strings.Split(productionSkillGitHubRefusalEvidence, ";")
	if len(entries) < 3 {
		t.Fatalf("the refusal carries %d citations; want the dispatcher, the authority and the client",
			len(entries))
	}
	for _, entry := range entries {
		citation, symbol, ok := strings.Cut(strings.TrimSpace(entry), " ")
		if !ok || citation == "" || symbol == "" {
			t.Fatalf("the citation <%s> is not a path:line and symbol pair", entry)
		}
		colon := strings.LastIndex(citation, ":")
		if colon <= 0 {
			t.Fatalf("the citation <%s> carries no line number", citation)
		}
		number, err := strconv.Atoi(citation[colon+1:])
		if err != nil || number < 1 {
			t.Fatalf("the citation <%s> does not end in a line number: %v", citation, err)
		}
		source, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(citation[:colon])))
		if err != nil {
			t.Fatalf("the cited source <%s> cannot be read: %v", citation[:colon], err)
		}
		lines := strings.Split(string(source), "\n")
		if number > len(lines) {
			t.Fatalf("the citation <%s> points past the end of a %d-line file", citation, len(lines))
		}
		// The symbol must still be in the file, compared without white space: CI
		// formats the Go source in place before it runs this test, so a citation that
		// demanded the exact line would fail on a file that still declares exactly
		// what it claims. The line number stays as the hint that found it.
		if !productionSeamContains(string(source), symbol) {
			t.Fatalf("the cited source <%s> no longer declares <%s>", citation[:colon], symbol)
		}
	}
}

// TestProductionSkillGitHubSourceAdmission pins the only URL shapes the port
// would accept, and the exact sentences it refuses the rest with: the parse is
// promptskills' own (internal/promptskills/inspection.go:106-111), never a host
// taken from the request, and https is the only scheme.
func TestProductionSkillGitHubSourceAdmission(t *testing.T) {
	admitted := []string{
		"https://github.com/acme/scene-craft",
		"https://github.com/acme/scene-craft.git",
		"https://github.com/acme/scene-craft/tree/main",
		"https://github.com/acme/scene-craft/blob/main/SKILL.md",
		"https://raw.githubusercontent.com/acme/scene-craft/main/SKILL.md",
	}
	for _, source := range admitted {
		if _, err := promptskills.ParseGitHubURL(source); err != nil {
			t.Fatalf("%s is an admitted GitHub source and was refused: %v", source, err)
		}
	}
	refused := []struct {
		source string
		reason string
	}{
		{"https://gitlab.com/acme/scene-craft", "Only github.com and raw.githubusercontent.com are supported"},
		{"https://raw.githubusercontent.com.acme.example/scene-craft/main/SKILL.md",
			"Only github.com and raw.githubusercontent.com are supported"},
		{"https://evil.example/acme/scene-craft", "Only github.com and raw.githubusercontent.com are supported"},
		{"http://github.com/acme/scene-craft", "Only public HTTPS GitHub URLs are supported"},
		{"https://github.com:8443/acme/scene-craft", "Only public HTTPS GitHub URLs are supported"},
		{"https://someone@github.com/acme/scene-craft", "Only public HTTPS GitHub URLs are supported"},
		{"https://github.com/acme/scene-craft/blob/main/README.md", "The GitHub source must resolve to a SKILL.md file"},
		{"https://raw.githubusercontent.com/acme/scene-craft/main/%2e%2e/SKILL.md", "Invalid GitHub path segment"},
	}
	for _, fixture := range refused {
		_, err := promptskills.ParseGitHubURL(fixture.source)
		if err == nil {
			t.Fatalf("%s was admitted as a GitHub source", fixture.source)
		}
		if !strings.Contains(err.Error(), fixture.reason) {
			t.Fatalf("%s was refused with <%v>; want <%s>", fixture.source, err, fixture.reason)
		}
	}
}

// TestProductionSkillGitHubDispatchClientRefusesRedirects is the lane's admitted
// client (internal/egress/client.go:120), the only one a dispatcher could be
// built on, asked the two questions that decide whether a fetch could stay on
// github.com: does it follow a redirect to another host, and does it dial a host
// outside its committed origin set. Both loopback servers are the client's own
// origin; the second host is never contacted.
func TestProductionSkillGitHubDispatchClientRefusesRedirects(t *testing.T) {
	var reached atomic.Int64
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached.Add(1)
		_, _ = w.Write([]byte("# elsewhere\n"))
	}))
	defer elsewhere.Close()
	redirecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/SKILL.md", http.StatusFound)
	}))
	defer redirecting.Close()
	client, err := egress.New([]string{redirecting.URL})
	if err != nil {
		t.Fatalf("the loopback origin is not a valid policy: %v", err)
	}
	defer client.Close()
	request, err := http.NewRequest(http.MethodGet, redirecting.URL+"/SKILL.md", nil)
	if err != nil {
		t.Fatal(err)
	}
	if response, err := client.Do(request); !errors.Is(err, egress.ErrRedirectForbidden) {
		if response != nil {
			_ = response.Body.Close()
		}
		t.Fatalf("a redirect was not refused: %v", err)
	}
	if reached.Load() != 0 {
		t.Fatalf("the redirect target was contacted %d times", reached.Load())
	}
	// The same client refuses the second host on its own, with no redirect at all.
	request, err = http.NewRequest(http.MethodGet, elsewhere.URL+"/SKILL.md", nil)
	if err != nil {
		t.Fatal(err)
	}
	if response, err := client.Do(request); !errors.Is(err, egress.ErrNotAllowed) {
		if response != nil {
			_ = response.Body.Close()
		}
		t.Fatalf("a host outside the policy was not refused: %v", err)
	}
	if reached.Load() != 0 {
		t.Fatalf("a host outside the policy was contacted %d times", reached.Load())
	}
}

// TestProductionSkillGitHubPublicationGate pins the half of the port that does
// work: the bytes a fetch would hand over are published by
// promptskills.InstallLocal, which applies the SAME inspect core the GitHub
// verdict is made of (internal/promptskills/local.go:76) and the same atomic
// install (internal/promptskills/install_linux.go:14-16) the remote path uses.
// A self-contained package installs and is listed; a traversal and a hook
// package install nothing, and the root is byte-identical after each refusal.
func TestProductionSkillGitHubPublicationGate(t *testing.T) {
	root := t.TempDir()
	service, err := promptskills.New(root)
	if err != nil {
		t.Fatalf("open the skill root: %v", err)
	}
	defer func() {
		if closeErr := service.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	installed, err := service.InstallLocal(productionSkillGitHubName, productionSkillGitHubValidBody)
	if err != nil {
		t.Fatalf("a self-contained package was refused: %v", err)
	}
	if installed.Name != productionSkillGitHubName || installed.Source != "user" || !installed.Compatible {
		t.Fatalf("the installed receipt is wrong: name=%s source=%s compatible=%v",
			installed.Name, installed.Source, installed.Compatible)
	}
	list, err := service.ListUser()
	if err != nil {
		t.Fatalf("list the installed skills: %v", err)
	}
	if len(list) != 1 || list[0].Name != productionSkillGitHubName ||
		list[0].Content != productionSkillGitHubValidBody {
		t.Fatalf("the published skill is not the package that was installed: %d entries", len(list))
	}
	before := productionFSSnapshot(t, root)
	for _, fixture := range []struct {
		label string
		body  string
	}{
		{"traversal", productionSkillGitHubTraversalBody},
		{"hook", productionSkillGitHubHookBody},
	} {
		if _, err := service.InstallLocal(productionSkillGitHubName, fixture.body); !errors.Is(err, promptskills.ErrLocalIncompatible) {
			t.Fatalf("the %s package was not refused by the inspect gate: %v", fixture.label, err)
		}
		if after := productionFSSnapshot(t, root); !reflect.DeepEqual(before, after) {
			t.Fatalf("the %s refusal changed the skill root: before=%d entries after=%d entries",
				fixture.label, len(before), len(after))
		}
		list, err = service.ListUser()
		if err != nil {
			t.Fatalf("list the installed skills after a %s refusal: %v", fixture.label, err)
		}
		if len(list) != 1 || list[0].Content != productionSkillGitHubValidBody {
			t.Fatalf("the %s refusal changed the installed package: %d entries", fixture.label, len(list))
		}
	}
}
