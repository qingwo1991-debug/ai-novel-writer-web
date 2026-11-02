import './browser/register-typescript.mjs'
import { test } from 'node:test'
import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { randomUUID } from 'node:crypto'
const { createLibraryVisibility } = await import('./library-visibility.ts')
const { createCanonicalProjectManifest } = await import('../src/shared/project-format.ts')

function fixture(t) {
  const base = fs.realpathSync(fs.mkdtempSync(path.join(os.tmpdir(), 'library-visibility-')))
  t.after(() => fs.rmSync(base, { recursive: true, force: true }))
  const libraryRoot = path.join(base, 'library'), globalRoot = path.join(base, 'global')
  fs.mkdirSync(libraryRoot); fs.mkdirSync(globalRoot)
  const statePath = path.join(globalRoot, 'web-library-hidden.json')
  const open = () => createLibraryVisibility({ libraryRoot, statePath })
  const manifest = projectId => createCanonicalProjectManifest({ projectId, createdAt: '2026-01-01T00:00:00.000Z' })
  const project = (name, projectId = randomUUID(), parent = libraryRoot) => {
    const root = path.join(parent, name), storage = path.join(root, '.ai-novel')
    fs.mkdirSync(storage, { recursive: true })
    fs.writeFileSync(path.join(storage, 'project.json'), JSON.stringify(manifest(projectId)))
    fs.writeFileSync(path.join(storage, 'project.db'), 'synthetic database; never opened')
    return { root, storage, projectId }
  }
  return { base, libraryRoot, statePath, globalRoot, open, project, manifest }
}

test('hide survives a new helper, leaves a sibling visible and never changes project files', t => {
  const f = fixture(t), a = f.project('A'), b = f.project('B')
  const manifestFile = path.join(a.storage, 'project.json'), database = path.join(a.storage, 'project.db')
  const before = [fs.readFileSync(manifestFile), fs.readFileSync(database)]
  const v = f.open()
  assert.equal(v.isHidden(a.root, a.projectId), false)
  v.hide(a.root)
  assert.equal(f.open().isHidden(a.root, a.projectId), true)
  assert.equal(f.open().isHidden(b.root, b.projectId), false)
  assert.deepEqual(fs.readFileSync(manifestFile), before[0])
  assert.deepEqual(fs.readFileSync(database), before[1])
  assert.deepEqual(fs.readdirSync(a.storage).sort(), ['project.db', 'project.json'])
  assert.equal(fs.statSync(f.statePath).mode & 0o777, 0o600)
  assert.deepEqual(fs.readdirSync(f.globalRoot), ['web-library-hidden.json'])
  assert.deepEqual(JSON.parse(fs.readFileSync(f.statePath, 'utf8')), { version: 1, hidden: [{ path: a.root, projectId: a.projectId }] })
})

test('replacement identity at the same path stays visible and mismatched IDs cannot hide it', t => {
  const f = fixture(t), a = f.project('A'), v = f.open()
  v.hide(a.root)
  const newId = randomUUID()
  fs.writeFileSync(path.join(a.storage, 'project.json'), JSON.stringify(f.manifest(newId)))
  assert.equal(v.isHidden(a.root, newId), false)
  assert.equal(v.isHidden(a.root, a.projectId), false)
  v.show(a.root)
  assert.equal(JSON.parse(fs.readFileSync(f.statePath, 'utf8')).hidden.length, 1, 'show removes only the current identity')
  v.hide(a.root)
  assert.equal(v.isHidden(a.root, newId), true)
  v.show(a.root)
  assert.equal(f.open().isHidden(a.root, newId), false)
})

test('show restores visibility persistently; repeated hide/show are idempotent', t => {
  const f = fixture(t), a = f.project('A'), v = f.open()
  v.hide(a.root); v.hide(a.root)
  assert.equal(JSON.parse(fs.readFileSync(f.statePath, 'utf8')).hidden.length, 1)
  const oldHelper = f.open()
  v.show(a.root); v.show(a.root)
  assert.equal(oldHelper.isHidden(a.root, a.projectId), false)
  assert.equal(f.open().isHidden(a.root, a.projectId), false)
  assert.deepEqual(JSON.parse(fs.readFileSync(f.statePath, 'utf8')), { version: 1, hidden: [] })
})

test('spaces and trailing blanks remain distinct exact filenames', t => {
  const f = fixture(t), a = f.project('  my book  '), b = f.project('  my book'), v = f.open()
  v.hide(a.root)
  assert.equal(f.open().isHidden(a.root, a.projectId), true)
  assert.equal(v.isHidden(b.root, b.projectId), false)
  assert.equal(JSON.parse(fs.readFileSync(f.statePath, 'utf8')).hidden[0].path, a.root)
  assert.throws(() => v.hide(`${a.root} `))
})

test('mutators reject outside, nested, relative, traversal and alias paths', t => {
  const f = fixture(t), a = f.project('A'), outside = f.project('outside', randomUUID(), f.base), nested = f.project('nested', randomUUID(), a.root)
  const v = f.open()
  for (const candidate of [outside.root, nested.root, f.libraryRoot, 'A', `${f.libraryRoot}/../library/A`, `${f.libraryRoot}/./A`, `${a.root}/`, `${a.root}\0`]) {
    assert.throws(() => v.hide(candidate), candidate)
    assert.throws(() => v.show(candidate), candidate)
    assert.equal(v.isHidden(candidate, a.projectId), false)
  }
  assert.equal(fs.existsSync(f.statePath), false)
})

test('project, storage, manifest and database symlinks are rejected without writes', t => {
  const f = fixture(t), a = f.project('A'), v = f.open()
  const alias = path.join(f.libraryRoot, 'alias')
  fs.symlinkSync(a.root, alias, 'dir')
  assert.throws(() => v.hide(alias))
  for (const name of ['project.json', 'project.db']) {
    const file = path.join(a.storage, name), target = path.join(f.base, name)
    fs.renameSync(file, target); fs.symlinkSync(target, file)
    assert.throws(() => v.hide(a.root))
    assert.equal(v.isHidden(a.root, a.projectId), false)
    fs.unlinkSync(file); fs.renameSync(target, file)
  }
  const target = path.join(f.base, 'storage')
  fs.renameSync(a.storage, target); fs.symlinkSync(target, a.storage, 'dir')
  assert.throws(() => v.hide(a.root))
  assert.equal(fs.existsSync(f.statePath), false)
})

test('invalid canonical manifests, IDs and nonregular or absent databases cannot be hidden', t => {
  const f = fixture(t), a = f.project('A'), v = f.open(), file = path.join(a.storage, 'project.json')
  for (const value of [{ projectId: a.projectId }, { ...f.manifest(a.projectId), projectId: '../bad' }, { ...f.manifest(a.projectId), projectId: ` ${a.projectId}` }, { ...f.manifest(a.projectId), extra: true }]) {
    fs.writeFileSync(file, JSON.stringify(value))
    assert.throws(() => v.hide(a.root))
  }
  fs.writeFileSync(file, JSON.stringify(f.manifest(a.projectId)))
  const database = path.join(a.storage, 'project.db')
  fs.unlinkSync(database)
  assert.throws(() => v.hide(a.root))
  fs.mkdirSync(database)
  assert.throws(() => v.hide(a.root))
  assert.equal(fs.existsSync(f.statePath), false)
})

test('corrupt or invalid persisted state throws and is never overwritten', t => {
  const f = fixture(t), a = f.project('A'), v = f.open()
  const entry = { path: a.root, projectId: a.projectId }
  const invalid = ['{', 'null', '[]', JSON.stringify({ version: 2, hidden: [] }), JSON.stringify({ version: 1, hidden: 'bad' }),
    JSON.stringify({ version: 1, hidden: [{ ...entry, projectId: 'invalid' }] }),
    JSON.stringify({ version: 1, hidden: [{ ...entry, path: path.join(f.base, 'outside') }] }),
    JSON.stringify({ version: 1, hidden: [entry, entry] }), JSON.stringify({ version: 1, hidden: [], extra: true })]
  for (const bytes of invalid) {
    fs.writeFileSync(f.statePath, bytes)
    assert.throws(() => f.open())
    assert.throws(() => v.hide(a.root))
    assert.throws(() => v.show(a.root))
    assert.throws(() => v.isHidden(a.root, a.projectId))
    assert.equal(fs.readFileSync(f.statePath, 'utf8'), bytes)
  }
})

test('unsafe state location and symlink state cannot modify their target', t => {
  const f = fixture(t), a = f.project('A'), v = f.open()
  assert.throws(() => createLibraryVisibility({ libraryRoot: f.libraryRoot, statePath: path.join(a.root, 'hidden.json') }))
  const target = path.join(f.base, 'target.json'), bytes = '{"version":1,"hidden":[]}'
  fs.writeFileSync(target, bytes); fs.symlinkSync(target, f.statePath)
  assert.throws(() => f.open())
  assert.throws(() => v.hide(a.root))
  assert.equal(fs.readFileSync(target, 'utf8'), bytes)
})
