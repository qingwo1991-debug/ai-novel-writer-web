package graphgen

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"time"
)

// Plot tree roles, statuses and source types (src/shared/plot-tree.ts:4-5,19-41).
const (
	PlotTreeRoleMain    = "main"
	PlotTreeRoleSubplot = "subplot"

	PlotTreeEventStatusPlanned   = "planned"
	PlotTreeEventStatusOccurred  = "occurred"

	PlotTreeReferenceBlueprint        = "blueprint"
	PlotTreeReferenceFinalizedChapter = "finalized-chapter"
	PlotTreeReferenceNarrativeThread  = "narrative-thread"
)

// MaxPlotTreeChapterNumber mirrors MAX_PLOT_TREE_CHAPTER_NUMBER
// (src/shared/plot-tree.ts:7).
const MaxPlotTreeChapterNumber = 4_294_967_295

// Plot tree validation errors. The TypeScript throws plain Error instances with
// these Chinese messages (src/shared/plot-tree.ts:97-373) rather than symbolic
// GENERATION_GRAPH_* codes, so the Go port keeps the messages verbatim to stay
// comparable; the dynamic `text(value, label)` failures are produced by
// plotTreeTextInvalid.
var (
	ErrPlotTreeSnapshotVersion      = errors.New("剧情树快照版本或写作语言无效")
	ErrPlotTreeGeneratedAt          = errors.New("剧情树生成时间无效")
	ErrPlotTreeSourceRevision       = errors.New("剧情树来源版本无效")
	ErrPlotTreeTracks               = errors.New("剧情树轨道无效")
	ErrPlotTreeTrackChapters        = errors.New("剧情树轨道章节范围无效")
	ErrPlotTreeTrackEvents          = errors.New("剧情树轨道事件无效")
	ErrPlotTreeEvent                = errors.New("剧情树事件无效")
	ErrPlotTreeEventSources         = errors.New("剧情树事件缺少来源引用")
	ErrPlotTreeEventSourceMismatch  = errors.New("剧情树事件状态与来源不匹配")
	ErrPlotTreeEventOutOfTrack      = errors.New("剧情树事件超出轨道章节范围")
	ErrPlotTreeTrackIDDuplicate     = errors.New("剧情树轨道 ID 重复")
	ErrPlotTreeMainParent           = errors.New("剧情树主线不能有父轨道")
	ErrPlotTreeSubplotParent        = errors.New("剧情树支线必须归属一条主线")
	ErrPlotTreeSubplotParentNotMain = errors.New("剧情树支线父轨道必须是现有主线")
	ErrPlotTreeLanguageMismatch     = errors.New("剧情树快照写作语言与当前项目不匹配")
	ErrPlotTreeRevisionMismatch     = errors.New("剧情树来源版本不匹配")
	ErrPlotTreeMissingSourceDomain  = errors.New("剧情树缺少有效事件来源")
	ErrPlotTreeTrackOutOfBounds     = errors.New("剧情树轨道章节范围超出当前剧情来源")
	ErrPlotTreeSourceReference      = errors.New("剧情树来源引用无效")
	ErrPlotTreeNarrativeReference   = errors.New("剧情树叙事来源引用无效")
	ErrPlotTreeSourceMissing        = errors.New("剧情树来源引用不存在")
)

// plotTreeTextInvalid mirrors `text(value, label)` (src/shared/plot-tree.ts:97-101),
// which builds `剧情树${label}无效` for the failing field.
func plotTreeTextInvalid(label string) error {
	return fmt.Errorf("剧情树%s无效", label)
}

// PlotTreeSourceReference mirrors PlotTreeSourceReference
// (src/shared/plot-tree.ts:19-31). It is a discriminated union in TypeScript, so
// the Go struct carries the members of every branch and JSONValue emits only
// the branch that applies.
type PlotTreeSourceReference struct {
	Type          string `json:"type"`
	DraftID       *int   `json:"draftId,omitempty"`
	PlanID        *int   `json:"planId,omitempty"`
	EventID       *int   `json:"eventId,omitempty"`
	ChapterNumber *int   `json:"chapterNumber,omitempty"`
}

// JSONValue mirrors the branch-specific object literals of sourceReference
// (src/shared/plot-tree.ts:192-223). The member order matters: de-duplication
// keys are JSON.stringify of these references (plot-tree-generation-pure.ts:188,240),
// so the Go port must produce the same strings.
func (reference PlotTreeSourceReference) JSONValue() any {
	members := Object{{Key: "type", Value: reference.Type}}
	switch reference.Type {
	case PlotTreeReferenceBlueprint:
		if reference.ChapterNumber != nil {
			members = append(members, Member{Key: "chapterNumber", Value: *reference.ChapterNumber})
		}
	case PlotTreeReferenceFinalizedChapter:
		if reference.DraftID != nil {
			members = append(members, Member{Key: "draftId", Value: *reference.DraftID})
		}
		if reference.ChapterNumber != nil {
			members = append(members, Member{Key: "chapterNumber", Value: *reference.ChapterNumber})
		}
	case PlotTreeReferenceNarrativeThread:
		if reference.PlanID != nil {
			members = append(members, Member{Key: "planId", Value: *reference.PlanID})
		}
		if reference.EventID != nil {
			members = append(members, Member{Key: "eventId", Value: *reference.EventID})
		}
		if reference.ChapterNumber != nil {
			members = append(members, Member{Key: "chapterNumber", Value: *reference.ChapterNumber})
		}
	}
	return members
}

// PlotTreeEvent mirrors PlotTreeEvent (src/shared/plot-tree.ts:33-38).
type PlotTreeEvent struct {
	Status        string                    `json:"status"`
	ChapterNumber int                       `json:"chapterNumber"`
	Summary       string                    `json:"summary"`
	Sources       []PlotTreeSourceReference `json:"sources"`
}

// PlotTreeTrack mirrors PlotTreeTrack (src/shared/plot-tree.ts:40-49).
type PlotTreeTrack struct {
	ID            string          `json:"id"`
	Title         string          `json:"title"`
	Role          string          `json:"role"`
	ParentTrackID *string         `json:"parentTrackId,omitempty"`
	StartChapter  int             `json:"startChapter"`
	EndChapter    int             `json:"endChapter"`
	Summary       string          `json:"summary"`
	Events        []PlotTreeEvent `json:"events"`
}

// PlotTreeSnapshot mirrors PlotTreeSnapshot (src/shared/plot-tree.ts:70-77).
type PlotTreeSnapshot struct {
	Version         int             `json:"version"`
	GeneratedAt     string          `json:"generatedAt"`
	WritingLanguage string          `json:"writingLanguage"`
	SourceRevision  *string         `json:"sourceRevision,omitempty"`
	Tracks          []PlotTreeTrack `json:"tracks"`
}

// PlotTreeDerivationKind mirrors PlotTreeDerivation['kind']
// (plot-tree-generation-pure.ts:351).
type PlotTreeDerivationKind string

// Derivation kinds (plot-tree-generation-pure.ts:351).
const (
	PlotTreeDerivationModel         PlotTreeDerivationKind = "model"
	PlotTreeDerivationNormalized    PlotTreeDerivationKind = "normalized"
	PlotTreeDerivationDeterministic PlotTreeDerivationKind = "deterministic"
)

// PlotTreeDerivation mirrors PlotTreeDerivation
// (plot-tree-generation-pure.ts:351).
type PlotTreeDerivation struct {
	Snapshot PlotTreeSnapshot        `json:"snapshot"`
	Kind     PlotTreeDerivationKind  `json:"kind"`
}

// plotTreeFencePattern mirrors /^```json\s*([\s\S]*?)\s*```$/iu
// (plot-tree-generation-pure.ts:143). The whitespace class is spelled out so
// that JavaScript's `\s` set is used instead of Go's RE2 `\s`.
var plotTreeFencePattern = regexp.MustCompile(
	"(?is)^" + "```json" +
		`[\t\n\v\f\r \x{00a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}]*` +
		`([\s\S]*?)` +
		`[\t\n\v\f\r \x{00a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}]*` +
		"```$",
)

// plotTreeSourceRevisionPattern mirrors /^[a-f0-9]{64}$/u
// (src/shared/plot-tree.ts:86-88).
var plotTreeSourceRevisionPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

// plotTreeTimestampLayouts approximates Date.parse for the timestamps stored in
// a snapshot. Go cannot accept every legacy Date.parse input (for example
// "March 5, 2020"), but every value the app writes is an ISO-8601 string.
var plotTreeTimestampLayouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02T15:04:05.000Z0700",
	"2006-01-02 15:04:05",
	"2006-01-02",
	time.RFC1123Z,
	time.RFC1123,
	time.RFC822Z,
	time.RFC822,
	time.ANSIC,
}

// ParsePlotTreeSnapshot mirrors parsePlotTreeSnapshot
// (plot-tree-generation-pure.ts:119-125).
func ParsePlotTreeSnapshot(content string, sources PlotTreeSourceBundle, generatedAt string) (PlotTreeSnapshot, error) {
	parsed, err := parsePlotTreeJSON(content)
	if err != nil {
		return PlotTreeSnapshot{}, err
	}
	return strictSnapshot(parsed["tracks"], sources, generatedAt)
}

// DerivePlotTreeSnapshot mirrors derivePlotTreeSnapshot
// (plot-tree-generation-pure.ts:353-361): a strict model snapshot first, then
// the tolerant normalization pass, then the deterministic source-backed tree.
// Only the final deterministic step can fail, exactly like the TypeScript where
// deterministicPlotTreeSnapshot throws PlotTreeSourceError.
func DerivePlotTreeSnapshot(content string, sources PlotTreeSourceBundle, generatedAt string) (PlotTreeDerivation, error) {
	parsed, parseErr := parsePlotTreeJSON(content)
	if parseErr == nil {
		if snapshot, err := strictSnapshot(parsed["tracks"], sources, generatedAt); err == nil {
			return PlotTreeDerivation{Snapshot: snapshot, Kind: PlotTreeDerivationModel}, nil
		}
		if snapshot, err := normalizePlotTreeSnapshot(parsed, sources, generatedAt); err == nil {
			return PlotTreeDerivation{Snapshot: snapshot, Kind: PlotTreeDerivationNormalized}, nil
		}
	}
	snapshot, err := deterministicPlotTreeSnapshot(sources, generatedAt)
	if err != nil {
		return PlotTreeDerivation{}, err
	}
	return PlotTreeDerivation{Snapshot: snapshot, Kind: PlotTreeDerivationDeterministic}, nil
}

// parsePlotTreeJSON mirrors parsePlotTreeJSON (plot-tree-generation-pure.ts:141-145):
// a single ```json fence is unwrapped, then the payload is JSON.parse'd. A
// payload that is not a JSON object yields an empty map, which fails the same
// way `parsed.tracks === undefined` does in TypeScript.
func parsePlotTreeJSON(content string) (map[string]any, error) {
	trimmed := jsTrim(content)
	payload := trimmed
	if fenced := plotTreeFencePattern.FindStringSubmatch(trimmed); fenced != nil {
		payload = jsTrim(fenced[1])
	}
	var parsed any
	if err := json.Unmarshal([]byte(payload), &parsed); err != nil {
		return nil, err
	}
	record, ok := jsonRecord(parsed)
	if !ok {
		return map[string]any{}, nil
	}
	return record, nil
}

// strictSnapshot mirrors strictSnapshot (plot-tree-generation-pure.ts:127-139):
// the caller's tracks are wrapped with the frozen identity fields of the source
// bundle, so validation always compares against the current project.
func strictSnapshot(tracks any, sources PlotTreeSourceBundle, generatedAt string) (PlotTreeSnapshot, error) {
	return AssertPlotTreeSnapshot(map[string]any{
		"version":          float64(1),
		"generatedAt":      generatedAt,
		"writingLanguage":  sources.WritingLanguage,
		"sourceRevision":   sources.SourceRevision,
		"tracks":           tracks,
	}, sources)
}

// HasUsablePlotTreeEventSource mirrors hasUsablePlotTreeEventSource
// (src/shared/plot-tree.ts:173-175).
func HasUsablePlotTreeEventSource(sources PlotTreeSourceBundle) bool {
	_, _, ok := plotTreeSourceChapterBounds(sources)
	return ok
}

// plotTreeSourceChapterBounds mirrors plotTreeSourceChapterBounds
// (src/shared/plot-tree.ts:130-171).
func plotTreeSourceChapterBounds(sources PlotTreeSourceBundle) (int, int, bool) {
	hasRange := false
	startChapter := 0
	endChapter := 0
	add := func(start, end int) {
		if !hasRange || start < startChapter {
			startChapter = start
		}
		if !hasRange || end > endChapter {
			endChapter = end
		}
		hasRange = true
	}
	for _, blueprint := range sources.Blueprints {
		if plotTreeValidChapterNumber(blueprint.ChapterNumber) &&
			(hasText(blueprint.Title) || hasText(blueprint.Purpose) || hasText(blueprint.KeyEvents)) {
			add(blueprint.ChapterNumber, blueprint.ChapterNumber)
		}
	}
	for _, chapter := range sources.FinalizedChapters {
		if isSafeIntegerValue(chapter.DraftID) && chapter.DraftID >= 1 &&
			plotTreeValidChapterNumber(chapter.ChapterNumber) &&
			(hasText(chapter.Title) || hasText(chapter.Summary)) {
			add(chapter.ChapterNumber, chapter.ChapterNumber)
		}
	}
	for _, thread := range sources.NarrativeThreads {
		if isSafeIntegerValue(thread.ID) && thread.ID >= 1 &&
			plotTreeValidChapterRange(thread.TargetStartChapter, thread.TargetEndChapter) &&
			hasText(thread.Title) && hasText(thread.Type) && hasText(thread.AuthorIntent) {
			add(thread.TargetStartChapter, thread.TargetEndChapter)
			for _, event := range thread.Events {
				if isSafeIntegerValue(event.ID) && event.ID >= 1 &&
					plotTreeValidChapterNumber(event.ChapterNumber) &&
					isNarrativeThreadEventType(event.Type) &&
					hasText(event.Evidence) && hasText(event.Reason) {
					add(event.ChapterNumber, event.ChapterNumber)
				}
			}
		}
	}
	return startChapter, endChapter, hasRange
}

// AssertPlotTreeSnapshotChapterBounds mirrors assertPlotTreeSnapshotChapterBounds
// (src/shared/plot-tree.ts:179-190).
func AssertPlotTreeSnapshotChapterBounds(snapshot PlotTreeSnapshot, sources PlotTreeSourceBundle) (PlotTreeSnapshot, error) {
	startChapter, endChapter, ok := plotTreeSourceChapterBounds(sources)
	if !ok {
		return PlotTreeSnapshot{}, ErrPlotTreeMissingSourceDomain
	}
	for _, track := range snapshot.Tracks {
		if track.StartChapter < startChapter || track.EndChapter > endChapter {
			return PlotTreeSnapshot{}, ErrPlotTreeTrackOutOfBounds
		}
	}
	return snapshot, nil
}

// AssertStoredPlotTreeSnapshot mirrors assertStoredPlotTreeSnapshot
// (src/shared/plot-tree.ts:274-372). `value` is a JSON-decoded value
// (map[string]any / []any / string / float64 / bool / nil), matching the
// TypeScript `unknown` parameter.
func AssertStoredPlotTreeSnapshot(value any) (PlotTreeSnapshot, error) {
	input, ok := jsonRecord(value)
	if !ok || !jsNumberEquals(input["version"], 1) || !isWritingLanguageValue(input["writingLanguage"]) {
		return PlotTreeSnapshot{}, ErrPlotTreeSnapshotVersion
	}
	writingLanguage := input["writingLanguage"].(string)
	generatedAt, err := plotTreeText(input["generatedAt"], "生成时间")
	if err != nil {
		return PlotTreeSnapshot{}, err
	}
	if !plotTreeTimestampValid(generatedAt) {
		return PlotTreeSnapshot{}, ErrPlotTreeGeneratedAt
	}
	var sourceRevision *string
	if rawRevision, present := input["sourceRevision"]; present {
		// `sourceRevision !== undefined` (line 298): JSON null counts as present.
		revision, isString := rawRevision.(string)
		if !isString || !plotTreeSourceRevisionPattern.MatchString(revision) {
			return PlotTreeSnapshot{}, ErrPlotTreeSourceRevision
		}
		sourceRevision = &revision
	}
	rawTracks, isArray := jsonArray(input["tracks"])
	if !isArray || len(rawTracks) == 0 {
		return PlotTreeSnapshot{}, ErrPlotTreeTracks
	}
	tracks := make([]PlotTreeTrack, 0, len(rawTracks))
	for _, candidate := range rawTracks {
		track, ok := jsonRecord(candidate)
		if !ok {
			return PlotTreeSnapshot{}, ErrPlotTreeTracks
		}
		// `['main','subplot'].includes(String(track.role))` (plot-tree.ts:298):
		// the coercion is kept, so a non-string role is compared by String().
		role := jsString(track["role"])
		if role != PlotTreeRoleMain && role != PlotTreeRoleSubplot {
			return PlotTreeSnapshot{}, ErrPlotTreeTracks
		}
		id, err := plotTreeText(track["id"], "轨道 ID")
		if err != nil {
			return PlotTreeSnapshot{}, err
		}
		title, err := plotTreeText(track["title"], "轨道标题")
		if err != nil {
			return PlotTreeSnapshot{}, err
		}
		summary, err := plotTreeText(track["summary"], "轨道摘要")
		if err != nil {
			return PlotTreeSnapshot{}, err
		}
		startChapter, err := plotTreeChapterInteger(track["startChapter"], "轨道章节范围")
		if err != nil {
			return PlotTreeSnapshot{}, err
		}
		endChapter, err := plotTreeChapterInteger(track["endChapter"], "轨道章节范围")
		if err != nil {
			return PlotTreeSnapshot{}, err
		}
		if endChapter < startChapter {
			return PlotTreeSnapshot{}, ErrPlotTreeTrackChapters
		}
		rawEvents, isArray := jsonArray(track["events"])
		if !isArray || len(rawEvents) == 0 {
			return PlotTreeSnapshot{}, ErrPlotTreeTrackEvents
		}
		events := make([]PlotTreeEvent, 0, len(rawEvents))
		for _, candidateEvent := range rawEvents {
			event, ok := jsonRecord(candidateEvent)
			if !ok {
				return PlotTreeSnapshot{}, ErrPlotTreeEvent
			}
			// `['planned','occurred'].includes(String(event.status))` (plot-tree.ts:313).
			status := jsString(event["status"])
			if status != PlotTreeEventStatusPlanned && status != PlotTreeEventStatusOccurred {
				return PlotTreeSnapshot{}, ErrPlotTreeEvent
			}
			rawSources, isArray := jsonArray(event["sources"])
			if !isArray || len(rawSources) == 0 {
				return PlotTreeSnapshot{}, ErrPlotTreeEventSources
			}
			chapterNumber, err := plotTreeChapterInteger(event["chapterNumber"], "事件章节")
			if err != nil {
				return PlotTreeSnapshot{}, err
			}
			eventSources := make([]PlotTreeSourceReference, 0, len(rawSources))
			for _, rawSource := range rawSources {
				source, err := plotTreeSourceReference(rawSource)
				if err != nil {
					return PlotTreeSnapshot{}, err
				}
				eventSources = append(eventSources, source)
			}
			for _, source := range eventSources {
				if !plotTreeSourceSupportsEvent(source, status, chapterNumber, nil) {
					return PlotTreeSnapshot{}, ErrPlotTreeEventSourceMismatch
				}
			}
			eventSummary, err := plotTreeText(event["summary"], "事件摘要")
			if err != nil {
				return PlotTreeSnapshot{}, err
			}
			events = append(events, PlotTreeEvent{
				Status:        status,
				ChapterNumber: chapterNumber,
				Summary:       eventSummary,
				Sources:       eventSources,
			})
		}
		for _, event := range events {
			if event.ChapterNumber < startChapter || event.ChapterNumber > endChapter {
				return PlotTreeSnapshot{}, ErrPlotTreeEventOutOfTrack
			}
		}
		var parentTrackID *string
		if rawParent, present := track["parentTrackId"]; present {
			parent, err := plotTreeText(rawParent, "父轨道 ID")
			if err != nil {
				return PlotTreeSnapshot{}, err
			}
			parentTrackID = &parent
		}
		tracks = append(tracks, PlotTreeTrack{
			ID:            id,
			Title:         title,
			Role:          role,
			ParentTrackID: parentTrackID,
			StartChapter:  startChapter,
			EndChapter:    endChapter,
			Summary:       summary,
			Events:        events,
		})
	}
	byID := make(map[string]PlotTreeTrack, len(tracks))
	for _, track := range tracks {
		if _, duplicate := byID[track.ID]; duplicate {
			return PlotTreeSnapshot{}, ErrPlotTreeTrackIDDuplicate
		}
		byID[track.ID] = track
	}
	for _, track := range tracks {
		if track.Role == PlotTreeRoleMain && track.ParentTrackID != nil {
			return PlotTreeSnapshot{}, ErrPlotTreeMainParent
		}
		if track.Role == PlotTreeRoleSubplot && track.ParentTrackID == nil {
			return PlotTreeSnapshot{}, ErrPlotTreeSubplotParent
		}
		if track.ParentTrackID != nil {
			parent, ok := byID[*track.ParentTrackID]
			if !ok || parent.Role != PlotTreeRoleMain {
				return PlotTreeSnapshot{}, ErrPlotTreeSubplotParentNotMain
			}
		}
	}
	return PlotTreeSnapshot{
		Version:         1,
		GeneratedAt:     generatedAt,
		WritingLanguage: writingLanguage,
		SourceRevision:  sourceRevision,
		Tracks:          tracks,
	}, nil
}

// AssertPlotTreeSnapshot mirrors assertPlotTreeSnapshot
// (src/shared/plot-tree.ts:374-395): the stored-snapshot validator plus
// language, revision, source-existence and chapter-domain checks against the
// project's current sources.
func AssertPlotTreeSnapshot(value any, sources PlotTreeSourceBundle) (PlotTreeSnapshot, error) {
	snapshot, err := AssertStoredPlotTreeSnapshot(value)
	if err != nil {
		return PlotTreeSnapshot{}, err
	}
	if snapshot.WritingLanguage != sources.WritingLanguage {
		return PlotTreeSnapshot{}, ErrPlotTreeLanguageMismatch
	}
	if snapshot.SourceRevision == nil || *snapshot.SourceRevision != sources.SourceRevision {
		return PlotTreeSnapshot{}, ErrPlotTreeRevisionMismatch
	}
	for _, track := range snapshot.Tracks {
		for _, event := range track.Events {
			for _, source := range event.Sources {
				if err := assertPlotTreeSourceExists(source, sources); err != nil {
					return PlotTreeSnapshot{}, err
				}
			}
			for _, source := range event.Sources {
				if !plotTreeSourceSupportsEvent(source, event.Status, event.ChapterNumber, &sources) {
					return PlotTreeSnapshot{}, ErrPlotTreeEventSourceMismatch
				}
			}
		}
	}
	return AssertPlotTreeSnapshotChapterBounds(snapshot, sources)
}

// plotTreeSourceReference mirrors sourceReference (src/shared/plot-tree.ts:192-223).
func plotTreeSourceReference(value any) (PlotTreeSourceReference, error) {
	input, ok := jsonRecord(value)
	if !ok {
		return PlotTreeSourceReference{}, ErrPlotTreeSourceReference
	}
	referenceType, _ := input["type"].(string)
	switch referenceType {
	case PlotTreeReferenceBlueprint:
		chapterNumber, err := plotTreeChapterInteger(input["chapterNumber"], "来源章节")
		if err != nil {
			return PlotTreeSourceReference{}, err
		}
		return PlotTreeSourceReference{Type: PlotTreeReferenceBlueprint, ChapterNumber: &chapterNumber}, nil
	case PlotTreeReferenceFinalizedChapter:
		draftID, err := plotTreePositiveInteger(input["draftId"], "来源定稿")
		if err != nil {
			return PlotTreeSourceReference{}, err
		}
		chapterNumber, err := plotTreeChapterInteger(input["chapterNumber"], "来源章节")
		if err != nil {
			return PlotTreeSourceReference{}, err
		}
		return PlotTreeSourceReference{
			Type:          PlotTreeReferenceFinalizedChapter,
			DraftID:       &draftID,
			ChapterNumber: &chapterNumber,
		}, nil
	case PlotTreeReferenceNarrativeThread:
		planID, err := plotTreePositiveInteger(input["planId"], "来源叙事计划")
		if err != nil {
			return PlotTreeSourceReference{}, err
		}
		reference := PlotTreeSourceReference{Type: PlotTreeReferenceNarrativeThread, PlanID: &planID}
		if rawEventID, present := input["eventId"]; present {
			eventID, err := plotTreePositiveInteger(rawEventID, "来源叙事事件")
			if err != nil {
				return PlotTreeSourceReference{}, err
			}
			reference.EventID = &eventID
		}
		if rawChapter, present := input["chapterNumber"]; present {
			chapterNumber, err := plotTreeChapterInteger(rawChapter, "来源章节")
			if err != nil {
				return PlotTreeSourceReference{}, err
			}
			reference.ChapterNumber = &chapterNumber
		}
		if (reference.EventID == nil) != (reference.ChapterNumber == nil) {
			return PlotTreeSourceReference{}, ErrPlotTreeNarrativeReference
		}
		return reference, nil
	}
	return PlotTreeSourceReference{}, ErrPlotTreeSourceReference
}

// plotTreeSourceSupportsEvent mirrors sourceSupportsEvent
// (src/shared/plot-tree.ts:225-247). A nil sources argument reproduces the
// TypeScript call that validates a stored snapshot without current sources.
func plotTreeSourceSupportsEvent(
	source PlotTreeSourceReference,
	status string,
	chapterNumber int,
	sources *PlotTreeSourceBundle,
) bool {
	switch source.Type {
	case PlotTreeReferenceBlueprint:
		return status == PlotTreeEventStatusPlanned &&
			source.ChapterNumber != nil && *source.ChapterNumber == chapterNumber
	case PlotTreeReferenceFinalizedChapter:
		return status == PlotTreeEventStatusOccurred &&
			source.ChapterNumber != nil && *source.ChapterNumber == chapterNumber
	}
	if source.EventID != nil {
		return status == PlotTreeEventStatusOccurred &&
			source.ChapterNumber != nil && *source.ChapterNumber == chapterNumber
	}
	if status != PlotTreeEventStatusPlanned {
		return false
	}
	if sources == nil {
		return true
	}
	plan, ok := findNarrativeThreadSource(*sources, source.PlanID)
	return ok &&
		chapterNumber >= plan.TargetStartChapter &&
		chapterNumber <= plan.TargetEndChapter
}

// assertPlotTreeSourceExists mirrors assertSourceExists
// (src/shared/plot-tree.ts:249-272).
func assertPlotTreeSourceExists(source PlotTreeSourceReference, sources PlotTreeSourceBundle) error {
	switch source.Type {
	case PlotTreeReferenceBlueprint:
		for _, blueprint := range sources.Blueprints {
			if blueprint.ChapterNumber == dereferenceInt(source.ChapterNumber) {
				return nil
			}
		}
		return ErrPlotTreeSourceMissing
	case PlotTreeReferenceFinalizedChapter:
		for _, chapter := range sources.FinalizedChapters {
			if chapter.DraftID == dereferenceInt(source.DraftID) &&
				chapter.ChapterNumber == dereferenceInt(source.ChapterNumber) {
				return nil
			}
		}
		return ErrPlotTreeSourceMissing
	}
	plan, ok := findNarrativeThreadSource(sources, source.PlanID)
	if !ok {
		return ErrPlotTreeSourceMissing
	}
	if source.EventID != nil {
		for _, event := range plan.Events {
			if event.ID == *source.EventID && event.ChapterNumber == dereferenceInt(source.ChapterNumber) {
				return nil
			}
		}
		return ErrPlotTreeSourceMissing
	}
	return nil
}

func findNarrativeThreadSource(sources PlotTreeSourceBundle, planID *int) (PlotTreeNarrativeThreadSource, bool) {
	for _, thread := range sources.NarrativeThreads {
		if thread.ID == dereferenceInt(planID) {
			return thread, true
		}
	}
	return PlotTreeNarrativeThreadSource{}, false
}

// normalizePlotTreeSnapshot mirrors normalizePlotTreeSnapshot
// (plot-tree-generation-pure.ts:171-200): drop structurally wrong tracks and
// events, de-duplicate event sources, and re-derive each track's chapter range
// before the strict validation runs again.
func normalizePlotTreeSnapshot(parsed map[string]any, sources PlotTreeSourceBundle, generatedAt string) (PlotTreeSnapshot, error) {
	rawTracks, _ := jsonArray(parsed["tracks"])
	normalized := make([]any, 0, len(rawTracks))
	for _, candidate := range rawTracks {
		track, ok := jsonRecord(candidate)
		if !ok {
			continue
		}
		rawEvents, ok := jsonArray(track["events"])
		if !ok {
			continue
		}
		seen := make(map[string]bool)
		events := make([]any, 0, len(rawEvents))
		for _, candidateEvent := range rawEvents {
			event, ok := jsonRecord(candidateEvent)
			if !ok {
				continue
			}
			status := jsString(event["status"])
			if status != PlotTreeEventStatusPlanned && status != PlotTreeEventStatusOccurred {
				continue
			}
			chapterNumber, ok := jsonSafeInteger(event["chapterNumber"])
			if !ok {
				continue
			}
			rawSources, ok := jsonArray(event["sources"])
			if !ok {
				continue
			}
			eventSources := make([]any, 0, len(rawSources))
			for _, rawSource := range rawSources {
				source, err := exactEventSource(rawSource, status, chapterNumber, sources, generatedAt)
				key := MustEncodeJSON(source)
				if err != nil || seen[key] {
					continue
				}
				seen[key] = true
				eventSources = append(eventSources, plotTreeSourceReferenceValue(source))
			}
			if len(eventSources) == 0 {
				continue
			}
			normalizedEvent := copyJSONRecord(event)
			normalizedEvent["status"] = status
			normalizedEvent["chapterNumber"] = float64(chapterNumber)
			normalizedEvent["sources"] = eventSources
			events = append(events, normalizedEvent)
		}
		if len(events) == 0 {
			continue
		}
		startChapter := 0
		endChapter := 0
		for index, event := range events {
			chapterNumber, _ := jsonSafeInteger(event.(map[string]any)["chapterNumber"])
			if index == 0 || chapterNumber < startChapter {
				startChapter = chapterNumber
			}
			if index == 0 || chapterNumber > endChapter {
				endChapter = chapterNumber
			}
		}
		normalizedTrack := copyJSONRecord(track)
		normalizedTrack["startChapter"] = float64(startChapter)
		normalizedTrack["endChapter"] = float64(endChapter)
		normalizedTrack["events"] = events
		normalized = append(normalized, normalizedTrack)
	}
	return strictSnapshot(normalized, sources, generatedAt)
}

// deterministicPlotTreeSnapshot mirrors deterministicPlotTreeSnapshot
// (plot-tree-generation-pure.ts:202-264): the source-backed fallback tree used
// when the model output cannot be salvaged. It throws NO_EVENT_SOURCES when
// nothing in the project can legally support an event.
func deterministicPlotTreeSnapshot(sources PlotTreeSourceBundle, generatedAt string) (PlotTreeSnapshot, error) {
	finalizedChapters := make([]PlotTreeFinalizedChapterSource, 0, len(sources.FinalizedChapters))
	for _, chapter := range sources.FinalizedChapters {
		if jsTrim(chapter.Summary) == "" && jsTrim(chapter.Title) == "" {
			continue
		}
		if _, err := exactEventSource(
			map[string]any{
				"type":          PlotTreeReferenceFinalizedChapter,
				"draftId":       float64(chapter.DraftID),
				"chapterNumber": float64(chapter.ChapterNumber),
			},
			PlotTreeEventStatusOccurred, chapter.ChapterNumber, sources, generatedAt,
		); err != nil {
			continue
		}
		finalizedChapters = append(finalizedChapters, chapter)
	}
	finalizedChapterNumbers := make(map[int]bool, len(finalizedChapters))
	for _, chapter := range finalizedChapters {
		finalizedChapterNumbers[chapter.ChapterNumber] = true
	}
	candidates := make([]PlotTreeEvent, 0, len(finalizedChapters)+len(sources.Blueprints)+len(sources.NarrativeThreads))
	for _, chapter := range finalizedChapters {
		candidates = append(candidates, PlotTreeEvent{
			Status:        PlotTreeEventStatusOccurred,
			ChapterNumber: chapter.ChapterNumber,
			Summary:       firstText(jsTrim(chapter.Summary), jsTrim(chapter.Title)),
			Sources: []PlotTreeSourceReference{{
				Type:          PlotTreeReferenceFinalizedChapter,
				DraftID:       intPointer(chapter.DraftID),
				ChapterNumber: intPointer(chapter.ChapterNumber),
			}},
		})
	}
	for _, blueprint := range sources.Blueprints {
		if finalizedChapterNumbers[blueprint.ChapterNumber] {
			continue
		}
		candidates = append(candidates, PlotTreeEvent{
			Status:        PlotTreeEventStatusPlanned,
			ChapterNumber: blueprint.ChapterNumber,
			Summary:       firstText(jsTrim(blueprint.KeyEvents), jsTrim(blueprint.Purpose), jsTrim(blueprint.Title)),
			Sources: []PlotTreeSourceReference{{
				Type:          PlotTreeReferenceBlueprint,
				ChapterNumber: intPointer(blueprint.ChapterNumber),
			}},
		})
	}
	for _, thread := range sources.NarrativeThreads {
		candidates = append(candidates, PlotTreeEvent{
			Status:        PlotTreeEventStatusPlanned,
			ChapterNumber: thread.TargetStartChapter,
			Summary:       firstText(jsTrim(thread.AuthorIntent), jsTrim(thread.Title)),
			Sources: []PlotTreeSourceReference{{
				Type:   PlotTreeReferenceNarrativeThread,
				PlanID: intPointer(thread.ID),
			}},
		})
		for _, event := range thread.Events {
			candidates = append(candidates, PlotTreeEvent{
				Status:        PlotTreeEventStatusOccurred,
				ChapterNumber: event.ChapterNumber,
				Summary:       firstText(jsTrim(event.Evidence), jsTrim(event.Reason)),
				Sources: []PlotTreeSourceReference{{
					Type:          PlotTreeReferenceNarrativeThread,
					PlanID:        intPointer(thread.ID),
					EventID:       intPointer(event.ID),
					ChapterNumber: intPointer(event.ChapterNumber),
				}},
			})
		}
	}
	seen := make(map[string]bool)
	sortedEvents := make([]PlotTreeEvent, 0, len(candidates))
	for _, event := range candidates {
		source, err := exactEventSource(
			plotTreeSourceReferenceValue(event.Sources[0]),
			event.Status, event.ChapterNumber, sources, generatedAt,
		)
		key := MustEncodeJSON(source)
		if event.Summary == "" || err != nil || seen[key] {
			continue
		}
		seen[key] = true
		sortedEvents = append(sortedEvents, PlotTreeEvent{
			Status:        event.Status,
			ChapterNumber: event.ChapterNumber,
			Summary:       event.Summary,
			Sources:       []PlotTreeSourceReference{source},
		})
	}
	if len(sortedEvents) == 0 {
		return PlotTreeSnapshot{}, ErrPlotTreeSource
	}
	// TypeScript sorts with String.prototype.localeCompare
	// (plot-tree-generation-pure.ts:245-249). Every comparable value here is
	// lowercase ASCII, where localeCompare and code-unit order agree.
	sort.SliceStable(sortedEvents, func(left, right int) bool {
		if sortedEvents[left].ChapterNumber != sortedEvents[right].ChapterNumber {
			return sortedEvents[left].ChapterNumber < sortedEvents[right].ChapterNumber
		}
		if sortedEvents[left].Status != sortedEvents[right].Status {
			return sortedEvents[left].Status < sortedEvents[right].Status
		}
		return MustEncodeJSON(sortedEvents[left].Sources[0]) <
			MustEncodeJSON(sortedEvents[right].Sources[0])
	})
	events := make([]any, 0, len(sortedEvents))
	for _, event := range sortedEvents {
		events = append(events, plotTreeEventValue(event))
	}
	track := map[string]any{
		"id":           "source-backed-progress",
		"title":        WritingLanguageText(sources.WritingLanguage, "剧情进展", "Plot progress"),
		"role":         PlotTreeRoleMain,
		"startChapter": float64(sortedEvents[0].ChapterNumber),
		"endChapter":   float64(sortedEvents[len(sortedEvents)-1].ChapterNumber),
		"summary": WritingLanguageText(
			sources.WritingLanguage,
			"基于当前章节与叙事线索的只读进展。",
			"Read-only progress from current chapters and narrative threads.",
		),
		"events": events,
	}
	return strictSnapshot([]any{track}, sources, generatedAt)
}

// exactEventSource mirrors exactEventSource (plot-tree-generation-pure.ts:153-169):
// the candidate source is proven by validating it inside a synthetic one-event
// snapshot against the current sources; any failure means "not usable".
func exactEventSource(
	value any,
	status string,
	chapterNumber int,
	sources PlotTreeSourceBundle,
	generatedAt string,
) (PlotTreeSourceReference, error) {
	synthetic := map[string]any{
		"id":            "source-check",
		"title":         "source-check",
		"role":          PlotTreeRoleMain,
		"startChapter":  float64(chapterNumber),
		"endChapter":    float64(chapterNumber),
		"summary":       "source-check",
		"events": []any{map[string]any{
			"status":        status,
			"chapterNumber": float64(chapterNumber),
			"summary":       "source-check",
			"sources":       []any{value},
		}},
	}
	snapshot, err := strictSnapshot([]any{synthetic}, sources, generatedAt)
	if err != nil {
		return PlotTreeSourceReference{}, err
	}
	if len(snapshot.Tracks) == 0 || len(snapshot.Tracks[0].Events) == 0 ||
		len(snapshot.Tracks[0].Events[0].Sources) == 0 {
		return PlotTreeSourceReference{}, ErrPlotTreeSourceReference
	}
	return snapshot.Tracks[0].Events[0].Sources[0], nil
}

// plotTreeSourceReferenceValue renders a validated reference as JSON-decoded
// values so it can be validated again by AssertPlotTreeSnapshot.
func plotTreeSourceReferenceValue(reference PlotTreeSourceReference) map[string]any {
	value := map[string]any{"type": reference.Type}
	if reference.DraftID != nil {
		value["draftId"] = float64(*reference.DraftID)
	}
	if reference.PlanID != nil {
		value["planId"] = float64(*reference.PlanID)
	}
	if reference.EventID != nil {
		value["eventId"] = float64(*reference.EventID)
	}
	if reference.ChapterNumber != nil {
		value["chapterNumber"] = float64(*reference.ChapterNumber)
	}
	return value
}

// plotTreeEventValue renders a validated event as JSON-decoded values.
func plotTreeEventValue(event PlotTreeEvent) map[string]any {
	sources := make([]any, 0, len(event.Sources))
	for _, source := range event.Sources {
		sources = append(sources, plotTreeSourceReferenceValue(source))
	}
	return map[string]any{
		"status":        event.Status,
		"chapterNumber": float64(event.ChapterNumber),
		"summary":       event.Summary,
		"sources":       sources,
	}
}

// plotTreeText mirrors `text` (src/shared/plot-tree.ts:97-101).
func plotTreeText(value any, label string) (string, error) {
	text, ok := value.(string)
	if !ok {
		return "", plotTreeTextInvalid(label)
	}
	trimmed := jsTrim(text)
	if trimmed == "" {
		return "", plotTreeTextInvalid(label)
	}
	return trimmed, nil
}

// plotTreePositiveInteger mirrors positiveInteger (src/shared/plot-tree.ts:103-106).
func plotTreePositiveInteger(value any, label string) (int, error) {
	number, ok := jsonSafeInteger(value)
	if !ok || number < 1 {
		return 0, plotTreeTextInvalid(label)
	}
	return number, nil
}

// plotTreeChapterInteger mirrors chapterInteger (src/shared/plot-tree.ts:115-118).
func plotTreeChapterInteger(value any, label string) (int, error) {
	number, ok := jsonSafeInteger(value)
	if !ok || !plotTreeValidChapterNumber(number) {
		return 0, plotTreeTextInvalid(label)
	}
	return number, nil
}

// plotTreeValidChapterNumber mirrors validChapterNumber
// (src/shared/plot-tree.ts:108-113).
func plotTreeValidChapterNumber(value int) bool {
	return value >= 1 && value <= MaxPlotTreeChapterNumber
}

// plotTreeValidChapterRange mirrors validChapterRange
// (src/shared/plot-tree.ts:124-128).
func plotTreeValidChapterRange(start, end int) bool {
	return plotTreeValidChapterNumber(start) && plotTreeValidChapterNumber(end) && end >= start
}

// hasText mirrors nonEmptyString (src/shared/plot-tree.ts:120-122).
func hasText(value string) bool {
	return jsTrim(value) != ""
}

func plotTreeTimestampValid(value string) bool {
	for _, layout := range plotTreeTimestampLayouts {
		if _, err := time.Parse(layout, value); err == nil {
			return true
		}
	}
	return false
}

func isWritingLanguageValue(value any) bool {
	language, ok := value.(string)
	if !ok {
		return false
	}
	for _, candidate := range WritingLanguages {
		if candidate == language {
			return true
		}
	}
	return false
}

func jsNumberEquals(value any, expected float64) bool {
	number, ok := jsonSafeInteger(value)
	return ok && float64(number) == expected
}

func copyJSONRecord(record map[string]any) map[string]any {
	copied := make(map[string]any, len(record)+3)
	for key, value := range record {
		copied[key] = value
	}
	return copied
}

// firstText mirrors the `a || b || c` chain the deterministic tree uses on
// trimmed strings: the first non-empty value wins.
func firstText(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func intPointer(value int) *int {
	return &value
}

func dereferenceInt(value *int) int {
	if value == nil {
		return 0
	}
	return *value
}

// isSafeIntegerValue mirrors Number.isSafeInteger for a Go int (plot-tree.ts:107,138,154).
func isSafeIntegerValue(value int) bool {
	return value <= 1<<53-1 && value >= -(1<<53-1)
}
