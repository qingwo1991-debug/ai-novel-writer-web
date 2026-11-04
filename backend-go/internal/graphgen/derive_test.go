package graphgen

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

const plotTreeGeneratedAt = "2026-09-13T00:00:00.000Z"

// plotTreeTestSources mirrors the fixture of
// src/services/__tests__/plot-tree-generator.test.ts:66-104.
func plotTreeTestSources() PlotTreeSourceBundle {
	return PlotTreeSourceBundle{
		WritingLanguage: WritingLanguageEnUS,
		Synopsis: PlotTreeSynopsisSource{
			Content: "A clerk investigates a ledger that vanished from a sealed safe.",
		},
		Blueprints: []PlotTreeBlueprintSource{{
			ChapterNumber: 1,
			Title:         "The empty safe",
			Purpose:       "Launch the investigation",
			KeyEvents:     "The clerk discovers the missing ledger.",
		}},
		FinalizedChapters: []PlotTreeFinalizedChapterSource{{
			DraftID:       41,
			ChapterNumber: 1,
			Title:         "The empty safe",
			Summary:       "The ledger is missing and the clerk preserves the broken seal.",
		}},
		NarrativeThreads: []PlotTreeNarrativeThreadSource{{
			ID:                 7,
			Title:              "Missing ledger",
			Type:               "mystery",
			TargetStartChapter: 1,
			TargetEndChapter:   5,
			AuthorIntent:       "Reveal the forger in chapter five.",
			Status:             "planted",
			Events: []PlotTreeNarrativeThreadEventSource{{
				ID:            11,
				ChapterNumber: 1,
				Type:          "planted",
				Evidence:      "the ledger was gone",
				Reason:        "The finalized chapter plants the mystery.",
			}},
		}},
		SourceRevision: strings.Repeat("0", 64),
	}
}

// modelPlotTreeResponse mirrors `modelResponse` in
// src/services/__tests__/plot-tree-generator.test.ts:106-129.
const modelPlotTreeResponse = `{"tracks":[{"id":"missing-ledger",` +
	`"title":"Missing ledger investigation","role":"main","startChapter":1,"endChapter":5,` +
	`"summary":"The clerk follows the missing ledger to its forger.","events":[` +
	`{"status":"planned","chapterNumber":1,"summary":"The investigation begins.",` +
	`"sources":[{"type":"blueprint","chapterNumber":1}]},` +
	`{"status":"occurred","chapterNumber":1,"summary":"The disappearance is confirmed.",` +
	`"sources":[{"type":"finalized-chapter","draftId":41,"chapterNumber":1}]},` +
	`{"status":"occurred","chapterNumber":1,"summary":"The mystery thread is planted.",` +
	`"sources":[{"type":"narrative-thread","planId":7,"eventId":11,"chapterNumber":1}]}]}]}`

func testModelSnapshot() PlotTreeSnapshot {
	revision := strings.Repeat("0", 64)
	return PlotTreeSnapshot{
		Version:         1,
		GeneratedAt:     plotTreeGeneratedAt,
		WritingLanguage: WritingLanguageEnUS,
		SourceRevision:  &revision,
		Tracks: []PlotTreeTrack{{
			ID:           "missing-ledger",
			Title:        "Missing ledger investigation",
			Role:         PlotTreeRoleMain,
			StartChapter: 1,
			EndChapter:   5,
			Summary:      "The clerk follows the missing ledger to its forger.",
			Events: []PlotTreeEvent{
				{
					Status:        PlotTreeEventStatusPlanned,
					ChapterNumber: 1,
					Summary:       "The investigation begins.",
					Sources: []PlotTreeSourceReference{{
						Type:          PlotTreeReferenceBlueprint,
						ChapterNumber: intPointer(1),
					}},
				},
				{
					Status:        PlotTreeEventStatusOccurred,
					ChapterNumber: 1,
					Summary:       "The disappearance is confirmed.",
					Sources: []PlotTreeSourceReference{{
						Type:          PlotTreeReferenceFinalizedChapter,
						DraftID:       intPointer(41),
						ChapterNumber: intPointer(1),
					}},
				},
				{
					Status:        PlotTreeEventStatusOccurred,
					ChapterNumber: 1,
					Summary:       "The mystery thread is planted.",
					Sources: []PlotTreeSourceReference{{
						Type:          PlotTreeReferenceNarrativeThread,
						PlanID:        intPointer(7),
						EventID:       intPointer(11),
						ChapterNumber: intPointer(1),
					}},
				},
			},
		}},
	}
}

func TestDerivePlotTreeSnapshotAcceptsModelOutput(t *testing.T) {
	derivation, err := DerivePlotTreeSnapshot(modelPlotTreeResponse, plotTreeTestSources(), plotTreeGeneratedAt)
	if err != nil {
		t.Fatalf("DerivePlotTreeSnapshot returned %v", err)
	}
	if derivation.Kind != PlotTreeDerivationModel {
		t.Fatalf("kind = %q, want %q", derivation.Kind, PlotTreeDerivationModel)
	}
	if want := testModelSnapshot(); !reflect.DeepEqual(derivation.Snapshot, want) {
		t.Fatalf("snapshot mismatch\n got: %+v\nwant: %+v", derivation.Snapshot, want)
	}
}

// TestDerivePlotTreeSnapshotUnwrapsFence pins parsePlotTreeJSON
// (plot-tree-generation-pure.ts:141-145).
func TestDerivePlotTreeSnapshotUnwrapsFence(t *testing.T) {
	content := "```json\n" + modelPlotTreeResponse + "\n```"
	derivation, err := DerivePlotTreeSnapshot(content, plotTreeTestSources(), plotTreeGeneratedAt)
	if err != nil {
		t.Fatalf("DerivePlotTreeSnapshot returned %v", err)
	}
	if derivation.Kind != PlotTreeDerivationModel {
		t.Fatalf("kind = %q, want %q", derivation.Kind, PlotTreeDerivationModel)
	}
}

// TestDerivePlotTreeSnapshotNormalizesOutOfDomainRange pins
// normalizePlotTreeSnapshot (plot-tree-generation-pure.ts:171-200): the model's
// invented 999 chapter range is replaced by the range of its surviving events.
func TestDerivePlotTreeSnapshotNormalizesOutOfDomainRange(t *testing.T) {
	content := strings.Replace(modelPlotTreeResponse, `"startChapter":1`, `"startChapter":999`, 1)
	derivation, err := DerivePlotTreeSnapshot(content, plotTreeTestSources(), plotTreeGeneratedAt)
	if err != nil {
		t.Fatalf("DerivePlotTreeSnapshot returned %v", err)
	}
	if derivation.Kind != PlotTreeDerivationNormalized {
		t.Fatalf("kind = %q, want %q", derivation.Kind, PlotTreeDerivationNormalized)
	}
	want := testModelSnapshot()
	want.Tracks[0].StartChapter = 1
	want.Tracks[0].EndChapter = 1
	if !reflect.DeepEqual(derivation.Snapshot, want) {
		t.Fatalf("snapshot mismatch\n got: %+v\nwant: %+v", derivation.Snapshot, want)
	}
}

// TestDerivePlotTreeSnapshotFallsBackToSources pins
// deterministicPlotTreeSnapshot (plot-tree-generation-pure.ts:202-264) for
// unparseable output: finalized chapters, then blueprints that are not
// finalized, then narrative threads, de-duplicated and sorted by
// chapterNumber, then status.
func TestDerivePlotTreeSnapshotFallsBackToSources(t *testing.T) {
	derivation, err := DerivePlotTreeSnapshot("not JSON", plotTreeTestSources(), plotTreeGeneratedAt)
	if err != nil {
		t.Fatalf("DerivePlotTreeSnapshot returned %v", err)
	}
	if derivation.Kind != PlotTreeDerivationDeterministic {
		t.Fatalf("kind = %q, want %q", derivation.Kind, PlotTreeDerivationDeterministic)
	}
	revision := strings.Repeat("0", 64)
	want := PlotTreeSnapshot{
		Version:         1,
		GeneratedAt:     plotTreeGeneratedAt,
		WritingLanguage: WritingLanguageEnUS,
		SourceRevision:  &revision,
		Tracks: []PlotTreeTrack{{
			ID:           "source-backed-progress",
			Title:        "Plot progress",
			Role:         PlotTreeRoleMain,
			StartChapter: 1,
			EndChapter:   1,
			Summary:      "Read-only progress from current chapters and narrative threads.",
			Events: []PlotTreeEvent{
				{
					Status:        PlotTreeEventStatusOccurred,
					ChapterNumber: 1,
					Summary:       "The ledger is missing and the clerk preserves the broken seal.",
					Sources: []PlotTreeSourceReference{{
						Type:          PlotTreeReferenceFinalizedChapter,
						DraftID:       intPointer(41),
						ChapterNumber: intPointer(1),
					}},
				},
				{
					Status:        PlotTreeEventStatusOccurred,
					ChapterNumber: 1,
					Summary:       "the ledger was gone",
					Sources: []PlotTreeSourceReference{{
						Type:          PlotTreeReferenceNarrativeThread,
						PlanID:        intPointer(7),
						EventID:       intPointer(11),
						ChapterNumber: intPointer(1),
					}},
				},
				{
					Status:        PlotTreeEventStatusPlanned,
					ChapterNumber: 1,
					Summary:       "Reveal the forger in chapter five.",
					Sources: []PlotTreeSourceReference{{
						Type:   PlotTreeReferenceNarrativeThread,
						PlanID: intPointer(7),
					}},
				},
			},
		}},
	}
	if !reflect.DeepEqual(derivation.Snapshot, want) {
		t.Fatalf("snapshot mismatch\n got: %+v\nwant: %+v", derivation.Snapshot, want)
	}
}

// TestDerivePlotTreeSnapshotDeterministicZhCN pins the language-dependent track
// title and summary chosen by promptLanguageText (plot-tree-generation-pure.ts:253-261).
func TestDerivePlotTreeSnapshotDeterministicZhCN(t *testing.T) {
	sources := plotTreeTestSources()
	sources.WritingLanguage = WritingLanguageZhCN
	derivation, err := DerivePlotTreeSnapshot("not JSON", sources, plotTreeGeneratedAt)
	if err != nil {
		t.Fatalf("DerivePlotTreeSnapshot returned %v", err)
	}
	if derivation.Snapshot.Tracks[0].Title != "剧情进展" {
		t.Fatalf("track title = %q", derivation.Snapshot.Tracks[0].Title)
	}
	if derivation.Snapshot.Tracks[0].Summary != "基于当前章节与叙事线索的只读进展。" {
		t.Fatalf("track summary = %q", derivation.Snapshot.Tracks[0].Summary)
	}
}

// TestDerivePlotTreeSnapshotWithoutSources pins PlotTreeSourceError
// (plot-tree-generation-pure.ts:250).
func TestDerivePlotTreeSnapshotWithoutSources(t *testing.T) {
	sources := plotTreeTestSources()
	sources.Blueprints = nil
	sources.FinalizedChapters = nil
	sources.NarrativeThreads = nil
	if _, err := DerivePlotTreeSnapshot("not JSON", sources, plotTreeGeneratedAt); !errors.Is(err, ErrPlotTreeSource) {
		t.Fatalf("expected ErrPlotTreeSource, got %v", err)
	}
}

func newPlotTreeSnapshotValue(t *testing.T) map[string]any {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal([]byte(modelPlotTreeResponse), &value); err != nil {
		t.Fatalf("fixture is not JSON: %v", err)
	}
	value["version"] = float64(1)
	value["generatedAt"] = plotTreeGeneratedAt
	value["writingLanguage"] = WritingLanguageEnUS
	value["sourceRevision"] = strings.Repeat("0", 64)
	return value
}

func firstPlotTreeTrack(t *testing.T, value map[string]any) map[string]any {
	t.Helper()
	tracks, ok := jsonArray(value["tracks"])
	if !ok || len(tracks) == 0 {
		t.Fatalf("fixture has no tracks")
	}
	track, ok := jsonRecord(tracks[0])
	if !ok {
		t.Fatalf("fixture track is not an object")
	}
	return track
}

func minimalPlotTreeTrack(id, role, parentTrackID string) map[string]any {
	track := map[string]any{
		"id":           id,
		"title":        id,
		"role":         role,
		"startChapter": float64(1),
		"endChapter":   float64(1),
		"summary":      id,
		"events": []any{map[string]any{
			"status":        PlotTreeEventStatusOccurred,
			"chapterNumber": float64(1),
			"summary":       "e",
			"sources": []any{map[string]any{
				"type":          PlotTreeReferenceFinalizedChapter,
				"draftId":       float64(41),
				"chapterNumber": float64(1),
			}},
		}},
	}
	if parentTrackID != "" {
		track["parentTrackId"] = parentTrackID
	}
	return track
}

// TestAssertPlotTreeSnapshotErrors pins every validation message of
// src/shared/plot-tree.ts:274-395.
func TestAssertPlotTreeSnapshotErrors(t *testing.T) {
	sources := plotTreeTestSources()
	cases := []struct {
		name   string
		mutate func(t *testing.T) map[string]any
		want   error
	}{
		{
			name: "version",
			mutate: func(t *testing.T) map[string]any {
				value := newPlotTreeSnapshotValue(t)
				value["version"] = float64(2)
				return value
			},
			want: ErrPlotTreeSnapshotVersion,
		},
		{
			name: "unknown writing language",
			mutate: func(t *testing.T) map[string]any {
				value := newPlotTreeSnapshotValue(t)
				value["writingLanguage"] = "fr-FR"
				return value
			},
			want: ErrPlotTreeSnapshotVersion,
		},
		{
			name: "generatedAt is not a date",
			mutate: func(t *testing.T) map[string]any {
				value := newPlotTreeSnapshotValue(t)
				value["generatedAt"] = "not-a-date"
				return value
			},
			want: ErrPlotTreeGeneratedAt,
		},
		{
			name: "malformed source revision",
			mutate: func(t *testing.T) map[string]any {
				value := newPlotTreeSnapshotValue(t)
				value["sourceRevision"] = "abc"
				return value
			},
			want: ErrPlotTreeSourceRevision,
		},
		{
			name: "no tracks",
			mutate: func(t *testing.T) map[string]any {
				value := newPlotTreeSnapshotValue(t)
				value["tracks"] = []any{}
				return value
			},
			want: ErrPlotTreeTracks,
		},
		{
			name: "duplicate track id",
			mutate: func(t *testing.T) map[string]any {
				value := newPlotTreeSnapshotValue(t)
				value["tracks"] = []any{
					minimalPlotTreeTrack("a", PlotTreeRoleMain, ""),
					minimalPlotTreeTrack("a", PlotTreeRoleMain, ""),
				}
				return value
			},
			want: ErrPlotTreeTrackIDDuplicate,
		},
		{
			name: "main track with a parent",
			mutate: func(t *testing.T) map[string]any {
				value := newPlotTreeSnapshotValue(t)
				value["tracks"] = []any{minimalPlotTreeTrack("a", PlotTreeRoleMain, "b")}
				return value
			},
			want: ErrPlotTreeMainParent,
		},
		{
			name: "subplot without a parent",
			mutate: func(t *testing.T) map[string]any {
				value := newPlotTreeSnapshotValue(t)
				value["tracks"] = []any{minimalPlotTreeTrack("a", PlotTreeRoleSubplot, "")}
				return value
			},
			want: ErrPlotTreeSubplotParent,
		},
		{
			name: "subplot parent is not a main track",
			mutate: func(t *testing.T) map[string]any {
				value := newPlotTreeSnapshotValue(t)
				value["tracks"] = []any{
					minimalPlotTreeTrack("a", PlotTreeRoleMain, ""),
					minimalPlotTreeTrack("b", PlotTreeRoleSubplot, "c"),
					minimalPlotTreeTrack("c", PlotTreeRoleSubplot, "a"),
				}
				return value
			},
			want: ErrPlotTreeSubplotParentNotMain,
		},
		{
			name: "event cites a source that does not exist",
			mutate: func(t *testing.T) map[string]any {
				value := newPlotTreeSnapshotValue(t)
				track := firstPlotTreeTrack(t, value)
				track["events"] = []any{map[string]any{
					"status":        PlotTreeEventStatusOccurred,
					"chapterNumber": float64(1),
					"summary":       "e",
					"sources": []any{map[string]any{
						"type":          PlotTreeReferenceFinalizedChapter,
						"draftId":       float64(999),
						"chapterNumber": float64(1),
					}},
				}}
				return value
			},
			want: ErrPlotTreeSourceMissing,
		},
		{
			name: "event at chapter 4 is outside a 1..3 track",
			mutate: func(t *testing.T) map[string]any {
				value := newPlotTreeSnapshotValue(t)
				track := firstPlotTreeTrack(t, value)
				track["startChapter"] = float64(1)
				track["endChapter"] = float64(3)
				track["events"] = []any{map[string]any{
					"status":        PlotTreeEventStatusPlanned,
					"chapterNumber": float64(4),
					"summary":       "e",
					"sources": []any{map[string]any{
						"type":   PlotTreeReferenceNarrativeThread,
						"planId": float64(7),
					}},
				}}
				return value
			},
			want: ErrPlotTreeEventOutOfTrack,
		},
		{
			name: "track range exceeds the source domain",
			mutate: func(t *testing.T) map[string]any {
				value := newPlotTreeSnapshotValue(t)
				track := firstPlotTreeTrack(t, value)
				track["startChapter"] = float64(1)
				track["endChapter"] = float64(9)
				return value
			},
			want: ErrPlotTreeTrackOutOfBounds,
		},
		{
			name: "snapshot language differs from the project",
			mutate: func(t *testing.T) map[string]any {
				value := newPlotTreeSnapshotValue(t)
				value["writingLanguage"] = WritingLanguageZhCN
				return value
			},
			want: ErrPlotTreeLanguageMismatch,
		},
		{
			name: "snapshot revision differs from the project",
			mutate: func(t *testing.T) map[string]any {
				value := newPlotTreeSnapshotValue(t)
				value["sourceRevision"] = strings.Repeat("1", 64)
				return value
			},
			want: ErrPlotTreeRevisionMismatch,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			value := testCase.mutate(t)
			if _, err := AssertPlotTreeSnapshot(value, sources); !errors.Is(err, testCase.want) {
				t.Fatalf("expected %v, got %v", testCase.want, err)
			}
		})
	}
}

// TestAssertPlotTreeSnapshotRejectsSourceStatusMismatch pins
// sourceSupportsEvent (src/shared/plot-tree.ts:225-247).
func TestAssertPlotTreeSnapshotRejectsSourceStatusMismatch(t *testing.T) {
	value := newPlotTreeSnapshotValue(t)
	track := firstPlotTreeTrack(t, value)
	track["events"] = []any{map[string]any{
		"status":        PlotTreeEventStatusPlanned,
		"chapterNumber": float64(1),
		"summary":       "e",
		"sources": []any{map[string]any{
			"type":          PlotTreeReferenceFinalizedChapter,
			"draftId":       float64(41),
			"chapterNumber": float64(1),
		}},
	}}
	if _, err := AssertPlotTreeSnapshot(value, plotTreeTestSources()); !errors.Is(err, ErrPlotTreeEventSourceMismatch) {
		t.Fatalf("expected ErrPlotTreeEventSourceMismatch, got %v", err)
	}
}

func TestAssertStoredPlotTreeSnapshotDoesNotNeedCurrentSources(t *testing.T) {
	// A stored snapshot whose sources no longer exist only needs to be
	// structurally valid; assertStoredPlotTreeSnapshot never consults sources.
	value := newPlotTreeSnapshotValue(t)
	track := firstPlotTreeTrack(t, value)
	track["startChapter"] = float64(1)
	track["endChapter"] = float64(9)
	snapshot, err := AssertStoredPlotTreeSnapshot(value)
	if err != nil {
		t.Fatalf("AssertStoredPlotTreeSnapshot returned %v", err)
	}
	if snapshot.Tracks[0].EndChapter != 9 {
		t.Fatalf("endChapter = %d, want 9", snapshot.Tracks[0].EndChapter)
	}
}

func TestHasUsablePlotTreeEventSource(t *testing.T) {
	if !HasUsablePlotTreeEventSource(plotTreeTestSources()) {
		t.Fatal("expected a usable source domain")
	}
	synopsisOnly := plotTreeTestSources()
	synopsisOnly.Blueprints = nil
	synopsisOnly.FinalizedChapters = nil
	synopsisOnly.NarrativeThreads = nil
	if HasUsablePlotTreeEventSource(synopsisOnly) {
		t.Fatal("a synopsis alone cannot support an event")
	}
	blankRows := plotTreeTestSources()
	blankRows.Blueprints = []PlotTreeBlueprintSource{{ChapterNumber: 0}}
	blankRows.FinalizedChapters = []PlotTreeFinalizedChapterSource{{DraftID: 0, ChapterNumber: 1}}
	blankRows.NarrativeThreads = nil
	if HasUsablePlotTreeEventSource(blankRows) {
		t.Fatal("non-empty but invalid rows cannot support an event")
	}
}
