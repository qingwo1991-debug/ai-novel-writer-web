// novel-server is the in-progress Go backend, not yet a production replacement.
package main

import (
 "context"
 "flag"
 "fmt"
 "log"
 "net"
 "net/http"
 "os"
 "os/signal"
 "syscall"
 "time"

 "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/policy"
 "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
 "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)
func main(){if err:=run();err!=nil{log.Fatal(err)}}
func run()error{
 address:=flag.String("listen","127.0.0.1:16082","migration-only loopback address")
 policyPath:=flag.String("policy","../web/operations.json","reviewed browser operation inventory")
 flag.Parse()
 if os.Getenv("NODE_ENV")=="production"||os.Getenv("NOVEL_DATA_ROOT")!=""{return fmt.Errorf("migration scaffold refuses production mode and data mounts")}
 host,_,err:=net.SplitHostPort(*address);if err!=nil{return err};ip:=net.ParseIP(host)
 if ip==nil||!ip.IsLoopback(){return fmt.Errorf("migration scaffold may listen only on a loopback IP")}
 file,err:=os.Open(*policyPath);if err!=nil{return err};defer file.Close()
 registry,err:=policy.Load(file);if err!=nil{return err}
 handler:=transport.New(registry,security.NewOrigins("http://127.0.0.1:16082"),"go-migration-uncommitted")
 defer handler.Close()
 // No database is opened and no domain operation is falsely bound to success.
 server:=&http.Server{Addr:*address,Handler:handler,ReadHeaderTimeout:30*time.Second,ReadTimeout:16*time.Minute,IdleTimeout:90*time.Second,MaxHeaderBytes:1024*1024}
 ctx,cancel:=signal.NotifyContext(context.Background(),syscall.SIGTERM,syscall.SIGINT);defer cancel()
 done:=make(chan error,1);go func(){done<-server.ListenAndServe()}()
 log.Printf("Go migration scaffold at %s; no author data; no Node fallback; not production-ready",*address)
 select{case err:=<-done:if err==http.ErrServerClosed{return nil};return err
 case<-ctx.Done():handler.Close();shutdown,cancel:=context.WithTimeout(context.Background(),10*time.Second);defer cancel();return server.Shutdown(shutdown)}
}
