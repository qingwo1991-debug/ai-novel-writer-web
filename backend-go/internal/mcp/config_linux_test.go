//go:build linux

package mcp

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCredentialConfigRejectsUnsafeFileEntries(t *testing.T) {
	root := t.TempDir()
	config := writeConfig(t, root, map[string]any{"mcpServers": map[string]any{"fixture": map[string]any{"command": "trusted-tool"}}})
	if _, err := readConfigFile(config); err != nil {
		t.Fatal("private regular config rejected", err)
	}
	if err := os.Chmod(config, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := readConfigFile(config); err == nil {
		t.Fatal("group/world-readable config accepted")
	}
	if err := os.Chmod(config, 0600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias.json")
	if err := os.Link(config, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := readConfigFile(config); err == nil {
		t.Fatal("multi-linked credential config accepted")
	}
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(config, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := readConfigFile(alias); err == nil {
		t.Fatal("symlink config accepted")
	}
	if _, err := readConfigFile(filepath.Join(root, "missing.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("missing config lost its distinct status", err)
	}
}

func TestUnsafeCredentialConfigRevokesLoadedGeneration(t *testing.T) {
	root := t.TempDir()
	path := writeConfig(t, root, map[string]any{"mcpServers": map[string]any{"fixture": map[string]any{"command": "trusted-tool"}}})
	m := New(Options{ConfigPath: path, RuntimeDir: filepath.Join(root, "runtime"), TrustedAdminGrant: true})
	defer m.Close()
	if err := m.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	if loaded := m.LoadConfig(); !loaded.Success || loaded.Status != "loaded" {
		t.Fatal("private config did not load", loaded)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if loaded := m.LoadConfig(); loaded.Success || loaded.Error != "MCP_CONFIG_INVALID" {
		t.Fatal("unsafe reload admitted", loaded)
	}
	if len(m.configs) != 0 || len(m.ServersStatus()) != 0 {
		t.Fatal("unsafe reload retained live configuration")
	}
}
