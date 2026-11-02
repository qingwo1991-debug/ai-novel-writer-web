// Package projectfs provides read-only, descriptor-confined project admission.
// It does not create projects, migrate data, or establish SQLite validity.
package projectfs

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"time"
	"unicode/utf8"
)

const (
	StorageDirectory = ".ai-novel"
	DatabaseName = "project.db"
	ManifestName = "project.json"
	manifestLimit int64 = 64 * 1024
	journalLimit int64 = 256 * 1024
	// MaxSnapshotBytes is a hard ceiling independent of caller-provided limits.
	MaxSnapshotBytes int64 = 64 * 1024 * 1024
)

var (
	ErrInvalidPath = errors.New("PROJECT_INVALID_PATH")
	ErrUnsafeDirectory = errors.New("PROJECT_STORAGE_UNSAFE_DIRECTORY")
	ErrUnsafeFile = errors.New("PROJECT_STORAGE_UNSAFE_FILE")
	ErrManifestInvalid = errors.New("PROJECT_MANIFEST_INVALID")
	ErrManifestChanged = errors.New("PROJECT_MANIFEST_CHANGED")
	ErrMigrationRequired = errors.New("PROJECT_MIGRATION_REQUIRED")
	ErrRecoveryRequired = errors.New("PROJECT_MIGRATION_RECOVERY_REQUIRED")
	ErrChanged = errors.New("PROJECT_SOURCE_CHANGED")
	ErrTooLarge = errors.New("PROJECT_FILE_TOO_LARGE")
	ErrClosed = errors.New("PROJECT_HANDLE_CLOSED")
	ErrUnsupported = errors.New("PROJECT_PLATFORM_UNSUPPORTED")
	ErrDatabaseSnapshotRequired = errors.New("PROJECT_DATABASE_SNAPSHOT_REQUIRED")
)

// Manifest is the exact six-key canonical project identity contract.
// Returning it by value grants no mutation capability.
type Manifest struct {
	SchemaVersion int `json:"schemaVersion"`
	Kind string `json:"kind"`
	ProjectID string `json:"projectId"`
	CreatedAt string `json:"createdAt"`
	StorageFormat string `json:"storageFormat"`
	StorageVersion int `json:"storageVersion"`
}

var uuidPattern = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// strictObject rejects duplicate keys (including in nested objects), trailing
// values, invalid UTF-8 and non-object top-level values before decoding fields.
func strictObject(data []byte) (map[string]json.RawMessage, error) {
	if !utf8.Valid(data) {
		return nil, ErrManifestInvalid
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	if err := consumeJSON(d, 0); err != nil {
		return nil, err
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, ErrManifestInvalid
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil || obj == nil {
		return nil, ErrManifestInvalid
	}
	return obj, nil
}

func consumeJSON(d *json.Decoder, depth int) error {
	if depth > 32 {
		return ErrManifestInvalid
	}
	token, err := d.Token()
	if err != nil {
		return ErrManifestInvalid
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := make(map[string]bool)
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return ErrManifestInvalid
			}
			name, ok := key.(string)
			if !ok || seen[name] {
				return ErrManifestInvalid
			}
			seen[name] = true
			if err := consumeJSON(d, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for d.More() {
			if err := consumeJSON(d, depth+1); err != nil {
				return err
			}
		}
	default:
		return ErrManifestInvalid
	}
	end, err := d.Token()
	if err != nil || (delim == '{' && end != json.Delim('}')) || (delim == '[' && end != json.Delim(']')) {
		return ErrManifestInvalid
	}
	return nil
}

func numberOne(raw json.RawMessage) bool {
	var value float64
	return len(raw) != 0 && json.Unmarshal(raw, &value) == nil && value == 1
}

func stringField(obj map[string]json.RawMessage, key string) (string, bool) {
	var value string
	raw, exists := obj[key]
	// json.Unmarshal(null, &string) succeeds, so explicitly require a string.
	returnValue := exists && len(raw) > 0 && raw[0] == '"' && json.Unmarshal(raw, &value) == nil
	return value, returnValue
}

// validDate accepts common ISO Date.parse inputs: date-only, year/month,
// RFC3339 with offsets/fractions, and timezone-less ISO date-times. Unlike JS
// Date.parse this intentionally rejects locale/RFC2822 dates, calendar rollover,
// expanded years and 24:00. Zone-less input is checked in UTC; only validity is
// used, never a converted instant. This stricter boundary is intentional.
func validDate(value string) bool {
	for _, layout := range []string{
		time.RFC3339Nano,
		"2006-01-02T15:04:05Z0700", "2006-01-02T15:04:05",
		"2006-01-02T15:04Z07:00", "2006-01-02T15:04",
		"2006-01-02 15:04:05Z07:00", "2006-01-02 15:04:05",
		"2006-01-02", "2006-01", "2006",
	} {
		if _, err := time.Parse(layout, value); err == nil {
			return true
		}
	}
	return false
}

// ParseManifest enforces exact keys, identity, formats, and duplicate rejection.
func ParseManifest(data []byte) (Manifest, error) {
	var m Manifest
	if int64(len(data)) > manifestLimit {
		return m, ErrManifestInvalid
	}
	obj, err := strictObject(data)
	if err != nil || len(obj) != 6 || !numberOne(obj["schemaVersion"]) || !numberOne(obj["storageVersion"]) {
		return m, ErrManifestInvalid
	}
	kind, kindOK := stringField(obj, "kind")
	id, idOK := stringField(obj, "projectId")
	created, createdOK := stringField(obj, "createdAt")
	storage, storageOK := stringField(obj, "storageFormat")
	if !kindOK || kind != "ai-novel-project" || !idOK || !uuidPattern.MatchString(id) || !createdOK || !validDate(created) || !storageOK || storage != "ai-novel" {
		return m, ErrManifestInvalid
	}
	return Manifest{SchemaVersion: 1, Kind: kind, ProjectID: id, CreatedAt: created, StorageFormat: storage, StorageVersion: 1}, nil
}
