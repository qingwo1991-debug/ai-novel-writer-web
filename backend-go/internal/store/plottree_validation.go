package store

import (
	"encoding/json"
	"errors"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// These are the shared plot-tree.ts validators, not a generator or a schema
// inferred from one. Stored snapshots deliberately need no current sources.
// The original has no track/event/text count ceiling: all traversal here is
// iterative and integer admission precedes conversion (including HTTP Numbers).
const plotMaxChapter = int64(4294967295)
const plotMaxSafeInteger = float64(9007199254740991)

func plotError(message string) error { return errors.New("剧情树" + message) }
func plotRecord(value any) map[string]any { record, _ := value.(map[string]any); return record }
func plotArray(value any) ([]any, bool) {
	switch values := value.(type) {
	case []any: return values, true
	case []map[string]any:
		result := make([]any, len(values)); for i := range values { result[i] = values[i] }; return result, true
	}
	return nil, false
}
func plotNumber(value any) (float64, bool) {
	switch number := value.(type) {
	case json.Number: n, err := number.Float64(); return n, err == nil
	case float64: return number, true
	case float32: return float64(number), true
	case int: return float64(number), true
	case int8: return float64(number), true
	case int16: return float64(number), true
	case int32: return float64(number), true
	case int64: return float64(number), true
	case uint: return float64(number), true
	case uint8: return float64(number), true
	case uint16: return float64(number), true
	case uint32: return float64(number), true
	case uint64: return float64(number), true
	}
	return 0, false
}
func plotInteger(value any, max float64, label string) (int64, error) {
	n, ok := plotNumber(value)
	if !ok || math.IsNaN(n) || math.IsInf(n, 0) || n < 1 || n > max || math.Trunc(n) != n { return 0, plotError(label + "无效") }
	return int64(n), nil
}
func plotChapter(value any, label string) (int64, error) { return plotInteger(value, float64(plotMaxChapter), label) }
func plotText(value any, label string) (string, error) {
	text, ok := value.(string)
	if !ok { return "", plotError(label + "无效") }
	text = strings.TrimFunc(text, narrativeSpace)
	if text == "" { return "", plotError(label + "无效") }
	return text, nil
}
func plotStringIs(value any, expected string) bool { text, ok := value.(string); return ok && text == expected }
func plotNumericIs(value any, expected int64) bool { n, ok := plotNumber(value); return ok && n == float64(expected) }

// JS String(array) is Array.join, including nested arrays. The enum gates in
// the original use String but return the original role/status value unchanged.
// Do not accidentally tighten this observable normal-JSON behavior.
func plotEnumString(value any) string {
	if values, ok := plotArray(value); ok {
		// Iterative join avoids stack growth on a model-controlled nested array.
		type frame struct { values []any; index int }
		stack := []frame{{values: values}}
		var out strings.Builder
		for len(stack) != 0 {
			f := &stack[len(stack)-1]
			if f.index == len(f.values) { stack = stack[:len(stack)-1]; continue }
			if f.index != 0 { out.WriteByte(',') }
			item := f.values[f.index]; f.index++
			if nested, ok := plotArray(item); ok { stack = append(stack, frame{values: nested}); continue }
			if item != nil { out.WriteString(plotEnumString(item)) }
		}
		return out.String()
	}
	switch value := value.(type) {
	case string: return value
	case nil: return "null"
	case bool: if value { return "true" }; return "false"
	}
	if number, ok := plotNumber(value); ok { return phNumberString(number) }
	return "[object Object]"
}

var plotRevisionPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
var plotISODatePattern = regexp.MustCompile(`^([+-][0-9]{6}|[0-9]{4})(?:-([0-9]{2})(?:-([0-9]{2}))?)?(?:T([0-9]{2}):([0-9]{2})(?::([0-9]{2})(?:\.([0-9]+))?)?(Z|[+-][0-9]{2}:?[0-9]{2})?)?$`)
var plotLegacyDatePattern = regexp.MustCompile(`^([0-9]{1,6})[-/., ]+([0-9]{1,2})(?:[-/., ]+([0-9]{1,6}))?$`)
var plotLegacyMonthPattern = regexp.MustCompile(`(?i)\b(jan|feb|mar|apr|may|jun|jul|aug|sep|oct|nov|dec)[a-z]*\b`)
var plotLegacyDigitsPattern = regexp.MustCompile(`[0-9]+`)
var plotDateCommentPattern = regexp.MustCompile(`\([^()]*\)`)

func plotDateParts(year, month, day, hour, minute, second, millis, offset int) bool {
	if year < -271821 || year > 275760 || month < 1 || month > 12 || day < 1 || day > 31 || hour < 0 || hour > 24 || minute > 59 || second > 59 { return false }
	if hour == 24 && (minute != 0 || second != 0 || millis != 0) { return false }
	stamp := time.Date(year, time.Month(month), day, hour, minute, second, millis*1000000, time.UTC).UnixMilli() - int64(offset)*60000
	return stamp >= -8640000000000000 && stamp <= 8640000000000000
}
func plotDateInt(text string) int { n, _ := strconv.Atoi(text); return n }
func plotLegacyYear(year int) int { if year < 50 { return year + 2000 }; if year < 100 { return year + 1900 }; return year }

// Date.parse is an admission check, not a date normalizer. Preserve the supplied
// trimmed string and accept ISO reduced precision, calendar rollovers, 24:00,
// expanded years and the original's common legacy/RFC forms as well as RFC3339.
func plotDateValid(value string) bool {
	if m := plotISODatePattern.FindStringSubmatch(value); m != nil {
		if m[1] == "-000000" { return false }
		year, month, day := plotDateInt(m[1]), 1, 1
		if m[2] != "" { month = plotDateInt(m[2]) }; if m[3] != "" { day = plotDateInt(m[3]) }
		if m[4] != "" && (m[2] == "" || m[3] == "") { return false }
		millis := 0; if m[7] != "" { millis = plotDateInt((m[7]+"000")[:3]) }
		offset := 0
		if zone := m[8]; len(zone) > 1 {
			zoneDigits := strings.ReplaceAll(zone[1:], ":", "")
			h, min := plotDateInt(zoneDigits[:2]), plotDateInt(zoneDigits[2:]); if h > 23 || min > 59 { return false }
			offset = h*60+min; if zone[0] == '-' { offset = -offset }
		}
		return plotDateParts(year, month, day, plotDateInt(m[4]), plotDateInt(m[5]), plotDateInt(m[6]), millis, offset)
	}
	// A failed ISO time form must not be rescued by the legacy parser.
	if strings.Contains(value, "T") && !strings.Contains(value, "GMT") && !strings.Contains(value, "UTC") { return false }
	legacy := value
	for { stripped := plotDateCommentPattern.ReplaceAllString(legacy, ""); if stripped == legacy { break }; legacy = stripped }
	legacy = strings.TrimFunc(legacy, narrativeSpace)
	for _, layout := range []string{time.RFC1123, time.RFC1123Z, time.RFC822, time.RFC822Z, time.RFC850, time.ANSIC, time.UnixDate,
		"Mon Jan 02 2006 15:04:05 GMT-0700", "Mon Jan 02 2006 15:04:05 GMT-0700 MST", "January 2, 2006", "Jan 2, 2006", "2 Jan 2006", "Jan 2 2006", "2006-1-2 15:04:05", "2006-1-2 15:04", "2006/1/2 15:04:05", "1/2/2006 15:04:05", "2006-1-2T15:04:05Z07:00"} {
		if date, err := time.Parse(layout, legacy); err == nil { return plotDateParts(date.Year(), int(date.Month()), date.Day(), date.Hour(), date.Minute(), date.Second(), date.Nanosecond()/1000000, 0) }
	}
	if n, err := strconv.Atoi(legacy); err == nil && !strings.HasPrefix(legacy, "-") {
		switch { case len(legacy) >= 3: return plotDateParts(n, 1, 1, 0, 0, 0, 0, 0)
		case n == 0: return true
		case n <= 12: return n >= 1
		case n < 32: return false
		default: return plotDateParts(plotLegacyYear(n), 1, 1, 0, 0, 0, 0, 0) }
	}
	if m := plotLegacyDatePattern.FindStringSubmatch(legacy); m != nil {
		a, b, c := plotDateInt(m[1]), plotDateInt(m[2]), plotDateInt(m[3])
		if a >= 32 { if m[3] == "" { c = 1 }; return plotDateParts(plotLegacyYear(a), b, c, 0, 0, 0, 0, 0) }
		if m[3] == "" { c = 2001 }; return plotDateParts(plotLegacyYear(c), a, b, 0, 0, 0, 0, 0)
	}
	// Month-word date forms permit long month names and ignored weekday words.
	if month := plotLegacyMonthPattern.FindStringSubmatch(legacy); month != nil && !strings.Contains(legacy, ":") {
		monthNumber := strings.Index("janfebmaraprmayjunjulaugsepoctnovdec", strings.ToLower(month[1]))/3 + 1
		digits := plotLegacyDigitsPattern.FindAllString(legacy, -1)
		if len(digits) == 1 { return plotDateParts(2001, monthNumber, plotDateInt(digits[0]), 0, 0, 0, 0, 0) }
		if len(digits) == 2 { a, b := plotDateInt(digits[0]), plotDateInt(digits[1]); if a >= 32 { a, b = b, a }; return plotDateParts(plotLegacyYear(b), monthNumber, a, 0, 0, 0, 0, 0) }
	}
	return false
}

func plotSourceReference(value any) (map[string]any, error) {
	input := plotRecord(value)
	if input == nil { return nil, plotError("来源引用无效") }
	if plotStringIs(input["type"], "blueprint") {
		chapter, err := plotChapter(input["chapterNumber"], "来源章节"); if err != nil { return nil, err }
		return map[string]any{"type": "blueprint", "chapterNumber": chapter}, nil
	}
	if plotStringIs(input["type"], "finalized-chapter") {
		id, err := plotInteger(input["draftId"], plotMaxSafeInteger, "来源定稿"); if err != nil { return nil, err }
		chapter, err := plotChapter(input["chapterNumber"], "来源章节"); if err != nil { return nil, err }
		return map[string]any{"type": "finalized-chapter", "draftId": id, "chapterNumber": chapter}, nil
	}
	if plotStringIs(input["type"], "narrative-thread") {
		id, err := plotInteger(input["planId"], plotMaxSafeInteger, "来源叙事计划"); if err != nil { return nil, err }
		result := map[string]any{"type": "narrative-thread", "planId": id}
		_, hasEvent := input["eventId"]; _, hasChapter := input["chapterNumber"]
		if hasEvent { event, err := plotInteger(input["eventId"], plotMaxSafeInteger, "来源叙事事件"); if err != nil { return nil, err }; result["eventId"] = event }
		if hasChapter { chapter, err := plotChapter(input["chapterNumber"], "来源章节"); if err != nil { return nil, err }; result["chapterNumber"] = chapter }
		if hasEvent != hasChapter { return nil, plotError("叙事来源引用无效") }
		return result, nil
	}
	return nil, plotError("来源引用无效")
}

func plotFindPlan(sources map[string]any, id int64) map[string]any {
	plans, _ := plotArray(sources["narrativeThreads"])
	for _, candidate := range plans { plan := plotRecord(candidate); if plotNumericIs(plan["id"], id) { return plan } }
	return nil
}
func plotCompareNumber(value any) float64 {
	if n, ok := plotNumber(value); ok { return n }
	if value == nil { return 0 }
	if b, ok := value.(bool); ok { if b { return 1 }; return 0 }
	if values, ok := plotArray(value); ok { value = plotEnumString(values) }
	if text, ok := value.(string); ok {
		text = strings.TrimFunc(text, narrativeSpace); if text == "" { return 0 }
		if len(text) > 2 && text[0] == '0' { base := 0; switch text[1] { case 'x', 'X': base = 16; case 'b', 'B': base = 2; case 'o', 'O': base = 8 }; if base != 0 { if n, err := strconv.ParseUint(text[2:], base, 64); err == nil { return float64(n) } } }
		if n, err := strconv.ParseFloat(text, 64); err == nil { return n }
	}
	return math.NaN()
}
func plotSupports(source map[string]any, status any, chapter int64, sources map[string]any) bool {
	if plotStringIs(source["type"], "blueprint") { return plotStringIs(status, "planned") && plotNumericIs(source["chapterNumber"], chapter) }
	if plotStringIs(source["type"], "finalized-chapter") { return plotStringIs(status, "occurred") && plotNumericIs(source["chapterNumber"], chapter) }
	if _, exists := source["eventId"]; exists { return plotStringIs(status, "occurred") && plotNumericIs(source["chapterNumber"], chapter) }
	if !plotStringIs(status, "planned") { return false }
	if sources == nil { return true }
	plan := plotFindPlan(sources, source["planId"].(int64))
	if plan == nil { return false }
	start, startPresent := plan["targetStartChapter"]; end, endPresent := plan["targetEndChapter"]
	return startPresent && endPresent && float64(chapter) >= plotCompareNumber(start) && float64(chapter) <= plotCompareNumber(end)
}

func plotValidateStored(value any) (map[string]any, error) {
	input := plotRecord(value)
	if input == nil || !plotNumericIs(input["version"], 1) || (!plotStringIs(input["writingLanguage"], "zh-CN") && !plotStringIs(input["writingLanguage"], "en-US")) { return nil, plotError("快照版本或写作语言无效") }
	generated, err := plotText(input["generatedAt"], "生成时间"); if err != nil { return nil, err }
	if !plotDateValid(generated) { return nil, plotError("生成时间无效") }
	revision, hasRevision := input["sourceRevision"]
	if hasRevision { text, ok := revision.(string); if !ok || !plotRevisionPattern.MatchString(text) { return nil, plotError("来源版本无效") } }
	candidates, ok := plotArray(input["tracks"]); if !ok || len(candidates) == 0 { return nil, plotError("轨道无效") }
	tracks := make([]any, 0, len(candidates))
	for _, candidate := range candidates {
		track := plotRecord(candidate)
		if track == nil || (plotEnumString(track["role"]) != "main" && plotEnumString(track["role"]) != "subplot") { return nil, plotError("轨道无效") }
		id, err := plotText(track["id"], "轨道 ID"); if err != nil { return nil, err }
		title, err := plotText(track["title"], "轨道标题"); if err != nil { return nil, err }
		summary, err := plotText(track["summary"], "轨道摘要"); if err != nil { return nil, err }
		start, err := plotChapter(track["startChapter"], "轨道章节范围"); if err != nil { return nil, err }
		end, err := plotChapter(track["endChapter"], "轨道章节范围"); if err != nil { return nil, err }; if end < start { return nil, plotError("轨道章节范围无效") }
		eventCandidates, ok := plotArray(track["events"]); if !ok || len(eventCandidates) == 0 { return nil, plotError("轨道事件无效") }
		events := make([]any, 0, len(eventCandidates))
		for _, candidateEvent := range eventCandidates {
			event := plotRecord(candidateEvent)
			if event == nil || (plotEnumString(event["status"]) != "planned" && plotEnumString(event["status"]) != "occurred") { return nil, plotError("事件无效") }
			sourceCandidates, ok := plotArray(event["sources"]); if !ok || len(sourceCandidates) == 0 { return nil, plotError("事件缺少来源引用") }
			chapter, err := plotChapter(event["chapterNumber"], "事件章节"); if err != nil { return nil, err }
			sourceRefs := make([]any, 0, len(sourceCandidates))
			for _, candidate := range sourceCandidates { source, err := plotSourceReference(candidate); if err != nil { return nil, err }; sourceRefs = append(sourceRefs, source) }
			for _, source := range sourceRefs { if !plotSupports(source.(map[string]any), event["status"], chapter, nil) { return nil, plotError("事件状态与来源不匹配") } }
			eventSummary, err := plotText(event["summary"], "事件摘要"); if err != nil { return nil, err }
			events = append(events, map[string]any{"status": event["status"], "chapterNumber": chapter, "summary": eventSummary, "sources": sourceRefs})
		}
		for _, event := range events { chapter := event.(map[string]any)["chapterNumber"].(int64); if chapter < start || chapter > end { return nil, plotError("事件超出轨道章节范围") } }
		normalized := map[string]any{"id": id, "title": title, "role": track["role"], "startChapter": start, "endChapter": end, "summary": summary, "events": events}
		if parent, exists := track["parentTrackId"]; exists { parent, err := plotText(parent, "父轨道 ID"); if err != nil { return nil, err }; normalized["parentTrackId"] = parent }
		tracks = append(tracks, normalized)
	}
	byID := make(map[string]map[string]any, len(tracks))
	for _, candidate := range tracks { track := candidate.(map[string]any); id := track["id"].(string); if _, exists := byID[id]; exists { return nil, plotError("轨道 ID 重复") }; byID[id] = track }
	for _, candidate := range tracks {
		track := candidate.(map[string]any); parent, hasParent := track["parentTrackId"].(string)
		if plotStringIs(track["role"], "main") && hasParent { return nil, plotError("主线不能有父轨道") }
		if plotStringIs(track["role"], "subplot") && !hasParent { return nil, plotError("支线必须归属一条主线") }
		if hasParent && !plotStringIs(byID[parent]["role"], "main") { return nil, plotError("支线父轨道必须是现有主线") }
	}
	result := map[string]any{"version": int64(1), "generatedAt": generated, "writingLanguage": input["writingLanguage"], "tracks": tracks}
	if hasRevision { result["sourceRevision"] = revision }
	return result, nil
}

func plotSourceExists(source map[string]any, sources map[string]any) bool {
	if plotStringIs(source["type"], "blueprint") {
		values, _ := plotArray(sources["blueprints"])
		for _, value := range values { if plotNumericIs(plotRecord(value)["chapterNumber"], source["chapterNumber"].(int64)) { return true } }; return false
	}
	if plotStringIs(source["type"], "finalized-chapter") {
		values, _ := plotArray(sources["finalizedChapters"])
		for _, value := range values { chapter := plotRecord(value); if plotNumericIs(chapter["draftId"], source["draftId"].(int64)) && plotNumericIs(chapter["chapterNumber"], source["chapterNumber"].(int64)) { return true } }; return false
	}
	plan := plotFindPlan(sources, source["planId"].(int64)); if plan == nil { return false }
	if id, exists := source["eventId"]; exists {
		events, _ := plotArray(plan["events"])
		for _, value := range events { event := plotRecord(value); if plotNumericIs(event["id"], id.(int64)) && plotNumericIs(event["chapterNumber"], source["chapterNumber"].(int64)) { return true } }; return false
	}
	return true
}
func plotNonEmpty(value any) bool { _, err := plotText(value, ""); return err == nil }
func plotSourceBounds(sources map[string]any) (int64, int64, bool) {
	start, end, found := plotMaxChapter, int64(0), false
	add := func(a, b int64) { if a < start { start = a }; if b > end { end = b }; found = true }
	blueprints, _ := plotArray(sources["blueprints"])
	for _, value := range blueprints { source := plotRecord(value); chapter, err := plotChapter(source["chapterNumber"], ""); if err == nil && (plotNonEmpty(source["title"]) || plotNonEmpty(source["purpose"]) || plotNonEmpty(source["keyEvents"])) { add(chapter, chapter) } }
	chapters, _ := plotArray(sources["finalizedChapters"])
	for _, value := range chapters { source := plotRecord(value); _, idErr := plotInteger(source["draftId"], plotMaxSafeInteger, ""); chapter, err := plotChapter(source["chapterNumber"], ""); if idErr == nil && err == nil && (plotNonEmpty(source["title"]) || plotNonEmpty(source["summary"])) { add(chapter, chapter) } }
	threads, _ := plotArray(sources["narrativeThreads"])
	for _, value := range threads {
		source := plotRecord(value); _, idErr := plotInteger(source["id"], plotMaxSafeInteger, ""); a, aErr := plotChapter(source["targetStartChapter"], ""); b, bErr := plotChapter(source["targetEndChapter"], "")
		if idErr != nil || aErr != nil || bErr != nil || b < a || !plotNonEmpty(source["title"]) || !plotNonEmpty(source["type"]) || !plotNonEmpty(source["authorIntent"]) { continue }
		add(a, b)
		events, _ := plotArray(source["events"])
		for _, value := range events {
			event := plotRecord(value); _, idErr := plotInteger(event["id"], plotMaxSafeInteger, ""); chapter, err := plotChapter(event["chapterNumber"], "")
			kind, _ := event["type"].(string)
			if idErr == nil && err == nil && (kind == "planted" || kind == "progressing" || kind == "resolved" || kind == "abandoned") && plotNonEmpty(event["evidence"]) && plotNonEmpty(event["reason"]) { add(chapter, chapter) }
		}
	}
	return start, end, found
}
func plotValidate(value any, sources map[string]any) (map[string]any, error) {
	snapshot, err := plotValidateStored(value); if err != nil { return nil, err }
	if !plotStringIs(sources["writingLanguage"], snapshot["writingLanguage"].(string)) { return nil, plotError("快照写作语言与当前项目不匹配") }
	revision, hasRevision := snapshot["sourceRevision"]; sourceRevision, hasSourceRevision := sources["sourceRevision"]
	if hasRevision != hasSourceRevision || (hasRevision && !plotStringIs(sourceRevision, revision.(string))) { return nil, plotError("来源版本不匹配") }
	for _, value := range snapshot["tracks"].([]any) {
		for _, value := range value.(map[string]any)["events"].([]any) {
			event := value.(map[string]any); refs := event["sources"].([]any)
			for _, source := range refs { if !plotSourceExists(source.(map[string]any), sources) { return nil, plotError("来源引用不存在") } }
			for _, source := range refs { if !plotSupports(source.(map[string]any), event["status"], event["chapterNumber"].(int64), sources) { return nil, plotError("事件状态与来源不匹配") } }
		}
	}
	start, end, found := plotSourceBounds(sources); if !found { return nil, plotError("缺少有效事件来源") }
	for _, value := range snapshot["tracks"].([]any) { track := value.(map[string]any); if track["startChapter"].(int64) < start || track["endChapter"].(int64) > end { return nil, plotError("轨道章节范围超出当前剧情来源") } }
	return snapshot, nil
}
