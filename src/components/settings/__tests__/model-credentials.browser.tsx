import { act, useState } from 'react'
import { createRoot, type Root } from 'react-dom/client'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { page } from 'vitest/browser'
import type { ModelDiscoveryRequest, ModelProfile } from '../../../shared/ipc-channels'
import { BUILTIN_PRESETS } from '../../../shared/provider-presets'
import { useLLMStore } from '../../../stores/llm-store'
import { useLocaleStore } from '../../../stores/locale-store'
import { ModelForm } from '../ModelForm'

;(globalThis as typeof globalThis & { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true

const originalLLM = useLLMStore.getState()
const originalLocale = useLocaleStore.getState()
const profile: ModelProfile = {
  id: 'server-credential', name: 'Server model', provider: 'custom', protocol: 'openai',
  baseUrl: 'https://saved-provider.invalid/v1', modelName: 'saved-model', apiKey: '', hasCredential: true,
  maxTokens: 4096, temperature: 0.7, purposes: ['generation'],
}
let root: Root | undefined
let container: HTMLDivElement | undefined

async function renderForm() {
  const discoverModels = vi.fn(async (_request: ModelDiscoveryRequest) => ({ success: true as const, models: [] }))
  const testConnection = vi.fn(async (_model: ModelProfile) => ({ success: true }))
  const save = vi.fn()
  useLocaleStore.setState({ locale: 'zh-CN' })
  useLLMStore.setState({ discoverModels, testConnection })
  function Form() {
    const [draft, setDraft] = useState(profile)
    return <ModelForm model={draft} onChange={setDraft} onSave={() => save(draft)} onCancel={() => {}}
      saving={false} purposeOptions={['generation']} presets={BUILTIN_PRESETS} />
  }
  container = document.createElement('div')
  document.body.append(container)
  root = createRoot(container)
  await act(async () => root?.render(<Form />))
  return { discoverModels, testConnection, save }
}

async function setInputValue(input: HTMLInputElement | HTMLSelectElement, value: string) {
  await act(async () => {
    const prototype = input instanceof HTMLSelectElement ? HTMLSelectElement.prototype : HTMLInputElement.prototype
    Object.getOwnPropertyDescriptor(prototype, 'value')?.set?.call(input, value)
    input.dispatchEvent(new Event(input instanceof HTMLSelectElement ? 'change' : 'input', { bubbles: true }))
  })
}

const keyInput = () => container!.querySelector<HTMLInputElement>('input[type="password"]')!
const baseUrlInput = () => Array.from(container!.querySelectorAll('input')).find(input => input.value.startsWith('https://'))!

afterEach(async () => {
  await act(async () => root?.unmount())
  container?.remove()
  root = undefined
  container = undefined
  useLLMStore.setState(originalLLM, true)
  useLocaleStore.setState(originalLocale, true)
  vi.restoreAllMocks()
})

describe('server-owned model credentials', () => {
  it('keeps a blank credential, discovers by saved identity and tests/saves without retrieving a key', async () => {
    const { discoverModels, testConnection, save } = await renderForm()
    expect(keyInput().value).toBe('')
    expect(keyInput().placeholder).toBe('已配置，留空保留')
    await expect.element(page.getByRole('button', { name: '保存配置', exact: true })).toBeEnabled()
    await act(async () => page.getByRole('button', { name: '获取模型列表', exact: true }).click())
    expect(discoverModels).toHaveBeenCalledWith({
      provider: profile.provider, protocol: profile.protocol, baseUrl: profile.baseUrl,
      apiKey: '', purposes: profile.purposes, savedModelId: profile.id,
    })
    await act(async () => page.getByRole('button', { name: '测试连接', exact: true }).click())
    expect(testConnection).toHaveBeenCalledWith(profile)
    await act(async () => page.getByRole('button', { name: '保存配置', exact: true }).click())
    expect(save).toHaveBeenCalledWith(profile)
    expect(keyInput().value).toBe('')
  })

  it.each(['endpoint', 'protocol', 'provider'] as const)('does not carry credential metadata to a new %s', async (field) => {
    const { discoverModels, testConnection } = await renderForm()
    if (field === 'endpoint') await setInputValue(baseUrlInput(), 'https://different-provider.invalid/v1')
    else {
      const select = Array.from(container!.querySelectorAll('select')).find(candidate => candidate.value === (field === 'protocol' ? 'openai' : 'custom'))!
      await setInputValue(select, field === 'protocol' ? 'gemini' : 'deepseek')
    }
    expect(keyInput().placeholder).not.toBe('已配置，留空保留')
    await expect.element(page.getByRole('button', { name: '测试连接', exact: true })).toBeDisabled()
    await expect.element(page.getByRole('button', { name: '保存配置', exact: true })).toBeDisabled()
    await act(async () => page.getByRole('button', { name: '获取模型列表', exact: true }).click())
    expect(discoverModels).not.toHaveBeenCalled()
    expect(testConnection).not.toHaveBeenCalled()
  })

  it('uses a replacement key explicitly and omits saved identity for discovery', async () => {
    const { discoverModels } = await renderForm()
    await setInputValue(keyInput(), 'replacement-fixture-key')
    await act(async () => page.getByRole('button', { name: '获取模型列表', exact: true }).click())
    expect(discoverModels).toHaveBeenCalledWith({ provider: profile.provider, protocol: profile.protocol,
      baseUrl: profile.baseUrl, apiKey: 'replacement-fixture-key', purposes: profile.purposes })
    expect(discoverModels.mock.calls[0][0]).not.toHaveProperty('savedModelId')
  })

  it('recovers the test button after a transport error without echoing sensitive details', async () => {
    const { testConnection } = await renderForm()
    testConnection.mockRejectedValueOnce(new Error('sensitive-transport-detail'))
    await act(async () => page.getByRole('button', { name: '测试连接', exact: true }).click())
    await expect.element(page.getByRole('button', { name: '测试连接', exact: true })).toBeEnabled()
    expect(container?.textContent).toContain('连接测试请求失败，请重试。')
    expect(container?.textContent).not.toContain('sensitive-transport-detail')
  })
})
