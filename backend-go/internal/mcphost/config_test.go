package mcphost

import (
	"context"
	"errors"
	"testing"
)

// This file exercises the credential registry directly. It is the half of this
// package a caller cannot see from the outside: the pure parse that must register
// every credential-shaped value with the host's own redactor BEFORE an entry
// becomes anything this package acts on, and that must refuse the whole file when
// that registration fails.
//
// Everything here is synthetic and assembled in memory: no file, no socket, no
// process and no host credential is read or written. The credential-shaped values
// below are concatenated from parts on purpose, so that no literal in this file is
// itself a credential-shaped token; the registry under test is what has to
// recognize them, not the reader. The end-to-end proof that a configured
// credential never reaches an HTTP answer lives beside the binder
// (cmd/novel-server/production_mcp_linux_test.go); a failure here is a failure of
// the registration boundary itself rather than of the transport around it.

// recordingRegistrar is a synthetic host redactor. It records what it was asked
// to register and can be made to refuse, which is the one way the registration
// boundary is allowed to fail.
type recordingRegistrar struct {
	seen   []string
	refuse bool
}

func (r *recordingRegistrar) register(value string) (bool, error) {
	r.seen = append(r.seen, value)
	if r.refuse {
		return false, errors.New("SYNTHETIC_REGISTRAR_REFUSAL")
	}
	return true, nil
}

func (r *recordingRegistrar) has(value string) bool {
	for _, seen := range r.seen {
		if seen == value {
			return true
		}
	}
	return false
}

// TestParseConfigRegistersEveryCredentialShape proves the registry receives every
// credential-shaped value the documented configuration can carry: the env values
// of a command entry, the userinfo secret of a URL, and every query value of a
// URL. It also proves the command entry is reduced to its symbolic refusal and
// that no command, argument or environment value survives on the entry.
func TestParseConfigRegistersEveryCredentialShape(t *testing.T) {
	userinfoSecret := "synthetic" + "-userinfo-" + "credential"
	envToken := "synthetic" + "-env-" + "credential"
	envShort := "synthetic" + "-short-" + "credential"
	queryToken := "synthetic" + "-query-" + "credential"
	raw := []byte(`{"mcpServers":{` +
		`"command-entry":{"command":"synthetic-binary","args":["--synthetic-argument"],` +
		`"env":{"TOKEN":"` + envToken + `","SHORT":"` + envShort + `"}},` +
		`"url-entry":{"url":"https://synthetic-author:` + userinfoSecret +
		`@example.test/mcp?synthetic-query-credential=` + queryToken + `"}}}`)
	registrar := &recordingRegistrar{}
	entries, err := parseConfig(raw, registrar.register)
	if err != nil {
		t.Fatal(err)
	}
	for _, credential := range []string{envToken, envShort, userinfoSecret, queryToken} {
		if !registrar.has(credential) {
			t.Fatalf("credential never registered: %s", credential)
		}
	}
	command, present := entries["command-entry"]
	if !present {
		t.Fatal("command entry missing")
	}
	if command.Transport != TransportStdio || !errors.Is(command.Refusal, ErrStdioUnavailable) || command.URL != "" {
		t.Fatalf("command entry was not reduced to its symbolic refusal: %+v", command)
	}
	if got := Code(command.Refusal); got != "MCP_STDIO_TRANSPORT_UNAVAILABLE" {
		t.Fatalf("command entry code: %s", got)
	}
	url, present := entries["url-entry"]
	if !present {
		t.Fatal("url entry missing")
	}
	if url.Transport != TransportURL || url.Refusal != nil {
		t.Fatalf("url entry: %+v", url)
	}
}

// TestParseConfigRefusesTheWholeFileWhenRegistrationFails proves the other half of
// the contract: an unregistered credential is not a secret this lane can promise to
// withhold, so the file is refused as a whole and no entry escapes the refusal.
func TestParseConfigRefusesTheWholeFileWhenRegistrationFails(t *testing.T) {
	envToken := "synthetic" + "-env-" + "credential"
	raw := []byte(`{"mcpServers":{"command-entry":{"command":"synthetic-binary",` +
		`"env":{"TOKEN":"` + envToken + `"}}}}`)
	registrar := &recordingRegistrar{refuse: true}
	entries, err := parseConfig(raw, registrar.register)
	if !errors.Is(err, ErrConfigInvalid) || entries != nil {
		t.Fatalf("a failed registration admitted entries: %+v %v", entries, err)
	}
	if len(registrar.seen) == 0 {
		t.Fatal("the registrar was never consulted")
	}
}

// TestParseConfigRefusesNonDocumentedShapes proves a document that is not the
// documented shape is refused whole rather than half-read, which is what keeps an
// author from believing a server they removed is still reachable.
func TestParseConfigRefusesNonDocumentedShapes(t *testing.T) {
	registrar := &recordingRegistrar{}
	for name, raw := range map[string]string{
		"empty":          ``,
		"not-an-object":  `[]`,
		"no-mcpServers":  `{"other":{}}`,
		"two-transports": `{"mcpServers":{"both":{"command":"synthetic-binary","url":"https://example.test/mcp"}}}`,
		"no-transport":   `{"mcpServers":{"neither":{}}}`,
		"control-in-id":  "{\"mcpServers\":{\"bad\u0000id\":{\"url\":\"https://example.test/mcp\"}}}",
		"non-string-env": `{"mcpServers":{"entry":{"command":"synthetic-binary","env":{"TOKEN":7}}}}`,
		"non-string-arg": `{"mcpServers":{"entry":{"command":"synthetic-binary","args":[7]}}}`,
	} {
		entries, err := parseConfig([]byte(raw), registrar.register)
		if !errors.Is(err, ErrConfigInvalid) || entries != nil {
			t.Fatalf("%s was admitted: %+v %v", name, entries, err)
		}
	}
}

// TestPolicyWithoutCommittedClientRefusesEveryEndpoint proves "no committed
// policy" is a refusal rather than a default allow: a manager built without the
// lane's egress client can still describe a configuration, but it cannot dial one.
func TestPolicyWithoutCommittedClientRefusesEveryEndpoint(t *testing.T) {
	for _, endpoint := range []string{"", "https://example.test/mcp", "not-a-url"} {
		if _, err := NewPolicy(nil).Admit(endpoint); !errors.Is(err, ErrPolicyUnavailable) {
			t.Fatalf("endpoint %q was not refused for want of a policy: %v", endpoint, err)
		}
	}
	var absent *Policy
	if _, err := absent.Admit("https://example.test/mcp"); !errors.Is(err, ErrPolicyUnavailable) {
		t.Fatalf("nil policy: %v", err)
	}
}

// TestNewRequiresAPathResolverAndARedactor proves a manager refuses to exist
// without the two ports its promises depend on: a path resolved by the lane, and a
// redactor that can withhold a configured credential.
func TestNewRequiresAPathResolverAndARedactor(t *testing.T) {
	resolve := func(context.Context) (string, error) { return "", nil }
	redact := func(string) (bool, error) { return true, nil }
	if _, err := New(Options{ConfigPath: resolve}); !errors.Is(err, ErrConfigUnreadable) {
		t.Fatalf("manager built without a redactor: %v", err)
	}
	if _, err := New(Options{RegisterSecret: redact}); !errors.Is(err, ErrConfigUnreadable) {
		t.Fatalf("manager built without a path resolver: %v", err)
	}
	manager, err := New(Options{ConfigPath: resolve, RegisterSecret: redact})
	if err != nil || manager == nil {
		t.Fatalf("manager with both ports: %+v %v", manager, err)
	}
}

// TestCodeIsSymbolicAndTotal proves every failure a caller can see leaves as one
// fixed symbol, that the committed policy's own vocabulary is inherited rather
// than restated, and that an unregistered error is never published as its text.
func TestCodeIsSymbolicAndTotal(t *testing.T) {
	for _, entry := range []struct {
		err  error
		code string
	}{
		{ErrConfigMissing, "MCP_CONFIG_MISSING"},
		{ErrConfigUnreadable, "MCP_CONFIG_UNREADABLE"},
		{ErrConfigInvalid, "MCP_CONFIG_INVALID"},
		{ErrServerNotConfigured, "MCP_SERVER_NOT_CONFIGURED"},
		{ErrServerNotConnected, "MCP_SERVER_NOT_CONNECTED"},
		{ErrStdioUnavailable, "MCP_STDIO_TRANSPORT_UNAVAILABLE"},
		{ErrServerURLInvalid, "MCP_SERVER_URL_INVALID"},
		{ErrPolicyUnavailable, "MCP_EGRESS_POLICY_UNAVAILABLE"},
		{ErrOriginNotAdmitted, "MCP_ORIGIN_NOT_ADMITTED"},
		{ErrToolNotDiscovered, "MCP_TOOL_NOT_DISCOVERED"},
		{context.Canceled, "MCP_REQUEST_CANCELED"},
		{context.DeadlineExceeded, "MCP_REQUEST_TIMEOUT"},
	} {
		if got := Code(entry.err); got != entry.code {
			t.Fatalf("%v: %s", entry.err, got)
		}
	}
	if got := Code(errors.New("synthetic-unregistered-failure")); got != "MCP_REQUEST_FAILED" {
		t.Fatalf("unregistered failure was published as: %s", got)
	}
	if got := Code(nil); got != "" {
		t.Fatalf("nil failure: %s", got)
	}
}
