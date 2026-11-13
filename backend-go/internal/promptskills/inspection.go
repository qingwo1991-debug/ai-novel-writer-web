// Package promptskills implements prompt-only writing skills. No skill is executed.
package promptskills

import (
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"unicode/utf16"
)

const MaxSkillBytes = 64 * 1024
const MaxBundledFiles = 16
const MaxFolderEntries = 200
const MaxFolderDepth = 4
const BundleManifest = ".pagekin-bundle.json"

var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
var segmentPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

func ValidName(s string) bool      { return namePattern.MatchString(s) && s != "." && s != ".." }
func rx(pattern, text string) bool { return regexp.MustCompile(pattern).MatchString(text) }

type Metadata struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName,omitempty"`
	Description string `json:"description"`
	Version     string `json:"version,omitempty"`
	Language    string `json:"language"`
	Stage       string `json:"stage,omitempty"`
}
type Inspection struct {
	Metadata            Metadata `json:"metadata"`
	Content             string   `json:"content"`
	Compatible          bool     `json:"compatible"`
	Reasons             []string `json:"reasons"`
	SuggestedStage      string   `json:"suggestedStage"`
	UTF8Bytes           int      `json:"utf8Bytes"`
	BundledFiles        []string `json:"bundledFiles,omitempty"`
	UnbundledReferences []string `json:"unbundledReferences,omitempty"`
}
type BundledFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}
type Bundle struct {
	Status string        `json:"status"`
	Files  []BundledFile `json:"files,omitempty"`
}

func (b Bundle) MarshalJSON() ([]byte, error) {
	if b.Status == "intact" {
		files := b.Files
		if files == nil {
			files = []BundledFile{}
		}
		return json.Marshal(struct {
			Status string        `json:"status"`
			Files  []BundledFile `json:"files"`
		}{b.Status, files})
	}
	return json.Marshal(struct {
		Status string `json:"status"`
	}{b.Status})
}

// Package inspections include empty arrays; legacy Markdown inspections omit them.
func (i Inspection) MarshalJSON() ([]byte, error) {
	type plain Inspection
	data, err := json.Marshal(plain(i))
	if err != nil {
		return nil, err
	}
	if i.BundledFiles == nil {
		return data, nil
	}
	var value map[string]any
	if err = json.Unmarshal(data, &value); err != nil {
		return nil, err
	}
	value["bundledFiles"] = i.BundledFiles
	unbundled := i.UnbundledReferences
	if unbundled == nil {
		unbundled = []string{}
	}
	value["unbundledReferences"] = unbundled
	return json.Marshal(value)
}

type FileReader func(string) (string, bool)

func unquote(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && ((s[0] == '\'' && s[len(s)-1] == '\'') || (s[0] == '"' && s[len(s)-1] == '"')) {
		return s[1 : len(s)-1]
	}
	return s
}

var frontmatterPattern = regexp.MustCompile(`^---\s*\r?\n([\s\S]*?)\r?\n---\s*(?:\r?\n|$)`)
var fieldPattern = regexp.MustCompile(`^\s*([^:#][^:]*):\s*(.*?)\s*$`)

func frontmatter(raw string, blocks bool) (map[string]string, string) {
	fields := map[string]string{}
	match := frontmatterPattern.FindStringSubmatchIndex(raw)
	if match == nil {
		return fields, strings.TrimSpace(raw)
	}
	lines := strings.Split(strings.ReplaceAll(raw[match[2]:match[3]], "\r\n", "\n"), "\n")
	for i := 0; i < len(lines); i++ {
		m := fieldPattern.FindStringSubmatch(lines[i])
		if m == nil {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(m[1]))
		value := m[2]
		if blocks && rx(`^[>|][+-]?$`, value) {
			block := []string{}
			for i+1 < len(lines) && (rx(`^\s+\S`, lines[i+1]) || strings.TrimSpace(lines[i+1]) == "") {
				i++
				block = append(block, strings.TrimSpace(lines[i]))
			}
			if value[0] == '>' {
				parts := []string{}
				for _, v := range block {
					if v != "" {
						parts = append(parts, v)
					}
				}
				fields[key] = strings.Join(parts, " ")
			} else {
				fields[key] = strings.TrimSpace(strings.Join(block, "\n"))
			}
		} else {
			fields[key] = unquote(value)
		}
	}
	return fields, strings.TrimSpace(raw[match[1]:])
}
func normalizeLanguage(s string) string {
	switch strings.ToLower(s) {
	case "en-us", "en", "english":
		return "en-US"
	case "zh-cn", "zh", "chinese":
		return "zh-CN"
	}
	return "bilingual"
}
func normalizeStage(s string) string {
	switch strings.ToLower(s) {
	case "planning", "drafting", "review", "refinement":
		return strings.ToLower(s)
	}
	return ""
}
func firstUTF16(s string, n int) string {
	units := utf16.Encode([]rune(s))
	if len(units) > n {
		units = units[:n]
	}
	return string(utf16.Decode(units))
}
func inspect(raw string, fields map[string]string, body string, files []BundledFile, relative bool, fallback string) Inspection {
	name := strings.TrimSpace(fields["name"])
	if name == "" {
		name = fallback
	}
	if name == "" {
		name = "unnamed-writing-skill"
	}
	chunks := []string{body}
	for _, f := range files {
		chunks = append(chunks, "---\n\n## "+f.Path+"\n\n"+strings.TrimSpace(f.Content))
	}
	content := strings.Join(chunks, "\n\n")
	reasons := []string{}
	add := func(condition bool, reason string) {
		if condition {
			reasons = append(reasons, reason)
		}
	}
	add(len(raw) > MaxSkillBytes || len(content) > MaxSkillBytes, "content-too-large")
	add(relative, "relative-reference")
	add(rx("(?im)(?:^|[\\s`'\"(])scripts?[/\\\\][^\\s`'\")]+", content) || rx(`(?i)\b(?:run|execute)\s+(?:the\s+)?script\b`, content) || rx(`运行.{0,12}脚本`, content), "script-dependency")
	add(rx("(?im)(?:^|[\\s`'\"(])hooks?[/\\\\][^\\s`'\")]+", content) || rx(`(?i)\b(?:install|run|execute)\s+(?:the\s+)?hook\b`, content) || rx(`安装.{0,12}钩子`, content), "hook-dependency")
	add(rx(`\bsub-?agents?\b|\bdelegate\b.{0,30}\bagents?\b|子代理|子智能体`, strings.ToLower(content)), "subagent-dependency")
	keys := []string{}
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	add(rx(`\ballowed[-_ ]?tools?\b|\btools?\b|\bmcp\b|\bhooks?\b|\bscripts?\b|\bsub-?agents?\b`, strings.Join(keys, " ")) || rx(`(?i)\b(?:use|call|invoke)\s+(?:the\s+)?[a-z0-9_-]+\s+tools?\b`, content) || rx(`(?:使用|调用).{0,24}工具`, content), "tool-dependency")
	stage := normalizeStage(fields["stage"])
	suggested := stage
	if suggested == "" {
		search := strings.ToLower(fields["name"] + " " + fields["description"] + " " + firstUTF16(content, 1000))
		switch {
		case rx(`review|critique|审稿|审阅|检查`, search):
			suggested = "review"
		case rx(`refin|polish|prose|润色|修稿|改写`, search):
			suggested = "refinement"
		case rx(`plan|outline|architect|规划|大纲|设定`, search):
			suggested = "planning"
		default:
			suggested = "drafting"
		}
	}
	description := fields["description"]
	if description == "" {
		description = "Writing skill: " + name
	}
	display := fields["display_name"]
	if display == "" {
		display = fields["display-name"]
	}
	return Inspection{Metadata: Metadata{Name: name, DisplayName: display, Description: description, Version: fields["version"], Language: normalizeLanguage(fields["language"]), Stage: stage}, Content: content, Compatible: len(reasons) == 0, Reasons: reasons, SuggestedStage: suggested, UTF8Bytes: len(content)}
}
func citesDirectory(s string) bool {
	return rx(`(?i)\]\(\s*(?:\.\.?[/\\]|(?:references?|assets?)[/\\])`, s) || rx("(?im)(?:^|[\\s`'\"(])(?:references?|assets?)[/\\\\][^\\s`'\")]+", s) || rx(`(?i)\$\{skill_dir\}`, s)
}
func InspectMarkdown(raw string) Inspection {
	f, b := frontmatter(raw, false)
	return inspect(raw, f, b, nil, citesDirectory(b), "")
}

type reference struct {
	path     string
	blocking bool
	at       int
	base     []string
}

func references(s string) []reference {
	out := []reference{}
	for _, pattern := range []string{`\]\(\s*(<?)([^)\s>]+)`, `(?m)^ {0,3}\[([^\]^\n][^\]\n]*|)\]:[ \t]*(<?)([^\s>]+)`} {
		re := regexp.MustCompile(pattern)
		for _, m := range re.FindAllStringSubmatchIndex(s, -1) {
			angled, path := "", ""
			if len(m) == 6 {
				angled = s[m[2]:m[3]]
				path = s[m[4]:m[5]]
			} else {
				angled = s[m[4]:m[5]]
				path = s[m[6]:m[7]]
			}
			if rx(`(?i)^(?:[a-z][a-z0-9+.-]*:|#|/)`, path) {
				continue
			}
			blocking := rx(`(?i)^(?:\.\.?[/\\]|(?:references?|assets?)[/\\])|\$\{skill_dir\}`, path)
			if angled != "" {
				blocking = rx(`(?i)\$\{skill_dir\}`, path)
			}
			out = append(out, reference{path: path, blocking: blocking, at: m[0]})
		}
	}
	for _, m := range regexp.MustCompile("(?im)(?:^|[\\s`'\"(])((?:\\$\\{skill_dir\\}[/\\\\])?(?:references?|assets?)[/\\\\][^\\s`'\")]+)").FindAllStringSubmatchIndex(s, -1) {
		out = append(out, reference{path: s[m[2]:m[3]], blocking: true, at: m[0]})
	}
	for _, m := range regexp.MustCompile("(?i)\\$\\{skill_dir\\}(?:[/\\\\][^\\s`'\")]+)?").FindAllStringIndex(s, -1) {
		out = append(out, reference{path: s[m[0]:m[1]], blocking: true, at: m[0]})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].at < out[j].at })
	return out
}
func textExtension(s string) bool {
	s = strings.ToLower(s)
	return strings.HasSuffix(s, ".md") || strings.HasSuffix(s, ".markdown") || strings.HasSuffix(s, ".txt")
}
func resolveReference(s string, base []string) string {
	fromRoot := rx(`(?i)^\$\{skill_dir\}`, s)
	s = regexp.MustCompile(`(?i)^\$\{skill_dir\}[/\\]?`).ReplaceAllString(s, "")
	s = regexp.MustCompile(`^[A-Za-z0-9._/\\-]*`).FindString(s)
	s = strings.TrimRight(s, ".")
	s = strings.ReplaceAll(s, "\\", "/")
	if s == "" {
		return ""
	}
	parts := append([]string{}, base...)
	if fromRoot {
		parts = nil
	}
	for _, p := range strings.Split(s, "/") {
		if p == "" || p == "." {
			continue
		}
		if p == ".." {
			if len(parts) == 0 {
				return ""
			}
			parts = parts[:len(parts)-1]
		} else {
			if !segmentPattern.MatchString(p) {
				return ""
			}
			parts = append(parts, p)
		}
	}
	s = strings.Join(parts, "/")
	if !textExtension(s) {
		return ""
	}
	return s
}
func bundleReferences(body string, read FileReader) ([]BundledFile, bool, []string) {
	files := []BundledFile{}
	unbundled := []string{}
	seenUnbundled := map[string]bool{}
	included := map[string]bool{"skill.md": true}
	queue := references(body)
	blocked := false
	for len(queue) > 0 {
		r := queue[0]
		queue = queue[1:]
		candidates := []string{resolveReference(r.path, r.base)}
		if !rx(`^\.\.?[/\\]`, r.path) {
			root := resolveReference(r.path, nil)
			if root != candidates[0] {
				candidates = append(candidates, root)
			}
		}
		var file *BundledFile
		already := false
		for _, p := range candidates {
			if p == "" {
				continue
			}
			if included[strings.ToLower(p)] {
				already = true
				break
			}
			if read != nil {
				if c, ok := read(p); ok {
					file = &BundledFile{Path: p, Content: c}
					break
				}
			}
		}
		if already {
			continue
		}
		if file == nil || len(files) >= MaxBundledFiles {
			if r.blocking {
				blocked = true
			} else {
				shown := regexp.MustCompile(`[#?].*$`).ReplaceAllString(r.path, "")
				if shown != "" && !seenUnbundled[shown] {
					unbundled = append(unbundled, shown)
					seenUnbundled[shown] = true
				}
			}
			continue
		}
		files = append(files, BundledFile{Path: file.Path, Content: strings.TrimSpace(file.Content)})
		included[strings.ToLower(file.Path)] = true
		parts := strings.Split(file.Path, "/")
		nested := references(file.Content)
		for i := range nested {
			nested[i].base = parts[:len(parts)-1]
		}
		queue = append(queue, nested...)
	}
	return files, blocked, unbundled
}
func InspectLocal(raw string, read FileReader, fallback string) Inspection {
	f, b := frontmatter(raw, true)
	files, blocked, unbundled := bundleReferences(b, read)
	result := inspect(raw, f, b, files, blocked, fallback)
	result.BundledFiles = []string{}
	for _, v := range files {
		result.BundledFiles = append(result.BundledFiles, v.Path)
	}
	result.UnbundledReferences = unbundled
	return result
}
func InspectInstalled(raw string, bundle Bundle, fallback string) Inspection {
	if bundle.Status == "none" {
		return InspectMarkdown(raw)
	}
	f, b := frontmatter(raw, true)
	files := []BundledFile{}
	if bundle.Status == "intact" {
		files = bundle.Files
	}
	listed := map[string]string{}
	for _, v := range files {
		listed[strings.ToLower(v.Path)] = v.Content
	}
	_, blocked, unbundled := bundleReferences(b, func(p string) (string, bool) { s, ok := listed[strings.ToLower(p)]; return s, ok })
	result := inspect(raw, f, b, files, blocked, fallback)
	result.BundledFiles = []string{}
	for _, v := range files {
		result.BundledFiles = append(result.BundledFiles, v.Path)
	}
	result.UnbundledReferences = unbundled
	if bundle.Status == "damaged" {
		result.Compatible = false
		result.Reasons = append(result.Reasons, "bundle-damaged")
	}
	return result
}

type GitHubLocation struct {
	Owner     string `json:"owner"`
	Repo      string `json:"repo"`
	Ref       string `json:"ref,omitempty"`
	Path      string `json:"path"`
	SourceURL string `json:"sourceUrl"`
}

func ParseGitHubURL(value string) (GitHubLocation, error) {
	fail := func(message string) (GitHubLocation, error) { return GitHubLocation{}, errors.New(message) }
	u, err := url.Parse(value)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fail("Invalid GitHub URL")
	}
	if u.Scheme != "https" || u.User != nil || u.Port() != "" {
		return fail("Only public HTTPS GitHub URLs are supported")
	}
	parts := []string{}
	for _, p := range strings.Split(u.EscapedPath(), "/") {
		if p != "" {
			parts = append(parts, p)
		}
	}
	safe := func(p string) (string, error) {
		s, e := url.PathUnescape(p)
		if e != nil || !segmentPattern.MatchString(s) || s == "." || s == ".." {
			return "", errors.New("Invalid GitHub path segment")
		}
		return s, nil
	}
	location := GitHubLocation{SourceURL: u.String()}
	var pathParts []string
	if strings.EqualFold(u.Hostname(), "raw.githubusercontent.com") {
		if len(parts) < 4 {
			return fail("Incomplete raw GitHub URL")
		}
		location.Ref, err = safe(parts[2])
		pathParts = parts[3:]
	} else {
		if !strings.EqualFold(u.Hostname(), "github.com") || len(parts) < 2 {
			return fail("Only github.com and raw.githubusercontent.com are supported")
		}
		if len(parts) == 2 {
			pathParts = []string{"SKILL.md"}
		} else {
			if (parts[2] != "tree" && parts[2] != "blob") || len(parts) < 4 {
				return fail("Use a GitHub repository, directory, blob, or raw SKILL.md URL")
			}
			location.Ref, err = safe(parts[3])
			pathParts = append([]string{}, parts[4:]...)
			if parts[2] == "tree" {
				pathParts = append(pathParts, "SKILL.md")
			}
		}
	}
	if err != nil {
		return GitHubLocation{}, err
	}
	location.Owner, err = safe(parts[0])
	if err != nil {
		return GitHubLocation{}, err
	}
	repo := regexp.MustCompile(`(?i)\.git$`).ReplaceAllString(parts[1], "")
	location.Repo, err = safe(repo)
	if err != nil {
		return GitHubLocation{}, err
	}
	decoded := []string{}
	for _, p := range pathParts {
		s, e := safe(p)
		if e != nil {
			return GitHubLocation{}, e
		}
		decoded = append(decoded, s)
	}
	location.Path = strings.Join(decoded, "/")
	if location.Path == "" || !strings.HasSuffix(strings.ToLower(location.Path), "skill.md") {
		return fail("The GitHub source must resolve to a SKILL.md file")
	}
	return location, nil
}
