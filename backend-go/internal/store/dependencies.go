package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"strings"
	"unicode/utf16"
)

const maxSafeInteger int64 = 9007199254740991

func parseDependencies(value sql.NullString) ([]DraftSourceDependency, bool) {
	empty := make([]DraftSourceDependency, 0)
	if !value.Valid {
		return empty, false
	}
	var raw []map[string]json.RawMessage
	if err := json.Unmarshal([]byte(value.String), &raw); err != nil || raw == nil || len(raw) > 500 {
		return empty, false
	}
	result := make([]DraftSourceDependency, 0, len(raw))
	seen := make(map[int64]bool)
	for _, item := range raw {
		if item == nil {
			return empty, false
		}
		var dep DraftSourceDependency
		var hash string
		if !safePositiveInteger(item["draftId"], &dep.DraftID) || seen[dep.DraftID] ||
			json.Unmarshal(item["contentHash"], &hash) != nil || len(hash) != 64 {
			return empty, false
		}
		for _, ch := range hash {
			if !(ch >= 'a' && ch <= 'f' || ch >= '0' && ch <= '9') {
				return empty, false
			}
		}
		dep.ContentHash = hash
		if kind, ok := item["kind"]; ok {
			if string(kind) == "null" || json.Unmarshal(kind, &dep.Kind) != nil ||
				(dep.Kind != "candidate" && dep.Kind != "finalized" && dep.Kind != "legacy-finalized") {
				return empty, false
			}
		}
		_, hasChapter := item["chapterNumber"]
		_, hasFinalization := item["finalizationId"]
		sealed := dep.Kind == "finalized" || dep.Kind == "legacy-finalized"
		if sealed {
			var chapter int64
			if !safePositiveInteger(item["chapterNumber"], &chapter) {
				return empty, false
			}
			dep.ChapterNumber = &chapter
		} else if hasChapter {
			return empty, false
		}
		if dep.Kind == "finalized" {
			if json.Unmarshal(item["finalizationId"], &dep.FinalizationID) != nil ||
				trimJS(dep.FinalizationID) == "" || len(utf16.Encode([]rune(dep.FinalizationID))) > 500 {
				return empty, false
			}
		} else if hasFinalization {
			return empty, false
		}
		seen[dep.DraftID] = true
		result = append(result, dep)
	}
	return result, true
}

func safePositiveInteger(raw json.RawMessage, result *int64) bool {
	// Unmarshal to any, not float64 directly: null must not silently become 0.
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return false
	}
	number, ok := value.(float64)
	if !ok || number < 1 || number > float64(maxSafeInteger) || number != float64(int64(number)) {
		return false
	}
	*result = int64(number)
	return true
}

type dependencyState struct {
	hash, status         string
	chapter              int64
	dependencies         []DraftSourceDependency
	valid, authoritative bool
	finalizationID       sql.NullString
	receiptHash          sql.NullString
	receiptMatches       bool
}

func loadDependencyStates(ctx context.Context, tx *sql.Tx, ids []int64) (map[int64]dependencyState, error) {
	states := make(map[int64]dependencyState)
	queued := make(map[int64]bool)
	pending := make([]int64, 0, len(ids))
	for _, id := range ids {
		if !queued[id] {
			queued[id] = true
			pending = append(pending, id)
		}
	}
	for offset := 0; offset < len(pending); {
		end := offset + 500
		if end > len(pending) {
			end = len(pending)
		}
		chunk := pending[offset:end]
		offset = end
		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}
		query := `SELECT d.id, d.chapter_number, d.status, d.source_dependencies,
 c.body, f.finalization_id, f.content_hash, f.content_snapshot,
 NOT EXISTS (SELECT 1 FROM drafts newer WHERE newer.chapter_number = d.chapter_number
 AND newer.status = 'finalized' AND (newer.version > d.version OR
 (newer.version = d.version AND newer.id > d.id)))
 FROM drafts d JOIN contents c ON c.id = d.content_id
 LEFT JOIN finalization_outbox f ON f.draft_id = d.id
 WHERE d.id IN (` + strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",") + ")"
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id int64
			var state dependencyState
			var dependencies, status sql.NullString
			var body string
			var snapshot sql.NullString
			err = rows.Scan(&id, &state.chapter, &status, &dependencies, &body,
				&state.finalizationID, &state.receiptHash, &snapshot, &state.authoritative)
			if err != nil {
				rows.Close()
				return nil, err
			}
			state.status = status.String
			digest := sha256.Sum256([]byte(body))
			state.hash = hex.EncodeToString(digest[:])
			state.receiptMatches = !snapshot.Valid || snapshot.String == body
			state.dependencies, state.valid = parseDependencies(dependencies)
			states[id] = state
			if state.valid {
				for _, dep := range state.dependencies {
					if !queued[dep.DraftID] {
						queued[dep.DraftID] = true
						pending = append(pending, dep.DraftID)
					}
				}
			}
		}
		err = rows.Err()
		closeErr := rows.Close()
		if err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, closeErr
		}
	}
	return states, nil
}

func dependencyCurrent(dep DraftSourceDependency, states map[int64]dependencyState) bool {
	state, ok := states[dep.DraftID]
	if !ok || state.hash != dep.ContentHash {
		return false
	}
	if dep.ChapterNumber == nil {
		return true
	}
	if state.status != "finalized" || !state.authoritative || state.chapter != *dep.ChapterNumber {
		return false
	}
	if dep.Kind == "legacy-finalized" {
		return !state.finalizationID.Valid
	}
	return state.finalizationID.Valid && state.finalizationID.String == dep.FinalizationID &&
		state.receiptHash.Valid && state.receiptHash.String == dep.ContentHash && state.receiptMatches
}

// Iterative DFS avoids stack overflow on long historical lineages and marks
// candidate cycles stale. A sealed finalized dependency stops traversal.
func lineageCurrent(root int64, states map[int64]dependencyState, memo map[int64]bool) bool {
	if current, ok := memo[root]; ok {
		return current
	}
	type frame struct {
		id   int64
		next int
	}
	stack := []frame{{id: root}}
	visiting := map[int64]bool{root: true}
	for len(stack) != 0 {
		top := &stack[len(stack)-1]
		if _, ok := memo[top.id]; ok {
			delete(visiting, top.id)
			stack = stack[:len(stack)-1]
			continue
		}
		state, ok := states[top.id]
		if !ok || !state.valid {
			memo[top.id] = false
			continue
		}
		if top.next == len(state.dependencies) {
			current := true
			for _, dep := range state.dependencies {
				if !dependencyCurrent(dep, states) || dep.ChapterNumber == nil && !memo[dep.DraftID] {
					current = false
					break
				}
			}
			memo[top.id] = current
			continue
		}
		dep := state.dependencies[top.next]
		top.next++
		if !dependencyCurrent(dep, states) {
			memo[top.id] = false
			continue
		}
		if dep.ChapterNumber != nil {
			continue
		}
		if _, ok := memo[dep.DraftID]; ok {
			continue
		}
		if visiting[dep.DraftID] {
			memo[top.id] = false
			continue
		}
		visiting[dep.DraftID] = true
		stack = append(stack, frame{id: dep.DraftID})
	}
	return memo[root]
}

func dependenciesCurrent(deps []DraftSourceDependency, states map[int64]dependencyState, memo map[int64]bool) bool {
	for _, dep := range deps {
		if !dependencyCurrent(dep, states) || dep.ChapterNumber == nil && !lineageCurrent(dep.DraftID, states, memo) {
			return false
		}
	}
	return true
}
