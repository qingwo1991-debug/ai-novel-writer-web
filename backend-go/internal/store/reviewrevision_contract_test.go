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

// Timestamp clocks are intentionally excluded, never text/status/source fields.
// This also maps SQLite BLOB values to the original Node Buffer JSON shape.
func reviewRevisionContractValue(value any) any {
	switch value := value.(type) {
	case []byte:
		data := make([]int, len(value))
		for i, b := range value {
			data[i] = int(b)
		}
		return map[string]any{"type": "Buffer", "data": data}
	case map[string]any:
		if value == nil {
			return nil
		}
		out := map[string]any{}
		for key, item := range value {
			if key != "created_at" && key != "updated_at" && key != "createdAt" && key != "updatedAt" {
				out[key] = reviewRevisionContractValue(item)
			}
		}
		return out
	case []map[string]any:
		out := make([]any, len(value))
		for i, item := range value {
			out[i] = reviewRevisionContractValue(item)
		}
		return out
	case []any:
		out := make([]any, len(value))
		for i, item := range value {
			out[i] = reviewRevisionContractValue(item)
		}
		return out
	default:
		return value
	}
}
func TestOriginalReviewRevisionRepositoriesContracts(t *testing.T) {
	path := os.Getenv("GO_REVIEWREVISION_CONTRACT")
	if path == "" {
		if os.Getenv("GITHUB_ACTIONS") == "true" {
			t.Fatal("original review/revision oracle mandatory")
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
			ID       int64          `json:"id"`
			Pending  bool           `json:"pending"`
			Result   any            `json:"result"`
			Error    string         `json:"error"`
			After    map[string]any `json:"after"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &oracle); err != nil {
		t.Fatal(err)
	}
	if oracle.FormatVersion != 1 || len(oracle.Cases) < 50 {
		t.Fatal("incomplete original review/revision oracle", len(oracle.Cases))
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
			repository := NewReviewRevisions(db)
			ctx := context.Background()
			var result any
			var opErr error
			switch fixture.Method {
			case "CreateReview":
				result, opErr = repository.CreateReview(ctx, fixture.Input)
			case "ListReviews":
				result, opErr = repository.ListReviews(ctx, fixture.ID)
			case "LatestReview":
				result, opErr = repository.LatestReview(ctx, fixture.ID)
			case "FullReview":
				result, opErr = repository.FullReview(ctx, fixture.ID)
			case "NextReviewIndex":
				result, opErr = repository.NextReviewIndex(ctx, fixture.ID)
			case "CreateRevision":
				result, opErr = repository.CreateRevision(ctx, fixture.Input)
			case "ReplacePending":
				result, opErr = repository.ReplacePending(ctx, fixture.Input)
			case "ListRevisions":
				result, opErr = repository.ListRevisions(ctx, fixture.ID, fixture.Pending)
			case "FullRevision":
				result, opErr = repository.FullRevision(ctx, fixture.ID)
			case "NextRevisionIndex":
				result, opErr = repository.NextRevisionIndex(ctx, fixture.ID)
			case "MergeRevision":
				result, opErr = repository.MergeRevision(ctx, fixture.Input)
			case "MarkDiscarded":
				opErr = repository.MarkDiscarded(ctx, fixture.ID)
			case "MarkMerged":
				opErr = repository.MarkMerged(ctx, int64(fixture.Input["id"].(float64)), int64(fixture.Input["mergedToDraftId"].(float64)))
			default:
				t.Fatal("unknown oracle method", fixture.Method)
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
				allowed := map[string]bool{"contents": true, "drafts": true, "reviews": true, "revisions": true, "review_cycles": true, "review_findings": true, "review_cycle_merges": true}
				if !allowed[table] {
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
				t.Fatal("original oracle copy modified", err)
			}
		})
	}
}
