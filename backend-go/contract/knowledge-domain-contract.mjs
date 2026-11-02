import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'
import { createHash } from 'node:crypto'
import { createRequire, stripTypeScriptTypes } from 'node:module'
import { runInNewContext } from 'node:vm'

assert.equal(process.env.GITHUB_ACTIONS,'true')
const root=process.env.RUNNER_TEMP
assert(root&&path.isAbsolute(root))
const destination=process.env.GO_KNOWLEDGEDOMAIN_CONTRACT
assert.equal(destination,path.join(root,'knowledge-domain-contract.json'))
const require=createRequire(path.join(root,'novel-go-oracle','package.json'))
const arrow=require('apache-arrow')
const source=fs.readFileSync(new URL('../../electron/services/vector-migration-snapshot.ts',import.meta.url),'utf8')
function extract(name){const marker=new RegExp(`^function ${name}\\(`,'m').exec(source);assert(marker,`missing original ${name}`);const end=source.indexOf('\n}',marker.index);assert(end>marker.index);return source.slice(marker.index,end+2)}
const originalJS=stripTypeScriptTypes(['plain','rows','fieldSchema','tableHash','validateTables'].map(extract).join('\n'),{mode:'transform'})
const digest=value=>createHash('sha256').update(value).digest('hex')
const original=runInNewContext(`${originalJS}\n({tableHash,validateTables})`,{tableFromIPC:arrow.tableFromIPC,digest,fail:code=>{throw new Error(code)},Buffer},{timeout:1000})
const field=(name,type,nullable=false,listSize=0,children=[])=>({name,type,nullable,listSize,metadata:false,children})
const chunkFields=()=>[field('id','utf8'),field('docId','utf8'),field('fileName','utf8'),field('text','utf8'),field('chunkIndex','int32'),field('totalChunks','int32'),field('importedAt','utf8'),field('corpusKind','utf8')]
const vectorField=()=>field('vector','fixed_size_list',false,2,[field('item','float32',true)])
const chunk=(id,index)=>({id,docId:'doc-Ω',fileName:'合成🙂',text:`Synthetic ${id} café\n`,chunkIndex:index,totalChunks:2,importedAt:'not-a-date-original-allows',corpusKind:'reference'})
const chunks=[chunk('a',0),chunk('é',1)]
const base={tables:[
 {name:'documents',version:1,metadata:false,schema:[field('id','utf8',true),field('fileName','utf8',true),field('importedAt','utf8',true),field('chunkCount','float64',true),field('filePath','utf8',true),field('corpusKind','utf8',true)],records:[{id:'doc-Ω',fileName:'合成🙂',importedAt:'not-a-date-original-allows',chunkCount:2,filePath:'knowledge-copy:doc-Ω',corpusKind:'reference'}]},
 {name:'chunks',version:1,metadata:false,schema:chunkFields(),records:chunks},
 {name:'chunks__space_1',version:2,metadata:false,schema:[...chunkFields(),vectorField()],records:chunks.map((r,i)=>({...r,vector:i?[3.125,-2.5]:[0.5,1]}))},
 {name:'chunks__space_2',version:1,metadata:false,schema:[...chunkFields(),vectorField()],records:[{...chunks[0],vector:[-1,0]}]},
 {name:'chunks__space_3',version:1,metadata:false,schema:[...chunkFields(),vectorField()],records:[]}
],registry:{version:1,activeGeneration:1,spaces:[['active',1],['inactive',2],['building',3]].map(([status,generation])=>({status,generation,tableName:`chunks__space_${generation}`,modelFingerprint:'synthetic',vectorDimension:2,distanceMetric:'l2',createdAt:'string-not-date'}))}}
function typeOf(f){switch(f.type){case 'utf8':return new arrow.Utf8();case 'int32':return new arrow.Int32();case 'float32':return new arrow.Float32();case 'float64':return new arrow.Float64();case 'fixed_size_list':return new arrow.FixedSizeList(f.listSize,arrowField(f.children[0]));default:throw Error('unsupported synthetic field')}}
function arrowField(f){return new arrow.Field(f.name,typeOf(f),f.nullable,f.metadata?new Map([['synthetic','metadata']]):new Map())}
function tableOf(table){const fields=table.schema.map(arrowField);const schema=new arrow.Schema(fields,table.metadata?new Map([['synthetic','metadata']]):new Map());if(!table.records.length)return new arrow.Table(schema);const children=fields.map(f=>arrow.vectorFromArray(table.records.map(r=>r[f.name]),f.type).data[0]);const data=arrow.makeData({type:new arrow.Struct(fields),length:table.records.length,children});return new arrow.Table(new arrow.RecordBatch(schema,data))}
const cases=[]
function add(name,mutate,expected,goAccepted=expected){const snapshot=structuredClone(base);mutate(snapshot);const tables=snapshot.tables.map(t=>{const table=tableOf(t);return {name:t.name,arrowIpc:arrow.tableToIPC(table,'stream'),rowCount:table.numRows,logicalHash:t.schema.some(f=>f.metadata||f.children.some(c=>c.metadata))?'unreached-metadata-rejection':original.tableHash(t.name,table)}});let accepted=true,code='';try{original.validateTables({tables,registryBytes:snapshot.registry===null?null:Buffer.from(JSON.stringify(snapshot.registry))})}catch(e){accepted=false;code=e.message}assert.equal(accepted,expected,`original result ${name}: ${code}`);cases.push({name,snapshot,originalAccepted:accepted,goAccepted,originalCode:code})}
add('valid-active-inactive-empty-building',()=>{},true)
add('no-registry-no-spaces',s=>{s.registry=null;s.tables=s.tables.slice(0,2)},true)
add('all-assets-empty',s=>{s.registry=null;s.tables=[]},true)
add('zero-chunk-document',s=>{s.tables=s.tables.slice(0,2);s.tables[0].records[0].chunkCount=0;s.tables[1].records=[];s.registry=null},true)
add('orphan-canonical',s=>{s.tables[1].records[0].docId='missing'},false)
add('document-count',s=>{s.tables[0].records[0].chunkCount=3},false)
add('chunk-index-duplicate',s=>{s.tables[1].records[1].chunkIndex=0},false)
add('chunk-total',s=>{s.tables[1].records[0].totalChunks=1},false)
add('duplicate-id',s=>{s.tables[1].records[1].id='a'},false)
add('space-doc-drift',s=>{s.tables[2].records[0].docId='other'},false)
add('space-text-drift',s=>{s.tables[3].records[0].text='changed'},false)
add('active-incomplete',s=>{s.tables[2].records.pop()},false)
add('second-active',s=>{s.registry.spaces[1].status='active'},false)
add('active-pointer',s=>{s.registry.activeGeneration=2},false)
add('registry-dimension',s=>{s.registry.spaces[0].vectorDimension=3},false)
add('unregistered-space',s=>{s.registry.spaces.pop()},false)
add('unknown-field',s=>{s.tables[1].schema.push(field('unknown','utf8'));s.tables[1].records.forEach(r=>r.unknown='x')},false)
add('missing-required-field',s=>{s.tables[1].schema=s.tables[1].schema.filter(f=>f.name!=='text');s.tables[1].records.forEach(r=>delete r.text)},false)
add('table-metadata',s=>{s.tables[1].metadata=true},false)
add('field-metadata',s=>{s.tables[1].schema[0].metadata=true},false)
add('vector-child-metadata',s=>{s.tables[2].schema.find(f=>f.name==='vector').children[0].metadata=true},false)
// Original domain validates text values but not empty-table scalar schema types.
add('stricter-empty-field-type',s=>{s.tables[4].schema.find(f=>f.name==='text').type='float64'},true,false)
fs.writeFileSync(destination,JSON.stringify({format:1,cases})+'\n',{flag:'wx',mode:0o600})
console.log(`Original validateTables/tableHash emitted ${cases.length} synthetic domain cases; not a Go logicalHash parity claim`)
