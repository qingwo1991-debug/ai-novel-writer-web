import { build } from 'esbuild'
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
const root = path.dirname(path.dirname(fileURLToPath(import.meta.url)))
const result = await build({
  entryPoints: [path.join(root, 'web/worker.ts')], outfile: path.join(root, 'dist-server/worker.mjs'),
  bundle: true, platform: 'node', format: 'esm', target: 'node22', packages: 'external', metafile: true,
  logLevel: 'info', sourcemap: 'linked',
})
const electronImports = Object.values(result.metafile.outputs).flatMap(output => output.imports).filter(entry => entry.path === 'electron' || entry.path.startsWith('electron/'))
if (electronImports.length) throw new Error('Node server bundle still imports Electron')
fs.mkdirSync(path.join(root, 'dist-server'), { recursive: true })
fs.writeFileSync(path.join(root, 'dist-server/metafile.json'), JSON.stringify(result.metafile, null, 2))
for (const file of ['server.mjs', 'security.mjs', 'downloads.mjs', 'public-response.mjs', 'client-departure.mjs']) fs.copyFileSync(path.join(root, 'web', file), path.join(root, 'dist-server', file))
console.log('Pure Node server bundle verified: no Electron imports.')
