package embedding

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
	"unicode/utf8"
)

func TestChunkTextOriginalSemantics(t *testing.T) {
	tests := []struct {
		name    string
		text    string
		max     int
		overlap int
		want    []string
	}{
		{"empty", "", 500, 50, []string{""}},
		{"all ECMAScript whitespace", "\ufeff \t\v\f\r\n\u00a0\u1680\u2000\u200a\u2028\u2029\u202f\u205f\u3000", 500, 50, []string{""}},
		{"BOM trim", "\ufeffhello\ufeff", 500, 50, []string{"hello"}},
		{"NEL not whitespace", "\u0085hello\u0085", 500, 50, []string{"\u0085hello\u0085"}},
		{"obsolete and zero width not whitespace", "\u180e\u200b", 500, 50, []string{"\u180e\u200b"}},
		{"BMP", "  甲乙丙丁  ", 3, 1, []string{"甲乙丙", "丙丁"}},
		{"astral UTF16 length", "A😀B", 4, 1, []string{"A😀B"}},
		{"astral safe hard cuts", "😀😀😀", 4, 2, []string{"😀😀", "😀😀"}},
		{"astral safe overlap", "😀ab", 2, 0, []string{"😀", "ab"}},
		{"single CRLF preserved", "a\r\nb", 10, 1, []string{"a\r\nb"}},
		{"paragraph CR preserved", "a\r\n\r\nb", 10, 1, []string{"a\r\n\nb"}},
		{"greedy paragraph whitespace", "a\n \n\t\n  b", 20, 1, []string{"a\n\n  b"}},
		{"paragraph split includes BOM", "a\n\ufeff\nb", 10, 1, []string{"a\n\nb"}},
		{"paragraph split excludes NEL", "a\n\u0085\nb", 10, 1, []string{"a\n\u0085\nb"}},
		{"paragraph concatenation ignores inserted separator in check", "ab\n\ncd", 4, 1, []string{"ab\n\n", "\ncd"}},
		{"paragraph overlap zero retains whole current", "ab\n\ncd", 3, 0, []string{"ab", "ab\n", "\ncd"}},
		{"sentence overlap zero retains whole current", "a.b.", 3, 0, []string{"a.", "a.b", "."}},
		{"sentence whitespace removed", "a. \t b!\r\n c?", 5, 1, []string{"a.b!", "!c?"}},
		{"Chinese sentence punctuation", "甲。乙！丙？丁", 4, 1, []string{"甲。乙！", "！丙？丁"}},
		{"adjacent punctuation", "a!?b", 3, 1, []string{"a!?", "?b"}},
		{"trim before hard split only", "  ab cd ef  ", 3, 1, []string{"ab ", " cd", "d e", "ef"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ChunkText(context.Background(), tc.text, tc.max, tc.overlap)
			if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ChunkText() = %#v, %v; want %#v", got, err, tc.want)
			}
		})
	}
}

func TestChunkTextUTF16BoundaryRejectsEntireResult(t *testing.T) {
	for _, tc := range []struct {
		name    string
		text    string
		max     int
		overlap int
	}{
		{"first hard cut", "😀x", 1, 0},
		{"later hard cut", "ok\n\nA😀B", 2, 0},
		{"hard cut overlap low surrogate", "😀😀😀", 4, 1},
		{"sentence suffix low surrogate", "😀\n\nx", 2, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ChunkText(context.Background(), tc.text, tc.max, tc.overlap)
			if got != nil || !errors.Is(err, ErrChunkUTF16Boundary) {
				t.Fatalf("got %#v, %v; want nil, ErrChunkUTF16Boundary", got, err)
			}
		})
	}
}

func TestChunkTextRejectsInvalidInputs(t *testing.T) {
	for _, tc := range []struct {
		name    string
		ctx     context.Context
		text    string
		max     int
		overlap int
	}{
		{"nil context", nil, "x", 500, 50},
		{"zero maximum", context.Background(), "x", 0, 0},
		{"negative maximum", context.Background(), "x", -1, 0},
		{"maximum too large", context.Background(), "x", 4001, 0},
		{"negative overlap", context.Background(), "x", 100, -1},
		{"overlap equal maximum", context.Background(), "x", 100, 100},
		{"overlap above maximum", context.Background(), "x", 100, 101},
		{"invalid UTF8", context.Background(), "a\xffb", 500, 50},
		{"encoded surrogate", context.Background(), "\xed\xa0\x80", 500, 50},
		{"truncated UTF8", context.Background(), "\xf0\x9f", 500, 50},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ChunkText(tc.ctx, tc.text, tc.max, tc.overlap)
			if got != nil || !errors.Is(err, ErrChunkInput) {
				t.Fatalf("got %#v, %v; want nil, ErrChunkInput", got, err)
			}
		})
	}
	got, err := ChunkText(context.Background(), "\ufffd", 1, 0)
	if err != nil || !reflect.DeepEqual(got, []string{"\ufffd"}) {
		t.Fatalf("valid encoded replacement scalar rejected: %#v, %v", got, err)
	}
}

func TestChunkTextLimits(t *testing.T) {
	t.Run("input bytes", func(t *testing.T) {
		got, err := ChunkText(context.Background(), strings.Repeat(" ", chunkInputMaxBytes+1), 500, 50)
		if got != nil || !errors.Is(err, ErrChunkLimit) {
			t.Fatalf("got non-nil=%v, %v; want nil, ErrChunkLimit", got != nil, err)
		}
	})
	t.Run("chunk count exact limit admitted", func(t *testing.T) {
		got, err := ChunkText(context.Background(), strings.Repeat("x", chunkMaxCount), 1, 0)
		if err != nil || len(got) != chunkMaxCount {
			t.Fatalf("got %d chunks, %v; want %d", len(got), err, chunkMaxCount)
		}
	})
	t.Run("chunk count atomic failure", func(t *testing.T) {
		got, err := ChunkText(context.Background(), strings.Repeat("x", chunkMaxCount+1), 1, 0)
		if got != nil || !errors.Is(err, ErrChunkLimit) {
			t.Fatalf("got non-nil=%v, %v; want nil, ErrChunkLimit", got != nil, err)
		}
	})
	t.Run("output UTF8 byte budget before count budget", func(t *testing.T) {
		// 3001 windows * 4000 BMP units * 3 UTF-8 bytes exceeds 32 MiB.
		got, err := ChunkText(context.Background(), strings.Repeat("界", 7000), 4000, 3999)
		if got != nil || !errors.Is(err, ErrChunkLimit) {
			t.Fatalf("got non-nil=%v, %v; want nil, ErrChunkLimit", got != nil, err)
		}
	})
}

func TestChunkTextGiantSentenceBounded(t *testing.T) {
	text := strings.Repeat("界", 16000)
	got, err := ChunkText(context.Background(), text, 500, 50)
	if err != nil || len(got) < 2 {
		t.Fatalf("got %d chunks, %v", len(got), err)
	}
	for i, value := range got {
		if !utf8.ValidString(value) || len(utf16.Encode([]rune(value))) > 500 {
			t.Fatalf("chunk %d violates UTF8 or UTF16 length bounds", i)
		}
		if i > 0 && !strings.HasPrefix(value, strings.Repeat("界", 50)) {
			t.Fatalf("chunk %d missing overlap", i)
		}
	}
}

// This context deterministically cancels at an Err checkpoint, avoiding
// scheduler-dependent cancellation tests and checking work-loop cooperation.
type chunkCancelContext struct {
	context.Context
	cancel   context.CancelFunc
	checks   int
	cancelAt int
}

func (c *chunkCancelContext) Err() error {
	c.checks++
	if c.checks == c.cancelAt {
		c.cancel()
	}
	return c.Context.Err()
}

func TestChunkTextCancellationIsAtomic(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, err := ChunkText(ctx, "hello", 500, 50)
	if got != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled: %#v, %v", got, err)
	}
	for _, at := range []int{2, 20, 70, 200} {
		ctx, cancel := context.WithCancel(context.Background())
		counted := &chunkCancelContext{Context: ctx, cancel: cancel, cancelAt: at}
		got, err := ChunkText(counted, strings.Repeat("x", 12000), 1, 0)
		cancel()
		if got != nil || !errors.Is(err, context.Canceled) || counted.checks < at {
			t.Fatalf("checkpoint %d: non-nil=%v, %v, checks=%d", at, got != nil, err, counted.checks)
		}
	}
	deadline, stop := context.WithDeadline(context.Background(), time.Unix(0, 0))
	defer stop()
	got, err = ChunkText(deadline, "hello", 500, 50)
	if got != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline: %#v, %v", got, err)
	}
}
