package mcphost

import (
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"strings"
	"unicode/utf8"
)

// The two transport labels are exactly the values MCPServerSummary admits
// (src/shared/ipc-channels.ts:1296-1300): "stdio" for a command entry and "sse"
// for a URL entry. The desktop labels every URL entry "sse" as well
// (electron/mcp/mcp-manager.ts:211), because that union predates the newer
// streamable-HTTP revision; the protocol this package actually speaks is MCP
// streamable HTTP over one POST endpoint, not the legacy two-channel binding.
const (
	// TransportStdio labels an entry that names a command.
	TransportStdio = "stdio"
	// TransportURL labels an entry that names a server URL.
	TransportURL = "sse"
)

// Entry is one configured MCP server reduced to what this container may act on.
//
// Command, args and env are deliberately absent: an entry that would start a
// process keeps only its identity and Refusal, so the bytes that would name a
// binary, a working directory or an environment credential do not survive
// parsing at all. A URL entry keeps its URL because that is the one thing this
// transport dials, and it is never published - see the package doc.
type Entry struct {
	// ID is the key of the entry in mcpServers, which is also its display name
	// (the reference names a server by its id: mcp-manager.ts:212).
	ID string
	// Name is the display name the renderer shows. The configuration format has
	// no separate name field, so it is the id.
	Name string
	// Transport is TransportStdio or TransportURL.
	Transport string
	// URL is the configured endpoint of a TransportURL entry, and empty for a
	// refused command entry.
	URL string
	// Refusal is non-nil for an entry this container cannot serve. It is
	// ErrStdioUnavailable today, and the caller publishes Code(Refusal).
	Refusal error
}

// Summary is the MCPServerSummary projection of one entry.
type Summary struct {
	ID        string
	Name      string
	Transport string
}

// Summary projects one entry into the renderer's shape.
func (e Entry) Summary() Summary {
	return Summary{ID: e.ID, Name: e.Name, Transport: e.Transport}
}

// loadEntries reads and parses the author's configuration file. The path is
// resolved by the caller from the pinned author data directory; it is never a
// request value.
//
// A missing file is ErrConfigMissing (the honest "unconfigured" state) and every
// other admission failure is ErrConfigUnreadable, which keeps "there is no
// configuration" distinguishable from "this configuration could not be read".
func loadEntries(path string, register func(string) (bool, error)) (map[string]Entry, error) {
	if path == "" {
		return nil, ErrConfigUnreadable
	}
	raw, err := readConfigFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrConfigMissing
		}
		if errors.Is(err, ErrConfigUnreadable) {
			return nil, ErrConfigUnreadable
		}
		return nil, ErrConfigUnreadable
	}
	return parseConfig(raw, register)
}

// parseConfig turns the documented Claude-compatible document
// (electron/mcp/mcp-manager.ts:44-67: one object with an mcpServers map) into the
// entries this package can act on. It performs no I/O.
//
// The whole file is refused as ErrConfigInvalid when any of it is not that shape,
// exactly like the reference reload, because a half-read configuration would let
// an author believe a server they removed is still reachable. A single valid
// entry that names a command is NOT a file-level failure: it becomes an entry
// with Refusal set, so every other entry keeps working.
func parseConfig(raw []byte, register func(string) (bool, error)) (map[string]Entry, error) {
	if len(raw) == 0 || len(raw) > MaxConfigBytes || register == nil {
		return nil, ErrConfigInvalid
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(raw, &root) != nil || root == nil {
		return nil, ErrConfigInvalid
	}
	var servers map[string]json.RawMessage
	if json.Unmarshal(root["mcpServers"], &servers) != nil || servers == nil || len(servers) > MaxCatalogEntries {
		return nil, ErrConfigInvalid
	}
	entries := make(map[string]Entry, len(servers))
	for id, value := range servers {
		if !validServerID(id) {
			return nil, ErrConfigInvalid
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal(value, &fields) != nil || fields == nil {
			return nil, ErrConfigInvalid
		}
		// Register every credential-shaped value before the entry is admitted
		// into anything this package acts on. Registration failure revokes the
		// whole file: an unregistered credential is not a secret this lane can
		// promise to withhold.
		if err := harvestCredentialValues(fields, register); err != nil {
			return nil, err
		}
		command, err := optionalString(fields, "command")
		if err != nil {
			return nil, err
		}
		endpoint, err := optionalString(fields, "url")
		if err != nil {
			return nil, err
		}
		if err := checkStringArray(fields, "args"); err != nil {
			return nil, err
		}
		if err := checkStringMap(fields, "env"); err != nil {
			return nil, err
		}
		hasCommand := strings.TrimSpace(command) != ""
		hasURL := strings.TrimSpace(endpoint) != ""
		// Exactly one transport, and a NUL cannot appear in either: the same
		// shape the reference reload enforces (mcp-manager.ts:199-205).
		if hasCommand == hasURL || strings.ContainsRune(command, '\x00') {
			return nil, ErrConfigInvalid
		}
		entry := Entry{ID: id, Name: id, Transport: TransportStdio}
		if hasURL {
			entry.Transport = TransportURL
			entry.URL = strings.TrimSpace(endpoint)
		} else {
			// Refused by name, never attempted. This container cannot start an
			// author-supplied process.
			entry.Refusal = ErrStdioUnavailable
		}
		entries[id] = entry
	}
	return entries, nil
}

// validServerID bounds one entry key. It is a map key from the author's own file,
// so it is never authority, only an identifier this package echoes back.
func validServerID(id string) bool {
	if id == "" || len(id) > 256 || !utf8.ValidString(id) {
		return false
	}
	for _, char := range id {
		if char < 0x20 || char == '\x7f' {
			return false
		}
	}
	return true
}

// optionalString reads an optional string field, refusing a present non-string
// (including null) exactly like the reference reload.
func optionalString(fields map[string]json.RawMessage, key string) (string, error) {
	value, present := fields[key]
	if !present {
		return "", nil
	}
	var text string
	if json.Unmarshal(value, &text) != nil {
		return "", ErrConfigInvalid
	}
	return text, nil
}

// checkStringArray accepts an absent, string-only args array.
func checkStringArray(fields map[string]json.RawMessage, key string) error {
	value, present := fields[key]
	if !present {
		return nil
	}
	var items []any
	if json.Unmarshal(value, &items) != nil {
		return ErrConfigInvalid
	}
	for _, item := range items {
		if _, ok := item.(string); !ok {
			return ErrConfigInvalid
		}
	}
	return nil
}

// checkStringMap accepts an absent, string-only env map.
func checkStringMap(fields map[string]json.RawMessage, key string) error {
	value, present := fields[key]
	if !present {
		return nil
	}
	var items map[string]any
	if json.Unmarshal(value, &items) != nil || items == nil {
		return ErrConfigInvalid
	}
	for name, item := range items {
		if name == "" || strings.ContainsAny(name, "=\x00") {
			return ErrConfigInvalid
		}
		if _, ok := item.(string); !ok {
			return ErrConfigInvalid
		}
	}
	return nil
}

// harvestCredentialValues registers the credential-shaped parts of one entry: the
// values of env, and the userinfo password plus every query value of url. The
// values themselves are not retained here; registration happens once, in the
// server's own sanitizer, and the parse drops them afterwards.
func harvestCredentialValues(fields map[string]json.RawMessage, register func(string) (bool, error)) error {
	if value, present := fields["env"]; present {
		var items map[string]any
		if json.Unmarshal(value, &items) == nil {
			for _, item := range items {
				if text, ok := item.(string); ok && text != "" {
					if _, err := register(text); err != nil {
						return ErrConfigInvalid
					}
				}
			}
		}
	}
	endpoint, present := fields["url"]
	if !present {
		return nil
	}
	var text string
	if json.Unmarshal(endpoint, &text) != nil {
		return nil
	}
	parsed, err := url.Parse(text)
	if err != nil || parsed == nil {
		return nil
	}
	if parsed.User != nil {
		if password, ok := parsed.User.Password(); ok && password != "" {
			if _, err := register(password); err != nil {
				return ErrConfigInvalid
			}
		}
	}
	for _, items := range parsed.Query() {
		for _, item := range items {
			if item == "" {
				continue
			}
			if _, err := register(item); err != nil {
				return ErrConfigInvalid
			}
		}
	}
	return nil
}
