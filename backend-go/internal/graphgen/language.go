package graphgen

// Writing languages accepted by a project (src/shared/writing-language.ts:1).
const (
	WritingLanguageZhCN = "zh-CN"
	WritingLanguageEnUS = "en-US"
)

// DefaultWritingLanguage mirrors DEFAULT_WRITING_LANGUAGE (writing-language.ts:5).
const DefaultWritingLanguage = WritingLanguageZhCN

// WritingLanguages mirrors WRITING_LANGUAGES (writing-language.ts:1) in source order.
var WritingLanguages = []string{WritingLanguageZhCN, WritingLanguageEnUS}

// ResolveWritingLanguage mirrors resolveWritingLanguage (src/shared/writing-language.ts:7-11):
// any value that is not exactly one of WRITING_LANGUAGES falls back to zh-CN.
// The TypeScript signature takes `unknown`; Go callers pass the raw string, and
// the empty string or an unknown tag resolves to the same default.
func ResolveWritingLanguage(value string) string {
	for _, language := range WritingLanguages {
		if language == value {
			return value
		}
	}
	return DefaultWritingLanguage
}

// WritingLanguageText mirrors writingLanguageText (src/shared/writing-language.ts:14-20):
// built-in copy is selected by project language and never by inspecting user content.
func WritingLanguageText(language, zhCNText, enUSText string) string {
	if language == WritingLanguageEnUS {
		return enUSText
	}
	return zhCNText
}
