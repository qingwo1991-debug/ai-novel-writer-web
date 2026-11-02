package llm

import (
 "bufio"
 "context"
 "errors"
 "io"
 "strings"
)
var errSSELimit=errors.New("LLM_STREAM_TOO_LARGE")
const maxSSEEventBytes=8*1024*1024
// Parse incremental SSE framing (CR/LF/CRLF, comments, multiline data and final
// unterminated event). A false callback terminates reading immediately at DONE
// or a fatal provider event; trailing data is not processed.
func readProviderSSE(ctx context.Context,source io.Reader,event func(string)bool)error{
 reader:=bufio.NewReaderSize(source,32*1024);line:=make([]byte,0,1024);data:=[]string{};eventBytes:=0;wireBytes:=0;afterCR:=false
 processLine:=func(raw []byte)bool{
  text:=strings.ToValidUTF8(string(raw),"\ufffd")
  if len(text)==0{if len(data)>0{value:=strings.Join(data,"\n");data=nil;eventBytes=0;return event(value)};return true}
  if strings.HasPrefix(text,":"){return true};field,value,found:=strings.Cut(text,":");if field!="data"{return true};if !found{value=""};value=strings.TrimPrefix(value," ");data=append(data,value);eventBytes+=len(value)+1;return true
 }
 for{
  if err:=ctx.Err();err!=nil{return err};b,err:=reader.ReadByte()
  if err!=nil{if err!=io.EOF{return err};if len(line)>0{if !processLine(line){return nil}};if eventBytes>maxSSEEventBytes{return errSSELimit};if len(data)>0{event(strings.Join(data,"\n"))};return nil}
  wireBytes++;if wireBytes>maxProviderBytes{return errSSELimit}
  if afterCR{afterCR=false;if b=='\n'{continue}}
  if b=='\r'||b=='\n'{if !processLine(line){return nil};line=line[:0];if eventBytes>maxSSEEventBytes{return errSSELimit};afterCR=b=='\r';continue}
  line=append(line,b);if len(line)+eventBytes>maxSSEEventBytes{return errSSELimit}
 }
}
