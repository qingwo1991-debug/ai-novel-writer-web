import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { createHash, randomUUID } from 'node:crypto'
import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest'

type IpcHandler = (event: unknown, ...args: unknown[]) => Promise<unknown>
const handlers = new Map<string, IpcHandler>()

vi.mock('../../platform', () => ({
  appHost: { isPackaged: false, getLocale: () => 'en-US' },
  operations: {
    handle: vi.fn((channel: string, handler: IpcHandler) => handlers.set(channel, handler)),
  },
}))

import { closeProjectDatabase } from '../../database'
import { openCanonicalProjectFixture as initProjectDatabase } from '../../../test/helpers/canonical-project-fixture'
import { DraftRepository } from '../../repositories/draft-repository'
import { FinalizationRepository } from '../../repositories/finalization-repository'
import { projectAccess } from '../../services/project-access'
import { registerDatabaseController } from '../db-controller'

let parentDirectory = ''
let session = { projectId: '', leaseId: '', projectPath: '' }

beforeAll(() => registerDatabaseController())

beforeEach(() => {
  parentDirectory = fs.mkdtempSync(path.join(os.tmpdir(), 'ai-novel-db-read-only-'))
  const lease = projectAccess.beginSession(projectAccess.createProject(parentDirectory, 'novel'))
  session = { projectId: lease.projectId, leaseId: lease.leaseId, projectPath: lease.rootPath }
  initProjectDatabase(lease.rootPath)
})

afterEach(() => {
  closeProjectDatabase()
  projectAccess.invalidateCurrentSession()
  fs.rmSync(parentDirectory, { recursive: true, force: true })
})

function finalizedDraft(content: string): number {
  const draftId = DraftRepository.create({ chapterNumber: 1, source: 'write', content, wordCount: content.length })
  FinalizationRepository.commit({
    finalizationId: randomUUID(),
    draftId,
    chapterNumber: 1,
    chapterTitle: '雾钟',
    content,
    expectedContent: content,
    contentHash: createHash('sha256').update(content, 'utf8').digest('hex'),
    contentRevision: 1,
    targetFileName: `第1章 雾钟 ${draftId}.txt`,
  })
  return draftId
}

function updateContent(draftId: number, content: string, expectedContent = DraftRepository.getFull(draftId)?.content) {
  return handlers.get('db:draft-update-content')!({}, draftId, content, content.length, session.projectPath, expectedContent, session)
}

describe('db:draft-update-content 对带定稿收据的稿', () => {
  it('定稿与被替换的旧定稿都返回只读拒绝，正文不变', async () => {
    const replaced = finalizedDraft('第一章旧定稿。')
    const current = finalizedDraft('第一章新定稿。')

    for (const draftId of [current, replaced]) {
      await expect(updateContent(draftId, '覆盖')).resolves.toMatchObject({
        success: false,
        errorCode: 'DRAFT_CONTENT_CONFLICT',
        error: expect.stringContaining('只读'),
      })
    }
    expect([DraftRepository.getFull(replaced)?.content, DraftRepository.getFull(current)?.content])
      .toEqual(['第一章旧定稿。', '第一章新定稿。'])
  })

  it('工作稿使用已读取正文作为基线照常保存', async () => {
    const draftId = DraftRepository.create({ chapterNumber: 2, source: 'write', content: '初稿', wordCount: 2 })

    await expect(updateContent(draftId, '改稿', '初稿')).resolves.toEqual({ success: true })
    expect(DraftRepository.getFull(draftId)?.content).toBe('改稿')
  })

  it('相同旧基线的并发保存只有一个成功，不覆盖首次提交', async () => {
    const original = '共同读取的初稿'
    const draftId = DraftRepository.create({ chapterNumber: 2, source: 'write', content: original, wordCount: original.length })
    const [first, stale] = await Promise.all([
      updateContent(draftId, '设备甲的人工编辑', original),
      updateContent(draftId, '设备乙的旧标签编辑', original),
    ])

    expect(first).toEqual({ success: true })
    expect(stale).toMatchObject({ success: false, errorCode: 'DRAFT_CONTENT_CONFLICT' })
    expect(DraftRepository.getFull(draftId)).toMatchObject({ content: '设备甲的人工编辑', wordCount: '设备甲的人工编辑'.length })
    expect(JSON.stringify(stale)).not.toContain('设备甲的人工编辑')
    expect(JSON.stringify(stale)).not.toContain('设备乙的旧标签编辑')
  })

  it('没有正文基线的旧调用明确拒绝，不降级为盲写', async () => {
    const draftId = DraftRepository.create({ chapterNumber: 2, source: 'write', content: '保留原稿', wordCount: 4 })
    const result = await handlers.get('db:draft-update-content')!({}, draftId, '没有基线的覆盖', 7, session.projectPath, session)

    expect(result).toMatchObject({ success: false, errorCode: 'DRAFT_CONTENT_CONFLICT' })
    expect(DraftRepository.getFull(draftId)).toMatchObject({ content: '保留原稿', wordCount: 4 })
  })

  it('正确处理空正文基线，并拒绝把规范化或trim结果当作同一正文', async () => {
    const draftId = DraftRepository.create({ chapterNumber: 2, source: 'write', content: '', wordCount: 0 })
    await expect(updateContent(draftId, 'e\u0301\r\n ', '')).resolves.toEqual({ success: true })
    await expect(updateContent(draftId, '覆盖', 'é\n')).resolves.toMatchObject({ success: false, errorCode: 'DRAFT_CONTENT_CONFLICT' })
    expect(DraftRepository.getFull(draftId)?.content).toBe('e\u0301\r\n ')
  })

  it('旧编辑在定稿后即使正文基线相同也不能保存', async () => {
    const original = '读取后被定稿的正文'
    const draftId = finalizedDraft(original)
    await expect(updateContent(draftId, '迟到编辑', original)).resolves.toMatchObject({ success: false, errorCode: 'DRAFT_CONTENT_CONFLICT' })
    expect(DraftRepository.getFull(draftId)?.content).toBe(original)
  })
})
