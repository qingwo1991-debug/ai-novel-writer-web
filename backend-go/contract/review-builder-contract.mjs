import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'

import { buildReviewGenerationReport } from '../../src/shared/review-generation-report.ts'

// The original TypeScript is the GitHub-only oracle. These synthetic reports
// never represent user chapters or a complete M03 admission proof.
assert.equal(process.env.GITHUB_ACTIONS, 'true')
assert.equal(process.platform, 'linux')
const output = process.env.GO_REVIEW_BUILDER_CONTRACT
const runnerTemp = process.env.RUNNER_TEMP
assert(output && runnerTemp && path.isAbsolute(output) && path.isAbsolute(runnerTemp))
const relative = path.relative(runnerTemp, output)
assert(relative && relative !== '..' && !relative.startsWith(`..${path.sep}`) && !path.isAbsolute(relative))
fs.mkdirSync(path.dirname(output), { recursive: true, mode: 0o700 })

const item = (severity = 'pass') => ({ category: '情节', severity, description: '门闩状态已核对。',
  ...(severity === 'pass' ? {} : { quote: '门闩仍开着' }) })
const goal = (id = 'ch7:keyEvents:1', text = '合上门闩') => ({ id, text })
const frozen = (items = [goal()], coverage = 'complete') => ({ chapterNumber: 7, coverage, items })
const result = (summary, items, goalReviews) => JSON.stringify({ summary, items, ...(goalReviews === undefined ? {} : { goalReviews }) })
const entry = (id, status, description, quotes) => ({ id, evidence: quotes.map(quote => ({ quote })), description, status })
const finding = (key, evidence) => ({ stableFactKey: key, severity: 'warning', sourceChapter: 2, evidence,
  issue: { zhCN: '旧章状态可能冲突。', enUS: 'Earlier chapter state may conflict.' },
  suggestion: { zhCN: '核对蓝图。', enUS: 'Check the blueprint.' } })
const complete = entry('ch7:keyEvents:1', 'completed', '已合上。', ['合上门闩'])
const unmet = entry('ch7:keyEvents:1', 'unmet', '仍未合。', ['门闩仍开着'])
const examples = [
  { name: 'zh-complete', content: result('审稿通过。', [item()], [complete]), sourceContent: '甲合上门闩。',
    frozenGoals: frozen(), writingLanguage: 'zh-CN', uiLocale: 'zh-CN', preflightFindings: [] },
  { name: 'en-unmet', content: result('Review complete.', [item('warning')], [unmet]), sourceContent: '门闩仍开着。',
    frozenGoals: frozen(), writingLanguage: 'en-US', uiLocale: 'en-US', preflightFindings: [] },
  { name: 'zh-unknown-coverage', content: result('起初通过。', [item()], null), sourceContent: '甲路过门口。',
    frozenGoals: frozen(), writingLanguage: 'zh-CN', uiLocale: 'zh-CN', preflightFindings: [] },
  { name: 'empty-not-configured', content: result('尚无目标。', [item()]), sourceContent: '空场。',
    frozenGoals: frozen([], 'not_configured'), writingLanguage: 'zh-CN', uiLocale: 'en-US', preflightFindings: [] },
  { name: 'v1-codepoint-bound', content: result(` ${'😀'.repeat(122)} `, [{ category: 'x', severity: 'warning',
    description: ` ${'甲'.repeat(202)} `, quote: ` ${'𐀀'.repeat(162)} ` }], [complete]),
    sourceContent: '甲合上门闩。', frozenGoals: frozen(), writingLanguage: 'zh-CN', uiLocale: 'zh-CN',
    preflightFindings: [], reportVersion: 1 },
  { name: 'two-goals-preflight', content: result('原摘要。', [item()], [complete,
    entry('ch7:mustShow:1', 'unknown', '无法确认。', [])]), sourceContent: '甲合上门闩。',
    frozenGoals: frozen([goal(), goal('ch7:mustShow:1', '出示木钥匙')]), writingLanguage: 'zh-CN',
    uiLocale: 'zh-CN', preflightFindings: [finding('fact:1', '旧章门闩关闭。'), finding('fact:2', '旧章钥匙遗失。')] },
  { name: 'unicode-escaping', content: result('含有\u2028分隔、<>&', [{ category: '声调', severity: 'pass',
    description: '第一行\n第二行 "\\"' }], [entry('ch7:keyEvents:1', 'completed', '行\u2028动', ['合上门闩'])]),
    sourceContent: '😀合上门闩。', frozenGoals: frozen([goal('ch7:keyEvents:1', '门闩\n闭合')]),
    writingLanguage: 'zh-CN', uiLocale: 'en-US', preflightFindings: [finding('fact:\u2028', '门闩仍开着')] },
  { name: 'invalid-model-item', content: result('坏报告。', [{ category: 'x', severity: 'warning', description: '缺引文' }], [complete]),
    sourceContent: '甲合上门闩。', frozenGoals: frozen(), writingLanguage: 'zh-CN', uiLocale: 'zh-CN', preflightFindings: [] },
]

const cases = examples.map(({ name, ...input }) => {
  try {
    const report = buildReviewGenerationReport(input)
    return { name, input, expected: { ok: true, compact: JSON.stringify(report), pretty: JSON.stringify(report, null, 2) } }
  } catch {
    return { name, input, expected: { ok: false } }
  }
})
assert.equal(cases.length, 8)
assert.equal(cases.filter(sample => sample.expected.ok).length, 7)
assert.equal(cases.find(sample => sample.name === 'invalid-model-item').expected.ok, false)
fs.writeFileSync(output, JSON.stringify({ formatVersion: 1, cases }) + '\n', { flag: 'wx', mode: 0o600 })
console.log(JSON.stringify({ formatVersion: 1, cases: cases.length, accepted: 7 }))
