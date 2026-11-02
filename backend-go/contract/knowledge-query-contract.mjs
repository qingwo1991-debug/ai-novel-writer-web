import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'
import {createHash} from 'node:crypto'
import {stripTypeScriptTypes} from 'node:module'
import {runInNewContext} from 'node:vm'
assert.equal(process.env.GITHUB_ACTIONS,'true')
const out=process.env.GO_KNOWLEDGEQUERY_CONTRACT
assert.equal(out,path.join(process.env.RUNNER_TEMP,'knowledge-query-contract.json'))
const vector=fs.readFileSync(new URL('../../electron/vector-store.ts',import.meta.url),'utf8'),kb=fs.readFileSync(new URL('../../electron/knowledge-base.ts',import.meta.url),'utf8')
function extract(source,name){const match=new RegExp(`^(?:export )?(?:async )?function ${name}\\(`,'m').exec(source);assert(match,name);const end=source.indexOf('\n}\n',match.index);assert(end>match.index);return source.slice(match.index,end+2).replace(/^export /,'')}
const names=['asNumberArray','vectorIsUsable','activeSpace','backfillSpaceForIdentity','vectorisedIdsForSpace','missingCanonicalRows','getChunksWithoutVectorsInternal','getStatsInternal','listDocumentsInternal','knowledgeCopyPath','readKnowledgeCopy']
const code=stripTypeScriptTypes(names.map(n=>extract(vector,n)).join('\n')+'\n'+extract(kb,'readDocumentCopy'),{mode:'transform'})
const temp=fs.mkdtempSync(path.join(process.env.RUNNER_TEMP,'knowledge-query-original-'))
const hash=text=>createHash('sha256').update(text,'utf8').digest('hex')
const field=(name,type='utf8',more={})=>({name,type,nullable:false,listSize:0,metadata:false,children:[],...more})
const docFields=[field('id'),field('fileName'),field('importedAt'),field('chunkCount','int32'),field('filePath'),field('corpusKind')]
const chunkFields=[field('id'),field('docId'),field('fileName'),field('text'),field('chunkIndex','int32'),field('totalChunks','int32'),field('importedAt'),field('corpusKind')]
const doc=(id,count=1,managed=true)=>({id,fileName:`${id}.txt`,importedAt:'2026-01-01',chunkCount:count,filePath:managed?'knowledge-copy:ignored-suffix':'/synthetic/file.txt',corpusKind:'reference'})
const chunk=(document,i,total)=>({id:`${document.id}-chunk-${i}`,docId:document.id,fileName:document.fileName,text:`正文 ${i} Ω🙂\n<&>`,chunkIndex:i,totalChunks:total,importedAt:document.importedAt,corpusKind:document.corpusKind})
const table=(name,schema,records)=>({name,version:1,metadata:false,schema,records})
const emptyRegistry={version:1,activeGeneration:null,spaces:[]}
const caseNames=['empty','managed-current','edited-current','dirty-current-hash','mismatch','missing-managed','unmanaged-copy','empty-copy','zero-chunk-document','building-partial','active-full','building-first-of-two','no-corpus-columns','no-chunks-table']
const cases=[]
for(const name of caseNames){const directory=path.join(temp,name);fs.mkdirSync(directory,{mode:0o700});fs.mkdirSync(path.join(directory,'knowledge-copies'),{mode:0o700});const d=doc("doc-星'🙂",name==='zero-chunk-document'||name==='no-chunks-table'?0:2,name!=='unmanaged-copy');let docs=name==='empty'?[]:[d],chunks=docs.flatMap(x=>Array.from({length:x.chunkCount},(_,i)=>chunk(x,i,x.chunkCount)));let tables=[table('documents',docFields,docs),table('chunks',chunkFields,chunks)],registry=structuredClone(emptyRegistry);const copies=[]
 if(!['empty','missing-managed'].includes(name)){const content=name==='empty-copy'?'':"副本 café e\u0301 🙂\n<&>\u2028\u2029";const copy={content,indexedHash:name==='mismatch'?'old':hash(content),edited:['edited-current','dirty-current-hash'].includes(name),indexDirty:name==='dirty-current-hash',unknown:{preserved:true}};const raw=JSON.stringify(copy);fs.writeFileSync(path.join(directory,'knowledge-copies',hash(d.id)+'.json'),raw,{flag:'wx',mode:0o600});copies.push({id:d.id,raw})}
 if(['building-partial','active-full','building-first-of-two'].includes(name)){for(let gen=1;gen<=(name==='building-first-of-two'?2:1);gen++){const status=name==='active-full'?'active':'building',space={generation:gen,tableName:`chunks__space_${gen}`,status,vectorDimension:2,modelFingerprint:'synthetic-model',createdAt:'synthetic-time',distanceMetric:'cosine'};registry.spaces.push(space);if(status==='active')registry.activeGeneration=gen;tables.push(table(space.tableName,[...chunkFields,field('vector','fixed_size_list',{listSize:2,children:[field('item','float32',{nullable:true})]})],chunks.slice(0,status==='active'?chunks.length:gen===1?1:2).map(row=>({...row,vector:[0.25,0.5]}))))}}
 if(name==='no-corpus-columns'){tables=tables.map(t=>({...t,schema:t.schema.filter(f=>f.name!=='corpusKind'),records:t.records.map(row=>{const v={...row};delete v.corpusKind;return v})}));docs=tables.find(t=>t.name==='documents').records}
 if(name==='no-chunks-table')tables=tables.filter(t=>t.name!=='chunks')
 if(name==='empty')tables=[]
 const byName=new Map(tables.map(t=>[t.name,t.records]));const db={async tableNames(){return [...byName.keys()]},async openTable(tableName){assert(byName.has(tableName));return {async countRows(){return byName.get(tableName).length},query(){let selected;return {select(fields){selected=fields;return this},async toArray(){return byName.get(tableName).map(row=>selected?Object.fromEntries(selected.map(k=>[k,row[k]])):{...row})}}}}}
 }
 const sandbox={fs,path,createHash,TABLE_NAME:'chunks',DOCS_TABLE_NAME:'documents',KNOWLEDGE_COPY_MARKER:'knowledge-copy:',getConnection:async()=>db,ensureCorpusKindColumn:async()=>{},loadOrRegisterLegacyRegistry:async()=>registry,normalizedIdentity:x=>x,getProjectDataRoot:x=>x,ensureMigration:async()=>{},withKnowledgeSourceGate:async(_r,fn)=>fn(),copyHash:hash,contentHash:hash}
 const original=runInNewContext(code+'\n({listDocumentsInternal,getStatsInternal,getChunksWithoutVectorsInternal,readDocumentCopy})',sandbox,{timeout:1000});sandbox.storeListDocuments=original.listDocumentsInternal
 const listed=await original.listDocumentsInternal(directory),stats=await original.getStatsInternal(directory),vectorless=await original.getChunksWithoutVectorsInternal(directory);const reads=[];for(const id of [d.id,'absent-doc'])reads.push({id,result:await original.readDocumentCopy(id,directory)})
 cases.push({name,snapshot:{tables,registry},copies,documents:listed,stats:{documentCount:stats.documentCount,totalChunks:stats.totalChunks,vectorDimension:stats.vectorDimension},vectorless,reads})
}
fs.writeFileSync(out,JSON.stringify({format:1,cases})+'\n',{flag:'wx',mode:0o600});fs.rmSync(temp,{recursive:true});console.log(`Original knowledge query functions emitted ${cases.length} complete query cases`)
