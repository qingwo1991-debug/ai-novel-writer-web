import assert from 'node:assert/strict'
import { createHash } from 'node:crypto'
import { existsSync, mkdirSync, readFileSync, realpathSync, writeFileSync } from 'node:fs'
import { registerHooks, stripTypeScriptTypes } from 'node:module'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

// GitHub-only real recursive TS differential oracle. No application runtime
// bridge, env/config/settings/author reads, generated fake catalog or stubs.
assert.equal(process.env.GITHUB_ACTIONS, 'true', 'generation-owner oracle is GitHub-only')
const temp = process.env.RUNNER_TEMP
const target = process.env.GO_GENERATION_OWNER_CONTRACT
assert(temp && target && path.isAbsolute(temp) && path.isAbsolute(target))
assert.equal(target, path.join(temp, 'novel-go-generation-owner', 'contract.json'))
assert(process.argv[2] === undefined || process.argv[2] === 'generate')
assert(!existsSync(target), 'never overwrite a contract')
const within = (base, candidate) => {
  const rel = path.relative(base, candidate)
  return rel && rel !== '..' && !rel.startsWith(`..${path.sep}`) && !path.isAbsolute(rel)
}
mkdirSync(path.dirname(target), { recursive: true, mode: 0o700 })
assert(within(realpathSync(temp), path.join(realpathSync(path.dirname(target)), path.basename(target))))
const root = new URL('../../', import.meta.url)
const allowed = [
  'electron/services/model-execution-lease.ts',
  'electron/services/main-generation-plan.ts',
  'electron/llm/generation-parameter-policy.ts',
  'src/shared/provider-presets.ts',
  'src/shared/reasoning-types.ts',
  'src/shared/reasoning-policy.ts',
  'src/shared/plot-outline-contract.ts',
  'src/services/generation/task-budget-planner.ts',
  'src/services/workflows/blueprint-batch-policy.ts',
]
const sources = new Map(allowed.map(name => {
  const url = new URL(name, root).href
  return [url, { name, source: readFileSync(fileURLToPath(url), 'utf8') }]
}))
const used = new Set()
const hooks = registerHooks({
  resolve(specifier, context, next) {
    if (specifier === 'node:crypto' || specifier === 'node:util') return next(specifier, context)
    if (sources.has(specifier)) return { url: specifier, shortCircuit: true }
    assert(specifier.startsWith('.') && context.parentURL, `unexpected dependency ${specifier}`)
    const url = new URL(specifier, context.parentURL).href
    const resolved = sources.has(url) ? url : `${url}.ts`
    assert(sources.has(resolved), `unreviewed transitive dependency ${specifier}`)
    return { url: resolved, shortCircuit: true }
  },
  load(url, context, next) {
    if (url.startsWith('node:')) return next(url, context)
    assert(sources.has(url), `unreviewed module ${url}`)
    used.add(url)
    return { format: 'module', source: stripTypeScriptTypes(sources.get(url).source, { mode: 'transform', sourceUrl: url }), shortCircuit: true }
  },
})
const previousFetch = globalThis.fetch
globalThis.fetch = () => { throw new Error('network forbidden in generation-owner oracle') }
try {
  const leases = await import(new URL('electron/services/model-execution-lease.ts', root).href)
  const plans = await import(new URL('electron/services/main-generation-plan.ts', root).href)
  const base = { id: 'public-synthetic-model', name: 'PUBLIC synthetic fixture', provider: 'custom', protocol: 'openai', baseUrl: 'https://example.invalid/v1', modelName: 'synthetic-nonreasoning', apiKey: 'PUBLIC-not-a-secret', temperature: 0.8, maxTokens: 8192, purposes: ['generation'] }
  const messages = [{ role: 'system', content: 'PUBLIC synthetic <>&\u2028\u2029' }, { role: 'user', content: '合成 😀\nno author content' }]
  const tasks = [
    { purpose: 'public-test', output: 'visible-text', messages },
    { purpose: 'structured-test', output: 'structured-data', messages, budgetDemand: { kind: 'structured-items', writingLanguage: 'zh-CN', requestedItems: 1 } },
    { purpose: 'split-test', output: 'structured-data', reasoningStage: 'review', messages, budgetDemand: { kind: 'structured-items', writingLanguage: 'en-US', requestedItems: 100 } },
    { purpose: 'draft-test', output: 'visible-text', messages, budgetDemand: { kind: 'draft-units', writingLanguage: 'zh-CN', requestedUnits: 2400, segmentable: false } },
  ]
  const profiles = [
    base,
    { ...base, protocol: 'gemini', baseUrl: 'https://gemini.example.invalid', capabilities: { contextWindowTokens: null, maxOutputTokens: 4096, reasoning: true, structuredOutput: true, usage: true } },
    { ...base, capabilities: { contextWindowTokens: 2048, maxOutputTokens: 7000, reasoning: true, structuredOutput: true, usage: true } },
    { ...base, provider: 'openai', baseUrl: 'https://api.openai.com/v1', modelName: 'gpt-4.1', maxTokens: 100000 },
    { ...base, provider: 'display-label-not-proof', baseUrl: 'https://api.openai.com', modelName: 'gpt-4.1-2025-04-14', capabilities: { contextWindowTokens: null, maxOutputTokens: 4096, reasoning: false, structuredOutput: true, usage: true } },
    { ...base, baseUrl: 'HTTPS://EXAMPLE.INVALID:443/v1///', reasoningOverride: 'high', embeddingOptions: { dimensions: 1024 } },
  ]
  const cases = []
  for (const [index, model] of profiles.entries()) {
    const receipt = leases.createModelExecutionLeaseReceipt(model, { leaseId: `public-${index}`, createdAt: 1000, expiresAt: 2000 })
    for (const [taskIndex, task] of tasks.entries()) {
      const rootOutput = taskIndex === 2 ? 1024 : 65536
      let expected
      try { expected = { plan: plans.buildMainGenerationPlan(model, receipt, task, { policy: { maxOutputPerRequest: rootOutput } }) } }
      catch (error) { assert(error instanceof plans.TaskBudgetPreflightError); expected = { error: 'TASK_BUDGET_SCOPE_SPLIT_REQUIRED', decision: error.decision } }
      cases.push({ name: `profile-${index}-task-${taskIndex}`, model, receipt, task, rootOutput, ...expected })
    }
  }
  // Exercise the REAL original immutable registry and expiry semantics, not a
  // hard-coded reproduction of expected state transitions.
  let current = structuredClone(base), now = 100
  const registry = new leases.ModelExecutionLeaseRegistry({ loadModel: () => current, now: () => now, ttlMs: 10, createLeaseId: () => 'public-registry' })
  const started = registry.begin(base.id)
  current.maxTokens = 1
  const frozenMaxTokens = registry.resolve(started.leaseId).maxTokens
  const resolved = registry.resolve(started.leaseId); resolved.maxTokens = 2
  assert.equal(registry.resolve(started.leaseId).maxTokens, frozenMaxTokens)
  now = 110
  let expiry
  try { registry.resolve(started.leaseId) } catch (error) { expiry = error.code }
  assert.equal(expiry, 'MODEL_EXECUTION_LEASE_EXPIRED')
  const invalidTasks = [
    { ...tasks[0], baseUrl: 'https://forbidden.invalid' },
    { ...tasks[0], purpose: '../escape' },
    { ...tasks[0], messages: [] },
    { ...tasks[0], budgetDemand: { kind: 'structured-items', writingLanguage: 'en-US', requestedItems: 1, apiKey: 'forbidden' } },
    { ...tasks[0], reasoningStage: 'provider-thinking' },
  ].map(task => { assert.throws(() => plans.assertSemanticGenerationTask(task), /GENERATION_SEMANTIC_TASK_INVALID/); return task })
  // Every recursive source is hashed and included; no shadow import or stubs.
  assert.equal(used.size, allowed.length)
  const sourceEvidence = [...used].sort().map(url => ({ path: sources.get(url).name, sha256: createHash('sha256').update(sources.get(url).source).digest('hex') }))
  writeFileSync(target, JSON.stringify({ version: 'native-generation-owner-foundation-v1', sourceEvidence, cases, invalidTasks, registry: { frozenMaxTokens, expiry }, limitations: ['native registry additionally binds caller/session/tab/project/epoch/purpose and invalidates revision/credential changes', 'finite admitted model subset only; no full durable owner, workflow, composition or provider qualification'] }) + '\n', { flag: 'wx', mode: 0o600 })
  console.log(`generation-owner contract: ${cases.length} real TS plan/lease cases; ${used.size} recursive original modules`)
} finally { globalThis.fetch = previousFetch; hooks.deregister() }
