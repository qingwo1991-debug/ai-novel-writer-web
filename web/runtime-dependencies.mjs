#!/usr/bin/env node
import fs from 'node:fs'
import path from 'node:path'
import { createRequire, isBuiltin } from 'node:module'
import { fileURLToPath } from 'node:url'
import { createHash } from 'node:crypto'

// Inventory mode is read-only. Copying is explicit and only used in a builder
// layer: never install, resolve over the network, or copy /app/node_modules/.pnpm.
const sourceRoot = path.dirname(path.dirname(fileURLToPath(import.meta.url)))
const arguments_ = process.argv.slice(2)
const copyIndex = arguments_.indexOf('--copy')
if (arguments_.length !== 0 && !(copyIndex === 0 && arguments_.length === 2 && path.isAbsolute(arguments_[1]))) {
  throw new Error('Usage: node web/runtime-dependencies.mjs [--copy /absolute/output]')
}
const destination = copyIndex === 0 ? path.resolve(arguments_[1]) : undefined
if (destination && (destination === sourceRoot || destination.startsWith(sourceRoot + path.sep)
  || destination === '/app' || fs.existsSync(destination))) {
  throw new Error('Copy destination must be a new directory outside the checkout and toolchain')
}
const target = { platform: 'linux', arch: process.arch, libc: 'glibc', node: process.versions.node, modules: process.versions.modules }
if (process.platform !== 'linux' || !['x64', 'arm64'].includes(target.arch)) throw new Error('Run on the target Linux build architecture')
const libc = process.report.getReport().header.glibcVersionRuntime
if (!libc) throw new Error('Debian/glibc runtime is required')
if (Number(process.versions.node.split('.')[0]) !== 22) throw new Error('Build and runtime must both use Node 22')

const blocked = name => /(?:^|\/)(?:electron(?:-[^/]*)?|sharp)$/u.test(name) || name.startsWith('@img/sharp-')
const packageName = specifier => specifier.startsWith('@') ? specifier.split('/').slice(0, 2).join('/') : specifier.split('/')[0]
const bare = specifier => !specifier.startsWith('.') && !path.isAbsolute(specifier) && !isBuiltin(specifier)
const meta = JSON.parse(fs.readFileSync(path.join(sourceRoot, 'dist-server/metafile.json'), 'utf8'))
const roots = new Set(['better-sqlite3', '@lancedb/lancedb', 'apache-arrow', 'yauzl'])
const discoveredImports = new Set()
for (const output of Object.values(meta.outputs)) for (const imported of output.imports ?? []) {
  if (imported.external && bare(imported.path)) { roots.add(packageName(imported.path)); discoveredImports.add(imported.path) }
}
// esbuild cannot see createRequire() calls. Inspect only inputs that actually
// entered the bundle; the explicit four seeds above remain a reviewed floor.
for (const input of Object.keys(meta.inputs)) {
  const filename = path.resolve(sourceRoot, input)
  const relative = path.relative(sourceRoot, filename)
  if (relative.startsWith('..') || path.isAbsolute(relative)) throw new Error(`Metafile input escapes source: ${input}`)
  const source = fs.readFileSync(filename, 'utf8')
  for (const match of source.matchAll(/\b(?:require|import)\(\s*['"]([^'"]+)['"]/gu)) {
    if (bare(match[1])) { roots.add(packageName(match[1])); discoveredImports.add(match[1]) }
  }
}

function resolvePackage(name, from) {
  if (!/^(?:@[a-z0-9._-]+\/)?[a-z0-9._-]+$/iu.test(name) || blocked(name)) throw new Error(`Forbidden runtime package: ${name}`)
  const require = createRequire(path.join(from, '__runtime_resolution__.cjs'))
  for (const directory of require.resolve.paths(name) ?? []) {
    const candidate = path.join(directory, name)
    try {
      const real = fs.realpathSync(candidate)
      const manifest = JSON.parse(fs.readFileSync(path.join(real, 'package.json'), 'utf8'))
      if (blocked(manifest.name ?? name)) throw new Error(`Forbidden runtime package: ${manifest.name}`)
      return { real, manifest }
    } catch (error) {
      if (error.code !== 'ENOENT' && error.code !== 'ENOTDIR') throw error
    }
  }
  return undefined
}
const compatible = (values, value) => !Array.isArray(values)
  || !values.includes(`!${value}`) && (values.every(item => item.startsWith('!')) || values.includes(value))
const packages = new Map()
const skipped = []
function collect(name, from, optional = false) {
  // Only the current Debian architecture's LanceDB binary is needed. Do not
  // follow optional packages for macOS/Windows/musl and inflate the closure.
  const lanceNative = '@lancedb/lancedb-linux-' + target.arch + '-gnu'
  if (name.startsWith('@lancedb/lancedb-') && name !== lanceNative) {
    skipped.push({ name, reason: 'not-target-platform' }); return undefined
  }
  const resolved = resolvePackage(name, from)
  if (!resolved) {
    if (optional && name !== lanceNative) { skipped.push({ name, reason: 'optional-not-installed' }); return undefined }
    throw new Error(`Installed runtime dependency missing: ${name} (from ${from})`)
  }
  const { real, manifest } = resolved
  if (!compatible(manifest.os, 'linux') || !compatible(manifest.cpu, target.arch) || !compatible(manifest.libc, 'glibc')) {
    if (optional) { skipped.push({ name, reason: 'not-target-platform' }); return undefined }
    throw new Error(`Incompatible runtime package: ${name}`)
  }
  if (packages.has(real)) return packages.get(real)
  const id = `${String(packages.size + 1).padStart(3, '0')}-${createHash('sha256').update(manifest.name + '@' + manifest.version + '\0' + real).digest('hex').slice(0, 12)}`
  const entry = { id, real, manifest, dependencies: new Map() }
  packages.set(real, entry)
  const dependencies = { ...manifest.dependencies, ...manifest.optionalDependencies }
  // Installed nonoptional peers are required by Node just like dependencies.
  for (const [peer, version] of Object.entries(manifest.peerDependencies ?? {})) {
    if (!manifest.peerDependenciesMeta?.[peer]?.optional) dependencies[peer] ??= version
    else if (resolvePackage(peer, real)) dependencies[peer] ??= version
  }
  for (const dependency of Object.keys(dependencies).sort()) {
    const child = collect(dependency, real, Object.hasOwn(manifest.optionalDependencies ?? {}, dependency))
    if (child) entry.dependencies.set(dependency, child)
  }
  return entry
}
const top = new Map([...roots].sort().map(name => [name, collect(name, sourceRoot)]))
if (!Array.from(packages.values()).some(entry => entry.manifest.name === `@lancedb/lancedb-linux-${target.arch}-gnu`)) {
  throw new Error('Required LanceDB native target was not collected')
}

function allowedFile(entry, filename) {
  const relative = path.relative(entry.real, filename)
  const parts = relative.split(path.sep)
  if (parts.includes('node_modules') || parts.includes('.git')) return false
  // better-sqlite3 ships several OS/arch prebuilds in one package. Keep only
  // target glibc's Node-compatible binary, plus ordinary package JS/assets.
  if (entry.manifest.name === 'better-sqlite3' && parts[0] === 'prebuilds' && relative.endsWith('.node')) {
    return path.basename(filename) === `linux-${target.arch}.node`
  }
  return true
}
function inventory(entry, filename = entry.real) {
  if (!allowedFile(entry, filename)) return { bytes: 0, files: 0 }
  const info = fs.lstatSync(filename)
  if (info.isSymbolicLink()) {
    // All dependency links are intentionally reconstructed below. Reject even
    // package-internal links rather than dereferencing possible cycles/escapes.
    throw new Error(`Unexpected package-content symlink: ${filename}`)
  }
  if (info.isDirectory()) return fs.readdirSync(filename).reduce((sum, name) => {
    const child = inventory(entry, path.join(filename, name))
    return { bytes: sum.bytes + child.bytes, files: sum.files + child.files }
  }, { bytes: 0, files: 0 })
  if (!info.isFile()) throw new Error(`Unsupported package entry: ${filename}`)
  return { bytes: info.size, files: 1 }
}
const entries = Array.from(packages.values()).map(entry => ({
  entry, size: inventory(entry),
}))
const report = {
  target, roots: [...roots].sort(), discoveredImports: [...discoveredImports].sort(),
  packageCount: entries.length,
  totalBytes: entries.reduce((sum, item) => sum + item.size.bytes, 0),
  totalFiles: entries.reduce((sum, item) => sum + item.size.files, 0),
  packages: entries.map(({ entry, size }) => ({ name: entry.manifest.name, version: entry.manifest.version, ...size })),
  skipped,
  policy: 'Existing installed production dependency/peer closure only; no installs; no Electron/sharp; target Linux/glibc native binaries only.',
}
if (destination) {
  fs.mkdirSync(path.join(destination, 'node_modules', '.runtime'), { recursive: true })
  const targetPath = entry => path.join(destination, 'node_modules', '.runtime', entry.id, 'package')
  const link = (from, to) => {
    fs.mkdirSync(path.dirname(from), { recursive: true })
    fs.symlinkSync(path.relative(path.dirname(from), to), from, 'dir')
  }
  for (const { entry } of entries) {
    fs.cpSync(entry.real, targetPath(entry), { recursive: true, dereference: true, filter: filename => allowedFile(entry, filename) })
  }
  for (const { entry } of entries) for (const [name, dependency] of entry.dependencies) {
    link(path.join(targetPath(entry), 'node_modules', name), targetPath(dependency))
  }
  for (const [name, entry] of top) link(path.join(destination, 'node_modules', name), targetPath(entry))
  fs.writeFileSync(path.join(destination, 'package.json'), JSON.stringify({ private: true, type: 'module' }) + '\n')
  fs.writeFileSync(path.join(destination, 'runtime-dependencies.json'), JSON.stringify(report, null, 2) + '\n')
}
console.log(JSON.stringify(report, null, 2))
