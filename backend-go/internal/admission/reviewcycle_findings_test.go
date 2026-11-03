package admission

import "testing"

func m03FindingTestBase() (m03CycleBasicEvidence, m03FindingRow) {
	content := "ab😀ab"
	basic := m03CycleBasicEvidence{cycleID: "cycle", projectID: "project", epoch: "epoch", baseDraftID: 7,
		sourceVersion: 2, sourceContent: content, sourceHash: m03StringHash(content)}
	start, end, occurrence := int64(2), int64(4), int64(1)
	unit, excerpt, target := "utf16-code-unit", m03StringHash("😀"), "target"
	f := m03FindingRow{M03FindingIdentity: M03FindingIdentity{FindingID: "finding", SourceProjectID: "project",
		SourceEpoch: "epoch", SourceID: "draft:7", SourceRevision: 2, SourceContentHash: basic.sourceHash,
		SpanStart: &start, SpanEnd: &end, SpanUnit: &unit, ExcerptHash: &excerpt, Occurrence: &occurrence,
		Category: "consistency", TargetID: &target, Kind: "objective", ProblemText: "problem"}, status: "unknown"}
	return basic, f
}

func TestM03FindingRowAnchorAndUTF16(t *testing.T) {
	basic, f := m03FindingTestBase()
	if err := m03VerifyFindingRow(f, basic, m03FindingRowProof{}, map[string]bool{}); err != nil {
		t.Fatal(err)
	}
	half := f
	halfEnd := int64(3)
	halfHash := m03StringHash("\ufffd") // Node UTF-8 replaces an isolated UTF-16 surrogate.
	half.SpanEnd, half.ExcerptHash = &halfEnd, &halfHash
	if err := m03VerifyFindingRow(half, basic, m03FindingRowProof{}, map[string]bool{}); err != nil {
		t.Fatal(err)
	}
	mutations := []struct {
		name   string
		change func(*m03FindingRow)
	}{
		{"wrong excerpt", func(f *m03FindingRow) { s := m03StringHash("ab"); f.ExcerptHash = &s }},
		{"wrong occurrence", func(f *m03FindingRow) { n := int64(2); f.Occurrence = &n }},
		{"split surrogate span", func(f *m03FindingRow) { n := int64(3); f.SpanEnd = &n }},
		{"out of source", func(f *m03FindingRow) { n := int64(7); f.SpanEnd = &n }},
		{"wrong source binding", func(f *m03FindingRow) { f.SourceEpoch = "other" }},
		{"invalid expected", func(f *m03FindingRow) { s := " \t"; f.ExpectedText = &s }},
	}
	for _, tc := range mutations {
		t.Run(tc.name, func(t *testing.T) {
			copy := f
			tc.change(&copy)
			if err := m03VerifyFindingRow(copy, basic, m03FindingRowProof{}, map[string]bool{}); err == nil {
				t.Fatal("invalid finding accepted")
			}
		})
	}
	seen := map[string]bool{}
	if err := m03VerifyFindingRow(f, basic, m03FindingRowProof{}, seen); err != nil {
		t.Fatal(err)
	}
	if err := m03VerifyFindingRow(f, basic, m03FindingRowProof{}, seen); err == nil {
		t.Fatal("duplicate anchor accepted")
	}
	second := f
	start, end, occurrence := int64(4), int64(6), int64(2)
	excerpt := m03StringHash("ab")
	second.FindingID = "second"
	second.SpanStart = &start
	second.SpanEnd = &end
	second.Occurrence = &occurrence
	second.ExcerptHash = &excerpt
	if err := m03VerifyFindingRow(second, basic, m03FindingRowProof{}, seen); err != nil {
		t.Fatal(err)
	}
}

func TestM03FindingRowStatusEvidence(t *testing.T) {
	basic, f := m03FindingTestBase()
	proof := m03FindingRowProof{recheckCount: 1, eligible: map[string]bool{"finding": true},
		recheckEvidence: map[string]m03FindingRecheckEvidence{"finding": {targetID: "target", evidenceHash: m03StringHash("evidence"), resolved: true}}}
	f.status = "resolved"
	hash := proof.recheckEvidence["finding"].evidenceHash
	f.evidenceHash = &hash
	if err := m03VerifyFindingRow(f, basic, proof, map[string]bool{}); err != nil {
		t.Fatal(err)
	}
	missing := proof
	missing.recheckCount = 0
	if err := m03VerifyFindingRow(f, basic, missing, map[string]bool{}); err == nil {
		t.Fatal("resolved without recheck accepted")
	}
	copy := f
	copy.status = "unknown"
	if err := m03VerifyFindingRow(copy, basic, proof, map[string]bool{}); err == nil {
		t.Fatal("incorrect recheck status accepted")
	}
	copy = f
	copy.status = "unresolved"
	if err := m03VerifyFindingRow(copy, basic, proof, map[string]bool{}); err == nil {
		t.Fatal("resolved evidence marked unresolved")
	}
	proof.unshown = map[string]bool{"finding": true}
	copy = f
	copy.status = "unknown"
	copy.evidenceHash = nil
	if err := m03VerifyFindingRow(copy, basic, proof, map[string]bool{}); err != nil {
		t.Fatal(err)
	}
}

func TestM03FindingRowWaiverAndUnverified(t *testing.T) {
	basic, f := m03FindingTestBase()
	f.SpanStart = nil
	f.SpanEnd = nil
	f.SpanUnit = nil
	f.ExcerptHash = nil
	f.Occurrence = nil
	f.TargetID = nil
	f.status = "unverified"
	if err := m03VerifyFindingRow(f, basic, m03FindingRowProof{}, map[string]bool{}); err != nil {
		t.Fatal(err)
	}
	index := int64(0)
	hash := m03StringHash("confirmation")
	f.status = "author-waived"
	f.evidenceHash = &hash
	f.confirmationItemIndex = &index
	proof := m03FindingRowProof{confirmation: &m03FindingConfirmation{contentHash: hash, cycleID: "cycle", schemaVersion: 2,
		items: []m03FindingConfirmationItem{{findingID: "finding", decision: "waive"}}}}
	if err := m03VerifyFindingRow(f, basic, proof, map[string]bool{}); err != nil {
		t.Fatal(err)
	}
	copy := f
	wrong := int64(1)
	copy.confirmationItemIndex = &wrong
	if err := m03VerifyFindingRow(copy, basic, proof, map[string]bool{}); err == nil {
		t.Fatal("missing waiver item accepted")
	}
	copy = f
	copy.status = "unverified"
	if err := m03VerifyFindingRow(copy, basic, proof, map[string]bool{}); err == nil {
		t.Fatal("unverified with waiver evidence accepted")
	}
}
