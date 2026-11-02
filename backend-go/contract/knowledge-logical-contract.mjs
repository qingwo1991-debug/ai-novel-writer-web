// CI-only actual exported-module oracle. No private helper extraction, source
// rewriting, fake Arrow type strings, Node Lance install, or business mocks.
import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'
import crypto from 'node:crypto'
import * as nodeModule from 'node:module'
import {SourceTextModule, SyntheticModule, createContext} from 'node:vm'
import {fileURLToPath, pathToFileURL} from 'node:url'
assert.equal(process.env.GITHUB_ACTIONS,'true')
const root=process.env.RUNNER_TEMP
assert(root&&path.isAbsolute(root))
const out=process.env.GO_KNOWLEDGE_LOGICAL_CONTRACT
assert.equal(out,path.join(root,'novel-go-knowledge-logical','contract.json'))
const require=nodeModule.createRequire(path.join(root,'novel-go-oracle','package.json'))
const arrow=require('apache-arrow')
const locale=new Intl.Collator().resolvedOptions().locale
assert.equal(locale,'en-US','this bounded Go lane requires an independently verified en-US Node default')
const hash=bytes=>crypto.createHash('sha256').update(bytes).digest('hex')
const temp=fs.mkdtempSync(path.join(root,'novel-go-logical-original-'))
const context=createContext({Buffer,process,console,TextEncoder,TextDecoder,setTimeout,clearTimeout,URL,Uint8Array,ArrayBuffer})
const cache=new Map(), tablesByPath=new Map()
let connects=0
const lance={async connect(directory){connects++;const tables=tablesByPath.get(directory);assert(tables,'connect must use a pre-created synthetic table directory');return {async tableNames(){return [...tables.keys()]},async openTable(name){const table=tables.get(name);assert(table);return {async version(){return 1},async checkout(version){assert.equal(version,1)},query(){let selected;return {select(fields){selected=fields;return this},async toArrow(){assert(!selected);return table},async toArray(){return table.toArray().map(row=>Object.fromEntries((selected??table.schema.fields.map(f=>f.name)).map(k=>[k,row[k]])))}}},close(){}}},close(){}}}}
function namespace(value,key){const names=Object.keys(value);if(!names.includes('default'))names.push('default');return new SyntheticModule(names,function(){for(const name of names)this.setExport(name,name==='default'&&!(name in value)?value:value[name])},{context,identifier:key})}
async function load(identifier){
 if(cache.has(identifier))return cache.get(identifier)
 let result
 if(identifier==='@lancedb/lancedb')result=namespace(lance,identifier)
 else if(identifier==='node:module')result=namespace({...nodeModule,createRequire:()=>require},identifier)
 else if(identifier.startsWith('node:'))result=namespace(await import(identifier),identifier)
 else if(!identifier.startsWith('file:'))result=namespace(require(identifier),identifier)
 else {const source=fs.readFileSync(fileURLToPath(identifier),'utf8');result=new SourceTextModule(nodeModule.stripTypeScriptTypes(source,{mode:'transform'}),{context,identifier,initializeImportMeta(meta){meta.url=identifier}})}
 cache.set(identifier,result)
 return result
}
async function link(specifier,referencing){if(specifier.startsWith('.')){const url=new URL(specifier,referencing.identifier);if(!path.extname(url.pathname))url.pathname+='.ts';return load(url.href)}return load(specifier)}
const migration=await load(new URL('../../electron/services/vector-migration-snapshot.ts',import.meta.url).href)
await migration.link(link)
await migration.evaluate()
const vector=await load(new URL('../../electron/vector-store.ts',import.meta.url).href)
await vector.link(link)
await vector.evaluate()
const field=(name,type='utf8',nullable=false,listSize=0,children=[])=>({name,type,nullable,listSize,metadata:false,children})
const docSchema=[field('id'),field('fileName'),field('importedAt'),field('chunkCount','float64'),field('filePath'),field('corpusKind')]
const chunkSchema=[field('text'),field('id'),field('docId'),field('fileName'),field('chapterNumber','float64',true),field('chapterTitle','utf8',true),field('chunkIndex','int32'),field('totalChunks','int32'),field('importedAt'),field('corpusKind')]
function type(f){switch(f.type){case'utf8':return new arrow.Utf8();case'int32':return new arrow.Int32();case'float32':return new arrow.Float32();case'float64':return new arrow.Float64();case'fixed_size_list':return new arrow.FixedSizeList(f.listSize,arrowField(f.children[0]));default:throw Error('unsupported synthetic type')}}
function arrowField(f){return new arrow.Field(f.name,type(f),f.nullable,new Map())}
function tableOf(t){const fields=t.schema.map(arrowField),schema=new arrow.Schema(fields);for(let i=0;i<fields.length;i++){const expected={utf8:'Utf8',int32:'Int32',float32:'Float32',float64:'Float64',fixed_size_list:`FixedSizeList[${t.schema[i].listSize}]<Float32>`}[t.schema[i].type];assert.equal(fields[i].type.toString(),expected,'real Arrow type spelling authority')}if(!t.records.length)return new arrow.Table(schema);const children=fields.map(f=>arrow.vectorFromArray(t.records.map(r=>r[f.name]),f.type).data[0]);return new arrow.Table(new arrow.RecordBatch(schema,arrow.makeData({type:new arrow.Struct(fields),length:t.records.length,children})))}
const ids=['ffffffff-ffff-4fff-bfff-ffffffffffff','00000000-0000-4000-8000-000000000000','a'.repeat(64),'finalization-'+ 'b'.repeat(64),'portable:'+ 'c'.repeat(64)]
// Prove the entire generated alphabet/separator ordering used by this fixture,
// not a claim that the Go comparator implements arbitrary ICU Unicode sorting.
const generated=[...ids,...'0123456789abcdef'.split('').map(c=>c.repeat(64))]
assert.deepEqual([...generated].sort((a,b)=>a.localeCompare(b)),[...generated].sort())
const base=()=>({tables:[{name:'documents',version:1,metadata:false,schema:structuredClone(docSchema),records:[{id:ids[0],fileName:'合成 café🙂.txt',importedAt:'synthetic',chunkCount:2,filePath:'knowledge-copy:ignored',corpusKind:'reference'}]},{name:'chunks',version:1,metadata:false,schema:structuredClone(chunkSchema),records:[1,0].map(i=>({id:ids[i+1],docId:ids[0],fileName:'合成 café🙂.txt',text:`文字e\u0301 é ﹤&> \" \\ / \b\f\n\r\t\u0000\u001f\u2028\u2029🙂`,chapterNumber:i?-0:1e-7,chapterTitle:null,chunkIndex:i,totalChunks:2,importedAt:'synthetic',corpusKind:'reference'}))}],registry:null})
const cases=[]
async function add(name,mutate=()=>{},registryRaw=null,goAccepted=true){const snapshot=base();mutate(snapshot);const dir=path.join(temp,name),source=path.join(dir,'source'),copy=path.join(dir,'copy');fs.mkdirSync(dir,{mode:0o700});for(const entry of [source,copy]){fs.mkdirSync(entry,{mode:0o700});fs.mkdirSync(path.join(entry,'lancedb'),{mode:0o700});if(registryRaw!==null)fs.writeFileSync(path.join(entry,'embedding-spaces.json'),registryRaw,{mode:0o600,flag:'wx'})}const actualTables=new Map(snapshot.tables.map(t=>[t.name,tableOf(t)]));tablesByPath.set(path.join(copy,'lancedb'),actualTables);const result=await migration.namespace.exportVectorStoreForMigration({storageRoot:copy,originalSourceRoot:source});assert.equal(result.summary.tableCount,snapshot.tables.length);cases.push({name,snapshot,registryRaw,goAccepted,summary:result.summary,tables:result.tables.map(t=>({name:t.name,logicalHash:t.logicalHash}))})}
await add('canonical-schema-order-unicode-controls')
await add('empty',s=>{s.tables=[]})
await add('registry-whitespace',()=>{},' {"version":1,"spaces":[],"activeGeneration":null} \n')
await add('registry-compact',()=>{},'{"version":1,"spaces":[],"activeGeneration":null}')
await add('single-arbitrary-unicode-id',s=>{s.tables[1].records=s.tables[1].records.slice(0,1);s.tables[1].records[0].id='𝄞e\u0301星';s.tables[1].records[0].chunkIndex=0;s.tables[1].records[0].totalChunks=1;s.tables[0].records[0].chunkCount=1})
await add('multiple-unicode-id-explicitly-unsupported',s=>{s.tables[1].records[0].id='ä';s.tables[1].records[1].id='z'},null,false)
for(const n of [-0,1e-6,1e-7,1e20,1e21,Number.MAX_VALUE,Number.MIN_VALUE,9007199254740992,9007199254740993,333333333.3333333,1000000000000000100,-1e-7])await add('number-'+cases.length,s=>{s.tables[1].records[0].chapterNumber=n})
await add('schema-reordered',s=>{s.tables.forEach(t=>t.schema.reverse())})
await add('table-order-reversed',s=>{s.tables.reverse()})
await add('empty-vector-table',s=>{s.tables.push({name:'chunks__space_2',version:99,metadata:false,schema:[...structuredClone(chunkSchema),field('vector','fixed_size_list',false,4,[field('item','float32',true)])],records:[]})},JSON.stringify({version:1,activeGeneration:null,spaces:[{generation:2,tableName:'chunks__space_2',status:'building',modelFingerprint:'synthetic',createdAt:'synthetic',distanceMetric:'cosine',vectorDimension:4}]}))
await add('active-float32-rounded',s=>{s.tables.push({name:'chunks__space_1',version:2,metadata:false,schema:[...structuredClone(chunkSchema),field('vector','fixed_size_list',false,4,[field('item','float32',true)])],records:s.tables[1].records.map(r=>({...r,vector:[0.1,-0,1.401298464324817e-45,3.4028234663852886e38]}))})},JSON.stringify({version:1,activeGeneration:1,spaces:[{generation:1,tableName:'chunks__space_1',status:'active',modelFingerprint:'synthetic',createdAt:'synthetic',distanceMetric:'dot',vectorDimension:4}]}))
const portable=[]
const portableBase=()=>({version:1,documents:[{docId:'任意单文档𝄞',fileName:'ＡＢＣ🙂.txt',corpusKind:'reference',chunks:[{chunkIndex:0,text:'原始e\u0301<&>\u2028\u2029🙂'}],copy:{content:'副本\u0000\n<&>🙂',indexedHash:hash('副本\u0000\n<&>🙂'),edited:true,indexDirty:false}}]})
function addPortable(name,mutate=()=>{},goAccepted=true){const value=portableBase();mutate(value);let bytes=null,accepted=true;try{bytes=vector.namespace.serializePortableKnowledgeSnapshot(value).toString('utf8')}catch(e){assert.equal(e.message,'PORTABLE_KNOWLEDGE_INVALID');accepted=false}portable.push({name,value,originalAccepted:accepted,goAccepted:accepted&&goAccepted,bytes})}
addPortable('copy-unicode-controls')
addPortable('dirty-copy',s=>{s.documents[0].copy.indexDirty=true;s.documents[0].copy.indexedHash='a'.repeat(64)})
addPortable('hash-mismatch',s=>{s.documents[0].copy.indexedHash='a'.repeat(64)})
addPortable('no-copy',s=>{delete s.documents[0].copy})
addPortable('utf16-512-filename',s=>{s.documents[0].fileName='🙂'.repeat(256)})
addPortable('utf16-514-filename',s=>{s.documents[0].fileName='🙂'.repeat(257)})
for(const fileName of ['ＣＯＮ.txt','ｃｏｍ１.txt','foo／bar.txt','foo＼bar.txt','foo：bar.txt','foo．','foo\u00a0','x\u0001.txt'])addPortable('nfkc-reject-'+portable.length,s=>{s.documents[0].fileName=fileName})
addPortable('decomposed-name-preserved',s=>{s.documents[0].fileName='e\u0301.txt'})
addPortable('empty-chunks',s=>{s.documents[0].chunks=[]})
addPortable('negative-zero-index',s=>{s.documents[0].chunks[0].chunkIndex=-0})
addPortable('unsafe-index',s=>{s.documents[0].chunks[0].chunkIndex=9007199254740992})
addPortable('empty-copy-content',s=>{s.documents[0].copy.content='';s.documents[0].copy.indexedHash=hash('')})
addPortable('multiple-generated-docs',s=>{s.documents=ids.map(docId=>({...structuredClone(s.documents[0]),docId})).reverse()})
addPortable('multiple-unicode-docs-unsupported',s=>{s.documents.push({...structuredClone(s.documents[0]),docId:'ä'})},false)
assert(connects>=cases.length)
assert.notEqual(cases.find(c=>c.name==='registry-whitespace').summary.logicalHash,cases.find(c=>c.name==='registry-compact').summary.logicalHash)
fs.mkdirSync(path.dirname(out),{mode:0o700,recursive:true})
fs.writeFileSync(out,JSON.stringify({format:1,locale,oracle:'whole-original-exported-modules-real-Arrow-table-only-Lance-seam',cases,portable})+'\n',{mode:0o600,flag:'wx'})
fs.rmSync(temp,{recursive:true})
console.log(`Actual exported original modules emitted ${cases.length} Arrow logical hashes and ${portable.length} portable cases; arbitrary multi-Unicode collation explicitly unsupported`)
