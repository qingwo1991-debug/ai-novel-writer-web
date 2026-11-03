//go:build linux

package globalconfig

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestDeleteUnreferencedModelOnlyChangesModelsFile(t *testing.T) {
	service, root := modelService(t)
	if err := service.SaveModel(syntheticModel("SYNTHETIC-SECRET")); err != nil {
		t.Fatal(err)
	}
	other := syntheticModel("OTHER-SECRET")
	other["id"] = "retained"
	if err := service.SaveModel(other); err != nil {
		t.Fatal(err)
	}
	config := []byte(`{"theme":"dark","defaultModelId":"retained","stageModelIds":{"review":"retained"},"unknown":{"text":"keep"}}`)
	configPath := filepath.Join(root, "config.json")
	if err := os.WriteFile(configPath, config, 0600); err != nil {
		t.Fatal(err)
	}
	if err := service.DeleteUnreferencedModel("saved"); err != nil {
		t.Fatal(err)
	}
	models, err := service.ListPrivateModels()
	if err != nil || len(models) != 1 || models[0]["id"] != "retained" || models[0]["apiKey"] != "OTHER-SECRET" {
		t.Fatal("unreferenced deletion changed retained model", err, models)
	}
	after, err := os.ReadFile(configPath)
	if err != nil || string(after) != string(config) {
		t.Fatal("config changed during single-file deletion", err)
	}
	info, err := os.Stat(filepath.Join(root, modelFile))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("models file lost private mode", err)
	}
}

func TestDeleteUnreferencedModelRefusesEveryKnownAndUnknownReference(t *testing.T) {
	for _, config := range []string{
		`{"defaultModelId":"saved"}`,
		`{"defaultEmbeddingModelId":"saved"}`,
		`{"stageModelIds":{"review":"saved"}}`,
		`{"futureRouting":{"alternates":["saved"]}}`,
	} {
		t.Run(config, func(t *testing.T) {
			service, root := modelService(t)
			if err := service.SaveModel(syntheticModel("SYNTHETIC-SECRET")); err != nil {
				t.Fatal(err)
			}
			configPath := filepath.Join(root, "config.json")
			if err := os.WriteFile(configPath, []byte(config), 0600); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(filepath.Join(root, modelFile))
			if err != nil {
				t.Fatal(err)
			}
			if err := service.DeleteUnreferencedModel("saved"); !errors.Is(err, ErrModelReferenced) {
				t.Fatal(err)
			}
			after, err := os.ReadFile(filepath.Join(root, modelFile))
			if err != nil || string(after) != string(before) {
				t.Fatal("referenced model was changed", err)
			}
		})
	}
}

func TestDeleteUnreferencedModelFailsClosedForDamagedConfig(t *testing.T) {
	for _, value := range []string{`{"defaultModelId":`, `null`, `[]`} {
		t.Run(value, func(t *testing.T) {
			service, root := modelService(t)
			if err := service.SaveModel(syntheticModel("SYNTHETIC-SECRET")); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "config.json"), []byte(value), 0600); err != nil {
				t.Fatal(err)
			}
			if err := service.DeleteUnreferencedModel("saved"); !errors.Is(err, ErrModelDeleteConfigCorrupt) {
				t.Fatal(err)
			}
			models, err := service.ListPrivateModels()
			if err != nil || len(models) != 1 || models[0]["apiKey"] != "SYNTHETIC-SECRET" {
				t.Fatal("damaged config caused model loss", err)
			}
		})
	}
}

func TestDeleteUnreferencedModelRejectsUnknownOrInvalidID(t *testing.T) {
	service, root := modelService(t)
	if err := service.DeleteUnreferencedModel(""); !errors.Is(err, ErrModelInvalid) {
		t.Fatal(err)
	}
	if err := service.DeleteUnreferencedModel("missing"); !errors.Is(err, ErrModelNotFound) {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, modelFile)); !os.IsNotExist(err) {
		t.Fatal("unknown deletion created a model file", err)
	}
}
