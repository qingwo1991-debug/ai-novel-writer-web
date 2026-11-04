package graphgen

import (
	"errors"
	"strings"
	"testing"
)

// The prompt texts below are hand-transcribed from the TypeScript source, not
// from task.go, so they guard the literal bytes a provider receives.
//
// plot-tree-generation-pure.ts:333-336 (zh-CN) and :339-342 (en-US), joined
// with '\n' by the `.join('\n')` on lines 337 and 343.
var plotTreeSystemZhCNExpected = []string{
	`你是小说剧情结构编辑。把给定的情节总大纲、章节蓝图、已定稿章节摘要和作者确认的叙事线索归纳为只读剧情树。`,
	`区分 main 主线与 subplot 支线；每条支线必须用 parentTrackId 关联一条主线，主线不能有 parentTrackId。planned 只能来自章节蓝图或人工叙事计划，occurred 只能来自已定稿章节或已确认叙事事件。`,
	`情节总大纲只用于归纳轨道和摘要，不是可引用来源；绝不能在事件 sources 中引用它，也绝不能输出 source.type="synopsis"。每个事件必须至少引用一个同章节的真实来源，且只能使用以下格式：{"type":"blueprint","chapterNumber":1}、{"type":"finalized-chapter","draftId":1,"chapterNumber":1}、{"type":"narrative-thread","planId":1}、{"type":"narrative-thread","planId":1,"eventId":1,"chapterNumber":1}。不得编造 ID 或章节。`,
	`只输出 JSON 对象：{"tracks":[{"id":"stable-id","title":"","role":"main","startChapter":1,"endChapter":1,"summary":"","events":[{"status":"planned|occurred","chapterNumber":1,"summary":"","sources":[]}]}]}。仅 subplot 轨道增加 parentTrackId。不要输出解释或 Markdown。`,
}

var plotTreeSystemEnUSExpected = []string{
	`You are a fiction plot-structure editor. Derive a read-only plot tree from the supplied synopsis, chapter blueprints, finalized chapter summaries, and author-confirmed narrative threads.`,
	`Separate main tracks from subplot tracks. Every subplot must reference one main track with parentTrackId; main tracks must not have parentTrackId. planned must be supported by a chapter blueprint or human narrative plan; occurred must be supported by a finalized chapter or confirmed narrative event.`,
	`The synopsis is context for synthesizing tracks and summaries, not a citable source. Never cite it in event sources and never emit source.type="synopsis". Every event must cite at least one real source for the same chapter using only these forms: {"type":"blueprint","chapterNumber":1}, {"type":"finalized-chapter","draftId":1,"chapterNumber":1}, {"type":"narrative-thread","planId":1}, or {"type":"narrative-thread","planId":1,"eventId":1,"chapterNumber":1}. Never invent an ID or chapter.`,
	`Return only one JSON object: {"tracks":[{"id":"stable-id","title":"","role":"main","startChapter":1,"endChapter":1,"summary":"","events":[{"status":"planned|occurred","chapterNumber":1,"summary":"","sources":[]}]}]}. Add parentTrackId only to subplot tracks. Do not return explanations or Markdown.`,
}

// narrative-thread-generation-pure.ts:131 with totalChapters = 4.
const planSystemZhCNExpected = `你是小说结构编辑。只从章节蓝图提出可供作者确认的伏笔与叙事线索计划，不得声称正文事件已经发生。所有章节号必须是 1..4 范围内的整数。优先提出 3–8 条真正有用的候选；不足 3 条时不要凑数。只输出 JSON 对象：{"candidates":[{"title":"","type":"","targetStartChapter":1,"targetEndChapter":1,"authorIntent":""}]}。最多 8 项。`

// narrative-thread-generation-pure.ts:132 with totalChapters = 12.
const planSystemEnUSExpected = `You are a fiction structure editor. Propose foreshadowing and narrative-thread plans from the chapter blueprint for author confirmation. Never claim that a manuscript event has occurred. Every chapter number must be an integer within 1..12. Prefer 3–8 genuinely useful candidates; do not pad the list when fewer than three are justified. Return only one JSON object: {"candidates":[{"title":"","type":"","targetStartChapter":1,"targetEndChapter":1,"authorIntent":""}]}. Maximum 8 items.`

// narrative-thread-generation-pure.ts:156-157.
const eventSystemZhCNExpected = `你是小说定稿事实审查员。只判断给定已定稿章节是否推进了给定叙事线索。证据必须是正文中逐字出现、最多 240 字的短摘录。只输出 JSON 对象：{"candidates":[{"type":"planted|progressing|resolved|abandoned","evidence":"","reason":""}]}。最多 5 项，不得输出计划 ID、草稿 ID 或章节号。`

const eventSystemEnUSExpected = `You review finalized fiction facts. Decide only whether the supplied finalized chapter advances the supplied narrative thread. Evidence must be a verbatim excerpt of at most 240 characters from the manuscript. Return only one JSON object: {"candidates":[{"type":"planted|progressing|resolved|abandoned","evidence":"","reason":""}]}. Maximum 5 items. Do not output plan IDs, draft IDs, or chapter numbers.`

func assertTask(t *testing.T, got Task, want Task) {
	t.Helper()
	if got.Purpose != want.Purpose {
		t.Errorf("purpose = %q, want %q", got.Purpose, want.Purpose)
	}
	if got.ReasoningStage != want.ReasoningStage {
		t.Errorf("reasoningStage = %q, want %q", got.ReasoningStage, want.ReasoningStage)
	}
	if got.Output != want.Output {
		t.Errorf("output = %q, want %q", got.Output, want.Output)
	}
	if len(got.Messages) != len(want.Messages) {
		t.Fatalf("message count = %d, want %d", len(got.Messages), len(want.Messages))
	}
	for index := range want.Messages {
		if got.Messages[index].Role != want.Messages[index].Role {
			t.Errorf("message %d role = %q, want %q", index, got.Messages[index].Role, want.Messages[index].Role)
		}
		if got.Messages[index].Content != want.Messages[index].Content {
			t.Errorf("message %d content mismatch\n got: %q\nwant: %q", index, got.Messages[index].Content, want.Messages[index].Content)
		}
	}
}

func plotTreeSources(writingLanguage string) PlotTreeSourceBundle {
	return PlotTreeSourceBundle{
		WritingLanguage:   writingLanguage,
		Synopsis:          PlotTreeSynopsisSource{Content: ""},
		Blueprints:        []PlotTreeBlueprintSource{{ChapterNumber: 1, Title: "开端"}},
		FinalizedChapters: nil,
		NarrativeThreads:  nil,
		SourceRevision:    strings.Repeat("0", 64),
	}
}

func TestBuildPlotTreeTaskZhCNMinimal(t *testing.T) {
	task, err := BuildPlotTreeTask(plotTreeSources(WritingLanguageZhCN))
	if err != nil {
		t.Fatalf("BuildPlotTreeTask returned %v", err)
	}
	assertTask(t, task, Task{
		Purpose:        PurposePlotTreeSnapshot,
		ReasoningStage: ReasoningStagePlanning,
		Output:         TaskOutputStructuredData,
		Messages: []Message{
			{Role: RoleSystem, Content: strings.Join(plotTreeSystemZhCNExpected, "\n")},
			{
				Role: RoleUser,
				Content: `{"synopsis":"","blueprints":[{"chapterNumber":1,"title":"开端","purpose":"","keyEvents":""}],` +
					`"finalizedChapters":[],"narrativeThreads":[]}`,
			},
		},
	})
}

func TestBuildPlotTreeTaskEnUSMultipleSources(t *testing.T) {
	sources := PlotTreeSourceBundle{
		WritingLanguage: WritingLanguageEnUS,
		Synopsis:        PlotTreeSynopsisSource{Content: "A clerk investigates 账本 that vanished."},
		Blueprints: []PlotTreeBlueprintSource{
			{ChapterNumber: 1, Title: "The empty safe", Purpose: "Launch", KeyEvents: "The 账本 vanishes."},
			{ChapterNumber: 2, Title: "Second wind", Purpose: "Escalate", KeyEvents: "The seal breaks."},
		},
		FinalizedChapters: []PlotTreeFinalizedChapterSource{
			{DraftID: 41, ChapterNumber: 1, Title: "The empty safe", Summary: "The ledger is missing."},
		},
		NarrativeThreads: []PlotTreeNarrativeThreadSource{
			{
				ID:                 7,
				Title:              "Missing ledger",
				Type:               "mystery",
				TargetStartChapter: 1,
				TargetEndChapter:   3,
				AuthorIntent:       "Reveal the forger in chapter three.",
				Status:             "planted",
				Events: []PlotTreeNarrativeThreadEventSource{
					{ID: 11, ChapterNumber: 1, Type: "planted", Evidence: "the ledger was gone", Reason: "plants the mystery."},
				},
			},
		},
		SourceRevision: strings.Repeat("0", 64),
	}
	task, err := BuildPlotTreeTask(sources)
	if err != nil {
		t.Fatalf("BuildPlotTreeTask returned %v", err)
	}
	assertTask(t, task, Task{
		Purpose:        PurposePlotTreeSnapshot,
		ReasoningStage: ReasoningStagePlanning,
		Output:         TaskOutputStructuredData,
		Messages: []Message{
			{Role: RoleSystem, Content: strings.Join(plotTreeSystemEnUSExpected, "\n")},
			{
				Role: RoleUser,
				Content: `{"synopsis":"A clerk investigates 账本 that vanished.",` +
					`"blueprints":[` +
					`{"chapterNumber":1,"title":"The empty safe","purpose":"Launch","keyEvents":"The 账本 vanishes."},` +
					`{"chapterNumber":2,"title":"Second wind","purpose":"Escalate","keyEvents":"The seal breaks."}],` +
					`"finalizedChapters":[` +
					`{"draftId":41,"chapterNumber":1,"title":"The empty safe","summary":"The ledger is missing."}],` +
					`"narrativeThreads":[` +
					`{"id":7,"title":"Missing ledger","type":"mystery","targetStartChapter":1,"targetEndChapter":3,` +
					`"authorIntent":"Reveal the forger in chapter three.","status":"planted","events":[` +
					`{"id":11,"chapterNumber":1,"type":"planted","evidence":"the ledger was gone","reason":"plants the mystery."}]}]}`,
			},
		},
	})
}

// TestBuildPlotTreeTaskTruncatesLongFacts pins boundedText
// (plot-tree-generation-pure.ts:266-270): 3000 units + '…' + 2999 units for a
// 6000-unit synopsis, 80 + '…' + 79 for a 160-unit label, 160 + '…' + 159 for a
// 320-unit detail.
func TestBuildPlotTreeTaskTruncatesLongFacts(t *testing.T) {
	sources := plotTreeSources(WritingLanguageZhCN)
	sources.Synopsis = PlotTreeSynopsisSource{Content: strings.Repeat("乙", 7_000)}
	sources.Blueprints = []PlotTreeBlueprintSource{{
		ChapterNumber: 1,
		Title:         strings.Repeat("甲", 200),
		Purpose:       strings.Repeat("丙", 400),
		KeyEvents:     "k",
	}}
	task, err := BuildPlotTreeTask(sources)
	if err != nil {
		t.Fatalf("BuildPlotTreeTask returned %v", err)
	}
	wantSynopsis := strings.Repeat("乙", 3_000) + "…" + strings.Repeat("乙", 2_999)
	wantTitle := strings.Repeat("甲", 80) + "…" + strings.Repeat("甲", 79)
	wantPurpose := strings.Repeat("丙", 160) + "…" + strings.Repeat("丙", 159)
	assertTask(t, task, Task{
		Purpose:        PurposePlotTreeSnapshot,
		ReasoningStage: ReasoningStagePlanning,
		Output:         TaskOutputStructuredData,
		Messages: []Message{
			{Role: RoleSystem, Content: strings.Join(plotTreeSystemZhCNExpected, "\n")},
			{
				Role: RoleUser,
				Content: `{"synopsis":"` + wantSynopsis + `","blueprints":[{"chapterNumber":1,"title":"` +
					wantTitle + `","purpose":"` + wantPurpose + `","keyEvents":"k"}],` +
					`"finalizedChapters":[],"narrativeThreads":[]}`,
			},
		},
	})
}

func TestBuildPlotTreeTaskRejectsMissingEventSources(t *testing.T) {
	sources := plotTreeSources(WritingLanguageZhCN)
	sources.Blueprints = nil
	if _, err := BuildPlotTreeTask(sources); !errors.Is(err, ErrPlotTreeSource) {
		t.Fatalf("expected ErrPlotTreeSource, got %v", err)
	}
}

// TestBuildPlotTreeTaskGuardOrder pins the guard order of
// plot-tree-generation-pure.ts:314-318: the source-domain check runs before the
// 200-row ceiling.
func TestBuildPlotTreeTaskGuardOrder(t *testing.T) {
	invalid := plotTreeSources(WritingLanguageZhCN)
	invalid.Blueprints = make([]PlotTreeBlueprintSource, 201)
	if _, err := BuildPlotTreeTask(invalid); !errors.Is(err, ErrPlotTreeSource) {
		t.Fatalf("expected ErrPlotTreeSource before the row ceiling, got %v", err)
	}
	oversized := plotTreeSources(WritingLanguageZhCN)
	oversized.Blueprints = make([]PlotTreeBlueprintSource, 201)
	for index := range oversized.Blueprints {
		oversized.Blueprints[index] = PlotTreeBlueprintSource{ChapterNumber: index + 1, Title: "章"}
	}
	_, err := BuildPlotTreeTask(oversized)
	var limitError *PlotTreeSourceLimitError
	if !errors.As(err, &limitError) || limitError.Maximum != 200 {
		t.Fatalf("expected *PlotTreeSourceLimitError{Maximum: 200}, got %v", err)
	}
}

func TestBuildPlotTreeTaskRejectsOversizedFacts(t *testing.T) {
	sources := plotTreeSources(WritingLanguageZhCN)
	sources.Blueprints = nil
	threads := make([]PlotTreeNarrativeThreadSource, 0, 200)
	for threadIndex := 0; threadIndex < 200; threadIndex++ {
		events := make([]PlotTreeNarrativeThreadEventSource, 0, 10)
		for eventIndex := 0; eventIndex < 10; eventIndex++ {
			events = append(events, PlotTreeNarrativeThreadEventSource{
				ID:            threadIndex*10 + eventIndex + 1,
				ChapterNumber: 1,
				Type:          "planted",
				Evidence:      strings.Repeat("证", 320),
				Reason:        strings.Repeat("因", 320),
			})
		}
		threads = append(threads, PlotTreeNarrativeThreadSource{
			ID:                 threadIndex + 1,
			Title:              "线索",
			Type:               "伏笔",
			TargetStartChapter: 1,
			TargetEndChapter:   1,
			AuthorIntent:       "意图",
			Status:             "planned",
			Events:             events,
		})
	}
	sources.NarrativeThreads = threads
	_, err := BuildPlotTreeTask(sources)
	var limitError *PlotTreeInputLimitError
	if !errors.As(err, &limitError) || limitError.MaximumCharacters != PlotTreeInputMaxCharacters {
		t.Fatalf("expected *PlotTreeInputLimitError{MaximumCharacters: 220000}, got %v", err)
	}
}

func TestBuildNarrativeThreadPlanTaskZhCNMinimal(t *testing.T) {
	task, err := BuildNarrativeThreadPlanTask(NarrativeThreadPlanTaskInput{
		WritingLanguage: WritingLanguageZhCN,
		TotalChapters:   4,
		Blueprint: Blueprint{
			ChapterNumber: 2,
			Title:         "第二章",
			Role:          "main",
			Purpose:       "",
			KeyEvents:     "",
			Characters:    []string{},
		},
	})
	if err != nil {
		t.Fatalf("BuildNarrativeThreadPlanTask returned %v", err)
	}
	assertTask(t, task, Task{
		Purpose:        PurposeNarrativeThreadPlanCandidate,
		ReasoningStage: ReasoningStagePlanning,
		Output:         TaskOutputStructuredData,
		Messages: []Message{
			{Role: RoleSystem, Content: planSystemZhCNExpected},
			{
				Role: RoleUser,
				Content: `{"totalChapters":4,"blueprint":{"chapterNumber":2,"title":"第二章","role":"main",` +
					`"purpose":"","keyEvents":"","characters":[],"suspenseHook":"","userGuidance":"",` +
					`"notes":"","notesUpdatedAt":""}}`,
			},
		},
	})
}

func TestBuildNarrativeThreadPlanTaskEnUSMultipleFields(t *testing.T) {
	task, err := BuildNarrativeThreadPlanTask(NarrativeThreadPlanTaskInput{
		WritingLanguage: WritingLanguageEnUS,
		TotalChapters:   12,
		Blueprint: Blueprint{
			ChapterNumber: 3,
			Title:         "第三章：铜钥匙",
			Role:          "main",
			Purpose:       "推进调查",
			KeyEvents:     "林岚找到铜钥匙。",
			Characters:    []string{"林岚", "赵铭"},
			NewCharacterCandidates: []BlueprintNewCharacterCandidate{
				{Name: "周叙", Role: "supporting"},
			},
			SuspenseHook:   "钥匙是谁藏的？",
			UserGuidance:   "",
			Notes:          "作者备注",
			NotesUpdatedAt: "2026-09-13T00:00:00.000Z",
		},
	})
	if err != nil {
		t.Fatalf("BuildNarrativeThreadPlanTask returned %v", err)
	}
	assertTask(t, task, Task{
		Purpose:        PurposeNarrativeThreadPlanCandidate,
		ReasoningStage: ReasoningStagePlanning,
		Output:         TaskOutputStructuredData,
		Messages: []Message{
			{Role: RoleSystem, Content: planSystemEnUSExpected},
			{
				Role: RoleUser,
				Content: `{"totalChapters":12,"blueprint":{"chapterNumber":3,"title":"第三章：铜钥匙",` +
					`"role":"main","purpose":"推进调查","keyEvents":"林岚找到铜钥匙。",` +
					`"characters":["林岚","赵铭"],"newCharacterCandidates":[{"name":"周叙","role":"supporting"}],` +
					`"suspenseHook":"钥匙是谁藏的？","userGuidance":"","notes":"作者备注",` +
					`"notesUpdatedAt":"2026-09-13T00:00:00.000Z"}}`,
			},
		},
	})
}

func TestBuildNarrativeThreadEventTaskZhCNMinimal(t *testing.T) {
	task, err := BuildNarrativeThreadEventTask(NarrativeThreadEventTaskInput{
		WritingLanguage: WritingLanguageZhCN,
		Plan: NarrativeThreadView{
			NarrativeThreadPlan: NarrativeThreadPlan{
				Title:              "线索",
				Type:               "伏笔",
				TargetStartChapter: 1,
				TargetEndChapter:   3,
				AuthorIntent:       "在第三章回收。",
			},
			ID:     5,
			Status: "planned",
		},
		DraftID:          41,
		ChapterNumber:    1,
		FinalizedContent: "林岚推开旧仓库的门。",
	})
	if err != nil {
		t.Fatalf("BuildNarrativeThreadEventTask returned %v", err)
	}
	assertTask(t, task, Task{
		Purpose:        PurposeNarrativeThreadEventCandidate,
		ReasoningStage: ReasoningStageReview,
		Output:         TaskOutputStructuredData,
		Messages: []Message{
			{Role: RoleSystem, Content: eventSystemZhCNExpected},
			{
				Role: RoleUser,
				Content: `{"chapterNumber":1,"plan":{"title":"线索","type":"伏笔","targetStartChapter":1,` +
					`"targetEndChapter":3,"authorIntent":"在第三章回收。","currentStatus":"planned"},` +
					`"finalizedContent":"林岚推开旧仓库的门。"}`,
			},
		},
	})
}

func TestBuildNarrativeThreadEventTaskEnUSMultipleFields(t *testing.T) {
	task, err := BuildNarrativeThreadEventTask(NarrativeThreadEventTaskInput{
		WritingLanguage: WritingLanguageEnUS,
		Plan: NarrativeThreadView{
			NarrativeThreadPlan: NarrativeThreadPlan{
				Title:              "Missing ledger",
				Type:               "mystery",
				TargetStartChapter: 1,
				TargetEndChapter:   5,
				AuthorIntent:       "Reveal the forger.",
			},
			ID:              7,
			Status:          "planted",
			DormantChapters: 2,
			Overdue:         true,
			Events: []NarrativeThreadStoredEvent{{
				NarrativeThreadEvent: NarrativeThreadEvent{
					PlanID:   7,
					DraftID:  41,
					Type:     "planted",
					Evidence: "the ledger was gone",
					Reason:   "plants the mystery.",
				},
				ID:            11,
				ChapterNumber: 1,
				ChapterTitle:  "The empty safe",
				CreatedAt:     "2026-09-13T00:00:00.000Z",
			}},
			CreatedAt: "2026-09-01T00:00:00.000Z",
			UpdatedAt: "2026-09-13T00:00:00.000Z",
		},
		DraftID:          41,
		ChapterNumber:    2,
		FinalizedContent: "The ledger is gone.\n“门框上有三道刻痕，”她说。",
	})
	if err != nil {
		t.Fatalf("BuildNarrativeThreadEventTask returned %v", err)
	}
	assertTask(t, task, Task{
		Purpose:        PurposeNarrativeThreadEventCandidate,
		ReasoningStage: ReasoningStageReview,
		Output:         TaskOutputStructuredData,
		Messages: []Message{
			{Role: RoleSystem, Content: eventSystemEnUSExpected},
			{
				Role: RoleUser,
				Content: `{"chapterNumber":2,"plan":{"title":"Missing ledger","type":"mystery",` +
					`"targetStartChapter":1,"targetEndChapter":5,"authorIntent":"Reveal the forger.",` +
					`"currentStatus":"planted"},"finalizedContent":"The ledger is gone.\n` +
					`“门框上有三道刻痕，”她说。"}`,
			},
		},
	})
}
