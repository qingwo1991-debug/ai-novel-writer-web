package graphgen

import (
	"errors"
	"strings"
	"testing"
)

const planCandidateContent = `{"candidates":[
{"title":"门框上的刻痕","type":"伏笔","targetStartChapter":2,"targetEndChapter":8,"authorIntent":"在第八回收。","eventType":"planted","evidence":"忽略"},
{"title":"校庆直播","type":"主线","targetStartChapter":1,"targetEndChapter":4,"authorIntent":"第四章回收。"},
{"title":"  第二计划  ","type":"伏笔","targetStartChapter":3,"targetEndChapter":3,"authorIntent":"单章"}
]}`

func TestParseNarrativeThreadPlanCandidatesAcceptsWellFormedOutput(t *testing.T) {
	plans, err := ParseNarrativeThreadPlanCandidates(planCandidateContent, 8, false)
	if err != nil {
		t.Fatalf("ParseNarrativeThreadPlanCandidates returned %v", err)
	}
	want := []NarrativeThreadPlan{
		{Title: "门框上的刻痕", Type: "伏笔", TargetStartChapter: 2, TargetEndChapter: 8, AuthorIntent: "在第八回收。"},
		{Title: "校庆直播", Type: "主线", TargetStartChapter: 1, TargetEndChapter: 4, AuthorIntent: "第四章回收。"},
		{Title: "第二计划", Type: "伏笔", TargetStartChapter: 3, TargetEndChapter: 3, AuthorIntent: "单章"},
	}
	if len(plans) != len(want) {
		t.Fatalf("candidate count = %d, want %d", len(plans), len(want))
	}
	for index := range want {
		if plans[index] != want[index] {
			t.Errorf("candidate %d = %+v, want %+v", index, plans[index], want[index])
		}
	}
}

// The invalid branch keeps only the four declared plan fields: model-supplied
// eventType/evidence keys are dropped (narrative-thread-generation-pure.ts:93-99).
func TestParseNarrativeThreadPlanCandidatesDropsEventFields(t *testing.T) {
	plans, err := ParseNarrativeThreadPlanCandidates(planCandidateContent, 8, false)
	if err != nil {
		t.Fatalf("ParseNarrativeThreadPlanCandidates returned %v", err)
	}
	if plans[0] != (NarrativeThreadPlan{
		Title: "门框上的刻痕", Type: "伏笔", TargetStartChapter: 2, TargetEndChapter: 8,
		AuthorIntent: "在第八回收。",
	}) {
		t.Fatalf("unexpected first plan %+v", plans[0])
	}
}

func TestParseNarrativeThreadPlanCandidatesRejectsInvalidOutput(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{
			name: "chapter range beyond totalChapters",
			content: `{"candidates":[{"title":"毕业后重逢","type":"伏笔","targetStartChapter":2,` +
				`"targetEndChapter":18,"authorIntent":"远期回收。"}]}`,
		},
		{
			name: "targetStartChapter before chapter one",
			content: `{"candidates":[{"title":"x","type":"y","targetStartChapter":0,` +
				`"targetEndChapter":2,"authorIntent":"z"}]}`,
		},
		{
			name: "targetEndChapter before targetStartChapter",
			content: `{"candidates":[{"title":"x","type":"y","targetStartChapter":4,` +
				`"targetEndChapter":2,"authorIntent":"z"}]}`,
		},
		{
			name: "non-integer chapter",
			content: `{"candidates":[{"title":"x","type":"y","targetStartChapter":1.5,` +
				`"targetEndChapter":2,"authorIntent":"z"}]}`,
		},
		{
			name: "chapter as string",
			content: `{"candidates":[{"title":"x","type":"y","targetStartChapter":"1",` +
				`"targetEndChapter":2,"authorIntent":"z"}]}`,
		},
		{
			name: "overlong authorIntent",
			content: `{"candidates":[{"title":"x","type":"y","targetStartChapter":1,` +
				`"targetEndChapter":2,"authorIntent":"` + strings.Repeat("甲", 1001) + `"}]}`,
		},
		{
			name: "blank title",
			content: `{"candidates":[{"title":"   ","type":"y","targetStartChapter":1,` +
				`"targetEndChapter":2,"authorIntent":"z"}]}`,
		},
		{
			name:    "missing candidates",
			content: `{}`,
		},
		{
			name:    "array envelope",
			content: `[]`,
		},
		{
			name:    "null candidates",
			content: `{"candidates":null}`,
		},
		{
			name:    "null candidate element",
			content: `{"candidates":[null]}`,
		},
		{
			name:    "nine candidates exceed MAX_PLAN_CANDIDATES",
			content: nineValidPlanCandidates(),
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := ParseNarrativeThreadPlanCandidates(testCase.content, 4, false); !errors.Is(err, ErrNarrativeThreadCandidatesInvalid) {
				t.Fatalf("expected ErrNarrativeThreadCandidatesInvalid, got %v", err)
			}
		})
	}
}

func nineValidPlanCandidates() string {
	candidates := make([]string, 0, 9)
	for index := 0; index < 9; index++ {
		candidates = append(candidates,
			`{"title":"线索","type":"伏笔","targetStartChapter":1,"targetEndChapter":2,"authorIntent":"回收"}`)
	}
	return `{"candidates":[` + strings.Join(candidates, ",") + `]}`
}

// Truncated model output is a JSON syntax error, never
// NARRATIVE_THREAD_CANDIDATES_INVALID: JSON.parse throws before the envelope
// checks (narrative-thread-generation-pure.ts:60).
func TestParseNarrativeThreadPlanCandidatesTruncatedJSON(t *testing.T) {
	_, err := ParseNarrativeThreadPlanCandidates(`{"candidates":[{"title":"门框上的刻痕"`, 4, false)
	if err == nil || errors.Is(err, ErrNarrativeThreadCandidatesInvalid) {
		t.Fatalf("expected a JSON syntax error, got %v", err)
	}
}

// TestParseNarrativeThreadPlanCandidatesLegacyStoredEffect pins the
// legacyStoredEffect difference (narrative-thread-generation-pure.ts:62, :78-100):
// invalid candidates are dropped, the ceiling is lifted and nothing throws.
func TestParseNarrativeThreadPlanCandidatesLegacyStoredEffect(t *testing.T) {
	content := `{"candidates":[` +
		`{"title":"保留","type":"伏笔","targetStartChapter":1,"targetEndChapter":2,"authorIntent":"保留"},` +
		`{"title":"越界","type":"伏笔","targetStartChapter":2,"targetEndChapter":18,"authorIntent":"越界"}` +
		`]}`
	if _, err := ParseNarrativeThreadPlanCandidates(content, 4, false); !errors.Is(err, ErrNarrativeThreadCandidatesInvalid) {
		t.Fatalf("strict parse should reject the whole envelope, got %v", err)
	}
	legacy, err := ParseNarrativeThreadPlanCandidates(content, 4, true)
	if err != nil {
		t.Fatalf("legacy parse returned %v", err)
	}
	if len(legacy) != 1 || legacy[0].Title != "保留" {
		t.Fatalf("legacy parse = %+v, want the single valid candidate", legacy)
	}
	if _, err := ParseNarrativeThreadPlanCandidates(nineValidPlanCandidates(), 4, false); !errors.Is(err, ErrNarrativeThreadCandidatesInvalid) {
		t.Fatalf("nine candidates must be rejected without legacy mode, got %v", err)
	}
	capped, err := ParseNarrativeThreadPlanCandidates(nineValidPlanCandidates(), 4, true)
	if err != nil {
		t.Fatalf("legacy parse returned %v", err)
	}
	if len(capped) != 8 {
		t.Fatalf("legacy parse length = %d, want the first 8 candidates", len(capped))
	}
	if empty, err := ParseNarrativeThreadPlanCandidates(`{"candidates":[null]}`, 4, true); err != nil || len(empty) != 0 {
		t.Fatalf("legacy parse of a null candidate = %v, %v; want an empty result", empty, err)
	}
}

func TestParseNarrativeThreadPlanCandidatesAcceptsEmptyList(t *testing.T) {
	plans, err := ParseNarrativeThreadPlanCandidates("\n\t{\"candidates\":[]}\n", 4, false)
	if err != nil {
		t.Fatalf("ParseNarrativeThreadPlanCandidates returned %v", err)
	}
	if len(plans) != 0 {
		t.Fatalf("candidate count = %d, want 0", len(plans))
	}
}

const finalizedChapterContent = "林岚推开旧仓库的门，发现门框上有三道平行刻痕。她没有声张。"

func TestParseNarrativeThreadEventCandidatesAcceptsWellFormedOutput(t *testing.T) {
	content := `{"candidates":[
{"type":"planted","evidence":"门框上有三道平行刻痕","reason":"第一章完成埋设。"},
{"type":"progressing","evidence":"门框上有 三道 平行刻痕","reason":"空格差异也必须接受。"},
{"type":"resolved","evidence":"她没有声张。","reason":"线索收束。"}
]}`
	events, err := ParseNarrativeThreadEventCandidates(content, finalizedChapterContent, false)
	if err != nil {
		t.Fatalf("ParseNarrativeThreadEventCandidates returned %v", err)
	}
	want := []NarrativeThreadEvent{
		{Type: "planted", Evidence: "门框上有三道平行刻痕", Reason: "第一章完成埋设。"},
		{Type: "progressing", Evidence: "门框上有 三道 平行刻痕", Reason: "空格差异也必须接受。"},
		{Type: "resolved", Evidence: "她没有声张。", Reason: "线索收束。"},
	}
	if len(events) != len(want) {
		t.Fatalf("event count = %d, want %d", len(events), len(want))
	}
	for index := range want {
		if events[index] != want[index] {
			t.Errorf("event %d = %+v, want %+v", index, events[index], want[index])
		}
	}
}

// The U+3000 ideographic space is in the ECMAScript \s set and is stripped from
// both sides before the containment check (narrative-thread-generation-pure.ts:109,116).
func TestParseNarrativeThreadEventCandidatesIgnoresFullWidthSpaces(t *testing.T) {
	events, err := ParseNarrativeThreadEventCandidates(
		`{"candidates":[{"type":"planted","evidence":"门框上有三道平行刻痕","reason":"宽空格被忽略。"}]}`,
		"林岚推开旧仓库的门，发现门框上有\u3000三道平行刻痕。她没有声张。",
		false,
	)
	if err != nil || len(events) != 1 {
		t.Fatalf("expected one event, got %v, %v", events, err)
	}
}

// U+0085 is NOT in the ECMAScript \s set even though Go's unicode.IsSpace
// accepts it, so it must survive the normalization and fail the check.
func TestParseNarrativeThreadEventCandidatesKeepsNonJSWhitespace(t *testing.T) {
	_, err := ParseNarrativeThreadEventCandidates(
		`{"candidates":[{"type":"planted","evidence":"门框上有\u0085三道平行刻痕","reason":"NEL 不是 \\s。"}]}`,
		finalizedChapterContent,
		false,
	)
	if !errors.Is(err, ErrNarrativeThreadCandidatesInvalid) {
		t.Fatalf("expected ErrNarrativeThreadCandidatesInvalid, got %v", err)
	}
}

func TestParseNarrativeThreadEventCandidatesRejectsInvalidOutput(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{
			name: "evidence not present in the finalized chapter",
			content: `{"candidates":[{"type":"resolved","evidence":"正文中不存在的银钥匙",` +
				`"reason":"不能确认。"}]}`,
		},
		{
			name: "planned is not an event type",
			content: `{"candidates":[{"type":"planned","evidence":"她没有声张","` +
				`"reason":"非法事件类型。"}]}`,
		},
		{
			name: "overlong reason",
			content: `{"candidates":[{"type":"planted","evidence":"她没有声张",` +
				`"reason":"` + strings.Repeat("甲", 501) + `"}]}`,
		},
		{
			name:    "blank evidence",
			content: `{"candidates":[{"type":"planted","evidence":"   ","reason":"空证据。"}]}`,
		},
		{
			name:    "null candidates",
			content: `{"candidates":null}`,
		},
		{
			name:    "null candidate element",
			content: `{"candidates":[null]}`,
		},
		{
			name:    "six candidates exceed MAX_EVENT_CANDIDATES",
			content: sixValidEventCandidates(),
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := ParseNarrativeThreadEventCandidates(testCase.content, finalizedChapterContent, false); !errors.Is(err, ErrNarrativeThreadCandidatesInvalid) {
				t.Fatalf("expected ErrNarrativeThreadCandidatesInvalid, got %v", err)
			}
		})
	}
}

func sixValidEventCandidates() string {
	candidates := make([]string, 0, 6)
	for index := 0; index < 6; index++ {
		candidates = append(candidates,
			`{"type":"planted","evidence":"她没有声张","reason":"重复证据。"}`)
	}
	return `{"candidates":[` + strings.Join(candidates, ",") + `]}`
}

func TestParseNarrativeThreadEventCandidatesTruncatedJSON(t *testing.T) {
	_, err := ParseNarrativeThreadEventCandidates(`{"candidates":[{"type":"planted"`, finalizedChapterContent, false)
	if err == nil || errors.Is(err, ErrNarrativeThreadCandidatesInvalid) {
		t.Fatalf("expected a JSON syntax error, got %v", err)
	}
}

// TestParseNarrativeThreadEventCandidatesLegacyStoredEffect pins the
// legacyStoredEffect difference (narrative-thread-generation-pure.ts:110-118):
// non-string types are tolerated only in legacy mode and invalid candidates are
// dropped instead of failing the whole envelope.
func TestParseNarrativeThreadEventCandidatesLegacyStoredEffect(t *testing.T) {
	arrayType := `{"candidates":[{"type":["planted"],"evidence":"她没有声张","reason":"数组类型。"}]}`
	if _, err := ParseNarrativeThreadEventCandidates(arrayType, finalizedChapterContent, false); !errors.Is(err, ErrNarrativeThreadCandidatesInvalid) {
		t.Fatalf("strict parse must reject a non-string type, got %v", err)
	}
	legacy, err := ParseNarrativeThreadEventCandidates(arrayType, finalizedChapterContent, true)
	if err != nil {
		t.Fatalf("legacy parse returned %v", err)
	}
	if len(legacy) != 1 || legacy[0].Type != "planted" {
		t.Fatalf("legacy parse = %+v, want the String()-coerced type", legacy)
	}
	mixed := `{"candidates":[` +
		`{"type":"planted","evidence":"她没有声张","reason":"保留"},` +
		`{"type":"resolved","evidence":"正文中不存在的银钥匙","reason":"丢弃"}` +
		`]}`
	if _, err := ParseNarrativeThreadEventCandidates(mixed, finalizedChapterContent, false); !errors.Is(err, ErrNarrativeThreadCandidatesInvalid) {
		t.Fatalf("strict parse should reject the whole envelope, got %v", err)
	}
	dropped, err := ParseNarrativeThreadEventCandidates(mixed, finalizedChapterContent, true)
	if err != nil {
		t.Fatalf("legacy parse returned %v", err)
	}
	if len(dropped) != 1 || dropped[0].Reason != "保留" {
		t.Fatalf("legacy parse = %+v, want only the valid candidate", dropped)
	}
	capped, err := ParseNarrativeThreadEventCandidates(sixValidEventCandidates(), finalizedChapterContent, true)
	if err != nil {
		t.Fatalf("legacy parse returned %v", err)
	}
	if len(capped) != 5 {
		t.Fatalf("legacy parse length = %d, want the first 5 candidates", len(capped))
	}
}
