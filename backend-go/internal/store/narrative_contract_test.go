package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestOriginalNarrativeAndExemptionRepositoriesContracts(t *testing.T) {
	path := os.Getenv("GO_NARRATIVE_CONTRACT")
	if path == "" {
		if os.Getenv("GITHUB_ACTIONS") == "true" {
			t.Fatal("original narrative/exemption oracle mandatory")
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
			Name     string         `json:"name"`
			Database string         `json:"database"`
			Method   string         `json:"method"`
			Input    map[string]any `json:"input"`
			Args     []any          `json:"args"`
			Result   any            `json:"result"`
			Error    string         `json:"error"`
			After    map[string]any `json:"after"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &oracle); err != nil {
		t.Fatal(err)
	}
	if oracle.FormatVersion != 1 || len(oracle.Cases) < 70 {
		t.Fatal("incomplete original narrative oracle", len(oracle.Cases))
	}
	normalize := func(value any) any {
		encoded, err := json.Marshal(reviewRevisionContractValue(value))
		if err != nil {
			t.Fatal(err)
		}
		var parsed any
		if err := json.Unmarshal(encoded, &parsed); err != nil {
			t.Fatal(err)
		}
		return parsed
	}
	for _, fixture := range oracle.Cases {
		t.Run(fixture.Name, func(t *testing.T) {
			original, err := os.ReadFile(fixture.Database)
			if err != nil {
				t.Fatal(err)
			}
			copy := filepath.Join(t.TempDir(), "copy.db")
			if err := os.WriteFile(copy, original, 0600); err != nil {
				t.Fatal(err)
			}
			dsn, err := ExistingDSN(copy)
			if err != nil {
				t.Fatal(err)
			}
			db, err := sql.Open("sqlite3", dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			repository := NewNarrative(db)
			ctx := context.Background()
			var result any
			var opErr error
			switch fixture.Method {
			case "CreatePlan":
				result, opErr = repository.CreatePlan(ctx, fixture.Input)
			case "UpdatePlan":
				result, opErr = repository.UpdatePlan(ctx, int64(fixture.Args[0].(float64)), fixture.Input)
			case "DeletePlan":
				opErr = repository.DeletePlan(ctx, int64(fixture.Args[0].(float64)))
			case "GetPlan":
				result, opErr = repository.GetPlan(ctx, int64(fixture.Args[0].(float64)))
			case "ConfirmEvent":
				result, opErr = repository.ConfirmEvent(ctx, fixture.Input)
			case "List":
				result, opErr = repository.List(ctx)
			case "Relevant":
				result, opErr = repository.Relevant(ctx, fixture.Input)
			case "Exemptions":
				result, opErr = repository.Exemptions(ctx)
			case "SaveExemption":
				opErr = repository.SaveExemption(ctx, fixture.Args[0].(string), fixture.Args[1].(string))
			case "RevokeExemption":
				opErr = repository.RevokeExemption(ctx, fixture.Args[0].(string))
			default:
				t.Fatal("unknown original method", fixture.Method)
			}
			code := ""
			if opErr != nil {
				code = opErr.Error()
			}
			if code != fixture.Error {
				t.Fatal("error Go/TS", code, fixture.Error)
			}
			if !reflect.DeepEqual(normalize(result), normalize(fixture.Result)) {
				t.Fatalf("result Go=%#v TS=%#v", normalize(result), normalize(fixture.Result))
			}
			after := map[string]any{}
			for table := range fixture.After {
				if table != "narrative_thread_plans" && table != "narrative_thread_confirmations" && table != "consistency_exemptions" {
					t.Fatal("oracle unexpected table", table)
				}
				rows, err := db.QueryContext(ctx, "SELECT * FROM "+table+" ORDER BY rowid")
				if err != nil {
					t.Fatal(err)
				}
				columns, err := rows.Columns()
				if err != nil {
					rows.Close()
					t.Fatal(err)
				}
				records := []any{}
				for rows.Next() {
					values := make([]any, len(columns))
					pointers := make([]any, len(columns))
					for i := range values {
						pointers[i] = &values[i]
					}
					if err := rows.Scan(pointers...); err != nil {
						rows.Close()
						t.Fatal(err)
					}
					record := map[string]any{}
					for i, key := range columns {
						record[key] = values[i]
					}
					records = append(records, record)
				}
				err = rows.Err()
				closeErr := rows.Close()
				if err != nil || closeErr != nil {
					t.Fatal(err, closeErr)
				}
				after[table] = records
			}
			if !reflect.DeepEqual(normalize(after), normalize(fixture.After)) {
				t.Fatalf("database effects differ\nGo=%#v\nTS=%#v", normalize(after), normalize(fixture.After))
			}
			still, err := os.ReadFile(fixture.Database)
			if err != nil || !bytes.Equal(still, original) {
				t.Fatal("original fixture mutated", err)
			}
		})
	}
}
