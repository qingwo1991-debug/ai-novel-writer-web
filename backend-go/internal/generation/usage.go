package generation

import (
	"encoding/json"
	"math"
	"strconv"
	"unicode/utf8"
)

// SettleProviderUsage ports settleProviderUsage in generation-run-service.ts.
// This is an internal settlement primitive, not an input/admission validator:
// trusted must be supplied only by a trusted provider adapter, never by public
// request data. The caller likewise supplies the frozen model's usage policy.
//
// The supported domain is finite acyclic JSON, using map[string]any, []any,
// []map[string]any, nil, bool, valid UTF-8 strings and Go/json.Number numbers.
// Numbers use binary64 JavaScript Number semantics. Unsupported Go inputs
// conservatively retain the reservation rather than acquiring billing authority.
// Optional token fields must explicitly contain nil: an absent property is JS
// undefined, which differs from null and fails the original safe-integer checks.
// Policy is retained unchanged; no model policy or accounting is inferred here.
func SettleProviderUsage(usage map[string]any, policy map[string]any) map[string]any {
	conservative := map[string]any{"policy": policy, "trusted": false}
	if usage == nil || !settlementJSON(usage) || !settlementJSON(policy) || !settlementTruthy(usage["trusted"]) {
		return conservative
	}
	p, pOK := settlementCount(usage["promptTokens"])
	c, cOK := settlementCount(usage["completionTokens"])
	rawR, hasR := usage["reasoningTokens"]
	rawT, hasT := usage["totalTokens"]
	r, rOK := settlementCount(rawR)
	t, tOK := settlementCount(rawT)
	rNull, tNull := hasR && rawR == nil, hasT && rawT == nil
	if !pOK || !cOK || (!rNull && !rOK) || (!tNull && !tOK) {
		return conservative
	}

	accepted := func(actual float64) map[string]any {
		receipt := map[string]any{"policy": policy, "trusted": true, "inputTokens": p, "completionTokens": c, "actualTokens": actual}
		if !rNull {
			receipt["reasoningTokens"] = r
		}
		return receipt
	}
	reasoning, _ := policy["reasoning"].(string)
	if reasoning == "unknown" {
		// A protocol label cannot establish an unknown supplier's billing rules.
		if tNull || !settlementSafe(p+c) || !(t == p+c && (rNull || r <= c) || !rNull && t == p+c+r) {
			return conservative
		}
		return accepted(t)
	}

	accounting, accountingPresent := usage["accounting"]
	policyReasoning, policyPresent := policy["reasoning"]
	accountingName, _ := accounting.(string)
	if accountingName == "unknown" || !settlementEqualScalar(accounting, accountingPresent, policyReasoning, policyPresent) {
		return conservative
	}
	if accountingName == "included-in-completion" && !rNull && r > c {
		return conservative
	}
	if accountingName == "separately-billed" && rNull {
		return conservative
	}
	actual, expectedTotal := p+c, p+c
	if accountingName == "separately-billed" {
		actual += r
		if settlementTruthy(usage["totalIncludesReasoning"]) {
			expectedTotal += r
		}
	}
	if !tNull && t != expectedTotal || !settlementSafe(actual) {
		return conservative
	}
	return accepted(actual)
}

func settlementSafe(number float64) bool {
	return !math.IsNaN(number) && !math.IsInf(number, 0) && number >= 0 && number <= 9007199254740991 && math.Trunc(number) == number
}

func settlementCount(value any) (float64, bool) {
	number, ok := settlementNumber(value)
	return number, ok && settlementSafe(number)
}

func settlementNumber(value any) (float64, bool) {
	var number float64
	switch value := value.(type) {
	case json.Number:
		text := string(value)
		if len(text) == 0 || (text[0] != '-' && (text[0] < '0' || text[0] > '9')) || !json.Valid([]byte(text)) {
			return 0, false
		}
		parsed, err := strconv.ParseFloat(text, 64)
		if err != nil {
			return 0, false
		}
		number = parsed
	case float64:
		number = value
	case float32:
		number = float64(value)
	case int:
		number = float64(value)
	case int8:
		number = float64(value)
	case int16:
		number = float64(value)
	case int32:
		number = float64(value)
	case int64:
		number = float64(value)
	case uint:
		number = float64(value)
	case uint8:
		number = float64(value)
	case uint16:
		number = float64(value)
	case uint32:
		number = float64(value)
	case uint64:
		number = float64(value)
	default:
		return 0, false
	}
	return number, !math.IsNaN(number) && !math.IsInf(number, 0)
}

func settlementTruthy(value any) bool {
	switch value := value.(type) {
	case nil:
		return false
	case bool:
		return value
	case string:
		return value != ""
	}
	if number, ok := settlementNumber(value); ok {
		return number != 0
	}
	return true // All supported JSON objects and arrays are truthy, even empty.
}

// The typed TS contract uses string accounting labels. Scalar comparisons also
// retain strict JS equality for malformed finite JSON labels; object-identity
// labels are outside that contract and conservatively cannot confer trust.
func settlementEqualScalar(a any, aPresent bool, b any, bPresent bool) bool {
	if aPresent != bPresent {
		return false
	}
	if !aPresent {
		return true
	}
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	if number, ok := settlementNumber(a); ok {
		other, otherOK := settlementNumber(b)
		return otherOK && number == other
	}
	switch value := a.(type) {
	case bool:
		other, ok := b.(bool)
		return ok && value == other
	case string:
		other, ok := b.(string)
		return ok && value == other
	default:
		return false
	}
}

func settlementJSON(value any) bool {
	// Marshal detects cycles and malformed json.Number without recursive walking
	// of a potentially cyclic graph. The walk then rejects non-JSON Go types and
	// invalid UTF-8 instead of silently accepting encoding/json's replacements.
	if _, err := json.Marshal(value); err != nil {
		return false
	}
	pending := []any{value}
	for len(pending) != 0 {
		item := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		switch value := item.(type) {
		case nil, bool:
		case string:
			if !utf8.ValidString(value) {
				return false
			}
		case map[string]any:
			if value == nil {
				return false
			}
			for key, child := range value {
				if !utf8.ValidString(key) {
					return false
				}
				pending = append(pending, child)
			}
		case []any:
			if value == nil {
				return false
			}
			pending = append(pending, value...)
		case []map[string]any:
			if value == nil {
				return false
			}
			for _, child := range value {
				pending = append(pending, child)
			}
		default:
			if _, ok := settlementNumber(item); !ok {
				return false
			}
		}
	}
	return true
}
