package graphgen

import (
	"sort"
)

// CanonicalJSON encodes a value that was decoded from JSON — so
// map[string]any, []any, string, float64, bool and nil — into a byte sequence
// that depends only on the value's content.
//
// EncodeJSON deliberately refuses Go maps, because a TypeScript writer hashes
// JSON.stringify(context) in property insertion order and a Go map cannot
// reproduce that order. That refusal is correct and stays. What it means in
// practice is that a value which has already been decoded into a map has lost
// the byte sequence its original writer hashed, and re-decoding it can never
// recover that sequence.
//
// CanonicalJSON is the answer for the graph lane: the graph context, task and
// slot hashes are defined over a canonical form instead of an insertion order.
// Object keys are sorted at every level whether the value arrived as a map or
// as an ordered Object, array order is preserved, and the leaf encoding is
// EncodeJSON's, so the JavaScript rules for numbers, strings and escapes are
// unchanged.
//
// The writer and the verifier must both use this function. A run written by
// CanonicalJSON can be verified by CanonicalJSON; a run written by the Node
// build cannot, which is why a Go deployment must not point at a database the
// Node build already executed graph generation against.
func CanonicalJSON(value any) (string, error) {
	canonical, err := canonicalValue(value)
	if err != nil {
		return "", err
	}
	return EncodeJSON(canonical)
}

// CanonicalHash is TextHash(CanonicalJSON(value)).
func CanonicalHash(value any) (string, error) {
	text, err := CanonicalJSON(value)
	if err != nil {
		return "", err
	}
	return TextHash(text), nil
}

// canonicalValue rewrites maps into ordered Objects with sorted keys and
// recurses through arrays. Values it does not recognise are passed through
// untouched so that EncodeJSON reports them.
func canonicalValue(value any) (any, error) {
	switch typed := value.(type) {
	case nil:
		return nil, nil
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		object := make(Object, 0, len(keys))
		for _, key := range keys {
			item, err := canonicalValue(typed[key])
			if err != nil {
				return nil, err
			}
			object = append(object, Member{Key: key, Value: item})
		}
		return object, nil
	case Object:
		// Objects are sorted too, not preserved: the point of this function is
		// that a value built as an ordered Object and the same value decoded
		// into a map must canonicalise identically. Callers that need
		// insertion order use EncodeJSON, which never sorts.
		members := make([]Member, 0, len(typed))
		keys := make([]string, 0, len(typed))
		values := make(map[string]any, len(typed))
		for _, member := range typed {
			if _, duplicate := values[member.Key]; !duplicate {
				keys = append(keys, member.Key)
			}
			values[member.Key] = member.Value
		}
		sort.Strings(keys)
		for _, key := range keys {
			item, err := canonicalValue(values[key])
			if err != nil {
				return nil, err
			}
			members = append(members, Member{Key: key, Value: item})
		}
		return Object(members), nil
	case []any:
		items := make([]any, 0, len(typed))
		for _, item := range typed {
			converted, err := canonicalValue(item)
			if err != nil {
				return nil, err
			}
			items = append(items, converted)
		}
		return items, nil
	case ObjectValue:
		return canonicalValue(typed.JSONValue())
	default:
		return value, nil
	}
}
