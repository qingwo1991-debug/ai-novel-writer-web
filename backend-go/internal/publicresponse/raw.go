package publicresponse

import (
	"encoding/json"
	"sort"
	"strconv"
	"unicode/utf8"
)

// encoding/json replaces unpaired JSON surrogate escapes with U+FFFD. The JS
// oracle does not. Parse only already-validated, bounded JSON into private nodes
// so literal redaction retains exact code units and object insertion order.
type rawField struct {
	key   string
	value any
}
type rawObject []rawField
type rawParser struct {
	data []byte
	pos  int
}

func (p *rawParser) space() {
	for p.pos < len(p.data) {
		switch p.data[p.pos] {
		case ' ', '\t', '\n', '\r':
			p.pos++
		default:
			return
		}
	}
}

func (p *rawParser) value() any {
	p.space()
	switch p.data[p.pos] {
	case '"':
		return p.text()
	case '{':
		p.pos++
		p.space()
		fields := rawObject{}
		positions := make(map[string]int)
		if p.data[p.pos] == '}' {
			p.pos++
			return fields
		}
		for {
			p.space()
			key := p.text()
			p.space()
			p.pos++
			value := p.value()
			if i, ok := positions[key]; ok {
				fields[i].value = value
			} else {
				positions[key] = len(fields)
				fields = append(fields, rawField{key, value})
			}
			p.space()
			delimiter := p.data[p.pos]
			p.pos++
			if delimiter == '}' {
				break
			}
		}
		// Object.keys lists canonical uint32 index keys first, in numeric order.
		sort.SliceStable(fields, func(i, j int) bool {
			a, ai := arrayIndex(fields[i].key)
			b, bi := arrayIndex(fields[j].key)
			if ai != bi {
				return ai
			}
			return ai && a < b
		})
		return fields
	case '[':
		p.pos++
		p.space()
		out := []any{}
		if p.data[p.pos] == ']' {
			p.pos++
			return out
		}
		for {
			out = append(out, p.value())
			p.space()
			delimiter := p.data[p.pos]
			p.pos++
			if delimiter == ']' {
				return out
			}
		}
	case 'n':
		p.pos += 4
		return nil
	case 't':
		p.pos += 4
		return true
	case 'f':
		p.pos += 5
		return false
	default:
		start := p.pos
		for p.pos < len(p.data) {
			b := p.data[p.pos]
			if b != '-' && b != '+' && b != '.' && b != 'e' && b != 'E' && (b < '0' || b > '9') {
				break
			}
			p.pos++
		}
		return json.Number(string(p.data[start:p.pos]))
	}
}

func arrayIndex(key string) (uint64, bool) {
	if key == "" || len(key) > 10 || (len(key) > 1 && key[0] == '0') {
		return 0, false
	}
	for _, b := range []byte(key) {
		if b < '0' || b > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseUint(key, 10, 32)
	return n, err == nil && n < 0xffffffff
}

func hexUnit(b byte) uint16 {
	if b >= '0' && b <= '9' {
		return uint16(b - '0')
	}
	if b >= 'a' && b <= 'f' {
		return uint16(b - 'a' + 10)
	}
	return uint16(b - 'A' + 10)
}

func (p *rawParser) text() string {
	p.pos++
	units := make([]uint16, 0)
	for p.data[p.pos] != '"' {
		b := p.data[p.pos]
		if b == '\\' {
			p.pos++
			escape := p.data[p.pos]
			p.pos++
			switch escape {
			case 'u':
				unit := uint16(0)
				for i := 0; i < 4; i++ {
					unit = unit<<4 | hexUnit(p.data[p.pos])
					p.pos++
				}
				units = append(units, unit)
			case 'b':
				units = append(units, '\b')
			case 'f':
				units = append(units, '\f')
			case 'n':
				units = append(units, '\n')
			case 'r':
				units = append(units, '\r')
			case 't':
				units = append(units, '\t')
			default:
				units = append(units, uint16(escape))
			}
		} else {
			r, size := utf8.DecodeRune(p.data[p.pos:])
			p.pos += size
			if r <= 0xffff {
				units = append(units, uint16(r))
			} else {
				r -= 0x10000
				units = append(units, uint16(r>>10)+0xd800, uint16(r&0x3ff)+0xdc00)
			}
		}
	}
	p.pos++
	return unitsString(units)
}

func (c *clone) rawVisit(value any, depth int) (any, bool) {
	switch node := value.(type) {
	case rawObject:
		if depth > MaxDepth {
			return c.text(truncated), true
		}
		out := make(map[string]any)
		for _, field := range node {
			if unsafeKey(field.key) {
				continue
			}
			key := c.text(field.key)
			if unsafeKey(key) {
				continue
			}
			if credentialKey(field.key) {
				out[key] = ""
			} else {
				out[key], _ = c.rawVisit(field.value, depth+1)
			}
		}
		return out, true
	case []any:
		if depth > MaxDepth {
			return c.text(truncated), true
		}
		out := make([]any, len(node))
		for i, item := range node {
			out[i], _ = c.rawVisit(item, depth+1)
		}
		return out, true
	case string:
		return c.text(node), true
	default:
		return value, true
	}
}
