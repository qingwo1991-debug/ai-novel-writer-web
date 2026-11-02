import assert from 'node:assert/strict'
import { registerHooks, stripTypeScriptTypes } from 'node:module'
import { readFileSync, writeFileSync, mkdirSync, existsSync } from 'node:fs'
import path from 'node:path'

// Synthetic, disposable GitHub runner only. No app data, home, .env or author
// files are loaded. Business implementations below are original TS sources;
// only platform/locale/locator/dialog authority boundaries are replaced.
assert.equal(process.env.GITHUB_ACTIONS, 'true')
const base = process.env.RUNNER_TEMP
const target = process.env.GO_PROMPT_SKILLS_CONTRACT
assert(base && target && path.isAbsolute(base) && path.isAbsolute(target))
assert.equal(target, path.join(base, 'novel-go-prompt-skills', 'contract.json'))
assert(!existsSync(target))
const directory = path.dirname(target)
mkdirSync(directory, { recursive: true, mode: 0o700 })
const appRoot = path.join(directory, 'typescript-app')
mkdirSync(appRoot, { mode: 0o700 })
const root = new URL('../../', import.meta.url)
const names = ['electron/controllers/app-data-controller.ts', 'electron/services/writing-skill-package.ts', 'src/shared/writing-skills.ts', 'electron/utils/config-utils.ts']
const sources = new Map(names.map(name => [new URL(name, root).href, readFileSync(new URL(name, root), 'utf8')]))
const boundary = new Map([
 ['electron/platform', `export const handlers = new Map(); export const operations = {handle:(name,fn)=>handlers.set(name,fn)}; export const appHost={getLocale:()=>"en-US"}`],
 ['electron/utils/remembered-dialog', 'export async function showOpenDialogInLastDirectory(){throw new Error("DIALOG_UNAVAILABLE")}'],
 ['electron/i18n', 'export function mainText(_locale,_zh,en){return en}'],
 ['electron/services/app-data-locator', `export const VELA_HOME=${JSON.stringify(appRoot)}; export const GLOBAL_CONFIG_PATH=VELA_HOME+"/config.json"; export const MODELS_CONFIG_PATH=VELA_HOME+"/models.json"; export const RECENT_PROJECTS_PATH=VELA_HOME+"/recent.json"; export function assertGlobalDataReady(){}; export function assertGlobalPathAccess(p){if(!p.startsWith(VELA_HOME+"/"))throw new Error("OUTSIDE_SYNTHETIC_ROOT")}`],
 ['src/shared/ipc-channels', 'export const GlobalConfig=undefined'],
].map(([name, source]) => [new URL(name, root).href, source]))
const hooks = registerHooks({
 resolve(specifier, context, next) {
  if (specifier.startsWith('node:')) return next(specifier, context)
  const resolved = new URL(specifier, context.parentURL).href
  if (boundary.has(resolved)) return {url:resolved, shortCircuit:true}
  if (sources.has(resolved)) return {url:resolved, shortCircuit:true}
  if (sources.has(resolved+'.ts')) return {url:resolved+'.ts', shortCircuit:true}
  throw new Error('UNEXPECTED_ORACLE_DEPENDENCY '+specifier)
 },
 load(url, context, next) {
  if (url.startsWith('node:')) return next(url, context)
  if (boundary.has(url)) return {format:'module',source:boundary.get(url),shortCircuit:true}
  assert(sources.has(url),url)
  return {format:'module',source:stripTypeScriptTypes(sources.get(url),{mode:'transform',sourceUrl:url}),shortCircuit:true}
 },
})
const shared = await import(new URL(names[2],root).href)
const pkg = await import(new URL(names[1],root).href)
const {registerAppDataController} = await import(new URL(names[0],root).href)
const {handlers} = await import(new URL('electron/platform',root).href)
registerAppDataController()
const clean = value => JSON.parse(JSON.stringify(value))
const cases = []
const rawCases = [
 '', 'Plain prompt only 中文 😀',
 '---\nname: public-review\ndescription: Review public text\nlanguage: English\nversion: 2\n---\nReview prose.',
 '---\nname: local-block\ndescription: >\n  Synthetic first line\n  second line\nstage: planning\n---\nPlan fictional events.',
 '---\nname: all-dependencies\nallowed-tools: Read\n---\nRead [ref](./references/a.md). Run the script. Install the hook. Delegate to an agent. Use the Read tool. 子代理 调用测试工具',
 '---\nname: ordinary-link\n---\nSee [note](FORMS.md) and [image](cover.png).',
 '---\nname: angled\n---\nSee [note](<./missing.md>).',
 '---\nname: reference-def\n---\nReview [note][id].\n[id]: ./missing.md',
 '---\nname: skills-var\n---\nRead ${SKILL_DIR}/assets/a.md.',
 '---\nname: quoted\ndisplay_name: "Public display"\nlanguage: zh\nstage: refinement\n---\n  Polish this.  ',
 'x'.repeat(65536), 'x'.repeat(65537),
]
for (const [index,raw] of rawCases.entries()) cases.push({name:'markdown-'+index,kind:'markdown',raw,result:clean(shared.inspectWritingSkillMarkdown(raw))})
const localCases = [
 {raw:'---\nname: public-local\ndescription: >\n  Folded description\n  fixture\n---\nRead [reference](./references/a.md).',files:{'references/a.md':' Public reference.\nRead [detail](./b.txt). ','references/b.txt':'Public detail.'}},
 {raw:'See [file](FORMS.md) and [image](image.png).',files:{'FORMS.md':'Public form.'},fallback:'public-fallback'},
 {raw:'Read references/missing.md.',files:{}},
 {raw:'See [root](SKILL.md).',files:{}},
 {raw:'See [voice](./references/a.md).',files:{'references/a.md':'[back](../SKILL.md)\nPublic voice.'}},
]
for (const [index,value] of localCases.entries()) cases.push({name:'local-'+index,kind:'local',...value,result:clean(shared.inspectLocalWritingSkill(value.raw,{fallbackName:value.fallback,readFile:p=>value.files[p]}))})
for (const [index,bundle] of [{status:'none'},{status:'intact',files:[]},{status:'damaged'},{status:'intact',files:[{path:'references/a.md',content:' Public bundled text. '}]}].entries()) {
 const raw='---\nname: installed-public\ndescription: >\n  Public installed\n  description\n---\nRead [reference](./references/a.md).'
 cases.push({name:'installed-'+index,kind:'installed',raw,bundle,result:clean(shared.inspectInstalledWritingSkill(raw,bundle,'public-fallback'))})
}
for (const [index,value] of ['https://github.com/public/fixture','https://github.com/public/fixture.git','https://github.com/public/fixture/tree/main/public','https://github.com/public/fixture/blob/main/public/SKILL.md','https://raw.githubusercontent.com/public/fixture/main/SKILL.md','http://github.com/public/fixture','https://user:pass@github.com/public/fixture','https://evil.invalid/public/fixture','https://github.com/public/fixture/blob/main/readme.txt','https://github.com/public/fixture/blob/main/%2F/SKILL.md'].entries()) {
 try {cases.push({name:'url-'+index,kind:'url',value,result:clean(shared.parseGitHubWritingSkillUrl(value)),failed:false})} catch {cases.push({name:'url-'+index,kind:'url',value,failed:true})}
}
const event={sender:{id:1,isDestroyed:()=>false}}
const invoke=(name,...args)=>handlers.get(name)(event,...args)
const promptActions=[]
const record=async(action,args=[])=>{const result=clean(await invoke('prompt:'+action+'-global',...args));promptActions.push({action,args,result})}
await record('load')
mkdirSync(path.join(appRoot,'prompts'),{mode:0o700})
writeFileSync(path.join(appRoot,'prompts','legacy-public.json'),JSON.stringify({key:'legacy-public',label:'Public legacy',content:'Old public prompt'}),{mode:0o600,flag:'wx'})
promptActions.push({action:'seed',file:'legacy-public.json',value:{key:'legacy-public',label:'Public legacy',content:'Old public prompt'}})
await record('load')
await record('save',[{key:'legacy-public',name:'Synthetic prompt',systemPrompt:'Public 中文 😀',extra:{retained:true}}])
await record('save',[{key:'legacy-public',writingLanguage:'en-US',content:'Public English'}])
await record('load')
await record('delete',['legacy-public','zh-CN'])
await record('load')
await record('delete',['legacy-public','en-US'])
await record('load')
const bundleFiles=[{path:'references/a.md',content:'Public original bundle text.'}]
const bundleReadCases=[]
for (const variant of ['none','empty','intact','tampered','invalid-manifest','missing-file','traversal']) {
 const manifest=variant==='none'?undefined:variant==='empty'?pkg.writingSkillBundleManifest([]):variant==='invalid-manifest'?'{bad':variant==='traversal'?JSON.stringify({version:1,files:[{path:'../outside.md',sha256:'0'.repeat(64)}]}):pkg.writingSkillBundleManifest(bundleFiles)
 const files=variant==='missing-file'?{}:{'references/a.md':variant==='tampered'?'Changed public text.':bundleFiles[0].content}
 const result=pkg.readInstalledWritingSkillBundle(p=>p===pkg.WRITING_SKILL_BUNDLE_MANIFEST?(manifest===undefined?null:Buffer.from(manifest)):files[p]===undefined?null:Buffer.from(files[p]))
 bundleReadCases.push({name:variant,manifest,files,result})
}
const packageCases=[]
for (const [index,fixture] of localCases.entries()) {
 const folder=path.join(directory,'synthetic-package-'+index)
 mkdirSync(folder,{mode:0o700})
 writeFileSync(path.join(folder,'SKILL.md'),fixture.raw,{mode:0o600,flag:'wx'})
 for (const [relative,content] of Object.entries(fixture.files)) {const filename=path.join(folder,relative);mkdirSync(path.dirname(filename),{recursive:true,mode:0o700});writeFileSync(filename,content,{mode:0o600,flag:'wx'})}
 const result=clean(pkg.loadLocalWritingSkill({kind:'folder',path:folder}))
 packageCases.push({name:'package-'+index,folder:path.basename(folder),raw:fixture.raw,files:fixture.files,result})
}
const inventoryFixtures=[]
for (const variant of ['legacy','bundled','damaged']) {
 const name='public-'+variant,folder=path.join(appRoot,'skills',name)
 mkdirSync(path.join(folder,'references'),{recursive:true,mode:0o700})
 const raw='---\nname: '+name+'\n---\nPublic prompt.'
 writeFileSync(path.join(folder,'SKILL.md'),raw,{mode:0o600,flag:'wx'})
 const manifest=variant==='legacy'?undefined:pkg.writingSkillBundleManifest(bundleFiles)
 if(manifest!==undefined)writeFileSync(path.join(folder,pkg.WRITING_SKILL_BUNDLE_MANIFEST),manifest,{mode:0o600,flag:'wx'})
 const files={'references/a.md':variant==='damaged'?'Changed public fixture.':bundleFiles[0].content}
 for(const [relative,content] of Object.entries(files))writeFileSync(path.join(folder,relative),content,{mode:0o600,flag:'wx'})
 inventoryFixtures.push({name,raw,manifest,files})
}
const publicInventory=value=>clean(value).map(({baseDir,filePath,...skill})=>skill).sort((a,b)=>a.name.localeCompare(b.name))
const inventoryBefore=publicInventory(await invoke('skills:list-user'))
const inventoryUninstall=clean(await invoke('skills:uninstall-user','public-bundled'))
const inventoryAfter=publicInventory(await invoke('skills:list-user'))
hooks.deregister()
writeFileSync(target,JSON.stringify({formatVersion:1,cases,promptActions,bundleReadCases,packageCases,inventoryFixtures,inventoryBefore,inventoryUninstall,inventoryAfter})+'\n',{flag:'wx',mode:0o600})
console.log(`Original prompt controller / shared writing-skills / package oracle: ${cases.length} inspections, ${promptActions.length} prompt steps, ${bundleReadCases.length} bundles`)
