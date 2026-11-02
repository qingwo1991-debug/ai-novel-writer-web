package admission

import (
 "bufio"
 "container/heap"
 "context"
 "encoding/binary"
 "errors"
 "io"
 "os"
 "sort"
)

// Row sorting spills private encoded rows instead of retaining the whole author
// database. A single encoded row is still capped at 64MiB; a table/database is
// not. Runs are merged at bounded fan-in with logarithmic live run metadata.
const rowSortChunkBytes=4*1024*1024
const rowSortChunkRows=4096
const rowSortFanIn=8
const maxEncodedRowBytes=64*1024*1024
var errRowSort=errors.New("SCHEMA_DOMAIN_SORT_FAILED")
type rowRun struct{path string;count int64}
type rowSorter struct{
 ctx context.Context
 parent string
 directory string
 rows []string
 bytes int
 chunkBytes int
 chunkRows int
 levels [][]rowRun
 count int64
}
func newRowSorter(ctx context.Context)*rowSorter{return &rowSorter{ctx:ctx,chunkBytes:rowSortChunkBytes,chunkRows:rowSortChunkRows}}
func(s *rowSorter)Close()error{if s.directory==""{return nil};err:=os.RemoveAll(s.directory);if err==nil{s.directory=""};return err}
func(s *rowSorter)Add(row string)error{
 if err:=s.ctx.Err();err!=nil{return err};if len(row)>maxEncodedRowBytes{return errors.New("SCHEMA_DOMAIN_ROW_TOO_LARGE")}
 // Spill BEFORE adding a row that would exceed the chunk, except a single
 // oversized chunk row which remains bounded by maxEncodedRowBytes.
 if len(s.rows)>0&&(s.bytes+len(row)>s.chunkBytes||len(s.rows)>=s.chunkRows){if err:=s.flush();err!=nil{return err}}
 s.rows=append(s.rows,row);s.bytes+=len(row);s.count++;return nil
}
func(s *rowSorter)createRun()(*os.File,error){
 if s.directory==""{directory,err:=os.MkdirTemp(s.parent,"novel-go-row-sort-");if err!=nil{return nil,err};s.directory=directory;if err:=os.Chmod(directory,0700);err!=nil{return nil,err}}
 return os.CreateTemp(s.directory,"run-")
}
func writeRow(w io.Writer,row string)error{
 var size [8]byte;binary.BigEndian.PutUint64(size[:],uint64(len(row)));if _,err:=w.Write(size[:]);err!=nil{return err};_,err:=io.WriteString(w,row);return err
}
func readRow(r io.Reader)(string,error){
 var size [8]byte;if _,err:=io.ReadFull(r,size[:]);err!=nil{return "",err};length:=binary.BigEndian.Uint64(size[:]);if length>maxEncodedRowBytes{return "",errRowSort}
 data:=make([]byte,int(length));if _,err:=io.ReadFull(r,data);err!=nil{return "",err};return string(data),nil
}
func(s *rowSorter)flush()error{
 if len(s.rows)==0{return nil};if err:=s.ctx.Err();err!=nil{return err}
 sort.Slice(s.rows,func(i,j int)bool{return lessUTF16(s.rows[i],s.rows[j])})
 file,err:=s.createRun();if err!=nil{return err};writer:=bufio.NewWriterSize(file,128*1024)
 for _,row:=range s.rows{if err:=s.ctx.Err();err!=nil{file.Close();return err};if err:=writeRow(writer,row);err!=nil{file.Close();return err}}
 if err:=errors.Join(writer.Flush(),file.Close());err!=nil{return err}
 run:=rowRun{path:file.Name(),count:int64(len(s.rows))};s.rows=nil;s.bytes=0
 return s.carry(run,0)
}
func(s *rowSorter)carry(run rowRun,level int)error{
 for len(s.levels)<=level{s.levels=append(s.levels,nil)};s.levels[level]=append(s.levels[level],run)
 if len(s.levels[level])<rowSortFanIn{return nil}
 merged,err:=s.merge(s.levels[level]);if err!=nil{return err};s.levels[level]=nil;return s.carry(merged,level+1)
}
type rowHead struct{row string;index int}
type rowHeap []rowHead
func(h rowHeap)Len()int{return len(h)}
func(h rowHeap)Less(i,j int)bool{if h[i].row==h[j].row{return h[i].index<h[j].index};return lessUTF16(h[i].row,h[j].row)}
func(h rowHeap)Swap(i,j int){h[i],h[j]=h[j],h[i]}
func(h *rowHeap)Push(v any){*h=append(*h,v.(rowHead))}
func(h *rowHeap)Pop()any{old:=*h;v:=old[len(old)-1];*h=old[:len(old)-1];return v}
func(s *rowSorter)visitRuns(runs []rowRun,visit func(string)error)error{
 if len(runs)>rowSortFanIn{return errRowSort}
 files:=make([]*os.File,0,len(runs));defer func(){for _,file:=range files{file.Close()}}();readers:=make([]*bufio.Reader,0,len(runs));heads:=&rowHeap{};counts:=make([]int64,len(runs))
 for i,run:=range runs{
  if err:=s.ctx.Err();err!=nil{return err};file,err:=os.Open(run.path);if err!=nil{return err};files=append(files,file);reader:=bufio.NewReaderSize(file,128*1024);readers=append(readers,reader)
  row,err:=readRow(reader);if err==io.EOF{if run.count!=0{return errRowSort};continue};if err!=nil{return err};counts[i]++;heap.Push(heads,rowHead{row,i})
 }
 for heads.Len()>0{
  if err:=s.ctx.Err();err!=nil{return err};head:=heap.Pop(heads).(rowHead);if err:=visit(head.row);err!=nil{return err}
  row,err:=readRow(readers[head.index]);if err==io.EOF{if counts[head.index]!=runs[head.index].count{return errRowSort};continue};if err!=nil{return err};counts[head.index]++;if counts[head.index]>runs[head.index].count{return errRowSort};heap.Push(heads,rowHead{row,head.index})
 }
 return nil
}
func(s *rowSorter)merge(runs []rowRun)(rowRun,error){
 file,err:=s.createRun();if err!=nil{return rowRun{},err};writer:=bufio.NewWriterSize(file,128*1024)
 err=s.visitRuns(runs,func(row string)error{return writeRow(writer,row)})
 err=errors.Join(err,writer.Flush(),file.Close());if err!=nil{return rowRun{},err}
 for _,run:=range runs{if err:=os.Remove(run.path);err!=nil{return rowRun{},err}}
 var count int64;for _,run:=range runs{count+=run.count};return rowRun{path:file.Name(),count:count},nil
}
func(s *rowSorter)Visit(visit func(string)error)error{
 if err:=s.ctx.Err();err!=nil{return err}
 if s.directory==""{
  sort.Slice(s.rows,func(i,j int)bool{return lessUTF16(s.rows[i],s.rows[j])});for _,row:=range s.rows{if err:=s.ctx.Err();err!=nil{return err};if err:=visit(row);err!=nil{return err}};return nil
 }
 if err:=s.flush();err!=nil{return err};var runs []rowRun;for _,level:=range s.levels{runs=append(runs,level...)}
 for len(runs)>rowSortFanIn{
  next:=[]rowRun{};for len(runs)>0{n:=rowSortFanIn;if len(runs)<n{n=len(runs)};merged,err:=s.merge(runs[:n]);if err!=nil{return err};next=append(next,merged);runs=runs[n:]};runs=next
 }
 return s.visitRuns(runs,visit)
}
