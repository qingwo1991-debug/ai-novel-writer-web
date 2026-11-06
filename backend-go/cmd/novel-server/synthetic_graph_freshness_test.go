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
		{"the kind rule proves no plan by itself", "plan", frozen, frozen, "conflict"},
		{"an empty kind proves nothing", "", frozen, frozen, "conflict"},
	}
	for _, testCase := range cases {
		got := syntheticGraphFreshness(testCase.kind, testCase.current, testCase.frozen)
		if got != testCase.expected {
			t.Fatalf("%s: got %q, want %q", testCase.name, got, testCase.expected)
		}
	}
}

// The plan kind is re-derived elsewhere: syntheticGraphSourceStatus re-reads the
// blueprint, the total chapter count and the writing language and hands the two
// digests to this equality rule. So the same pair the kind rule above refuses has
// to answer "current" here - the split is pinned from both sides. A refactor that
// makes this rule refuse a real match would turn every unchanged plan run into a
// conflict and disable the confirmation it gates, which is the opposite failure
// from the one the case above guards.
func TestSyntheticGraphHashFreshnessProvesSameness(t *testing.T) {
	const frozen = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	cases := []struct {
		name     string
		current  string
		frozen   string
		expected string
	}{
		{"identical digests are current", frozen, frozen, "current"},
		{"a moved source is a conflict", "bb", frozen, "conflict"},
		{"no current digest proves nothing", "", frozen, "conflict"},
		{"no frozen digest proves nothing", frozen, "", "conflict"},
	}
	for _, testCase := range cases {
		got := syntheticGraphHashFreshness(testCase.current, testCase.frozen)
		if got != testCase.expected {
			t.Fatalf("%s: got %q, want %q", testCase.name, got, testCase.expected)
		}
	}
}
