/** Fixed external destinations shown in model-provider settings. */
export const MODEL_PROVIDER_RESOURCE_URLS = {
  // No referral/invitation code: a clean, unaffiliated sign-up destination.
  'siliconflow-invite': 'https://cloud.siliconflow.cn',
  'siliconflow-console': 'https://cloud.siliconflow.cn',
  'siliconflow-docs': 'https://docs.siliconflow.cn',
} as const

export type ModelProviderResourceId = keyof typeof MODEL_PROVIDER_RESOURCE_URLS

/** Reject arbitrary renderer-supplied URLs; only fixed resource IDs are accepted. */
export function isModelProviderResourceId(value: unknown): value is ModelProviderResourceId {
  return typeof value === 'string'
    && Object.prototype.hasOwnProperty.call(MODEL_PROVIDER_RESOURCE_URLS, value)
}
