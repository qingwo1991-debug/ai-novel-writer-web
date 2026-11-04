// Package graphgen ports the pure plot-graph generation helpers of
// AI-Novel-Writer from TypeScript to Go without changing behaviour.
//
// Every function here mirrors a specific TypeScript source file, and the doc
// comment of each one names the file and line range it was taken from. Prompt
// templates are byte-for-byte copies of the TypeScript template literals: the
// provider sees the same text, so the tests in this package pin the complete
// output of every builder instead of spot-checking fragments.
package graphgen

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"unicode/utf16"
)

// Reasoning stages and task outputs accepted by the generation runtime
// (plot-tree-generation-pure.ts:325-326, narrative-thread-generation-pure.ts:124-125, :149-150).
const (
	ReasoningStagePlanning = "planning"
	ReasoningStageReview   = "review"

	TaskOutputStructuredData = "structured-data"
)

// Task purposes (plot-tree-generation-pure.ts:324, narrative-thread-generation-pure.ts:123, :148).
const (
	PurposePlotTreeSnapshot             = "plot-tree-snapshot"
	PurposeNarrativeThreadPlanCandidate = "narrative-thread-plan-candidate"
	PurposeNarrativeThreadEventCandidate = "narrative-thread-event-candidate"
)

// Message roles as used by the builders.
const (
	RoleSystem = "system"
	RoleUser   = "user"
)

// PlotTreeInputMaxCharacters mirrors PLOT_TREE_INPUT_MAX_CHARACTERS
// (plot-tree-generation-pure.ts:21).
const PlotTreeInputMaxCharacters = 220_000

// GenerationBudget mirrors a frozen budget object such as
// PLOT_TREE_GENERATION_BUDGET (plot-tree-generation-pure.ts:14-19) or
// NARRATIVE_THREAD_CANDIDATE_BUDGET (narrative-thread-generation-pure.ts:38-43).
type GenerationBudget struct {
	MaxAttempts                       int `json:"maxAttempts"`
	MaxRequestedOutputTokens          int `json:"maxRequestedOutputTokens"`
	MaxRequestedOutputTokensPerAttempt int `json:"maxRequestedOutputTokensPerAttempt"`
	DeadlineMs                        int `json:"deadlineMs"`
}

// PlotTreeGenerationBudget mirrors PLOT_TREE_GENERATION_BUDGET
// (plot-tree-generation-pure.ts:14-19); deadlineMs is `10 * 60_000`.
var PlotTreeGenerationBudget = GenerationBudget{
	MaxAttempts:                        1,
	MaxRequestedOutputTokens:           8192,
	MaxRequestedOutputTokensPerAttempt: 8192,
	DeadlineMs:                         600_000,
}

// NarrativeThreadCandidateBudget mirrors NARRATIVE_THREAD_CANDIDATE_BUDGET
// (narrative-thread-generation-pure.ts:38-43).
var NarrativeThreadCandidateBudget = GenerationBudget{
	MaxAttempts:                        1,
	MaxRequestedOutputTokens:           4096,
	MaxRequestedOutputTokensPerAttempt: 4096,
	DeadlineMs:                         120_000,
}

// Plot tree errors. The TypeScript classes carry symbolic codes
// (plot-tree-generation-pure.ts:55-83); the Go sentinels keep the same code
// strings so a caller can switch on them exactly like the TS error classes.
var (
	// ErrPlotTreeSource mirrors PlotTreeSourceError (plot-tree-generation-pure.ts:55-63).
	ErrPlotTreeSource = errors.New("NO_EVENT_SOURCES")
	// ErrPlotTreeSourceLimit mirrors PlotTreeSourceLimitError (plot-tree-generation-pure.ts:65-73).
	ErrPlotTreeSourceLimit = errors.New("SOURCE_LIMIT_EXCEEDED")
	// ErrPlotTreeInputLimit mirrors PlotTreeInputLimitError (plot-tree-generation-pure.ts:75-83).
	ErrPlotTreeInputLimit = errors.New("SOURCE_INPUT_TOO_LARGE")
)

// PlotTreeSourceLimitError mirrors PlotTreeSourceLimitError and keeps the
// `maximum` field the TypeScript class exposes.
type PlotTreeSourceLimitError struct {
	Maximum int
}

func (e *PlotTreeSourceLimitError) Error() string { return ErrPlotTreeSourceLimit.Error() }

// Unwrap lets errors.Is(err, ErrPlotTreeSourceLimit) hold.
func (e *PlotTreeSourceLimitError) Unwrap() error { return ErrPlotTreeSourceLimit }

// PlotTreeInputLimitError mirrors PlotTreeInputLimitError and keeps the
// `maximumCharacters` field the TypeScript class exposes.
type PlotTreeInputLimitError struct {
	MaximumCharacters int
}

func (e *PlotTreeInputLimitError) Error() string { return ErrPlotTreeInputLimit.Error() }

// Unwrap lets errors.Is(err, ErrPlotTreeInputLimit) hold.
func (e *PlotTreeInputLimitError) Unwrap() error { return ErrPlotTreeInputLimit }

// Task mirrors the object literal every builder returns
// (plot-tree-generation-pure.ts:323-348, narrative-thread-generation-pure.ts:122-143).
type Task struct {
	Purpose        string    `json:"purpose"`
	ReasoningStage string    `json:"reasoningStage"`
	Output         string    `json:"output"`
	Messages       []Message `json:"messages"`
}

// Message mirrors the `{ role, content }` literals of the messages array.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// BlueprintNewCharacterCandidate mirrors BlueprintNewCharacterCandidate
// (src/shared/blueprint-semantic-contract.ts:12-15).
type BlueprintNewCharacterCandidate struct {
	Name string `json:"name"`
	Role string `json:"role"`
}

// Blueprint mirrors BlueprintData (electron/repositories/blueprint-repository.ts:36-55).
// Field order is the insertion order of rowToData (blueprint-repository.ts:152-165),
// which is the key order JSON.stringify emits for the plan task's user message.
type Blueprint struct {
	ChapterNumber int      `json:"chapterNumber"`
	Title         string   `json:"title"`
	Role          string   `json:"role"`
	Purpose       string   `json:"purpose"`
	KeyEvents     string   `json:"keyEvents"`
	Characters    []string `json:"characters"`
	// TypeScript declares both of the next two as optional (`?:`), so a zero
	// value is omitted like `undefined`. A present-but-empty candidate list is
	// indistinguishable from an absent one here; the real row mapper
	// (rowToData) never sets either field.
	NewCharacterCandidates []BlueprintNewCharacterCandidate `json:"newCharacterCandidates,omitempty"`
	RelationshipHints      any                               `json:"relationshipHints,omitempty"`
	SuspenseHook           string                            `json:"suspenseHook"`
	UserGuidance           string                            `json:"userGuidance"`
	Notes                  string                            `json:"notes"`
	NotesUpdatedAt         string                            `json:"notesUpdatedAt"`
}

// PlotTreeSynopsisSource mirrors PlotTreeSourceBundle['synopsis'] (src/shared/plot-tree.ts:73-75).
type PlotTreeSynopsisSource struct {
	Content string `json:"content"`
}

// PlotTreeBlueprintSource mirrors PlotTreeSourceBundle['blueprints'][number] (src/shared/plot-tree.ts:76-81).
type PlotTreeBlueprintSource struct {
	ChapterNumber int    `json:"chapterNumber"`
	Title         string `json:"title"`
	Purpose       string `json:"purpose"`
	KeyEvents     string `json:"keyEvents"`
}

// PlotTreeFinalizedChapterSource mirrors PlotTreeSourceBundle['finalizedChapters'][number]
// (src/shared/plot-tree.ts:82-87).
type PlotTreeFinalizedChapterSource struct {
	DraftID       int    `json:"draftId"`
	ChapterNumber int    `json:"chapterNumber"`
	Title         string `json:"title"`
	Summary       string `json:"summary"`
}

// PlotTreeNarrativeThreadEventSource mirrors the inline event object of
// PlotTreeNarrativeThreadSource (src/shared/plot-tree.ts:59-65).
type PlotTreeNarrativeThreadEventSource struct {
	ID            int    `json:"id"`
	ChapterNumber int    `json:"chapterNumber"`
	Type          string `json:"type"`
	Evidence      string `json:"evidence"`
	Reason        string `json:"reason"`
}

// PlotTreeNarrativeThreadSource mirrors PlotTreeNarrativeThreadSource
// (src/shared/plot-tree.ts:51-66).
type PlotTreeNarrativeThreadSource struct {
	ID                 int                                     `json:"id"`
	Title              string                                  `json:"title"`
	Type               string                                  `json:"type"`
	TargetStartChapter int                                     `json:"targetStartChapter"`
	TargetEndChapter   int                                     `json:"targetEndChapter"`
	AuthorIntent       string                                  `json:"authorIntent"`
	Status             string                                  `json:"status"`
	Events             []PlotTreeNarrativeThreadEventSource    `json:"events"`
}

// PlotTreeSourceBundle mirrors PlotTreeSourceBundle (src/shared/plot-tree.ts:68-91).
type PlotTreeSourceBundle struct {
	WritingLanguage   string                            `json:"writingLanguage"`
	Synopsis          PlotTreeSynopsisSource            `json:"synopsis"`
	Blueprints        []PlotTreeBlueprintSource         `json:"blueprints"`
	FinalizedChapters []PlotTreeFinalizedChapterSource   `json:"finalizedChapters"`
	NarrativeThreads  []PlotTreeNarrativeThreadSource   `json:"narrativeThreads"`
	SourceRevision    string                            `json:"sourceRevision"`
	Snapshot          *PlotTreeSnapshot                 `json:"snapshot"`
	// `storedSnapshotInvalid?: true` (src/shared/plot-tree.ts:90).
	StoredSnapshotInvalid bool `json:"storedSnapshotInvalid,omitempty"`
}

// plotTreeSourceLimits mirrors PLOT_TREE_SOURCE_LIMITS (plot-tree-generation-pure.ts:23-34).
type plotTreeSourceLimits struct {
	synopsisCharacters                   int
	labelCharacters                      int
	detailCharacters                     int
	largeProjectThreshold                int
	largeProjectSynopsisCharacters       int
	largeProjectLabelCharacters          int
	largeProjectDetailCharacters         int
	largeProjectEventDetailCharacters    int
	blueprints                           int
	finalizedChapters                    int
}

var plotTreeSourceLimit = plotTreeSourceLimits{
	synopsisCharacters:                6_000,
	labelCharacters:                   160,
	detailCharacters:                  320,
	largeProjectThreshold:             120,
	largeProjectSynopsisCharacters:    3_000,
	largeProjectLabelCharacters:       48,
	largeProjectDetailCharacters:      64,
	largeProjectEventDetailCharacters: 24,
	blueprints:                        200,
	finalizedChapters:                 200,
}

// utf16Length mirrors JavaScript String.prototype.length: it counts UTF-16 code
// units, not code points and not bytes, which is what the TypeScript length
// checks compare against.
func utf16Length(value string) int {
	return len(utf16.Encode([]rune(value)))
}

// jsSliceFrom mirrors String.prototype.slice(start) for a negative or positive
// start index.
func jsSliceFrom(units []uint16, start int) []uint16 {
	if start < 0 {
		start += len(units)
		if start < 0 {
			start = 0
		}
	} else if start > len(units) {
		start = len(units)
	}
	return units[start:]
}

// jsSliceTo mirrors String.prototype.slice(0, end).
func jsSliceTo(units []uint16, end int) []uint16 {
	if end < 0 {
		end += len(units)
		if end < 0 {
			end = 0
		}
	} else if end > len(units) {
		end = len(units)
	}
	return units[:end]
}

// boundedText mirrors boundedText (plot-tree-generation-pure.ts:266-270):
//
//	const head = Math.ceil((maximum - 1) / 2)
//	return `${value.slice(0, head)}…${value.slice(-(maximum - head - 1))}`
//
// The arithmetic runs on UTF-16 code units exactly like the TypeScript, so a
// truncation boundary that lands inside a surrogate pair (only possible when
// the source text contains astral characters) loses the pair on both sides:
// Go decodes the dangling half as U+FFFD, while JavaScript keeps a lone
// surrogate that JSON.stringify would emit as \udXXX.
func boundedText(value string, maximum int) string {
	units := utf16.Encode([]rune(value))
	if len(units) <= maximum {
		return value
	}
	head := int(math.Ceil(float64(maximum-1) / 2))
	prefix := jsSliceTo(units, head)
	suffix := jsSliceFrom(units, -(maximum - head - 1))
	return string(utf16.Decode(prefix)) + "…" + string(utf16.Decode(suffix))
}

// plotTreeFacts is the JSON payload of the plot-tree user message. Field order
// and tags mirror generationFacts (plot-tree-generation-pure.ts:280-310).
type plotTreeFacts struct {
	Synopsis          string                    `json:"synopsis"`
	Blueprints        []plotTreeFactBlueprint   `json:"blueprints"`
	FinalizedChapters []plotTreeFactChapter     `json:"finalizedChapters"`
	NarrativeThreads  []plotTreeFactThread      `json:"narrativeThreads"`
}

type plotTreeFactBlueprint struct {
	ChapterNumber int    `json:"chapterNumber"`
	Title         string `json:"title"`
	Purpose       string `json:"purpose"`
	KeyEvents     string `json:"keyEvents"`
}

type plotTreeFactChapter struct {
	DraftID       int    `json:"draftId"`
	ChapterNumber int    `json:"chapterNumber"`
	Title         string `json:"title"`
	Summary       string `json:"summary"`
}

type plotTreeFactThreadEvent struct {
	ID            int    `json:"id"`
	ChapterNumber int    `json:"chapterNumber"`
	Type          string `json:"type"`
	Evidence      string `json:"evidence"`
	Reason        string `json:"reason"`
}

type plotTreeFactThread struct {
	ID                 int                        `json:"id"`
	Title              string                     `json:"title"`
	Type               string                     `json:"type"`
	TargetStartChapter int                        `json:"targetStartChapter"`
	TargetEndChapter   int                        `json:"targetEndChapter"`
	AuthorIntent       string                     `json:"authorIntent"`
	Status             string                     `json:"status"`
	Events             []plotTreeFactThreadEvent  `json:"events"`
}

// generationFacts mirrors generationFacts (plot-tree-generation-pure.ts:272-311).
func generationFacts(sources PlotTreeSourceBundle) plotTreeFacts {
	limits := plotTreeSourceLimit
	largeProject := len(sources.Blueprints) > limits.largeProjectThreshold ||
		len(sources.FinalizedChapters) > limits.largeProjectThreshold
	labelCharacters := limits.labelCharacters
	detailCharacters := limits.detailCharacters
	eventDetailCharacters := limits.detailCharacters
	synopsisCharacters := limits.synopsisCharacters
	if largeProject {
		labelCharacters = limits.largeProjectLabelCharacters
		detailCharacters = limits.largeProjectDetailCharacters
		eventDetailCharacters = limits.largeProjectEventDetailCharacters
		synopsisCharacters = limits.largeProjectSynopsisCharacters
	}
	blueprints := make([]plotTreeFactBlueprint, 0, len(sources.Blueprints))
	for _, blueprint := range sources.Blueprints {
		blueprints = append(blueprints, plotTreeFactBlueprint{
			ChapterNumber: blueprint.ChapterNumber,
			Title:         boundedText(blueprint.Title, labelCharacters),
			Purpose:       boundedText(blueprint.Purpose, detailCharacters),
			KeyEvents:     boundedText(blueprint.KeyEvents, detailCharacters),
		})
	}
	finalizedChapters := make([]plotTreeFactChapter, 0, len(sources.FinalizedChapters))
	for _, chapter := range sources.FinalizedChapters {
		finalizedChapters = append(finalizedChapters, plotTreeFactChapter{
			DraftID:       chapter.DraftID,
			ChapterNumber: chapter.ChapterNumber,
			Title:         boundedText(chapter.Title, labelCharacters),
			Summary:       boundedText(chapter.Summary, detailCharacters),
		})
	}
	narrativeThreads := make([]plotTreeFactThread, 0, len(sources.NarrativeThreads))
	for _, thread := range sources.NarrativeThreads {
		events := make([]plotTreeFactThreadEvent, 0, len(thread.Events))
		for _, event := range thread.Events {
			events = append(events, plotTreeFactThreadEvent{
				ID:            event.ID,
				ChapterNumber: event.ChapterNumber,
				Type:          event.Type,
				Evidence:      boundedText(event.Evidence, eventDetailCharacters),
				Reason:        boundedText(event.Reason, eventDetailCharacters),
			})
		}
		narrativeThreads = append(narrativeThreads, plotTreeFactThread{
			ID:                 thread.ID,
			Title:              boundedText(thread.Title, labelCharacters),
			Type:               boundedText(thread.Type, labelCharacters),
			TargetStartChapter: thread.TargetStartChapter,
			TargetEndChapter:   thread.TargetEndChapter,
			AuthorIntent:       boundedText(thread.AuthorIntent, detailCharacters),
			Status:             thread.Status,
			Events:             events,
		})
	}
	return plotTreeFacts{
		Synopsis:          boundedText(sources.Synopsis.Content, synopsisCharacters),
		Blueprints:        blueprints,
		FinalizedChapters: finalizedChapters,
		NarrativeThreads:  narrativeThreads,
	}
}

// plotTreeSystemZhCN is the zh-CN half of the plot-tree system message. The four
// entries are the four array literals of plot-tree-generation-pure.ts:333-336,
// joined with '\n' exactly like the TypeScript `.join('\n')` on line 337.
var plotTreeSystemZhCN = []string{
	`你是小说剧情结构编辑。把给定的情节总大纲、章节蓝图、已定稿章节摘要和作者确认的叙事线索归纳为只读剧情树。`,
	`区分 main 主线与 subplot 支线；每条支线必须用 parentTrackId 关联一条主线，主线不能有 parentTrackId。planned 只能来自章节蓝图或人工叙事计划，occurred 只能来自已定稿章节或已确认叙事事件。`,
	`情节总大纲只用于归纳轨道和摘要，不是可引用来源；绝不能在事件 sources 中引用它，也绝不能输出 source.type="synopsis"。每个事件必须至少引用一个同章节的真实来源，且只能使用以下格式：{"type":"blueprint","chapterNumber":1}、{"type":"finalized-chapter","draftId":1,"chapterNumber":1}、{"type":"narrative-thread","planId":1}、{"type":"narrative-thread","planId":1,"eventId":1,"chapterNumber":1}。不得编造 ID 或章节。`,
	`只输出 JSON 对象：{"tracks":[{"id":"stable-id","title":"","role":"main","startChapter":1,"endChapter":1,"summary":"","events":[{"status":"planned|occurred","chapterNumber":1,"summary":"","sources":[]}]}]}。仅 subplot 轨道增加 parentTrackId。不要输出解释或 Markdown。`,
}

// plotTreeSystemEnUS is the en-US half of the plot-tree system message
// (plot-tree-generation-pure.ts:339-342, joined on line 343).
var plotTreeSystemEnUS = []string{
	`You are a fiction plot-structure editor. Derive a read-only plot tree from the supplied synopsis, chapter blueprints, finalized chapter summaries, and author-confirmed narrative threads.`,
	`Separate main tracks from subplot tracks. Every subplot must reference one main track with parentTrackId; main tracks must not have parentTrackId. planned must be supported by a chapter blueprint or human narrative plan; occurred must be supported by a finalized chapter or confirmed narrative event.`,
	`The synopsis is context for synthesizing tracks and summaries, not a citable source. Never cite it in event sources and never emit source.type="synopsis". Every event must cite at least one real source for the same chapter using only these forms: {"type":"blueprint","chapterNumber":1}, {"type":"finalized-chapter","draftId":1,"chapterNumber":1}, {"type":"narrative-thread","planId":1}, or {"type":"narrative-thread","planId":1,"eventId":1,"chapterNumber":1}. Never invent an ID or chapter.`,
	`Return only one JSON object: {"tracks":[{"id":"stable-id","title":"","role":"main","startChapter":1,"endChapter":1,"summary":"","events":[{"status":"planned|occurred","chapterNumber":1,"summary":"","sources":[]}]}]}. Add parentTrackId only to subplot tracks. Do not return explanations or Markdown.`,
}

// narrativeThreadPlanSystemZhCN mirrors the zh-CN template literal of
// buildNarrativeThreadPlanTask (narrative-thread-generation-pure.ts:131). The
// single `%d` stands for `${input.totalChapters}`.
const narrativeThreadPlanSystemZhCN = `你是小说结构编辑。只从章节蓝图提出可供作者确认的伏笔与叙事线索计划，不得声称正文事件已经发生。所有章节号必须是 1..%d 范围内的整数。优先提出 3–8 条真正有用的候选；不足 3 条时不要凑数。只输出 JSON 对象：{"candidates":[{"title":"","type":"","targetStartChapter":1,"targetEndChapter":1,"authorIntent":""}]}。最多 8 项。`

// narrativeThreadPlanSystemEnUS mirrors the en-US template literal
// (narrative-thread-generation-pure.ts:132).
const narrativeThreadPlanSystemEnUS = `You are a fiction structure editor. Propose foreshadowing and narrative-thread plans from the chapter blueprint for author confirmation. Never claim that a manuscript event has occurred. Every chapter number must be an integer within 1..%d. Prefer 3–8 genuinely useful candidates; do not pad the list when fewer than three are justified. Return only one JSON object: {"candidates":[{"title":"","type":"","targetStartChapter":1,"targetEndChapter":1,"authorIntent":""}]}. Maximum 8 items.`

// narrativeThreadEventSystemZhCN mirrors the zh-CN literal of
// buildNarrativeThreadEventTask (narrative-thread-generation-pure.ts:156).
const narrativeThreadEventSystemZhCN = `你是小说定稿事实审查员。只判断给定已定稿章节是否推进了给定叙事线索。证据必须是正文中逐字出现、最多 240 字的短摘录。只输出 JSON 对象：{"candidates":[{"type":"planted|progressing|resolved|abandoned","evidence":"","reason":""}]}。最多 5 项，不得输出计划 ID、草稿 ID 或章节号。`

// narrativeThreadEventSystemEnUS mirrors the en-US literal
// (narrative-thread-generation-pure.ts:157).
const narrativeThreadEventSystemEnUS = `You review finalized fiction facts. Decide only whether the supplied finalized chapter advances the supplied narrative thread. Evidence must be a verbatim excerpt of at most 240 characters from the manuscript. Return only one JSON object: {"candidates":[{"type":"planted|progressing|resolved|abandoned","evidence":"","reason":""}]}. Maximum 5 items. Do not output plan IDs, draft IDs, or chapter numbers.`

// BuildPlotTreeTask mirrors buildPlotTreeTask (plot-tree-generation-pure.ts:313-349).
// The three guards keep the TypeScript order: a source domain check
// (hasUsablePlotTreeEventSource, src/shared/plot-tree.ts:173-175), the
// 200-row blueprint/finalized ceilings, then the 220_000 code-unit input
// ceiling measured on the JSON.stringify'd facts.
func BuildPlotTreeTask(sources PlotTreeSourceBundle) (Task, error) {
	if !HasUsablePlotTreeEventSource(sources) {
		return Task{}, ErrPlotTreeSource
	}
	if len(sources.Blueprints) > plotTreeSourceLimit.blueprints ||
		len(sources.FinalizedChapters) > plotTreeSourceLimit.finalizedChapters {
		return Task{}, &PlotTreeSourceLimitError{Maximum: plotTreeSourceLimit.blueprints}
	}
	facts, err := EncodeJSON(generationFacts(sources))
	if err != nil {
		return Task{}, err
	}
	if utf16Length(facts) > PlotTreeInputMaxCharacters {
		return Task{}, &PlotTreeInputLimitError{MaximumCharacters: PlotTreeInputMaxCharacters}
	}
	language := sources.WritingLanguage
	return Task{
		Purpose:        PurposePlotTreeSnapshot,
		ReasoningStage: ReasoningStagePlanning,
		Output:         TaskOutputStructuredData,
		Messages: []Message{
			{
				Role: RoleSystem,
				Content: WritingLanguageText(
					language,
					strings.Join(plotTreeSystemZhCN, "\n"),
					strings.Join(plotTreeSystemEnUS, "\n"),
				),
			},
			{Role: RoleUser, Content: facts},
		},
	}, nil
}

// NarrativeThreadPlanTaskInput mirrors
// Omit<GenerateNarrativeThreadPlanCandidateInput, 'modelId' | 'signal'>
// (narrative-thread-generation-pure.ts:121) and keeps only the fields the
// builder reads.
type NarrativeThreadPlanTaskInput struct {
	WritingLanguage string
	TotalChapters   int
	Blueprint       Blueprint
}

// narrativeThreadPlanUserMessage mirrors the JSON.stringify call on
// narrative-thread-generation-pure.ts:137-140.
type narrativeThreadPlanUserMessage struct {
	TotalChapters int       `json:"totalChapters"`
	Blueprint     Blueprint `json:"blueprint"`
}

// BuildNarrativeThreadPlanTask mirrors buildNarrativeThreadPlanTask
// (narrative-thread-generation-pure.ts:121-144).
func BuildNarrativeThreadPlanTask(input NarrativeThreadPlanTaskInput) (Task, error) {
	content := WritingLanguageText(
		input.WritingLanguage,
		fmt.Sprintf(narrativeThreadPlanSystemZhCN, input.TotalChapters),
		fmt.Sprintf(narrativeThreadPlanSystemEnUS, input.TotalChapters),
	)
	userMessage, err := EncodeJSON(narrativeThreadPlanUserMessage{
		TotalChapters: input.TotalChapters,
		Blueprint:     input.Blueprint,
	})
	if err != nil {
		return Task{}, err
	}
	return Task{
		Purpose:        PurposeNarrativeThreadPlanCandidate,
		ReasoningStage: ReasoningStagePlanning,
		Output:         TaskOutputStructuredData,
		Messages: []Message{
			{Role: RoleSystem, Content: content},
			{Role: RoleUser, Content: userMessage},
		},
	}, nil
}

// NarrativeThreadEventTaskInput mirrors
// Omit<GenerateNarrativeThreadEventCandidateInput, 'modelId' | 'signal'>
// (narrative-thread-generation-pure.ts:146).
type NarrativeThreadEventTaskInput struct {
	WritingLanguage  string
	Plan             NarrativeThreadView
	DraftID          int
	ChapterNumber    int
	FinalizedContent string
}

// narrativeThreadEventPlanMessage mirrors the nested `plan` literal of
// narrative-thread-generation-pure.ts:164-171. Only these six fields are
// serialised; the plan's id, events and timestamps never reach the provider.
type narrativeThreadEventPlanMessage struct {
	Title              string `json:"title"`
	Type               string `json:"type"`
	TargetStartChapter int    `json:"targetStartChapter"`
	TargetEndChapter   int    `json:"targetEndChapter"`
	AuthorIntent       string `json:"authorIntent"`
	CurrentStatus      string `json:"currentStatus"`
}

// narrativeThreadEventUserMessage mirrors the JSON.stringify call on
// narrative-thread-generation-pure.ts:162-173.
type narrativeThreadEventUserMessage struct {
	ChapterNumber    int                            `json:"chapterNumber"`
	Plan             narrativeThreadEventPlanMessage `json:"plan"`
	FinalizedContent string                         `json:"finalizedContent"`
}

// BuildNarrativeThreadEventTask mirrors buildNarrativeThreadEventTask
// (narrative-thread-generation-pure.ts:146-177). `input.draftId` is part of the
// TypeScript input but is deliberately not serialised (line 162-173), and the Go
// input keeps it for callers' symmetry only.
func BuildNarrativeThreadEventTask(input NarrativeThreadEventTaskInput) (Task, error) {
	content := WritingLanguageText(
		input.WritingLanguage,
		narrativeThreadEventSystemZhCN,
		narrativeThreadEventSystemEnUS,
	)
	userMessage, err := EncodeJSON(narrativeThreadEventUserMessage{
		ChapterNumber: input.ChapterNumber,
		Plan: narrativeThreadEventPlanMessage{
			Title:              input.Plan.Title,
			Type:               input.Plan.Type,
			TargetStartChapter: input.Plan.TargetStartChapter,
			TargetEndChapter:   input.Plan.TargetEndChapter,
			AuthorIntent:       input.Plan.AuthorIntent,
			CurrentStatus:      input.Plan.Status,
		},
		FinalizedContent: input.FinalizedContent,
	})
	if err != nil {
		return Task{}, err
	}
	return Task{
		Purpose:        PurposeNarrativeThreadEventCandidate,
		ReasoningStage: ReasoningStageReview,
		Output:         TaskOutputStructuredData,
		Messages: []Message{
			{Role: RoleSystem, Content: content},
			{Role: RoleUser, Content: userMessage},
		},
	}, nil
}
