package admission

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
)

func TestM03RecheckChangeClassificationAndEligibility(t *testing.T) {
	source := "甲在城内。乙在楼上。"
	merged := "甲在城外。乙在楼上。"
	rows := []m03RecheckFinding{
		{FindingID: "first", TargetID: "t1", Kind: "objective", Status: "unresolved", HasSpan: true, SpanStart: 0, SpanEnd: 4, Occurrence: 1},
		{FindingID: "second", TargetID: "t2", Kind: "literary", Status: "unknown", HasSpan: true, SpanStart: 5, SpanEnd: 9, Occurrence: 2},
		{FindingID: "waived", Status: "author-waived"},
	}
	got, err := m03EligibleRecheck(source, merged, rows, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].FindingID != "first" {
		t.Fatalf("changed evidence set differs: %+v", got)
	}
	if got := m03ClassifyEvidenceChange("甲在城内。", "甲 在城内。", rows[0]); got != "unchanged" {
		t.Fatalf("visible no-op classified %q", got)
	}
	if got := m03ClassifyEvidenceChange("甲😀乙", "甲丙乙", m03RecheckFinding{Kind: "literary", HasSpan: true, SpanStart: 1, SpanEnd: 2, Occurrence: 1}); got != "ambiguous" {
		t.Fatalf("split surrogate classified %q", got)
	}
	if got := m03ClassifyEvidenceChange("旧稿", "新稿", m03RecheckFinding{MustShow: true}); got != "changed" {
		t.Fatalf("mustShow classified %q", got)
	}
}

func TestM03RecheckReceiptBindsItemAndRejectsReboundEvidence(t *testing.T) {
	mergedHash := strings.Repeat("a", 64)
	findingHash := strings.Repeat("b", 64)
	item := `{"category":"continuity","severity":"warning","findingId":"f1","targetId":"t1","description":"仍有冲突","resolved":false}`
	resolved := false
	proof := m03RecheckReceiptExpected{CycleID: "c1", MergedHash: mergedHash, FindingSetHash: findingHash,
		ComparisonVersion: 1, Eligible: []m03RecheckFinding{{FindingID: "f1", TargetID: "t1"}},
		ReportItems: []m03RecheckReportItem{{CanonicalJSON: item, Resolved: &resolved}}}
	canonical := `{"findingId":"f1","targetId":"t1","reviewItemIndex":0,"item":` + item + `,"resolved":false}`
	digest := sha256.Sum256([]byte(canonical))
	hash := hex.EncodeToString(digest[:])
	usage := fmt.Sprintf(`{"reviewCycleRecheck":{"version":2,"cycleId":"c1","comparisonVersion":1,"mergedHash":"%s","findingSetHash":"%s","findings":[{"findingId":"f1","targetId":"t1","reviewItemIndex":0,"resolved":false,"evidenceHash":"%s"}]}}`, mergedHash, findingHash, hash)
	version, evidence, err := m03RecheckReceiptProof(usage, proof)
	if err != nil || version != 2 || evidence["f1"].EvidenceHash != hash {
		t.Fatalf("valid original-shape receipt rejected: version=%d evidence=%+v err=%v", version, evidence, err)
	}
	for name, mutated := range map[string]string{
		"hash":          strings.Replace(usage, hash, strings.Repeat("c", 64), 1),
		"wrong target":  strings.Replace(usage, `"targetId":"t1"`, `"targetId":"other"`, 1),
		"v2 resolved":   strings.Replace(usage, `"resolved":false`, `"resolved":true`, 1),
		"unknown item":  strings.Replace(usage, `"reviewItemIndex":0`, `"reviewItemIndex":1`, 1),
		"wrong version": strings.Replace(usage, `"version":2`, `"version":3`, 1),
	} {
		if _, _, err := m03RecheckReceiptProof(mutated, proof); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}
