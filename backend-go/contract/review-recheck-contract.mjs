import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'

import { buildReviewCycleRecheckReport } from '../../src/shared/review-cycle.ts'

// The checked-in TypeScript function is the oracle. All text is synthetic and
// this file is executed only on an isolated GitHub Actions runner.
assert.equal(process.env.GITHUB_ACTIONS, 'true')
assert.equal(process.platform, 'linux')
const output = process.env.GO_REVIEW_RECHECK_CONTRACT
const runnerTemp = process.env.RUNNER_TEMP
assert(output && runnerTemp && path.isAbsolute(output) && path.isAbsolute(runnerTemp))
const relative = path.relative(runnerTemp, output)
assert(relative && relative !== '..' && !relative.startsWith(`..${path.sep}`) && !path.isAbsolute(relative))
fs.mkdirSync(path.dirname(output), { recursive: true, mode: 0o700 })

const finding = (findingId, overrides = {}) => ({ findingId, targetId: `goal-${findingId}`,
  category: 'continuity', kind: 'objective', problem: '旧事实需要纠正', ...overrides })
const context = (findings, overrides = {}) => ({ version: 2, cycleId: 'cycle-synthetic',
  comparisonVersion: 1, mergedHash: 'a'.repeat(64), findingSetHash: 'b'.repeat(64),
  findings, ...overrides })
const item = (findingId, overrides = {}) => ({ findingId, targetId: `goal-${findingId}`,
  resolved: false, evidenceQuote: '甲推开门', reason: '门闩状态已有说明。', ...overrides })
const outputText = (items, summary = '复核完成。') => JSON.stringify({ summary, items })
const base = { content: outputText([item('a')]), merged: '甲推开门，门闩是新的。',
  context: context([finding('a')]), locale: 'zh-CN', reportVersion: 2 }
const examples = [
  ['v2-unresolved', base],
  ['v1-resolved', { ...base, content: outputText([item('a', { resolved: true })]),
    context: context([finding('a')], { version: 1 }), reportVersion: 1 }],
  ['v2-resolved-awaits-author-zh', { ...base, content: outputText([item('a', { resolved: true })]) }],
  ['v2-resolved-awaits-author-en', { ...base, content: outputText([item('a', { resolved: true,
    reason: 'The door state is explicit.' })], 'Checked.'), locale: 'en-US' }],
  ['v1-fenced', { ...base, content: `  \`\`\`json\n${outputText([item('a')])}\n\`\`\` `, reportVersion: 1 }],
  ['v2-visible-prose', { ...base, content: `结果如下：\n${outputText([item('a')])}\n请核对。` }],
  ['v2-thinking', { ...base, content: `<think>内部推理 {"not":"report"}</think>\n${outputText([item('a')])}` }],
  ['v2-array-root', { ...base, content: `[${outputText([item('a')])}]` }],
  ['v2-two-objects', { ...base, content: `${outputText([item('a')])}\n${outputText([item('a')])}` }],
  ['invalid-extra-root', { ...base, content: JSON.stringify({ summary: 'ok', items: [item('a')], extra: true }) }],
  ['invalid-extra-item', { ...base, content: outputText([item('a', { extra: 1 })]) }],
  ['blank-reason', { ...base, content: outputText([item('a', { reason: '\ufeff \u00a0' })]) }],
  ['duplicate-finding-id', { ...base, content: outputText([item('a'), item('a')]) }],
  ['missing-finding', { ...base, content: outputText([]) }],
  ['wrong-target', { ...base, content: outputText([item('a', { targetId: 'elsewhere' })]) }],
  ['duplicate-quote', { ...base, merged: '甲推开门。又见甲推开门。' }],
  ['missing-quote', { ...base, merged: '乙走进房间。' }],
  ['two-findings-order', { ...base, content: outputText([item('b', { resolved: true }), item('a')]),
    context: context([finding('a'), finding('b')]) }],
  ['must-show-new', { ...base, context: context([finding('a', { mustShow: true })], { sourceContent: '旧章无门' }) }],
  ['must-show-old', { ...base, context: context([finding('a', { mustShow: true })], { sourceContent: '旧章有甲推开门' }) }],
  ['must-show-missing-source', { ...base, context: context([finding('a', { mustShow: true })]) }],
  ['must-show-punctuation-only', { ...base, content: outputText([item('a', { evidenceQuote: '！' })]),
    merged: '甲！推开门', context: context([finding('a', { mustShow: true })], { sourceContent: '旧章' }) }],
  ['special-json-escaping', { ...base, content: outputText([item('a', { reason: '<>&\u2028下一行' })], '<>&\u2029摘要') }],
  ['no-findings', { ...base, context: context([]), content: outputText([]) }],
]
const cases = examples.map(([name, input]) => {
  const result = buildReviewCycleRecheckReport(input.content, input.merged, input.context, input.locale, input.reportVersion)
  return { name, ...input, expected: result, expectedJSON: JSON.stringify(result), expectedPrettyJSON: JSON.stringify(result, null, 2) }
})
assert.equal(cases.length, 24)
assert.equal(cases.find(value => value.name === 'v2-resolved-awaits-author-zh').expected.decisions[0].status, 'unknown')
assert.equal(cases.find(value => value.name === 'v1-resolved').expected.decisions[0].status, 'resolved')
fs.writeFileSync(output, JSON.stringify({ formatVersion: 1, cases }) + '\n', { flag: 'wx', mode: 0o600 })
console.log(JSON.stringify({ formatVersion: 1, cases: cases.length }))
