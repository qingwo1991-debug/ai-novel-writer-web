package runtime

import (
 "encoding/json"
 "errors"
 "strconv"
 "sync"
)

var ErrEventLimit=errors.New("EVENT_CONNECTION_LIMIT")
var ErrEventSize=errors.New("EVENT_TOO_LARGE")
var ErrHubClosed=errors.New("EVENT_HUB_CLOSED")

type Event struct { ID uint64; Payload []byte }
func (e Event) Wire() []byte {return append(append([]byte("id: "+strconv.FormatUint(e.ID,10)+"\ndata: "),e.Payload...),[]byte("\n\n")...)}
// EventHub is private to one session+tab. Callers must redact credentials before
// publishing. It retains at most 2048 events and 8MiB total, and disconnects slow
// readers instead of allowing an unbounded queue to block a model dispatcher.
type EventHub struct {
 mu sync.Mutex
 sequence uint64
 events []Event
 bytes int
 subscribers map[uint64]chan Event
 next uint64
 closed bool
}
func NewEventHub()*EventHub{return &EventHub{subscribers:make(map[uint64]chan Event)}}
func(h *EventHub)Publish(channel string,args []any)error{
 payload,err:=json.Marshal(struct{Channel string `json:"channel"`; Args []any `json:"args"`}{channel,args});if err!=nil{return err}
 if len(payload)>8*1024*1024{return ErrEventSize}
 h.mu.Lock();defer h.mu.Unlock();if h.closed{return ErrHubClosed}
 h.sequence++;event:=Event{ID:h.sequence,Payload:payload};h.events=append(h.events,event);h.bytes+=len(payload)
 for len(h.events)>2048||h.bytes>8*1024*1024{h.bytes-=len(h.events[0].Payload);h.events[0]=Event{};h.events=h.events[1:]}
 for id,subscriber:=range h.subscribers{select{case subscriber<-cloneEvent(event):default:close(subscriber);delete(h.subscribers,id)}}
 return nil
}
func cloneEvent(event Event)Event{return Event{ID:event.ID,Payload:append([]byte(nil),event.Payload...)}}
// Subscribe snapshots replay and registers the live queue atomically: an event
// cannot fall between the history read and a newly connected reader.
func(h *EventHub)Subscribe(after uint64)([]Event,<-chan Event,func(),error){
 h.mu.Lock();defer h.mu.Unlock();if h.closed{return nil,nil,nil,ErrHubClosed}
 if len(h.subscribers)>=3{return nil,nil,nil,ErrEventLimit}
 replay:=[]Event{};if after>0{for _,event:=range h.events{if event.ID>after{replay=append(replay,cloneEvent(event))}}}
 h.next++;id:=h.next;queue:=make(chan Event,64);h.subscribers[id]=queue
 var once sync.Once
 cancel:=func(){once.Do(func(){h.mu.Lock();defer h.mu.Unlock();if current,ok:=h.subscribers[id];ok{close(current);delete(h.subscribers,id)}})}
 return replay,queue,cancel,nil
}
func(h *EventHub)Connections()int{h.mu.Lock();defer h.mu.Unlock();return len(h.subscribers)}
func(h *EventHub)Close(){h.mu.Lock();defer h.mu.Unlock();if h.closed{return};h.closed=true;for id,queue:=range h.subscribers{close(queue);delete(h.subscribers,id)};h.events=nil;h.bytes=0}
