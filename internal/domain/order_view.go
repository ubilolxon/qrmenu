package domain

import (
	"time"

	"github.com/google/uuid"

	"github.com/ubilolxon/qrmenu/internal/i18n"
)

// The wire shape of an order, defined here for the same reason PublicMenu is:
// an order reaches a client two different ways — as the response to a request,
// and as an SSE event pushed to a tablet — and those two must be the same
// shape. A client that has to handle "an order from the list endpoint" and "an
// order from the stream" as different types will eventually handle one of them
// wrong. Defining it in domain is what lets both the service (which publishes)
// and the HTTP layer (which responds) produce it without an import cycle.
//
// Note what is absent: guest_session_id. It is the guest's only credential, and
// it appears in no response, on either surface.

type OrderView struct {
	ID     string      `json:"id"`
	Code   string      `json:"code"`
	Status OrderStatus `json:"status"`
	// The legal moves from here, so a staff tablet renders exactly the buttons
	// that will work rather than discovering a 409 on tap.
	NextStatuses []OrderStatus `json:"next_statuses"`

	TableID    *string `json:"table_id"`
	TableLabel string  `json:"table_label"`

	Locale string  `json:"locale"`
	Note   *string `json:"note"`

	Currency        string `json:"currency"`
	SubtotalMinor   int64  `json:"subtotal_minor"`
	ServiceFeeMinor int64  `json:"service_fee_minor"`
	TotalMinor      int64  `json:"total_minor"`

	Lines []OrderLineView `json:"lines"`

	BusinessDay  string     `json:"business_day"`
	PlacedAt     time.Time  `json:"placed_at"`
	AcceptedAt   *time.Time `json:"accepted_at"`
	ReadyAt      *time.Time `json:"ready_at"`
	ClosedAt     *time.Time `json:"closed_at"`
	CancelReason *string    `json:"cancel_reason"`
}

// Names are carried as the full translation object rather than one resolved
// string. The guest ordered in Russian, the kitchen screen is in Uzbek, and the
// owner's report is in English — all three read the same snapshot.
type OrderLineView struct {
	ID        string  `json:"id"`
	ItemID    *string `json:"item_id"`
	VariantID *string `json:"variant_id"`

	Name        i18n.Text           `json:"name"`
	VariantName i18n.Text           `json:"variant_name"`
	Modifiers   []OrderLineModifier `json:"modifiers"`

	Qty            int32   `json:"qty"`
	UnitPriceMinor int64   `json:"unit_price_minor"`
	LineTotalMinor int64   `json:"line_total_minor"`
	Note           *string `json:"note"`
}

func (o Order) View() OrderView {
	view := OrderView{
		ID:              o.ID.String(),
		Code:            o.Code(),
		Status:          o.Status,
		NextStatuses:    o.Status.NextStatuses(),
		TableID:         uuidStringOrNil(o.TableID),
		TableLabel:      o.TableLabel,
		Locale:          o.Locale,
		Note:            o.Note,
		Currency:        o.Currency,
		SubtotalMinor:   o.SubtotalMinor,
		ServiceFeeMinor: o.ServiceFeeMinor,
		TotalMinor:      o.TotalMinor,
		BusinessDay:     o.BusinessDay.Format(time.DateOnly),
		PlacedAt:        o.PlacedAt,
		AcceptedAt:      o.AcceptedAt,
		ReadyAt:         o.ReadyAt,
		ClosedAt:        o.ClosedAt,
		CancelReason:    o.CancelReason,
		Lines:           make([]OrderLineView, 0, len(o.Lines)),
	}
	if view.NextStatuses == nil {
		view.NextStatuses = []OrderStatus{}
	}

	for _, l := range o.Lines {
		line := OrderLineView{
			ID:             l.ID.String(),
			ItemID:         uuidStringOrNil(l.ItemID),
			VariantID:      uuidStringOrNil(l.VariantID),
			Name:           l.Name,
			VariantName:    l.VariantName,
			Modifiers:      l.Modifiers,
			Qty:            l.Qty,
			UnitPriceMinor: l.UnitPriceMinor,
			LineTotalMinor: l.LineTotalMinor,
			Note:           l.Note,
		}
		// JSON must never carry null where the client expects a list or an
		// object it will index into.
		if line.VariantName == nil {
			line.VariantName = i18n.Text{}
		}
		if line.Modifiers == nil {
			line.Modifiers = []OrderLineModifier{}
		}
		view.Lines = append(view.Lines, line)
	}
	return view
}

type WaiterCallView struct {
	ID             string           `json:"id"`
	TableID        *string          `json:"table_id"`
	TableLabel     string           `json:"table_label"`
	Kind           WaiterCallKind   `json:"kind"`
	Status         WaiterCallStatus `json:"status"`
	Note           *string          `json:"note"`
	CreatedAt      time.Time        `json:"created_at"`
	AcknowledgedAt *time.Time       `json:"acknowledged_at"`
	ResolvedAt     *time.Time       `json:"resolved_at"`
}

func (c WaiterCall) View() WaiterCallView {
	return WaiterCallView{
		ID:             c.ID.String(),
		TableID:        uuidStringOrNil(c.TableID),
		TableLabel:     c.TableLabel,
		Kind:           c.Kind,
		Status:         c.Status,
		Note:           c.Note,
		CreatedAt:      c.CreatedAt,
		AcknowledgedAt: c.AcknowledgedAt,
		ResolvedAt:     c.ResolvedAt,
	}
}

// OrderViews and WaiterCallViews keep the "never null where a list is expected"
// rule in one place rather than at each call site.
func OrderViews(orders []Order) []OrderView {
	out := make([]OrderView, len(orders))
	for i, o := range orders {
		out[i] = o.View()
	}
	return out
}

func WaiterCallViews(calls []WaiterCall) []WaiterCallView {
	out := make([]WaiterCallView, len(calls))
	for i, c := range calls {
		out[i] = c.View()
	}
	return out
}

// uuidStringOrNil renders an optional id, keeping null as null rather than
// turning it into the zero UUID — a deleted table must read as absent, not as
// table 00000000-0000-0000-0000-000000000000.
func uuidStringOrNil(id *uuid.UUID) *string {
	if id == nil {
		return nil
	}
	s := id.String()
	return &s
}
