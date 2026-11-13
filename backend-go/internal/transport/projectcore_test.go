package transport

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestProjectCoreNullableSourceAndBufferWire(t *testing.T) {
	expected := map[string]any{}
	for _, key := range []string{"synopsis", "premise", "charactersArch", "worldbuilding", "genre", "writingLanguage", "plotStructure", "narrativePov", "globalGuidance", "totalChapters", "wordsPerChapter"} {
		expected[key] = nil
	}
	request := map[string]any{"synopsis": "new", "expected": expected}
	if err := validateSynopsisRequest(request); err != nil {
		t.Fatal("SQL NULL baseline must reach original CAS conflict", err)
	}
	expected["totalChapters"] = float64(-1)
	if err := validateSynopsisRequest(request); err != nil {
		t.Fatal("legacy negative baseline rejected", err)
	}
	expected["wordsPerChapter"] = float64(8.5)
	if err := validateSynopsisRequest(request); err != nil {
		t.Fatal("SQLite REAL source baseline rejected", err)
	}
	delete(expected, "premise")
	if err := validateSynopsisRequest(request); err == nil {
		t.Fatal("missing baseline accepted")
	}
	data, err := json.Marshal(projectCoreWire(map[string]any{"synopsis": []byte{0, 255, 34}, "genre": nil}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"synopsis":{"data":[0,255,34],"type":"Buffer"}`) || !strings.Contains(string(data), `"genre":null`) {
		t.Fatal(string(data))
	}
}
