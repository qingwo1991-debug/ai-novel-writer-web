package main

import "testing"

// The freshness verdict gates confirmation, which writes finalized text over what
// the author already has. Every case that is not a positive proof of sameness must
// therefore read as "conflict"; a future refactor that loosens this is a data-safety
// regression, not a simplification.
func TestSyntheticGraphFreshnessFailsClosed(t *testing.T) {
	const frozen = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	cases := []struct {
		name     string
		kind     string
		current  string
		frozen   string
		expected string
	}{
		{"event still current", "event", frozen, frozen, "current"},
		{"event changed", "event", "bb", frozen, "conflict"},
		{"event with no current hash", "event", "", frozen, "conflict"},
		{"event with no frozen hash", "event", frozen, "", "conflict"},
		{"plot is not re-derived yet", "plot", frozen, frozen, "conflict"},
		{"plan is not re-derived yet", "plan", frozen, frozen, "conflict"},
		{"an empty kind proves nothing", "", frozen, frozen, "conflict"},
	}
	for _, testCase := range cases {
		got := syntheticGraphFreshness(testCase.kind, testCase.current, testCase.frozen)
		if got != testCase.expected {
			t.Fatalf("%s: got %q, want %q", testCase.name, got, testCase.expected)
		}
	}
}
