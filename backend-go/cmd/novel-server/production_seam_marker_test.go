package main

import "strings"

// productionSeamContains reports whether a seam marker is still present in the
// source it names, comparing tokens rather than bytes.
//
// The comparison has to ignore whitespace because this repository's Go source is
// written compactly and the workflow formats it in place before it runs these
// tests: `gofmt -w ./cmd ./internal` (.github/workflows/go-backend.yml:56 and
// :445) turns `if x{return y}` into `if x { return y }` and adds the space after
// a comma in a parameter list. A byte comparison therefore fails against text
// that says exactly what the marker says, which is a false alarm about drift
// where there is none - and the whole point of these markers is that a failure
// means the port really moved.
//
// Collapsing runs of white space keeps the check honest: every word of the marker
// still has to appear, in order, in the same file.
func productionSeamContains(source, marker string) bool {
	collapse := func(value string) string { return strings.Join(strings.Fields(value), " ") }
	return strings.Contains(collapse(source), collapse(marker))
}
