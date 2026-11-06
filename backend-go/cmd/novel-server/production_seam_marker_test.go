package main

import (
	"strings"
)

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

// productionSeamDiagnosis explains a marker that was not found, so a failure is a
// diagnosis instead of a repetition. It reports the whitespace-collapsed source
// line the marker claims, and whether the marker's head is still present anywhere
// in the file - the difference between "the port moved" and "the tail changed".
func productionSeamDiagnosis(source, marker string, line int) string {
	collapse := func(value string) string { return strings.Join(strings.Fields(value), " ") }
	lines := strings.Split(source, "\n")
	reported := ""
	if line > 0 && line <= len(lines) {
		reported = collapse(lines[line-1])
		if len(reported) > 160 {
			reported = reported[:160]
		}
	}
	head := collapse(marker)
	if len(head) > 24 {
		head = head[:24]
	}
	where := "the head is not in the file either"
	if strings.Contains(collapse(source), head) {
		where = "the head is still in the file, so the text drifted after it"
	}
	return "line " + itoaForSeam(line) + " now reads <" + reported + ">; " + where
}

func itoaForSeam(value int) string {
	if value == 0 {
		return "0"
	}
	var digits []byte
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	return string(digits)
}
