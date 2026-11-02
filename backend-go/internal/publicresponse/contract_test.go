package publicresponse

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

// The original JS implementation is imported by the separately invoked oracle
// exporter. Tests never execute node or copy the JavaScript sanitizer algorithm.
func TestOriginalPublicResponseContracts(t *testing.T) {
	path := os.Getenv("GO_PUBLIC_RESPONSE_CONTRACT")
	if path == "" {
		if os.Getenv("GITHUB_ACTIONS") == "true" { t.Fatal("original public-response oracle is mandatory") }
		t.Skip("GitHub-only original public-response oracle")
	}
	data, err := os.ReadFile(path)
	if err != nil { t.Fatal(err) }
	var oracle struct {
		FormatVersion int `json:"formatVersion"`
		Source string `json:"source"`
		Encoding string `json:"encoding"`
		Cases []struct {
			Name string `json:"name"`
			Kind string `json:"kind"`
			Secrets [][]uint16 `json:"secrets"`
			Registrations []struct { Retained bool `json:"retained"`; Error string `json:"error"` } `json:"registrations"`
			Input json.RawMessage `json:"input"`
			InputUnits []uint16 `json:"inputUnits"`
			ExpectedUnits []uint16 `json:"expectedUnits"`
			Expected json.RawMessage `json:"expected"`
			Depth int `json:"depth"`
			ObjectLeaf bool `json:"objectLeaf"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &oracle); err != nil { t.Fatal(err) }
	if oracle.FormatVersion != 1 || oracle.Source != "web/public-response.mjs" || oracle.Encoding != "utf16-code-units" || len(oracle.Cases) < 150 { t.Fatal("incomplete or incompatible public-response oracle") }
	decode := func(data []byte) any {
		decoder := json.NewDecoder(strings.NewReader(string(data)))
		decoder.UseNumber()
		var value any
		if err := decoder.Decode(&value); err != nil { t.Fatal(err) }
		return value
	}
	for _, fixture := range oracle.Cases {
		t.Run(fixture.Name, func(t *testing.T) {
			s := New()
			if len(fixture.Secrets) != len(fixture.Registrations) { t.Fatal("registration fixture length mismatch") }
			for i, secret := range fixture.Secrets {
				ok, err := s.RegisterSecret(unitsString(secret))
				code := ""; if err != nil { code = err.Error() }
				if ok != fixture.Registrations[i].Retained || code != fixture.Registrations[i].Error { t.Fatalf("registration %d differs from original JS", i) }
			}
			var input any
			switch fixture.Kind {
			case "string", "error-string": input = unitsString(fixture.InputUnits)
			case "json": input = decode(fixture.Input)
			case "raw-json":
				var raw string
				if err := json.Unmarshal(fixture.Input, &raw); err != nil { t.Fatal(err) }
				input = json.RawMessage(raw)
			case "cycle-map": value := map[string]any{"message": "synthetic-token"}; value["self"] = value; input = value
			case "cycle-array": value := make([]any, 1); value[0] = value; input = value
			case "shared": value := map[string]any{"text": "synthetic-token", "apiKey": "private-value"}; input = []any{value, value}
			case "depth":
				input = "synthetic-token"
				if fixture.ObjectLeaf { input = map[string]any{"leaf": "synthetic-token"} }
				for i := 0; i < fixture.Depth; i++ { input = []any{input} }
			case "unsupported": input = map[string]any{"missing": func(){}, "callable": func(){}, "symbol": make(chan int), "array": []any{func(){}, func(){}, make(chan int), nil, nil}, "nil": nil}
			default: t.Fatal("unknown original fixture kind", fixture.Kind)
			}
			actual := s.Sanitize(input)
			if fixture.Kind == "string" || fixture.Kind == "error-string" {
				text, ok := actual.(string)
				if !ok { t.Fatal("string oracle became nonstring") }
				if !reflect.DeepEqual(stringUnits(text), fixture.ExpectedUnits) { t.Fatalf("UTF16 string result differs: Go=%v JS=%v", stringUnits(text), fixture.ExpectedUnits) }
				return
			}
			encoded, err := json.Marshal(actual)
			if err != nil { t.Fatal(err) }
			if !reflect.DeepEqual(decode(encoded), decode(fixture.Expected)) { t.Fatalf("sanitizer result differs: Go=%s JS=%s", encoded, fixture.Expected) }
		})
	}
}
