package store

import (
 "context"
 "errors"
 "testing"
)
func TestDraftCASRejectsAliasedContentAndRawBlob(t *testing.T){
 for _,kind:=range []string{"finalized-draft","review","revision","blob"}{t.Run(kind,func(t *testing.T){db,drafts,_:=newFixture(t);seed(t,db,1,1,1,"draft","exact old body")
  switch kind{
  case "finalized-draft":mustExec(t,db,"INSERT INTO drafts(id,chapter_number,version,status,content_id) VALUES(2,2,1,'finalized',1)")
  case "review":mustExec(t,db,"CREATE TABLE reviews(id INTEGER PRIMARY KEY,content_id INTEGER NOT NULL)");mustExec(t,db,"INSERT INTO reviews(id,content_id) VALUES(2,1)")
  case "revision":mustExec(t,db,"CREATE TABLE revisions(id INTEGER PRIMARY KEY,content_id INTEGER NOT NULL)");mustExec(t,db,"INSERT INTO revisions(id,content_id) VALUES(2,1)")
  case "blob":mustExec(t,db,"UPDATE contents SET body=CAST(body AS BLOB) WHERE id=1")
  }
  expected:="exact old body";err:=drafts.UpdateContentIfCurrent(context.Background(),1,"must not overwrite",8,&expected)
  if kind=="blob"{if !errors.Is(err,ErrDraftContentConflict){t.Fatal(err)}}else if !errors.Is(err,ErrContentOwnershipConflict){t.Fatal(err)}
  var body,storage string;if err:=db.QueryRow("SELECT CAST(body AS TEXT),typeof(body) FROM contents WHERE id=1").Scan(&body,&storage);err!=nil{t.Fatal(err)};if body!=expected{t.Fatal("shared or non-text content overwritten",body)};if kind=="blob"&&storage!="blob"{t.Fatal("unsafe implicit conversion",storage)}
  var count int;if err:=db.QueryRow("SELECT word_count FROM drafts WHERE id=1").Scan(&count);err!=nil||count!=11{t.Fatal("metadata changed",count,err)}
 })}
}
