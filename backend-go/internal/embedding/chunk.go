package embedding

import (
	"context"
	"errors"
	"unicode/utf16"
	"unicode/utf8"
)

// Chunk failures are intentionally symbolic: they never expose input text.
var (
	ErrChunkInput         = errors.New("EMBEDDING_CHUNK_INPUT")
	ErrChunkLimit         = errors.New("EMBEDDING_CHUNK_LIMIT")
	ErrChunkUTF16Boundary = errors.New("EMBEDDING_CHUNK_UTF16_BOUNDARY")
)

const (
	chunkInputMaxBytes  = 32 * 1024 * 1024
	chunkOutputMaxBytes = 32 * 1024 * 1024
	chunkMaxCount       = 10000
	chunkCheckInterval  = 1024
)

// ChunkText ports electron/embedding.ts chunkText using JavaScript UTF-16 code
// units, including its slice(-0) overlap behavior. Callers supply their defaults
// (normally 500 and 50); maxChars accepts 1..4000 and overlap 0..maxChars-1.
// Input and aggregate output are limited to 32 MiB each, and output to 10000
// chunks. Invalid UTF-8 is rejected. If any final JavaScript chunk would contain
// an unpaired surrogate, the whole call fails rather than replacing it or
// silently changing its boundary. Every failure returns a nil result.
func ChunkText(ctx context.Context, text string, maxChars, overlap int) ([]string, error) {
	if ctx == nil {
		return nil, ErrChunkInput
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if maxChars < 1 || maxChars > 4000 || overlap < 0 || overlap >= maxChars {
		return nil, ErrChunkInput
	}
	if len(text) > chunkInputMaxBytes {
		return nil, ErrChunkLimit
	}

	// Decode explicitly: Go's range and utf16.Decode would replace malformed
	// encodings. Keep code units until an actual output chunk is validated.
	units := make([]uint16, 0, len(text))
	for offset, count := 0, 0; offset < len(text); count++ {
		if count%chunkCheckInterval == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		r, size := utf8.DecodeRuneInString(text[offset:])
		if r == utf8.RuneError && size == 1 {
			return nil, ErrChunkInput
		}
		if r > 0xffff {
			hi, lo := utf16.EncodeRune(r)
			units = append(units, uint16(hi), uint16(lo))
		} else {
			units = append(units, uint16(r))
		}
		offset += size
	}

	state := chunkState{ctx: ctx, maxChars: maxChars, overlap: overlap}
	// /\n\s*\n/ is greedy: the delimiter ends at the last LF in a
	// whitespace run, not at its final whitespace character. In particular,
	// CR before the first LF is still part of the preceding paragraph.
	start := 0
	for i := 0; i < len(units); {
		if err := state.check(i); err != nil {
			return nil, err
		}
		if units[i] != '\n' {
			i++
			continue
		}
		j, lastLF := i+1, -1
		for j < len(units) && chunkWhitespace(units[j]) {
			if err := state.check(j); err != nil {
				return nil, err
			}
			if units[j] == '\n' {
				lastLF = j
			}
			j++
		}
		if lastLF >= 0 {
			if err := state.paragraph(units[start:i]); err != nil {
				return nil, err
			}
			start = lastLF + 1
		}
		// There are no more LFs before j, including the unmatched case.
		i = j
	}
	if err := state.paragraph(units[start:]); err != nil {
		return nil, err
	}
	if err := state.push(state.current); err != nil {
		return nil, err
	}
	if len(state.chunks) == 0 {
		// This is deliberately [text.trim()], including [""] for whitespace.
		trimmed, err := chunkTrim(ctx, units)
		if err != nil {
			return nil, err
		}
		if err := state.emit(trimmed); err != nil {
			return nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return state.chunks, nil
}

type chunkState struct {
	ctx        context.Context
	maxChars   int
	overlap    int
	current    []uint16
	chunks     []string
	outputSize int
}

func (s *chunkState) check(index int) error {
	if index%chunkCheckInterval == 0 {
		return s.ctx.Err()
	}
	return nil
}

func (s *chunkState) paragraph(para []uint16) error {
	if err := s.ctx.Err(); err != nil {
		return err
	}
	trimmed, err := chunkTrim(s.ctx, para)
	if err != nil || len(trimmed) == 0 {
		return err
	}
	if len(para) > s.maxChars {
		if len(s.current) > 0 {
			if err := s.push(s.current); err != nil {
				return err
			}
			s.current = nil
		}
		var sentenceChunk []uint16
		addSentence := func(sentence []uint16) error {
			if err := s.ctx.Err(); err != nil {
				return err
			}
			if len(sentenceChunk)+len(sentence) > s.maxChars && len(sentenceChunk) > 0 {
				if err := s.push(sentenceChunk); err != nil {
					return err
				}
				sentenceChunk = chunkSuffix(sentenceChunk, s.overlap)
			}
			sentenceChunk = append(sentenceChunk, sentence...)
			return nil
		}
		// /(?<=[。！？.!?])\s*/ splits immediately after punctuation,
		// even without whitespace. A zero-width match at end is ignored by
		// JS split, but consumed trailing whitespace produces an empty field.
		start := 0
		for i := 0; i < len(para); {
			if err := s.check(i); err != nil {
				return err
			}
			if !chunkPunctuation(para[i]) || i+1 == len(para) {
				i++
				continue
			}
			end := i + 1
			j := end
			for j < len(para) && chunkWhitespace(para[j]) {
				if err := s.check(j); err != nil {
					return err
				}
				j++
			}
			if err := addSentence(para[start:end]); err != nil {
				return err
			}
			start, i = j, j
		}
		if err := addSentence(para[start:]); err != nil {
			return err
		}
		trimmed, err := chunkTrim(s.ctx, sentenceChunk)
		if err != nil {
			return err
		}
		if len(trimmed) > 0 {
			s.current = sentenceChunk
		}
		return nil
	}

	if len(s.current)+len(para) > s.maxChars && len(s.current) > 0 {
		if err := s.push(s.current); err != nil {
			return err
		}
		s.current = chunkSuffix(s.current, s.overlap)
		s.current = append(s.current, '\n', '\n')
	} else if len(s.current) > 0 {
		s.current = append(s.current, '\n', '\n')
	}
	s.current = append(s.current, para...)
	return nil
}

func (s *chunkState) push(value []uint16) error {
	if err := s.ctx.Err(); err != nil {
		return err
	}
	normalized, err := chunkTrim(s.ctx, value)
	if err != nil || len(normalized) == 0 {
		return err
	}
	if len(normalized) <= s.maxChars {
		return s.emit(normalized)
	}
	for start := 0; start < len(normalized); {
		end := start + s.maxChars
		if end > len(normalized) {
			end = len(normalized)
		}
		if err := s.emit(normalized[start:end]); err != nil {
			return err
		}
		if end == len(normalized) {
			break
		}
		// Bounds guarantee maxChars-overlap >= 1, matching Math.max.
		start = end - s.overlap
	}
	return nil
}

func (s *chunkState) emit(value []uint16) error {
	if err := s.ctx.Err(); err != nil {
		return err
	}
	if len(s.chunks) >= chunkMaxCount {
		return ErrChunkLimit
	}
	encoded := make([]byte, 0, len(value)*3)
	for i := 0; i < len(value); i++ {
		if err := s.check(i); err != nil {
			return err
		}
		u := value[i]
		r := rune(u)
		if u >= 0xd800 && u <= 0xdbff {
			if i+1 >= len(value) || value[i+1] < 0xdc00 || value[i+1] > 0xdfff {
				return ErrChunkUTF16Boundary
			}
			r = utf16.DecodeRune(rune(u), rune(value[i+1]))
			i++
		} else if u >= 0xdc00 && u <= 0xdfff {
			return ErrChunkUTF16Boundary
		}
		encoded = utf8.AppendRune(encoded, r)
	}
	if len(encoded) > chunkOutputMaxBytes-s.outputSize {
		return ErrChunkLimit
	}
	s.outputSize += len(encoded)
	s.chunks = append(s.chunks, string(encoded))
	return nil
}

func chunkSuffix(value []uint16, overlap int) []uint16 {
	// JavaScript slice(-0) is slice(0), not an empty suffix.
	if overlap == 0 || overlap >= len(value) {
		return value
	}
	return value[len(value)-overlap:]
}

func chunkTrim(ctx context.Context, value []uint16) ([]uint16, error) {
	start, end := 0, len(value)
	for start < end && chunkWhitespace(value[start]) {
		if start%chunkCheckInterval == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		start++
	}
	for end > start && chunkWhitespace(value[end-1]) {
		if end%chunkCheckInterval == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		end--
	}
	return value[start:end], nil
}

// ECMAScript WhiteSpace + LineTerminator, shared by trim and regex \s.
// Deliberately includes BOM and excludes NEL, U+180E, and U+200B.
func chunkWhitespace(u uint16) bool {
	switch u {
	case 0x0009, 0x000a, 0x000b, 0x000c, 0x000d, 0x0020, 0x00a0,
		0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000, 0xfeff:
		return true
	default:
		return u >= 0x2000 && u <= 0x200a
	}
}

func chunkPunctuation(u uint16) bool {
	switch u {
	case '。', '！', '？', '.', '!', '?':
		return true
	default:
		return false
	}
}
