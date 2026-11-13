package llm

import (
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// VisibleFilter ports electron/llm/visible-stream.ts. Only in-band think tags
// are filtered; provider protocol reasoning fields must never enter this filter.
// Like the TypeScript original, the zero value is ready to use.
type VisibleFilter struct {
	stream  thinkStream
	visible []uint16
}

func NewVisibleFilter() *VisibleFilter {
	return &VisibleFilter{}
}

// Push preserves whitespace and returns exactly this call's visible emission.
func (f *VisibleFilter) Push(text string) string {
	emitted := f.stream.push(text, false)
	f.visible = append(f.visible, emitted...)
	return visibleString(emitted)
}

// Finish deliberately emits nothing. The original has no flush/finish method:
// pending partial markup and unfinished thoughts stay undisclosed. This adapter
// does not clear state, so even a later Push behaves like the original object.
func (f *VisibleFilter) Finish() string {
	return ""
}

func (f *VisibleFilter) Text() string {
	return visibleString(f.visible)
}

// InBandReasoning ports InBandReasoningStream in provider.interface.ts. It emits
// at most one nonempty concatenated reasoning chunk per Push, without tag text.
type InBandReasoning struct {
	stream thinkStream
	emit   func(string)
}

func NewInBandReasoning(emit func(string)) *InBandReasoning {
	return &InBandReasoning{emit: emit}
}

func (r *InBandReasoning) Push(text string) {
	if r.emit == nil {
		return
	}
	if emitted := r.stream.push(text, true); len(emitted) != 0 {
		r.emit(visibleString(emitted))
	}
}

type thinkStream struct {
	pending []uint16
	depth   int
}

// Use UTF-16 units, not Go byte offsets: the original slices the eight-unit tag
// lookahead before lowercasing. This also preserves splits inside surrogate pairs.
func (s *thinkStream) push(text string, reasoning bool) []uint16 {
	input := append(s.pending, visibleUnits(text)...)
	s.pending = nil
	var parts []uint16
	for cursor := 0; cursor < len(input); {
		opening := cursor
		for opening < len(input) && input[opening] != '<' {
			opening++
		}
		if (s.depth > 0) == reasoning && opening > cursor {
			parts = append(parts, input[cursor:opening]...)
		}
		if opening == len(input) {
			break
		}
		cursor = opening
		end := cursor + 8
		if end > len(input) {
			end = len(input)
		}
		tail := input[cursor:end]
		switch {
		case thinkTagPrefix(tail, "<think>"):
			s.depth++
			cursor += len("<think>")
			continue
		case thinkTagPrefix(tail, "</think>"):
			if s.depth > 0 {
				s.depth--
			}
			cursor += len("</think>")
			continue
		}
		if thinkPartialTag(tail, "<think>") || thinkPartialTag(tail, "</think>") {
			s.pending = input[cursor:]
			break
		}
		if (s.depth > 0) == reasoning {
			parts = append(parts, '<')
		}
		cursor++
	}
	return parts
}

// Of the non-ASCII characters, only Kelvin sign lowercases to an ASCII letter
// in these tags. Comparing this way is equivalent to JS toLowerCase followed by
// startsWith; importantly, capital dotted I must NOT be reduced to plain i.
func thinkTagUnit(unit uint16) uint16 {
	if unit >= 'A' && unit <= 'Z' {
		return unit + ('a' - 'A')
	}
	if unit == 0x212a {
		return 'k'
	}
	return unit
}

func thinkTagPrefix(tail []uint16, tag string) bool {
	if len(tail) < len(tag) {
		return false
	}
	for i := 0; i < len(tag); i++ {
		if thinkTagUnit(tail[i]) != uint16(tag[i]) {
			return false
		}
	}
	return true
}

func thinkPartialTag(tail []uint16, tag string) bool {
	if len(tail) > len(tag) {
		return false
	}
	for i, unit := range tail {
		if thinkTagUnit(unit) != uint16(tag[i]) {
			return false
		}
	}
	return true
}

// JavaScript strings can contain lone UTF-16 surrogates. Go strings carry those
// units reversibly as WTF-8; valid scalar strings retain ordinary UTF-8. Standard
// JSON decoding already replaces malformed surrogate escapes before this API.
func visibleUnits(text string) []uint16 {
	var units []uint16
	for len(text) != 0 {
		r, size := utf8.DecodeRuneInString(text)
		if r == utf8.RuneError && size == 1 && len(text) >= 3 && text[0] == 0xed && text[1] >= 0xa0 && text[1] <= 0xbf && text[2] >= 0x80 && text[2] <= 0xbf {
			units = append(units, uint16(text[0]&0x0f)<<12|uint16(text[1]&0x3f)<<6|uint16(text[2]&0x3f))
			text = text[3:]
			continue
		}
		if r > 0xffff {
			hi, lo := utf16.EncodeRune(r)
			units = append(units, uint16(hi), uint16(lo))
		} else {
			units = append(units, uint16(r))
		}
		text = text[size:]
	}
	return units
}

func visibleString(units []uint16) string {
	var out strings.Builder
	for i := 0; i < len(units); i++ {
		unit := units[i]
		if unit >= 0xd800 && unit <= 0xdbff && i+1 < len(units) && units[i+1] >= 0xdc00 && units[i+1] <= 0xdfff {
			out.WriteRune(utf16.DecodeRune(rune(unit), rune(units[i+1])))
			i++
		} else if unit >= 0xd800 && unit <= 0xdfff {
			out.WriteByte(0xe0 | byte(unit>>12))
			out.WriteByte(0x80 | byte((unit>>6)&0x3f))
			out.WriteByte(0x80 | byte(unit&0x3f))
		} else {
			out.WriteRune(rune(unit))
		}
	}
	return out.String()
}
