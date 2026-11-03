import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'

import { normalizeChapterGoalReview, chapterGoalReviewItems } from '../../src/shared/chapter-goal-review.ts'

// This manifest is generated from the original TypeScript only in GitHub Actions.
assert.equal(process.env.GITHUB_ACTIONS, 'true')
assert.equal(process.platform, 'linux')
const output = process.env.GO_REVIEW_GOALS_CONTRACT
const runnerTemp = process.env.RUNNER_TEMP
assert(output && runnerTemp && path.isAbsolute(output) && path.isAbsolute(runnerTemp))
const relative = path.relative(runnerTemp, output)
assert(relative && relative !== '..' && !relative.startsWith(`..${path.sep}`) && !path.isAbsolute(relative))
fs.mkdirSync(path.dirname(output), { recursive: true, mode: 0o700 })

const goal = (id, text = '归还借书') => ({ id, text })
const frozen = (items, coverage = 'complete', chapterNumber = 7) => ({ chapterNumber, coverage, items })
const g1 = goal('ch7:keyEvents:1')
const g2 = goal('ch7:mustShow:1', '明确展示门闩')
const entry = (id, status, description, quotes) => ({ id, status, description, evidence: quotes.map(quote => ({ quote })) })
const examples = [
  ['zh-two-valid', [entry(g1.id, 'completed', '已经归还。', ['交还借书']), entry(g2.id, 'unmet', '门闩未合。', ['门闩仍开着'])], frozen([g1, g2]), '甲交还借书。门闩仍开着。', 'zh-CN'],
  ['en-unknown', [entry(g1.id, 'unknown', 'Cannot determine.', [])], frozen([g1]), 'No book scene.', 'en-US'],
  ['missing-item', [], frozen([g1]), '甲交还借书。', 'zh-CN'],
  ['raw-not-array', null, frozen([g1]), '甲交还借书。', 'en-US'],
  ['duplicate-id', [entry(g1.id, 'unknown', 'A', []), entry(g1.id, 'unknown', 'B', [])], frozen([g1]), '', 'zh-CN'],
  ['alien-id', [entry(g1.id, 'unknown', 'A', []), entry('other', 'unknown', 'B', [])], frozen([g1]), '', 'zh-CN'],
  ['non-record', [entry(g1.id, 'unknown', 'A', []), 4], frozen([g1]), '', 'zh-CN'],
  ['extra-candidate-key', [{ ...entry(g1.id, 'unknown', 'A', []), extra: true }], frozen([g1]), '', 'zh-CN'],
  ['extra-evidence-key', [{ ...entry(g1.id, 'completed', 'A', []), evidence: [{ quote: '归还', source: 'draft' }] }], frozen([g1]), '归还', 'zh-CN'],
  ['bad-evidence-array', [{ id: g1.id, status: 'completed', description: 'A', evidence: null }], frozen([g1]), '归还', 'zh-CN'],
  ['bad-status', [entry(g1.id, 'done', 'A', [])], frozen([g1]), '', 'zh-CN'],
  ['blank-description-js-trim', [entry(g1.id, 'unknown', '\ufeff\u00a0', [])], frozen([g1]), '', 'en-US'],
  ['no-evidence-completed', [entry(g1.id, 'completed', 'A', [])], frozen([g1]), '', 'zh-CN'],
  ['missing-quote', [entry(g1.id, 'unmet', 'A', ['不存在'])], frozen([g1]), '归还', 'zh-CN'],
  ['exact-repeated-first', [entry(g1.id, 'completed', 'A', ['归还'])], frozen([g1]), '归还，再次归还', 'zh-CN'],
  ['wrapped-curly-unique', [entry(g1.id, 'completed', 'A', ['“归还”'])], frozen([g1]), '甲归还了书。', 'zh-CN'],
  ['wrapped-straight-unique', [entry(g1.id, 'completed', 'A', ['"归还"'])], frozen([g1]), '甲归还了书。', 'zh-CN'],
  ['wrapped-duplicate', [entry(g1.id, 'completed', 'A', ['“归还”'])], frozen([g1]), '归还，归还', 'zh-CN'],
  ['wrapped-overlapping', [entry(g1.id, 'completed', 'A', ['"aa"'])], frozen([g1]), 'aaa', 'zh-CN'],
  ['wrapped-empty', [entry(g1.id, 'completed', 'A', ['""'])], frozen([g1]), 'abc', 'zh-CN'],
  ['utf16-emoji-prefix', [entry(g1.id, 'completed', 'Emoji.', ['归还'])], frozen([g1]), '😀归还', 'en-US'],
  ['utf16-emoji-quote', [entry(g1.id, 'completed', 'Emoji.', ['😀归还'])], frozen([g1]), '甲😀归还', 'en-US'],
  ['unicode-json-order', [entry(g1.id, 'completed', 'L\u2028R "\\"', ['归还'])], frozen([goal(g1.id, '行\n动')]), '归还', 'zh-CN'],
  ['empty-not-configured', null, frozen([], 'not_configured'), '', 'zh-CN'],
  ['empty-unknown', null, frozen([], 'unknown'), '', 'en-US'],
  ['empty-complete-extraneous', [entry('alien', 'unknown', 'A', [])], frozen([], 'complete'), '', 'en-US'],
]

const cases = examples.map(([name, raw, goals, draft, language]) => {
  const review = normalizeChapterGoalReview(raw, goals, draft, language)
  const items = chapterGoalReviewItems(review, language)
  return { name, raw, goals, draft, language, expectedReview: review,
    expectedReviewJSON: JSON.stringify(review), expectedItems: items, expectedItemsJSON: JSON.stringify(items) }
})
assert.equal(cases.length, 26)
assert.equal(cases.find(item => item.name === 'wrapped-overlapping').expectedReview.coverage, 'unknown')
assert.equal(cases.find(item => item.name === 'utf16-emoji-prefix').expectedReview.items[0].evidence[0].start, 2)
fs.writeFileSync(output, JSON.stringify({ formatVersion: 1, cases }) + '\n', { flag: 'wx', mode: 0o600 })
console.log(JSON.stringify({ formatVersion: 1, cases: cases.length }))
