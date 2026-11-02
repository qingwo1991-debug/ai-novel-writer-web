package store

import (
 "context"
 "database/sql"
 "errors"
 "reflect"
 "strings"
 "testing"
)

// Synthetic values are seeded into private byte-copies of the original baseline
// v7, never an independently invented roster schema. Execution is GitHub-only.
func rosterFixture(t *testing.T) (*sql.DB, *Characters) {
 t.Helper()
 db, _, _ := narrativeFixture(t)
 mustExec(t, db, `UPDATE characters SET retired=1;
 UPDATE character_identity_meta SET revision=17 WHERE id='main';
 UPDATE project_core SET writing_language='zh-CN',characters_arch='' WHERE id='main';
 UPDATE character_roster_meta SET revision=11,migration_state='empty',legacy_markdown='',projection_hash='',fact_hash='' WHERE id='main'`)
 return db, NewCharacters(db)
}
func rosterSeed(t *testing.T, db *sql.DB, id, name, role string) {
 t.Helper()
 mustExec(t, db, "INSERT INTO characters(character_id,name,role) VALUES(?,?,?)", id, name, role)
}
func rosterRead(t *testing.T, r *Characters) map[string]any {
 t.Helper()
 value, err := r.ReadRoster(context.Background()); if err != nil { t.Fatal(err) }; return value
}
func rosterReady(t *testing.T, db *sql.DB, projection, factJSON string) {
 t.Helper()
 mustExec(t, db, "UPDATE project_core SET characters_arch=? WHERE id='main'", projection)
 mustExec(t, db, "UPDATE character_roster_meta SET migration_state='ready',projection_hash=?,fact_hash=? WHERE id='main'", rosterHash(projection), rosterHash(factJSON))
}
func rosterEdge(t *testing.T, db *sql.DB, id, source, target, relation string) {
 t.Helper()
 mustExec(t, db, "INSERT OR IGNORE INTO character_identity_approvals(operation_id,payload_hash,receipt_json) VALUES('roster-test-approval','hash','{}')")
 mustExec(t, db, `INSERT INTO character_relationships(relationship_id,source_character_id,target_character_id,relation,source_display_snapshot,target_display_snapshot,provenance_json,approval_id) VALUES(?,?,?,?,'stale source','stale target','{}','roster-test-approval')`, id, source, target, relation)
}
func TestRosterEmptyReadNoWritesAndMissingProject(t *testing.T) {
 for _, r := range []*Characters{nil, NewCharacters(nil), {}} { if row, err := r.ReadRoster(context.Background()); row != nil || !errors.Is(err, ErrProjectNotOpen) { t.Fatalf("missing=%v %v", row, err) } }
 db, r := rosterFixture(t)
 db.SetMaxOpenConns(1)
 var changes, schema int64
 if err := db.QueryRow("SELECT total_changes()").Scan(&changes); err != nil { t.Fatal(err) }
 if err := db.QueryRow("PRAGMA schema_version").Scan(&schema); err != nil { t.Fatal(err) }
 row := rosterRead(t, r)
 if row["status"] != "empty" || row["revision"] != int64(11) || row["identityRevision"] != int64(17) || row["schemaVersion"] != int64(1) || row["renderedMarkdown"] != "" || row["factHash"] != rosterHash("[]") || row["projectionHash"] != rosterHash("") || row["nameOnlyFactHash"] != "" { t.Fatal(row) }
 if entries := row["entries"].([]map[string]any); entries == nil || len(entries) != 0 { t.Fatal(entries) }
 if aliases := row["aliases"].([]map[string]any); aliases == nil || len(aliases) != 0 { t.Fatal(aliases) }
 if _, exists := row["legacyMarkdown"]; exists { t.Fatal(row) }
 var afterChanges, afterSchema int64
 if err := db.QueryRow("SELECT total_changes()").Scan(&afterChanges); err != nil { t.Fatal(err) }
 if err := db.QueryRow("PRAGMA schema_version").Scan(&afterSchema); err != nil { t.Fatal(err) }
 if changes != afterChanges || schema != afterSchema { t.Fatalf("read wrote changes=%d/%d schema=%d/%d", changes, afterChanges, schema, afterSchema) }
 cancelled, cancel := context.WithCancel(context.Background()); cancel()
 if row, err := r.ReadRoster(cancelled); row != nil || !errors.Is(err, context.Canceled) { t.Fatalf("cancel=%v %v", row, err) }
 rosterRead(t, r) // A cancelled reservation cannot leak the only connection.
}
func TestRosterReadyExactOrderedHashesAndLanguageFallback(t *testing.T) {
 db, r := rosterFixture(t)
 rosterSeed(t, db, "roster-test-id", "Ada😀", " MAIN ")
 mustExec(t, db, `UPDATE characters SET gender='F',age='0',notes='a<&>\nline',cs_location='Town',cs_updated_at_chapter=0,
 cs_provenance=? WHERE character_id='roster-test-id'`, `{"z":{"b":2,"a":1},"location":{"chapterNumber":0,"kind":"author"},"10":"ten","2":"two","z":{"last":true,"first":false},"4294967295":"ordinary","01":"not-index"}`)
 // Literal independently specified in the original entryFromCharacter order.
 nameJSON := `[{"name":"Ada😀","role":"protagonist","gender":"F","age":"0","appearance":"","personality":"","background":"","abilities":"","motivation":"","relationships":[],"arc":"","notes":"a<&>\\nline","currentState":{"location":"Town","powerLevel":"","physicalState":"","mentalState":"","keyItems":"","recentEvents":"","updatedAtChapter":0,"provenance":{"2":"two","10":"ten","z":{"last":true,"first":false},"location":{"chapterNumber":0,"kind":"author"},"4294967295":"ordinary","01":"not-index"}}}]`
 identityJSON := strings.TrimSuffix(nameJSON, "}]")+`,"characterId":"roster-test-id"}]`
 projection := "# Character graph\n\n## Protagonist: Ada😀\n- Gender: F\n- Age: 0\n- Notes: a<&>\\nline"
 rosterReady(t, db, projection, nameJSON)
 // Current project is Chinese but historical ready English remains readable.
 row := rosterRead(t, r)
 if row["status"] != "ready" || row["renderedMarkdown"] != projection || row["projectionHash"] != rosterHash(projection) || row["factHash"] != rosterHash(identityJSON) || row["nameOnlyFactHash"] != rosterHash(nameJSON) { t.Fatalf("ordered hashes/fallback=%v\nname=%s\nidentity=%s", row, nameJSON, identityJSON) }
 entry := row["entries"].([]map[string]any)[0]
 if entry["role"] != "protagonist" || entry["currentState"].(map[string]any)["updatedAtChapter"] != int64(0) { t.Fatal(entry) }
 mustExec(t, db, "UPDATE project_core SET writing_language='en-US' WHERE id='main'")
 if next := rosterRead(t, r); !reflect.DeepEqual(row, next) { t.Fatalf("language changed historical snapshot %v / %v", row, next) }
 mustExec(t, db, "UPDATE characters SET cs_location='Different' WHERE character_id='roster-test-id'")
 stale := rosterRead(t, r)
 if stale["status"] != "inconsistent" || stale["factHash"] == row["factHash"] || stale["projectionHash"] != row["projectionHash"] { t.Fatal(stale) }
}
func TestRosterUTF16RolesActualIDRelationsAndAliasProjection(t *testing.T) {
 db, r := rosterFixture(t)
 for _, item := range []struct{id,name,role string}{
  {"roster-test-a","\ue000","supporting"}, {"roster-test-b","😀","supporting"},
  {"roster-test-c","Zed","protagonist"}, {"roster-test-d","Same","villain"},
  {"roster-test-e","Same","antagonist"}, {"roster-test-f","Minor","龙套"},
  {"roster-test-g","Unknown","mystery"}, {"roster-test-retired","Retired","protagonist"},
 } { rosterSeed(t, db, item.id, item.name, item.role) }
 mustExec(t, db, "UPDATE characters SET retired=1 WHERE character_id='roster-test-retired'")
 mustExec(t, db, "UPDATE characters SET relationships=' old raw relationship evidence ' WHERE character_id='roster-test-c'")
 rosterEdge(t, db, "roster-test-edge-01", "roster-test-c", "roster-test-e", "second same target")
 rosterEdge(t, db, "roster-test-edge-02", "roster-test-c", "roster-test-d", "first same target")
 rosterEdge(t, db, "roster-test-edge-03", "roster-test-c", "roster-test-retired", "must disappear")
 rosterEdge(t, db, "roster-test-edge-04", "roster-test-retired", "roster-test-c", "retired source")
 mustExec(t, db, `INSERT INTO character_aliases(character_id,name,source_key,valid_from,valid_through) VALUES
 ('roster-test-c','Zed','b',2,NULL),('roster-test-c','Zed','a',4,8),('roster-test-c','Zed','a',1,NULL),('roster-test-retired','Retired','r',0,NULL)`)
 row := rosterRead(t, r)
 entries := row["entries"].([]map[string]any)
 names := []string{}; for _, entry := range entries { names = append(names, entry["name"].(string)) }
 if !reflect.DeepEqual(names, []string{"Zed","Unknown","😀","\ue000","Same","Same","Minor"}) { t.Fatal(names) }
 edges := entries[0]["relationships"].([]map[string]any)
 if len(edges) != 2 || edges[0]["targetCharacterId"] != "roster-test-e" || edges[1]["targetCharacterId"] != "roster-test-d" || edges[0]["target"] != "Same" { t.Fatal(edges) }
 if entries[0]["legacyRelationshipNotes"] != " old raw relationship evidence " { t.Fatal(entries[0]) }
 aliases := row["aliases"].([]map[string]any)
 if len(aliases) != 3 || aliases[0]["sourceKey"] != "a" || aliases[0]["validFrom"] != int64(1) || aliases[0]["validThrough"] != nil || aliases[1]["validThrough"] != int64(8) || aliases[2]["sourceKey"] != "b" { t.Fatal(aliases) }
 beforeIdentity := row["factHash"]
 beforeNames, err := rosterEntriesJSON(entries, map[string]plotObject{}, false); if err != nil { t.Fatal(err) }
 mustExec(t, db, "UPDATE character_relationships SET target_character_id='roster-test-d' WHERE relationship_id='roster-test-edge-01'")
 changed := rosterRead(t, r)
 afterNames, err := rosterEntriesJSON(changed["entries"].([]map[string]any), map[string]plotObject{}, false); if err != nil { t.Fatal(err) }
 if beforeNames != afterNames || beforeIdentity == changed["factHash"] || changed["renderedMarkdown"] != row["renderedMarkdown"] { t.Fatalf("same-name ID retarget lost identity distinction %v / %v", row, changed) }
}
func TestRosterStatusClassificationAndNoLegacyTextInterpretation(t *testing.T) {
 for _, test := range []struct{state,legacy,projection string; character bool; want string}{
  {"empty","\ufeff \t"," \n",false,"empty"},
  {"empty","evidence","",false,"inconsistent"},
  {"empty","","",true,"inconsistent"},
  {"legacy_cards_preserved","","",false,"inconsistent"},
  {"legacy_markdown_pending","# 角色图谱\n\n## 主角：not a card","changed",false,"legacy_repair_required"},
  {"legacy_markdown_pending"," \ufeff ","",false,"inconsistent"},
  {"legacy_markdown_pending","evidence","",true,"inconsistent"},
 } {
  t.Run(test.state+"-"+test.want+test.legacy,func(t *testing.T){
   db,r := rosterFixture(t); if test.character { rosterSeed(t,db,"roster-test-x","X","minor") }
   mustExec(t,db,"UPDATE character_roster_meta SET migration_state=?,legacy_markdown=? WHERE id='main'",test.state,test.legacy)
   mustExec(t,db,"UPDATE project_core SET characters_arch=? WHERE id='main'",test.projection)
   row:=rosterRead(t,r); if row["status"]!=test.want { t.Fatal(row) }
   if test.legacy!="" { if row["legacyMarkdown"]!=test.legacy { t.Fatal(row) } } else if _,ok:=row["legacyMarkdown"];ok { t.Fatal(row) }
   if !test.character && len(row["entries"].([]map[string]any))!=0 { t.Fatal("legacy Markdown interpreted",row) }
  })
 }
 db,r:=rosterFixture(t); rosterReady(t,db,"","[]")
 if row:=rosterRead(t,r);row["status"]!="empty" { t.Fatal(row) }
 for _,query:=range []string{"UPDATE character_roster_meta SET fact_hash='wrong' WHERE id='main'","UPDATE character_roster_meta SET projection_hash='wrong' WHERE id='main'","UPDATE project_core SET characters_arch='external change' WHERE id='main'"} {
  rosterReady(t,db,"","[]");mustExec(t,db,query);if row:=rosterRead(t,r);row["status"]!="inconsistent" { t.Fatal(row) }
 }
}
func TestRosterLocalizedMarkdownDoesNotRenderState(t *testing.T) {
 entry:=map[string]any{"name":"A\nB","role":"supporting","gender":"女","age":float64(12.5),"appearance":"<red>","personality":"安静","background":"背景","abilities":"能力","motivation":"动机","arc":"弧光","notes":"备注","relationships":[]map[string]any{{"target":"甲","relation":"友"}},"legacyRelationshipNotes":"old","currentState":map[string]any{"location":"SECRET"}}
 zh:="# 角色图谱\n\n## 配角：A\nB\n- 性别：女\n- 年龄：12.5\n- 外貌：<red>\n- 性格：安静\n- 背景：背景\n- 能力：能力\n- 动机：动机\n- 弧光：弧光\n- 备注：备注\n- 关系：甲（友）\n- 关系备注：old"
 en:="# Character graph\n\n## Supporting character: A\nB\n- Gender: 女\n- Age: 12.5\n- Appearance: <red>\n- Personality: 安静\n- Background: 背景\n- Abilities: 能力\n- Motivation: 动机\n- Arc: 弧光\n- Notes: 备注\n- Relationship: 甲 (友)\n- Relationship notes: old"
 if got:=rosterMarkdown([]map[string]any{entry},false);got!=zh { t.Fatalf("Chinese=%q",got) }
 if got:=rosterMarkdown([]map[string]any{entry},true);got!=en { t.Fatalf("English=%q",got) }
}
func TestRosterMissingSchemasExplicitNoDDL(t *testing.T) {
 for _,test:=range []struct{query string;want error}{
  {"DROP TABLE character_identity_meta",ErrCharacterIDSchemaRequired},
  {"DROP TABLE character_roster_meta",ErrCharacterRosterSchemaUnsupported},
  {"DELETE FROM character_roster_meta WHERE id='main'",ErrCharacterRosterMetaMissing},
  {"ALTER TABLE character_aliases RENAME COLUMN valid_through TO unsupported_through",ErrCharacterIDSchemaRequired},
 } {
  t.Run(test.query,func(t *testing.T){db,r:=rosterFixture(t);mustExec(t,db,test.query)
   before,err:=rrMaps(context.Background(),db,"SELECT type,name,sql FROM sqlite_master ORDER BY type,name");if err!=nil {t.Fatal(err)}
   if row,err:=r.ReadRoster(context.Background());row!=nil||!errors.Is(err,test.want){t.Fatalf("row=%v error=%v want=%v",row,err,test.want)}
   after,err:=rrMaps(context.Background(),db,"SELECT type,name,sql FROM sqlite_master ORDER BY type,name");if err!=nil||!reflect.DeepEqual(before,after){t.Fatalf("schema changed %v",err)}
  })
 }
}
func TestRosterProvenanceInvalidFallbackAndUnsupportedDomains(t *testing.T) {
 db,r:=rosterFixture(t);rosterSeed(t,db,"roster-test-x","X","minor")
 for _,raw:=range []string{"{}","not JSON","[]","null","42"} {
  mustExec(t,db,"UPDATE characters SET cs_updated_at_chapter=0,cs_provenance=? WHERE character_id='roster-test-x'",raw)
  entry:=rosterRead(t,r)["entries"].([]map[string]any)[0]
  if _,ok:=entry["currentState"].(map[string]any)["provenance"];ok {t.Fatal(entry)}
 }
 for _,raw:=range []string{`{"x":"\ud800"}`,`{"x":1e400}`,string([]byte{255})} {
  mustExec(t,db,"UPDATE characters SET cs_provenance=? WHERE character_id='roster-test-x'",raw)
  if row,err:=r.ReadRoster(context.Background());row!=nil||!errors.Is(err,ErrCharacterInputUnsupported){t.Fatalf("unsupported raw=%q row=%v err=%v",raw,row,err)}
 }
 mustExec(t,db,"UPDATE characters SET cs_provenance='{}',gender=X'0102' WHERE character_id='roster-test-x'")
 if row,err:=r.ReadRoster(context.Background());row!=nil||!errors.Is(err,ErrCharacterInputUnsupported){t.Fatalf("blob=%v %v",row,err)}
 mustExec(t,db,"UPDATE characters SET gender='',cs_updated_at_chapter=NULL,cs_provenance=? WHERE character_id='roster-test-x'",`{"x":"\ud800"}`)
 if row:=rosterRead(t,r);len(row["entries"].([]map[string]any))!=1 {t.Fatal(row)} // Unused provenance is not parsed.
}

func TestRosterOrderedOpaqueProvenanceJSON(t *testing.T) {
 for _,test:=range []struct{raw,want string}{
  {`{"b":1,"a":{"z":2,"y":3},"b":4}`,`{"b":4,"a":{"z":2,"y":3}}`},
  {`{"4294967295":1,"3":2,"0":0,"01":3,"2":4,"a":[{"9":"x","1":"y","b":true,"a":null}]}`,`{"0":0,"2":4,"3":2,"4294967295":1,"01":3,"a":[{"1":"y","9":"x","b":true,"a":null}]}`},
  {`{"escaped":"<&>\u2028\u2029\ud83d\ude00\u0000","number":-0,"small":1e-7,"fixed":1e20}`,"{\"escaped\":\"<&>\u2028\u2029😀\\u0000\",\"number\":0,\"small\":1e-7,\"fixed\":100000000000000000000}"},
 } {
  parsed,err:=rosterProvenance(test.raw);if err!=nil{t.Fatal(err)}
  got,err:=plotJSON(parsed);if err!=nil||got!=test.want{t.Fatalf("raw=%s\njson=%s\nwant=%s error=%v",test.raw,got,test.want,err)}
 }
}
