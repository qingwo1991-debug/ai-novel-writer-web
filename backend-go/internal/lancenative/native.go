//go:build lance_native && cgo
// Package lancenative is an isolated synthetic compatibility spike. It is not
// used by main, author admission or production repositories. Paths are trusted
// private CI copies; the caller must exclude all concurrent filesystem writers.
package lancenative
/*
#cgo CFLAGS: -I${SRCDIR}/../../native-lance/include
#include <stdlib.h>
#include "novel_lance.h"
*/
import "C"
import("context";"encoding/json";"errors";"os";"path/filepath";"strings";"unsafe")
var ErrInput=errors.New("LANCE_NATIVE_INPUT_INVALID")
var ErrNative=errors.New("LANCE_NATIVE_OPERATION_FAILED")
const maxManifest=1024*1024
// Table describes a pinned current snapshot. Import creates a fresh history;
// it does not claim to recreate historical versions or migrate author assets.
type Table struct{Name string `json:"name"`;Version uint64 `json:"version"`;Rows uint64 `json:"rows"`;File string `json:"file"`}
type Manifest struct{Tables []Table `json:"tables"`}
func validPath(path string)bool{return filepath.IsAbs(path)&&filepath.Clean(path)==path&&!strings.ContainsAny(path,"\x00\\")}
func pathBytes(path string)(unsafe.Pointer,C.size_t,func(),error){if !validPath(path){return nil,0,nil,ErrInput};value:=C.CBytes([]byte(path));return value,C.size_t(len(path)),func(){C.free(value)},nil}
func decode(status C.int32_t,result *C.NvBytes,diagnostic *C.NvBytes)(Manifest,error){defer C.nv_lance_free(result);defer C.nv_lance_free(diagnostic);if status!=0{return Manifest{},ErrNative};if result.len==0||result.len>maxManifest||result.ptr==nil{return Manifest{},ErrNative};raw:=C.GoBytes(unsafe.Pointer(result.ptr),C.int(result.len));var manifest Manifest;if err:=json.Unmarshal(raw,&manifest);err!=nil||manifest.Tables==nil{return Manifest{},ErrNative};return manifest,nil}
// Native calls are synchronous and cannot be interrupted through Go context.
// Cancellation is checked before dispatch and after native work has completed;
// no goroutine returns early or destroys native resources still in use.
func Export(ctx context.Context,path,out string)(Manifest,error){if ctx==nil{return Manifest{},ErrInput};if err:=ctx.Err();err!=nil{return Manifest{},err};if path==out{return Manifest{},ErrInput};p,plen,free,err:=pathBytes(path);if err!=nil{return Manifest{},err};defer free();o,olen,freeOut,err:=pathBytes(out);if err!=nil{return Manifest{},err};defer freeOut();var result,diagnostic C.NvBytes;status:=C.nv_lance_export((*C.uint8_t)(p),plen,(*C.uint8_t)(o),olen,&result,&diagnostic);manifest,err:=decode(status,&result,&diagnostic);if err!=nil{return Manifest{},err};if err:=ctx.Err();err!=nil{return Manifest{},err};return manifest,nil}
func Import(ctx context.Context,staging,input string,manifest Manifest)(Manifest,error){if ctx==nil{return Manifest{},ErrInput};if err:=ctx.Err();err!=nil{return Manifest{},err};if staging==input{return Manifest{},ErrInput};s,slen,freeStage,err:=pathBytes(staging);if err!=nil{return Manifest{},err};defer freeStage();i,ilen,freeInput,err:=pathBytes(input);if err!=nil{return Manifest{},err};defer freeInput();raw,err:=json.Marshal(manifest);if err!=nil||len(raw)>maxManifest{return Manifest{},ErrInput};body:=C.CBytes(raw);defer C.free(body);var result,diagnostic C.NvBytes;status:=C.nv_lance_import((*C.uint8_t)(s),slen,(*C.uint8_t)(i),ilen,(*C.uint8_t)(body),C.size_t(len(raw)),&result,&diagnostic);restored,err:=decode(status,&result,&diagnostic);if err!=nil{return Manifest{},err};if err:=ctx.Err();err!=nil{return Manifest{},err};return restored,nil}
// EnsureExistingCopy validates a physical directory before invoking an API that
// could otherwise create absent database directories. Rust repeats this guard.
func EnsureExistingCopy(path string)error{if !validPath(path){return ErrInput};for current:=path;;current=filepath.Dir(current){info,err:=os.Lstat(current);if err!=nil||!info.IsDir()||info.Mode()&os.ModeSymlink!=0{return ErrInput};if current==string(filepath.Separator){return nil}}}
