export const AVATAR_MAX_EDGE = 256
export const AVATAR_JPEG_QUALITY = 82
export type AvatarImageExtension = 'png' | 'jpg' | 'jpeg' | 'webp'

export interface CompressedAvatar {
  bytes: Buffer
  extension: AvatarImageExtension
}

export function detectAvatarExtension(bytes: Uint8Array): AvatarImageExtension | null {
  if (bytes.length >= 8 && bytes[0] === 0x89 && bytes[1] === 0x50 && bytes[2] === 0x4e
    && bytes[3] === 0x47 && bytes[4] === 0x0d && bytes[5] === 0x0a && bytes[6] === 0x1a && bytes[7] === 0x0a) return 'png'
  if (bytes.length >= 3 && bytes[0] === 0xff && bytes[1] === 0xd8 && bytes[2] === 0xff) return 'jpg'
  if (bytes.length >= 12 && Buffer.from(bytes).toString('ascii', 0, 4) === 'RIFF'
    && Buffer.from(bytes).toString('ascii', 8, 12) === 'WEBP') return 'webp'
  return null
}

export function avatarMime(extension: AvatarImageExtension): 'image/png' | 'image/jpeg' | 'image/webp' {
  if (extension === 'png') return 'image/png'
  if (extension === 'webp') return 'image/webp'
  return 'image/jpeg'
}

export type AvatarCompressor = (bytes: Buffer, extension: AvatarImageExtension) => CompressedAvatar
let compressor: AvatarCompressor | undefined

/** Optional optimization. Native hosts may inject an image codec; Node preserves original bytes. */
export function configureAvatarCompressor(adapter: AvatarCompressor | undefined): void { compressor = adapter }

export function compressAvatarImage(bytes: Buffer, extension: AvatarImageExtension): CompressedAvatar {
  const unchanged = (): CompressedAvatar => ({ bytes, extension })
  if (!compressor) return unchanged()
  try {
    const candidate = compressor(bytes, extension)
    return Buffer.isBuffer(candidate.bytes) && candidate.bytes.byteLength > 0
      && candidate.bytes.byteLength < bytes.byteLength && detectAvatarExtension(candidate.bytes) === candidate.extension
      ? candidate : unchanged()
  } catch { return unchanged() }
}
