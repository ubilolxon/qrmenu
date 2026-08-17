package realtime

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// These tests exist for the failure modes a hub gets wrong, not for the happy
// path: a send landing on a closed channel, a slow subscriber blocking the
// publisher, and events leaking between restaurants.

func TestPublishReachesSubscribers(t *testing.T) {
	t.Parallel()

	hub := New()
	room := uuid.New()

	a := hub.Subscribe(room)
	b := hub.Subscribe(room)
	defer hub.Unsubscribe(a)
	defer hub.Unsubscribe(b)

	if got := hub.Subscribers(room); got != 2 {
		t.Fatalf("Subscribers = %d, want 2", got)
	}

	if err := hub.Publish(room, EventOrderCreated, map[string]string{"code": "042"}); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	for name, sub := range map[string]*Subscriber{"a": a, "b": b} {
		event := receive(t, sub)
		if event.Type != EventOrderCreated {
			t.Errorf("%s: type = %q, want %q", name, event.Type, EventOrderCreated)
		}
		var payload map[string]string
		if err := json.Unmarshal(event.Data, &payload); err != nil {
			t.Fatalf("%s: payload does not decode: %v", name, err)
		}
		if payload["code"] != "042" {
			t.Errorf("%s: payload = %v", name, payload)
		}
		if event.Seq == 0 {
			t.Errorf("%s: sequence numbers start at 1, so a client can spot a gap", name)
		}
	}
}

// The tenant boundary, at the one layer that does not go through Postgres. An
// order from one restaurant appearing on another's kitchen screen would be the
// worst possible leak — it is live, and it is on a wall.
func TestPublishDoesNotCrossRestaurants(t *testing.T) {
	t.Parallel()

	hub := New()
	mine, theirs := uuid.New(), uuid.New()

	sub := hub.Subscribe(mine)
	defer hub.Unsubscribe(sub)

	if err := hub.Publish(theirs, EventOrderCreated, map[string]string{"secret": "yes"}); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	select {
	case event := <-sub.Events():
		t.Fatalf("received another restaurant's event: %+v", event)
	case <-time.After(50 * time.Millisecond):
	}
}

// One wedged tablet must not stall the kitchen. This is the property the whole
// buffered-channel design exists for, so it is asserted directly: fill a
// subscriber's buffer, then check that publishing still returns promptly and
// that a healthy subscriber alongside it still receives.
func TestPublishDropsRatherThanBlocks(t *testing.T) {
	t.Parallel()

	hub := New()
	room := uuid.New()

	wedged := hub.Subscribe(room)
	healthy := hub.Subscribe(room)
	defer hub.Unsubscribe(wedged)
	defer hub.Unsubscribe(healthy)

	// Fill both buffers, then drain only the healthy one, so the next publishes
	// have somewhere to go for one and not the other.
	for i := range Buffer {
		if err := hub.Publish(room, EventOrderCreated, map[string]int{"n": i}); err != nil {
			t.Fatalf("Publish: %v", err)
		}
	}
	for range Buffer {
		receive(t, healthy)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 10 {
			_ = hub.Publish(room, EventOrderUpdated, map[string]int{"n": i})
		}
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Publish blocked on a subscriber that was not reading; one frozen device must never stall the rest")
	}

	// The healthy subscriber got everything published after the drain.
	for range 10 {
		if event := receive(t, healthy); event.Type != EventOrderUpdated {
			t.Errorf("healthy subscriber got %q", event.Type)
		}
	}

	if !wedged.TakeLagged() {
		t.Error("the wedged subscriber should be flagged as lagged so the stream can tell it to resync")
	}
	// The flag is consumed, so one lag episode produces one resync rather than
	// one per event.
	if wedged.TakeLagged() {
		t.Error("TakeLagged should clear the flag")
	}
	if hub.Dropped() == 0 {
		t.Error("dropped events should be counted; a silent drop is the bug this design trades for")
	}
}

func TestUnsubscribeClosesAndIsIdempotent(t *testing.T) {
	t.Parallel()

	hub := New()
	room := uuid.New()
	sub := hub.Subscribe(room)

	hub.Unsubscribe(sub)
	if _, open := <-sub.Events(); open {
		t.Error("the channel should be closed after Unsubscribe")
	}
	// A handler that both defers Unsubscribe and calls it on an error path must
	// not panic on a double close.
	hub.Unsubscribe(sub)

	if got := hub.Subscribers(room); got != 0 {
		t.Errorf("Subscribers = %d, want 0", got)
	}
	if got := hub.Total(); got != 0 {
		t.Errorf("Total = %d, want 0; an empty room should not be kept alive", got)
	}
}

// The race the locking is shaped to prevent: a publish in flight while a
// connection drops would otherwise send on a closed channel and panic the
// process. Run with -race, which is how CI runs it.
func TestConcurrentPublishAndUnsubscribe(t *testing.T) {
	t.Parallel()

	hub := New()
	room := uuid.New()

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				sub := hub.Subscribe(room)
				hub.Unsubscribe(sub)
			}
		}()
	}
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 200 {
				_ = hub.Publish(room, EventOrderUpdated, map[string]int{"n": i})
			}
		}()
	}
	wg.Wait()

	if got := hub.Total(); got != 0 {
		t.Errorf("Total = %d, want 0", got)
	}
}

// Sequence numbers must be strictly increasing across the whole hub, since that
// is what lets a client detect a gap.
func TestSequenceIsMonotonic(t *testing.T) {
	t.Parallel()

	hub := New()
	room := uuid.New()
	sub := hub.Subscribe(room)
	defer hub.Unsubscribe(sub)

	var last uint64
	for range 5 {
		if err := hub.Publish(room, EventOrderUpdated, struct{}{}); err != nil {
			t.Fatalf("Publish: %v", err)
		}
		event := receive(t, sub)
		if event.Seq <= last {
			t.Fatalf("sequence went %d -> %d", last, event.Seq)
		}
		last = event.Seq
	}
}

// A payload that will not encode is a bug in the caller, and must be reported
// rather than delivered as a broken frame.
func TestPublishRejectsUnencodablePayload(t *testing.T) {
	t.Parallel()

	hub := New()
	room := uuid.New()
	sub := hub.Subscribe(room)
	defer hub.Unsubscribe(sub)

	if err := hub.Publish(room, EventOrderCreated, make(chan int)); err == nil {
		t.Fatal("expected an encoding error")
	}
	select {
	case event := <-sub.Events():
		t.Fatalf("a failed encode must deliver nothing, got %+v", event)
	default:
	}
}

func receive(t *testing.T, sub *Subscriber) Event {
	t.Helper()
	select {
	case event, open := <-sub.Events():
		if !open {
			t.Fatal("subscriber channel closed unexpectedly")
		}
		return event
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for an event")
		return Event{}
	}
}
