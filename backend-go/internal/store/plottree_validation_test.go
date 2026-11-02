package store

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"reflect"
	"strings"
	"testing"
)

func plotTestSnapshot() map[string]any {
	return map[string]any{
		"version": 1, "generatedAt": "2026-01-02T03:04:05.678Z", "writingLanguage": "zh-CN", "sourceRevision": strings.Repeat("a1", 32),
		"tracks": []any{map[string]any{
			"id": "main", "title": "星钥主线", "role": "main", "startChapter": 3, "endChapter": 9, "summary": "主线摘要 😀",
			"events": []any{map[string]any{"status": "planned", "chapterNumber": 5, "summary": "发现星钥 😀", "sources": []any{map[string]any{"type": "blueprint", "chapterNumber": 5}}}},
		}},
	}
}
func plotTestTrack(value map[string]any) map[string]any { return value["tracks"].([]any)[0].(map[string]any) }
func plotTestEvent(value map[string]any) map[string]any { return plotTestTrack(value)["events"].([]any)[0].(map[string]any) }
func plotTestRef(value map[string]any) map[string]any { return plotTestEvent(value)["sources"].([]any)[0].(map[string]any) }
func plotTestSources() map[string]any {
	return map[string]any{
		"writingLanguage": "zh-CN", "sourceRevision": strings.Repeat("a1", 32),
		"blueprints": []map[string]any{{"chapterNumber": int64(3), "title": "开篇"}, {"chapterNumber": int64(5), "purpose": "发现星钥"}, {"chapterNumber": int64(9), "keyEvents": "收束"}},
		"finalizedChapters": []map[string]any{},
		"narrativeThreads": []map[string]any{{"id": int64(7), "title": "线索", "type": "伏笔", "authorIntent": "揭晓", "targetStartChapter": int64(3), "targetEndChapter": int64(9),
			"events": []map[string]any{{"id": int64(17), "chapterNumber": int64(20), "type": "resolved", "evidence": "证据", "reason": "解释"}}}},
	}
}
func plotTestJSON(t *testing.T, value any) any {
	t.Helper(); data, err := json.Marshal(value); if err != nil { t.Fatal(err) }
	var normalized any; if err := json.Unmarshal(data, &normalized); err != nil { t.Fatal(err) }; return normalized
}
func plotTestError(t *testing.T, value any, expected string) {
	t.Helper(); result, err := plotValidateStored(value)
	if err == nil || err.Error() != expected || result != nil { t.Fatalf("got result=%#v error=%v; want %s", result, err, expected) }
}

func TestPlotValidationNativeWhitespaceAndNormalizedOwnership(t *testing.T) {
	value := plotTestSnapshot(); track, event, ref := plotTestTrack(value), plotTestEvent(value), plotTestRef(value)
	value["extra"] = true; track["extra"] = 1; event["extra"] = "x"; ref["extra"] = []any{1}
	track["id"] = "\uFEFF \tmain\n\uFEFF"; track["title"] = "\uFEFF\u0085😀标题\u0085\uFEFF"
	track["summary"] = " 甲\t  乙\n😀 "; event["summary"] = "\u0085"
	before := plotTestJSON(t, value)
	result, err := plotValidateStored(value); if err != nil { t.Fatal(err) }
	if !reflect.DeepEqual(before, plotTestJSON(t, value)) { t.Fatal("validator mutated input") }
	outTrack := result["tracks"].([]any)[0].(map[string]any); outEvent := outTrack["events"].([]any)[0].(map[string]any); outRef := outEvent["sources"].([]any)[0].(map[string]any)
	if outTrack["id"] != "main" || outTrack["title"] != "\u0085😀标题\u0085" || outTrack["summary"] != "甲\t  乙\n😀" || outEvent["summary"] != "\u0085" { t.Fatal("not ECMAScript trim", result) }
	for _, object := range []map[string]any{result, outTrack, outEvent, outRef} { if _, exists := object["extra"]; exists { t.Fatal("undeclared key survived") } }
	outTrack["title"] = "changed"; outEvent["summary"] = "changed"; outRef["chapterNumber"] = 9
	if !reflect.DeepEqual(before, plotTestJSON(t, value)) { t.Fatal("output maps alias original maps") }
	if narrativeLength("😀文") != 3 { t.Fatal("shared UTF-16 semantics changed") }
	for _, field := range []string{"sourceRevision"} { bad := plotTestSnapshot(); bad[field] = nil; plotTestError(t, bad, "剧情树来源版本无效") }
	without := plotTestSnapshot(); delete(without, "sourceRevision"); result, err = plotValidateStored(without); if err != nil { t.Fatal(err) }; if _, exists := result["sourceRevision"]; exists { t.Fatal("omitted optional key became null") }
}

func TestPlotValidationNativeJSONNumbersAndNoOverflow(t *testing.T) {
	invalid := []any{nil, true, "5", json.Number("1e1000"), json.Number("9007199254740992"), json.Number("4294967296"), json.Number("1.5"), math.Inf(1), math.NaN(), ^uint64(0), int64(1<<63-1), -1, 0}
	for index, number := range invalid { t.Run(fmt.Sprint(index), func(t *testing.T) { value := plotTestSnapshot(); plotTestTrack(value)["startChapter"] = number; plotTestError(t, value, "剧情树轨道章节范围无效") }) }
	value := plotTestSnapshot(); value["version"] = json.Number("1.0")
	plotTestTrack(value)["startChapter"], plotTestTrack(value)["endChapter"] = json.Number("4294967295"), json.Number("4.294967295e9")
	plotTestEvent(value)["chapterNumber"], plotTestRef(value)["chapterNumber"] = json.Number("4294967295.0"), json.Number("4294967295")
	result, err := plotValidateStored(value); if err != nil { t.Fatal(err) }
	if result["tracks"].([]any)[0].(map[string]any)["endChapter"] != int64(4294967295) { t.Fatal("chapter ceiling normalized incorrectly") }
	value = plotTestSnapshot(); event := plotTestEvent(value); event["status"] = "occurred"; event["sources"] = []any{map[string]any{"type": "finalized-chapter", "draftId": json.Number("9007199254740991"), "chapterNumber": 5}}
	if _, err := plotValidateStored(value); err != nil { t.Fatal("safe integer ID rejected", err) }
	plotTestRef(value)["draftId"] = json.Number("9007199254740992"); plotTestError(t, value, "剧情树来源定稿无效")
}

func TestPlotValidationNativeBoundedGraphCyclesAndNoInventedCountCaps(t *testing.T) {
	for _, count := range []int{1, 2, 32, 257} { t.Run(fmt.Sprintf("cycle-%d", count), func(t *testing.T) {
		value := plotTestSnapshot(); tracks := make([]any, count)
		for index := range tracks { candidate := plotTestTrack(plotTestSnapshot()); candidate["id"] = fmt.Sprint(index); candidate["role"] = "subplot"; candidate["parentTrackId"] = fmt.Sprint((index+1)%count); tracks[index] = candidate }
		value["tracks"] = tracks; plotTestError(t, value, "剧情树支线父轨道必须是现有主线")
	}) }
	// The actual original module defines no MAX_TRACKS/MAX_EVENTS limit. Exercise
	// bounded large inputs and parent fanout; do not invent a rejecting ceiling.
	value := plotTestSnapshot(); tracks := []any{plotTestTrack(value)}
	for index := 0; index < 2048; index++ { candidate := plotTestTrack(plotTestSnapshot()); candidate["id"] = fmt.Sprintf("sub-%d", index); candidate["role"] = "subplot"; candidate["parentTrackId"] = "main"; tracks = append(tracks, candidate) }
	value["tracks"] = tracks; if _, err := plotValidateStored(value); err != nil { t.Fatal("invented track cap", err) }
	value = plotTestSnapshot(); events := make([]any, 4097); for index := range events { events[index] = plotTestEvent(plotTestSnapshot()) }; plotTestTrack(value)["events"] = events
	plotTestTrack(value)["title"] = strings.Repeat("😀", 8193); if _, err := plotValidateStored(value); err != nil { t.Fatal("invented event/text cap", err) }
	value = plotTestSnapshot(); var role any = "main"; for depth := 0; depth < 4096; depth++ { role = []any{role} }; plotTestTrack(value)["role"] = role
	if _, err := plotValidateStored(value); err != nil { t.Fatal("original String(array) enum admission", err) }
}

func TestPlotValidationNativeCurrentBoundsAndHistoricalIndependence(t *testing.T) {
	value, sources := plotTestSnapshot(), plotTestSources(); before, sourcesBefore := plotTestJSON(t, value), plotTestJSON(t, sources)
	if _, err := plotValidate(value, sources); err != nil { t.Fatal("SQL []map sources not supported", err) }
	if !reflect.DeepEqual(before, plotTestJSON(t, value)) || !reflect.DeepEqual(sourcesBefore, plotTestJSON(t, sources)) { t.Fatal("current validation mutated caller data") }
	plotTestTrack(value)["endChapter"] = 20
	if _, err := plotValidate(value, sources); err != nil { t.Fatal("valid confirmation should expand current bounds", err) }
	sources["narrativeThreads"].([]map[string]any)[0]["events"].([]map[string]any)[0]["reason"] = "\uFEFF "
	if _, err := plotValidate(value, sources); err == nil || err.Error() != "剧情树轨道章节范围超出当前剧情来源" { t.Fatal("invalid confirmation expanded bounds", err) }
	plotTestTrack(value)["endChapter"] = plotMaxChapter
	if _, err := plotValidateStored(value); err != nil { t.Fatal("stored snapshot wrongly depends on current bounds", err) }
	value = plotTestSnapshot(); plotTestEvent(value)["sources"] = []any{map[string]any{"type": "narrative-thread", "planId": 7}}
	if _, err := plotValidate(value, sources); err != nil { t.Fatal(err) }
	plan := sources["narrativeThreads"].([]map[string]any)[0]; plan["targetStartChapter"] = "6"
	if _, err := plotValidate(value, sources); err == nil || err.Error() != "剧情树事件状态与来源不匹配" { t.Fatal("planned event ignored first plan range", err) }
	plan["targetStartChapter"] = "3"; plan["targetEndChapter"] = "9"
	if _, err := plotValidate(value, sources); err != nil { t.Fatal("original relational coercion not preserved", err) }
	plan["id"] = "7"
	if _, err := plotValidate(value, sources); err == nil || err.Error() != "剧情树来源引用不存在" { t.Fatal("coerced source identity", err) }
}

func TestPlotValidationNativeErrorPrecedence(t *testing.T) {
	value := plotTestSnapshot(); event := plotTestEvent(value); event["status"] = "occurred"; event["summary"] = ""; event["sources"] = []any{map[string]any{"type": "blueprint", "chapterNumber": 5}, map[string]any{"type": "narrative-thread", "planId": 0}}
	plotTestError(t, value, "剧情树来源叙事计划无效")
	value = plotTestSnapshot(); plotTestTrack(value)["parentTrackId"] = nil; plotTestEvent(value)["summary"] = ""; plotTestError(t, value, "剧情树事件摘要无效")
	value = plotTestSnapshot(); value["tracks"] = append(value["tracks"].([]any), plotTestTrack(plotTestSnapshot()), nil); plotTestError(t, value, "剧情树轨道无效")
	value = plotTestSnapshot(); plotTestEvent(value)["sources"] = []any{map[string]any{"type": "narrative-thread", "planId": 7, "chapterNumber": nil}}; plotTestError(t, value, "剧情树来源章节无效")
}

func TestOriginalPlotValidationContract(t *testing.T) {
	path := os.Getenv("GO_PLOTVALIDATION_CONTRACT")
	if path == "" { if os.Getenv("GITHUB_ACTIONS") == "true" { t.Fatal("GO_PLOTVALIDATION_CONTRACT original shared validator oracle is mandatory on GitHub Actions") }; t.Skip("GitHub-only original shared validator oracle") }
	data, err := os.ReadFile(path); if err != nil { t.Fatal(err) }
	for _, useNumber := range []bool{false, true} { t.Run(fmt.Sprintf("UseNumber-%t", useNumber), func(t *testing.T) {
		var oracle struct {
			FormatVersion int `json:"formatVersion"`
			Cases []struct { Name string `json:"name"`; Method string `json:"method"`; Value any `json:"value"`; Sources map[string]any `json:"sources"`; Result any `json:"result"`; Error string `json:"error"` } `json:"cases"`
		}
		decoder := json.NewDecoder(bytes.NewReader(data)); if useNumber { decoder.UseNumber() }; if err := decoder.Decode(&oracle); err != nil { t.Fatal(err) }
		if oracle.FormatVersion != 1 || len(oracle.Cases) < 80 { t.Fatalf("incomplete original validator oracle: version=%d cases=%d", oracle.FormatVersion, len(oracle.Cases)) }
		names := map[string]bool{}; stored, current := 0, 0
		for _, fixture := range oracle.Cases { if fixture.Name == "" || names[fixture.Name] { t.Fatal("oracle missing/duplicate name", fixture.Name) }; names[fixture.Name] = true
			switch fixture.Method { case "stored": stored++; case "validate": current++; default: t.Fatal("unknown oracle method", fixture.Method) }
			t.Run(fixture.Name, func(t *testing.T) {
				beforeValue, beforeSources := plotTestJSON(t, fixture.Value), plotTestJSON(t, fixture.Sources)
				var result map[string]any; var err error
				if fixture.Method == "stored" { result, err = plotValidateStored(fixture.Value) } else { result, err = plotValidate(fixture.Value, fixture.Sources) }
				message := ""; if err != nil { message = err.Error() }; if message != fixture.Error { t.Fatalf("exact error Go=%q TS=%q", message, fixture.Error) }
				if !reflect.DeepEqual(plotTestJSON(t, result), plotTestJSON(t, fixture.Result)) { t.Fatalf("normalized result Go=%#v TS=%#v", plotTestJSON(t, result), plotTestJSON(t, fixture.Result)) }
				if !reflect.DeepEqual(beforeValue, plotTestJSON(t, fixture.Value)) || !reflect.DeepEqual(beforeSources, plotTestJSON(t, fixture.Sources)) { t.Fatal("validator mutated original input or sources") }
			})
		}
		if stored < 40 || current < 30 { t.Fatalf("oracle lacks structural/current-source coverage: stored=%d current=%d", stored, current) }
	}) }
	still, err := os.ReadFile(path); if err != nil || !bytes.Equal(data, still) { t.Fatal("oracle file mutated", err) }
}
