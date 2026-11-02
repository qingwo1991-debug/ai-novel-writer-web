import assert from 'node:assert/strict'
import { createHash } from 'node:crypto'
import fs from 'node:fs'
import path from 'node:path'
import { stripTypeScriptTypes } from 'node:module'
import { runInNewContext } from 'node:vm'

// Only extract the original local copy reader/current predicate. Never import
// vector-store's connection pool or native runtimes, never access author input.
assert.equal(process.env.GITHUB_ACTIONS, 'true')
const out=process.env.GO_KNOWLEDGECOPY_CONTRACT
assert(out&&process.env.RUNNER_TEMP&&path.isAbsolute(out))
assert.equal(out,path.join(process.env.RUNNER_TEMP,'knowledgecopy-contract.json'))
const source=fs.readFileSync(new URL('../../electron/vector-store.ts',import.meta.url),'utf8')
const names=['knowledgeCopyPath','readKnowledgeCopy','isDocumentCopyIndexCurrent']
const extracted=names.map(name=>{
 const marker=new RegExp(`^(?:export )?function ${name}\\(`,'m').exec(source)
 assert(marker,`original ${name} missing`)
 const end=source.indexOf('\n}',marker.index)
 assert(end>marker.index)
 return source.slice(marker.index,end+2).replace(/^export /,'')
}).join('\n')
const hash= /^const contentHash = .+$/m.exec(source)?.[0]
assert(hash)
const js=stripTypeScriptTypes(`${hash}\n${extracted}`,{mode:'transform'})
const root=fs.mkdtempSync(path.join(process.env.RUNNER_TEMP,'knowledgecopy-original-'))
fs.mkdirSync(path.join(root,'knowledge-copies'),{mode:0o700})
const original=runInNewContext(`${js}\n({readKnowledgeCopy,isDocumentCopyIndexCurrent})`,{fs,path,createHash,KNOWLEDGE_COPY_MARKER:'knowledge-copy:'},{timeout:1000})
const sha=value=>createHash('sha256').update(value,'utf8').digest('hex')
const content="Synthetic café / e\u0301 / 星海🙂\nO'Brien\tΩ"
const inputs=[
 {name:'current',copy:{content,indexedHash:sha(content),edited:false,indexDirty:false}},
 {name:'edited-but-current',copy:{content,indexedHash:sha(content),edited:true,indexDirty:false}},
 {name:'dirty-but-hash-matches',copy:{content,indexedHash:sha(content),edited:true,indexDirty:true}},
 {name:'hash-mismatch',copy:{content,indexedHash:'old',edited:false,indexDirty:false}},
 {name:'dirty-and-mismatch',copy:{content,indexedHash:'',edited:true,indexDirty:true}},
 {name:'empty-current',copy:{content:'',indexedHash:sha(''),edited:false,indexDirty:false}},
 {name:'extra-fields-preserved',copy:{content,indexedHash:sha(content),edited:false,indexDirty:false,unknown:{valid:'preserved'}}},
]
const cases=inputs.map(({name,copy},i)=>{
 const id=`synthetic-doc-${i}`
 const raw=`\n${JSON.stringify(copy,null,2)}\n\n`
 const file=path.join(root,'knowledge-copies',`${sha(id)}.json`)
 fs.writeFileSync(file,raw,{flag:'wx',mode:0o600})
 const value=original.readKnowledgeCopy(root,id)
 assert.deepEqual(JSON.parse(JSON.stringify(value)),copy)
 const current=original.isDocumentCopyIndexCurrent(root,id,'knowledge-copy:'+id)
 return {name,raw,current,dirty:copy.indexDirty,edited:copy.edited,hashMismatch:sha(copy.content)!==copy.indexedHash,rawSHA256:sha(raw)}
})
const linkInputs=[
 {name:'managed-current-unicode',id:'doc-星海🙂',filePath:'knowledge-copy:doc-星海🙂',copy:inputs[0].copy},
 {name:'managed-suffix-ignored',id:'doc-suffix',filePath:'knowledge-copy:OTHER-ID',copy:inputs[1].copy},
 {name:'managed-empty-suffix',id:'doc-empty',filePath:'knowledge-copy:',copy:inputs[0].copy},
 {name:'managed-missing',id:'doc-missing',filePath:'knowledge-copy:doc-missing',copy:null},
 {name:'managed-dirty',id:'doc-dirty',filePath:'knowledge-copy:doc-dirty',copy:inputs[2].copy},
 {name:'managed-mismatch',id:'doc-mismatch',filePath:'knowledge-copy:doc-mismatch',copy:inputs[3].copy},
 {name:'unmanaged-no-file',id:'doc-unmanaged',filePath:'/synthetic/source.pdf',copy:null},
 {name:'unmanaged-extra-copy',id:'doc-extra',filePath:'plain.pdf',copy:inputs[0].copy},
 {name:'marker-not-prefix',id:'doc-not-prefix',filePath:'xknowledge-copy:doc-not-prefix',copy:null},
]
const linkCases=linkInputs.map(({name,id,filePath,copy})=>{const raw=copy===null?null:`\n${JSON.stringify(copy,null,2)}\n`;if(raw!==null)fs.writeFileSync(path.join(root,'knowledge-copies',`${sha(id)}.json`),raw,{flag:'wx',mode:0o600});const managed=filePath.startsWith('knowledge-copy:');const current=original.isDocumentCopyIndexCurrent(root,id,filePath);return {name,id,filePath,raw,managed,current}})
fs.writeFileSync(out,JSON.stringify({format:1,cases,linkCases})+'\n',{flag:'wx',mode:0o600})
fs.rmSync(root,{recursive:true}) // only this attempt-owned synthetic oracle dir
console.log(`Original copy reader/current predicate emitted ${cases.length} cases`)
