import assert from 'node:assert/strict'
import { existsSync, mkdirSync, writeFileSync } from 'node:fs'
import path from 'node:path'
import { VisibleStreamFilter } from '../../electron/llm/visible-stream.ts'
import { InBandReasoningStream } from '../../electron/llm/provider.interface.ts'

// Invoke only in GitHub Actions, through the existing register-typescript.mjs
// loader. This oracle imports the original classes; it never duplicates their
// scanner, reads author content/configuration, or accesses any credential key.
assert.equal(process.env.GITHUB_ACTIONS, 'true', 'contract generation is GitHub-only')
const base = process.env.RUNNER_TEMP
const target = process.env.GO_VISIBLE_CONTRACT
assert(base && target && path.isAbsolute(base) && path.isAbsolute(target))
const relative = path.relative(base, target)
assert(relative && !relative.startsWith('..') && !path.isAbsolute(relative), 'must use disposable runner directory')
assert.equal(path.basename(target), 'visible-contract.json')
assert.equal(path.normalize(target), target, 'contract path must be canonical')
assert(!existsSync(target), 'do not overwrite any existing contract')

// There is deliberately no EOF flush in the source. The Go Finish adapter must
// emit nothing and preserve state. Fail if the original ever acquires one, so
// that a future source change cannot silently invalidate that adapter contract.
for (const method of ['finish', 'flush']) {
  assert.equal(typeof VisibleStreamFilter.prototype[method], 'undefined', `review new original ${method} semantics`)
  assert.equal(typeof InBandReasoningStream.prototype[method], 'undefined', `review new original ${method} semantics`)
}

// Arrays of UTF-16 code units prevent Go encoding/json from silently replacing
// an unpaired surrogate. Both chunks and results include actual JS half-pairs.
const units = text => Array.from({ length: text.length }, (_, i) => text.charCodeAt(i))
const cases = []
const add = (name, chunks) => {
  const visible = new VisibleStreamFilter()
  let currentReasoning = []
  const reasoning = new InBandReasoningStream(chunk => currentReasoning.push(units(chunk)))
  const steps = chunks.map(chunk => {
    currentReasoning = []
    const emitted = visible.push(chunk)
    reasoning.push(chunk)
    return { input: units(chunk), emitted: units(emitted), text: units(visible.text), reasoning: currentReasoning }
  })
  cases.push({ name, steps, finish: [], text: units(visible.text) })
}

const sources = [
  ['empty', ''],
  ['plain', '  visible\t\r\n正文 😀\u0000<&>\u2028\u2029  '],
  ['simple', 'before<think>private</think>after'],
  ['nested', 'a<think>x<think>y</think>z</think>b'],
  ['unmatched-close', '</think>a</think>b'],
  ['unmatched-open', 'a<think>private<think>nested'],
  ['case', 'a<ThInK>x</tHiNk>b<THINK>y</THINK>c'],
  ['kelvin-case', 'a<thinK>x</THINK>b'],
  ['dotted-i', 'a<thİnk>x</thİnk>b'],
  ['invalid-tags', 'a<think >b< think>c</think >d<thinking>e'],
  ['literal-markup', 'a<foo>x</foo><<<!><>b'],
  ['partial-open', 'a<thi'],
  ['partial-close', 'a</thin'],
  ['hidden-partial', 'a<think>x</thin'],
  ['hidden-invalid', 'a<think>x<no>y<<z</think>b'],
  ['empty-thought', '<think></think><think><think></think></think>'],
  ['whitespace', '\t \r\n<think> \t\r\n </think>\r\n \t'],
  ['surrogate-pair', '😀<think>🧠</think>🚀'],
  ['lone-surrogates', '\ud800a\udfff<think>\ud800x\udfff</think>\udfff\ud800'],
  ['surrogate-near-tag', '<thi\ud800nk>x</think>\udfff<thi'],
  ['separated-thoughts', 'a<think>x</think>b<think>y</think>c'],
  ['many-closes', '</think></think><think>x</think></think>z'],
  ['lookahead', '<think>𐀀</think><thinK>\u0130</thinK>'],
]

for (const [label, source] of sources) {
  add(`${label}/whole`, [source])
  add(`${label}/units`, ['', ...source.split(''), ''])
  for (let split = 0; split <= source.length; split++) {
    add(`${label}/split-${split}`, [source.slice(0, split), '', source.slice(split)])
  }
}

// Every two-boundary partition of the fundamental tag pair, including empty
// chunks, exercises opening/closing fragments across three separate pushes.
const pair = '<think>r</think>'
for (let first = 0; first <= pair.length; first++) {
  for (let second = first; second <= pair.length; second++) {
    add(`three-way/${first}-${second}`, [pair.slice(0, first), pair.slice(first, second), pair.slice(second)])
  }
}

// Reproducible token combinations broaden nesting and ambiguous-boundary paths
// without fixtures copied from the Go implementation or nondeterministic seeds.
const tokens = ['', '<', '</', '<thi', '<think>', '</think>', '<THINK>', '</THINK>', '<thinK>', '</thinK>', '<thİnk>', '>', 'x', ' \t\n', '😀', '\ud800', '\udfff']
let state = 0x31a2b4c5
const next = () => { state = (Math.imul(state, 1664525) + 1013904223) >>> 0; return state }
for (let sample = 0; sample < 128; sample++) {
  const chunks = Array.from({ length: 24 }, () => tokens[next() % tokens.length])
  add(`synthetic/${sample}`, chunks)
}

mkdirSync(path.dirname(target), { recursive: true, mode: 0o700 })
writeFileSync(target, JSON.stringify({ formatVersion: 1, encoding: 'utf16-code-units', sourceHasFinish: false, cases }) + '\n', { flag: 'wx', mode: 0o600 })
console.log(JSON.stringify({ formatVersion: 1, caseCount: cases.length, sourceHasFinish: false }))
