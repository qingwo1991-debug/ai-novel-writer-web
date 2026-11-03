//go:build linux

// Package globaladmission verifies a completed original global-data migration
// without writing the source, the generation, or startup state.
package globaladmission

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf8"
)

const maxJSONBytes = 16 << 20

var generationID = regexp.MustCompile(`^[a-f0-9-]{36}$`)

var objectOrder = [...]string{"config.json", "models.json", "recent-projects.json", "mcp_config.json", "prompts", "skills", "skins"}

var objects = map[string]bool{
	"config.json": true, "models.json": true, "recent-projects.json": true,
	"mcp_config.json": true, "prompts": true, "skills": true, "skins": true,
}

// Roots are administrator-owned locations. The caller must hold the global
// single-writer exclusion while verifying them; no browser field is authority.
type Roots struct {
	LegacySource    string
	CanonicalTarget string
	UserData        string
}

type Ready struct {
	State                 string `json:"state"`
	GlobalGeneration      string `json:"globalGeneration"`
	DataRoot              string `json:"dataRoot"`
	LegacySourceIgnored   bool   `json:"legacySourceIgnored"`
	PreservedUnknownCount int64  `json:"preservedUnknownCount"`
}

type Failure struct{ Code string }

func (f *Failure) Error() string { return f.Code }

func fail(code string) error { return &Failure{Code: code} }

// Code returns the original migration-style public rejection code, if any.
func Code(err error) string {
	var failure *Failure
	if errors.As(err, &failure) {
		return failure.Code
	}
	return ""
}

func record(value any) (map[string]any, bool) {
	mapped, ok := value.(map[string]any)
	return mapped, ok
}

func cleanAbsolute(path string) bool {
	return path != "" && filepath.IsAbs(path) && filepath.Clean(path) == path
}

func safeRoot(path string) (bool, error) {
	if !cleanAbsolute(path) {
		return false, fail("GLOBAL_ROOT_UNSAFE")
	}
	current := string(filepath.Separator)
	parts := strings.Split(strings.TrimPrefix(path, current), current)
	exists := true
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return false, fail("GLOBAL_ROOT_UNSAFE")
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			exists = false
			break
		}
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return false, fail("GLOBAL_ROOT_UNSAFE")
		}
		if current == path {
			stat, ok := info.Sys().(*syscall.Stat_t)
			if !ok || stat.Uid != uint32(os.Geteuid()) || info.Mode().Perm()&0022 != 0 {
				return false, fail("GLOBAL_ROOT_UNSAFE")
			}
		}
	}
	return exists, nil
}

func rootsSafe(roots Roots) error {
	values := []string{roots.LegacySource, roots.CanonicalTarget, roots.UserData}
	for _, value := range values {
		if _, err := safeRoot(value); err != nil {
			return err
		}
	}
	for i := range values {
		for j := i + 1; j < len(values); j++ {
			if values[i] == values[j] || strings.HasPrefix(values[i], values[j]+string(filepath.Separator)) || strings.HasPrefix(values[j], values[i]+string(filepath.Separator)) {
				return fail("GLOBAL_ROOT_INTERSECTION")
			}
		}
	}
	return nil
}

func selectedEntry(path string) (os.FileInfo, bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fail("GLOBAL_MIGRATION_IO_FAILED")
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, true, fail("GLOBAL_ASSET_LINK")
	}
	return info, true, nil
}

func singleFile(path string) error {
	info, exists, err := selectedEntry(path)
	if err != nil {
		return err
	}
	if !exists || !info.Mode().IsRegular() {
		return fail("GLOBAL_ASSET_TYPE")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Nlink != 1 {
		return fail("GLOBAL_ASSET_HARDLINK")
	}
	if stat.Uid != uint32(os.Geteuid()) || info.Mode().Perm()&0022 != 0 {
		return fail("GLOBAL_ASSET_TYPE")
	}
	return nil
}

func parseJSON(path string) (any, error) {
	if err := singleFile(path); err != nil {
		return nil, fail("GLOBAL_JSON_INVALID")
	}
	before, err := os.Lstat(path)
	if err != nil {
		return nil, fail("GLOBAL_JSON_INVALID")
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fail("GLOBAL_JSON_INVALID")
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	actual, err := file.Stat()
	if err != nil || !os.SameFile(before, actual) {
		return nil, fail("GLOBAL_JSON_INVALID")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxJSONBytes+1))
	if err != nil || len(data) > maxJSONBytes {
		return nil, fail("GLOBAL_JSON_INVALID")
	}
	// JSON.parse preserves isolated UTF-16 surrogate escapes, while Go's JSON
	// decoder replaces each with U+FFFD. Replacing distinct model IDs could
	// falsely satisfy a default-model reference, so reject that encoding.
	if !utf8.Valid(data) || loneSurrogateEscape(string(data)) {
		return nil, fail("GLOBAL_JSON_INVALID")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, fail("GLOBAL_JSON_INVALID")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, fail("GLOBAL_JSON_INVALID")
	}
	return value, nil
}

func selectedFiles(root string) ([]string, error) {
	var names []string
	for _, name := range objectOrder {
		path := filepath.Join(root, name)
		info, exists, err := selectedEntry(path)
		if err != nil {
			return nil, err
		}
		if !exists {
			continue
		}
		if strings.HasSuffix(name, ".json") {
			if err := singleFile(path); err != nil {
				return nil, err
			}
			names = append(names, name)
			continue
		}
		if !info.IsDir() {
			return nil, fail("GLOBAL_ASSET_TYPE")
		}
		err = filepath.WalkDir(path, func(location string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return fail("GLOBAL_MIGRATION_IO_FAILED")
			}
			if location == path {
				current, err := os.Lstat(path)
				if err != nil || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(info, current) {
					return fail("GLOBAL_ASSET_LINK")
				}
				return nil
			}
			info, err := os.Lstat(location)
			if err != nil {
				return fail("GLOBAL_MIGRATION_IO_FAILED")
			}
			if info.Mode()&os.ModeSymlink != 0 {
				return fail("GLOBAL_ASSET_LINK")
			}
			if info.IsDir() {
				return nil
			}
			if !info.Mode().IsRegular() {
				return fail("GLOBAL_ASSET_TYPE")
			}
			if stat, ok := info.Sys().(*syscall.Stat_t); !ok || stat.Nlink != 1 {
				return fail("GLOBAL_ASSET_HARDLINK")
			}
			relative, err := filepath.Rel(root, location)
			if err != nil {
				return fail("GLOBAL_MIGRATION_IO_FAILED")
			}
			names = append(names, relative)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return names, nil
}

func optionalJSON(path string, missing any) (any, error) {
	_, exists, err := selectedEntry(path)
	if err != nil {
		return nil, err
	}
	if !exists {
		return missing, nil
	}
	return parseJSON(path)
}

func validateGeneration(root string) error {
	names, err := selectedFiles(root)
	if err != nil {
		return err
	}
	configValue, err := optionalJSON(filepath.Join(root, "config.json"), map[string]any{})
	if err != nil {
		return err
	}
	modelsValue, err := optionalJSON(filepath.Join(root, "models.json"), []any{})
	if err != nil {
		return err
	}
	config, configOK := record(configValue)
	models, modelsOK := modelsValue.([]any)
	if !configOK || !modelsOK {
		return fail("GLOBAL_CONFIG_SCHEMA_INVALID")
	}
	ids := make(map[string]bool)
	for _, raw := range models {
		model, ok := record(raw)
		if !ok {
			return fail("GLOBAL_MODEL_ID_INVALID")
		}
		id, ok := model["id"].(string)
		if !ok || id == "" || ids[id] {
			return fail("GLOBAL_MODEL_ID_INVALID")
		}
		for _, key := range []string{"apiKey", "baseUrl", "name", "provider", "protocol", "modelName"} {
			if value, present := model[key]; present {
				if _, ok := value.(string); !ok {
					return fail("GLOBAL_MODEL_SCHEMA_INVALID")
				}
			}
		}
		ids[id] = true
	}
	if value, present := config["updatePreferences"]; present {
		if _, ok := record(value); !ok {
			return fail("GLOBAL_UPDATE_PREFERENCES_INVALID")
		}
	}
	for _, key := range []string{"defaultModelId", "defaultEmbeddingModelId"} {
		if value := config[key]; value != nil {
			id, ok := value.(string)
			if !ok || !ids[id] {
				return fail("GLOBAL_DEFAULT_MODEL_MISSING")
			}
		}
	}
	for _, name := range []string{"recent-projects.json", "mcp_config.json"} {
		path := filepath.Join(root, name)
		_, exists, err := selectedEntry(path)
		if err != nil {
			return err
		}
		if !exists {
			continue
		}
		value, err := parseJSON(path)
		if err != nil {
			return err
		}
		if name == "recent-projects.json" {
			if _, ok := value.([]any); !ok {
				return fail("GLOBAL_OBJECT_SCHEMA_INVALID")
			}
			continue
		}
		mcp, ok := record(value)
		if !ok {
			return fail("GLOBAL_OBJECT_SCHEMA_INVALID")
		}
		servers, ok := record(mcp["mcpServers"])
		if !ok {
			return fail("GLOBAL_OBJECT_SCHEMA_INVALID")
		}
		for _, raw := range servers {
			server, ok := record(raw)
			if !ok {
				return fail("GLOBAL_MCP_INVALID")
			}
			command, commandOK := server["command"].(string)
			url, urlOK := server["url"].(string)
			hasCommand, hasURL := commandOK && jsTrim(command) != "", urlOK && jsTrim(url) != ""
			if hasCommand == hasURL {
				return fail("GLOBAL_MCP_INVALID")
			}
			if value, present := server["args"]; present {
				args, ok := value.([]any)
				if !ok {
					return fail("GLOBAL_MCP_INVALID")
				}
				for _, arg := range args {
					if _, ok := arg.(string); !ok {
						return fail("GLOBAL_MCP_INVALID")
					}
				}
			}
			if value, present := server["env"]; present {
				env, ok := record(value)
				if !ok {
					return fail("GLOBAL_MCP_INVALID")
				}
				for _, entry := range env {
					if _, ok := entry.(string); !ok {
						return fail("GLOBAL_MCP_INVALID")
					}
				}
			}
		}
	}
	for _, name := range names {
		if strings.HasPrefix(name, "prompts"+string(filepath.Separator)) && strings.HasSuffix(name, ".json") {
			value, err := parseJSON(filepath.Join(root, name))
			if err != nil {
				return err
			}
			if _, ok := record(value); !ok {
				return fail("GLOBAL_PROMPT_INVALID")
			}
		}
	}
	return nil
}

func safeInteger(value any) (int64, bool) {
	number, ok := value.(json.Number)
	if !ok {
		return 0, false
	}
	parsed, err := strconv.ParseFloat(string(number), 64)
	if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) || parsed < 0 ||
		parsed > 9007199254740991 || math.Trunc(parsed) != parsed {
		return 0, false
	}
	return int64(parsed), true
}

func jsTrim(value string) string {
	return strings.TrimFunc(value, func(r rune) bool {
		switch r {
		case '\t', '\n', '\v', '\f', '\r', ' ', '\u00a0', '\u1680', '\u2028', '\u2029', '\u202f', '\u205f', '\u3000', '\ufeff':
			return true
		}
		return r >= '\u2000' && r <= '\u200a'
	})
}

func hexQuad(value string, offset int) (uint16, bool) {
	if offset+4 > len(value) {
		return 0, false
	}
	var result uint16
	for i := offset; i < offset+4; i++ {
		result <<= 4
		switch c := value[i]; {
		case c >= '0' && c <= '9':
			result |= uint16(c - '0')
		case c >= 'a' && c <= 'f':
			result |= uint16(c - 'a' + 10)
		case c >= 'A' && c <= 'F':
			result |= uint16(c - 'A' + 10)
		default:
			return 0, false
		}
	}
	return result, true
}

func loneSurrogateEscape(value string) bool {
	inString := false
	for i := 0; i < len(value); i++ {
		switch value[i] {
		case '"':
			inString = !inString
		case '\\':
			if !inString || i+1 >= len(value) {
				continue
			}
			if value[i+1] != 'u' {
				i++
				continue
			}
			code, ok := hexQuad(value, i+2)
			if !ok {
				continue
			} // JSON decoder rejects malformed escapes.
			if code >= 0xdc00 && code <= 0xdfff {
				return true
			}
			if code >= 0xd800 && code <= 0xdbff {
				if i+11 >= len(value) || value[i+6] != '\\' || value[i+7] != 'u' {
					return true
				}
				low, ok := hexQuad(value, i+8)
				if !ok || low < 0xdc00 || low > 0xdfff {
					return true
				}
				i += 11
				continue
			}
			i += 5
		}
	}
	return false
}

// VerifyCompleted reads the original completed receipt and its generation.
// It does not claim source-byte equality after receipt publication or install
// startup state. The caller retains the global writer lock throughout.
func VerifyCompleted(ctx context.Context, roots Roots) (Ready, error) {
	var zero Ready
	if ctx == nil {
		return zero, fail("GLOBAL_MIGRATION_IO_FAILED")
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	if err := rootsSafe(roots); err != nil {
		return zero, err
	}
	receiptPath := filepath.Join(roots.CanonicalTarget, ".migration", "receipt.json")
	if _, err := safeRoot(filepath.Dir(receiptPath)); err != nil {
		return zero, err
	}
	if _, exists, err := selectedEntry(receiptPath); err != nil {
		return zero, fail("GLOBAL_JSON_INVALID")
	} else if !exists {
		return zero, fail("GLOBAL_RECEIPT_MISSING")
	}
	value, err := parseJSON(receiptPath)
	if err != nil {
		return zero, err
	}
	receipt, ok := record(value)
	if !ok {
		return zero, fail("GLOBAL_RECEIPT_INVALID")
	}
	version, versionOK := safeInteger(receipt["version"])
	count, countOK := safeInteger(receipt["preservedUnknownCount"])
	generation, generationOK := receipt["generation"].(string)
	completed, completedOK := receipt["completed"].(bool)
	required, requiredOK := receipt["requiredObjects"].([]any)
	if !versionOK || version != 1 || !countOK || !generationOK || !generationID.MatchString(generation) || !completedOK || !completed || !requiredOK {
		return zero, fail("GLOBAL_RECEIPT_INVALID")
	}
	dataRoot := filepath.Join(roots.CanonicalTarget, "generations", generation)
	exists, err := safeRoot(dataRoot)
	if err != nil {
		return zero, err
	}
	if !exists {
		return zero, fail("GLOBAL_COMMITTED_GENERATION_MISSING")
	}
	for _, raw := range required {
		name, ok := raw.(string)
		if !ok || !objects[name] {
			return zero, fail("GLOBAL_RECEIPT_INVALID")
		}
		_, exists, err := selectedEntry(filepath.Join(dataRoot, name))
		if err != nil {
			return zero, err
		}
		if !exists {
			return zero, fail("GLOBAL_COMMITTED_OBJECT_MISSING")
		}
	}
	if err := validateGeneration(dataRoot); err != nil {
		return zero, err
	}
	legacyExists, err := safeRoot(roots.LegacySource)
	if err != nil {
		return zero, err
	}
	return Ready{State: "ready", GlobalGeneration: generation, DataRoot: dataRoot,
		LegacySourceIgnored: legacyExists, PreservedUnknownCount: count}, nil
}
