package main

import "strings"

// productionSeamContains reports whether a seam marker is still present in the
// source it names, ignoring white space entirely.
//
// The comparison cannot be byte for byte, or even token by token: this
// repository's Go source is written compactly and the workflow formats it in
// place before it runs these tests (`gofmt -w ./cmd ./internal` at
// .github/workflows/go-backend.yml:56, and again at :445). The formatter inserts
// spaces where the compact text had none - around `!=`, after a `case ...:` colon,
// before a block's `{`, after a comma in a parameter list - and it rebreaks lines.
// Collapsing runs of white space is still not enough, because `x:return` and
// `x: return` stay different; removing white space altogether is exact, since the
// formatter never reorders or renames anything.
//
// So a marker is a sequence of characters that has to survive formatting, and a
// failure here means the declaration really moved or changed.
func productionSeamContains(source, marker string) bool {
	return strings.Contains(productionSeamCompact(source), productionSeamCompact(marker))
}

// productionSeamCompact removes every white-space character. Every marker in this
// package is Go or TypeScript punctuation, identifiers and string contents, so a
// genuine false positive would need the same characters to appear in a different
// order - which is precisely what these markers are chosen to prevent.
func productionSeamCompact(value string) string {
	return strings.Join(strings.Fields(value), "")
}

// productionSeamDiagnosis explains a marker that was not found, so a failure is a
// diagnosis instead of a repetition. It reports the line the marker claimed and,
// when the marker's head survives, the compacted text around that head - the only
// way to see what changed after it.
func productionSeamDiagnosis(source, marker string, line int) string {
	lines := strings.Split(source, "\n")
	reported := ""
	if line > 0 && line <= len(lines) {
		reported = productionSeamCompact(lines[line-1])
		if len(reported) > 160 {
			reported = reported[:160]
		}
	}
	compact := productionSeamCompact(marker)
	head := compact
	if len(head) > 24 {
		head = head[:24]
	}
	where := "the head is not in the file either"
	if at := strings.Index(productionSeamCompact(source), head); at >= 0 {
		flat := productionSeamCompact(source)
		end := at + len(head) + 120
		if end > len(flat) {
			end = len(flat)
		}
		where = "the head is still in the file at compact offset " + itoaForSeam(at) + ", followed by <" + flat[at+len(head):end] + ">"
	}
	return "line " + itoaForSeam(line) + " now holds <" + reported + ">; " + where
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
