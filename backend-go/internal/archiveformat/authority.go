package archiveformat

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"unicode/utf16"
)

const MaxRuntimeHistoryBytes = 16 * 1024 * 1024
const MaxTransferAuthorityBytes = 4 * 1024 * 1024

var uuidPattern = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
var authorityIDPattern = regexp.MustCompile(`^[\p{L}\p{N}._:@+\-]+$`)
var tablePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,127}$`)

func authorityID(v any) (string, bool) {
	s, ok := v.(string)
	return s, ok && authorityIDPattern.MatchString(s) && len(utf16.Encode([]rune(s))) <= 512
}
func authorityInteger(v any, min uint64) (uint64, bool) {
	n, e := integer(v, maxSafeInteger)
	return n, e == nil && n >= min
}

// FreezeGuard records historical non-replayable rows. Active is not evidence of
// a verified current database receipt. Never use this codec to authorize replay.
type FreezeGuard struct {
	active     bool
	origin     string
	generation string
	frozen     map[string]bool
}

func (g *FreezeGuard) Active() bool { return g != nil && g.active }
func (g *FreezeGuard) IsFrozen(table, recordID string) bool {
	return g != nil && g.frozen[table+"\x00"+recordID]
}
func (g *FreezeGuard) AssertMutable(table, recordID string) error {
	if g.IsFrozen(table, recordID) {
		return failure("PORTABLE_RUNTIME_FROZEN")
	}
	return nil
}
func ParseRuntimeFreeze(data []byte) (*FreezeGuard, error) {
	bad := func() (*FreezeGuard, error) { return nil, failure("PORTABLE_RUNTIME_FREEZE_INVALID") }
	if len(data) > MaxRuntimeHistoryBytes {
		return bad()
	}
	v, e := decodeJSON(data)
	if e != nil {
		return bad()
	}
	m, e := object(v, "version", "originProjectId", "snapshotGeneration", "nonReplayable", "requiresRuntimeFreezeGuard", "records", "avatarReferenceProjections")
	if e != nil {
		return bad()
	}
	version, ok := authorityInteger(m["version"], 1)
	origin, _ := m["originProjectId"].(string)
	generation, gok := authorityID(m["snapshotGeneration"])
	rows, rok := m["records"].([]any)
	avatars, aok := m["avatarReferenceProjections"].([]any)
	if !ok || version != 1 || !uuidPattern.MatchString(origin) || !gok || m["nonReplayable"] != true || m["requiresRuntimeFreezeGuard"] != true || !rok || !aok || len(rows) > 100000 || len(avatars) > 100000 {
		return bad()
	}
	for _, v := range avatars {
		if _, e := object(v); e != nil {
			return bad()
		}
	}
	g := &FreezeGuard{active: true, origin: origin, generation: generation, frozen: map[string]bool{}}
	for _, v := range rows {
		r, e := object(v, "projectionId", "table", "recordId", "terminalState", "projection", "projectionHash", "excludedFields", "nonReplayable", "originalReceiptVerified")
		if e != nil {
			return bad()
		}
		_, pid := authorityID(r["projectionId"])
		table, tok := r["table"].(string)
		id, iok := authorityID(r["recordId"])
		_, terminal := authorityID(r["terminalState"])
		_, pe := object(r["projection"])
		hash, _ := r["projectionHash"].(string)
		excluded, xok := r["excludedFields"].([]any)
		if !pid || !tok || !tablePattern.MatchString(table) || !iok || !terminal || pe != nil || !hashPattern.MatchString(hash) || !xok || r["nonReplayable"] != true || r["originalReceiptVerified"] != false {
			return bad()
		}
		for _, x := range excluded {
			if _, ok := authorityID(x); !ok {
				return bad()
			}
		}
		key := table + "\x00" + id
		if g.frozen[key] {
			return bad()
		}
		g.frozen[key] = true
	}
	return g, nil
}

type FinalizationAuthority struct {
	FinalizationID string `json:"finalizationId"`
	DraftID        uint64 `json:"draftId"`
	ChapterNumber  uint64 `json:"chapterNumber"`
	ContentHash    string `json:"contentHash"`
}
type SummarySourceAuthority struct {
	SummaryID            uint64 `json:"summaryId"`
	DraftID              uint64 `json:"draftId"`
	ChapterNumber        uint64 `json:"chapterNumber"`
	SourceFinalizationID string `json:"sourceFinalizationId"`
	SourceContentHash    string `json:"sourceContentHash"`
	ProjectionGeneration uint64 `json:"projectionGeneration"`
}

// TransferAuthority contains only source-bound portable facts, not original
// machine authority. Parsing verifies the portable receipt ID, not database
// currentness, schema, target mapping or runtime installation. Those gates are
// the future restoration owner's responsibility before publication.
type TransferAuthority struct {
	Version                    int                      `json:"version"`
	ReceiptID                  string                   `json:"receiptId"`
	OriginProjectID            string                   `json:"originProjectId"`
	TargetProjectID            *string                  `json:"targetProjectId"`
	SnapshotGeneration         string                   `json:"snapshotGeneration"`
	SourceSchemaVersion        int                      `json:"sourceSchemaVersion"`
	PortableDatabaseSHA256     string                   `json:"portableDatabaseSha256"`
	RequiresRuntimeFreezeGuard bool                     `json:"requiresRuntimeFreezeGuard"`
	Finalizations              []FinalizationAuthority  `json:"finalizations"`
	SummarySources             []SummarySourceAuthority `json:"summarySources"`
}

func ParseTransferAuthority(data []byte) (TransferAuthority, error) {
	var out TransferAuthority
	bad := func() (TransferAuthority, error) {
		return TransferAuthority{}, failure("PORTABLE_TRANSFER_AUTHORITY_INVALID")
	}
	if len(data) > MaxTransferAuthorityBytes {
		return bad()
	}
	v, e := decodeJSON(data)
	if e != nil {
		return bad()
	}
	m, e := object(v, "version", "receiptId", "originProjectId", "targetProjectId", "snapshotGeneration", "sourceSchemaVersion", "portableDatabaseSha256", "requiresRuntimeFreezeGuard", "finalizations", "summarySources")
	if e != nil {
		return bad()
	}
	version, vok := authorityInteger(m["version"], 1)
	schema, sok := authorityInteger(m["sourceSchemaVersion"], 1)
	origin, _ := m["originProjectId"].(string)
	generation, gok := authorityID(m["snapshotGeneration"])
	receipt, rok := authorityID(m["receiptId"])
	hash, _ := m["portableDatabaseSha256"].(string)
	finals, fok := m["finalizations"].([]any)
	summaries, smok := m["summarySources"].([]any)
	if !vok || version != 1 || !sok || schema != 7 || !uuidPattern.MatchString(origin) || !gok || !rok || !hashPattern.MatchString(hash) || m["requiresRuntimeFreezeGuard"] != true || !fok || !smok || len(finals) > 100000 || len(summaries) > 100000 {
		return bad()
	}
	var target *string
	if m["targetProjectId"] != nil {
		s, ok := m["targetProjectId"].(string)
		if !ok || !uuidPattern.MatchString(s) {
			return bad()
		}
		target = &s
	}
	out = TransferAuthority{Version: 1, ReceiptID: receipt, OriginProjectID: origin, TargetProjectID: target, SnapshotGeneration: generation, SourceSchemaVersion: 7, PortableDatabaseSHA256: hash, RequiresRuntimeFreezeGuard: true, Finalizations: make([]FinalizationAuthority, 0, len(finals)), SummarySources: make([]SummarySourceAuthority, 0, len(summaries))}
	byID := map[string]FinalizationAuthority{}
	drafts := map[uint64]bool{}
	chapters := map[uint64]bool{}
	for _, v := range finals {
		r, e := object(v, "finalizationId", "draftId", "chapterNumber", "contentHash")
		if e != nil {
			return bad()
		}
		id, iok := authorityID(r["finalizationId"])
		draft, dok := authorityInteger(r["draftId"], 1)
		chapter, cok := authorityInteger(r["chapterNumber"], 1)
		hash, _ := r["contentHash"].(string)
		if !iok || !dok || !cok || !hashPattern.MatchString(hash) || drafts[draft] || chapters[chapter] {
			return bad()
		}
		if _, ok := byID[id]; ok {
			return bad()
		}
		f := FinalizationAuthority{id, draft, chapter, hash}
		out.Finalizations = append(out.Finalizations, f)
		byID[id] = f
		drafts[draft] = true
		chapters[chapter] = true
	}
	summaryIDs := map[uint64]bool{}
	for _, v := range summaries {
		r, e := object(v, "summaryId", "draftId", "chapterNumber", "sourceFinalizationId", "sourceContentHash", "projectionGeneration")
		if e != nil {
			return bad()
		}
		summary, siok := authorityInteger(r["summaryId"], 1)
		draft, dok := authorityInteger(r["draftId"], 1)
		chapter, cok := authorityInteger(r["chapterNumber"], 1)
		id, iok := authorityID(r["sourceFinalizationId"])
		hash, _ := r["sourceContentHash"].(string)
		generation, gok := authorityInteger(r["projectionGeneration"], 0)
		f, exists := byID[id]
		if !siok || !dok || !cok || !iok || !gok || !hashPattern.MatchString(hash) || summaryIDs[summary] || !exists || f.DraftID != draft || f.ChapterNumber != chapter || f.ContentHash != hash {
			return bad()
		}
		summaryIDs[summary] = true
		out.SummarySources = append(out.SummarySources, SummarySourceAuthority{summary, draft, chapter, id, hash, generation})
	}
	payload := struct {
		OriginProjectID        string                   `json:"originProjectId"`
		SnapshotGeneration     string                   `json:"snapshotGeneration"`
		PortableDatabaseSHA256 string                   `json:"portableDatabaseSha256"`
		Finalizations          []FinalizationAuthority  `json:"finalizations"`
		SummarySources         []SummarySourceAuthority `json:"summarySources"`
	}{out.OriginProjectID, out.SnapshotGeneration, out.PortableDatabaseSHA256, out.Finalizations, out.SummarySources}
	encoded, e := marshalJS(payload)
	if e != nil {
		return bad()
	}
	digest := sha256.Sum256(encoded)
	expected := "portable-transfer:" + hex.EncodeToString(digest[:])[:32]
	if out.ReceiptID != expected {
		return bad()
	}
	return out, nil
}

// ValidatePortableBoundary is an optional semantic gate above the raw ZIP
// codec. Authority requires a valid matching freeze sidecar. It never upgrades
// historical projections into receipted/replayable work. Database hash binding
// here is not a replacement for future SQL currentness/history verification.
func ValidatePortableBoundary(manifest Manifest, freezeBytes, authorityBytes []byte) (*FreezeGuard, *TransferAuthority, error) {
	if len(freezeBytes) == 0 && len(authorityBytes) == 0 {
		return &FreezeGuard{}, nil, nil
	}
	if len(freezeBytes) == 0 {
		return nil, nil, failure("PORTABLE_RUNTIME_FREEZE_INVALID")
	}
	g, e := ParseRuntimeFreeze(freezeBytes)
	if e != nil {
		return nil, nil, e
	}
	if g.origin != manifest.OriginProjectID || g.generation != manifest.SnapshotGeneration {
		return nil, nil, failure("PORTABLE_RUNTIME_FREEZE_INVALID")
	}
	if len(authorityBytes) == 0 {
		return g, nil, nil
	}
	a, e := ParseTransferAuthority(authorityBytes)
	if e != nil {
		return nil, nil, e
	}
	if a.OriginProjectID != manifest.OriginProjectID || a.SnapshotGeneration != manifest.SnapshotGeneration || a.TargetProjectID != nil {
		return nil, nil, failure("PORTABLE_TRANSFER_AUTHORITY_INVALID")
	}
	bound := false
	for _, entry := range manifest.Entries {
		if entry.Disposition == "portable-database" && entry.SHA256 == a.PortableDatabaseSHA256 {
			bound = true
		}
	}
	if !bound {
		return nil, nil, failure("PORTABLE_TRANSFER_AUTHORITY_INVALID")
	}
	return g, &a, nil
}
