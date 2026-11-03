import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'

import { parseReviewGenerationResult } from '../../src/shared/review-generation-report.ts'

// Run only in GitHub Actions. The checked-in TypeScript implementation is the
// oracle; all examples are synthetic report text, never author data.
assert.equal(process.env.GITHUB_ACTIONS, 'true')
assert.equal(process.platform, 'linux')
const output = process.env.GO_REVIEW_REPORT_PARSER_CONTRACT
const runnerTemp = process.env.RUNNER_TEMP
assert(output && runnerTemp && path.isAbsolute(output) && path.isAbsolute(runnerTemp))
const relative = path.relative(runnerTemp, output)
assert(relative && relative !== '..' && !relative.startsWith(`..${path.sep}`) && !path.isAbsolute(relative))
fs.mkdirSync(path.dirname(output), { recursive: true, mode: 0o700 })

const item = { category: 'continuity', severity: 'warning', description: '门闩状态需确认。', quote: '甲推开门' }
const base = { summary: '发现一处问题。', items: [item] }
const json = value => JSON.stringify(value)
const examples = [
  ['basic-v1', json(base), 1],
  ['basic-v2', json(base), 2],
  ['fenced-bom', `\ufeff  \`\`\`json\n${json(base)}\n\`\`\`  \ufeff`, 2],
  ['visible-prose', `审稿结果如下：\n${json(base)}\n请核对。`, 2],
  ['surrogate-in-discarded-prose', `序言 "\\ud800"\n${json({ summary: 'ok', items: [{ category: 'x', severity: 'pass', description: 'ok' }] })}`, 2],
  ['paired-thinking', `<think>内部推理${json({ secret: true })}</think>\n${json(base)}`, 2],
  ['orphan-closing-structured', `分析隐藏内容</think>\n\`\`\`json\n${json(base)}\n\`\`\``, 2],
  ['malformed-before-valid', `错误 {bad}，有效 ${json(base)}`, 2],
  ['pass-blank-quote', json({ summary: '   通过   ', items: [{ category: 'style', severity: 'pass', description: '  已确认  ', quote: '\ufeff \u00a0' }] }), 2],
  ['goal-reviews-null', json({ summary: '保留 goalReviews', items: [item], goalReviews: null }), 2],
  ['goal-reviews-object', json({ summary: '保留 goalReviews', items: [item], goalReviews: { goals: [1, '😀'], x: true } }), 2],
  ['v1-codepoints', json({ summary: `  ${'😀'.repeat(122)}尾巴  `, items: [{ ...item, description: ` ${'甲'.repeat(202)}尾巴 `, quote: ` ${'𐀀'.repeat(162)}尾巴 ` }] }), 1],
  ['v2-no-bound', json({ summary: `  ${'😀'.repeat(122)}尾巴  `, items: [{ ...item, description: ` ${'甲'.repeat(202)}尾巴 `, quote: ` ${'𐀀'.repeat(162)}尾巴 ` }] }), 2],
  ['js-trim-not-nel', json({ summary: '\u0085a\u0085', items: [{ category: 'x', severity: 'warning', description: '\u0085b\u0085', quote: '\u0085c\u0085' }] }), 2],
  ['multiple-results', `${json(base)}\n${json(base)}`, 2],
  ['array-root-inner-object', `[${json(base)}]`, 2],
  ['truncated-object', `说明：${json(base).slice(0, -2)}`, 2],
  ['unknown-top-field', json({ ...base, extra: 1 }), 2],
  ['unknown-item-field', json({ summary: 'x', items: [{ ...item, extra: 1 }] }), 2],
  ['missing-warning-quote', json({ summary: 'x', items: [{ category: 'x', severity: 'warning', description: 'x' }] }), 2],
  ['blank-warning-quote', json({ summary: 'x', items: [{ category: 'x', severity: 'warning', description: 'x', quote: '\ufeff ' }] }), 2],
  ['empty-after-trim', json({ summary: '\ufeff \u00a0', items: [item] }), 2],
  ['too-many-items', json({ summary: 'x', items: Array.from({ length: 11 }, () => item) }), 2],
  ['invalid-version', json(base), 3],
  ['isolated-surrogate', '{"summary":"\\ud800","items":[{"category":"x","severity":"pass","description":"ok"}]}', 2],
]

const cases = examples.map(([name, content, version]) => {
  let expected
  try { expected = { ok: true, value: parseReviewGenerationResult(content, version) } }
  catch { expected = { ok: false } }
  return { name, content, version, expected, unsupportedUnicode: name === 'isolated-surrogate' }
})
assert.equal(cases.length, 25)
assert.equal(cases.find(item => item.name === 'isolated-surrogate').expected.ok, true)
assert.equal(cases.find(item => item.name === 'multiple-results').expected.ok, false)
assert.equal(cases.find(item => item.name === 'v1-codepoints').expected.value.summary.length, 240)
fs.writeFileSync(output, JSON.stringify({ formatVersion: 1, cases }) + '\n', { flag: 'wx', mode: 0o600 })
console.log(JSON.stringify({ formatVersion: 1, cases: cases.length, accepted: cases.filter(item => item.expected.ok).length,
  unsupportedUnicode: 1 }))
