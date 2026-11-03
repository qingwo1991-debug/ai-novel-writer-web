package admission

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"unicode/utf16"
	"unicode/utf8"
)

// m03FindingRowProof contains the independent receipt facts established by
// confirmation and recheck verification. An empty proof is appropriate only
// for a cycle with neither confirmation nor recheck.
type m03FindingRowProof struct {
	recheckCount      int64
	eligible, unshown map[string]bool
	recheckEvidence   map[string]m03FindingRecheckEvidence
	confirmation      *m03FindingConfirmation
}

type m03FindingRecheckEvidence struct {
	targetID, evidenceHash string
	resolved               bool
}

type m03FindingConfirmation struct {
	contentHash, cycleID string
	schemaVersion        int64
	items                []m03FindingConfirmationItem
}

type m03FindingConfirmationItem struct {
	findingID, decision string
}

type m03FindingRow struct {
	M03FindingIdentity
	status                string
	evidenceHash          *string
	confirmationItemIndex *int64
}

// m03VerifyFindingRows mirrors the final per-finding loop in the original
// M03 verifier. The caller must first prove the cycle, review effect,
// confirmation snapshot, recheck receipt, and immutable finding-set hash.
func m03VerifyFindingRows(ctx context.Context, tx *sql.Tx, basic m03CycleBasicEvidence, proof m03FindingRowProof) error {
	rows, err := tx.QueryContext(ctx, `SELECT finding_id,source_project_id,source_epoch,source_id,source_revision,
		source_content_hash,span_start,span_end,span_unit,excerpt_hash,occurrence,
		category,target_id,kind,problem_text,expected_text,status,evidence_hash,confirmation_item_index
		FROM review_findings WHERE cycle_id=? ORDER BY finding_id`, basic.cycleID)
	if err != nil {
		return err
	}
	defer rows.Close()
	seen := make(map[string]bool)
	for rows.Next() {
		var f m03FindingRow
		var start, end, occurrence, item sql.NullInt64
		var unit, excerpt, target, expected, evidence sql.NullString
		if err := rows.Scan(&f.FindingID, &f.SourceProjectID, &f.SourceEpoch, &f.SourceID, &f.SourceRevision,
			&f.SourceContentHash, &start, &end, &unit, &excerpt, &occurrence, &f.Category, &target,
			&f.Kind, &f.ProblemText, &expected, &f.status, &evidence, &item); err != nil {
			return err
		}
		if start.Valid {
			f.SpanStart = &start.Int64
		}
		if end.Valid {
			f.SpanEnd = &end.Int64
		}
		if unit.Valid {
			f.SpanUnit = &unit.String
		}
		if excerpt.Valid {
			f.ExcerptHash = &excerpt.String
		}
		if occurrence.Valid {
			f.Occurrence = &occurrence.Int64
		}
		if target.Valid {
			f.TargetID = &target.String
		}
		if expected.Valid {
			f.ExpectedText = &expected.String
		}
		if evidence.Valid {
			f.evidenceHash = &evidence.String
		}
		if item.Valid {
			f.confirmationItemIndex = &item.Int64
		}
		if err := m03VerifyFindingRow(f, basic, proof, seen); err != nil {
			return err
		}
	}
	return rows.Err()
}

func m03VerifyFindingRow(f m03FindingRow, basic m03CycleBasicEvidence, proof m03FindingRowProof, seen map[string]bool) error {
	if !m03FindingStringsValid(f) || !m03Nonempty(f.FindingID) || !m03Nonempty(f.SourceProjectID) ||
		!m03Nonempty(f.SourceEpoch) || !m03Nonempty(f.SourceID) || !m03SafeInteger(f.SourceRevision, 0) ||
		!m03Hash(f.SourceContentHash) || f.SourceProjectID != basic.projectID || f.SourceEpoch != basic.epoch ||
		f.SourceID != fmt.Sprintf("draft:%d", basic.baseDraftID) || f.SourceRevision != basic.sourceVersion ||
		f.SourceContentHash != basic.sourceHash || !m03Nonempty(f.Category) || !m03Nonempty(f.ProblemText) ||
		f.ExpectedText != nil && !m03Nonempty(*f.ExpectedText) ||
		(f.Kind != "objective" && f.Kind != "literary") ||
		(f.status != "unverified" && f.status != "unresolved" && f.status != "unknown" && f.status != "resolved" && f.status != "author-waived") {
		return errors.New("M03 finding identity invalid")
	}
	emptyAnchor := f.SpanStart == nil && f.SpanEnd == nil && f.SpanUnit == nil && f.ExcerptHash == nil && f.Occurrence == nil && f.TargetID == nil
	if f.status == "unverified" || f.status == "author-waived" && emptyAnchor {
		if !emptyAnchor || f.status == "unverified" && (f.evidenceHash != nil || f.confirmationItemIndex != nil) {
			return errors.New("M03 unverified finding anchor invalid")
		}
	} else {
		if f.SpanStart == nil || f.SpanEnd == nil || f.SpanUnit == nil || f.ExcerptHash == nil || f.Occurrence == nil || f.TargetID == nil ||
			!m03SafeInteger(*f.SpanStart, 0) || !m03SafeInteger(*f.SpanEnd, 1) || *f.SpanEnd <= *f.SpanStart ||
			*f.SpanUnit != "utf16-code-unit" || !m03Hash(*f.ExcerptHash) || !m03SafeInteger(*f.Occurrence, 1) ||
			!m03Nonempty(*f.TargetID) {
			return errors.New("M03 finding anchor invalid")
		}
		source := utf16.Encode([]rune(basic.sourceContent))
		if *f.SpanEnd > int64(len(source)) {
			return errors.New("M03 finding span outside source")
		}
		excerptUnits := source[*f.SpanStart:*f.SpanEnd]
		if m03StringHash(string(utf16.Decode(excerptUnits))) != *f.ExcerptHash {
			return errors.New("M03 finding excerpt hash differs")
		}
		anchor := m03FindingAnchorKey(f)
		if seen[anchor] {
			return errors.New("M03 duplicate finding anchor")
		}
		seen[anchor] = true
		var count int64
		for offset := 0; offset <= int(*f.SpanStart); {
			found := m03FindUTF16(source, excerptUnits, offset)
			if found < 0 || int64(found) > *f.SpanStart {
				break
			}
			count++
			offset = found + len(excerptUnits)
		}
		if count != *f.Occurrence {
			return errors.New("M03 finding occurrence differs")
		}
	}
	if f.evidenceHash != nil && !m03Hash(*f.evidenceHash) {
		return errors.New("M03 finding evidence hash invalid")
	}
	if proof.eligible[f.FindingID] && !proof.unshown[f.FindingID] {
		evidence, has := proof.recheckEvidence[f.FindingID]
		if has && (f.TargetID == nil || evidence.targetID != *f.TargetID) {
			has = false
		}
		status := "unknown"
		if has {
			if evidence.resolved {
				status = "resolved"
			} else {
				status = "unresolved"
			}
		}
		if f.status != status || has && (f.evidenceHash == nil || *f.evidenceHash != evidence.evidenceHash) || !has && f.evidenceHash != nil {
			return errors.New("M03 finding recheck status differs")
		}
	}
	if f.status == "resolved" {
		evidence, has := proof.recheckEvidence[f.FindingID]
		if proof.recheckCount != 1 || f.evidenceHash == nil || !has || !evidence.resolved || f.TargetID == nil ||
			evidence.targetID != *f.TargetID || evidence.evidenceHash != *f.evidenceHash {
			return errors.New("M03 resolved finding lacks recheck evidence")
		}
	}
	if f.status == "author-waived" {
		confirmation := proof.confirmation
		if confirmation == nil || f.evidenceHash == nil || *f.evidenceHash != confirmation.contentHash ||
			f.confirmationItemIndex == nil || !m03SafeInteger(*f.confirmationItemIndex, 0) ||
			*f.confirmationItemIndex >= int64(len(confirmation.items)) || confirmation.schemaVersion != 2 ||
			confirmation.cycleID != basic.cycleID {
			return errors.New("M03 waived finding lacks confirmation")
		}
		item := confirmation.items[*f.confirmationItemIndex]
		if item.findingID != f.FindingID || item.decision != "waive" {
			return errors.New("M03 waiver item differs")
		}
	} else if f.confirmationItemIndex != nil {
		return errors.New("M03 finding has orphan confirmation item")
	}
	return nil
}

func m03FindingStringsValid(f m03FindingRow) bool {
	values := []string{f.FindingID, f.SourceProjectID, f.SourceEpoch, f.SourceID, f.SourceContentHash, f.Category, f.Kind, f.ProblemText, f.status}
	for _, value := range values {
		if !utf8.ValidString(value) {
			return false
		}
	}
	for _, value := range []*string{f.SpanUnit, f.ExcerptHash, f.TargetID, f.ExpectedText, f.evidenceHash} {
		if value != nil && !utf8.ValidString(*value) {
			return false
		}
	}
	return true
}

func m03FindingAnchorKey(f m03FindingRow) string {
	encoded := []byte{'['}
	encoded = appendJSString(encoded, f.SourceContentHash)
	encoded = append(encoded, ",{\"start\":"...)
	encoded = append(encoded, fmt.Sprint(*f.SpanStart)...)
	encoded = append(encoded, ",\"end\":"...)
	encoded = append(encoded, fmt.Sprint(*f.SpanEnd)...)
	encoded = append(encoded, ",\"unit\":"...)
	encoded = appendJSString(encoded, *f.SpanUnit)
	encoded = append(encoded, "},"...)
	encoded = append(encoded, fmt.Sprint(*f.Occurrence)...)
	encoded = append(encoded, ',')
	encoded = appendJSString(encoded, *f.ExcerptHash)
	encoded = append(encoded, ',')
	encoded = appendJSString(encoded, f.Category)
	encoded = append(encoded, ',')
	encoded = appendJSString(encoded, *f.TargetID)
	return string(append(encoded, ']'))
}

func m03FindUTF16(source, needle []uint16, from int) int {
	for i := from; i+len(needle) <= len(source); i++ {
		match := true
		for j := range needle {
			if source[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}
