// CI-only fixture: execute the original private chapter splitter and prose
// counter after stripping TypeScript types. No application backend JS is used
// by the Go runtime.
import { readFileSync, writeFileSync } from 'node:fs'
import { stripTypeScriptTypes } from 'node:module'
import { runInNewContext } from 'node:vm'

function section(source, start, end) {
  const a = source.indexOf(start)
  const b = source.indexOf(end, a + start.length)
  if (a < 0 || b < 0 || b <= a) throw new Error(`original import source changed: ${start}`)
  return source.slice(a, b)
}

const controller = readFileSync(new URL('../../electron/controllers/import-controller.ts', import.meta.url), 'utf8')
const units = readFileSync(new URL('../../src/shared/draft-units.ts', import.meta.url), 'utf8')
const source = [
  section(controller, 'const RE_CN_CHAPTER', 'const IMPORT_GRANT_TTL_MS'),
  section(controller, 'function chineseNumToArabic', 'export function registerImportController'),
  section(units, 'const LEGACY_CHINESE_CHARACTER_PATTERN', '\n// 单章视图'),
  section(units, '// 单章视图', '\nfunction countUncached'),
  units.slice(units.indexOf('function countUncached')),
].join('\n').replace(/\bexport\s+(?=function|const)/g, '')

const cases = [
  { name: 'standard', content: '第1章：雨夜\n雨落长安。\nChapter 2: Dawn\nCafé e\u0301 🙂 42' },
  { name: 'markdown', content: '## Chapter 11 - Dawn\nsecond line\n### 第十二章·归途\n第二段' },
  { name: 'preface', content: '序言\n丢弃标题而保留正文\n第十章：雨夜\nCafé 中 🙂 12。' },
  { name: 'title-only', content: '第1章\n第2章\n正文\n第零章\n后文' },
  { name: 'no-heading', content: '雨落长安。\nCafé e\u0301 🙂 42' },
  { name: 'unicode-space', content: '#\u00a0第3章—春夜\n风从海上来。' },
  { name: 'empty-heading', content: 'Chapter 1\n正文' },
  { name: 'count-limit', content: '第1章\na\n第2章\nb', maxChapters: 1 },
]

const executable = stripTypeScriptTypes(source, { mode: 'transform' })
const result = runInNewContext(`${executable}\nJSON.stringify(cases.map(item => {
  try {
    const parsed = hasChapterHeadings(item.content)
      ? splitSingleFileContent(item.content, item.maxChapters ?? 5000)
      : [{ number: 1, title: 'book', content: item.content, wordCount: countDraftUnits(item.content) }]
    return { name: item.name, chapters: parsed }
  } catch (error) {
    return { name: item.name, error: error.message }
  }
}))`, { cases })
const output = JSON.stringify({ format: 1, cases: JSON.parse(result) })
if (!process.env.GO_IMPORTPREVIEW_CONTRACT) throw new Error('GO_IMPORTPREVIEW_CONTRACT required')
writeFileSync(process.env.GO_IMPORTPREVIEW_CONTRACT, output)
