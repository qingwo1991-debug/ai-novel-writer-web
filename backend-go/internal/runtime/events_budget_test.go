package runtime

import (
	"strings"
	"testing"
)

func TestLargeSlowSubscriberIsEvictedByBytesBeforeCount(t *testing.T) {
	hub := NewEventHub()
	defer hub.Close()
	_, queue, cancel, err := hub.Subscribe(0)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	payload := strings.Repeat("x", 1024*1024)
	for range 8 {
		if err := hub.Publish("synthetic", []any{payload}); err != nil {
			t.Fatal(err)
		}
	}
	if hub.Connections() != 0 {
		t.Fatal("slow subscriber exceeded 8MiB retained delivery budget")
	}
	total := 0
	for event := range queue {
		total += len(event.Payload)
		event.Done()
		event.Done()
	}
	if total > 8*1024*1024 {
		t.Fatal("live queue budget", total)
	}
}
func TestAcknowledgedEventsReclaimByteBudget(t *testing.T) {
	hub := NewEventHub()
	defer hub.Close()
	_, queue, cancel, err := hub.Subscribe(0)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	payload := strings.Repeat("x", 1024*1024)
	for range 24 {
		if err := hub.Publish("synthetic", []any{payload}); err != nil {
			t.Fatal(err)
		}
		event := <-queue
		event.Done()
		event.Done()
	}
	if hub.Connections() != 1 {
		t.Fatal("fast subscriber lost despite acknowledgement")
	}
}
