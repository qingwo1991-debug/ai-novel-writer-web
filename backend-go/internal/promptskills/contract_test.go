//go:build linux

package promptskills

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func normalized(t *testing.T, value any) any {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err = json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}
func TestOriginalTypeScriptPromptSkillsContract(t *testing.T) {
	path := os.Getenv("GO_PROMPT_SKILLS_CONTRACT")
	if path == "" {
		if os.Getenv("GITHUB_ACTIONS") == "true" {
			t.Fatal("original TypeScript prompt-skills oracle is mandatory")
		}
		t.Skip("GitHub-only original TypeScript differential")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var oracle struct {
		FormatVersion int `json:"formatVersion"`
		Cases         []struct {
			Name     string            `json:"name"`
			Kind     string            `json:"kind"`
			Raw      string            `json:"raw"`
			Files    map[string]string `json:"files"`
			Fallback string            `json:"fallback"`
			Bundle   Bundle            `json:"bundle"`
			Value    string            `json:"value"`
			Failed   bool              `json:"failed"`
			Result   any               `json:"result"`
		} `json:"cases"`
		PromptActions []struct {
			Action string         `json:"action"`
			Args   []any          `json:"args"`
			File   string         `json:"file"`
			Value  map[string]any `json:"value"`
			Result any            `json:"result"`
		} `json:"promptActions"`
		BundleReadCases []struct {
			Name     string            `json:"name"`
			Manifest *string           `json:"manifest"`
			Files    map[string]string `json:"files"`
			Result   any               `json:"result"`
		} `json:"bundleReadCases"`
		PackageCases []struct {
			Name   string            `json:"name"`
			Folder string            `json:"folder"`
			Raw    string            `json:"raw"`
			Files  map[string]string `json:"files"`
			Result any               `json:"result"`
		} `json:"packageCases"`
	}
	if err = json.Unmarshal(data, &oracle); err != nil {
		t.Fatal(err)
	}
	if oracle.FormatVersion != 1 || len(oracle.Cases) < 30 || len(oracle.PromptActions) < 10 || len(oracle.BundleReadCases) < 7 || len(oracle.PackageCases) < 5 {
		t.Fatal("incomplete original source oracle")
	}
	for _, c := range oracle.Cases {
		t.Run(c.Name, func(t *testing.T) {
			var actual any
			switch c.Kind {
			case "markdown":
				actual = InspectMarkdown(c.Raw)
			case "local":
				actual = InspectLocal(c.Raw, func(p string) (string, bool) { v, ok := c.Files[p]; return v, ok }, c.Fallback)
			case "installed":
				actual = InspectInstalled(c.Raw, c.Bundle, "public-fallback")
			case "url":
				location, err := ParseGitHubURL(c.Value)
				if (err != nil) != c.Failed {
					t.Fatalf("URL failure mismatch: %v", err)
				}
				if c.Failed {
					return
				}
				actual = location
			default:
				t.Fatal("unexpected oracle kind")
			}
			if !reflect.DeepEqual(normalized(t, actual), c.Result) {
				t.Fatalf("Go=%#v\nTS=%#v", normalized(t, actual), c.Result)
			}
		})
	}
	root := t.TempDir()
	service, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	for index, action := range oracle.PromptActions {
		var actual any
		switch action.Action {
		case "seed":
			if err = os.MkdirAll(filepath.Join(root, "prompts"), 0700); err != nil {
				t.Fatal(err)
			}
			data, _ := json.Marshal(action.Value)
			if err = os.WriteFile(filepath.Join(root, "prompts", action.File), data, 0600); err != nil {
				t.Fatal(err)
			}
			continue
		case "load":
			actual, err = service.LoadPrompts()
		case "save":
			err = service.SavePrompt(action.Args[0].(map[string]any))
			actual = map[string]any{"success": err == nil}
		case "delete":
			err = service.DeletePrompt(action.Args[0].(string), action.Args[1].(string))
			actual = map[string]any{"success": err == nil}
		default:
			t.Fatal("unknown prompt action")
		}
		if err != nil {
			t.Fatalf("step %d: %v", index, err)
		}
		if !reflect.DeepEqual(normalized(t, actual), action.Result) {
			t.Fatalf("prompt step %d Go=%#v TS=%#v", index, normalized(t, actual), action.Result)
		}
	}
	for _, fixture := range oracle.PackageCases {
		t.Run(fixture.Name, func(t *testing.T) {
			root := t.TempDir()
			folder := filepath.Join(root, fixture.Folder)
			if err := os.Mkdir(folder, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(folder, "SKILL.md"), []byte(fixture.Raw), 0600); err != nil {
				t.Fatal(err)
			}
			for p, value := range fixture.Files {
				target := filepath.Join(folder, p)
				if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(target, []byte(value), 0600); err != nil {
					t.Fatal(err)
				}
			}
			stage, err := NewPackageRoot(root)
			if err != nil {
				t.Fatal(err)
			}
			defer stage.Close()
			actual, err := stage.Load(fixture.Folder, "folder")
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(normalized(t, actual), fixture.Result) {
				t.Fatalf("Go=%#v TS=%#v", normalized(t, actual), fixture.Result)
			}
		})
	}
	for _, fixture := range oracle.BundleReadCases {
		t.Run("bundle-"+fixture.Name, func(t *testing.T) {
			root := t.TempDir()
			if fixture.Manifest != nil {
				if err = os.WriteFile(filepath.Join(root, BundleManifest), []byte(*fixture.Manifest), 0600); err != nil {
					t.Fatal(err)
				}
			}
			for p, value := range fixture.Files {
				target := filepath.Join(root, p)
				if err = os.MkdirAll(filepath.Dir(target), 0700); err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(target, []byte(value), 0600); err != nil {
					t.Fatal(err)
				}
			}
			dir, err := openOwnedRoot(root)
			if err != nil {
				t.Fatal(err)
			}
			defer dir.close()
			actual := readBundle(dir)
			if !reflect.DeepEqual(normalized(t, actual), fixture.Result) {
				t.Fatalf("Go=%#v TS=%#v", normalized(t, actual), fixture.Result)
			}
		})
	}
}
