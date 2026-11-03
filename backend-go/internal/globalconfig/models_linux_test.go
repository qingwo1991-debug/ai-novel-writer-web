//go:build linux

package globalconfig

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func modelService(t *testing.T) (*Service, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { service.Close() })
	return service, root
}

func syntheticModel(key string) ModelRecord {
	return ModelRecord{
		"id": "saved", "name": "Synthetic", "provider": "openai",
		"protocol": "openai", "baseUrl": "https://example.invalid/v1/",
		"apiKey": key, "modelName": "synthetic", "temperature": 0.5,
		"maxTokens": 1024, "purposes": []string{"generation"},
		"futureField": map[string]any{"retained": "中文🙂"},
	}
}

func TestModelsPreservePrivateCredentialOnlyForSameEndpoint(t *testing.T) {
	service, root := modelService(t)
	if err := service.SaveModel(syntheticModel("SYNTHETIC-SECRET")); err != nil {
		t.Fatal(err)
	}
	input := syntheticModel("")
	input["name"] = "Renamed"
	input["baseUrl"] = "https://example.invalid/v1"
	input["hasCredential"] = false
	input["savedModelId"] = "other"
	if err := service.SaveModel(input); err != nil {
		t.Fatal(err)
	}
	models, err := service.ListPrivateModels()
	if err != nil || len(models) != 1 {
		t.Fatal(err, models)
	}
	if models[0]["apiKey"] != "SYNTHETIC-SECRET" || models[0]["name"] != "Renamed" {
		t.Fatal("credential or update lost")
	}
	if _, exists := models[0]["hasCredential"]; exists {
		t.Fatal("browser hint persisted")
	}
	if _, exists := models[0]["savedModelId"]; exists {
		t.Fatal("browser override persisted")
	}
	if models[0]["futureField"].(map[string]any)["retained"] != "中文🙂" {
		t.Fatal("unknown metadata lost")
	}
	stat, err := os.Stat(filepath.Join(root, modelFile))
	if err != nil || stat.Mode().Perm() != 0600 {
		t.Fatal("model config not private", err)
	}

	changed := syntheticModel("")
	changed["baseUrl"] = "https://other.invalid/v1"
	before, err := os.ReadFile(filepath.Join(root, modelFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := service.SaveModel(changed); !errors.Is(err, ErrCredentialEndpointChanged) {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filepath.Join(root, modelFile))
	if err != nil || string(after) != string(before) {
		t.Fatal("endpoint change overwrote private credential", err)
	}
	changed["apiKey"] = "REPLACEMENT-SECRET"
	if err := service.SaveModel(changed); err != nil {
		t.Fatal(err)
	}
	models, err = service.ListPrivateModels()
	if err != nil || models[0]["apiKey"] != "REPLACEMENT-SECRET" {
		t.Fatal("explicit key replacement failed", err)
	}
}

func TestModelsNeverOverwriteCorruptOrUnsafeExistingFile(t *testing.T) {
	service, root := modelService(t)
	file := filepath.Join(root, modelFile)
	if err := os.WriteFile(file, []byte(`{"not":"an array"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := service.SaveModel(syntheticModel("new")); !errors.Is(err, ErrModelCorrupt) {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(file)
	if err != nil || string(raw) != `{"not":"an array"}` {
		t.Fatal("corrupt file replaced", err)
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.json")
	if err := os.WriteFile(outside, []byte(`[]`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, file); err != nil {
		t.Fatal(err)
	}
	if err := service.SaveModel(syntheticModel("new")); !errors.Is(err, ErrUnsafeFile) {
		t.Fatal(err)
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(outside, file); err != nil {
		t.Fatal(err)
	}
	if err := service.SaveModel(syntheticModel("new")); !errors.Is(err, ErrUnsafeFile) {
		t.Fatal(err)
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(`[]`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := service.SaveModel(syntheticModel("new")); !errors.Is(err, ErrUnsafeFile) {
		t.Fatal(err)
	}
}

func TestModelsRejectInvalidInputWithoutCreatingFile(t *testing.T) {
	service, root := modelService(t)
	invalid := syntheticModel("key")
	delete(invalid, "id")
	if err := service.SaveModel(invalid); !errors.Is(err, ErrModelInvalid) {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, modelFile)); !os.IsNotExist(err) {
		t.Fatal("invalid input created models file", err)
	}
	tooLarge := syntheticModel(strings.Repeat("a", MaxConfigBytes))
	if err := service.SaveModel(tooLarge); !errors.Is(err, ErrModelInvalid) {
		t.Fatal(err)
	}
	models, err := service.ListPrivateModels()
	if err != nil || len(models) != 0 {
		t.Fatal("missing model file did not return empty private list", err)
	}
}
