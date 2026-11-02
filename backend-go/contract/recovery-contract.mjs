// Execute the actual repository and recursive source dependencies, GitHub only.
import assert from 'node:assert/strict'
import {builtinModules,createRequire,registerHooks,stripTypeScriptTypes} from 'node:module'
import {existsSync,mkdirSync,readFileSync,writeFileSync,openSync,closeSync} from 'node:fs'
import path from 'node:path'
import {fileURLToPath} from 'node:url'
assert.equal(process.env.GITHUB_ACTIONS,'true')
const base=process.env.RUNNER_TEMP,target=process.env.GO_RECOVERY_CONTRACT
assert.equal(target,path.join(base,'novel-go-recovery','contract.json'))
const baseline=path.join(path.dirname(process.env.GO_SQLITE_CONTRACT),'baseline-v7.db'),directory=path.dirname(target)
assert(!existsSync(directory));mkdirSync(directory,{recursive:true,mode:0o700})
const root=new URL('../../',import.meta.url),dbURL=new URL('electron/database',root).href,key='__GO_RECOVERY_ORIGINAL_DB__'
const Database=createRequire(import.meta.url)('better-sqlite3');const probe=new Database(':memory:');probe.close()
globalThis[key]=null
const builtins=new Set(builtinModules.flatMap(name=>[name,`node:${name}`]))
const originalModules=new Set()
const hooks=registerHooks({resolve(specifier,context,next){if(builtins.has(specifier))return next(specifier,context);assert(specifier.startsWith('.')||specifier.startsWith('file:'),specifier);const url=new URL(specifier,context.parentURL??root).href;if(url===dbURL||url===dbURL+'.ts')return {url:'recovery-contract:database',shortCircuit:true};const candidate=url.endsWith('.ts')?url:url+'.ts',filename=fileURLToPath(candidate),relative=path.relative(fileURLToPath(root),filename);assert(relative&&!relative.startsWith('..')&&!path.isAbsolute(relative));assert(/^(electron|src\/(shared|services|utils))\//.test(relative),relative);assert(existsSync(filename),relative);originalModules.add(relative);return {url:candidate,shortCircuit:true}},load(url,context,next){if(url==='recovery-contract:database')return {format:'module',source:`export function getProjectDb(){return globalThis[${JSON.stringify(key)}]}\nexport function getCurrentProjectPath(){return null}`,shortCircuit:true};if(!url.startsWith(root.href)||!url.endsWith('.ts'))return next(url,context);return {format:'module',source:stripTypeScriptTypes(readFileSync(new URL(url),'utf8'),{mode:'transform',sourceUrl:url}),shortCircuit:true}}})
const {RecoveryCandidateRepository:R}=await import(new URL('electron/repositories/recovery-candidate-repository.ts',root).href)
const unicode='中文 café e\u0301 🙂 <&>\u2028\u2029 " \\ \n'
function source(changes={}){return {chapterNumber:1,title:'合成标题',role:'铺垫',purpose:'找到钥匙',keyEvents:'打开旧门',characters:['林秋','星使'],suspenseHook:'门后是谁',userGuidance:'作者提示',...changes}}
function request(changes={}){return {runId:'run-original',stepId:'step-original',projectId:'synthetic-project',chapterNumber:1,chapterTitle:'合成章名',source:source(),sourceDraft:null,visibleText:'<think>private reasoning</think> 合成可见正文🙂 ',failureCode:'UPSTREAM_INTERRUPTED',failureReason:'synthetic interruption',...changes}}
function insert(db,table,row){const cols=Object.keys(row);assert(cols.every(x=>/^[a-z_]+$/.test(x)));db.prepare(`INSERT INTO ${table}(${cols.join(',')}) VALUES(${cols.map(()=>'?').join(',')})`).run(...Object.values(row))}
function blueprint(db,changes={}){const s=source(changes);insert(db,'blueprints',{chapter_number:s.chapterNumber,title:s.title,role:s.role,purpose:s.purpose,key_events:s.keyEvents,characters:JSON.stringify(s.characters),suspense_hook:s.suspenseHook??'',user_guidance:s.userGuidance??''})}
function draft(db,version=1){const content=db.prepare('INSERT INTO contents(body) VALUES(?)').run('synthetic current body');return Number(db.prepare("INSERT INTO drafts(chapter_number,version,status,content_id) VALUES(1,?,'draft',?)").run(version,content.lastInsertRowid).lastInsertRowid)}
const definitions=[];const add=(name,method,options={})=>definitions.push({name,method,...options})
add('record-current-no-draft','Record',{setup:db=>blueprint(db)})
add('record-no-blueprint-stale','Record')
add('record-unicode-ordered-hash','Record',{input:request({source:source({title:unicode,characters:[unicode],suspenseHook:unicode,userGuidance:unicode}),chapterTitle:unicode,visibleText:unicode}),setup:db=>blueprint(db,{title:unicode,characters:[unicode],suspenseHook:unicode,userGuidance:unicode})})
add('record-nullish-optionals','Record',{input:request({source:source({suspenseHook:null,userGuidance:null})}),setup:db=>blueprint(db,{suspenseHook:'',userGuidance:''})})
for(const field of ['runId','stepId','projectId']){add('record-empty-'+field,'Record',{input:request({[field]:' \uFEFF '})});add('record-trim-'+field,'Record',{input:request({[field]:' \uFEFF id \uFEFF '})})}
for(const chapterNumber of [0,-1,1.5])add('record-invalid-chapter-'+String(chapterNumber).replace('.','_'),'Record',{input:request({chapterNumber})})
add('record-source-chapter-mismatch','Record',{input:request({source:source({chapterNumber:2})})})
for(const sourceDraft of [{id:0,version:1},{id:1,version:0},{id:1.5,version:1}])add('record-invalid-draft-'+definitions.length,'Record',{input:request({sourceDraft})})
add('record-source-draft-exact','Record',{setup:(db,def)=>{blueprint(db);def.input=request({sourceDraft:{id:draft(db),version:1}})}})
add('record-source-draft-version-stale','Record',{setup:(db,def)=>{blueprint(db);def.input=request({sourceDraft:{id:draft(db),version:2}})}})
add('record-source-no-draft-but-current-draft','Record',{setup:db=>{blueprint(db);draft(db)}})
for(const [label,text]of [['paired','prefix<think>secret</think>正文'],['unclosed','prefix<think>private'],['orphan','private</think> visible <THINK>secret</THINK> end'],['nested','<think>outer<think>inner</think>visible'],['whitespace',' \uFEFF '],['only-thought','<think>secret</think>']])add('record-visible-'+label,'Record',{input:request({visibleText:text}),setup:db=>blueprint(db)})
add('record-truncate-reason','Record',{input:request({failureCode:' '+ 'c'.repeat(170)+' ',failureReason:' '+ 'r'.repeat(2010)+' '})})
add('record-replacement-valid','Record',{setup:(db,def)=>{blueprint(db);const old=R.record(request());def.input=request({replacesCandidateId:old.candidateId});def.oldId=old.candidateId}})
for(const [label,changes]of [['other-run',{runId:'other'}],['other-project',{projectId:'other'}],['other-chapter',{chapterNumber:2,source:source({chapterNumber:2})}]])add('record-replacement-'+label,'Record',{setup:(db,def)=>{const old=R.record(request());def.input=request({...changes,replacesCandidateId:old.candidateId});def.oldId=old.candidateId}})
add('record-replacement-missing','Record',{input:request({replacesCandidateId:'absent'})})
add('list-empty','List')
add('list-current','List',{setup:db=>{blueprint(db);R.record(request())}})
add('list-content-corrupt','List',{setup:db=>{R.record(request());db.exec("UPDATE recovery_candidates SET visible_text='tampered'")}})
add('list-source-corrupt','List',{setup:db=>{R.record(request());db.exec("UPDATE recovery_candidates SET source_snapshot='tampered'")}})
add('list-legacy-draft-identity-uncaptured','List',{setup:db=>{blueprint(db);R.record(request());db.exec('UPDATE recovery_candidates SET source_draft_identity_captured=0')}})
add('list-blueprint-changed','List',{setup:db=>{blueprint(db);R.record(request());db.exec("UPDATE blueprints SET title='changed'")}})
add('list-draft-body-changed-same-identity','List',{setup:db=>{blueprint(db);const id=draft(db);R.record(request({sourceDraft:{id,version:1}}));db.exec("UPDATE contents SET body='edited same identity'")}})
add('list-new-draft-identity','List',{setup:db=>{blueprint(db);const id=draft(db);R.record(request({sourceDraft:{id,version:1}}));draft(db,2)}})
for(const method of ['Update','Resolve'])for(const mode of ['current','stale','corrupt-content','corrupt-source','resolved','missing'])add(method.toLowerCase()+'-'+mode,method,{text:'new <think>private</think> visible',status:'continued',setup:(db,def)=>{if(mode==='missing'){def.id='absent';return}blueprint(db);def.id=R.record(request()).candidateId;if(mode==='stale')db.exec("UPDATE blueprints SET title='changed'");if(mode==='corrupt-content')db.exec("UPDATE recovery_candidates SET content_hash='bad'");if(mode==='corrupt-source')db.exec("UPDATE recovery_candidates SET source_hash='bad'");if(mode==='resolved')R.resolve(def.id,'discarded')}})
for(const status of ['discarded','bad'])add('resolve-'+status+'-stale','Resolve',{status,setup:(db,def)=>{def.id=R.record(request()).candidateId}})
add('update-empty-visible','Update',{text:'<think>secret</think>',setup:(db,def)=>{blueprint(db);def.id=R.record(request()).candidateId}})
const cases=[]
function normalize(value,ids){if(Array.isArray(value))return value.map(v=>normalize(v,ids));if(value&&typeof value==='object')return Object.fromEntries(Object.entries(value).filter(([k])=>!['created_at','resolved_at','createdAt','resolvedAt'].includes(k)).map(([k,v])=>[k,normalize(v,ids)]));if(typeof value==='string'&&ids.has(value))return ids.get(value);return value}
try{const bytes=readFileSync(baseline);for(const def of definitions){const file=path.join(directory,def.name+'.db'),fd=openSync(file,'wx',0o600);try{writeFileSync(fd,bytes)}finally{closeSync(fd)}const db=new Database(file);try{db.pragma('foreign_keys=ON');globalThis[key]=db;def.setup?.(db,def);const before=db.serialize();let result=null,error;try{switch(def.method){case'Record':result=R.record(def.input??request());break;case'List':result=R.listPending();break;case'Update':result=R.updatePending(def.id,def.text);break;case'Resolve':R.resolve(def.id,def.status);break;default:assert.fail(def.method)}}catch(e){assert(e instanceof Error);error=e.message;assert(!error.startsWith('UNREACHABLE_'))}const rows=db.prepare('SELECT * FROM recovery_candidates ORDER BY rowid').all();const ids=new Map(rows.map((r,i)=>[r.candidate_id,`candidate-${i}`]));const fields={name:def.name,method:def.method,input:def.input??(def.method==='Record'?request():undefined),id:def.id,text:def.text,status:def.status,result:normalize(result,ids),after:normalize(rows,ids),error};const beforePath=path.join(directory,def.name+'-before.db');writeFileSync(beforePath,before,{flag:'wx',mode:0o600});cases.push(fields)}finally{globalThis[key]=null;db.close()}};assert(cases.length>=50);writeFileSync(target,JSON.stringify({format:1,cases,originalModules:[...originalModules].sort()}),{flag:'wx',mode:0o600});console.error(`Original RecoveryCandidateRepository emitted ${cases.length} cases using actual recursive modules; sourceCurrent draft identity only, not body CAS`)}finally{hooks.deregister();delete globalThis[key]}
