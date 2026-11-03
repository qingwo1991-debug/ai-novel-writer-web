package publicresponse

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
)

func retained(t *testing.T, s *Sanitizer, values ...string) {
	t.Helper()
	for _, value := range values { if ok, err := s.RegisterSecret(value); !ok || err != nil { t.Fatal("synthetic registration rejected", err) } }
}

func TestUTF16RegistrationAndLiteralOverlap(t *testing.T) {
	for _, tc := range []struct { name string; secrets []string; input, want string }{
		{"self overlap", []string{"aaaa"}, "xaaaaaay", "x[REDACTED]y"},
		{"cross overlap", []string{"abcde", "defgh"}, "xabcdefghy", "x[REDACTED]y"},
		{"adjacent union", []string{"abcd", "efgh"}, "xabcdefghy", "x[REDACTED]y"},
		{"literal metacharacters", []string{"a.*$"}, "aZZ a.*$", "aZZ [REDACTED]"},
		{"spaces preserved", []string{" abc "}, "abc| abc |abc", "abc|[REDACTED]|abc"},
		{"marker secret", []string{"REDA"}, "xREDAy[REDACTED]z", "xy[CTED]z"},
		{"whole marker secret", []string{"[REDACTED]"}, "x[REDACTED]y", "xy"},
		{"new marker boundary", []string{"abcd", "x[RE"}, "xabcdz", "DACTED]z"},
		{"fixed point join", []string{"abcd", "REDA", "xyzq"}, "xyabcdzq", ""},
		{"astral minimum", []string{"😀😀"}, "left😀😀right", "left[REDACTED]right"},
	} { t.Run(tc.name, func(t *testing.T) { s := New(); retained(t, s, tc.secrets...); if got := s.Sanitize(tc.input); got != tc.want { t.Fatalf("got %#v want %#v", got, tc.want) } }) }
	s := New()
	for _, short := range []string{"", "abc", "界界界", "😀a"} { if ok, err := s.RegisterSecret(short); ok || err != nil { t.Fatal("UTF16 short secret accepted") } }
	retained(t, s, strings.Repeat("😀", MaxSecretLength/2))
	if ok, err := s.RegisterSecret(strings.Repeat("😀", MaxSecretLength/2)+"x"); ok || !errors.Is(err, ErrSecretLimit) { t.Fatal("astral per-secret bound not enforced") }
}

func TestRegistrationCapacityNoEvictionOrMutation(t *testing.T) {
	s := New()
	for i := 0; i < MaxSecrets; i++ { retained(t, s, fmt.Sprintf("synthetic-%04d", i)) }
	retained(t, s, "synthetic-0000")
	if ok, err := s.RegisterSecret("synthetic-extra"); ok || err != ErrSecretLimit || err.Error() != "PUBLIC_RESPONSE_SECRET_LIMIT" { t.Fatal("count overflow must fail closed") }
	if s.Sanitize("synthetic-0000 synthetic-extra") != "[REDACTED] synthetic-extra" { t.Fatal("failed registration mutated registry or evicted first secret") }
	total := New()
	for i := 0; i < MaxSecretCharacters/MaxSecretLength; i++ { retained(t, total, fmt.Sprintf("%04d", i)+strings.Repeat("界", MaxSecretLength-4)) }
	if ok, err := total.RegisterSecret("next"); ok || err != ErrSecretLimit { t.Fatal("UTF16 total bound not enforced") }
	retained(t, total, "0000"+strings.Repeat("界", MaxSecretLength-4))
	if s.characters > MaxSecretCharacters || len(s.units) != MaxSecrets { t.Fatal("registry bounds changed") }
}

func TestImmutableCloneKeysCredentialsAndUnsupportedValues(t *testing.T) {
	s := New(); retained(t, s, "fake-token")
	private := map[string]any{
		"apiKey": "fake-token", "PASSWORD": "keep-private", "accessTOKEN": 7, "secret": nil,
		"key": "ordinary", "zero": 0, "false": false, "null": nil,
		"__proto__": "bad", "prototype": "bad", "constructor": "bad", "Constructor": "safe",
		"fake-token/key": "prefix fake-token suffix", "nested": []any{map[string]any{"apiKey": "private", "text": "fake-token"}, false, 0, nil},
		"function": func() {}, "channel": make(chan int),
	}
	out := s.Sanitize(private).(map[string]any)
	for _, key := range []string{"apiKey", "PASSWORD", "accessTOKEN", "secret"} { if out[key] != "" { t.Fatal("credential field not blanked", key) } }
	for _, key := range []string{"__proto__", "prototype", "constructor", "function", "channel", "fake-token/key"} { if _, exists := out[key]; exists { t.Fatal("unsafe/unsupported key survived", key) } }
	if out["key"] != "ordinary" || out["Constructor"] != "safe" || out["[REDACTED]/key"] != "prefix [REDACTED] suffix" { t.Fatal("ordinary fields or key redaction changed") }
	out["nested"].([]any)[0].(map[string]any)["text"] = "changed"
	if private["apiKey"] != "fake-token" || private["nested"].([]any)[0].(map[string]any)["text"] != "fake-token" { t.Fatal("private input mutated or aliased") }
	// Response fields never implicitly register new credentials.
	if s.Sanitize("keep-private") != "keep-private" { t.Fatal("response inferred a credential") }
	array := s.Sanitize([]any{func(){}, make(chan int), math.NaN(), math.Inf(1)}).([]any)
	for _, item := range array { if item != nil { t.Fatal("non-JSON array item not null") } }
}

func TestRedactedUnsafeKeysCollisionsAndSurrogates(t *testing.T) {
	s := New(); retained(t, s, "REDA")
	out := s.Sanitize(map[string]any{"conREDAstructor": "must-not-copy", "__proREDAto__": "must-not-copy", "APİKEY": "ordinary", "APIKEY": "private"}).(map[string]any)
	for _, key := range []string{"constructor", "__proto__"} { if _, exists := out[key]; exists { t.Fatal("redacted dangerous key survived") } }
	if out["APİKEY"] != "ordinary" || out["APIKEY"] != "" { t.Fatal("JS Unicode lowercase semantics differ") }
	collision := New(); retained(t, collision, "aaaa", "bbbb")
	value := collision.Sanitize(map[string]any{"aaaa": "first", "bbbb": "last"}).(map[string]any)
	if len(value) != 1 || value[redacted] != "last" { t.Fatal("redacted-key collision not deterministic") }
	for _, units := range [][]uint16{{'a', 0xd800, 'b', 'c'}, {0xd83d, 0xde00, 0xd83d, 0xde00}, {0xdfff, 'a', 'b', 'c'}} {
		secret := unitsString(units)
		isolated := New(); retained(t, isolated, secret)
		if isolated.Sanitize("before"+secret+"after") != "before[REDACTED]after" { t.Fatal("UTF16 literal failed") }
		if !reflect.DeepEqual(stringUnits(secret), units) { t.Fatal("UTF16 units did not round-trip") }
	}
	// Same JS string, two Go encodings: one Set member and one budget charge.
	pair := New(); retained(t, pair, "😀😀", "\xed\xa0\xbd\xed\xb8\x80\xed\xa0\xbd\xed\xb8\x80")
	if len(pair.units) != 1 || pair.characters != 4 { t.Fatal("JS-equivalent secret counted twice") }
}

func TestCyclesDepthAndSharedObjects(t *testing.T) {
	s := New()
	self := map[string]any{}; self["self"] = self
	if s.Sanitize(self).(map[string]any)["self"] != circular { t.Fatal("map cycle not detected") }
	array := make([]any, 1); array[0] = array
	if s.Sanitize(array).([]any)[0] != circular { t.Fatal("slice cycle not detected") }
	shared := map[string]any{"value": "same"}
	out := s.Sanitize([]any{shared, shared}).([]any)
	out[0].(map[string]any)["value"] = "changed"
	if out[1].(map[string]any)["value"] != "same" || shared["value"] != "same" { t.Fatal("shared objects not independently cloned") }
	var nested any = map[string]any{"leaf": "visible"}
	for i := 0; i < MaxDepth+1; i++ { nested = []any{nested} }
	got := s.Sanitize(nested)
	for i := 0; i < MaxDepth+1; i++ { got = got.([]any)[0] }
	if got != truncated { t.Fatal("depth boundary mismatch", got) }
	// Primitive leaves are handled before depth truncation in the JS source.
	var primitive any = "visible"
	for i := 0; i < MaxDepth+1; i++ { primitive = []any{primitive} }
	got = s.Sanitize(primitive)
	for i := 0; i < MaxDepth+1; i++ { got = got.([]any)[0] }
	if got != "visible" { t.Fatal("primitive leaf was truncated") }
	retained(t, s, "Circular", "Truncated")
	if s.Sanitize(self).(map[string]any)["self"] != "[[REDACTED]]" { t.Fatal("cycle marker itself leaked registered literal") }
}

type hostileMarshaler struct { Text string `json:"text"`; APIKey string `json:"apiKey"`; private string }
func (hostileMarshaler) MarshalJSON() ([]byte, error) { panic("must never invoke MarshalJSON") }

func TestRawMessageAndTypedValuesCannotBypass(t *testing.T) {
	s := New(); retained(t, s, "fake-token")
	input := json.RawMessage(`{"apiKey":"fake-token","message":"fake-token","large":9007199254740993,"__proto__":{"secret":"x"}}`)
	before := append([]byte(nil), input...)
	out := s.Sanitize(input).(map[string]any)
	if out["apiKey"] != "" || out["message"] != redacted || out["large"] != json.Number("9007199254740993") { t.Fatal("raw JSON sanitization or numeric precision failed", out) }
	if !reflect.DeepEqual(input, json.RawMessage(before)) { t.Fatal("RawMessage mutated") }
	lone := New(); retained(t, lone, unitsString([]uint16{'a', 0xd800, 'b', 'c'}))
	if lone.Sanitize(json.RawMessage(`{"text":"a\ud800bc"}`)).(map[string]any)["text"] != redacted { t.Fatal("RawMessage lost lone surrogate before matching") }
	collision := New(); retained(t, collision, "aaaa", "bbbb")
	ordered := collision.Sanitize(json.RawMessage(`{"bbbb":"first","aaaa":"last"}`)).(map[string]any)
	if ordered[redacted] != "last" { t.Fatal("RawMessage object insertion order lost") }
	for _, invalid := range []json.RawMessage{json.RawMessage(`{"x":"fake-token"`), json.RawMessage(`{} {}`), json.RawMessage(strings.Repeat("[", 257)+strings.Repeat("]", 257)), json.RawMessage(strings.Repeat(" ", MaxRawJSONBytes+1))} { if s.Sanitize(invalid) != truncated { t.Fatal("unsafe RawMessage not rejected safely") } }
	var typedNil *hostileMarshaler
	var rawNil json.RawMessage
	if s.Sanitize(typedNil) != nil || s.Sanitize(rawNil) != nil { t.Fatal("typed nil was not null") }
	value := hostileMarshaler{Text: "fake-token", APIKey: "private", private: "hidden"}
	clone := s.Sanitize(&value).(map[string]any)
	if clone["text"] != redacted || clone["apiKey"] != "" || len(clone) != 2 { t.Fatal("struct clone invoked methods or leaked private fields") }
	if value.APIKey != "private" || value.Text != "fake-token" { t.Fatal("typed private input mutated") }
	if _, err := json.Marshal(clone); err != nil { t.Fatal(err) }
	for _, invalid := range []json.Number{"", "fake-token", "{}", "[1]", "true"} { if s.Sanitize(invalid) != nil { t.Fatal("invalid json.Number survived") } }
}

func TestConcurrentRegistryAndClones(t *testing.T) {
	s := New(); retained(t, s, "first-secret")
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ { wg.Add(1); go func(i int) { defer wg.Done(); _, err := s.RegisterSecret(fmt.Sprintf("concurrent-%04d", i)); if err != nil { t.Error(err) }; for j := 0; j < 8; j++ { if s.Sanitize("first-secret") != redacted { t.Error("registered secret leaked") } } }(i) }
	wg.Wait()
}

type promotedResponseA struct {
	Text string `json:"text"`
	Password string `json:"password"`
	Collision string `json:"collision"`
}
type promotedResponseB struct {
	Collision string `json:"collision"`
}
type promotedResponse struct {
	promotedResponseA
	promotedResponseB
	Text string `json:"text"`
	Hidden string `json:"-"`
}

func TestPromotedStructFieldsResolveBeforeCredentialFiltering(t *testing.T) {
	s := New()
	retained(t, s, "synthetic-secret")
	value := promotedResponse{
		promotedResponseA: promotedResponseA{Text: "shadowed", Password: "synthetic-secret", Collision: "first"},
		promotedResponseB: promotedResponseB{Collision: "second"},
		Text: "synthetic-secret",
		Hidden: "synthetic-secret",
	}
	out := s.Sanitize(value).(map[string]any)
	if out["text"] != redacted || out["password"] != "" { t.Fatalf("promoted fields leaked or lost: %#v", out) }
	if _, exists := out["collision"]; exists { t.Fatal("ambiguous promoted field survived") }
	if _, exists := out["Hidden"]; exists { t.Fatal("ignored field survived") }
}

func TestDraftFullKeepsFlattenedOriginalResponseShape(t *testing.T) {
	value := store.DraftFull{
		DraftMeta: store.DraftMeta{ID: 7, ChapterNumber: 3, Version: 2, Status: "draft", SourceDependencies: []store.DraftSourceDependency{}},
		Content: "synthetic content",
	}
	out := New().Sanitize(value).(map[string]any)
	if out["id"] != int64(7) || out["chapterNumber"] != int64(3) || out["content"] != "synthetic content" {
		t.Fatalf("draft fields were not flattened: %#v", out)
	}
	if _, nested := out["DraftMeta"]; nested { t.Fatal("embedded DraftMeta escaped as a nested object") }
	if dependencies, ok := out["sourceDependencies"].([]any); !ok || len(dependencies) != 0 {
		t.Fatalf("empty sourceDependencies changed shape: %#v", out["sourceDependencies"])
	}
}
