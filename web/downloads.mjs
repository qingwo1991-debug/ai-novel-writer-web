import fs from 'node:fs'
import path from 'node:path'
import { safePath, HttpError } from './security.mjs'

const table = new Uint32Array(256)
for (let n = 0; n < 256; n++) { let c = n; for (let k = 0; k < 8; k++) c = (c & 1) ? 0xedb88320 ^ (c >>> 1) : c >>> 1; table[n] = c >>> 0 }
function crc32(bytes) { let crc = 0xffffffff; for (const byte of bytes) crc = table[(crc ^ byte) & 255] ^ (crc >>> 8); return (crc ^ 0xffffffff) >>> 0 }
/** Bounded plain ZIP for browser multi-file exports, not a project archive. */
export function directoryZip(directory) {
  const entries = []
  function scan(relative = '') {
    for (const item of fs.readdirSync(safePath(directory, relative), { withFileTypes: true })) {
      if (item.isSymbolicLink()) throw new HttpError(403, 'EXPORT_SYMLINK_DENIED')
      const name = relative ? `${relative}/${item.name}` : item.name
      if (item.isDirectory()) scan(name)
      else if (item.isFile()) {
        if (entries.length >= 20000) throw new HttpError(413, 'EXPORT_ENTRY_LIMIT')
        const file = safePath(directory, name), size = fs.statSync(file).size
        entries.push({ name, file, size })
      }
    }
  }
  scan()
  if (entries.reduce((total, item) => total + item.size, 0) > 256 * 1024 * 1024) throw new HttpError(413, 'EXPORT_TOO_LARGE')
  const chunks = [], central = []; let offset = 0
  for (const entry of entries) {
    const name = Buffer.from(entry.name, 'utf8'), bytes = fs.readFileSync(entry.file), crc = crc32(bytes)
    const header = Buffer.alloc(30)
    header.writeUInt32LE(0x04034b50); header.writeUInt16LE(20, 4); header.writeUInt16LE(0x800, 6)
    header.writeUInt32LE(crc, 14); header.writeUInt32LE(bytes.length, 18); header.writeUInt32LE(bytes.length, 22); header.writeUInt16LE(name.length, 26)
    chunks.push(header, name, bytes)
    const record = Buffer.alloc(46)
    record.writeUInt32LE(0x02014b50); record.writeUInt16LE(20, 4); record.writeUInt16LE(20, 6); record.writeUInt16LE(0x800, 8)
    record.writeUInt32LE(crc, 16); record.writeUInt32LE(bytes.length, 20); record.writeUInt32LE(bytes.length, 24); record.writeUInt16LE(name.length, 28); record.writeUInt32LE(offset, 42)
    central.push(record, name); offset += header.length + name.length + bytes.length
  }
  const centralBytes = Buffer.concat(central), end = Buffer.alloc(22)
  end.writeUInt32LE(0x06054b50); end.writeUInt16LE(entries.length, 8); end.writeUInt16LE(entries.length, 10); end.writeUInt32LE(centralBytes.length, 12); end.writeUInt32LE(offset, 16)
  return Buffer.concat([...chunks, centralBytes, end])
}
