package store

import (
 "context"
 "database/sql"
 "errors"
 "strings"
 "sync"
 "testing"
)
func plotRepositorySnapshot(revision any)map[string]any{return map[string]any{"version":1,"generatedAt":"2026-01-01T00:00:00.000Z","writingLanguage":"zh-CN","sourceRevision":revision,"tracks":[]any{map[string]any{"id":"main","title":"Main","role":"main","startChapter":1,"endChapter":1,"summary":"Summary","events":[]any{map[string]any{"status":"planned","chapterNumber":1,"summary":"Event","sources":[]any{map[string]any{"type":"blueprint","chapterNumber":1}}}}}}}}
func TestPlotTreeSaveCASRollbackAndConsistentRead(t *testing.T){
 ctx:=context.Background();db,_,path:=narrativeFixture(t);mustExec(t,db,"INSERT INTO blueprints(chapter_number,title,purpose,key_events)VALUES(1,'on','purpose','event'); UPDATE project_core SET synopsis='on',writing_language='zh-CN' WHERE id='main'");repository:=NewPlotTree(db);first,err:=repository.Read(ctx);if err!=nil{t.Fatal(err)};snapshot:=plotRepositorySnapshot(first["sourceRevision"]);if _,err:=repository.Save(ctx,snapshot,first["sourceRevision"].(string));err!=nil{t.Fatal(err)}
 mustExec(t,db,"UPDATE project_core SET synopsis='author edited' WHERE id='main'");if _,err:=repository.Save(ctx,snapshot,first["sourceRevision"].(string));!errors.Is(err,ErrPlotTreeSourcesChanged){t.Fatal("stale source saved",err)};var before string;if err:=db.QueryRow("SELECT plot_tree_snapshot FROM project_core WHERE id='main'").Scan(&before);err!=nil{t.Fatal(err)}
 current,err:=repository.Read(ctx);if err!=nil{t.Fatal(err)};snapshot["sourceRevision"]=current["sourceRevision"];mustExec(t,db,`CREATE TRIGGER reject_plot BEFORE UPDATE OF plot_tree_snapshot ON project_core BEGIN SELECT RAISE(ROLLBACK,'synthetic plot rollback'); END`);if _,err:=repository.Save(ctx,snapshot,current["sourceRevision"].(string));err==nil||!strings.Contains(err.Error(),"synthetic plot rollback"){t.Fatal(err)};var after string;if err:=db.QueryRow("SELECT plot_tree_snapshot FROM project_core WHERE id='main'").Scan(&after);err!=nil||after!=before{t.Fatal("partial plot save",after,err)};mustExec(t,db,"DROP TRIGGER reject_plot");cancelled,cancel:=context.WithCancel(ctx);cancel();if _,err:=repository.Save(cancelled,snapshot,current["sourceRevision"].(string));err==nil{t.Fatal("cancelled save succeeded")};if _,err:=repository.Save(ctx,snapshot,current["sourceRevision"].(string));err!=nil{t.Fatal("pool left unusable",err)}
 dsn,err:=ExistingDSN(path);if err!=nil{t.Fatal(err)};other,err:=sql.Open("sqlite3",dsn);if err!=nil{t.Fatal(err)};defer other.Close();mustExec(t,db,"UPDATE project_core SET synopsis='on' WHERE id='main'");start:=make(chan struct{});failures:=make(chan error,2);var group sync.WaitGroup;group.Add(2)
 go func(){defer group.Done();<-start;for i:=0;i<25;i++{state:="on";if i%2==0{state="off"};err:=NewReviewRevisions(other).immediate(ctx,func(conn *sql.Conn)error{if _,err:=conn.ExecContext(ctx,"UPDATE project_core SET synopsis=? WHERE id='main'",state);err!=nil{return err};_,err:=conn.ExecContext(ctx,"UPDATE blueprints SET title=? WHERE chapter_number=1",state);return err});if err!=nil{failures<-err;return}}}()
 go func(){defer group.Done();<-start;for i:=0;i<25;i++{value,err:=repository.Read(ctx);if err!=nil{failures<-err;return};if value["synopsis"].(map[string]any)["content"]!=value["blueprints"].([]map[string]any)[0]["title"]{failures<-errors.New("source aggregation mixed atomic transactions");return}}}();close(start);group.Wait();close(failures);for err:=range failures{t.Fatal(err)}
}
