// Package importsource implements captured-source inspection only. It does not
// publish projects, commit import runs, execute effects, or accept filesystem paths.
package importsource

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"path"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

const (
	MaxChapters     = 5000
	MaxSourceFiles  = 5000
	MaxTotalBytes   = 128 * 1024 * 1024
	MaxChapterBytes = 16 * 1024 * 1024
)

type Limits struct {
	MaxChapters    int
	MaxSourceFiles int
	MaxTotalBytes  int64
}

func DefaultLimits() Limits { return Limits{MaxChapters, MaxSourceFiles, MaxTotalBytes} }
func validLimits(l Limits) bool {
	return l.MaxChapters > 0 && l.MaxChapters <= MaxChapters && l.MaxSourceFiles > 0 && l.MaxSourceFiles <= MaxSourceFiles && l.MaxTotalBytes > 0 && l.MaxTotalBytes <= MaxTotalBytes
}

// Chapter is an immutable-value snapshot. SourceChapterNumber is made unique
// per source by Inspect; the pure parser retains the original numbering.
type Chapter struct {
	Number              int    `json:"number"`
	SourceIndex         int    `json:"sourceIndex"`
	SourceChapterNumber int    `json:"sourceChapterNumber"`
	Title               string `json:"title"`
	Content             string `json:"content"`
	WordCount           int    `json:"wordCount"`
	ContentFingerprint  string `json:"contentFingerprint"`
	ContentSize         int    `json:"contentSize"`
}

type Parsed struct {
	DisplayName string    `json:"displayName"`
	MediaType   string    `json:"mediaType"`
	Size        int64     `json:"size"`
	RawSize     int64     `json:"-"`
	Chapters    []Chapter `json:"chapters"`
}

var (
	cnHeading = regexp.MustCompile(`^第[一二三四五六七八九十百千零0-9]+章[\t\n\v\f\r \x{00a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}：:·—-]*([^\r\x{2028}\x{2029}]*)`)
	enHeading = regexp.MustCompile(`(?i)^Chapter[\t\n\v\f\r \x{00a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}]+([0-9]+)[\t\n\v\f\r \x{00a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}：:·—-]*([^\r\x{2028}\x{2029}]*)`)
	mdHeading = regexp.MustCompile(`(?i)^#{1,3}[\t\n\v\f\r \x{00a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}]+(?:第[一二三四五六七八九十百千零0-9]+章|Chapter[\t\n\v\f\r \x{00a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}]+[0-9]+)[\t\n\v\f\r \x{00a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}：:·—-]*([^\r\x{2028}\x{2029}]*)`)
	cnNumber  = regexp.MustCompile(`第([一二三四五六七八九十百千零0-9]+)章`)
	enNumber  = regexp.MustCompile(`(?i)Chapter[\t\n\v\f\r \x{00a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}]+([0-9]+)`)
)

func jsSpace(r rune) bool {
	return r == '\t' || r == '\n' || r == '\v' || r == '\f' || r == '\r' || r == ' ' || r == 0xa0 || r == 0x1680 || r >= 0x2000 && r <= 0x200a || r == 0x2028 || r == 0x2029 || r == 0x202f || r == 0x205f || r == 0x3000 || r == 0xfeff
}
func trim(s string) string        { return strings.TrimFunc(s, jsSpace) }
func fingerprint(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
func safeDisplayName(s string) bool {
	return trim(s) != "" && utf16Length(s) <= 255 && utf8.ValidString(s) && !strings.ContainsAny(s, "/\\\x00") && s != "." && s != ".."
}

// Count JavaScript string code units where its metadata bounds use .length.
func utf16Length(s string) int {
	n := 0
	for _, r := range s {
		n++
		if r > 0xffff {
			n++
		}
	}
	return n
}

// CountDraftUnits ports src/shared/draft-units.ts algorithm v3, not byte/rune
// length: Han code points, Unicode words, and non-symbol visible remainder.
func CountDraftUnits(s string) int {
	runes := []rune(norm.NFC.String(s))
	n := 0
	for i := 0; i < len(runes); {
		r := runes[i]
		if unicode.Is(unicode.Han, r) {
			n++
			i++
			continue
		}
		if unicode.IsLetter(r) {
			n++
			i++
			for i < len(runes) {
				r = runes[i]
				if !unicode.Is(unicode.Han, r) && (unicode.IsLetter(r) || unicode.IsMark(r)) {
					i++
					continue
				}
				if (r == '\'' || r == '’') && i+1 < len(runes) && unicode.IsLetter(runes[i+1]) && !unicode.Is(unicode.Han, runes[i+1]) {
					i += 2
					continue
				}
				break
			}
			continue
		}
		if !jsSpace(r) && !unicode.IsPunct(r) && !unicode.IsSymbol(r) {
			n++
		}
		i++
	}
	return n
}

func headingTitle(s string) (string, bool) {
	s = trim(s)
	// Negated capture classes preserve JavaScript dot's line-terminator rules.
	for _, re := range []*regexp.Regexp{cnHeading, enHeading, mdHeading} {
		m := re.FindStringSubmatch(s)
		if m != nil {
			title := trim(m[len(m)-1])
			if title == "" {
				title = s
			}
			return title, true
		}
	}
	return s, false
}
func chapterNumber(s string) int {
	if m := cnNumber.FindStringSubmatch(s); m != nil {
		// parseInt consumes a leading decimal run even in mixed Chinese numerals.
		prefix := 0
		for prefix < len(m[1]) && m[1][prefix] >= '0' && m[1][prefix] <= '9' {
			prefix++
		}
		if prefix > 0 {
			return decimalNumber(m[1][:prefix])
		}
		digits := map[rune]int{'零': 0, '一': 1, '二': 2, '三': 3, '四': 4, '五': 5, '六': 6, '七': 7, '八': 8, '九': 9, '十': 10, '百': 100, '千': 1000}
		total, current := 0, 0
		for _, r := range m[1] {
			v, ok := digits[r]
			if !ok {
				continue
			}
			if v >= 10 {
				if current == 0 {
					current = 1
				}
				total += current * v
				current = 0
			} else {
				current = v
			}
		}
		return total + current
	}
	if m := enNumber.FindStringSubmatch(s); m != nil {
		return decimalNumber(m[1])
	}
	return 0
}
func decimalNumber(s string) int {
	v, err := strconv.ParseUint(s, 10, 53)
	if err != nil || v > 9007199254740991 {
		return 0
	}
	return int(v)
}
func makeChapter(number int, title, content string) Chapter {
	return Chapter{Number: number, Title: title, Content: content, WordCount: CountDraftUnits(content), ContentFingerprint: fingerprint(content), ContentSize: len(content)}
}
func splitText(content string, max int) ([]Chapter, error) {
	chapters := make([]Chapter, 0)
	lines := strings.Split(content, "\n")
	hasHeading := false
	for _, line := range lines {
		if _, ok := headingTitle(line); ok {
			hasHeading = true
			break
		}
	}
	if !hasHeading {
		return nil, nil
	}
	var header string
	body := make([]string, 0)
	active := false
	auto := 0
	flush := func() error {
		if !active {
			return nil
		}
		auto++
		text := trim(strings.Join(body, "\n"))
		if text == "" {
			return nil
		}
		if len(chapters) >= max {
			return errors.New("IMPORT_CHAPTER_COUNT_EXCEEDED")
		}
		number := chapterNumber(header)
		if number == 0 {
			number = auto
		}
		title, _ := headingTitle(header)
		chapters = append(chapters, makeChapter(number, title, text))
		return nil
	}
	for _, line := range lines {
		if _, ok := headingTitle(line); ok {
			if err := flush(); err != nil {
				return nil, err
			}
			header = line
			body = body[:0]
			active = true
		} else if active {
			body = append(body, line)
		} else {
			header = line
			active = true
		}
	}
	if err := flush(); err != nil {
		return nil, err
	}
	return chapters, nil
}

// Parse reads a server-captured reader. There is deliberately no Open(path),
// renderer identity, filesystem fallback, subprocess, or ZIP extraction API.
func Parse(displayName string, reader io.Reader, limits Limits) (Parsed, error) {
	out := Parsed{DisplayName: displayName, Chapters: make([]Chapter, 0)}
	if !validLimits(limits) {
		return out, errors.New("IMPORT_RESOURCE_LIMIT_INVALID")
	}
	if !safeDisplayName(displayName) || reader == nil {
		return out, errors.New("IMPORT_SOURCE_INVALID")
	}
	raw, err := io.ReadAll(io.LimitReader(reader, limits.MaxTotalBytes+1))
	if err != nil {
		return out, errors.New("IMPORT_SOURCE_READ_FAILED")
	}
	if int64(len(raw)) > limits.MaxTotalBytes {
		return out, errors.New("IMPORT_SOURCE_BYTES_EXCEEDED")
	}
	out.RawSize = int64(len(raw))
	extension := strings.ToLower(path.Ext(displayName))
	out.MediaType = "text/plain"
	if extension == ".epub" {
		out.MediaType = "application/epub+zip"
		extracted, e := extractEPUB(raw, limits.MaxTotalBytes)
		if e != nil {
			return out, e
		}
		for i, c := range extracted {
			out.Size += int64(len(c.content))
			if out.Size > limits.MaxTotalBytes {
				return out, errors.New("IMPORT_SOURCE_BYTES_EXCEEDED")
			}
			parsed, e := splitText(c.content, limits.MaxChapters-len(out.Chapters))
			if e != nil {
				return out, e
			}
			if parsed == nil {
				parsed = []Chapter{makeChapter(i+1, c.title, c.content)}
			}
			if len(parsed) > limits.MaxChapters-len(out.Chapters) {
				return out, errors.New("IMPORT_CHAPTER_COUNT_EXCEEDED")
			}
			out.Chapters = append(out.Chapters, parsed...)
		}
	} else {
		if extension != ".txt" && extension != ".text" && extension != ".md" {
			return out, errors.New("IMPORT_SOURCE_TYPE_UNSUPPORTED")
		}
		if extension == ".md" {
			out.MediaType = "text/markdown"
		}
		if !utf8.Valid(raw) {
			return out, errors.New("IMPORT_SOURCE_ENCODING_INVALID")
		}
		content := strings.TrimPrefix(string(raw), "\ufeff")
		out.Size = int64(len(content))
		content = trim(content)
		if content == "" {
			return out, errors.New("IMPORT_SOURCE_EMPTY")
		}
		parsed, e := splitText(content, limits.MaxChapters)
		if e != nil {
			return out, e
		}
		if parsed == nil {
			title := strings.TrimSuffix(displayName, path.Ext(displayName))
			number := chapterNumber(title)
			if number == 0 {
				number = 1
			}
			parsed = []Chapter{makeChapter(number, title, content)}
		}
		out.Chapters = parsed
	}
	if len(out.Chapters) == 0 {
		return out, errors.New("IMPORT_SOURCE_TITLE_ONLY")
	}
	for _, c := range out.Chapters {
		if c.ContentSize > MaxChapterBytes {
			return out, errors.New("IMPORT_INSPECTION_INVALID")
		}
	}
	return out, nil
}
