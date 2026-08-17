package handler

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/ubilolxon/qrmenu/internal/config"
	"github.com/ubilolxon/qrmenu/internal/domain"
	"github.com/ubilolxon/qrmenu/internal/httperr"
	"github.com/ubilolxon/qrmenu/internal/ratelimit"
	"github.com/ubilolxon/qrmenu/internal/service"
)

// Order is the guest's side of ordering: no login, a QR token for identity of
// place, and a cookie for identity of person.
type Order struct {
	svc     *service.Order
	cfg     config.Public
	secure  bool
	limiter *ratelimit.Limiter
}

// secure comes from the auth cookie's setting because it answers the same
// question — whether this deployment is served over HTTPS — and having two
// switches that must agree is how they end up disagreeing.
func NewOrder(svc *service.Order, cfg config.Public, secure bool, limiter *ratelimit.Limiter) Order {
	return Order{svc: svc, cfg: cfg, secure: secure, limiter: limiter}
}

// ------------------------------------------------------------------ requests

type placeOrderRequest struct {
	Lines  []cartLineRequest `json:"lines"`
	Note   string            `json:"note"`
	Locale string            `json:"locale"`
}

// Note what a line cannot carry: a price. The server looks up what everything
// costs; anything a client sends about money is ignored, and rejected outright
// because DecodeJSON refuses unknown fields.
type cartLineRequest struct {
	ItemID      string   `json:"item_id"`
	VariantID   *string  `json:"variant_id"`
	ModifierIDs []string `json:"modifier_ids"`
	Qty         int32    `json:"qty"`
	Note        string   `json:"note"`
}

type callWaiterRequest struct {
	Kind string `json:"kind"`
	Note string `json:"note"`
}

// toCart parses the ids, reporting every malformed one at the path the client
// sent it under so a bad row can be highlighted rather than the whole cart
// rejected with "invalid UUID".
func (req placeOrderRequest) toCart() (domain.Cart, error) {
	var v domain.Validator
	cart := domain.Cart{Note: req.Note, Lines: make([]domain.CartLine, 0, len(req.Lines))}

	for i, in := range req.Lines {
		field := func(name string) string { return "lines[" + strconv.Itoa(i) + "]." + name }

		itemID, err := uuid.Parse(strings.TrimSpace(in.ItemID))
		if err != nil {
			v.Add(field("item_id"), "must be a valid UUID")
			continue
		}
		line := domain.CartLine{ItemID: itemID, Qty: in.Qty, Note: in.Note}

		if in.VariantID != nil && strings.TrimSpace(*in.VariantID) != "" {
			variantID, err := uuid.Parse(strings.TrimSpace(*in.VariantID))
			if err != nil {
				v.Add(field("variant_id"), "must be a valid UUID")
				continue
			}
			line.VariantID = &variantID
		}

		line.ModifierIDs = make([]uuid.UUID, 0, len(in.ModifierIDs))
		bad := false
		for _, raw := range in.ModifierIDs {
			modifierID, err := uuid.Parse(strings.TrimSpace(raw))
			if err != nil {
				v.Add(field("modifier_ids"), "must contain valid UUIDs")
				bad = true
				break
			}
			line.ModifierIDs = append(line.ModifierIDs, modifierID)
		}
		if bad {
			continue
		}

		cart.Lines = append(cart.Lines, line)
	}

	if err := v.Err(); err != nil {
		return domain.Cart{}, err
	}
	return cart, nil
}

// ------------------------------------------------------------------ handlers

// Place records a guest's order for the table their QR code belongs to.
func (h Order) Place(w http.ResponseWriter, r *http.Request) {
	token := chi.URLParam(r, "token")

	var req placeOrderRequest
	if err := DecodeJSON(w, r, &req); err != nil {
		RenderError(w, r, err)
		return
	}
	cart, err := req.toCart()
	if err != nil {
		RenderError(w, r, err)
		return
	}

	// Rate limited on the table, not the IP: everyone in the restaurant shares
	// one NAT address, so limiting by IP would let the first table's order rush
	// silence the rest of the room.
	if !h.allow(w, r, "order:"+domain.NormalizeQRToken(token), ratelimit.OrdersPerMinute) {
		return
	}

	// Minted here rather than on the menu page: a session that has never
	// ordered anything owns nothing and is not worth setting a cookie for.
	session, ok := h.ensureSession(w, r)
	if !ok {
		return
	}

	placed, err := h.svc.Place(r.Context(), token, session, service.PlaceInput{
		Cart:           cart,
		Locale:         req.Locale,
		IdempotencyKey: r.Header.Get("Idempotency-Key"),
	})
	if err != nil {
		RenderError(w, r, err)
		return
	}

	// 200 rather than 201 for a replay, so a client can tell "your order was
	// received" from "a second order was created".
	status := http.StatusCreated
	if placed.Replayed {
		status = http.StatusOK
	}
	httperr.JSON(w, status, placed.Order.View())
}

// MyOrders lists what this guest has ordered at this restaurant.
func (h Order) MyOrders(w http.ResponseWriter, r *http.Request) {
	orders, err := h.svc.GuestOrders(r.Context(), chi.URLParam(r, "token"), readSession(r, h.cfg.GuestCookieName))
	if err != nil {
		RenderError(w, r, err)
		return
	}
	httperr.JSON(w, http.StatusOK, map[string]any{"orders": domain.OrderViews(orders)})
}

// Get returns one order to the guest who placed it, which is how the phone
// shows "your food is being prepared" without a login.
func (h Order) Get(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "orderID", "order_id")
	if !ok {
		return
	}
	order, err := h.svc.GuestOrder(r.Context(), id, readSession(r, h.cfg.GuestCookieName))
	if err != nil {
		RenderError(w, r, err)
		return
	}
	httperr.JSON(w, http.StatusOK, order.View())
}

// CallWaiter raises a call from the table.
func (h Order) CallWaiter(w http.ResponseWriter, r *http.Request) {
	token := chi.URLParam(r, "token")

	var req callWaiterRequest
	if err := DecodeJSON(w, r, &req); err != nil {
		RenderError(w, r, err)
		return
	}

	if !h.allow(w, r, "call:"+domain.NormalizeQRToken(token), ratelimit.CallsPerMinute) {
		return
	}

	session, ok := h.ensureSession(w, r)
	if !ok {
		return
	}

	called, err := h.svc.CallWaiter(r.Context(), token, session,
		domain.WaiterCallKind(strings.TrimSpace(req.Kind)), req.Note)
	if err != nil {
		RenderError(w, r, err)
		return
	}

	// 200 when the press landed on a call that was already live, for the same
	// reason a replayed order is not a 201.
	status := http.StatusCreated
	if !called.Created {
		status = http.StatusOK
	}
	httperr.JSON(w, status, called.Call.View())
}

// ------------------------------------------------------------------ session

// ensureSession returns the guest's session id, minting one if this is their
// first write. The cookie must be set before the body is written, so this runs
// ahead of the service call.
func (h Order) ensureSession(w http.ResponseWriter, r *http.Request) (string, bool) {
	if existing := readSession(r, h.cfg.GuestCookieName); existing != "" {
		return existing, true
	}

	session, err := domain.NewGuestSession()
	if err != nil {
		httperr.Internal(w, r, err)
		return "", false
	}

	http.SetCookie(w, &http.Cookie{
		Name:  h.cfg.GuestCookieName,
		Value: session,
		Path:  "/",
		// Lax, not Strict: a guest arriving from a QR scanner app or a chat
		// message is a cross-site navigation, and Strict would withhold the
		// cookie on exactly that first hop — the one every guest takes.
		SameSite: http.SameSiteLaxMode,
		// HttpOnly even though the SPA might like to read it: it is a
		// credential, and nothing on the page needs its value.
		HttpOnly: true,
		Secure:   h.secure,
		MaxAge:   int(h.cfg.GuestSessionTTL.Seconds()),
	})
	return session, true
}

func readSession(r *http.Request, name string) string {
	cookie, err := r.Cookie(name)
	if err != nil || !domain.ValidGuestSession(cookie.Value) {
		return ""
	}
	return cookie.Value
}

// allow applies the guest write limit, answering 429 with Retry-After when the
// bucket is empty.
func (h Order) allow(w http.ResponseWriter, r *http.Request, key string, limit int) bool {
	if h.limiter == nil {
		return true
	}
	result := h.limiter.Allow(r.Context(), key, limit)
	if result.Allowed {
		return true
	}
	w.Header().Set("Retry-After", strconv.Itoa(int(result.RetryAfter.Seconds()+0.999)))
	httperr.Write(w, http.StatusTooManyRequests, "RATE_LIMITED",
		"Too many requests from this table. Please wait a moment.", nil)
	return false
}
