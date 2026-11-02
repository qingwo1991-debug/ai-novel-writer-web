package admission

import (
 "context"
 "errors"
 "io"
 "os"
 "path/filepath"
 "reflect"
 "sort"
 "strconv"
 "strings"
 "testing"
)
func TestExternalRowSortMultiLevelMatchesJSOrderAndPrivateCleanup(t *testing.T){
 sorter:=newRowSorter(context.Background());parent:=t.TempDir();sorter.parent=parent;sorter.chunkBytes=48;sorter.chunkRows=3;defer sorter.Close()
 expected:=[]string{"𐀀","\ue000","a","a","中文","😀","<>&\u2028\u2029","","z"}
 for i:=0;i<240;i++{expected=append(expected,strconv.Itoa((i*97)%113)+strings.Repeat("x",i%9))}
 for _,row:=range expected{if err:=sorter.Add(row);err!=nil{t.Fatal(err)}}
 if sorter.directory==""{t.Fatal("did not spill")};info,err:=os.Stat(sorter.directory);if err!=nil||info.Mode().Perm()!=0700{t.Fatal("private directory",err)}
 entries,err:=os.ReadDir(sorter.directory);if err!=nil{t.Fatal(err)};for _,entry:=range entries{info,err:=entry.Info();if err!=nil||info.Mode().Perm()!=0600{t.Fatal("private run",err)}}
 actual:=[]string{};if err:=sorter.Visit(func(row string)error{actual=append(actual,row);return nil});err!=nil{t.Fatal(err)}
 sort.Slice(expected,func(i,j int)bool{return lessUTF16(expected[i],expected[j])});if !reflect.DeepEqual(actual,expected){t.Fatal("spilled merge order differs")}
 if sorter.count!=int64(len(expected)){t.Fatal("lost duplicate rows")};if err:=sorter.Close();err!=nil{t.Fatal(err)}
 entries,err=os.ReadDir(parent);if err!=nil||len(entries)!=0{t.Fatal("plaintext run leaked",entries,err)}
}
func TestExternalRowSortCancellationVisitorAndCorruptRun(t *testing.T){
 ctx,cancel:=context.WithCancel(context.Background());sorter:=newRowSorter(ctx);sorter.parent=t.TempDir();sorter.chunkRows=1
 for _,row:=range []string{"b","a","c"}{if err:=sorter.Add(row);err!=nil{t.Fatal(err)}}
 cancel();if err:=sorter.Visit(func(string)error{return nil});!errors.Is(err,context.Canceled){t.Fatal(err)};if err:=sorter.Close();err!=nil{t.Fatal(err)}
 sorter=newRowSorter(context.Background());sorter.parent=t.TempDir();sorter.chunkRows=1;defer sorter.Close()
 for _,row:=range []string{"b","a"}{if err:=sorter.Add(row);err!=nil{t.Fatal(err)}}
 failure:=errors.New("synthetic visitor fail");if err:=sorter.Visit(func(string)error{return failure});!errors.Is(err,failure){t.Fatal(err)}
 if err:=os.WriteFile(sorter.levels[0][0].path,[]byte{0,1},0600);err!=nil{t.Fatal(err)}
 if err:=sorter.Visit(func(string)error{return nil});!errors.Is(err,io.ErrUnexpectedEOF){t.Fatal("corrupt run accepted",err)}
 if err:=os.Truncate(sorter.levels[0][0].path,0);err!=nil{t.Fatal(err)}
 if err:=sorter.Visit(func(string)error{return nil});!errors.Is(err,errRowSort){t.Fatal("whole-record truncation accepted",err)}
}
func TestExternalRowSortIOFailureAndRowLimit(t *testing.T){
 sorter:=newRowSorter(context.Background());sorter.parent=filepath.Join(t.TempDir(),"missing");sorter.chunkRows=1;defer sorter.Close()
 if err:=sorter.Add("one");err!=nil{t.Fatal(err)};if err:=sorter.Add("two");err==nil{t.Fatal("missing scratch accepted")}
 sorter=newRowSorter(context.Background());if err:=sorter.Add(strings.Repeat("x",maxEncodedRowBytes+1));err==nil{t.Fatal("oversize row accepted")}
}
