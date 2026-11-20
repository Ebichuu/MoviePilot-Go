package events

import (
	"context"
	"sync"
	"testing"
	"time"
)

type recorder struct {
	mu     sync.Mutex
	events []Event
}

func (r *recorder) handler(_ context.Context, event Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}

func (r *recorder) len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.events)
}

func (r *recorder) first() Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.events) == 0 {
		return Event{}
	}
	return r.events[0]
}

func waitFor(t *testing.T, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not met within timeout")
}

func TestBusDeliversToEverySubscriber(t *testing.T) {
	t.Parallel()
	bus := New(nil, 8)
	defer bus.Close()

	first := &recorder{}
	second := &recorder{}
	bus.Subscribe("first", first.handler)
	bus.Subscribe("second", second.handler)

	bus.Publish(Event{Type: TypeDownloadCompleted, Username: "admin"})

	waitFor(t, func() bool { return first.len() == 1 && second.len() == 1 })
	event := first.first()
	if event.Type != TypeDownloadCompleted || event.Username != "admin" {
		t.Fatalf("first event = %#v", event)
	}
	if event.At.IsZero() {
		t.Fatalf("event timestamp was not filled in")
	}
	handled, dropped := bus.Stats()
	if handled != 2 || dropped != 0 {
		t.Fatalf("stats = (%d, %d), want (2, 0)", handled, dropped)
	}
}

func TestBusDropsWhenSubscriberOverloaded(t *testing.T) {
	t.Parallel()
	bus := New(nil, 1)
	defer bus.Close()

	release := make(chan struct{})
	bus.Subscribe("slow", func(context.Context, Event) { <-release })
	for i := 0; i < 8; i++ {
		bus.Publish(Event{Type: TypeOrganizationCompleted})
	}
	close(release)

	waitFor(t, func() bool {
		_, dropped := bus.Stats()
		return dropped > 0
	})
}

func TestBusRecoversHandlerPanic(t *testing.T) {
	t.Parallel()
	bus := New(nil, 4)
	defer bus.Close()

	healthy := &recorder{}
	bus.Subscribe("panics", func(context.Context, Event) { panic("boom") })
	bus.Subscribe("healthy", healthy.handler)

	bus.Publish(Event{Type: TypeSubscribeCompleted})
	waitFor(t, func() bool { return healthy.len() == 1 })
}

func TestBusIgnoresPublishAfterClose(t *testing.T) {
	t.Parallel()
	bus := New(nil, 4)
	counter := &recorder{}
	bus.Subscribe("counter", counter.handler)
	bus.Close()
	bus.Publish(Event{Type: TypeDownloadCompleted})
	if counter.len() != 0 {
		t.Fatalf("handled after close = %d, want 0", counter.len())
	}
}
