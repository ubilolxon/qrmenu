package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/ubilolxon/qrmenu/internal/domain"
	"github.com/ubilolxon/qrmenu/internal/http/middleware"
	"github.com/ubilolxon/qrmenu/internal/httperr"
	"github.com/ubilolxon/qrmenu/internal/realtime"
	"github.com/ubilolxon/qrmenu/internal/service"
)

// Staff is the waiter and kitchen surface: the live stream, the open board, and
// the two actions that move work along.
type Staff struct {
	svc *service.Order
}

func NewStaff(svc *service.Order) Staff { return Staff{svc: svc} }

// Heartbeat is how often a comment line is sent down an idle stream.
//
// Nothing in the protocol needs it. Proxies do: an idle connection through
// nginx, a load balancer or a hotel router gets closed somewhere between 30 and
// 60 seconds, and the client then reconnects in a loop that looks like a
// server bug. Twenty seconds is comfortably inside every default worth caring
// about.
const Heartbeat = 20 * time.Second

// Stream is the SSE connection a kitchen or waiter tablet holds open.
//
// Authentication is a bearer token in the Authorization header, which means a
// browser's built-in EventSource **cannot** be used — it cannot set headers.
// The client must read the stream with fetch() and a ReadableStream. The
// alternative, accepting the token as a query parameter, would write a live
// credential into every access log, proxy log and Referer header between here
// and the tablet, and those logs outlive the token.
func (h Staff) Stream(w http.ResponseWriter, r *http.Request) {
	identity := middleware.MustIdentity(r.Context())

	restaurantID, ok := h.queryRestaurant(w, r)
	if !ok {
		return
	}
	// Checked before a single byte is written: a long-lived stream is the worst
	// possible place to discover an authorisation mistake, because the client
	// has already been told the connection succeeded.
	if err := h.svc.AssertRestaurant(r.Context(), identity.TenantID, restaurantID); err != nil {
		RenderError(w, r, err)
		return
	}

	// Everything below goes through a ResponseController rather than a
	// `w.(http.Flusher)` assertion. The assertion looks equivalent and is not:
	// the logging middleware wraps the writer in a struct that *embeds the
	// http.ResponseWriter interface*, so Flush is not promoted onto the wrapper
	// and the assertion fails for every request that passes through it. The
	// controller follows Unwrap() down to the real connection, which is exactly
	// what it exists for.
	rc := http.NewResponseController(w)

	// The server runs with a write timeout, which for a normal request is a
	// defence against a slow client and for this one would be a guillotine: the
	// stream would die at exactly WriteTimeout, every time, and reconnect
	// forever. Clearing the deadline applies to this connection only.
	//
	// This runs before any header is written, so a failure can still be
	// reported as a 500 rather than as a stream that dies a minute later.
	if err := rc.SetWriteDeadline(time.Time{}); err != nil {
		httperr.Internal(w, r, fmt.Errorf("sse: clear write deadline: %w", err))
		return
	}

	header := w.Header()
	header.Set("Content-Type", "text/event-stream")
	header.Set("Cache-Control", "no-cache, no-transform")
	header.Set("Connection", "keep-alive")
	// nginx buffers proxied responses by default, which for a stream means the
	// events arrive in a clump when the buffer fills — or never.
	header.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	sub := h.svc.Hub().Subscribe(restaurantID)
	defer h.svc.Hub().Unsubscribe(sub)

	// This hub keeps no history, so Last-Event-ID cannot be honoured and is
	// deliberately ignored. Instead every connection opens with `ready`, whose
	// contract is "load the board now" — which is also the recovery for a
	// dropped event, so the client has one code path rather than two.
	writeEvent(w, realtime.Event{
		Type: realtime.EventReady,
		Data: mustJSON(map[string]any{"restaurant_id": restaurantID.String()}),
	})
	// From here the status line is already sent, so a write failure can only be
	// handled by hanging up — which is the correct response to a client that
	// has gone away.
	if err := rc.Flush(); err != nil {
		return
	}

	ticker := time.NewTicker(Heartbeat)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return

		case event, open := <-sub.Events():
			if !open {
				return
			}
			// A dropped event is announced rather than hidden. The client
			// reloads; the alternative is a board quietly missing an order,
			// which is the failure this whole surface exists to prevent.
			if sub.TakeLagged() {
				writeEvent(w, realtime.Event{
					Type: realtime.EventResync,
					Data: mustJSON(map[string]any{"reason": "events were dropped"}),
				})
			}
			writeEvent(w, event)
			if err := rc.Flush(); err != nil {
				return
			}

		case <-ticker.C:
			// A comment line: valid SSE, ignored by every client, and enough
			// traffic to keep an idle connection alive.
			fmt.Fprint(w, ": ping\n\n")
			// The heartbeat is also how a dead peer is detected: writing to a
			// closed connection is what finally errors, and this loop would
			// otherwise sit on an idle socket forever.
			if err := rc.Flush(); err != nil {
				return
			}
		}
	}
}

// Orders is the open board, and what a client loads on `ready` or `resync`.
func (h Staff) Orders(w http.ResponseWriter, r *http.Request) {
	identity := middleware.MustIdentity(r.Context())
	restaurantID, ok := h.queryRestaurant(w, r)
	if !ok {
		return
	}

	orders, err := h.svc.OpenOrders(r.Context(), identity.TenantID, restaurantID)
	if err != nil {
		RenderError(w, r, err)
		return
	}
	httperr.JSON(w, http.StatusOK, map[string]any{"orders": domain.OrderViews(orders)})
}

type orderStatusRequest struct {
	Status string `json:"status"`
	Reason string `json:"reason"`
}

// SetOrderStatus moves an order along.
func (h Staff) SetOrderStatus(w http.ResponseWriter, r *http.Request) {
	identity := middleware.MustIdentity(r.Context())
	id, ok := pathID(w, r, "orderID", "order_id")
	if !ok {
		return
	}

	var req orderStatusRequest
	if err := DecodeJSON(w, r, &req); err != nil {
		RenderError(w, r, err)
		return
	}

	order, err := h.svc.SetStatus(r.Context(), identity.TenantID, id,
		identity.Role, domain.OrderStatus(req.Status), req.Reason)
	if err != nil {
		RenderError(w, r, err)
		return
	}
	httperr.JSON(w, http.StatusOK, order.View())
}

// Calls lists the tables currently asking for someone.
func (h Staff) Calls(w http.ResponseWriter, r *http.Request) {
	identity := middleware.MustIdentity(r.Context())
	restaurantID, ok := h.queryRestaurant(w, r)
	if !ok {
		return
	}

	calls, err := h.svc.OpenCalls(r.Context(), identity.TenantID, restaurantID)
	if err != nil {
		RenderError(w, r, err)
		return
	}
	httperr.JSON(w, http.StatusOK, map[string]any{"calls": domain.WaiterCallViews(calls)})
}

type callStatusRequest struct {
	Status string `json:"status"`
}

func (h Staff) SetCallStatus(w http.ResponseWriter, r *http.Request) {
	identity := middleware.MustIdentity(r.Context())
	id, ok := pathID(w, r, "callID", "call_id")
	if !ok {
		return
	}

	var req callStatusRequest
	if err := DecodeJSON(w, r, &req); err != nil {
		RenderError(w, r, err)
		return
	}

	call, err := h.svc.SetCallStatus(r.Context(), identity.TenantID, id,
		domain.WaiterCallStatus(req.Status))
	if err != nil {
		RenderError(w, r, err)
		return
	}
	httperr.JSON(w, http.StatusOK, call.View())
}

// queryRestaurant reads ?restaurant_id=.
//
// A query parameter rather than a path segment because a tenant may run several
// restaurants and a device is pointed at exactly one — but the ownership check
// is what actually protects it, not the shape of the URL.
func (h Staff) queryRestaurant(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	raw := r.URL.Query().Get("restaurant_id")
	if raw == "" {
		RenderError(w, r, domain.Errorf(errBadRequest, "restaurant_id is required."))
		return uuid.Nil, false
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		RenderError(w, r, domain.Errorf(errBadRequest, "restaurant_id must be a valid UUID."))
		return uuid.Nil, false
	}
	return id, true
}

// writeEvent renders one SSE frame.
//
// The data is already-encoded JSON and therefore contains no raw newline, which
// is what keeps this a single `data:` line — a multi-line payload would need
// each line prefixed, and a payload that forgot to would silently truncate at
// the first newline.
func writeEvent(w http.ResponseWriter, e realtime.Event) {
	if e.Seq > 0 {
		fmt.Fprintf(w, "id: %d\n", e.Seq)
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e.Type, e.Data)
}

func mustJSON(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		// Only reachable with a value this file constructs itself, so a failure
		// is a programming error, not a runtime condition.
		return json.RawMessage(`{}`)
	}
	return raw
}
