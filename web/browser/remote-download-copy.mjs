import fs from 'node:fs'
import path from 'node:path'
import { execFile } from 'node:child_process'
import { promisify } from 'node:util'

// Host-only helper. Copies exactly two completed browser downloads requested by
// the owned native acceptance run; never reads cookies, user contexts or tokens.
const exec = promisify(execFile)
const root = path.resolve(process.cwd(), '.native-test-data/browser-transfer')
const output = path.resolve(process.argv[2] ?? '')
const inside = (parent, child) => child.startsWith(`${parent}${path.sep}`)
if (!inside(root, output)) throw new Error('Output must be inside the ignored browser-transfer artifact root')
fs.mkdirSync(output, { recursive: true })
const requestFile = path.join(output, 'download-copy-request.json')
const receipts = []
let active = false
let watcher
let timeout
const completion = new Promise((resolve, reject) => {
  const copy = async () => {
    if (active || !fs.existsSync(requestFile)) return
    active = true
    try {
      const request = JSON.parse(fs.readFileSync(requestFile, 'utf8'))
      if (!/^[a-zA-Z0-9-]{16,80}$/.test(request.guid)) throw new Error('Invalid browser download GUID')
      if (!/^\/tmp\/novel-native-acceptance-[a-zA-Z0-9_-]+\/[a-zA-Z0-9-]{16,80}$/.test(request.remotePath)
        || path.basename(request.remotePath) !== request.guid) throw new Error('Remote path is not an owned acceptance download')
      const workspacePrefix = '/workspace/'
      if (!request.localPath.startsWith(workspacePrefix)) throw new Error('Destination must be a workspace artifact')
      const local = path.resolve(process.cwd(), request.localPath.slice(workspacePrefix.length))
      if (!inside(output, local) || !['downloaded-manuscript.txt', 'downloaded-project.ainovel'].includes(path.basename(local))) throw new Error('Unexpected download destination')
      if (fs.existsSync(local)) throw new Error('Refusing to overwrite an existing browser artifact')
      const temporary = `${local}.copying`
      await exec('docker', ['cp', `browserless:${request.remotePath}`, temporary])
      const info = fs.lstatSync(temporary)
      if (!info.isFile() || info.isSymbolicLink() || info.size === 0) throw new Error('Copied browser download is not a nonempty regular file')
      // Remove the request before publishing the artifact; the browser script
      // resumes on the atomic rename and may immediately request the next file.
      fs.unlinkSync(requestFile)
      fs.renameSync(temporary, local)
      receipts.push({ guid: request.guid, remotePath: request.remotePath, copiedArtifact: local, bytes: info.size })
      fs.writeFileSync(path.join(output, 'remote-download-copy-receipts.json'), JSON.stringify(receipts, null, 2))
      console.log('COPIED_REAL_BROWSER_DOWNLOAD', JSON.stringify(receipts.at(-1)))
      if (receipts.length === 2) resolve(receipts)
    } catch (error) { reject(error) } finally { active = false }
  }
  watcher = fs.watch(output, () => { void copy() })
  timeout = setTimeout(() => reject(new Error(`Timed out after copying ${receipts.length}/2 actual browser downloads`)), 5 * 60_000)
  void copy()
})
try { await completion } finally { watcher.close(); clearTimeout(timeout) }
