package archiveformat

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"strings"
	"testing"
)

func exportFixture(t *testing.T) ExportSnapshot {
	t.Helper()
	origin := "123e4567-e89b-42d3-a456-426614174000"
	generation := "generation-1"
	database := []byte("verified schema-7 private snapshot")
	fingerprint := sha256Hex([]byte("schema"))
	freeze := []byte(`{"version":1,"originProjectId":"` + origin + `","snapshotGeneration":"` + generation + `","nonReplayable":true,"requiresRuntimeFreezeGuard":true,"records":[],"avatarReferenceProjections":[]}`)
	databaseHash := sha256Hex(database)
	payload := struct {
		OriginProjectID        string                   `json:"originProjectId"`
		SnapshotGeneration     string                   `json:"snapshotGeneration"`
		PortableDatabaseSHA256 string                   `json:"portableDatabaseSha256"`
		Finalizations          []FinalizationAuthority  `json:"finalizations"`
		SummarySources         []SummarySourceAuthority `json:"summarySources"`
	}{origin, generation, databaseHash, []FinalizationAuthority{}, []SummarySourceAuthority{}}
	encoded, err := marshalJS(payload)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(encoded)
	receiptID := "portable-transfer:" + hex.EncodeToString(digest[:])[:32]
	authority := TransferAuthority{1, receiptID, origin, nil, generation, 7, databaseHash, true, []FinalizationAuthority{}, []SummarySourceAuthority{}}
	transfer, err := marshalJS(authority)
	if err != nil {
		t.Fatal(err)
	}
	values := []struct {
		path, disposition string
		bytes             []byte
	}{
		{"project.db", "portable-database", database},
		{".ai-novel/portable-transfer-authority.json", "transfer-receipt", transfer},
		{"portable-runtime-freeze.json", "history-projection", freeze},
	}
	var sources []ExportSource
	var entries []Entry
	var total uint64
	for _, value := range values {
		entry := Entry{value.path, uint64(len(value.bytes)), sha256Hex(value.bytes), value.disposition}
		content := append([]byte(nil), value.bytes...)
		entries = append(entries, entry)
		sources = append(sources, ExportSource{entry, func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(content)), nil }})
		total += uint64(len(content))
	}
	m := Manifest{1, 7, origin, generation, "2026-11-03T00:00:00.000Z", total, total, entries, map[string]uint64{"table.project_core": 1}, []OmittedItem{}, []string{receiptID}, []string{}}
	return ExportSnapshot{m, sources, freeze, transfer, SourceEvidence{7, fingerprint, 1, 1}, func(context.Context) error { return nil }, func(context.Context) error { return nil }}
}

func TestExportPrivateCompleteAndVerified(t *testing.T) {
	snapshot := exportFixture(t)
	current, unchanged := 0, 0
	snapshot.CheckCurrent = func(context.Context) error { current++; return nil }
	snapshot.VerifyUnchanged = func(context.Context) error { unchanged++; return nil }
	var staged bytes.Buffer
	receipt, err := ExportPrivate(context.Background(), &staged, snapshot, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if current != 2 || unchanged != 2 || !receipt.RequiresRuntimeFreezeGuard || receipt.TargetByteSize != uint64(staged.Len()) || receipt.EntryCount != 3 || receipt.TargetSHA256 != sha256Hex(staged.Bytes()) {
		t.Fatalf("receipt/proofs: %+v %d %d", receipt, current, unchanged)
	}
	if _, err := Inspect(bytes.NewReader(staged.Bytes()), int64(staged.Len()), Limits{}); err != nil {
		t.Fatalf("built archive: %v", err)
	}
}

func TestExportPrivateRejectsMissingAuthorityBeforeWriting(t *testing.T) {
	snapshot := exportFixture(t)
	snapshot.VerifyUnchanged = nil
	var staged bytes.Buffer
	_, err := ExportPrivate(context.Background(), &staged, snapshot, Limits{})
	if Code(err) != "PORTABLE_EXPORT_AUTHORITY_REQUIRED" || staged.Len() != 0 {
		t.Fatalf("%v, bytes=%d", err, staged.Len())
	}
	snapshot = exportFixture(t)
	snapshot.RuntimeFreeze = []byte("changed")
	_, err = ExportPrivate(context.Background(), &staged, snapshot, Limits{})
	if err == nil || staged.Len() != 0 {
		t.Fatalf("sidecar accepted: %v", err)
	}
}

func TestExportPrivateRejectsSourceChangeAndRequiresDiscard(t *testing.T) {
	snapshot := exportFixture(t)
	calls := 0
	snapshot.VerifyUnchanged = func(context.Context) error {
		calls++
		if calls == 2 {
			return io.ErrUnexpectedEOF
		}
		return nil
	}
	var staged bytes.Buffer
	_, err := ExportPrivate(context.Background(), &staged, snapshot, Limits{})
	if Code(err) != "PORTABLE_SOURCE_CHANGED" || staged.Len() == 0 {
		t.Fatalf("%v, bytes=%d", err, staged.Len())
	}
	snapshot = exportFixture(t)
	snapshot.Sources[0].Open = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("changed")), nil }
	staged.Reset()
	_, err = ExportPrivate(context.Background(), &staged, snapshot, Limits{})
	if Code(err) != "PORTABLE_ARCHIVE_SOURCE_CHANGED" {
		t.Fatalf("mutated stream: %v", err)
	}
}
