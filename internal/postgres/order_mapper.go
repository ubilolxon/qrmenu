package postgres

import (
	"encoding/json"

	"github.com/google/uuid"

	"github.com/ubilolxon/qrmenu/internal/domain"
	"github.com/ubilolxon/qrmenu/internal/i18n"
	"github.com/ubilolxon/qrmenu/internal/postgres/gen"
)

// ToOrder maps an order and its lines.
//
// The snapshot columns are decoded here, and a column that will not parse
// degrades to an empty value rather than failing the read — the same call made
// for image variants, and for the same reason. These rows are historical: one
// corrupt name from a year ago must not take out the kitchen screen for every
// order placed tonight.
func ToOrder(o gen.Order, lines []gen.OrderItem) domain.Order {
	order := domain.Order{
		ID:              o.ID,
		RestaurantID:    o.RestaurantID,
		TableID:         o.TableID,
		TableLabel:      o.TableLabel,
		CodeSeq:         o.CodeSeq,
		BusinessDay:     o.BusinessDay,
		Status:          domain.OrderStatus(o.Status),
		GuestSessionID:  o.GuestSessionID,
		Locale:          o.Locale,
		Note:            o.Note,
		SubtotalMinor:   o.SubtotalMinor,
		ServiceFeeMinor: o.ServiceFeeMinor,
		TotalMinor:      o.TotalMinor,
		Currency:        o.Currency,
		PlacedAt:        o.PlacedAt,
		AcceptedAt:      o.AcceptedAt,
		ReadyAt:         o.ReadyAt,
		ClosedAt:        o.ClosedAt,
		CancelReason:    o.CancelReason,
		UpdatedAt:       o.UpdatedAt,
		Lines:           make([]domain.OrderLine, 0, len(lines)),
	}
	for _, l := range lines {
		order.Lines = append(order.Lines, ToOrderLine(l))
	}
	return order
}

func ToOrderLine(l gen.OrderItem) domain.OrderLine {
	line := domain.OrderLine{
		ID:             l.ID,
		ItemID:         l.ItemID,
		VariantID:      l.VariantID,
		Name:           textOrEmpty(l.NameSnapshot),
		VariantName:    textOrEmpty(l.VariantNameSnapshot),
		Qty:            l.Qty,
		UnitPriceMinor: l.UnitPriceMinor,
		LineTotalMinor: l.LineTotalMinor,
		Note:           l.Note,
		SortOrder:      l.SortOrder,
	}

	line.Modifiers = []domain.OrderLineModifier{}
	if len(l.Modifiers) > 0 {
		var mods []domain.OrderLineModifier
		if err := json.Unmarshal(l.Modifiers, &mods); err == nil && mods != nil {
			line.Modifiers = mods
		}
	}
	return line
}

// ToOrders stitches lines onto their orders in one pass, so listing a screenful
// costs two queries rather than one per order.
func ToOrders(rows []gen.Order, lines []gen.OrderItem) []domain.Order {
	byOrder := make(map[uuid.UUID][]gen.OrderItem, len(rows))
	for _, l := range lines {
		byOrder[l.OrderID] = append(byOrder[l.OrderID], l)
	}

	out := make([]domain.Order, len(rows))
	for i, o := range rows {
		out[i] = ToOrder(o, byOrder[o.ID])
	}
	return out
}

func ToWaiterCall(c gen.WaiterCall) domain.WaiterCall {
	return domain.WaiterCall{
		ID:             c.ID,
		RestaurantID:   c.RestaurantID,
		TableID:        c.TableID,
		TableLabel:     c.TableLabel,
		Kind:           domain.WaiterCallKind(c.Kind),
		Status:         domain.WaiterCallStatus(c.Status),
		Note:           c.Note,
		CreatedAt:      c.CreatedAt,
		AcknowledgedAt: c.AcknowledgedAt,
		ResolvedAt:     c.ResolvedAt,
	}
}

func ToWaiterCalls(rows []gen.WaiterCall) []domain.WaiterCall {
	out := make([]domain.WaiterCall, len(rows))
	for i, r := range rows {
		out[i] = ToWaiterCall(r)
	}
	return out
}

func textOrEmpty(raw []byte) i18n.Text {
	t, err := i18n.FromRaw(raw)
	if err != nil {
		return i18n.Text{}
	}
	return t
}
