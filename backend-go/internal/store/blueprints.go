package store

import (
 "context"
 "crypto/sha256"
 "database/sql"
 "encoding/hex"
 "encoding/json"
 "errors"
 "io"
 "math"
 "sort"
 "strconv"
 "strings"
)

var (
 ErrBlueprintOperationIDRequired = errors.New("BLUEPRINT_OPERATION_ID_REQUIRED")
 ErrBlueprintCommitModeInvalid = errors.New("BLUEPRINT_COMMIT_MODE_INVALID")
 ErrBlueprintCommitRangeInvalid = errors.New("BLUEPRINT_COMMIT_RANGE_INVALID")
 ErrBlueprintCommitRangeIncomplete = errors.New("BLUEPRINT_COMMIT_RANGE_INCOMPLETE")
 ErrBlueprintOperationConflict = errors.New("BLUEPRINT_OPERATION_CONFLICT")
 ErrBlueprintCommitReceiptInvalid = errors.New("BLUEPRINT_COMMIT_RECEIPT_INVALID")
 ErrBlueprintCommitReadBackFailed = errors.New("BLUEPRINT_COMMIT_READ_BACK_FAILED")
 ErrBlueprintCharacterSyncOperationIDRequired = errors.New("BLUEPRINT_CHARACTER_SYNC_OPERATION_ID_REQUIRED")
 ErrBlueprintCharacterSyncOperationMissing = errors.New("BLUEPRINT_CHARACTER_SYNC_OPERATION_MISSING")
 ErrBlueprintCharacterSyncInputInvalid = errors.New("BLUEPRINT_CHARACTER_SYNC_INPUT_INVALID")
 ErrBlueprintCharacterSyncReceiptInvalid = errors.New("BLUEPRINT_CHARACTER_SYNC_RECEIPT_INVALID")
 ErrBlueprintCharacterSyncStateInvalid = errors.New("BLUEPRINT_CHARACTER_SYNC_STATE_INVALID")
 ErrBlueprintCommitCanonicalKeyUnsupported = errors.New("BLUEPRINT_COMMIT_CANONICAL_KEY_UNSUPPORTED")
)

// Blueprints borrows an admitted baseline database. No DDL, generation recovery
// retirement, project clear, or character-sync completion authority lives here.
// Inbound maps have already lost JavaScript property insertion order: characters
// objects are serialized deterministically by plotJSON, not claimed byte-exact
// JSON.stringify for arbitrary input object order. Arrays/scalars retain JS JSON
// spelling. Reads accept every JSON value, not only the annotated string array.
type Blueprints struct{ db *sql.DB }
func NewBlueprints(db *sql.DB) *Blueprints { return &Blueprints{db: db} }

const blueprintColumns = `chapter_number AS chapterNumber,title,role,purpose,
 key_events AS keyEvents,characters,suspense_hook AS suspenseHook,
 user_guidance AS userGuidance,notes,notes_updated_at AS notesUpdatedAt`

func blueprintParseJSON(value any) (any, bool) {
 var parsed any
 decoder := json.NewDecoder(strings.NewReader(narrativeJoinText(value)))
 decoder.UseNumber()
 if err := decoder.Decode(&parsed); err != nil { return nil, false }
 var tail any
 if err := decoder.Decode(&tail); err != io.EOF { return nil, false }
 return parsed, true
}

func blueprintRows(ctx context.Context, q rrQueryer, suffix string, args ...any) ([]map[string]any, error) {
 rows, err := rrMaps(ctx, q, "SELECT "+blueprintColumns+" FROM blueprints "+suffix, args...)
 if err != nil { return nil, err }
 for _, row := range rows {
  for _, key := range []string{"role","purpose","keyEvents","suspenseHook","userGuidance","notes","notesUpdatedAt"} {
   if row[key] == nil { row[key] = "" }
  }
  parsed, ok := blueprintParseJSON(row["characters"])
  if !ok { parsed = []any{} }
  row["characters"] = parsed
 }
 return rows, nil
}
func (b *Blueprints) All(ctx context.Context) ([]map[string]any, error) {
 if b == nil || b.db == nil { return nil, ErrProjectNotOpen }
 return blueprintRows(ctx,b.db,"ORDER BY chapter_number ASC")
}
// better-sqlite3 treats arrays as positional packs and objects as named binds.
// Anonymous-only SQL has no named slots; an object leaves the arity short.
func blueprintBindings(values []any,count int)([]any,error){
 out:=[]any{};var appendValue func(any)error;appendValue=func(value any)error{
  switch v:=value.(type){case []any:for _,item:=range v{if err:=appendValue(item);err!=nil{return err}};return nil;case map[string]any:return nil}
  if err:=narrativeBinding(value);err!=nil{return err};out=append(out,value);return nil
 };for _,value:=range values{if err:=appendValue(value);err!=nil{return nil,err}}
 if len(out)<count{return nil,errors.New("Too few parameter values were provided")};if len(out)>count{return nil,errors.New("Too many parameter values were provided")};return out,nil
}
func (b *Blueprints) Get(ctx context.Context, chapter any) (map[string]any, error) {
 if b == nil || b.db == nil { return nil, ErrProjectNotOpen }
 args,err:=blueprintBindings([]any{chapter},1);if err!=nil{return nil,err}
 rows, err := blueprintRows(ctx,b.db,"WHERE chapter_number=?",args...)
 if err != nil || len(rows) == 0 { return nil, err }
 return rows[0],nil
}

const blueprintUpsertSQL = `INSERT INTO blueprints(chapter_number,title,role,purpose,key_events,characters,
 suspense_hook,user_guidance,notes,notes_updated_at) VALUES(?,?,?,?,?,?,?,?,?,?)
 ON CONFLICT(chapter_number) DO UPDATE SET title=excluded.title,role=excluded.role,
 purpose=excluded.purpose,key_events=excluded.key_events,characters=excluded.characters,
 suspense_hook=excluded.suspense_hook,user_guidance=excluded.user_guidance,
 notes=excluded.notes,notes_updated_at=excluded.notes_updated_at,updated_at=datetime('now')`

func blueprintJSONValue(value any) any {
 // Extend the shared serializer to native equivalents of HTTP array/number
 // shapes, recursively (including characters nested in an immutable receipt).
 switch v:=value.(type){
 case int32:return int64(v)
 case []string:items:=make([]any,len(v));for i,item:=range v{items[i]=item};return items
 case []map[string]any:items:=make([]any,len(v));for i,item:=range v{items[i]=blueprintJSONValue(item)};return items
 case []any:items:=make([]any,len(v));for i,item:=range v{items[i]=blueprintJSONValue(item)};return items
 case map[string]any:out:=make(map[string]any,len(v));for key,item:=range v{out[key]=blueprintJSONValue(item)};return out
 }
 return value
}
func blueprintJSON(value any) (string,error) { return plotJSON(blueprintJSONValue(value)) }
func blueprintUpsert(ctx context.Context, conn interface{ ExecContext(context.Context,string,...any)(sql.Result,error) }, input map[string]any) error {
 var characters any
 if value,exists:=input["characters"]; exists { encoded,err:=blueprintJSON(value); if err!=nil{return err}; characters=encoded }
 args:=[]any{input["chapterNumber"],input["title"],input["role"],input["purpose"],input["keyEvents"],characters,
  input["suspenseHook"],input["userGuidance"],input["notes"],input["notesUpdatedAt"]}
 args,err:=blueprintBindings(args,10);if err!=nil{return err}
 _,err=conn.ExecContext(ctx,blueprintUpsertSQL,args...)
 return err
}
func (b *Blueprints) Upsert(ctx context.Context,input map[string]any) error {
 if b==nil||b.db==nil{return ErrProjectNotOpen}
 return blueprintUpsert(ctx,b.db,input)
}
func (b *Blueprints) UpsertMany(ctx context.Context,items []map[string]any) error {
 if b==nil||b.db==nil{return ErrProjectNotOpen}
 return NewReviewRevisions(b.db).immediate(ctx,func(conn *sql.Conn)error{
  for _,item:=range items { if err:=blueprintUpsert(ctx,conn,item);err!=nil{return err} };return nil
 })
}
func (b *Blueprints) Delete(ctx context.Context,chapter any) error {
 if b==nil||b.db==nil{return ErrProjectNotOpen}
 args,err:=blueprintBindings([]any{chapter},1);if err!=nil{return err}
 _,err=b.db.ExecContext(ctx,"DELETE FROM blueprints WHERE chapter_number=?",args...);return err
}

// This is the sole lazy repair: one atomic DML statement on already-admitted
// tables. It deliberately neither recreates missing schema nor retires runs.
const blueprintBackfillSQL = `INSERT OR IGNORE INTO blueprint_character_sync_operations(
 operation_id,blueprint_commit_operation_id,blueprint_commit_payload_hash,start_chapter,end_chapter,character_sync_input)
 SELECT 'blueprint-sync-'||operation_id,operation_id,payload_hash,start_chapter,end_chapter,character_sync_input
 FROM blueprint_commit_operations`
const blueprintSyncColumns = `operation_id AS operationId,blueprint_commit_operation_id AS blueprintCommitOperationId,
 blueprint_commit_payload_hash AS blueprintCommitPayloadHash,status,start_chapter AS startChapter,
 end_chapter AS endChapter,character_sync_input AS characterSyncInput,completion_receipt AS completionReceipt,
 created_at AS createdAt,updated_at AS updatedAt,completed_at AS completedAt`
func blueprintSyncRows(ctx context.Context,q rrQueryer,suffix string,args ...any)([]map[string]any,error){
 rows,err:=rrMaps(ctx,q,"SELECT "+blueprintSyncColumns+" FROM blueprint_character_sync_operations "+suffix,args...)
 if err!=nil{return nil,err}
 for _,row:=range rows {
  completion:=row["completionReceipt"]
  if completion!=nil {
   parsed,ok:=blueprintParseJSON(completion); if !ok{return nil,ErrBlueprintCharacterSyncReceiptInvalid}
   object,ok:=parsed.(map[string]any);if !ok{return nil,ErrBlueprintCharacterSyncReceiptInvalid};row["completionReceipt"]=object
  }else{delete(row,"completionReceipt")}
  if (row["status"]=="pending"&&completion!=nil)||(row["status"]=="completed"&&completion==nil){return nil,ErrBlueprintCharacterSyncStateInvalid}
  parsed,ok:=blueprintParseJSON(row["characterSyncInput"]);if !ok{return nil,ErrBlueprintCharacterSyncInputInvalid}
  items,ok:=parsed.([]any);if !ok{return nil,ErrBlueprintCharacterSyncInputInvalid};row["characterSyncInput"]=items
  if !phTruthy(row["completedAt"]){delete(row,"completedAt")}
 }
 return rows,nil
}
func blueprintSync(ctx context.Context,q rrQueryer,id string)(map[string]any,error){
 rows,err:=blueprintSyncRows(ctx,q,"WHERE operation_id=?",id)
 if err!=nil||len(rows)==0{return nil,err};return rows[0],nil
}
func (b *Blueprints) PendingSync(ctx context.Context)([]map[string]any,error){
 if b==nil||b.db==nil{return nil,ErrProjectNotOpen}
 if _,err:=b.db.ExecContext(ctx,blueprintBackfillSQL);err!=nil{return nil,err}
 return blueprintSyncRows(ctx,b.db,"WHERE status='pending' ORDER BY created_at ASC,operation_id ASC")
}
func (b *Blueprints) GetSync(ctx context.Context,id string)(map[string]any,error){
 // Original method checks the ID before requireProjectDb (unlike commitRange).
 if strings.TrimFunc(id,narrativeSpace)==""{return nil,ErrBlueprintCharacterSyncOperationIDRequired}
 if b==nil||b.db==nil{return nil,ErrProjectNotOpen}
 if _,err:=b.db.ExecContext(ctx,blueprintBackfillSQL);err!=nil{return nil,err}
 row,err:=blueprintSync(ctx,b.db,id);if err!=nil{return nil,err}
 // Raw completion receipts are never accepted as authoritative proposal/roster
 // evidence. That owner has not been ported; pending reads remain supported.
 if row!=nil&&row["status"]=="completed"{return nil,ErrGenerationOwnerRequired}
 return row,nil
}

type blueprintRange struct{ id,mode string;start,end int64;items []map[string]any }
func blueprintNumber(value any)(float64,bool){
 switch v:=value.(type){case int:return float64(v),true;case int32:return float64(v),true;case int64:return float64(v),true;case float64:return v,true;case json.Number:f,err:=v.Float64();return f,err==nil};return 0,false
}
func blueprintStrictEqual(a,b any)bool{
 if x,ok:=blueprintNumber(a);ok{y,ok:=blueprintNumber(b);return ok&&x==y};return rrEqual(a,b)
}
func blueprintRangeInput(input map[string]any)(blueprintRange,error){
 r:=blueprintRange{};r.id,_=input["operationId"].(string)
 if strings.TrimFunc(r.id,narrativeSpace)==""{return r,ErrBlueprintOperationIDRequired}
 r.mode,_=input["mode"].(string);if r.mode!="full"&&r.mode!="replace-range"{return r,ErrBlueprintCommitModeInvalid}
 start,startOK:=blueprintNumber(input["startChapter"]);end,endOK:=blueprintNumber(input["endChapter"])
 if !startOK||!endOK||math.IsNaN(start)||math.IsInf(start,0)||math.IsNaN(end)||math.IsInf(end,0)||math.Trunc(start)!=start||math.Trunc(end)!=end||start<1||end<start||(r.mode=="full"&&start!=1){return r,ErrBlueprintCommitRangeInvalid}
 // Deliberate native hardening, not the original Number.isInteger contract:
 // reject unsafe integers and ranges over 10,000 before allocating/iterating.
 if start>9007199254740991||end>9007199254740991||end-start+1>10000{return r,ErrBlueprintCommitRangeInvalid}
 r.start,r.end=int64(start),int64(end)
 switch items:=input["blueprints"].(type){
 case []map[string]any:r.items=items
 case []any: r.items=make([]map[string]any,len(items));for i,item:=range items{m,ok:=item.(map[string]any);if !ok{return r,ErrBlueprintCommitRangeIncomplete};r.items[i]=m}
 default:return r,ErrBlueprintCommitRangeIncomplete
 }
 if len(r.items)!=int(r.end-r.start+1){return r,ErrBlueprintCommitRangeIncomplete}
 seen:=map[int64]bool{}
 for _,item:=range r.items{chapter,ok:=blueprintNumber(item["chapterNumber"]);if !ok||math.IsNaN(chapter)||math.Trunc(chapter)!=chapter||chapter<start||chapter>end{return r,ErrBlueprintCommitRangeIncomplete};n:=int64(chapter);if seen[n]{return r,ErrBlueprintCommitRangeIncomplete};seen[n]=true}
 return r,nil
}

// canonicalize's en-US collation is supported for ordinary ASCII alphanumeric
// property names only. Do NOT silently hash Unicode/punctuation with Go's
// lexical order. No runtime Node/ICU subprocess or new collation dependency.
// Arrays preserve order; JS integer-index properties enumerate numerically.
func blueprintIndex(key string)(uint64,bool){
 if key==""||(len(key)>1&&key[0]=='0'){return 0,false}
 n,err:=strconv.ParseUint(key,10,32);return n,err==nil&&n<4294967295&&strconv.FormatUint(n,10)==key
}
func blueprintCanonical(value any)(any,error){
 switch v:=value.(type){
 case map[string]any:
  keys:=make([]string,0,len(v));for key:=range v{if key==""{return nil,ErrBlueprintCommitCanonicalKeyUnsupported};for _,c:=range key{if !(c>='a'&&c<='z'||c>='A'&&c<='Z'||c>='0'&&c<='9'){return nil,ErrBlueprintCommitCanonicalKeyUnsupported}};keys=append(keys,key)}
  sort.Slice(keys,func(i,j int)bool{
   a,ai:=blueprintIndex(keys[i]);b,bi:=blueprintIndex(keys[j]);if ai!=bi{return ai};if ai{return a<b}
   left,right:=strings.ToLower(keys[i]),strings.ToLower(keys[j]);if left!=right{return left<right}
   // en-US's default case tertiary weight is lowercase before uppercase.
   return keys[i]>keys[j]
  })
  ordered:=plotObject{};for _,key:=range keys{entry,err:=blueprintCanonical(v[key]);if err!=nil{return nil,err};ordered=append(ordered,plotField{key,entry})};return ordered,nil
 case []map[string]any:items:=make([]any,len(v));for i,item:=range v{items[i]=item};return blueprintCanonical(items)
 case []any:items:=make([]any,len(v));for i,item:=range v{entry,err:=blueprintCanonical(item);if err!=nil{return nil,err};items[i]=entry};return items,nil
 case []string:items:=make([]any,len(v));for i,item:=range v{items[i]=item};return items,nil
 case int32:return int64(v),nil
 }
 return value,nil
}
func blueprintPayloadHash(input map[string]any)(string,error){
 payload:=map[string]any{};for _,key:=range []string{"mode","startChapter","endChapter","blueprints"}{payload[key]=input[key]}
 canonical,err:=blueprintCanonical(payload);if err!=nil{return "",err};encoded,err:=plotJSON(canonical);if err!=nil{return "",err}
 sum:=sha256.Sum256([]byte(encoded));return hex.EncodeToString(sum[:]),nil
}
func blueprintExact(ctx context.Context,q rrQueryer,r blueprintRange)([]map[string]any,error){
 rows,err:=blueprintRows(ctx,q,"WHERE chapter_number BETWEEN ? AND ? ORDER BY chapter_number ASC",r.start,r.end)
 if err!=nil{return nil,err}
 // Same original assertExactRange precedence, including stored corrupt mode/ID.
 request:=map[string]any{"operationId":r.id,"mode":r.mode,"startChapter":r.start,"endChapter":r.end,"blueprints":rows}
 if _,err:=blueprintRangeInput(request);err!=nil{return nil,err};return rows,nil
}
func blueprintAuthorFields(input,existing map[string]any)map[string]any{
 out:=map[string]any{};for key,value:=range input{out[key]=value}
 for _,keys:=range [][]string{{"userGuidance"},{"notes","notesUpdatedAt"}}{
  source:=input;if !phTruthy(input[keys[0]]){source=existing}
  for _,key:=range keys{value:=source[key];if !phTruthy(value){value=""};out[key]=value}
 };return out
}
func blueprintSame(saved,expected map[string]any)bool{
 for _,key:=range []string{"chapterNumber","title","role","purpose","keyEvents","suspenseHook","userGuidance","notes","notesUpdatedAt"}{value,exists:=expected[key];if !exists||!blueprintStrictEqual(saved[key],value){return false}}
 left,err:=blueprintJSON(saved["characters"]);if err!=nil{return false};value,exists:=expected["characters"];if !exists{return false};right,err:=blueprintJSON(value);return err==nil&&left==right
}
func blueprintSnapshot(persisted []map[string]any,inputs []any)[]map[string]any{
 for _,saved:=range persisted{
  // Original Map construction keeps the last duplicate chapter entry.
  var matched map[string]any
  for _,item:=range inputs{if input,ok:=item.(map[string]any);ok&&blueprintStrictEqual(saved["chapterNumber"],input["chapterNumber"]){matched=input}}
  for _,key:=range []string{"relationshipHints","newCharacterCandidates"}{if value,exists:=matched[key];exists{saved[key]=value}}
 };return persisted
}
func blueprintReceipt(ctx context.Context,q rrQueryer,r blueprintRange,hash string,inputs []any,idempotent bool)(map[string]any,error){
 persisted,err:=blueprintExact(ctx,q,r);if err!=nil{return nil,err}
 snapshot:=blueprintSnapshot(persisted,inputs)
 syncOperation,err:=blueprintSync(ctx,q,"blueprint-sync-"+r.id);if err!=nil{return nil,err};if syncOperation==nil{return nil,ErrBlueprintCharacterSyncOperationMissing}
 if syncOperation["status"]=="completed"{return nil,ErrGenerationOwnerRequired}
 chapters:=make([]any,len(snapshot));for i,item:=range snapshot{chapters[i]=item["chapterNumber"]}
 return map[string]any{"mode":r.mode,"operationId":r.id,"payloadHash":hash,"idempotent":idempotent,
  "startChapter":r.start,"endChapter":r.end,"chapterNumbers":chapters,"snapshot":snapshot,
  "characterSyncInput":inputs,"characterSyncOperation":syncOperation},nil
}
func (b *Blueprints) CommitRange(ctx context.Context,input map[string]any)(map[string]any,error){
 if b==nil||b.db==nil{return nil,ErrProjectNotOpen}
 r,err:=blueprintRangeInput(input);if err!=nil{return nil,err}
 // Presence, even JSON null, is not transferable generation ownership. Deny
 // before any replay path or writes; authorRecovery is equally owner-bound.
 if _,exists:=input["generationRunHandle"];exists{return nil,ErrGenerationOwnerRequired}
 if _,exists:=input["authorRecovery"];exists{return nil,ErrGenerationOwnerRequired}
 // As in the original, admission/schema repair happens before payload hashing
 // and before the effect transaction; a failed commit can still repair old sync.
 if _,err:=b.db.ExecContext(ctx,blueprintBackfillSQL);err!=nil{return nil,err}
 hash,err:=blueprintPayloadHash(input);if err!=nil{return nil,err}
 var receipt map[string]any
 err=NewReviewRevisions(b.db).immediate(ctx,func(conn *sql.Conn)error{
  operations,err:=rrMaps(ctx,conn,`SELECT operation_id,payload_hash,mode,start_chapter,end_chapter,character_sync_input
   FROM blueprint_commit_operations WHERE operation_id=?`,r.id);if err!=nil{return err}
  if len(operations)>0{
   operation:=operations[0];if operation["payload_hash"]!=hash{return ErrBlueprintOperationConflict}
   parsed,ok:=blueprintParseJSON(operation["character_sync_input"]);if !ok{return ErrBlueprintCommitReceiptInvalid};items,ok:=parsed.([]any);if !ok{return ErrBlueprintCommitReceiptInvalid}
   stored:=blueprintRange{id:r.id};stored.mode,_=operation["mode"].(string)
   start,startOK:=rrInteger(operation["start_chapter"],1);end,endOK:=rrInteger(operation["end_chapter"],1);if !startOK||!endOK{return ErrBlueprintCommitRangeInvalid};stored.start,stored.end=start,end
   receipt,err=blueprintReceipt(ctx,conn,stored,hash,items,true);return err
  }
  if r.mode=="full"{if _,err:=conn.ExecContext(ctx,"DELETE FROM blueprints WHERE chapter_number < ? OR chapter_number > ?",r.start,r.end);err!=nil{return err}}
  existing,err:=blueprintRows(ctx,conn,"WHERE chapter_number BETWEEN ? AND ?",r.start,r.end);if err!=nil{return err}
  byChapter:=map[int64]map[string]any{};for _,row:=range existing{chapter,ok:=rrInteger(row["chapterNumber"],1);if ok{byChapter[chapter]=row}}
  expected:=map[int64]map[string]any{};syncInput:=make([]any,len(r.items))
  for i,item:=range r.items{chapter,_:=blueprintNumber(item["chapterNumber"]);merged:=blueprintAuthorFields(item,byChapter[int64(chapter)]);expected[int64(chapter)]=merged;syncInput[i]=merged;if err:=blueprintUpsert(ctx,conn,merged);err!=nil{return err}}
  persisted,err:=blueprintExact(ctx,conn,r);if err!=nil{return err}
  for _,saved:=range persisted{chapter,_:=rrInteger(saved["chapterNumber"],1);if !blueprintSame(saved,expected[chapter]){return ErrBlueprintCommitReadBackFailed}}
  encoded,err:=blueprintJSON(syncInput);if err!=nil{return err}
  if _,err:=conn.ExecContext(ctx,`INSERT INTO blueprint_commit_operations(operation_id,payload_hash,mode,start_chapter,end_chapter,character_sync_input)
   VALUES(?,?,?,?,?,?)`,r.id,hash,r.mode,r.start,r.end,encoded);err!=nil{return err}
  if _,err:=conn.ExecContext(ctx,`INSERT INTO blueprint_character_sync_operations(operation_id,blueprint_commit_operation_id,
   blueprint_commit_payload_hash,start_chapter,end_chapter,character_sync_input) VALUES(?,?,?,?,?,?)`,"blueprint-sync-"+r.id,r.id,hash,r.start,r.end,encoded);err!=nil{return err}
  receipt,err=blueprintReceipt(ctx,conn,r,hash,syncInput,false);return err
 })
 if err!=nil{return nil,err};return receipt,nil
}
