package webdav

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// This fixture is produced ONLY by the untouched original-source Node oracle in
// disposable CI. It is optional locally; absence is reported as a skip, never as
// differential evidence. No credentials/configuration/author files are read.
func TestOriginalSourceContract(t *testing.T) {
	file := os.Getenv("GO_WEBDAV_CONTRACT")
	if file == "" {
		t.Skip("GO_WEBDAV_CONTRACT not set: original-source differential oracle not executed")
	}
	base := os.Getenv("RUNNER_TEMP")
	if os.Getenv("GITHUB_ACTIONS") != "true" || base == "" || !filepath.IsAbs(base) || !filepath.IsAbs(file) || filepath.Base(file) != "contract.json" || filepath.Base(filepath.Dir(file)) != "novel-go-webdav" {
		t.Fatal("oracle requires disposable GitHub runner path")
	}
	realBase, err := filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatal(err)
	}
	realFile, err := filepath.EvalSymlinks(file)
	if err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(realBase, realFile)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		t.Fatal("oracle path escapes runner temporary directory")
	}
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var contract struct {
		FormatVersion int `json:"formatVersion"`
		Endpoints     []struct {
			Input string `json:"input"`
			Value string `json:"value"`
			Error string `json:"error"`
		} `json:"endpoints"`
		Locations []struct {
			Endpoint   string `json:"endpoint"`
			Book       string `json:"book"`
			Generation string `json:"generation"`
			Value      struct {
				Collections []string `json:"collections"`
				Generations string   `json:"generations"`
				Archive     string   `json:"archive"`
				Manifest    string   `json:"manifest"`
				Completion  string   `json:"completion"`
			} `json:"value"`
		} `json:"locations"`
		Manifests []struct {
			Name  string          `json:"name"`
			Input string          `json:"input"`
			Value json.RawMessage `json:"value"`
			Error string          `json:"error"`
		} `json:"manifests"`
		Completions []struct {
			Name  string `json:"name"`
			Input string `json:"input"`
			Error string `json:"error"`
		} `json:"completions"`
		Hrefs []struct {
			Name  string   `json:"name"`
			Root  string   `json:"root"`
			Input string   `json:"input"`
			Value []string `json:"value"`
			Error string   `json:"error"`
		} `json:"hrefs"`
		Catalogs []struct {
			Name  string       `json:"name"`
			Input []Manifest   `json:"input"`
			Value []Generation `json:"value"`
		} `json:"catalogs"`
	}
	if err := json.Unmarshal(b, &contract); err != nil {
		t.Fatal(err)
	}
	if contract.FormatVersion != 1 || len(contract.Endpoints) < 10 || len(contract.Manifests) < 10 || len(contract.Completions) < 5 || len(contract.Hrefs) < 10 || len(contract.Catalogs) == 0 {
		t.Fatal("incomplete original-source oracle")
	}
	for _, c := range contract.Endpoints {
		t.Run("endpoint/"+c.Input, func(t *testing.T) {
			actual, err := NormalizeEndpoint(c.Input)
			if c.Error != "" {
				if err != InputInvalid {
					t.Fatalf("expected input rejection, got %q %v", actual, err)
				}
				return
			}
			if err != nil || actual != c.Value {
				t.Fatalf("got %q %v want %q", actual, err, c.Value)
			}
		})
	}
	for _, c := range contract.Locations {
		t.Run("paths/"+c.Book, func(t *testing.T) {
			actual, err := GenerationLocations(c.Endpoint, c.Book, c.Generation)
			if err != nil {
				t.Fatal(err)
			}
			collections := []string{}
			for _, u := range actual.Collections {
				collections = append(collections, u.String())
			}
			if !reflect.DeepEqual(collections, c.Value.Collections) || actual.Generations.String() != c.Value.Generations || actual.Archive.String() != c.Value.Archive || actual.Manifest.String() != c.Value.Manifest || actual.Completion.String() != c.Value.Completion {
				t.Fatalf("path differential %+v", actual)
			}
		})
	}
	for _, c := range contract.Manifests {
		t.Run("manifest/"+c.Name, func(t *testing.T) {
			actual, err := ParseManifest([]byte(c.Input))
			if c.Error != "" {
				if err != RemoteInvalid {
					t.Fatalf("expected %s got %v", c.Error, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var want Manifest
			if err := json.Unmarshal(c.Value, &want); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(actual, want) {
				t.Fatalf("manifest differential %+v %+v", actual, want)
			}
		})
	}
	for _, c := range contract.Completions {
		t.Run("completion/"+c.Name, func(t *testing.T) {
			_, err := ParseCompletion([]byte(c.Input))
			if c.Error != "" {
				if err != RemoteInvalid {
					t.Fatalf("expected %s got %v", c.Error, err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, c := range contract.Hrefs {
		t.Run("href/"+c.Name, func(t *testing.T) {
			root, err := url.Parse(c.Root)
			if err != nil {
				t.Fatal(err)
			}
			actual, err := GenerationHrefs([]byte(c.Input), root, 1<<20)
			if c.Error != "" {
				if err != DAVResponseInvalid {
					t.Fatalf("expected %s got %v", c.Error, err)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(actual, c.Value) {
				t.Fatalf("href differential %v %v want %v", actual, err, c.Value)
			}
		})
	}
	for _, c := range contract.Catalogs {
		t.Run("catalog/"+c.Name, func(t *testing.T) {
			if actual := Catalog(c.Input); !reflect.DeepEqual(actual, c.Value) {
				t.Fatalf("catalog differential %+v want %+v", actual, c.Value)
			}
		})
	}
}
