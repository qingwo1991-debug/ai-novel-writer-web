// Package importpreview reproduces the bounded, text-only chapter preview of
// the legacy importer. It has no path, grant, database, or publication access.
package importpreview

import (
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

const (
	MaxSourceBytes = 128 * 1024 * 1024
	MaxChapters    = 5000
)

var (
	ErrInput        = errors.New("IMPORT_PREVIEW_INPUT_INVALID")
	ErrSourceBytes  = errors.New("IMPORT_SOURCE_BYTES_EXCEEDED")
	ErrChapterCount = errors.New("IMPORT_CHAPTER_COUNT_EXCEEDED")
	ErrUnsupported  = errors.New("IMPORT_FORMAT_UNSUPPORTED")
)

type Limits struct {
	MaxBytes    int
	MaxChapters int
}

type Chapter struct {
	Number    int    `json:"number"`
	Title     string `json:"title"`
	Content   string `json:"content"`
	WordCount int    `json:"wordCount"`
}

// ParseText accepts only already read UTF-8 text and a display basename. File
// selection, source identity, grants, and durable import remain separate.
func ParseText(displayName, content string, limits Limits) ([]Chapter, error) {
	if limits.MaxBytes < 1 || limits.MaxBytes > MaxSourceBytes || limits.MaxChapters < 1 || limits.MaxChapters > MaxChapters {
		return nil, ErrInput
	}
	if displayName == "" || displayName == "." || displayName == ".." || filepath.Base(displayName) != displayName || strings.ContainsAny(displayName, "/\\") || !utf8.ValidString(displayName) || !utf8.ValidString(content) {
		return nil, ErrInput
	}
	for _, r := range displayName {
		if unicode.IsControl(r) {
			return nil, ErrInput
		}
	}
	if len(content) > limits.MaxBytes {
		return nil, ErrSourceBytes
	}
	ext := strings.ToLower(filepath.Ext(displayName))
	if ext != ".txt" && ext != ".text" && ext != ".md" {
		return nil, ErrUnsupported
	}
	content = jsTrim(content)
	if content == "" {
		return nil, nil
	}
	lines := strings.Split(content, "\n")
	hasHeading := false
	for _, line := range lines {
		if _, _, ok := heading(line); ok {
			hasHeading = true
			break
		}
	}
	if !hasHeading {
		base := strings.TrimSuffix(displayName, filepath.Ext(displayName))
		number := extractChapterNumber(base)
		if number == 0 {
			number = 1
		}
		return []Chapter{{Number: number, Title: base, Content: content, WordCount: CountDraftUnits(content)}}, nil
	}

	chapters := make([]Chapter, 0, min(len(lines), limits.MaxChapters))
	var header string
	var body []string
	active := false
	autoNumber := 0
	appendCurrent := func() error {
		if !active {
			return nil
		}
		autoNumber++ // The legacy importer advances even for title-only chapters.
		text := jsTrim(strings.Join(body, "\n"))
		if text == "" {
			return nil
		}
		if len(chapters) >= limits.MaxChapters {
			return ErrChapterCount
		}
		number := extractChapterNumber(header)
		if number == 0 {
			number = autoNumber
		}
		title := jsTrim(header)
		if _, parsedTitle, ok := heading(header); ok && parsedTitle != "" {
			title = parsedTitle
		}
		chapters = append(chapters, Chapter{Number: number, Title: title, Content: text, WordCount: CountDraftUnits(text)})
		return nil
	}
	for _, line := range lines {
		if _, _, ok := heading(line); ok {
			if err := appendCurrent(); err != nil {
				return nil, err
			}
			header, body, active = line, nil, true
		} else if active {
			body = append(body, line)
		} else {
			// Preserve the legacy preface rule: the first line is its title.
			header, active = line, true
		}
	}
	if err := appendCurrent(); err != nil {
		return nil, err
	}
	return chapters, nil
}

func heading(line string) (number int, title string, ok bool) {
	trimmed := jsTrim(line)
	s := trimmed
	if strings.HasPrefix(s, "#") {
		i := 0
		for i < len(s) && s[i] == '#' {
			i++
		}
		if i > 3 || i == len(s) {
			return 0, "", false
		}
		space, _ := utf8.DecodeRuneInString(s[i:])
		if !isJSSpace(space) {
			return 0, "", false
		}
		s = strings.TrimLeftFunc(s[i:], isJSSpace)
	}
	if strings.HasPrefix(s, "第") {
		rest := strings.TrimPrefix(s, "第")
		i := 0
		for i < len(rest) {
			r, n := utf8.DecodeRuneInString(rest[i:])
			if !isChineseNumber(r) {
				break
			}
			i += n
		}
		if i == 0 || !strings.HasPrefix(rest[i:], "章") {
			return 0, "", false
		}
		end := rest[i+len("章"):]
		return chineseNumToArabic(rest[:i]), jsTrim(strings.TrimLeftFunc(end, isTitleSeparator)), true
	}
	if len(s) >= len("Chapter") && strings.EqualFold(s[:len("Chapter")], "Chapter") {
		rest := s[len("Chapter"):]
		if rest == "" {
			return 0, "", false
		}
		space, _ := utf8.DecodeRuneInString(rest)
		if !isJSSpace(space) {
			return 0, "", false
		}
		rest = strings.TrimLeftFunc(rest, isJSSpace)
		i := 0
		for i < len(rest) && rest[i] >= '0' && rest[i] <= '9' {
			i++
		}
		if i == 0 {
			return 0, "", false
		}
		n, _ := strconv.Atoi(rest[:i])
		return n, jsTrim(strings.TrimLeftFunc(rest[i:], isTitleSeparator)), true
	}
	return 0, "", false
}

func extractChapterNumber(s string) int {
	for i := 0; i < len(s); i++ {
		if strings.HasPrefix(s[i:], "第") {
			if n, _, ok := heading(s[i:]); ok {
				return n
			}
		}
		if len(s)-i >= 7 && strings.EqualFold(s[i:i+7], "Chapter") {
			if n, _, ok := heading(s[i:]); ok {
				return n
			}
		}
	}
	return 0
}

func chineseNumToArabic(s string) int {
	// JavaScript parseInt first accepts a decimal prefix, even if followed by
	// Chinese numerals. The source format is bounded by the caller's bytes.
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i > 0 {
		n, _ := strconv.Atoi(s[:i])
		return n
	}
	value := map[rune]int{'零': 0, '一': 1, '二': 2, '三': 3, '四': 4, '五': 5, '六': 6, '七': 7, '八': 8, '九': 9, '十': 10, '百': 100, '千': 1000}
	result, current := 0, 0
	for _, r := range s {
		v := value[r]
		if v >= 10 {
			if current == 0 {
				current = 1
			}
			result += current * v
			current = 0
		} else {
			current = v
		}
	}
	return result + current
}

func isChineseNumber(r rune) bool {
	return strings.ContainsRune("一二三四五六七八九十百千零0123456789", r)
}

func isJSSpace(r rune) bool {
	return unicode.IsSpace(r) || r == '\uFEFF'
}

func jsTrim(s string) string { return strings.TrimFunc(s, isJSSpace) }

func isTitleSeparator(r rune) bool {
	return isJSSpace(r) || strings.ContainsRune("：:·—-", r)
}

// CountDraftUnits matches the visible prose counter's NFC/Han/Unicode-word
// algorithm for valid UTF-8 input. It deliberately excludes punctuation and
// emoji while retaining one unit per digit or unpaired combining mark.
func CountDraftUnits(text string) int {
	runes := []rune(norm.NFC.String(text))
	units := 0
	for i := 0; i < len(runes); {
		r := runes[i]
		if unicode.Is(unicode.Han, r) {
			units++
			i++
			continue
		}
		if unicode.IsLetter(r) {
			units++
			i++
			for i < len(runes) && !unicode.Is(unicode.Han, runes[i]) && (unicode.IsLetter(runes[i]) || unicode.IsMark(runes[i])) {
				i++
			}
			for i+1 < len(runes) && (runes[i] == '\'' || runes[i] == '’') && unicode.IsLetter(runes[i+1]) && !unicode.Is(unicode.Han, runes[i+1]) {
				i += 2
				for i < len(runes) && !unicode.Is(unicode.Han, runes[i]) && (unicode.IsLetter(runes[i]) || unicode.IsMark(runes[i])) {
					i++
				}
			}
			continue
		}
		if !isJSSpace(r) && !unicode.IsPunct(r) && !unicode.IsSymbol(r) {
			units++
		}
		i++
	}
	return units
}
