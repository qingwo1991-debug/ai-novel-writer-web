package generation

import (
	"encoding/json"
	"math"
	"reflect"
	"testing"
)

func settlementTestPolicy(reasoning any) map[string]any {
	return map[string]any{"reasoning": reasoning, "estimatorVersion": "test-v1", "safetyMarginTokens": 10, "canBoundTotalLiability": false}
}

func settlementTestUsage(changes map[string]any) map[string]any {
	usage := map[string]any{"promptTokens": 50, "completionTokens": 30, "reasoningTokens": 20, "totalTokens": 80, "accounting": "included-in-completion", "totalIncludesReasoning": true, "trusted": true}
	for key, value := range changes {
		usage[key] = value
	}
	return usage
}

func TestSettleProviderUsageAccountingBranches(t *testing.T) {
	cases := []struct {
		name      string
		reasoning string
		changes   map[string]any
		trusted   bool
		actual    float64
	}{
		{"included", "included-in-completion", nil, true, 80},
		{"included-null-total", "included-in-completion", map[string]any{"totalTokens": nil}, true, 80},
		{"included-null-reasoning", "included-in-completion", map[string]any{"reasoningTokens": nil}, true, 80},
		{"included-too-much-reasoning", "included-in-completion", map[string]any{"reasoningTokens": 31}, false, 0},
		{"included-total-mismatch", "included-in-completion", map[string]any{"totalTokens": 100}, false, 0},
		{"included-total-flag-irrelevant", "included-in-completion", map[string]any{"totalIncludesReasoning": false}, true, 80},
		{"separate-total-includes", "separately-billed", map[string]any{"accounting": "separately-billed", "totalTokens": 100}, true, 100},
		{"separate-total-excludes", "separately-billed", map[string]any{"accounting": "separately-billed", "totalIncludesReasoning": false}, true, 100},
		{"separate-null-total", "separately-billed", map[string]any{"accounting": "separately-billed", "totalTokens": nil}, true, 100},
		{"separate-null-reasoning", "separately-billed", map[string]any{"accounting": "separately-billed", "reasoningTokens": nil}, false, 0},
		{"separate-zero-reasoning", "separately-billed", map[string]any{"accounting": "separately-billed", "reasoningTokens": 0}, true, 80},
		{"separate-total-mismatch", "separately-billed", map[string]any{"accounting": "separately-billed"}, false, 0},
		{"accounting-mismatch", "included-in-completion", map[string]any{"accounting": "separately-billed"}, false, 0},
		{"known-policy-unknown-accounting", "included-in-completion", map[string]any{"accounting": "unknown"}, false, 0},
		{"unknown-included-total", "unknown", nil, true, 80},
		{"unknown-separate-total", "unknown", map[string]any{"totalTokens": 100, "accounting": "unknown"}, true, 100},
		{"unknown-null-reasoning", "unknown", map[string]any{"reasoningTokens": nil}, true, 80},
		{"unknown-null-total", "unknown", map[string]any{"totalTokens": nil}, false, 0},
		{"unknown-inconsistent-total", "unknown", map[string]any{"totalTokens": 81}, false, 0},
		{"unknown-included-excess-reasoning", "unknown", map[string]any{"reasoningTokens": 31}, false, 0},
		{"unknown-separate-excess-reasoning", "unknown", map[string]any{"reasoningTokens": 31, "totalTokens": 111}, true, 111},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			policy := settlementTestPolicy(tc.reasoning)
			usage := settlementTestUsage(tc.changes)
			got := SettleProviderUsage(usage, policy)
			want := map[string]any{"policy": policy, "trusted": tc.trusted}
			if tc.trusted {
				want["inputTokens"], want["completionTokens"], want["actualTokens"] = float64(50), float64(30), tc.actual
				if usage["reasoningTokens"] != nil {
					r, _ := settlementNumber(usage["reasoningTokens"])
					want["reasoningTokens"] = r
				}
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("got %#v; want %#v", got, want)
			}
		})
	}
}

func TestSettleProviderUsageMissingNullAndSafeNumbers(t *testing.T) {
	policy := settlementTestPolicy("included-in-completion")
	if got := SettleProviderUsage(nil, policy); !reflect.DeepEqual(got, map[string]any{"policy": policy, "trusted": false}) {
		t.Fatal(got)
	}
	for _, key := range []string{"promptTokens", "completionTokens", "reasoningTokens", "totalTokens"} {
		usage := settlementTestUsage(nil)
		delete(usage, key)
		if SettleProviderUsage(usage, policy)["trusted"] != false {
			t.Errorf("missing %s was accepted", key)
		}
		for _, invalid := range []any{-1, 0.5, "20", true, []any{}, map[string]any{}, float64(9007199254740992)} {
			if got := SettleProviderUsage(settlementTestUsage(map[string]any{key: invalid}), policy); got["trusted"] != false {
				t.Errorf("%s=%#v was accepted: %#v", key, invalid, got)
			}
		}
	}
	for _, key := range []string{"promptTokens", "completionTokens"} {
		if SettleProviderUsage(settlementTestUsage(map[string]any{key: nil}), policy)["trusted"] != false {
			t.Errorf("null %s was accepted", key)
		}
	}
	cases := []struct {
		name      string
		reasoning string
		changes   map[string]any
		trusted   bool
		actual    float64
	}{
		{"zero", "included-in-completion", map[string]any{"promptTokens": 0, "completionTokens": 0, "reasoningTokens": 0, "totalTokens": 0}, true, 0},
		{"max-safe", "included-in-completion", map[string]any{"promptTokens": int64(9007199254740991), "completionTokens": 0, "reasoningTokens": nil, "totalTokens": nil}, true, 9007199254740991},
		{"overflow-included", "included-in-completion", map[string]any{"promptTokens": int64(9007199254740991), "completionTokens": 1, "reasoningTokens": nil, "totalTokens": nil}, false, 0},
		{"overflow-separate", "separately-billed", map[string]any{"accounting": "separately-billed", "promptTokens": int64(9007199254740990), "completionTokens": 1, "reasoningTokens": 1, "totalTokens": nil}, false, 0},
		{"overflow-unknown", "unknown", map[string]any{"promptTokens": int64(9007199254740991), "completionTokens": 1, "reasoningTokens": 0, "totalTokens": int64(9007199254740991)}, false, 0},
		{"json-number-rounding", "included-in-completion", map[string]any{"promptTokens": json.Number("49.999999999999999999"), "completionTokens": json.Number("3e1"), "totalTokens": json.Number("80.0")}, true, 80},
		{"negative-zero", "included-in-completion", map[string]any{"promptTokens": math.Copysign(0, -1), "completionTokens": 0, "reasoningTokens": nil, "totalTokens": 0}, true, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SettleProviderUsage(settlementTestUsage(tc.changes), settlementTestPolicy(tc.reasoning))
			if got["trusted"] != tc.trusted || tc.trusted && got["actualTokens"] != tc.actual {
				t.Fatal(got)
			}
		})
	}
}

func TestSettleProviderUsageTruthinessAndInputDomain(t *testing.T) {
	policy := settlementTestPolicy("included-in-completion")
	for _, value := range []any{false, nil, "", 0, json.Number("0"), math.Copysign(0, -1)} {
		if SettleProviderUsage(settlementTestUsage(map[string]any{"trusted": value}), policy)["trusted"] != false {
			t.Errorf("falsy trusted %#v accepted", value)
		}
	}
	for _, value := range []any{true, "false", 1, -1, []any{}, map[string]any{}, []map[string]any{}} {
		if SettleProviderUsage(settlementTestUsage(map[string]any{"trusted": value}), policy)["trusted"] != true {
			t.Errorf("truthy trusted %#v rejected", value)
		}
	}
	for _, tc := range []struct {
		value any
		total int
	}{{false, 80}, {nil, 80}, {0, 80}, {"", 80}, {true, 100}, {"false", 100}, {[]any{}, 100}, {map[string]any{}, 100}} {
		usage := settlementTestUsage(map[string]any{"accounting": "separately-billed", "totalIncludesReasoning": tc.value, "totalTokens": tc.total})
		if got := SettleProviderUsage(usage, settlementTestPolicy("separately-billed")); got["trusted"] != true || got["actualTokens"] != float64(100) {
			t.Errorf("totalIncludesReasoning=%#v: %#v", tc.value, got)
		}
	}
	cyclic := map[string]any{}
	cyclic["self"] = cyclic
	for _, value := range []any{math.NaN(), math.Inf(1), math.Inf(-1), json.Number("NaN"), json.Number("01"), json.Number("1e999"), []byte("secret"), make(chan int), func() {}, struct{ N int }{1}, string([]byte{0xff}), map[string]any{string([]byte{0xff}): 1}, cyclic, map[string]any(nil), []any(nil)} {
		usage := settlementTestUsage(map[string]any{"extra": value})
		if got := SettleProviderUsage(usage, policy); got["trusted"] != false || len(got) != 2 {
			t.Errorf("unsupported %T conferred trust", value)
		}
		badPolicy := settlementTestPolicy("included-in-completion")
		badPolicy["extra"] = value
		if got := SettleProviderUsage(settlementTestUsage(nil), badPolicy); got["trusted"] != false {
			t.Errorf("unsupported policy %T conferred trust", value)
		}
	}
	usage := settlementTestUsage(nil)
	beforeUsage, _ := json.Marshal(usage)
	beforePolicy, _ := json.Marshal(policy)
	got := SettleProviderUsage(usage, policy)
	afterUsage, _ := json.Marshal(usage)
	afterPolicy, _ := json.Marshal(policy)
	if string(beforeUsage) != string(afterUsage) || string(beforePolicy) != string(afterPolicy) {
		t.Fatal("settlement mutated its inputs")
	}
	policy["marker"] = "retained"
	if got["policy"].(map[string]any)["marker"] != "retained" {
		t.Fatal("original policy identity was not retained")
	}
}
