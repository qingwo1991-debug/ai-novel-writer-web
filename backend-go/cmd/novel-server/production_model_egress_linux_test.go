//go:build linux

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/egress"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/globalconfig"
)

// The credentials below exist only inside a disposable t.TempDir() tree. They
// prove that a secret committed next to the model configuration never reaches the
// policy, a log line, an error value or a returned value. None of them is real.
const (
	productionModelOriginSecret = "synthetic-private-origin-secret"
	productionModelProxySecret  = "synthetic-private-proxy-secret"
)

// productionLogCapture is a race-safe log sink. The lane may log from its own
// worker goroutines while the test reads what was written, so the buffer is
// guarded instead of borrowed.
type productionLogCapture struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (c *productionLogCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buffer.Write(p)
}

func (c *productionLogCapture) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buffer.String()
}

// productionCommittedConfigRoot writes one private committed global root:
// config.json is the document llm:get-default-model reads, models.json the private
// model store. An empty body means that document is absent.
func productionCommittedConfigRoot(t *testing.T, config, models string) *globalconfig.Service {
	t.Helper()
	root := t.TempDir()
	if config != "" {
		if err := os.WriteFile(filepath.Join(root, "config.json"), []byte(config), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if models != "" {
		if err := os.WriteFile(filepath.Join(root, "models.json"), []byte(models), 0600); err != nil {
			t.Fatal(err)
		}
	}
	service, err := globalconfig.New(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	return service
}

// productionCommittedConfigPath and productionCommittedModelsPath name the two
// documents of the committed generation a production fixture pins.
func productionCommittedConfigPath(root string) string {
	return filepath.Join(root, productionConfigDirectory, productionConfigTarget,
		"generations", productionTestGeneration, "config.json")
}

func productionCommittedModelsPath(root string) string {
	return filepath.Join(root, productionConfigDirectory, productionConfigTarget,
		"generations", productionTestGeneration, "models.json")
}

// productionCommitModelOrigins rewrites the fixture's committed config document
// before the lane pins it: it adds the explicit allowlist and an unrelated proxy
// credential the policy must never copy.
func productionCommitModelOrigins(t *testing.T, root string, origins []string) {
	t.Helper()
	raw, err := os.ReadFile(productionCommittedConfigPath(root))
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	list := make([]any, 0, len(origins))
	for _, origin := range origins {
		list = append(list, origin)
	}
	document[globalconfig.ModelOriginsKey] = list
	document["proxy"] = map[string]any{
		"enabled": true, "type": "http", "host": "127.0.0.1", "port": 7890,
		"password": productionModelProxySecret,
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(productionCommittedConfigPath(root), encoded, 0600); err != nil {
		t.Fatal(err)
	}
}

// TestProductionModelEgressMatchesTheCommittedAllowlist proves the policy is
// exactly the author's committed allowlist: nothing is added, nothing is widened,
// and a profile endpoint outside it stays refused.
func TestProductionModelEgressMatchesTheCommittedAllowlist(t *testing.T) {
	service := productionCommittedConfigRoot(t,
		`{"theme":"dark","defaultModelId":null,"modelOrigins":["https://api.example.invalid:8443","http://127.0.0.1:8317"]}`,
		`[{"id":"story","provider":"custom","protocol":"openai","baseUrl":"https://profile.example.invalid/v1","apiKey":"`+productionTestModelSecret+`"}]`)
	plan, err := productionModelEgressPlan(service)
	if err != nil {
		t.Fatal(err)
	}
	defer plan.policy.Close()
	want := []string{"http://127.0.0.1:8317", "https://api.example.invalid:8443"}
	if !slices.Equal(plan.origins, want) {
		t.Fatalf("policy origins = %v; want exactly the committed allowlist %v", plan.origins, want)
	}
	if err := plan.policy.CheckBaseURL("https://api.example.invalid:8443/v1/chat/completions"); err != nil {
		t.Fatalf("a committed origin was refused: %v", err)
	}
	if err := plan.policy.CheckBaseURL("https://profile.example.invalid/v1"); !errors.Is(err, egress.ErrNotAllowed) {
		t.Fatalf("an endpoint outside the committed allowlist was admitted: %v", err)
	}
	if strings.Contains(strings.Join(plan.origins, ","), productionTestModelSecret) {
		t.Fatal("the model credential was copied into the policy provenance")
	}
}

// TestProductionModelEgressDerivesFromCommittedModelEndpoints proves the second
// source: when the config declares no allowlist, the endpoints the author already
// committed in models.json become the origins, and the endpoint path is dropped.
func TestProductionModelEgressDerivesFromCommittedModelEndpoints(t *testing.T) {
	service := productionCommittedConfigRoot(t, `{"theme":"dark"}`,
		`[{"id":"story","provider":"custom","protocol":"openai","baseUrl":"https://synthetic.invalid/v1","apiKey":"`+productionTestModelSecret+`"}]`)
	plan, err := productionModelEgressPlan(service)
	if err != nil {
		t.Fatal(err)
	}
	defer plan.policy.Close()
	if !slices.Equal(plan.origins, []string{"https://synthetic.invalid"}) {
		t.Fatalf("origins = %v; want the committed endpoint reduced to its origin", plan.origins)
	}
	if err := plan.policy.CheckBaseURL("https://synthetic.invalid/v1/chat/completions"); err != nil {
		t.Fatalf("the committed model endpoint was refused: %v", err)
	}
	if strings.Contains(strings.Join(plan.origins, ","), productionTestModelSecret) {
		t.Fatal("the model credential was copied into the policy")
	}
}

// TestProductionModelEgressRefusesRatherThanAllowingByDefault drives every
// refusal shape: an absent origin, a malformed one, a wildcard, a relative value,
// a path, another scheme, userinfo, and a wrong JSON type. Each must produce a
// symbolic refusal and no policy at all - never a default-allow policy.
func TestProductionModelEgressRefusesRatherThanAllowingByDefault(t *testing.T) {
	profile := `[{"id":"story","provider":"custom","protocol":"openai","baseUrl":"https://profile.example.invalid/v1","apiKey":"synthetic"}]`
	cases := []struct {
		name   string
		config string
		models string
		want   error
	}{
		{"no-committed-origin", `{"theme":"dark"}`, "", errProductionModelEgressUnconfigured},
		{"empty-model-store", `{"theme":"dark"}`, `[]`, errProductionModelEgressUnconfigured},
		{"empty-allowlist", `{"modelOrigins":[]}`, "", errProductionModelEgressUnconfigured},
		{"damaged-config", `{"modelOrigins":`, "", errProductionModelEgressUnavailable},
		{"config-is-not-an-object", `null`, "", errProductionModelEgressUnavailable},
		{"userinfo-endpoint", `{"modelOrigins":["https://author:` + productionModelOriginSecret + `@api.example.invalid"]}`, "", errProductionModelEgressInvalid},
		{"wildcard-origin", `{"modelOrigins":["https://*.example.invalid"]}`, "", errProductionModelEgressInvalid},
		{"relative-origin", `{"modelOrigins":["api.example.invalid"]}`, "", errProductionModelEgressInvalid},
		{"path-is-not-an-origin", `{"modelOrigins":["https://api.example.invalid/v1"]}`, "", errProductionModelEgressInvalid},
		{"non-http-scheme", `{"modelOrigins":["ftp://api.example.invalid"]}`, "", errProductionModelEgressInvalid},
		{"not-an-array", `{"modelOrigins":"https://api.example.invalid"}`, "", errProductionModelEgressInvalid},
		{"non-string-entry", `{"modelOrigins":[8443]}`, "", errProductionModelEgressInvalid},
		{"malformed-allowlist-wins-over-profiles", `{"modelOrigins":["https://*.example.invalid"]}`, profile, errProductionModelEgressInvalid},
		{"profile-without-endpoint", `{"theme":"dark"}`, `[{"id":"story","provider":"custom","protocol":"openai","apiKey":"synthetic"}]`, errProductionModelEgressInvalid},
		{"profile-userinfo-endpoint", `{"theme":"dark"}`, `[{"id":"story","provider":"custom","protocol":"openai","baseUrl":"https://author:` + productionModelOriginSecret + `@api.example.invalid/v1","apiKey":"synthetic"}]`, errProductionModelEgressInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := productionModelEgressPlan(productionCommittedConfigRoot(t, tc.config, tc.models))
			if !errors.Is(err, tc.want) {
				t.Fatalf("%s refused with %v; want %v", tc.name, err, tc.want)
			}
			if plan != nil {
				t.Fatalf("%s produced a policy instead of refusing", tc.name)
			}
			// A refusal is symbolic: it never echoes the rejected committed value.
			if strings.Contains(err.Error(), productionModelOriginSecret) ||
				strings.Contains(err.Error(), "api.example.invalid") {
				t.Fatalf("%s echoed the rejected value: %v", tc.name, err)
			}
		})
	}
}

// TestProductionModelEgressWiresCommittedOriginsWithoutDialing proves the lane
// keeps a real policy built from the committed config, and that constructing it
// neither dials the committed origin nor logs a committed credential.
func TestProductionModelEgressWiresCommittedOriginsWithoutDialing(t *testing.T) {
	root := syntheticProductionDataDir(t)
	// The exact origin is a loopback listener this test owns, so any dispatch
	// attempt during startup would land in its backlog.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	origin := "http://" + listener.Addr().String()
	productionCommitModelOrigins(t, root, []string{origin})
	layout, err := productionLayoutFor(root)
	if err != nil {
		t.Fatal(err)
	}

	var logs productionLogCapture
	previous := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(previous)
	open, err := openProduction(context.Background(), layout, productionConfig{
		Schema: productionTestContract(), MaxDatabaseBytes: productionMaxDatabaseBytes,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer open.Close()

	if open.modelEgress == nil {
		t.Fatalf("the lane built no policy: %v", open.modelEgressErr)
	}
	if !slices.Equal(open.modelEgress.origins, []string{origin}) {
		t.Fatalf("lane policy origins = %v; want exactly %q", open.modelEgress.origins, origin)
	}
	policy, err := open.ModelEgress()
	if err != nil || policy == nil {
		t.Fatalf("ModelEgress = %v, %v; want the committed policy", policy, err)
	}
	if err := policy.CheckBaseURL(origin + "/v1"); err != nil {
		t.Fatalf("the committed origin was refused by the lane policy: %v", err)
	}
	// No network call was made: a dial would already be queued on this listener.
	if tcp, ok := listener.(*net.TCPListener); ok {
		if err := tcp.SetDeadline(time.Now().Add(150 * time.Millisecond)); err != nil {
			t.Fatal(err)
		}
	}
	if connection, acceptErr := listener.Accept(); acceptErr == nil {
		_ = connection.Close()
		t.Fatal("building the model-egress policy dialed the committed origin")
	}
	// Credentials committed next to the origin never reach a log line.
	for _, secret := range []string{productionTestModelSecret, productionModelProxySecret} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("the lane logged the committed credential %q", secret)
		}
	}
	if !strings.Contains(logs.String(), "restricted to 1 committed origin") {
		t.Fatalf("the lane did not report its committed policy: %s", logs.String())
	}
}

// TestProductionLaneRefusesAbsentOrMalformedOriginWithoutFailingStartup proves an
// optional provider field stays optional: a committed config with no origin, or
// with a malformed one, leaves the policy nil with a specific refusal, does not
// fail startup, and never becomes a default-allow policy.
func TestProductionLaneRefusesAbsentOrMalformedOriginWithoutFailingStartup(t *testing.T) {
	cases := []struct {
		name   string
		commit func(t *testing.T, root string)
		want   error
	}{
		{"absent", func(t *testing.T, root string) {
			// No allowlist and no committed model store: the config names no origin
			// at all. The routing ids go with the store so the generation stays a
			// valid committed document.
			if err := os.WriteFile(productionCommittedConfigPath(root),
				[]byte(`{"theme":"dark","defaultModelId":null}`), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(productionCommittedModelsPath(root), []byte(`[]`), 0600); err != nil {
				t.Fatal(err)
			}
		}, errProductionModelEgressUnconfigured},
		{"malformed", func(t *testing.T, root string) {
			productionCommitModelOrigins(t, root, []string{"https://*.example.invalid"})
		}, errProductionModelEgressInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := syntheticProductionDataDir(t)
			tc.commit(t, root)
			layout, err := productionLayoutFor(root)
			if err != nil {
				t.Fatal(err)
			}
			open, err := openProduction(context.Background(), layout, productionConfig{
				Schema: productionTestContract(), MaxDatabaseBytes: productionMaxDatabaseBytes,
			})
			if err != nil {
				t.Fatalf("an optional origin field failed startup: %v", err)
			}
			defer open.Close()
			policy, egressErr := open.ModelEgress()
			if policy != nil || !errors.Is(egressErr, tc.want) {
				t.Fatalf("ModelEgress = %v, %v; want no policy and %v", policy, egressErr, tc.want)
			}
			if open.modelEgress != nil {
				t.Fatal("the lane kept a policy after refusing the committed origin")
			}
			// The refusal did not break the lane it was derived for.
			if len(open.projects) != 1 {
				t.Fatalf("the lane lost its admitted project: %d", len(open.projects))
			}
		})
	}
}

// TestProductionModelEgressDoesNotBindTheRemainingGenerationExecution proves the
// policy is only a dispatch boundary: with an origin committed, every generation
// name the lane does not own still fails closed and the committed-state reads keep
// answering. generation:execute is the one dispatch name the lane does own, so it
// is excluded here and asserted by
// TestProductionLaneExecutesAdmittedRunThroughTheCommittedAllowlist.
func TestProductionModelEgressDoesNotBindTheRemainingGenerationExecution(t *testing.T) {
	root := syntheticProductionDataDir(t)
	productionCommitModelOrigins(t, root, []string{"https://api.example.invalid:8443"})
	handler, cleanup, err := productionHandlerWithContract(migrationRegistry(t), productionDurableAddress, "", root, productionConfig{
		Schema: productionTestContract(), MaxDatabaseBytes: productionMaxDatabaseBytes,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	author := newProductionDurableClient(t, handler, productionTestTab)
	// generation:begin is the admitted run-admission name (asserted by
	// TestProductionLaneAdmitsGenerationBeginUnderTheLiveLease) and
	// generation:execute the admitted dispatch name; every other name stays
	// unbound even with a policy in place.
	for _, operation := range []string{
		"generation:bind-material-decision",
		"generation:restart",
		"generation:discard-candidate", "generation:retry-draft-short-outline",
		"graph-generation:cancel", "finalization-generation:cancel",
	} {
		response := author.call(t, operation, []any{})
		if response.Code != http.StatusNotImplemented ||
			!strings.Contains(response.Body.String(), "GO_OPERATION_NOT_MIGRATED") {
			t.Fatalf("%s answered %d instead of the lane's fail-closed 501: %s",
				operation, response.Code, response.Body.String())
		}
	}
	// The already-served model reads are untouched by the new policy.
	var defaultModel any
	author.decode(t, "llm:get-default-model", []any{}, &defaultModel)
	if defaultModel != productionTestModelID {
		t.Fatalf("llm:get-default-model regressed: %v", defaultModel)
	}
}
