import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'

import { mergeConsistencyFindingsIntoReview } from '../../src/shared/consistency-preflight.ts'

// Original TypeScript supplies synthetic, GitHub-only differential fixtures.
// This does not calculate preflight findings or certify a saved M03 review.
assert.equal(process.env.GITHUB_ACTIONS, 'true')
const output = process.env.GO_REVIEW_CONSISTENCY_MERGE_CONTRACT
const runnerTemp = process.env.RUNNER_TEMP
assert(output && runnerTemp && path.isAbsolute(output) && path.isAbsolute(runnerTemp))
const relative = path.relative(runnerTemp, output)
assert(relative && relative !== '..' && !relative.startsWith(`..${path.sep}`) && !path.isAbsolute(relative))
fs.mkdirSync(path.dirname(output), { recursive: true, mode: 0o700 })

function finding(key, chapter, evidence, zhCN, enUS) {
  return { stableFactKey: key, severity: 'warning', sourceChapter: chapter,
    evidence, issue: { zhCN, enUS }, suggestion: {
      zhCN: '调整蓝图，或说明这是回忆、幻象等刻意安排。',
      enUS: 'Adjust the blueprint, or record an intentional device such as a flashback or vision.',
    } }
}

const first = finding('fact:0001', 1, '甲推开门', '证据需要核对。', 'Evidence needs checking.')
const second = finding('fact:0002', 7, '乙在终章离开。\u2028<&> 😀',
  '状态未知，须核实“乙”。', 'Status unresolved; verify “B”.')
const cases = [
  { name: 'empty-finding-list', review: { summary: '保留原结论', items: [{ category: 'plot', severity: 'pass', description: '通过' }] },
    findings: [], locale: 'zh-CN' },
  { name: 'zh-append-in-source-order', review: { summary: '审稿', items: [{ category: 'human', severity: 'warning', description: '原项' }],
    goalReview: { coverage: 'not_configured', items: [] } }, findings: [first, second], locale: 'zh-CN' },
  { name: 'en-reversed-order', review: { summary: 'Review', items: [] }, findings: [second, first], locale: 'en-US' },
  { name: 'missing-items-preserves-extra-fields', review: { summary: 'Only summary', extension: { z: 1, a: 'x<&>' } },
    findings: [first], locale: 'en-US' },
  { name: 'nonarray-items-becomes-empty', review: { summary: 'Review', items: null }, findings: [first], locale: 'zh-CN' },
  { name: 'unicode-control-escaping', review: { summary: '含有\u2028换行', items: [{ description: '原值\n第二行', quote: '<&>' }] },
    findings: [second], locale: 'zh-CN' },
]
const rendered = cases.map(sample => {
  const originalReviewJSON = JSON.stringify(sample.review)
  const expectedJSON = JSON.stringify(mergeConsistencyFindingsIntoReview(sample.review, sample.findings, sample.locale))
  const expected = JSON.parse(expectedJSON)
  assert(Array.isArray(expected.items))
  assert.equal(expected.items.length, (Array.isArray(sample.review.items) ? sample.review.items.length : 0) + sample.findings.length)
  for (let index = 0; index < sample.findings.length; index++) {
    const mapped = expected.items[expected.items.length - sample.findings.length + index]
    assert.deepEqual(Object.keys(mapped), ['category', 'severity', 'description', 'quote', 'stableFactKey', 'sourceChapter'])
  }
  return { name: sample.name, reviewJSON: originalReviewJSON, findings: sample.findings, locale: sample.locale, expectedJSON }
})
assert.equal(rendered.length, 6)
fs.writeFileSync(output, JSON.stringify({ formatVersion: 1, cases: rendered }) + '\n', { flag: 'wx', mode: 0o600 })
console.log(JSON.stringify({ formatVersion: 1, cases: rendered.map(({ name, locale }) => ({ name, locale })) }))
