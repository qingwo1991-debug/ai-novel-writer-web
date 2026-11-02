package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// Every SQL test uses a private byte-copy of the original TypeScript baseline,
// not a parallel hand-maintained schema. CI must provide the original fixture.
func narrativeFixture(t *testing.T) (*sql.DB, *Narrative, string) {
	t.Helper()
	contract := os.Getenv("GO_SQLITE_CONTRACT")
	if contract == "" {
		if os.Getenv("GITHUB_ACTIONS") == "true" { t.Fatal("GO_SQLITE_CONTRACT is mandatory for narrative tests") }
		t.Skip("GitHub-only original TypeScript baseline fixture")
	}
	data, err := os.ReadFile(contract)
	if err != nil { t.Fatal(err) }
	var manifest struct { Fixtures []struct { Name string `json:"name"`; File string `json:"file"` } `json:"fixtures"` }
	if err := json.Unmarshal(data, &manifest); err != nil { t.Fatal(err) }
	var source string
	for _, fixture := range manifest.Fixtures {
		if fixture.Name == "baseline-v7" { source = filepath.Join(filepath.Dir(contract), fixture.File); break }
	}
	if source == "" { t.Fatal("original baseline-v7 fixture missing") }
	original, err := os.ReadFile(source)
	if err != nil { t.Fatal(err) }
	path := filepath.Join(t.TempDir(), "narrative #? 中文.db")
	if err := os.WriteFile(path, original, 0600); err != nil { t.Fatal(err) }
	t.Cleanup(func() {
		after, err := os.ReadFile(source)
		if err != nil || !bytes.Equal(original, after) { t.Errorf("original baseline fixture mutated: %v", err) }
	})
	dsn, err := ExistingDSN(path)
	if err != nil { t.Fatal(err) }
	db, err := sql.Open("sqlite3", dsn)
	if err != nil { t.Fatal(err) }
	db.SetMaxOpenConns(4)
	t.Cleanup(func() { _ = db.Close() })
	mustExec(t, db, `DELETE FROM narrative_thread_confirmations; DELETE FROM narrative_thread_plans;
 DELETE FROM consistency_exemptions; UPDATE drafts SET status='archived'`)
	return db, NewNarrative(db), path
}

func narrativePlan() map[string]any {
	return map[string]any{"title": " 暗门😀 ", "type": " 伏笔 ", "targetStartChapter": float64(2),
		"targetEndChapter": float64(5), "authorIntent": " 阿青寻找真相 "}
}

func narrativeCreate(t *testing.T, n *Narrative) int64 {
	t.Helper()
	plan, err := n.CreatePlan(context.Background(), narrativePlan())
	if err != nil { t.Fatal(err) }
	return plan["id"].(int64)
}

func narrativeSource(t *testing.T, db *sql.DB, id, chapter int64, body string, outbox bool) {
	t.Helper()
	mustExec(t, db, "INSERT INTO contents(id,body) VALUES(?,?)", id, body)
	mustExec(t, db, "INSERT INTO drafts(id,chapter_number,version,status,content_id) VALUES(?,?,910,'finalized',?)", id, chapter, id)
	if outbox {
		mustExec(t, db, `INSERT INTO finalization_outbox(finalization_id,draft_id,chapter_number,chapter_title,
 content_hash,content_revision,content_snapshot,target_file_name,knowledge_document_id)
 VALUES(?,?,?,?,'not-a-hash-on-purpose',1,?,'must-not-open.txt','must-not-read-knowledge')`,
			fmt.Sprintf("narrative-%d", id), id, chapter, fmt.Sprintf("第%d章", chapter), body)
	}
}

func narrativeEvent(planID, draftID int64, kind, evidence string) map[string]any {
	return map[string]any{"planId": planID, "draftId": draftID, "type": kind, "evidence": evidence, "reason": " 作者确认 "}
}

func narrativeCount(t *testing.T, db *sql.DB, table string) int64 {
	t.Helper()
	var count int64
	if err := db.QueryRow("SELECT COUNT(*) FROM "+table).Scan(&count); err != nil { t.Fatal(err) }
	return count
}

func TestNarrativeNilDatabaseContract(t *testing.T) {
	ctx := context.Background()
	for _, n := range []*Narrative{nil, NewNarrative(nil), {}} {
		if row, err := n.CreatePlan(ctx, nil); row != nil || !errors.Is(err, ErrProjectNotOpen) { t.Fatalf("create %v %v", row, err) }
		if row, err := n.UpdatePlan(ctx, 1, nil); row != nil || !errors.Is(err, ErrProjectNotOpen) { t.Fatalf("update %v %v", row, err) }
		if err := n.DeletePlan(ctx, 1); !errors.Is(err, ErrProjectNotOpen) { t.Fatal(err) }
		if row, err := n.GetPlan(ctx, 1); row != nil || !errors.Is(err, ErrProjectNotOpen) { t.Fatalf("get %v %v", row, err) }
		if row, err := n.ConfirmEvent(ctx, nil); row != nil || !errors.Is(err, ErrProjectNotOpen) { t.Fatalf("confirm %v %v", row, err) }
		if rows, err := n.List(ctx); rows != nil || !errors.Is(err, ErrProjectNotOpen) { t.Fatalf("list %v %v", rows, err) }
		if rows, err := n.Relevant(ctx, nil); rows != nil || !errors.Is(err, ErrProjectNotOpen) { t.Fatalf("relevant %v %v", rows, err) }
		if rows, err := n.Exemptions(ctx); rows == nil || len(rows) != 0 || err != nil { t.Fatalf("exemptions %v %v", rows, err) }
		if err := n.SaveExemption(ctx, "", ""); !errors.Is(err, ErrProjectNotOpen) { t.Fatal(err) }
		if err := n.RevokeExemption(ctx, ""); !errors.Is(err, ErrProjectNotOpen) { t.Fatal(err) }
	}
}

func TestNarrativeJavaScriptWhitespaceAndUTF16(t *testing.T) {
	if narrativeLength("😀") != 2 || narrativeLength("甲😀乙") != 4 { t.Fatal("length is not UTF-16 units") }
	for _, test := range []struct { value string; want string; valid bool }{
		{"\ufeff\t\u3000甲\u2029\ufeff", "甲", true}, {"\ufeff\u00a0", "", false},
		{"\u0085", "\u0085", true}, {"\u0085甲\u0085", "\u0085甲\u0085", true},
	} {
		got, valid := narrativeText(test.value, 120)
		if got != test.want || valid != test.valid { t.Fatalf("trim %q => %q %v", test.value, got, valid) }
	}
	if got := narrativeCompact("a\ufeff\u2000\r\nb\u0085c"); got != "ab\u0085c" { t.Fatalf("JS whitespace removal %q", got) }
	for _, test := range []struct { key string; limit int }{{"title",120},{"type",60},{"authorIntent",1000}} {
		input := narrativePlan(); input[test.key] = strings.Repeat("😀", test.limit/2)
		if _, err := narrativePlanInput(input); err != nil { t.Fatalf("exact UTF-16 boundary %s %v", test.key, err) }
		input[test.key] = strings.Repeat("😀", test.limit/2)+"x"
		if _, err := narrativePlanInput(input); !errors.Is(err, ErrNarrativeThreadPlanInvalid) { t.Fatalf("over UTF-16 boundary %s %v", test.key, err) }
	}
}

func TestNarrativePlanValidationAndReadback(t *testing.T) {
	db, n, _ := narrativeFixture(t); ctx := context.Background()
	if rows, err := n.List(ctx); err != nil || rows == nil || len(rows) != 0 { t.Fatalf("empty %v %v", rows, err) }
	for _, key := range []string{"title", "type", "authorIntent"} {
		for _, value := range []any{" \ufeff", false, nil, []byte("text")} {
			input := narrativePlan(); input[key] = value
			if _, err := n.CreatePlan(ctx, input); !errors.Is(err, ErrNarrativeThreadPlanInvalid) { t.Fatalf("invalid text %s %v: %v", key, value, err) }
		}
	}
	for _, value := range []any{0, -1, 1.5, "2", true, nil, math.NaN(), math.Inf(1), float64(9007199254740992), int64(9007199254740992)} {
		input := narrativePlan(); input["targetStartChapter"] = value
		if _, err := n.CreatePlan(ctx, input); !errors.Is(err, ErrNarrativeThreadRangeInvalid) { t.Fatalf("invalid range %v: %v", value, err) }
	}
	input := narrativePlan(); input["targetEndChapter"] = 1
	if _, err := n.CreatePlan(ctx, input); !errors.Is(err, ErrNarrativeThreadRangeInvalid) { t.Fatal(err) }
	input["title"] = " "
	if _, err := n.UpdatePlan(ctx, -1, input); !errors.Is(err, ErrNarrativeThreadPlanInvalid) { t.Fatalf("validation precedence %v", err) }
	if narrativeCount(t, db, "narrative_thread_plans") != 0 { t.Fatal("invalid plan wrote") }
	input = narrativePlan(); input["extra"] = "not persisted"
	created, err := n.CreatePlan(ctx, input)
	if err != nil || created["title"] != "暗门😀" || created["type"] != "伏笔" || created["authorIntent"] != "阿青寻找真相" || created["createdAt"] == "" || len(created) != 8 { t.Fatalf("create %v %v", created, err) }
	if input["title"] != " 暗门😀 " { t.Fatal("mutated input") }
	id := created["id"].(int64)
	mustExec(t, db, "UPDATE narrative_thread_plans SET created_at='created-fixed',updated_at='old' WHERE id=?", id)
	input["title"], input["targetStartChapter"], input["targetEndChapter"] = " \ufeff新标题\ufeff ", json.Number("3"), int64(9007199254740991)
	updated, err := n.UpdatePlan(ctx, id, input)
	if err != nil || updated["title"] != "新标题" || updated["targetStartChapter"] != int64(3) || updated["targetEndChapter"] != int64(9007199254740991) || updated["createdAt"] != "created-fixed" || updated["updatedAt"] == "old" { t.Fatalf("update %v %v", updated, err) }
	read, err := n.GetPlan(ctx, id)
	if err != nil || !reflect.DeepEqual(read, updated) { t.Fatalf("get %v %v", read, err) }
	if row, err := n.GetPlan(ctx, -1); row != nil || err != nil { t.Fatalf("missing %v %v", row, err) }
	if _, err := n.UpdatePlan(ctx, -1, narrativePlan()); !errors.Is(err, ErrNarrativeThreadPlanNotFound) { t.Fatal(err) }
	if err := n.DeletePlan(ctx, -1); !errors.Is(err, ErrNarrativeThreadPlanNotFound) { t.Fatal(err) }
}

func TestNarrativeConfirmationSourcePrecedenceAndSpread(t *testing.T) {
	db, n, _ := narrativeFixture(t); ctx := context.Background(); planID := narrativeCreate(t, n)
	body := "开门\ufeff\r\n见山😀，暗线\u0085仍在。"
	narrativeSource(t, db, 810001, 3, body, true)
	narrativeSource(t, db, 810002, 4, body, false)
	input := narrativeEvent(-1, -1, "unknown", "开门")
	if _, err := n.ConfirmEvent(ctx, input); !errors.Is(err, ErrNarrativeThreadEventInvalid) { t.Fatalf("event validation precedence %v", err) }
	input["type"] = "planted"
	if _, err := n.ConfirmEvent(ctx, input); !errors.Is(err, ErrNarrativeThreadEventSourceNotFinalized) { t.Fatalf("source before plan %v", err) }
	input["draftId"] = int64(810002)
	if _, err := n.ConfirmEvent(ctx, input); !errors.Is(err, ErrNarrativeThreadEventSourceNotFinalized) { t.Fatalf("missing outbox %v", err) }
	input["draftId"], input["evidence"] = int64(810001), "not in snapshot"
	if _, err := n.ConfirmEvent(ctx, input); !errors.Is(err, ErrNarrativeThreadEvidenceNotInSource) { t.Fatalf("evidence before plan %v", err) }
	input["evidence"] = "开门见山😀"
	if _, err := n.ConfirmEvent(ctx, input); !errors.Is(err, ErrNarrativeThreadPlanNotFound) { t.Fatal(err) }
	// Body edits after finalization are irrelevant: the frozen outbox remains
	// authority, even with an opaque/non-hash content_hash and unrelated body.
	mustExec(t, db, "UPDATE contents SET body='later edit excludes every quote' WHERE id=810001")
	input["planId"], input["evidence"], input["reason"] = planID, " \ufeff开门见山😀\ufeff ", "\ufeff作者确认\ufeff"
	input["extra"], input["id"], input["chapterNumber"], input["createdAt"] = map[string]any{"keep": true}, "caller-id", -1, "caller-clock"
	result, err := n.ConfirmEvent(ctx, input)
	if err != nil || result["id"] != "caller-id" || result["evidence"] != "开门见山😀" || result["reason"] != "作者确认" || result["chapterNumber"] != int64(3) || result["chapterTitle"] != "第3章" || result["createdAt"] == "caller-clock" || !reflect.DeepEqual(result["extra"], input["extra"]) { t.Fatalf("event spread %v %v", result, err) }
	if input["evidence"] != " \ufeff开门见山😀\ufeff " { t.Fatal("mutated event input") }
	if _, err := n.ConfirmEvent(ctx, input); err == nil || !strings.Contains(err.Error(), "UNIQUE") { t.Fatalf("original exact duplicate constraint %v", err) }
	input["type"] = "progressing"
	if _, err := n.ConfirmEvent(ctx, input); err != nil { t.Fatalf("same source/evidence distinct event allowed: %v", err) }
	input["evidence"] = "暗线仍在"
	if _, err := n.ConfirmEvent(ctx, input); !errors.Is(err, ErrNarrativeThreadEvidenceNotInSource) { t.Fatalf("NEL must not be stripped %v", err) }
	input["evidence"] = "暗线\u0085仍在"
	if _, err := n.ConfirmEvent(ctx, input); err != nil { t.Fatal(err) }
	mustExec(t, db, "UPDATE finalization_outbox SET content_snapshot=CAST(content_snapshot AS BLOB) WHERE draft_id=810001")
	if _, err := n.ConfirmEvent(ctx, input); !errors.Is(err, ErrNarrativeThreadEventSourceNotFinalized) { t.Fatalf("BLOB source fails closed, not panic/coercion: %v", err) }
}

func TestNarrativeConfirmationRejectsBooleanBindingWithoutWrites(t *testing.T) {
	db,n,_:=narrativeFixture(t);ctx:=context.Background()
	mustExec(t,db,`INSERT INTO narrative_thread_plans(id,title,type,target_start_chapter,target_end_chapter,author_intent)
 VALUES(1,'plan','type',1,5,'intent')`)
	narrativeSource(t,db,810001,3,"evidence",true)
	mustExec(t,db,`INSERT OR IGNORE INTO drafts(id,chapter_number,version,status,content_id) VALUES(1,877001,910,'finalized',810001);
 UPDATE drafts SET status='finalized',content_id=810001 WHERE id=1;
 DELETE FROM finalization_outbox WHERE draft_id=1;
 INSERT INTO finalization_outbox(finalization_id,draft_id,chapter_number,content_hash,content_revision,content_snapshot,target_file_name)
 VALUES('boolean-alias-source',1,877001,'opaque',1,'evidence','unused.txt')`)
	const bindingError="SQLite3 can only bind numbers, strings, bigints, buffers, and null"
	for _,key:=range []string{"draftId","planId"}{
		input:=narrativeEvent(1,1,"planted","evidence");input[key]=true
		if _,err:=n.ConfirmEvent(ctx,input);err==nil||err.Error()!=bindingError{t.Fatalf("bool %s silently bound as SQLite 1: %v",key,err)}
	}
	input:=narrativeEvent(1,1,"planted","not in source");input["planId"]=true
	if _,err:=n.ConfirmEvent(ctx,input);!errors.Is(err,ErrNarrativeThreadEvidenceNotInSource){t.Fatalf("plan binding must not precede evidence %v",err)}
	if narrativeCount(t,db,"narrative_thread_confirmations")!=0{t.Fatal("invalid bindings wrote confirmations")}
	input=narrativeEvent(1,1,"planted","evidence");input["planId"],input["draftId"]="1","1"
	if _,err:=n.ConfirmEvent(ctx,input);err!=nil{t.Fatalf("original SQLite string id binding allowed %v",err)}
	input=narrativeEvent(1,1,"progressing","evidence");input["planId"],input["draftId"]=json.Number("1"),json.Number("1")
	if _,err:=n.ConfirmEvent(ctx,input);err!=nil{t.Fatalf("HTTP UseNumber binding allowed %v",err)}
}

func TestNarrativeEventUTF16Boundaries(t *testing.T) {
	db, n, _ := narrativeFixture(t); ctx := context.Background(); id := narrativeCreate(t, n)
	narrativeSource(t, db, 810001, 3, strings.Repeat("😀", 121), true)
	input := narrativeEvent(id, 810001, "planted", strings.Repeat("😀",120)); input["reason"] = strings.Repeat("😀",250)
	if _, err := n.ConfirmEvent(ctx, input); err != nil { t.Fatalf("240/500 units valid %v", err) }
	for _, change := range []struct{ key string; value any }{{"evidence",strings.Repeat("😀",120)+"x"},{"reason",strings.Repeat("😀",250)+"x"},{"type"," planted "},{"evidence","\ufeff "},{"reason",false}} {
		value := narrativeEvent(id,810001,"planted","😀"); value[change.key] = change.value
		if _, err := n.ConfirmEvent(ctx,value); !errors.Is(err,ErrNarrativeThreadEventInvalid) { t.Fatalf("event validation %s %v",change.key,err) }
	}
}

func TestNarrativeListOrderingSupersessionAndDormancy(t *testing.T) {
	db, n, _ := narrativeFixture(t); ctx := context.Background(); first := narrativeCreate(t,n); second := narrativeCreate(t,n)
	narrativeSource(t,db,810001,3,"first second",true)
	narrativeSource(t,db,810002,4,"middle",true)
	narrativeSource(t,db,810003,6,"last",true)
	narrativeSource(t,db,810004,10,"no outbox",false)
	// Insert in reverse chapter order; same-chapter tie is confirmation id ASC.
	for _, input := range []map[string]any{narrativeEvent(first,810003,"resolved","last"),narrativeEvent(first,810001,"planted","first"),narrativeEvent(first,810001,"progressing","second"),narrativeEvent(first,810002,"abandoned","middle")} {
		if _,err:=n.ConfirmEvent(ctx,input);err!=nil{t.Fatal(err)}
	}
	rows,err:=n.List(ctx)
	if err!=nil||len(rows)!=2||rows[0]["id"]!=first||rows[1]["id"]!=second||rows[0]["status"]!="resolved"||rows[0]["dormantChapters"]!=int64(0)||rows[0]["overdue"]!=false||rows[1]["status"]!="planned"||rows[1]["dormantChapters"]!=int64(8)||rows[1]["overdue"]!=true{t.Fatalf("list %v %v",rows,err)}
	events:=rows[0]["events"].([]map[string]any)
	if len(events)!=4||events[0]["type"]!="planted"||events[1]["type"]!="progressing"||events[2]["type"]!="abandoned"||events[3]["type"]!="resolved"{t.Fatalf("event order %v",events)}
	mustExec(t,db,"UPDATE drafts SET status='archived' WHERE id=810003")
	rows,err=n.List(ctx)
	if err!=nil||rows[0]["status"]!="abandoned"||len(rows[0]["events"].([]map[string]any))!=3{t.Fatalf("superseded hidden %v %v",rows,err)}
	mustExec(t,db,"UPDATE drafts SET status='archived' WHERE id=810002")
	rows,err=n.List(ctx)
	if err!=nil||rows[0]["status"]!="progressing"||rows[0]["dormantChapters"]!=int64(7)||rows[0]["overdue"]!=true{t.Fatalf("max includes finalized no-outbox %v %v",rows,err)}
	if narrativeCount(t,db,"narrative_thread_confirmations")!=4{t.Fatal("hiding superseded events must not delete history")}
	// Legacy arbitrary statuses/raw SQL values are projected, not normalized.
	mustExec(t,db,"PRAGMA ignore_check_constraints=ON; UPDATE narrative_thread_confirmations SET event_type='legacy-status' WHERE draft_id=810001; PRAGMA ignore_check_constraints=OFF")
	mustExec(t,db,"UPDATE narrative_thread_plans SET title=CAST('raw-title' AS BLOB) WHERE id=?",first)
	rows,err=n.List(ctx)
	if err!=nil||rows[0]["status"]!="legacy-status"||!bytes.Equal(rows[0]["title"].([]byte),[]byte("raw-title"))||rows[0]["dormantChapters"]!=int64(7){t.Fatalf("legacy/raw values %v %v",rows,err)}
	contentsBefore,draftsBefore,outboxBefore:=narrativeCount(t,db,"contents"),narrativeCount(t,db,"drafts"),narrativeCount(t,db,"finalization_outbox")
	if err:=n.DeletePlan(ctx,first);err!=nil{t.Fatal(err)}
	if narrativeCount(t,db,"narrative_thread_confirmations")!=0||narrativeCount(t,db,"narrative_thread_plans")!=1||narrativeCount(t,db,"contents")!=contentsBefore||narrativeCount(t,db,"drafts")!=draftsBefore||narrativeCount(t,db,"finalization_outbox")!=outboxBefore{t.Fatal("delete cascade crossed ownership boundary")}
}

func TestNarrativeRelevantInclusiveAndTextOutsideWindow(t *testing.T) {
	db,n,_:=narrativeFixture(t);ctx:=context.Background();first:=narrativeCreate(t,n)
	narrativeSource(t,db,810001,3,"秘密证词",true)
	if _,err:=n.ConfirmEvent(ctx,narrativeEvent(first,810001,"planted","秘密证词"));err!=nil{t.Fatal(err)}
	for _,chapter:=range []int{2,5}{rows,err:=n.Relevant(ctx,map[string]any{"chapterNumber":chapter,"title":"","keyEvents":"","characters":[]string{}});if err!=nil||len(rows)!=1{t.Fatalf("inclusive range %d %v %v",chapter,rows,err)}}
	for _,input:=range []map[string]any{
		{"chapterNumber":99,"title":"","keyEvents":"","characters":[]string{"\ufeff 阿青 \ufeff"}},
		{"chapterNumber":99,"title":"","keyEvents":"","characters":[]any{" 秘密证词 "}},
		{"chapterNumber":99,"title":"关于暗门😀的故事","keyEvents":"","characters":[]string{}},
		{"chapterNumber":1,"title":"","keyEvents":"暗门😀出现","characters":[]string{}},
	}{rows,err:=n.Relevant(ctx,input);if err!=nil||len(rows)!=1{t.Fatalf("outside-window text %v %v %v",input,rows,err)}}
	rows,err:=n.Relevant(ctx,map[string]any{"chapterNumber":99,"title":"unrelated","keyEvents":"","characters":[]string{"\ufeff ","\u0085阿青\u0085"}})
	if err!=nil||rows==nil||len(rows)!=0{t.Fatalf("blank/NEL character %v %v",rows,err)}
	input:=narrativePlan();input["title"]="😀";second,err:=n.CreatePlan(ctx,input);if err!=nil{t.Fatal(err)}
	rows,err=n.Relevant(ctx,map[string]any{"chapterNumber":99,"title":"😀","keyEvents":"","characters":[]string{}})
	if err!=nil||len(rows)!=1||rows[0]["id"]!=second["id"]{t.Fatalf("one astral title is two JS units %v %v",rows,err)}
	input["title"]="甲";if _,err:=n.UpdatePlan(ctx,second["id"].(int64),input);err!=nil{t.Fatal(err)}
	rows,err=n.Relevant(ctx,map[string]any{"chapterNumber":99,"title":"甲","keyEvents":"","characters":[]string{}})
	if err!=nil||len(rows)!=0{t.Fatalf("one BMP title insufficient %v %v",rows,err)}
	if _,err:=n.ConfirmEvent(ctx,narrativeEvent(first,810001,"resolved","秘密证词"));err!=nil{t.Fatal(err)}
	rows,err=n.Relevant(ctx,map[string]any{"chapterNumber":3,"title":"暗门😀","keyEvents":"","characters":[]string{"阿青"}})
	if err!=nil||len(rows)!=1||rows[0]["id"]!=second["id"]{t.Fatalf("terminal excluded before range/text %v %v",rows,err)}
	mustExec(t,db,"UPDATE narrative_thread_plans SET author_intent=CAST('Alice' AS BLOB),title=CAST('BlobTitle' AS BLOB) WHERE id=?",second["id"])
	for _,context:=range []map[string]any{{"chapterNumber":99,"title":"","keyEvents":"","characters":[]string{"Alice"}},{"chapterNumber":99,"title":"BlobTitle","keyEvents":"","characters":[]string{}}}{
		rows,err=n.Relevant(ctx,context)
		if err!=nil||len(rows)!=1||!bytes.Equal(rows[0]["title"].([]byte),[]byte("BlobTitle"))||!bytes.Equal(rows[0]["authorIntent"].([]byte),[]byte("Alice")){t.Fatalf("read matching stringifies Buffer without changing raw projection %v %v",rows,err)}
	}
}

func TestNarrativeExemptionsValidationUpsertAndStrictFlags(t *testing.T) {
	db,n,_:=narrativeFixture(t);ctx:=context.Background()
	if err:=n.SaveExemption(ctx," \ufeff"," ");!errors.Is(err,ErrConsistencyExemptionKeyInvalid){t.Fatalf("key before reason %v",err)}
	if err:=n.SaveExemption(ctx,"key"," \ufeff");!errors.Is(err,ErrConsistencyExemptionReasonInvalid){t.Fatal(err)}
	for _,test:=range []struct{key,reason string;want error}{{strings.Repeat("😀",250)+"x","reason",ErrConsistencyExemptionKeyInvalid},{"key",strings.Repeat("😀",250)+"x",ErrConsistencyExemptionReasonInvalid}}{if err:=n.SaveExemption(ctx,test.key,test.reason);!errors.Is(err,test.want){t.Fatal(err)}}
	if err:=n.SaveExemption(ctx," \ufeffb\ufeff "," \ufeff原因😀\ufeff ");err!=nil{t.Fatal(err)}
	if err:=n.SaveExemption(ctx,"a",strings.Repeat("😀",250));err!=nil{t.Fatal(err)}
	if err:=n.RevokeExemption(ctx,"missing");err!=nil{t.Fatalf("missing revoke no-op %v",err)}
	if err:=n.RevokeExemption(ctx," b ");err!=nil{t.Fatal(err)}
	rows,err:=n.Exemptions(ctx)
	if err!=nil||len(rows)!=2||rows[0]["stableFactKey"]!="a"||rows[0]["revoked"]!=false||rows[1]["reason"]!="原因😀"||rows[1]["revoked"]!=true{t.Fatalf("sorted flags %v %v",rows,err)}
	if err:=n.SaveExemption(ctx,"b","new");err!=nil{t.Fatal(err)}
	rows,err=n.Exemptions(ctx);if err!=nil||rows[1]["revoked"]!=false||rows[1]["reason"]!="new"{t.Fatalf("upsert resets revoke %v %v",rows,err)}
	if err:=n.SaveExemption(ctx,"\u0085","\u0085");err!=nil{t.Fatalf("NEL is not blank %v",err)}
	if err:=n.RevokeExemption(ctx,"\ufeff");!errors.Is(err,ErrConsistencyExemptionKeyInvalid){t.Fatal(err)}
	// Corrupt legacy storage is explicitly synthetic: temporarily bypass CHECK,
	// then assert strict ===1 behavior instead of truthiness or suffix matching.
	mustExec(t,db,"PRAGMA ignore_check_constraints=ON; UPDATE consistency_exemptions SET revoked=2 WHERE stable_fact_key='a'; PRAGMA ignore_check_constraints=OFF")
	mustExec(t,db,"UPDATE consistency_exemptions SET reason=CAST('raw-reason' AS BLOB) WHERE stable_fact_key='b'")
	rows,err=n.Exemptions(ctx)
	if err!=nil{t.Fatal(err)}
	for _,row:=range rows{if row["stableFactKey"]=="a"&&row["revoked"]!=false{t.Fatal("truthy legacy flag accepted")};if row["stableFactKey"]=="b"&&!bytes.Equal(row["reason"].([]byte),[]byte("raw-reason")){t.Fatal("BLOB coerced")}}
}

func TestNarrativeImmediateTriggerRollbackAndPoolRecovery(t *testing.T) {
	for _,action:=range []string{"ABORT","ROLLBACK"}{t.Run(action,func(t *testing.T){
		db,n,_:=narrativeFixture(t);db.SetMaxOpenConns(1);ctx:=context.Background();id:=narrativeCreate(t,n)
		narrativeSource(t,db,810001,3,"evidence",true)
		mustExec(t,db,fmt.Sprintf(`CREATE TRIGGER narrative_block_create AFTER INSERT ON narrative_thread_plans
 BEGIN SELECT RAISE(%s,'create blocked'); END`,action))
		before:=narrativeCount(t,db,"narrative_thread_plans")
		if _,err:=n.CreatePlan(ctx,narrativePlan());err==nil||!strings.Contains(err.Error(),"create blocked"){t.Fatalf("create trigger %v",err)}
		if narrativeCount(t,db,"narrative_thread_plans")!=before{t.Fatal("create trigger left write")}
		mustExec(t,db,"DROP TRIGGER narrative_block_create")
		mustExec(t,db,fmt.Sprintf(`CREATE TRIGGER narrative_block_update AFTER UPDATE ON narrative_thread_plans
 BEGIN SELECT RAISE(%s,'update blocked'); END`,action))
		input:=narrativePlan();input["title"]="changed"
		if _,err:=n.UpdatePlan(ctx,id,input);err==nil||!strings.Contains(err.Error(),"update blocked"){t.Fatalf("update trigger %v",err)}
		row,err:=n.GetPlan(ctx,id);if err!=nil||row["title"]!="暗门😀"{t.Fatalf("partial update %v %v",row,err)}
		mustExec(t,db,"DROP TRIGGER narrative_block_update")
		mustExec(t,db,fmt.Sprintf(`CREATE TRIGGER narrative_block_event AFTER INSERT ON narrative_thread_confirmations
 BEGIN UPDATE narrative_thread_plans SET title='partial' WHERE id=NEW.plan_id;
 SELECT RAISE(%s,'event blocked'); END`,action))
		if _,err:=n.ConfirmEvent(ctx,narrativeEvent(id,810001,"planted","evidence"));err==nil||!strings.Contains(err.Error(),"event blocked"){t.Fatalf("event trigger %v",err)}
		if narrativeCount(t,db,"narrative_thread_confirmations")!=0{t.Fatal("partial event survived")}
		row,err=n.GetPlan(ctx,id);if err!=nil||row["title"]!="暗门😀"{t.Fatalf("trigger side effect survived %v %v",row,err)}
		mustExec(t,db,"DROP TRIGGER narrative_block_event")
		cancelled,cancel:=context.WithCancel(ctx);cancel()
		if _,err:=n.ConfirmEvent(cancelled,narrativeEvent(id,810001,"planted","evidence"));err==nil{t.Fatal("cancelled confirm succeeded")}
		if _,err:=n.CreatePlan(cancelled,narrativePlan());err==nil{t.Fatal("cancelled create succeeded")}
		recovery,stop:=context.WithTimeout(ctx,5*time.Second);defer stop()
		if _,err:=n.ConfirmEvent(recovery,narrativeEvent(id,810001,"planted","evidence"));err!=nil{t.Fatalf("pool recovery %v",err)}
		if _,err:=n.List(recovery);err!=nil{t.Fatalf("single-connection list deadlock %v",err)}
		if _,err:=n.UpdatePlan(recovery,id,narrativePlan());err!=nil{t.Fatalf("single-connection update deadlock %v",err)}
	})}
}

func TestNarrativeConcurrentEventPlanDeletionAndConsistentReads(t *testing.T) {
	db,n,path:=narrativeFixture(t);ctx:=context.Background()
	// WAL is set only on this disposable test copy to permit genuine concurrent
	// reader/writer overlap; production repositories never change journal mode.
	mustExec(t,db,"PRAGMA journal_mode=WAL")
	narrativeSource(t,db,810001,3,"evidence",true)
	dsn,err:=ExistingDSN(path);if err!=nil{t.Fatal(err)}
	other,err:=sql.Open("sqlite3",dsn);if err!=nil{t.Fatal(err)};defer other.Close();second:=NewNarrative(other)
	for round:=0;round<12;round++{
		id:=narrativeCreate(t,n);start:=make(chan struct{});failures:=make(chan error,3);var group sync.WaitGroup
		group.Add(3)
		go func(){defer group.Done();<-start;_,err:=n.ConfirmEvent(ctx,narrativeEvent(id,810001,"planted","evidence"));if err!=nil&&!errors.Is(err,ErrNarrativeThreadPlanNotFound){failures<-err}}()
		go func(){defer group.Done();<-start;if err:=second.DeletePlan(ctx,id);err!=nil{failures<-err}}()
		go func(){defer group.Done();<-start;rows,err:=second.List(ctx);if err!=nil{failures<-err;return};for _,row:=range rows{events:=row["events"].([]map[string]any);if row["status"]=="planned"&&len(events)!=0{failures<-fmt.Errorf("hybrid planned view: %v",row)};if row["status"]=="planted"&&(len(events)!=1||events[0]["planId"]!=row["id"]){failures<-fmt.Errorf("hybrid event view: %v",row)}}}()
		close(start);group.Wait();close(failures);for err:=range failures{t.Fatal(err)}
		if narrativeCount(t,db,"narrative_thread_confirmations")!=0{t.Fatal("orphan event survived concurrent plan delete")}
	}
	id:=narrativeCreate(t,n)
	if _,err:=n.ConfirmEvent(ctx,narrativeEvent(id,810001,"planted","evidence"));err!=nil{t.Fatal(err)}
	mustExec(t,db,"UPDATE narrative_thread_plans SET title='on' WHERE id=?",id)
	failures:=make(chan error,2);start:=make(chan struct{});var group sync.WaitGroup;group.Add(2)
	go func(){defer group.Done();<-start;for i:=0;i<40;i++{err:=NewReviewRevisions(other).immediate(ctx,func(conn *sql.Conn)error{title,status:="on","finalized";if i%2==0{title,status="off","archived"};if _,err:=conn.ExecContext(ctx,"UPDATE narrative_thread_plans SET title=? WHERE id=?",title,id);err!=nil{return err};_,err:=conn.ExecContext(ctx,"UPDATE drafts SET status=? WHERE id=810001",status);return err});if err!=nil{failures<-err;return}}}()
	go func(){defer group.Done();<-start;for i:=0;i<40;i++{rows,err:=n.List(ctx);if err!=nil{failures<-err;return};if len(rows)!=1{failures<-fmt.Errorf("plan disappeared %v",rows);return};row:=rows[0];if row["title"]=="on"&&row["status"]!="planted"||row["title"]=="off"&&row["status"]!="planned"{failures<-fmt.Errorf("read transaction mixed atomic states %v",row);return}}}()
	close(start);group.Wait();close(failures);for err:=range failures{t.Fatal(err)}
}
