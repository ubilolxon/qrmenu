// Package realtime carries server-pushed events to staff devices.
//
// Server-Sent Events rather than WebSocket: the traffic is entirely one way —
// the kitchen screen never tells the server anything, it taps a normal PATCH —
// and SSE is plain HTTP. It reconnects on its own, survives every proxy that
// understands HTTP/1.1 chunked responses, and needs no library on either side.
// A WebSocket would buy bidirectionality nobody uses, in exchange for a
// handshake that corporate proxies and restaurant wifi captive portals break.
package realtime

import (
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/google/uuid"
)

// Buffer is how many events a single connection may fall behind by before it
// starts losing them.
//
// Thirty-two is roughly a very busy minute in one restaurant. A tablet that is
// further behind than that is not slow, it is wedged — asleep, or on a dead
// wifi link with an open socket — and waiting for it is the failure that
// matters here: one frozen device must never stall the publisher and take the
// whole kitchen's screen down with it.
const Buffer = 32

// Event is one thing that happened, already encoded.
//
// The payload is marshalled once at publish time rather than per subscriber:
// twelve tablets in a restaurant would otherwise mean twelve identical JSON
// encodes of the same order.
type Event struct {
	// Seq is per hub and strictly increasing. A client that sees a gap knows it
	// missed something and can reload rather than quietly showing a stale board.
	Seq  uint64
	Type string
	Data json.RawMessage
}

// Hub is the registry of live connections, keyed by restaurant.
//
// Single-instance only, on purpose: with one API process this is a map and a
// mutex, and nothing else is needed. Running several instances means an order
// created on A must reach a tablet connected to B, which is a Redis pub/sub
// fan-out in front of this type — Publish becomes "publish to Redis", and each
// instance subscribes and calls the local deliver. The shape below is what
// makes that a wrapper rather than a rewrite.
type Hub struct {
	mu    sync.RWMutex
	rooms map[uuid.UUID]map[*Subscriber]struct{}
	seq   atomic.Uint64

	// Dropped counts events discarded because a subscriber's buffer was full,
	// across the whole process. It is the number to watch: a nonzero rate means
	// devices are missing updates.
	dropped atomic.Uint64
}

func New() *Hub {
	return &Hub{rooms: make(map[uuid.UUID]map[*Subscriber]struct{})}
}

// Subscriber is one connected device.
type Subscriber struct {
	ch   chan Event
	room uuid.UUID

	// lagged is set when this subscriber lost an event. There is no backfill —
	// the hub keeps no history — so the honest recovery is to tell the client
	// to reload its list. Losing events silently is what turns a dropped update
	// into an order nobody cooks.
	lagged atomic.Bool
}

// Events is the stream to select on. It is closed on Unsubscribe.
func (s *Subscriber) Events() <-chan Event { return s.ch }

// TakeLagged reports whether events were lost since the last call, and clears
// the flag — so one lag episode produces one resync, not one per event.
func (s *Subscriber) TakeLagged() bool { return s.lagged.Swap(false) }

// Subscribe registers a device for one restaurant's events.
func (h *Hub) Subscribe(room uuid.UUID) *Subscriber {
	s := &Subscriber{ch: make(chan Event, Buffer), room: room}

	h.mu.Lock()
	defer h.mu.Unlock()

	if h.rooms[room] == nil {
		h.rooms[room] = make(map[*Subscriber]struct{})
	}
	h.rooms[room][s] = struct{}{}
	return s
}

// Unsubscribe removes a device and closes its channel.
//
// The close happens under the write lock, and Publish sends under the read
// lock, so a send can never land on a closed channel — that mutual exclusion is
// the whole reason the locking is shaped this way. Calling it twice is safe.
func (h *Hub) Unsubscribe(s *Subscriber) {
	if s == nil {
		return
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	subs, ok := h.rooms[s.room]
	if !ok {
		return
	}
	if _, ok := subs[s]; !ok {
		return
	}
	delete(subs, s)
	if len(subs) == 0 {
		// Otherwise a restaurant that was busy once keeps an empty map alive
		// for the life of the process.
		delete(h.rooms, s.room)
	}
	close(s.ch)
}

// Publish fans an event out to every device watching that restaurant.
//
// It never blocks. A subscriber whose buffer is full has the event dropped and
// is flagged as lagged, because the alternative — blocking until it catches up
// — makes the slowest device in the building the speed limit for every order.
// Returns an error only if the payload will not encode, which is a programming
// error in the caller, not a delivery failure.
func (h *Hub) Publish(room uuid.UUID, eventType string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode %s event: %w", eventType, err)
	}

	event := Event{Seq: h.seq.Add(1), Type: eventType, Data: data}

	h.mu.RLock()
	defer h.mu.RUnlock()

	for s := range h.rooms[room] {
		select {
		case s.ch <- event:
		default:
			s.lagged.Store(true)
			h.dropped.Add(1)
		}
	}
	return nil
}

// Subscribers reports how many devices are watching one restaurant.
func (h *Hub) Subscribers(room uuid.UUID) int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.rooms[room])
}

// Total reports every live connection, for the readiness endpoint and metrics.
func (h *Hub) Total() int {
	h.mu.RLock()
	defer h.mu.RUnlock()

	n := 0
	for _, subs := range h.rooms {
		n += len(subs)
	}
	return n
}

// Dropped reports how many events have been discarded for full buffers since
// the process started.
func (h *Hub) Dropped() uint64 { return h.dropped.Load() }

// Event types. Named here rather than spelled at each call site, because the
// client switches on these strings and a typo would produce an event nobody
// handles and no error anywhere.
const (
	// EventReady is sent once on connect. The client should load the current
	// board when it arrives: SSE reconnects on its own and this hub keeps no
	// history, so "what did I miss" is always answered by refetching.
	EventReady = "ready"
	// EventResync means this connection lost events. Same recovery.
	EventResync = "resync"

	EventOrderCreated = "order.created"
	EventOrderUpdated = "order.updated"
	EventWaiterCalled = "waiter.called"
	EventWaiterUpdate = "waiter.updated"
)
