package runtime

import (
 "strings"
 "sync"
 "testing"
)
func TestEventWireAndReplayAreBrowserCompatible(t *testing.T){
 hub:=NewEventHub();if err:=hub.Publish("llm:stream-chunk",[]any{map[string]any{"chunk":"中文","requestId":"synthetic"}});err!=nil{t.Fatal(err)}
 if err:=hub.Publish("llm:stream-done",[]any{});err!=nil{t.Fatal(err)}
 history,live,cancel,err:=hub.Subscribe(1);if err!=nil{t.Fatal(err)};defer cancel()
 if len(history)!=1||history[0].ID!=2||!strings.HasPrefix(string(history[0].Wire()),"id: 2\ndata: {\"channel\":\"llm:stream-done\",\"args\":[]}"){t.Fatalf("bad replay %v",history)}
 if err:=hub.Publish("browser:project-lost",[]any{map[string]string{"code":"TEST"}});err!=nil{t.Fatal(err)}
 if event:=<-live;event.ID!=3{t.Fatal(event)}
}
func TestEventHistoryBoundAndDefensiveCopy(t *testing.T){
 hub:=NewEventHub();for range 2100{if err:=hub.Publish("test",[]any{});err!=nil{t.Fatal(err)}}
 history,_,cancel,err:=hub.Subscribe(1);if err!=nil{t.Fatal(err)};cancel()
 if len(history)!=2048||history[0].ID!=53||history[len(history)-1].ID!=2100{t.Fatal(len(history))}
 history[0].Payload[0]='X'
 again,_,cancel,err:=hub.Subscribe(52);if err!=nil{t.Fatal(err)};defer cancel()
 if again[0].Payload[0]!='{'{t.Fatal("replay mutated internal history")}
}
func TestSlowSubscribersDisconnectAndConnectionsCap(t *testing.T){
 hub:=NewEventHub();_,queue,cancel,err:=hub.Subscribe(0);if err!=nil{t.Fatal(err)};defer cancel()
 _,_,two,_:=hub.Subscribe(0);defer two();_,_,three,_:=hub.Subscribe(0);defer three()
 if _,_,_,err:=hub.Subscribe(0);err!=ErrEventLimit{t.Fatal(err)}
 for range 65{if err:=hub.Publish("test",[]any{});err!=nil{t.Fatal(err)}}
 if hub.Connections()!=0{t.Fatal("slow readers retained")};for range queue{}
}
func TestPublishAndSubscribeConcurrent(t *testing.T){
 hub:=NewEventHub();var wg sync.WaitGroup
 for range 8{wg.Go(func(){for range 100{_ = hub.Publish("test",[]any{})}})}
 for range 2{wg.Go(func(){for range 30{_,_,cancel,err:=hub.Subscribe(1);if err==nil{cancel()}}})}
 wg.Wait();hub.Close();hub.Close();if err:=hub.Publish("closed",[]any{});err!=ErrHubClosed{t.Fatal(err)}
}
