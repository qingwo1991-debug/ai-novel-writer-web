package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestOriginalProjectCoreRepositoryContracts(t *testing.T) {
	path := os.Getenv("GO_PROJECTCORE_CONTRACT")
	if path == "" {
		if os.Getenv("GITHUB_ACTIONS") == "true" {
			t.Fatal("original project-core oracle mandatory")
		}
		t.Skip("GitHub-only original repository oracle")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var oracle struct {
		FormatVersion int `json:"formatVersion"`
		Cases         []struct {
			Name   string         `json:"name"`
			File   string         `json:"file"`
			Action string         `json:"action"`
			Input  map[string]any `json:"input"`
			Result any            `json:"result"`
			Error  string         `json:"error"`
			After  map[string]any `json:"after"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &oracle); err != nil {
		t.Fatal(err)
	}
	if oracle.FormatVersion != 1 || len(oracle.Cases) < 25 {
		t.Fatal("incomplete original oracle")
	}
	normalize := func(value any) any {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		var decoded any
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			t.Fatal(err)
		}
		return decoded
	}
	for _, fixture := range oracle.Cases {
		t.Run(fixture.Name, func(t *testing.T) {
			source, err := os.ReadFile(filepath.Join(filepath.Dir(path), fixture.File))
			if err != nil {
				t.Fatal(err)
			}
			copy := filepath.Join(t.TempDir(), "copy.db")
			if err := os.WriteFile(copy, source, 0600); err != nil {
				t.Fatal(err)
			}
			uri := url.URL{Scheme: "file", Path: copy}
			uri.RawQuery = "mode=rw&_busy_timeout=5000"
			db, err := sql.Open("sqlite3", uri.String())
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			repository := NewProjectCore(db)
			var result any
			var opErr error
			switch fixture.Action {
			case "get":
				result, opErr = repository.Get(context.Background())
			case "update":
				opErr = repository.Update(context.Background(), fixture.Input)
			case "commitSynopsis":
				result, opErr = repository.CommitSynopsis(context.Background(), fixture.Input)
			default:
				t.Fatal("unknown oracle action")
			}
			code := ""
			if opErr != nil {
				code = opErr.Error()
			}
			if code != fixture.Error {
				t.Fatal("error mismatch", code, fixture.Error)
			}
			if !reflect.DeepEqual(normalize(result), normalize(fixture.Result)) {
				t.Fatalf("result Go=%#v TS=%#v", result, fixture.Result)
			}
			rows, err := db.Query("SELECT * FROM project_core WHERE id='main'")
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			columns, err := rows.Columns()
			if err != nil {
				t.Fatal(err)
			}
			if !rows.Next() {
				t.Fatal("missing authority row")
			}
			values := make([]any, len(columns))
			ptrs := make([]any, len(columns))
			for i := range values {
				ptrs[i] = &values[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			after := map[string]any{}
			for i, key := range columns {
				if key != "updated_at" {
					after[key] = values[i]
				}
			}
			if !reflect.DeepEqual(normalize(after), normalize(fixture.After)) {
				t.Fatalf("row changed differently\nGo=%#v\nTS=%#v", normalize(after), normalize(fixture.After))
			}
		})
	}
}
