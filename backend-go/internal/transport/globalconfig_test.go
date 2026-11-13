package transport

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/globalconfig"
)

func trustedConfigFixture(t *testing.T) (*globalconfig.Service, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	config, err := globalconfig.New(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = config.Close() })
	return config, root
}
func TestGlobalConfigHTTPProjectionAndNoProxyTruthinessBypass(t *testing.T) {
	server := fixture(t)
	config, root := trustedConfigFixture(t)
	raw := `{"theme":"dark","editorFontSize":18,"models":[{"apiKey":"PRIVATE-MODEL-SECRET"}],"dialogPath":"/private/host","unknown":{"credential":"PRIVATE-EXTENSION"},"proxy":{"enabled":true,"username":"PRIVATE-PROXY-USER","password":"PRIVATE-PROXY-PASS"},"stageModelIds":{"review":"model-1"}}`
	if err := os.WriteFile(filepath.Join(root, "config.json"), []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	startup := globalconfig.NewStartup(globalconfig.StartupCapture{Generation: "admitted-test-generation"})
	if err := server.BindGlobalConfig(config, startup); err != nil {
		t.Fatal(err)
	}
	cookie, csrf := bootstrap(t, server)
	get := rpc(t, server, cookie, csrf, "config:get", `{"args":[]}`, nil)
	if get.Code != 200 || strings.Contains(get.Body.String(), "PRIVATE-") || strings.Contains(get.Body.String(), "/private/") || !strings.Contains(get.Body.String(), `"editorFontSize":18`) {
		t.Fatal(get.Code, get.Body.String())
	}
	for _, value := range []string{"null", "false", "0", `""`, "{}", `{"enabled":true}`} {
		response := rpc(t, server, cookie, csrf, "config:set", `{"args":[{"proxy":`+value+`}]}`, nil)
		if response.Code != 403 || !strings.Contains(response.Body.String(), "PROXY_SERVER_ADMIN_ONLY") {
			t.Fatal(value, response.Code, response.Body.String())
		}
	}
	for _, key := range []string{"models", "stageModelIds", "arbitraryCommand"} {
		response := rpc(t, server, cookie, csrf, "config:set", `{"args":[{"`+key+`":null}]}`, nil)
		if response.Code != 400 || !strings.Contains(response.Body.String(), "CONFIG_FIELD_DENIED") {
			t.Fatal(key, response.Code, response.Body.String())
		}
	}
	unchanged, err := os.ReadFile(filepath.Join(root, "config.json"))
	if err != nil || string(unchanged) != raw {
		t.Fatal("rejected browser config mutated private settings", err)
	}
	updated := rpc(t, server, cookie, csrf, "config:set", `{"args":[{"theme":"light"}]}`, nil)
	if updated.Code != 200 || !strings.Contains(updated.Body.String(), `"success":true`) {
		t.Fatal(updated.Body.String())
	}
	current, err := config.Get()
	if err != nil {
		t.Fatal(err)
	}
	object := current.(map[string]any)
	if object["theme"] != "light" || object["dialogPath"] != "/private/host" || object["models"].([]any)[0].(map[string]any)["apiKey"] != "PRIVATE-MODEL-SECRET" {
		t.Fatal("private extension/credential data lost")
	}
}
func TestStartupHTTPCookieAndTabIsolationExactAckAndPendingGate(t *testing.T) {
	server := fixture(t)
	config, _ := trustedConfigFixture(t)
	startup := globalconfig.NewStartup(globalconfig.StartupCapture{Generation: "committed-generation"})
	if err := server.BindGlobalConfig(config, startup); err != nil {
		t.Fatal(err)
	}
	cookie, csrf := bootstrap(t, server)
	ack := func(body string, headers map[string]string) bool {
		response := rpc(t, server, cookie, csrf, "startup:appearance-ack", body, headers)
		if response.Code != 200 {
			t.Fatal(response.Body.String())
		}
		return strings.Contains(response.Body.String(), `"result":true`)
	}
	makeAck := func(revision int64, generation string, extra bool) string {
		value := map[string]any{"storageKey": globalconfig.AppearanceStorageKey, "profileRevision": revision, "globalGeneration": generation}
		if extra {
			value["extra"] = true
		}
		data, _ := json.Marshal(map[string]any{"args": []any{value}})
		return string(data)
	}
	if !ack(makeAck(8, "committed-generation", false), nil) || ack(makeAck(7, "committed-generation", false), nil) || ack(makeAck(9, "stale", false), nil) || ack(makeAck(9, "committed-generation", true), nil) {
		t.Fatal("invalid acknowledgment accepted or valid acknowledgment refused")
	}
	for _, headers := range []map[string]string{nil, {"X-Client-Id": "synthetic-tab-0002"}} {
		response := rpc(t, server, cookie, csrf, "startup:get-state", `{"args":[]}`, headers)
		hasFloor := strings.Contains(response.Body.String(), `"appearanceRevisionFloor":8`)
		if response.Code != 200 || hasFloor != (headers == nil) {
			t.Fatal(response.Body.String())
		}
	}
	cookie2, csrf2 := bootstrap(t, server)
	other := rpc(t, server, cookie2, csrf2, "startup:get-state", `{"args":[]}`, nil)
	if strings.Contains(other.Body.String(), "appearanceRevisionFloor") {
		t.Fatal("appearance acknowledgment crossed authenticated sessions")
	}
	pending := fixture(t)
	pendingConfig, _ := trustedConfigFixture(t)
	if err := pending.BindGlobalConfig(pendingConfig, globalconfig.NewStartup(globalconfig.StartupCapture{})); err != nil {
		t.Fatal(err)
	}
	pc, pt := bootstrap(t, pending)
	response := rpc(t, pending, pc, pt, "config:get", `{"args":[]}`, nil)
	if response.Code != 409 || !strings.Contains(response.Body.String(), "GLOBAL_DATA_NOT_READY") {
		t.Fatal(response.Body.String())
	}
}
func TestGlobalConfigBindingIsAtomic(t *testing.T) {
	server := fixture(t)
	config, _ := trustedConfigFixture(t)
	if err := server.Bind("startup:appearance-ack", func(context.Context, Caller, []any) (any, error) { return false, nil }); err != nil {
		t.Fatal(err)
	}
	if err := server.BindGlobalConfig(config, globalconfig.NewStartup(globalconfig.StartupCapture{})); err == nil {
		t.Fatal("collision accepted")
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.handlers["config:get"] != nil || server.handlers["config:set"] != nil || server.handlers["startup:get-state"] != nil {
		t.Fatal("partial config handler family published")
	}
}
