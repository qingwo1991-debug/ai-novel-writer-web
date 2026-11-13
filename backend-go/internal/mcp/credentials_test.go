package mcp

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestCredentialRegistrationBeforeConfigAdmission(t *testing.T) {
	root := t.TempDir()
	p := writeConfig(t, root, map[string]any{"mcpServers": map[string]any{"fixture": map[string]any{"command": "never-invoked", "env": map[string]string{"TOKEN": "public-test-token", "SHORT": "ok"}}}})
	m := New(Options{ConfigPath: p, RuntimeDir: filepath.Join(root, "runtime"), TrustedAdminGrant: true})
	defer m.Close()
	if m.HasSecretRegistrar() {
		t.Fatal("unexpected registrar")
	}
	seen := make(map[string]bool)
	if err := m.SetSecretRegistrar(func(value string) (bool, error) { seen[value] = true; return len(value) >= 4, nil }); err != nil {
		t.Fatal(err)
	}
	if !m.HasSecretRegistrar() {
		t.Fatal("registrar not installed")
	}
	if err := m.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	if result := m.LoadConfig(); !result.Success {
		t.Fatalf("load: %+v", result)
	}
	if !seen["public-test-token"] || !seen["ok"] {
		t.Fatalf("not registered: %+v", seen)
	}
	if err := m.SetSecretRegistrar(func(string) (bool, error) { return true, nil }); err == nil {
		t.Fatal("enabled registrar replaced")
	}
}
func TestRegistrationFailureRevokesConfigAndConnections(t *testing.T) {
	root := t.TempDir()
	p := writeConfig(t, root, map[string]any{"mcpServers": map[string]any{"fixture": map[string]any{"command": "never-invoked", "env": map[string]string{"TOKEN": "public-test-token"}}}})
	fail := false
	m := New(Options{ConfigPath: p, RuntimeDir: filepath.Join(root, "runtime"), TrustedAdminGrant: true, RegisterSecret: func(string) (bool, error) {
		if fail {
			return false, errors.New("INTERNAL_SECRET_TEXT_NEVER_EXPOSE")
		}
		return true, nil
	}})
	defer m.Close()
	if err := m.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	if result := m.LoadConfig(); !result.Success {
		t.Fatal(result)
	}
	// A synthetic active generation proves admission failure revokes pending work
	// without starting any external process or making a network request.
	c := newConnection(serverConfig{ID: "fixture"}, m.options)
	c.status = "connected"
	c.tools = []map[string]any{{"name": "fixture"}}
	pending := make(chan response, 1)
	c.pending[1] = pending
	m.mu.Lock()
	m.servers["fixture"] = c
	m.mu.Unlock()
	fail = true
	if result := m.LoadConfig(); result.Success || result.Error != "MCP_CONFIG_INVALID" {
		t.Fatalf("unsafe registration failure: %+v", result)
	}
	if len(m.ListTools()) != 0 || len(m.ServersStatus()) != 0 {
		t.Fatal("registration failure retained generation")
	}
	if reply := <-pending; reply.err == nil || reply.err.Error() != "MCP_SERVER_NOT_CONNECTED" {
		t.Fatal("pending not revoked")
	}
	if len(m.configs) != 0 {
		t.Fatal("failed config retained")
	}
}
