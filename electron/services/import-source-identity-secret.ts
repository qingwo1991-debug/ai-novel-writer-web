import path from 'node:path'
import { appHost } from '../platform'
import { loadOrCreatePrivateKey } from '../platform/server-secret-storage'

const SECRET_FILE_NAME = 'import-source-identity.secret'

/** Application-level HMAC key. The project DB never receives this value. */
export function loadApplicationImportSourceSecret(): Buffer {
  return loadOrCreatePrivateKey(path.join(appHost.getPath('userData'), SECRET_FILE_NAME))
}
