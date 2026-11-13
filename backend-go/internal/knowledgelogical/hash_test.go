package knowledgelogical

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/knowledgedomain"
)

func TestECMAScriptOwnedEncoder(t *testing.T) {
	for _, tc := range []struct {
		n    float64
		want string
	}{
		{math.Copysign(0, -1), "0"}, {1e-6, "0.000001"}, {1e-7, "1e-7"}, {1e20, "100000000000000000000"}, {1e21, "1e+21"}, {-1e21, "-1e+21"}, {math.SmallestNonzeroFloat64, "5e-324"}, {9007199254740992, "9007199254740992"},
	} {
		got, err := numberString(tc.n)
		if err != nil || got != tc.want {
			t.Fatalf("got %q want %q error %v", got, tc.want, err)
		}
	}
	raw, err := encode(context.Background(), object{{"z", "<&>\u2028\u2029🙂/"}, {"a", "\x00\b\f\n\r\t\"\\"}}, 1000)
	if err != nil || string(raw) != "{\"z\":\"<&>\u2028\u2029🙂/\",\"a\":\"\\u0000\\b\\f\\n\\r\\t\\\"\\\\\"}" {
		t.Fatalf("bad string/order encoding %q %v", raw, err)
	}
	if _, err := encode(context.Background(), "large", 2); err == nil {
		t.Fatal("output budget bypass")
	}
	if _, err := number(json.Number("9007199254740993")); err != nil {
		t.Fatal("JS Number unsafe literal conversion rejected")
	}
}
func TestLimitsLocaleCancellationAndUTF16(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := HashSnapshot(ctx, knowledgedomain.Snapshot{}, testBudget(), Options{Locale: "en-US"}); err != context.Canceled {
		t.Fatal(err)
	}
	if _, err := HashSnapshot(nil, knowledgedomain.Snapshot{}, testBudget(), Options{Locale: "en-US"}); err == nil {
		t.Fatal("nil context")
	}
	if _, err := HashSnapshot(context.Background(), knowledgedomain.Snapshot{}, testBudget(), Options{}); err == nil {
		t.Fatal("locale inference forbidden")
	}
	if err := ordering([]string{"ä", "z"}, Options{Locale: "en-US"}); err == nil {
		t.Fatal("unproved ICU collation accepted")
	}
	if err := ordering([]string{"arbitrary🙂"}, Options{Locale: "en-US"}); err != nil {
		t.Fatal("singleton needs no locale ordering")
	}
	if !utf16Less("𝄞", "\ue000") {
		t.Fatal("default sort must compare UTF16, not UTF8")
	}
	if !portableFileName(strings.Repeat("🙂", 256)) || portableFileName(strings.Repeat("🙂", 257)) {
		t.Fatal("filename UTF16 limit")
	}
	for _, s := range []string{"ＣＯＮ.txt", "foo／bar.txt", "foo．", "foo\u00a0"} {
		if portableFileName(s) {
			t.Fatal("NFKC filename rule")
		}
	}
	if !portableFileName("e\u0301.txt") {
		t.Fatal("original decomposed name should be retained")
	}
	bad := testBudget()
	bad.MaxRows = 0
	if _, err := SerializePortable(context.Background(), PortableSnapshot{Version: 1}, bad, Options{Locale: "en-US"}); err == nil {
		t.Fatal("budget")
	}
}
func TestEmptyOriginalHashAndRegistryByteAuthority(t *testing.T) {
	ctx := context.Background()
	snapshot := knowledgedomain.Snapshot{}
	out, err := HashSnapshot(ctx, snapshot, testBudget(), Options{Locale: "en-US"})
	if err != nil {
		t.Fatal(err)
	}
	if out.LogicalHash != digest([]byte("[[],null]")) {
		t.Fatal("empty original snapshot protocol")
	}
	a := json.RawMessage(`{"version":1,"spaces":[],"activeGeneration":null}`)
	b := append(append(json.RawMessage(nil), a...), '\n')
	snapshot.Registry = a
	first, err := HashSnapshot(ctx, snapshot, testBudget(), Options{Locale: "en-US"})
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Registry = b
	second, err := HashSnapshot(ctx, snapshot, testBudget(), Options{Locale: "en-US"})
	if err != nil {
		t.Fatal(err)
	}
	if first.LogicalHash == second.LogicalHash {
		t.Fatal("registry was normalized instead of raw-hashed")
	}
}
