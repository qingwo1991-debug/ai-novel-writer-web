//go:build lance_native && cgo

package lancenative

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func private(t *testing.T) string {
	t.Helper()
	path := t.TempDir()
	if err := os.Chmod(path, 0700); err != nil {
		t.Fatal(err)
	}
	return path
}
func TestNativeExportMissingNeverCreatesDatabase(t *testing.T) {
	root := private(t)
	missing := filepath.Join(root, "missing")
	if _, err := Export(context.Background(), missing, private(t)); err == nil {
		t.Fatal("absent database accepted")
	}
	if _, err := os.Lstat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("absent database was created", err)
	}
}
func TestNativeInputsAndCancellation(t *testing.T) {
	for _, path := range []string{"relative", "/tmp/../tmp", string([]byte{'/', 0})} {
		if _, err := Export(context.Background(), path, private(t)); !errors.Is(err, ErrInput) {
			t.Fatal(err)
		}
	}
	root := private(t)
	if _, err := Export(nil, root, private(t)); !errors.Is(err, ErrInput) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Export(ctx, root, private(t)); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := Import(ctx, root, private(t), Manifest{}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
func TestNativeRejectsMalformedStreamsWithoutOverwritingExistingStage(t *testing.T) {
	input := private(t)
	if err := os.WriteFile(filepath.Join(input, "chunks.arrows"), []byte("not an Arrow stream"), 0600); err != nil {
		t.Fatal(err)
	}
	stage := private(t)
	manifest := Manifest{Tables: []Table{{Name: "chunks", Version: 1, Rows: 1, File: "chunks.arrows"}}}
	if _, err := Import(context.Background(), stage, input, manifest); err == nil {
		t.Fatal("malformed Arrow admitted")
	}
	sentinel := filepath.Join(stage, "do-not-overwrite")
	if err := os.WriteFile(sentinel, []byte("existing"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Import(context.Background(), stage, input, manifest); err == nil {
		t.Fatal("nonempty stage admitted")
	}
	data, err := os.ReadFile(sentinel)
	if err != nil || string(data) != "existing" {
		t.Fatal("existing content overwritten", err)
	}
}
func TestNativeRefusesEscapingManifestAndSymlinkRoots(t *testing.T) {
	input := private(t)
	for _, item := range []Table{{Name: "../chunks", File: "../outside.arrows"}, {Name: "chunks", File: "../outside.arrows"}, {Name: "arbitrary", File: "arbitrary.arrows"}} {
		if _, err := Import(context.Background(), private(t), input, Manifest{Tables: []Table{item}}); err == nil {
			t.Fatal("manifest escape accepted", item)
		}
	}
	root := private(t)
	actual := private(t)
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(actual, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := Export(context.Background(), alias, private(t)); err == nil {
		t.Fatal("symlink db accepted")
	}
	if err := EnsureExistingCopy(alias); !errors.Is(err, ErrInput) {
		t.Fatal(err)
	}
}
func TestReadDomainBoundedPrivateFixedFile(t *testing.T) {
	ctx := context.Background()
	out := private(t)
	path := filepath.Join(out, "domain.json")
	want := []byte(`{"tables":[]}`)
	if err := os.WriteFile(path, want, 0600); err != nil {
		t.Fatal(err)
	}
	raw, err := ReadDomain(ctx, out)
	if err != nil || string(raw) != string(want) {
		t.Fatal("fixed sidecar read failed", err)
	}
	if _, err := ReadDomain(nil, out); !errors.Is(err, ErrInput) {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := ReadDomain(canceled, out); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadDomain(ctx, out); !errors.Is(err, ErrNative) || err.Error() != "LANCE_NATIVE_OPERATION_FAILED (status 2)" {
		t.Fatal("public file accepted or unsanitized error", err)
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(private(t), "alias")
	if err := os.Link(path, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadDomain(ctx, out); !errors.Is(err, ErrNative) {
		t.Fatal("hardlinked file accepted", err)
	}
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(alias, path); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadDomain(ctx, out); !errors.Is(err, ErrNative) {
		t.Fatal("symlink file accepted", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	// Sparse size check needs no large allocation or native runtime work.
	if err := file.Truncate(maxDomain + 1); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadDomain(ctx, out); !errors.Is(err, ErrNative) || err.Error() != "LANCE_NATIVE_OPERATION_FAILED (status 3)" {
		t.Fatal("oversized sidecar accepted or unsanitized error", err)
	}
}

func TestNativeExportDomainExistingFileNeverClobbered(t *testing.T) {
	out := private(t)
	path := filepath.Join(out, "domain.json")
	if err := os.WriteFile(path, []byte("existing"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Export(context.Background(), private(t), out); !errors.Is(err, ErrNative) {
		t.Fatal("nonempty sidecar output accepted", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != "existing" {
		t.Fatal("existing sidecar overwritten", err)
	}
}

func TestNativeExportDomainFixture(t *testing.T) {
	root := os.Getenv("GO_LANCE_CONTRACTROOT")
	if root == "" {
		t.Skip("optional GitHub synthetic fixture is not supplied")
	}
	out := private(t)
	manifest, err := Export(context.Background(), filepath.Join(root, "copy", "lancedb"), out)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := ReadDomain(context.Background(), out)
	if err != nil {
		t.Fatal(err)
	}
	type domainField struct {
		Name     string        `json:"name"`
		Type     string        `json:"type"`
		Nullable bool          `json:"nullable"`
		ListSize int           `json:"listSize"`
		Metadata bool          `json:"metadata"`
		Children []domainField `json:"children"`
	}
	var domain struct {
		Tables []struct {
			Name     string            `json:"name"`
			Version  uint64            `json:"version"`
			Metadata bool              `json:"metadata"`
			Schema   []domainField     `json:"schema"`
			Records  []map[string]any `json:"records"`
		} `json:"tables"`
	}
	if err := json.Unmarshal(raw, &domain); err != nil {
		t.Fatal("invalid domain JSON")
	}
	if len(domain.Tables) != len(manifest.Tables) {
		t.Fatal("domain membership differs from IPC manifest")
	}
	empty, text, integer, number, vector := false, false, false, false, false
	for index, table := range domain.Tables {
		entry := manifest.Tables[index]
		if table.Name != entry.Name || table.Version != entry.Version || uint64(len(table.Records)) != entry.Rows || table.Metadata || len(table.Schema) == 0 {
			t.Fatal("domain table identity, version, rows or schema mismatch")
		}
		if entry.Rows == 0 {
			empty = table.Records != nil
		}
		for _, record := range table.Records {
			for _, field := range table.Schema {
				value, exists := record[field.Name]
				if !exists {
					t.Fatal("record omitted a schema field")
				}
				if value == nil {
					continue
				}
				switch field.Type {
				case "utf8":
					if _, ok := value.(string); !ok {
						t.Fatal("string field not a JSON string")
					}
					text = true
				case "int32":
					if n, ok := value.(float64); !ok || n != float64(int32(n)) {
						t.Fatal("int32 field not an exact JSON integer")
					}
					integer = true
				case "float32", "float64":
					if _, ok := value.(float64); !ok {
						t.Fatal("float field not a JSON number")
					}
					number = true
				case "fixed_size_list":
					items, ok := value.([]any)
					if !ok || len(items) != field.ListSize || len(field.Children) != 1 || field.Children[0].Type != "float32" {
						t.Fatal("typed vector shape not preserved")
					}
					for _, item := range items {
						if _, ok := item.(float64); !ok {
							t.Fatal("vector scalar not a JSON number")
						}
					}
					vector = true
				default:
					t.Fatal("unsupported sidecar type")
				}
			}
		}
	}
	if !empty || !text || !integer || !number || !vector {
		t.Fatal("fixture did not prove typed records and empty schema-bearing tables")
	}
}

func TestNativeRepeatFailureFreeAndZeroReports(t *testing.T) {
	root := private(t)
	for range 16 {
		result, err := Export(context.Background(), filepath.Join(root, "missing"), private(t))
		if err == nil || !reflect.DeepEqual(result, Manifest{}) {
			t.Fatal(result, err)
		}
	}
}
