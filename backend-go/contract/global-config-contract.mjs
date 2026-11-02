// GitHub-only actual-original oracle. Business modules are read verbatim and
// type-stripped, never patched or replaced by business clones. The sole import
// seam supplies appHost.getLocale and operations.handle. Actual app-data-locator
// is installed with a fresh synthetic root; no author/default volume is opened.
import assert from 'node:assert/strict'
import { builtinModules, createRequire, registerHooks } from 'node:module'
import { existsSync, mkdirSync, readFileSync, readdirSync, unlinkSync, writeFileSync } from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

assert.equal(process.env.GITHUB_ACTIONS, 'true', 'global config oracle is GitHub-only')
const base = process.env.RUNNER_TEMP, target = process.env.GO_GLOBAL_CONFIG_CONTRACT
assert(base && target && path.isAbsolute(base) && path.isAbsolute(target))
assert.equal(target, path.join(base, 'novel-go-global-config', 'contract.json'))
const directory = path.dirname(target)
assert(!existsSync(directory), 'never reuse an oracle directory')
mkdirSync(directory, { recursive: true, mode: 0o700 })
const dataRoot = path.join(directory, 'synthetic-data')
mkdirSync(dataRoot, { mode: 0o700 })
// Actual locator's legacy guards must be synthetic too, even though its legacy
// paths are never read. No production VELA_HOME/config/model/key is inspected.
process.env.AI_NOVEL_LEGACY_SOURCE_HOME = path.join(directory, 'synthetic-legacy-never-read')
process.env.AI_NOVEL_VELA_HOME = process.env.AI_NOVEL_LEGACY_SOURCE_HOME
process.env.AI_NOVEL_APP_DATA_HOME = dataRoot
const root = new URL('../../', import.meta.url)
const platformURL = new URL('electron/platform', root).href
const seam = 'global-config-contract:platform', key = '__GO_GLOBAL_CONFIG_ORACLE__'
assert(!Object.hasOwn(globalThis, key))
const state = { locale: 'en-US', handlers: new Map() }
globalThis[key] = state
// Load existing CI tooling before restricting original-module dependencies.
// createRequire honors the runner's explicit NODE_PATH tool prefix.
const { transformSync } = createRequire(import.meta.url)('esbuild')
transformSync('const toolingPrewarm: number = 1', { loader: 'ts' })
const originals = new Set(), builtins = new Set(builtinModules.flatMap(name => [name, `node:${name}`]))
const hooks = registerHooks({
  resolve(specifier, context, next) {
    if (builtins.has(specifier)) return next(specifier, context)
    assert(specifier.startsWith('.') || specifier.startsWith('file:'), `unexpected dependency ${specifier}`)
    const url = new URL(specifier, context.parentURL ?? root).href
    if (url === platformURL || url === platformURL + '.ts') return { url: seam, shortCircuit: true }
    const candidate = url.endsWith('.ts') ? url : url + '.ts'
    const filename = fileURLToPath(candidate), relative = path.relative(fileURLToPath(root), filename)
    assert(relative && !relative.startsWith('..') && !path.isAbsolute(relative))
    assert(/^(electron|src\/(shared|i18n|utils))\//u.test(relative), relative)
    assert(existsSync(filename), `missing unmodified original ${relative}`)
    originals.add(relative)
    return { url: candidate, shortCircuit: true }
  },
  load(url, context, next) {
    if (url === seam) return { format: 'module', source: `const s=globalThis[${JSON.stringify(key)}]; export const appHost={getLocale:()=>s.locale}; export const operations={handle:(name,fn)=>s.handlers.set(name,fn)}`, shortCircuit: true }
    if (!url.startsWith(root.href) || !url.endsWith('.ts')) return next(url, context)
    // Existing esbuild TS erasure removes imports used only as types too (the
    // original uses `import { GlobalConfig }`, not `import type`). No source
    // rewriting: transform sees the complete, unmodified original text.
    return { format: 'module', source: transformSync(readFileSync(new URL(url), 'utf8'), { loader: 'ts', format: 'esm', target: 'esnext', sourcefile: fileURLToPath(url), tsconfigRaw: { compilerOptions: { verbatimModuleSyntax: false } } }).code, shortCircuit: true }
  },
})
const clone = value => JSON.parse(JSON.stringify(value))
try {
  const locator = await import(new URL('electron/services/app-data-locator.ts', root).href)
  locator.installGlobalDataLocator(dataRoot, 'synthetic-fixture-generation', process.env.AI_NOVEL_LEGACY_SOURCE_HOME)
  const utils = await import(new URL('electron/utils/config-utils.ts', root).href)
  const { registerConfigController } = await import(new URL('electron/controllers/config-controller.ts', root).href)
  const { registerStartupController } = await import(new URL('electron/controllers/startup-controller.ts', root).href)
  registerConfigController()
  const configFile = path.join(dataRoot, 'config.json'), configCases = []
  const definitions = [
    ['missing-defaults', null, {}],
    ['missing-stage-injection', null, { stageModelIds: { review: 'MUST-NOT-INSTALL' }, theme: 'light' }],
    ['missing-proxy-shallow-replace', null, { proxy: { enabled: true } }],
    ['existing-no-default-backfill', '{"unknown":7}', { locale: 'en-US' }],
    ['preserve-models-and-secrets-internal-only', JSON.stringify({ models: [{ id: 'public-synthetic', apiKey: 'SYNTHETIC-NOT-A-SECRET' }], arbitrary: { nested: ['保留', null] }, stageModelIds: { review: 'current', planning: null } }), { theme: 'light', stageModelIds: { review: 'stale' } }],
    ['existing-stage-null', '{"stageModelIds":null,"theme":"dark"}', { stageModelIds: 'ignored', theme: 'light' }],
    ['existing-stage-string', '{"stageModelIds":"legacy"}', { stageModelIds: {} }],
    ['nested-replaced-not-merged', '{"proxy":{"enabled":false,"host":"old","port":7890},"keep":true}', { proxy: { host: 'new' } }],
    ['null-values-kept', '{}', { theme: null, extra: null }],
    ['unicode-and-html', '{}', { arbitrary: '中文 café e\u0301 🙂 <&>\u2028\u2029\u0000', outlineTargetUnits: 11 }],
    ['corrupt-json-refused-en', '{BROKEN_SYNTHETIC', { theme: 'light' }],
    ['corrupt-json-refused-zh', '{BROKEN_SYNTHETIC', { theme: 'light' }, 'zh-CN'],
    ['array-refused', '[1,{"x":2}]', { theme: 'light' }],
    ['string-refused', '"synthetic"', {}],
    ['boolean-refused', 'false', {}],
    ['number-refused', '3.5', {}],
    ['literal-null-native-error', 'null', {}],
    ['empty-bytes-refused', '', {}],
    ['trailing-json-refused', '{} {}', {}],
    ['saved-locale-preserved', '{"locale":"zh-CN","models":[],"extra":false}', { theme: 'dark' }],
    ['json-bom-refused', '\uFEFF{}', {}],
  ]
  for (const [name, initialRaw, patch, systemLocale = 'en-US'] of definitions) {
    if (existsSync(configFile)) unlinkSync(configFile)
    if (initialRaw !== null) writeFileSync(configFile, initialRaw, { flag: 'wx', mode: 0o600 })
    state.locale = systemLocale
    const beforeEntries = readdirSync(dataRoot).sort()
    const expectedGet = clone(await state.handlers.get('config:get')({}))
    const expectedResult = clone(await state.handlers.get('config:set')({}, clone(patch)))
    const afterRaw = existsSync(configFile) ? readFileSync(configFile, 'utf8') : null
    const afterEntries = readdirSync(dataRoot).sort()
    // The actual writer's owned temporary is gone on return; no unrelated file
    // changes are hidden by normalizing generated UUIDs or dropping snapshots.
    assert.deepEqual(beforeEntries, initialRaw === null ? [] : ['config.json'])
    assert.deepEqual(afterEntries, ['config.json'])
    if (!expectedResult.success) assert.equal(afterRaw, initialRaw)
    configCases.push({ name, initialRaw, patch, systemLocale, expectedGet, expectedResult, afterRaw, afterValue: expectedResult.success ? JSON.parse(afterRaw) : null, beforeEntries, afterEntries })
  }
  const startupCases = []
  const ack = (revision, generation = 'synthetic-generation') => ({ storageKey: 'ai-novel-writer-appearance-v3', profileRevision: revision, globalGeneration: generation })
  for (const capture of [
    { generation: '', blockedCode: '', notice: null },
    { generation: 'synthetic-generation', blockedCode: '', notice: null },
    { generation: 'synthetic-generation', blockedCode: '', notice: { legacySourceIgnored: true, preservedUnknownCount: 3, forbidden: 'not exposed' } },
    { generation: 'synthetic-generation', blockedCode: 'GLOBAL_DATA_BLOCKED', notice: null },
    { generation: 'synthetic-generation', blockedCode: 'GLOBAL_MODEL_CREDENTIALS_DIFFER', notice: null },
  ]) {
    const handlers = new Map(), senders = new Map(['alice', 'bob'].map((name, i) => [name, { id: i + 1, mainFrame: { url: 'https://synthetic.invalid/app#appearance' }, isDestroyed: () => false }]))
    let selected = senders.get('alice')
    registerStartupController({ ipc: { handle: (name, fn) => handlers.set(name, fn) }, getTrustedSender: () => selected, rendererUrl: 'https://synthetic.invalid/app', getGlobalGeneration: () => capture.generation || null, getBlockedCode: () => capture.blockedCode || undefined, getMigrationNotice: () => capture.notice || undefined })
    const steps = []
    const execute = (caller, trusted, input, trustFailure = '') => {
      selected = senders.get(caller)
      const sender = selected
      const event = { sender, senderFrame: trusted ? sender.mainFrame : { url: sender.mainFrame.url } }
      if (trustFailure === 'different-document') sender.mainFrame.url = 'https://synthetic.invalid/other'
      if (trustFailure === 'different-query') sender.mainFrame.url = 'https://synthetic.invalid/app?evil=1'
      if (trustFailure === 'destroyed') sender.isDestroyed = () => true
      if (trustFailure === 'different-sender') event.sender = { ...sender }
      const expected = clone(input === undefined ? handlers.get('startup:get-state')(event) : handlers.get('startup:appearance-ack')(event, input))
      steps.push({ caller, trusted: trusted && !trustFailure, kind: input === undefined ? 'state' : 'ack', ...(input === undefined ? {} : { input }), expected, trustFailure })
      sender.mainFrame.url = 'https://synthetic.invalid/app#appearance'; sender.isDestroyed = () => false
    }
    execute('alice', true)
    execute('alice', false)
    for (const failure of ['different-document', 'different-query', 'destroyed', 'different-sender']) execute('alice', true, undefined, failure)
    for (const input of [null, [], false, {}, { ...ack(1), extra: true }, { profileRevision: 1, globalGeneration: capture.generation }, { ...ack(1), storageKey: 'wrong' }, ack(0), ack(-1), ack(1.5), ack('1'), ack(9007199254740992), ack(1, null), ack(1, 'wrong-generation')]) execute('alice', true, input)
    execute('alice', false, ack(4))
    execute('alice', true, ack(3)); execute('alice', true)
    execute('bob', true); execute('bob', true, ack(1)); execute('bob', true)
    execute('alice', true, ack(2)); execute('alice', true, ack(3)); execute('alice', true, ack(5)); execute('alice', true)
    execute('alice', true, ack(9007199254740991)); execute('alice', true)
    startupCases.push({ capture, steps })
  }
  for (const required of ['electron/utils/config-utils.ts', 'electron/controllers/config-controller.ts', 'electron/controllers/startup-controller.ts', 'electron/services/app-data-locator.ts', 'electron/i18n.ts', 'src/shared/appearance-profile.ts']) assert(originals.has(required), required)
  writeFileSync(target, JSON.stringify({ formatVersion: 1, authority: 'actual-unmodified-original-modules', originals: [...originals].sort(), defaultConfig: utils.DEFAULT_GLOBAL_CONFIG, configCases, startupCases }) + '\n', { flag: 'wx', mode: 0o600 })
  console.log(`Actual original global config/startup oracle: ${configCases.length} config cases, ${startupCases.reduce((n, item) => n + item.steps.length, 0)} startup results`)
} finally {
  hooks.deregister()
  delete globalThis[key]
}
