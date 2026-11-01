import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { createHash } from 'node:crypto'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { openCanonicalProjectFixture } from '../../test/helpers/canonical-project-fixture'
import { closeProjectDatabase, getProjectDb, initProjectDatabase } from '../database'
import { importFinalizedText, importText, readDocumentCopy, saveDocumentCopy } from '../knowledge-base'
import { closeConnection, getConnection, getDocumentIntegrity, listDocuments, readKnowledgeCopy, writeKnowledgeCopy } from '../vector-store'
import { FinalizationRepository } from '../repositories/finalization-repository'
import { getProjectDataRoot } from '../services/project-data-locator'
import type { FinalizedSourceIdentity } from '../../src/shared/finalized-continuity'

const { generateEmbeddings } = vi.hoisted(() => ({ generateEmbeddings: vi.fn() }))
vi.mock('../embedding', () => ({ chunkText: (text: string) => [text], generateEmbeddings }))
const hash = (text: string) => createHash('sha256').update(text).digest('hex')
const model = { provider: 'openai', baseUrl: 'https://synthetic.invalid/v1', apiKey: 'synthetic', modelName: 'receipt-test' }
let root = ''
function finalize(content = '门闩牢固，守塔人拿起灯。', version = 1): FinalizedSourceIdentity {
  const db = getProjectDb()!
  const contentId = Number(db.prepare('INSERT INTO contents(body) VALUES(?)').run(content).lastInsertRowid)
  const draftId = Number(db.prepare("INSERT INTO drafts(chapter_number,version,status,content_id,word_count) VALUES(1,?,'draft',?,?)")
    .run(version, contentId, content.length).lastInsertRowid)
  const finalizationId = `finalization-${version}`
  FinalizationRepository.commit({ draftId, finalizationId, chapterNumber: 1, chapterTitle: '灯塔', content,
    contentHash: hash(content), contentRevision: version, targetFileName: `chapter-1-v${version}.txt` })
  return { draftId, finalizationId, chapterNumber: 1, contentHash: hash(content) }
}
const imported = (source: FinalizedSourceIdentity, text = '门闩牢固，守塔人拿起灯。') =>
  importFinalizedText(text, '第1章 灯塔.txt', source, root, 'openai', model)
beforeEach(() => {
  root = fs.mkdtempSync(path.join(os.tmpdir(), 'novel-finalization-kb-receipt-'))
  openCanonicalProjectFixture(root)
  generateEmbeddings.mockReset(); generateEmbeddings.mockResolvedValue([[0.2, 0.7]])
})
afterEach(() => {
  vi.restoreAllMocks()
  closeProjectDatabase(); closeConnection(root)
  fs.rmSync(root, { recursive: true, force: true })
})

describe('durable finalization knowledge identity', () => {
  it('reuses completed import after lost link/mark ACK and process-style reopen without another embedding', async () => {
    const source = finalize()
    const link = vi.spyOn(FinalizationRepository, 'linkKnowledgeDocument').mockImplementationOnce(() => { throw new Error('synthetic lost link ACK') })
    expect(await imported(source)).toMatchObject({ success: false, error: 'synthetic lost link ACK' })
    expect(await listDocuments(root)).toHaveLength(1)
    expect(generateEmbeddings).toHaveBeenCalledOnce()
    closeProjectDatabase(); closeConnection(root); initProjectDatabase(root)
    const retry = await imported(source)
    expect(retry).toMatchObject({ success: true, idempotent: true, chunkCount: 1 })
    expect(await imported(source)).toMatchObject({ success: true, idempotent: true, docId: retry.docId })
    expect(await listDocuments(root)).toHaveLength(1)
    expect(generateEmbeddings).toHaveBeenCalledOnce()
    expect(FinalizationRepository.getByDraftId(source.draftId)?.knowledgeDocumentId).toBe(retry.docId)
    expect(link).toHaveBeenCalledTimes(3)
  })
  it('supports unconfigured embedding FTS-only finalization and retries without a provider call', async () => {
    const source = finalize(), ftsModel = { ...model, apiKey: '' }
    const first = await importFinalizedText('门闩牢固，守塔人拿起灯。', '第1章 灯塔.txt', source, root, 'openai', ftsModel)
    const retry = await importFinalizedText('门闩牢固，守塔人拿起灯。', '第1章 灯塔.txt', source, root, 'openai', ftsModel)
    expect(first).toMatchObject({ success: true, idempotent: false })
    expect(retry).toMatchObject({ success: true, idempotent: true, docId: first.docId })
    expect(generateEmbeddings).not.toHaveBeenCalled()
    expect(FinalizationRepository.getByDraftId(source.draftId)?.knowledgeDocumentId).toBe(first.docId)
  })
  it('serializes concurrent exact retries into one durable import and one provider request', async () => {
    const source = finalize()
    const results = await Promise.all([imported(source), imported(source)])
    expect(results.map(result => result.success)).toEqual([true, true])
    expect(results[0].docId).toBe(results[1].docId)
    expect(results[1].idempotent).toBe(true)
    expect(generateEmbeddings).toHaveBeenCalledOnce()
    expect(await listDocuments(root)).toHaveLength(1)
  })
  it('binds new finalized source distinctly, replaces stale filename text and leaves ordinary manual imports unchanged', async () => {
    const firstSource = finalize(), first = await imported(firstSource)
    const content = '门闩重新断裂，守塔人放下灯。', nextSource = finalize(content, 2)
    const second = await imported(nextSource, content)
    expect(second.success).toBe(true); expect(second.docId).not.toBe(first.docId)
    expect(await listDocuments(root)).toEqual([expect.objectContaining({ id: second.docId })])
    const deniedOld = await imported(firstSource)
    expect(deniedOld).toMatchObject({ success: false, error: 'FINALIZATION_KNOWLEDGE_SOURCE_CHANGED' })
    const beforeCalls = generateEmbeddings.mock.calls.length
    const manual1 = await importText('手工资料甲', 'manual.txt', root, 'openai', { ...model, apiKey: '' })
    const manual2 = await importText('手工资料乙', 'manual.txt', root, 'openai', { ...model, apiKey: '' })
    expect(manual2.docId).not.toBe(manual1.docId)
    expect(await listDocuments(root)).toHaveLength(2)
    expect(generateEmbeddings).toHaveBeenCalledTimes(beforeCalls)
  })
  it('refuses caller text/hash/identity changes before any vector writes or provider call', async () => {
    const source = finalize()
    for (const altered of [{ ...source, contentHash: hash('forged') }, { ...source, finalizationId: 'forged' }, { ...source, chapterNumber: 2 }]) {
      expect(await imported(altered)).toMatchObject({ success: false, error: 'FINALIZATION_KNOWLEDGE_SOURCE_CHANGED' })
    }
    expect(await imported(source, 'forged manuscript')).toMatchObject({ success: false, error: 'FINALIZATION_KNOWLEDGE_SOURCE_CHANGED' })
    expect(generateEmbeddings).not.toHaveBeenCalled()
    expect(await listDocuments(root)).toEqual([])
  })
  it('rechecks finalized authority after an asynchronous provider result before writing the index', async () => {
    const source = finalize()
    generateEmbeddings.mockImplementationOnce(async () => {
      getProjectDb()!.prepare("UPDATE drafts SET status='revised' WHERE id=?").run(source.draftId)
      return [[0.2, 0.7]]
    })
    expect(await imported(source)).toMatchObject({ success: false, error: 'FINALIZATION_KNOWLEDGE_SOURCE_CHANGED' })
    expect(await listDocuments(root)).toEqual([])
    expect(FinalizationRepository.getByDraftId(source.draftId)?.knowledgeDocumentId).toBe('')
    expect(generateEmbeddings).toHaveBeenCalledOnce()
  })
  it('never overwrites author-edited project copies or silently repairs them with a paid request', async () => {
    const source = finalize(), first = await imported(source)
    const copy = await readDocumentCopy(first.docId!, root)
    await saveDocumentCopy(first.docId!, '作者后来手工修订的资料', copy.contentHash!, root)
    expect(await imported(source)).toMatchObject({ success: false, error: 'FINALIZATION_KNOWLEDGE_REPAIR_REQUIRED' })
    expect((await readDocumentCopy(first.docId!, root)).content).toBe('作者后来手工修订的资料')
    expect(generateEmbeddings).toHaveBeenCalledOnce()
  })
  it('detects damaged active vector evidence rather than skipping or repeating the import', async () => {
    const source = finalize(), first = await imported(source)
    const integrity = await getDocumentIntegrity(root, first.docId!)
    const active = integrity!.embeddingGenerations.find(generation => generation.status === 'active')!
    const connection = await getConnection(root), table = await connection.openTable(active.tableName)
    await table.delete(`\`docId\` = '${first.docId}'`)
    expect((await getDocumentIntegrity(root, first.docId!))?.complete).toBe(false)
    expect(await imported(source)).toMatchObject({ success: false, error: 'FINALIZATION_KNOWLEDGE_REPAIR_REQUIRED' })
    expect(generateEmbeddings).toHaveBeenCalledOnce()
  })
  it('refuses a linked receipt whose entire index disappeared instead of buying embeddings again', async () => {
    const source = finalize(), first = await imported(source), connection = await getConnection(root)
    for (const name of await connection.tableNames()) {
      const table = await connection.openTable(name)
      await table.delete(name === 'documents' ? `id = '${first.docId}'` : `\`docId\` = '${first.docId}'`)
    }
    expect(await getDocumentIntegrity(root, first.docId!)).toBeNull()
    expect(await imported(source)).toMatchObject({ success: false, error: 'FINALIZATION_KNOWLEDGE_REPAIR_REQUIRED' })
    expect(generateEmbeddings).toHaveBeenCalledOnce()
  })
  it('detects stale copy receipts even if the canonical index remains intact', async () => {
    const source = finalize(), first = await imported(source)
    const storage = getProjectDataRoot(root), copy = readKnowledgeCopy(storage, first.docId!)!
    writeKnowledgeCopy(storage, first.docId!, { ...copy, indexDirty: true })
    expect(await imported(source)).toMatchObject({ success: false, error: 'FINALIZATION_KNOWLEDGE_REPAIR_REQUIRED' })
    expect(generateEmbeddings).toHaveBeenCalledOnce()
  })
})
