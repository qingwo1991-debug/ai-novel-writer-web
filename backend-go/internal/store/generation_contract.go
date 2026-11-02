package store

import (
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

// These are pure generation-contract.ts / generation-run-repository.ts binding
// primitives, NOT generation admission, durable acknowledgement, or capability.
// The supported transport domain is finite, acyclic JSON with valid UTF-8 Go
// strings (including keys), represented by map[string]any, []any, []map[string]any,
// nil, bool, string and Go/json.Number numbers. Numbers have JS Number semantics.
// JS undefined, non-string .trim() receivers, non-object fingerprint/ref records,
// non-array sourceRefs, and object-valued ledger identity comparisons are explicit
// unsupported boundaries, not additional original-contract validation rules.
// Lone UTF-16 surrogates cannot be represented faithfully by valid UTF-8 Go
// strings; callers must reject them before lossy JSON decoding. No normalization
// of author text, whitespace, hashes or identifiers occurs here.
var ErrGLUnsupportedInput = errors.New("GL_UNSUPPORTED_INPUT")

func glError(code string) error { return errors.New(code) }

func glDomain(value any) error {
	// Marshal detects cycles before the iterative UTF-8/type walk. Do not admit
	// arbitrary structs, []byte, NaN/Infinity, or invalid json.Number lexemes.
	if _, err := json.Marshal(value); err != nil {
		return ErrGLUnsupportedInput
	}
	pending := []any{value}
	for len(pending) != 0 {
		item := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		switch v := item.(type) {
		case nil, bool:
		case string:
			if !utf8.ValidString(v) {
				return ErrGLUnsupportedInput
			}
		case map[string]any:
			if v == nil {
				return ErrGLUnsupportedInput
			}
			for key, child := range v {
				if !utf8.ValidString(key) {
					return ErrGLUnsupportedInput
				}
				pending = append(pending, child)
			}
		case []any:
			if v == nil {
				return ErrGLUnsupportedInput
			}
			pending = append(pending, v...)
		case []map[string]any:
			if v == nil {
				return ErrGLUnsupportedInput
			}
			for _, child := range v {
				pending = append(pending, child)
			}
		default:
			number, ok := plotNumber(item)
			if !ok || math.IsNaN(number) || math.IsInf(number, 0) {
				return ErrGLUnsupportedInput
			}
		}
	}
	return nil
}

func glInteger(value any, minimum float64) (float64, bool) {
	number, ok := plotNumber(value)
	return number, ok && !math.IsNaN(number) && !math.IsInf(number, 0) &&
		math.Abs(number) <= 9007199254740991 && math.Trunc(number) == number && number >= minimum
}

func glTruthy(value any) bool {
	if number, ok := plotNumber(value); ok {
		return number != 0 && !math.IsNaN(number)
	}
	return phTruthy(value)
}

// RegExp.test coerces JSON arrays to strings (including nested singleton arrays).
// In particular [hash] passes isContentHash in the original; do not tighten it.
func glHash(value any) bool { return plotRevisionPattern.MatchString(plotEnumString(value)) }

// A missing field is JS undefined, distinct from explicit null. Primitive IDs
// retain strict equality / Set SameValueZero semantics, without bool coercion.
func glIdentity(record map[string]any, key string) (string, error) {
	value, exists := record[key]
	if !exists {
		return "undefined", nil
	}
	switch v := value.(type) {
	case nil:
		return "null", nil
	case bool:
		return "bool:" + strconv.FormatBool(v), nil
	case string:
		return "string:" + v, nil
	}
	if number, ok := plotNumber(value); ok {
		return "number:" + phNumberString(number), nil
	}
	return "", ErrGLUnsupportedInput
}

func glEqualFields(a map[string]any, ak string, b map[string]any, bk string) (bool, error) {
	av, aExists := a[ak]
	bv, bExists := b[bk]
	if aExists != bExists {
		return false, nil
	}
	if !aExists {
		return true, nil
	}
	// Object !== primitive is decidable without granting objects an identity.
	_, aMap := av.(map[string]any)
	_, aArray := plotArray(av)
	_, bMap := bv.(map[string]any)
	_, bArray := plotArray(bv)
	if (aMap || aArray) != (bMap || bArray) {
		return false, nil
	}
	aid, err := glIdentity(a, ak)
	if err != nil {
		return false, err
	}
	bid, err := glIdentity(b, bk)
	if err != nil {
		return false, err
	}
	return aid == bid, nil
}

// GLRootKey deliberately excludes epoch and rootActionId from the returned key,
// while still requiring their original nonblank-string admission checks.
func GLRootKey(root map[string]any) (string, error) {
	if err := glDomain(root); err != nil {
		return "", err
	}
	for _, key := range []string{"projectId", "epoch", "operation", "uiActionNonce", "rootActionId"} {
		text, ok := root[key].(string)
		if !ok {
			return "", ErrGLUnsupportedInput
		}
		if strings.TrimFunc(text, narrativeSpace) == "" {
			return "", glError("INVALID_ROOT_ACTION")
		}
	}
	if !glHash(root["frozenInputHash"]) {
		return "", glError("INVALID_ROOT_ACTION")
	}
	return plotJSON([]any{root["projectId"], root["operation"], root["uiActionNonce"], root["frozenInputHash"]})
}

// GLAssertReservation preserves check order and intentionally never sums or caps
// token liabilities: tokens are bookkeeping only (ADR-0023).
func GLAssertReservation(root, policy map[string]any, attempts []map[string]any, next map[string]any, elapsed any) error {
	if _, err := GLRootKey(root); err != nil {
		return err
	}
	for _, input := range []any{policy, next, elapsed} {
		if err := glDomain(input); err != nil {
			return err
		}
	}
	// nil attempts is the conventional Go spelling of an empty readonly array.
	for _, attempt := range attempts {
		if err := glDomain(attempt); err != nil {
			return err
		}
	}
	for _, value := range []any{policy["maxPhysicalRequests"], policy["maxTokenLiability"], policy["maxOutputPerRequest"], policy["maxActiveElapsedMs"], next["reservedTokens"], next["requestedOutputTokens"]} {
		if _, ok := glInteger(value, 1); !ok {
			return glError("INVALID_BUDGET")
		}
	}
	elapsedNumber, elapsedOK := glInteger(elapsed, 0)
	if !elapsedOK {
		return glError("INVALID_BUDGET")
	}
	sameRoot, err := glEqualFields(next, "rootActionId", root, "rootActionId")
	if err != nil {
		return err
	}
	if !plotStringIs(root["status"], "active") || !sameRoot || !glTruthy(next["attemptId"]) || !glTruthy(next["reservationId"]) || !plotStringIs(next["status"], "reserved") {
		return glError("INVALID_RESERVATION")
	}
	requested, _ := glInteger(next["requestedOutputTokens"], 1)
	reserved, _ := glInteger(next["reservedTokens"], 1)
	maxOutput, _ := glInteger(policy["maxOutputPerRequest"], 1)
	if requested > maxOutput || reserved < requested {
		return glError("INVALID_OUTPUT_RESERVATION")
	}
	for _, attempt := range attempts {
		rootEqual, err := glEqualFields(attempt, "rootActionId", root, "rootActionId")
		if err != nil {
			return err
		}
		if !rootEqual {
			return glError("RESERVATION_CONFLICT")
		}
		for _, key := range []string{"attemptId", "reservationId"} {
			equal, err := glEqualFields(attempt, key, next, key)
			if err != nil {
				return err
			}
			if equal {
				return glError("RESERVATION_CONFLICT")
			}
		}
	}
	for _, attempt := range attempts {
		if _, ok := glInteger(attempt["reservedTokens"], 1); !ok {
			return glError("INVALID_LEDGER")
		}
		if actual, exists := attempt["actualTokens"]; exists {
			if _, ok := glInteger(actual, 0); !ok {
				return glError("INVALID_LEDGER")
			}
		}
	}
	for _, key := range []string{"attemptId", "reservationId"} {
		seen := make(map[string]bool, len(attempts))
		for _, attempt := range attempts {
			identity, err := glIdentity(attempt, key)
			if err != nil {
				return err
			}
			if seen[identity] {
				return glError("INVALID_LEDGER")
			}
			seen[identity] = true
		}
	}
	physical := 0
	for _, attempt := range attempts {
		switch status, _ := attempt["status"].(string); status {
		case "reserved", "dispatch-marked", "settled", "unknown":
			physical++
		case "cancelled-before-dispatch":
		default:
			return glError("INVALID_LEDGER")
		}
	}
	maxPhysical, _ := glInteger(policy["maxPhysicalRequests"], 1)
	maxElapsed, _ := glInteger(policy["maxActiveElapsedMs"], 1)
	if float64(physical) >= maxPhysical || elapsedNumber >= maxElapsed {
		return glError("ROOT_BUDGET_EXHAUSTED")
	}
	return nil
}

func GLAssertAttemptTransition(before, after string) error {
	if before == "reserved" && (after == "dispatch-marked" || after == "cancelled-before-dispatch") ||
		before == "dispatch-marked" && (after == "settled" || after == "unknown") {
		return nil
	}
	return glError("INVALID_ATTEMPT_TRANSITION")
}

// GLTokenLiability is a selector, not a ledger validator. Null/string/object
// actualTokens remain unchanged. Undefined selected values are unsupported,
// never silently confused with JSON null. Returned objects are borrowed values,
// matching JS reference behavior; this function itself performs no mutation.
func GLTokenLiability(attempt map[string]any) (any, error) {
	if err := glDomain(attempt); err != nil {
		return nil, err
	}
	if plotStringIs(attempt["status"], "cancelled-before-dispatch") {
		return 0, nil
	}
	if plotStringIs(attempt["status"], "settled") {
		if value, exists := attempt["actualTokens"]; exists {
			return value, nil
		}
	}
	if value, exists := attempt["reservedTokens"]; exists {
		return value, nil
	}
	return nil, ErrGLUnsupportedInput
}

// GLBinding returns only the original six binding fields as an independent JSON
// snapshot. It does not validate source spans, author text hashes, manifest
// contents, unknown ref fields, or grant any strict execution capability.
func GLBinding(request map[string]any) (map[string]any, error) {
	if err := glDomain(request); err != nil {
		return nil, err
	}
	invalid := func() (map[string]any, error) { return nil, glError("GENERATION_BINDING_INVALID") }
	manifest, manifestOK := request["sourceManifest"].(map[string]any)
	if !manifestOK || len(manifest) == 0 || !glTruthy(request["projectId"]) || !glTruthy(request["epoch"]) || !glTruthy(request["contextSnapshotId"]) {
		return invalid()
	}
	fingerprint, ok := request["fingerprint"].(map[string]any)
	if !ok {
		return nil, ErrGLUnsupportedInput
	}
	if len(fingerprint) != 9 {
		return invalid()
	}
	for _, key := range []string{"chapterBriefHash", "authorGuidanceHash", "dependencyHash", "contextSnapshotHash", "templateHash", "skillSnapshotHash", "modelLeaseRevision", "policyHash", "outputContractHash"} {
		if !glHash(fingerprint[key]) {
			return invalid()
		}
	}
	refs, ok := plotArray(request["sourceRefs"])
	if !ok {
		return nil, ErrGLUnsupportedInput
	}
	for _, value := range refs {
		ref, ok := value.(map[string]any)
		if !ok {
			return nil, ErrGLUnsupportedInput
		}
		if !glTruthy(ref["projectId"]) || !glTruthy(ref["epoch"]) {
			return invalid()
		}
		for _, key := range []string{"projectId", "epoch"} {
			equal, err := glEqualFields(ref, key, request, key)
			if err != nil {
				return nil, err
			}
			if !equal {
				return invalid()
			}
		}
		if !glTruthy(ref["sourceId"]) || !glHash(ref["contentHash"]) {
			return invalid()
		}
		if _, ok := glInteger(ref["revision"], 0); !ok {
			return invalid()
		}
	}
	selected := make(map[string]any, 6)
	for _, key := range []string{"projectId", "epoch", "fingerprint", "contextSnapshotId", "sourceManifest", "sourceRefs"} {
		selected[key] = request[key]
	}
	encoded, err := json.Marshal(selected)
	if err != nil {
		return nil, ErrGLUnsupportedInput
	}
	var result map[string]any
	if err := json.Unmarshal(encoded, &result); err != nil {
		return nil, ErrGLUnsupportedInput
	}
	// Decode all native numeric spellings to JS Numbers, then use the shared
	// JS encoder so nested negative zero becomes +0 exactly as JSON.stringify.
	jsEncoded, err := plotJSON(result)
	if err != nil {
		return nil, ErrGLUnsupportedInput
	}
	if err := json.Unmarshal([]byte(jsEncoded), &result); err != nil {
		return nil, ErrGLUnsupportedInput
	}
	return result, nil
}
