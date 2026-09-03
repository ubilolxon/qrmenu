package domain

import (
	"fmt"
	"time"

	"github.com/google/uuid"
)

// An invoice is the unit a payment is made against, and the only thing a
// payment provider is ever told about. It snapshots the plan code, amount and
// currency for the same reason an order line snapshots a dish name: what the
// customer was charged has to survive the next price change.

type InvoiceStatus string

const (
	InvoiceOpen     InvoiceStatus = "open"
	InvoicePaid     InvoiceStatus = "paid"
	InvoiceVoid     InvoiceStatus = "void"
	InvoiceRefunded InvoiceStatus = "refunded"
)

var invoiceTransitions = map[InvoiceStatus][]InvoiceStatus{
	InvoiceOpen:     {InvoicePaid, InvoiceVoid},
	InvoicePaid:     {InvoiceRefunded},
	InvoiceVoid:     nil,
	InvoiceRefunded: nil,
}

func (s InvoiceStatus) Valid() bool {
	_, ok := invoiceTransitions[s]
	return ok
}

func (s InvoiceStatus) CanTransition(to InvoiceStatus) bool {
	for _, allowed := range invoiceTransitions[s] {
		if allowed == to {
			return true
		}
	}
	return false
}

// Payable reports whether a provider may still take money for this invoice.
// Everything a payment callback does starts with this question.
func (s InvoiceStatus) Payable() bool { return s == InvoiceOpen }

type Invoice struct {
	ID             uuid.UUID       `json:"id"`
	TenantID       uuid.UUID       `json:"-"`
	SubscriptionID uuid.UUID       `json:"-"`
	Number         string          `json:"number"`
	PlanCode       string          `json:"plan_code"`
	AmountMinor    int64           `json:"amount_minor"`
	Currency       string          `json:"currency"`
	Status         InvoiceStatus   `json:"status"`
	PeriodStart    time.Time       `json:"period_start"`
	PeriodEnd      time.Time       `json:"period_end"`
	Provider       PaymentProvider `json:"provider"`
	PaidAt         *time.Time      `json:"paid_at"`
	Memo           *string         `json:"memo,omitempty"`
	CreatedAt      time.Time       `json:"created_at"`
}

// FormatInvoiceNumber builds the human-readable reference. Only the year and
// the sequence are stored, so the printed form can never drift from them.
func FormatInvoiceNumber(year, seq int) string {
	return fmt.Sprintf("INV-%d-%04d", year, seq)
}

// ------------------------------------------------------- payment transactions

// TxnState is the lifecycle of one provider's attempt to move money. It is
// deliberately three states and not more: everything a provider reports maps
// onto created, performed or cancelled, and inventing a richer vocabulary here
// would mean translating each provider's into ours twice.
type TxnState string

const (
	TxnCreated   TxnState = "created"
	TxnPerformed TxnState = "performed"
	TxnCancelled TxnState = "cancelled"
)

type PaymentTransaction struct {
	ID            uuid.UUID       `json:"id"`
	InvoiceID     uuid.UUID       `json:"invoice_id"`
	TenantID      uuid.UUID       `json:"-"`
	Provider      PaymentProvider `json:"provider"`
	ProviderTxnID string          `json:"provider_txn_id"`
	State         TxnState        `json:"state"`
	AmountMinor   int64           `json:"amount_minor"`
	CreatedMs     int64           `json:"created_ms"`
	PerformedMs   *int64          `json:"performed_ms"`
	CancelledMs   *int64          `json:"cancelled_ms"`
	CancelReason  *int32          `json:"cancel_reason"`
	CreatedAt     time.Time       `json:"created_at"`
}

// MaxTxnAge is how long a provider may leave a transaction in 'created' before
// it is treated as abandoned. Payme's own contract uses twelve hours, and a
// checkout nobody completed must not block the invoice forever.
const MaxTxnAge = 12 * time.Hour

// Timeout reports whether an unfinished transaction has been open too long.
func (t PaymentTransaction) Timeout(now time.Time) bool {
	return t.State == TxnCreated && now.Sub(time.UnixMilli(t.CreatedMs)) > MaxTxnAge
}
