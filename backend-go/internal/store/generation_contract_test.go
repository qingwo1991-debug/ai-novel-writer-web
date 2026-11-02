package store

import (
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"
)

func glTestRoot() map[string]any {
	return map[string]any{"projectId": "project", "epoch": "epoch", "operation": "write", "uiActionNonce": "nonce", "rootActionId": "root", "frozenInputHash": strings.Repeat("a", 64), "status": "active"}
}

func glTestPolicy() map[string]any {
	return map[string]any{"maxPhysicalRequests": 2, "maxTokenLiability": 1, "maxOutputPerRequest": 8, "maxActiveElapsedMs": 100}
}

func glTestAttempt(id string) map[string]any {
	return map[string]any{"rootActionId": "root", "attemptId": id, "reservationId": "reservation:" + id, "status": "reserved", "reservedTokens": 10, "requestedOutputTokens": 8}
}

func glTestBinding() map[string]any {
	fingerprint := map[string]any{}
	for _, key := range []string{"chapterBriefHash", "authorGuidanceHash", "dependencyHash", "contextSnapshotHash", "templateHash", "skillSnapshotHash", "modelLeaseRevision", "policyHash", "outputContractHash"} {
		fingerprint[key] = strings.Repeat("b", 64)
	}
	return map[string]any{"projectId": "project", "epoch": "epoch", "fingerprint": fingerprint, "contextSnapshotId": "snapshot", "sourceManifest": map[string]any{"authorText": "原文\r\n😀e\u0301 <>&\u2028"}, "sourceRefs": []any{map[string]any{"projectId": "project", "epoch": "epoch", "sourceId": "source", "contentHash": strings.Repeat("c", 64), "revision": 0}}, "operation": "ignored"}
}

func glTestCode(t *testing.T, err error, want string) {
	t.Helper()
	actual := ""
	if err != nil {
		actual = err.Error()
	}
	if actual != want {
		t.Fatalf("error = %q; want %q", actual, want)
	}
}

func glTestJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func TestGLRootKeyUnicodeAndExcludedIdentity(t *testing.T) {
	root := glTestRoot()
	root["projectId"] = "\ufeff项目😀<>&\u2028\u2029\u00a0"
	root["operation"] = " write "
	before := glTestJSON(t, root)
	key, err := GLRootKey(root)
	glTestCode(t, err, "")
	want := "[\"\ufeff项目😀<>&\u2028\u2029\u00a0\",\" write \",\"nonce\",\"" + strings.Repeat("a", 64) + "\"]"
	if key != want {
		t.Fatalf("key = %q; want %q", key, want)
	}
	if glTestJSON(t, root) != before {
		t.Fatal("root mutated")
	}
	root["rootActionId"], root["epoch"] = "reopened", "later-epoch"
	again, err := GLRootKey(root)
	glTestCode(t, err, "")
	if again != key {
		t.Fatal("epoch/rootActionId incorrectly included in key")
	}
	root["epoch"] = "\ufeff\u00a0\u2028\u3000"
	_, err = GLRootKey(root)
	glTestCode(t, err, "INVALID_ROOT_ACTION")
	root["epoch"] = "\u0085"
	_, err = GLRootKey(root)
	glTestCode(t, err, "")
	root["frozenInputHash"] = []any{strings.Repeat("a", 64)}
	key, err = GLRootKey(root)
	glTestCode(t, err, "")
	if !strings.Contains(key, ",[\"") {
		t.Fatal("regex coercion accepted array but key lost original value")
	}
	root["operation"] = true
	_, err = GLRootKey(root)
	glTestCode(t, err, "GL_UNSUPPORTED_INPUT")
}

func TestGLReservationSafeNumbersAndPrecedence(t *testing.T) {
	for _, bad := range []any{true, false, nil, "8", 1.5, float64(9007199254740992), json.Number("9007199254740992"), -1, 0} {
		t.Run(glTestJSON(t, bad), func(t *testing.T) {
			policy := glTestPolicy()
			policy["maxTokenLiability"] = bad
			next := glTestAttempt("next")
			next["status"] = "settled"
			glTestCode(t, GLAssertReservation(glTestRoot(), policy, nil, next, 0), "INVALID_BUDGET")
		})
	}
	policy, next := glTestPolicy(), glTestAttempt("next")
	for _, key := range []string{"maxPhysicalRequests", "maxTokenLiability", "maxOutputPerRequest", "maxActiveElapsedMs"} {
		policy[key] = json.Number("9007199254740991")
	}
	next["reservedTokens"], next["requestedOutputTokens"] = json.Number("9007199254740991"), json.Number("9007199254740991")
	glTestCode(t, GLAssertReservation(glTestRoot(), policy, nil, next, math.Copysign(0, -1)), "")
	glTestCode(t, GLAssertReservation(glTestRoot(), policy, nil, next, true), "INVALID_BUDGET")
	glTestCode(t, GLAssertReservation(glTestRoot(), policy, nil, next, -1), "INVALID_BUDGET")
	root := glTestRoot()
	root["projectId"] = " "
	policy["maxPhysicalRequests"] = false
	glTestCode(t, GLAssertReservation(root, policy, nil, next, 0), "INVALID_ROOT_ACTION")
}

func TestGLReservationBudgetIsNotTokenCapAndNoMutation(t *testing.T) {
	root, policy, next := glTestRoot(), glTestPolicy(), glTestAttempt("next")
	settled := glTestAttempt("prior")
	settled["status"], settled["actualTokens"] = "settled", 9007199254740991
	cancelled := glTestAttempt("cancelled")
	cancelled["status"] = "cancelled-before-dispatch"
	delete(cancelled, "requestedOutputTokens")
	attempts := []map[string]any{settled, cancelled}
	before := glTestJSON(t, []any{root, policy, attempts, next})
	glTestCode(t, GLAssertReservation(root, policy, attempts, next, 99), "")
	if glTestJSON(t, []any{root, policy, attempts, next}) != before {
		t.Fatal("reservation mutated input")
	}
	glTestCode(t, GLAssertReservation(root, policy, attempts, next, 100), "ROOT_BUDGET_EXHAUSTED")
	cancelled["status"] = "unknown"
	glTestCode(t, GLAssertReservation(root, policy, attempts, next, 0), "ROOT_BUDGET_EXHAUSTED")
	cancelled["status"], cancelled["actualTokens"] = "cancelled-before-dispatch", nil
	glTestCode(t, GLAssertReservation(root, policy, attempts, next, 0), "INVALID_LEDGER")
	delete(cancelled, "actualTokens")
	cancelled["reservedTokens"] = 0
	glTestCode(t, GLAssertReservation(root, policy, attempts, next, 0), "INVALID_LEDGER")
}

func TestGLReservationMinimalLedgerAndTruthyIDs(t *testing.T) {
	root, policy, next := glTestRoot(), glTestPolicy(), glTestAttempt("next")
	next["attemptId"], next["reservationId"] = true, []any{}
	glTestCode(t, GLAssertReservation(root, policy, nil, next, 0), "")
	next["attemptId"] = " "
	glTestCode(t, GLAssertReservation(root, policy, nil, next, 0), "")
	next["attemptId"] = 0
	glTestCode(t, GLAssertReservation(root, policy, nil, next, 0), "INVALID_RESERVATION")
	next = glTestAttempt("next")
	prior := glTestAttempt("prior")
	delete(prior, "attemptId")
	delete(prior, "reservationId")
	delete(prior, "requestedOutputTokens")
	glTestCode(t, GLAssertReservation(root, policy, []map[string]any{prior}, next, 0), "")
	duplicate := glTestAttempt("another")
	delete(duplicate, "attemptId")
	glTestCode(t, GLAssertReservation(root, policy, []map[string]any{prior, duplicate}, next, 0), "INVALID_LEDGER")
	prior["attemptId"], prior["status"] = next["attemptId"], "bad-status"
	glTestCode(t, GLAssertReservation(root, policy, []map[string]any{prior}, next, 0), "RESERVATION_CONFLICT")
	next["requestedOutputTokens"] = 9
	glTestCode(t, GLAssertReservation(root, policy, []map[string]any{prior}, next, 0), "INVALID_OUTPUT_RESERVATION")
	prior = glTestAttempt("prior")
	prior["attemptId"], prior["reservationId"] = false, 0
	next = glTestAttempt("next")
	next["attemptId"], next["reservationId"] = true, "0"
	glTestCode(t, GLAssertReservation(root, policy, []map[string]any{prior}, next, 0), "")
}

func TestGLAttemptTransitionFiniteDomain(t *testing.T) {
	statuses := []string{"reserved", "dispatch-marked", "settled", "unknown", "cancelled-before-dispatch", "", "invalid"}
	for _, before := range statuses {
		for _, after := range statuses {
			want := "INVALID_ATTEMPT_TRANSITION"
			if before == "reserved" && (after == "dispatch-marked" || after == "cancelled-before-dispatch") || before == "dispatch-marked" && (after == "settled" || after == "unknown") {
				want = ""
			}
			glTestCode(t, GLAssertAttemptTransition(before, after), want)
		}
	}
}

func TestGLTokenLiabilityIsRawSelector(t *testing.T) {
	cases := []struct {
		name    string
		attempt map[string]any
		want    any
		code    string
	}{
		{"cancelled", map[string]any{"status": "cancelled-before-dispatch", "actualTokens": "not numeric"}, 0, ""},
		{"actual over estimate", map[string]any{"status": "settled", "reservedTokens": 10, "actualTokens": 1000}, 1000, ""},
		{"zero actual", map[string]any{"status": "settled", "reservedTokens": 10, "actualTokens": 0}, 0, ""},
		{"null actual", map[string]any{"status": "settled", "reservedTokens": 10, "actualTokens": nil}, nil, ""},
		{"string actual", map[string]any{"status": "settled", "actualTokens": "raw"}, "raw", ""},
		{"object actual", map[string]any{"status": "settled", "actualTokens": map[string]any{"raw": true}}, map[string]any{"raw": true}, ""},
		{"missing actual", map[string]any{"status": "settled", "reservedTokens": 10}, 10, ""},
		{"unknown ignores actual", map[string]any{"status": "unknown", "reservedTokens": 10, "actualTokens": 999}, 10, ""},
		{"invalid status not validated", map[string]any{"status": "arbitrary", "reservedTokens": "raw"}, "raw", ""},
		{"undefined boundary", map[string]any{"status": "settled"}, nil, "GL_UNSUPPORTED_INPUT"},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			before := glTestJSON(t, item.attempt)
			got, err := GLTokenLiability(item.attempt)
			glTestCode(t, err, item.code)
			if !reflect.DeepEqual(got, item.want) {
				t.Fatalf("liability = %#v; want %#v", got, item.want)
			}
			if glTestJSON(t, item.attempt) != before {
				t.Fatal("token selector mutated input")
			}
		})
	}
}

func TestGLBindingSnapshotAndOriginalValidationOnly(t *testing.T) {
	request := glTestBinding()
	request["projectId"], request["epoch"], request["contextSnapshotId"] = 17, true, []any{}
	ref := request["sourceRefs"].([]any)[0].(map[string]any)
	ref["projectId"], ref["epoch"], ref["sourceId"] = 17, true, []any{}
	ref["span"] = map[string]any{"start": -100, "end": false, "unit": "not checked"}
	ref["arbitrary"] = "retained"
	request["fingerprint"].(map[string]any)["modelLeaseRevision"] = []any{[]any{strings.Repeat("b", 64)}}
	request["sourceManifest"].(map[string]any)["negativeZero"] = math.Copysign(0, -1)
	request["sourceManifest"].(map[string]any)["roundedNumber"] = json.Number("9007199254740993")
	before := glTestJSON(t, request)
	result, err := GLBinding(request)
	glTestCode(t, err, "")
	if len(result) != 6 || result["operation"] != nil {
		t.Fatal("binding field projection differs")
	}
	manifest := result["sourceManifest"].(map[string]any)
	if math.Signbit(manifest["negativeZero"].(float64)) || manifest["roundedNumber"] != float64(9007199254740992) {
		t.Fatal("binding snapshot did not use JS Number/stringify semantics")
	}
	if glTestJSON(t, request) != before {
		t.Fatal("binding mutated input")
	}
	copiedRef := result["sourceRefs"].([]any)[0].(map[string]any)
	if copiedRef["revision"] != float64(0) || copiedRef["arbitrary"] != "retained" {
		t.Fatal("ref revision zero / extra fields not retained")
	}
	copiedRef["span"].(map[string]any)["start"] = 100
	result["sourceManifest"].(map[string]any)["authorText"] = "changed"
	result["fingerprint"].(map[string]any)["modelLeaseRevision"].([]any)[0].([]any)[0] = "changed"
	if glTestJSON(t, request) != before {
		t.Fatal("binding output aliases input")
	}
}

func TestGLBindingRejectionsAndEmptyRefs(t *testing.T) {
	cases := []struct {
		name   string
		change func(map[string]any)
	}{
		{"empty manifest", func(r map[string]any) { r["sourceManifest"] = map[string]any{} }},
		{"array manifest", func(r map[string]any) { r["sourceManifest"] = []any{true} }},
		{"falsy project", func(r map[string]any) { r["projectId"] = 0 }},
		{"falsy epoch", func(r map[string]any) { r["epoch"] = false }},
		{"falsy context", func(r map[string]any) { r["contextSnapshotId"] = "" }},
		{"fingerprint extra field", func(r map[string]any) { r["fingerprint"].(map[string]any)["extra"] = true }},
		{"fingerprint wrong key", func(r map[string]any) {
			fp := r["fingerprint"].(map[string]any)
			delete(fp, "chapterBriefHash")
			fp["extra"] = strings.Repeat("b", 64)
		}},
		{"uppercase hash", func(r map[string]any) { r["fingerprint"].(map[string]any)["policyHash"] = strings.Repeat("A", 64) }},
		{"ref epoch mismatch", func(r map[string]any) { r["sourceRefs"].([]any)[0].(map[string]any)["epoch"] = "other" }},
		{"ref source false", func(r map[string]any) { r["sourceRefs"].([]any)[0].(map[string]any)["sourceId"] = false }},
		{"ref revision bool", func(r map[string]any) { r["sourceRefs"].([]any)[0].(map[string]any)["revision"] = false }},
		{"ref revision negative", func(r map[string]any) { r["sourceRefs"].([]any)[0].(map[string]any)["revision"] = -1 }},
		{"ref revision unsafe", func(r map[string]any) {
			r["sourceRefs"].([]any)[0].(map[string]any)["revision"] = json.Number("9007199254740992")
		}},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			request := glTestBinding()
			item.change(request)
			before := glTestJSON(t, request)
			_, err := GLBinding(request)
			glTestCode(t, err, "GENERATION_BINDING_INVALID")
			if glTestJSON(t, request) != before {
				t.Fatal("rejected binding mutated input")
			}
		})
	}
	request := glTestBinding()
	request["sourceRefs"] = []any{}
	request["projectId"], request["epoch"], request["contextSnapshotId"] = " ", " ", " "
	_, err := GLBinding(request)
	glTestCode(t, err, "")
}

func TestGLExplicitUnsupportedTransportBoundaries(t *testing.T) {
	for _, value := range []any{math.NaN(), math.Inf(1), math.Inf(-1), json.Number("1e999"), []byte("buffer"), string([]byte{0xff}), map[string]any{string([]byte{0xff}): true}} {
		_, err := GLTokenLiability(map[string]any{"reservedTokens": value})
		glTestCode(t, err, "GL_UNSUPPORTED_INPUT")
	}
	cycle := map[string]any{}
	cycle["self"] = cycle
	_, err := GLTokenLiability(cycle)
	glTestCode(t, err, "GL_UNSUPPORTED_INPUT")
	request := glTestBinding()
	delete(request, "sourceRefs")
	_, err = GLBinding(request)
	glTestCode(t, err, "GL_UNSUPPORTED_INPUT")
	request = glTestBinding()
	delete(request, "fingerprint")
	_, err = GLBinding(request)
	glTestCode(t, err, "GL_UNSUPPORTED_INPUT")
	prior, next := glTestAttempt("prior"), glTestAttempt("next")
	prior["attemptId"], next["attemptId"] = []any{}, []any{}
	glTestCode(t, GLAssertReservation(glTestRoot(), glTestPolicy(), []map[string]any{prior}, next, 0), "GL_UNSUPPORTED_INPUT")
}
