package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/ubilolxon/qrmenu/internal/billing"
	"github.com/ubilolxon/qrmenu/internal/domain"
	"github.com/ubilolxon/qrmenu/internal/postgres"
	"github.com/ubilolxon/qrmenu/internal/postgres/gen"
)

// Billing owns subscriptions, invoices and the payment record. It is also the
// billing.Ledger every payment provider talks to, which is why the provider
// registry is injected after construction rather than passed to the
// constructor: a provider needs a ledger and the ledger needs the registry, and
// a setter is a smaller price than a third type that exists only to break the
// loop.
type Billing struct {
	store     *postgres.Store
	providers *billing.Registry
	logger    *slog.Logger
}

func NewBilling(store *postgres.Store, logger *slog.Logger) *Billing {
	return &Billing{store: store, logger: logger}
}

// UseProviders wires the configured payment methods. Called once at startup,
// before the server begins serving.
func (s *Billing) UseProviders(r *billing.Registry) { s.providers = r }

// Providers exposes the registry so the router can mount each provider's
// callback, and the pricing page can list what is actually configured.
func (s *Billing) Providers() *billing.Registry { return s.providers }

var _ billing.Ledger = (*Billing)(nil)

// ------------------------------------------------------------------ reading

// Usage is what a tenant is currently consuming, in the same units the limits
// are expressed in. It is read for the billing page only — never on a write
// path, where each check counts just the one resource it needs.
type Usage struct {
	Restaurants int `json:"restaurants"`
	MenuItems   int `json:"menu_items"`
	Tables      int `json:"tables"`
	StaffUsers  int `json:"staff_users"`
	StorageMB   int `json:"storage_mb"`
}

// Overview is everything the billing page shows in one round trip.
type Overview struct {
	Entitlement domain.Entitlement `json:"entitlement"`
	Usage       Usage              `json:"usage"`
	Invoices    []domain.Invoice   `json:"invoices"`
}

// InvoiceHistory is how many past invoices the billing page shows. More than a
// year of monthly bills is an export, not a page.
const InvoiceHistory = 24

func (s *Billing) Overview(ctx context.Context, tenantID uuid.UUID) (Overview, error) {
	row, err := s.store.GetEntitlement(ctx, tenantID)
	if err != nil {
		return Overview{}, fmt.Errorf("get entitlement: %w", postgres.MapError(err))
	}
	ent, err := postgres.ToEntitlement(row)
	if err != nil {
		return Overview{}, err
	}

	usage, err := s.usage(ctx, tenantID)
	if err != nil {
		return Overview{}, err
	}

	invoices, err := s.store.ListInvoices(ctx, gen.ListInvoicesParams{TenantID: tenantID, Limit: InvoiceHistory})
	if err != nil {
		return Overview{}, fmt.Errorf("list invoices: %w", postgres.MapError(err))
	}

	return Overview{Entitlement: ent, Usage: usage, Invoices: postgres.ToInvoices(invoices)}, nil
}

func (s *Billing) usage(ctx context.Context, tenantID uuid.UUID) (Usage, error) {
	restaurants, err := s.store.CountRestaurants(ctx, tenantID)
	if err != nil {
		return Usage{}, fmt.Errorf("count restaurants: %w", postgres.MapError(err))
	}
	items, err := s.store.CountItemsByTenant(ctx, tenantID)
	if err != nil {
		return Usage{}, fmt.Errorf("count items: %w", postgres.MapError(err))
	}
	tables, err := s.store.CountTablesByTenant(ctx, tenantID)
	if err != nil {
		return Usage{}, fmt.Errorf("count tables: %w", postgres.MapError(err))
	}
	staff, err := s.store.CountActiveUsersByTenant(ctx, tenantID)
	if err != nil {
		return Usage{}, fmt.Errorf("count staff: %w", postgres.MapError(err))
	}
	bytes, err := s.store.SumImageBytesByTenant(ctx, tenantID)
	if err != nil {
		return Usage{}, fmt.Errorf("sum image bytes: %w", postgres.MapError(err))
	}
	return Usage{
		Restaurants: int(restaurants),
		MenuItems:   int(items),
		Tables:      int(tables),
		StaffUsers:  int(staff),
		StorageMB:   int((bytes + bytesPerMB - 1) / bytesPerMB),
	}, nil
}

func (s *Billing) Invoices(ctx context.Context, tenantID uuid.UUID) ([]domain.Invoice, error) {
	rows, err := s.store.ListInvoices(ctx, gen.ListInvoicesParams{TenantID: tenantID, Limit: InvoiceHistory})
	if err != nil {
		return nil, fmt.Errorf("list invoices: %w", postgres.MapError(err))
	}
	return postgres.ToInvoices(rows), nil
}

// ---------------------------------------------------------------- subscribing

// Checkout is the answer to "I want this plan": an invoice to pay and wherever
// the provider wants the customer sent.
type Checkout struct {
	Invoice  domain.Invoice         `json:"invoice"`
	Redirect billing.CheckoutResult `json:"checkout"`
}

// invoiceAttempts bounds the retry when two invoices race for the same yearly
// sequence number. Allocated as max+1 and made safe by a unique index, exactly
// like the daily order number.
const invoiceAttempts = 5

// Subscribe raises an invoice for a plan and starts a checkout.
//
// It does not change the subscription. Nothing here is entitled to: the plan
// changes when the money arrives, and the only thing allowed to say that has
// happened is a provider callback verified against that provider's own
// credentials, or an owner marking the invoice paid by hand.
func (s *Billing) Subscribe(ctx context.Context, tenantID uuid.UUID, planCode string, provider domain.PaymentProvider, returnTo string) (Checkout, error) {
	sub, err := s.store.GetSubscription(ctx, tenantID)
	if err != nil {
		return Checkout{}, fmt.Errorf("get subscription: %w", postgres.MapError(err))
	}

	planRow, err := s.store.GetPlan(ctx, planCode)
	if err != nil {
		return Checkout{}, fmt.Errorf("get plan: %w", postgres.MapError(err))
	}
	plan, err := postgres.ToPlan(planRow)
	if err != nil {
		return Checkout{}, err
	}

	// Moving to a free plan takes effect immediately: there is nothing to pay,
	// so waiting for a payment that will never arrive would leave the customer
	// on a plan they asked to leave.
	if plan.PriceMinor == 0 {
		if _, err := s.store.SetFreePlan(ctx, sub.ID); err != nil {
			return Checkout{}, fmt.Errorf("set free plan: %w", postgres.MapError(err))
		}
		s.logger.Info("subscription moved to the free plan",
			"tenant_id", tenantID, "from_plan", sub.PlanCode)
		// No invoice, and nowhere to send the customer. The empty invoice id is
		// how the handler tells this apart from a checkout that needs paying.
		return Checkout{Redirect: billing.CheckoutResult{
			Instructions: "You are now on the free plan.",
		}}, nil
	}

	if !provider.Valid() {
		return Checkout{}, domain.Errorf(domain.ErrNotFound, "%q is not a payment method.", provider)
	}
	impl, err := s.providerFor(provider)
	if err != nil {
		return Checkout{}, err
	}

	invoice, err := s.raiseInvoice(ctx, sub, plan, provider)
	if err != nil {
		return Checkout{}, err
	}

	result, err := impl.Checkout(ctx, billing.CheckoutRequest{Invoice: invoice, ReturnTo: returnTo})
	if err != nil {
		// The invoice is left open rather than deleted. It is a real thing the
		// customer asked for, and it can be paid through another method or
		// voided by an owner; removing it would also remove the record that
		// the checkout was ever attempted.
		return Checkout{}, fmt.Errorf("start checkout: %w", err)
	}
	return Checkout{Invoice: invoice, Redirect: result}, nil
}

func (s *Billing) providerFor(name domain.PaymentProvider) (billing.Provider, error) {
	if s.providers == nil {
		return nil, domain.Errorf(domain.ErrNotFound, "No payment methods are configured.")
	}
	return s.providers.Get(name)
}

// raiseInvoice writes the bill. The period on it is what the customer is buying
// now, computed the same way the activation will compute it: an unchanged plan
// extends the current period, a different one starts today.
func (s *Billing) raiseInvoice(ctx context.Context, sub gen.Subscription, plan domain.Plan, provider domain.PaymentProvider) (domain.Invoice, error) {
	start := time.Now()
	if sub.PlanCode == plan.Code && sub.CurrentPeriodEnd != nil && sub.CurrentPeriodEnd.After(start) {
		start = *sub.CurrentPeriodEnd
	}
	end := start.AddDate(0, plan.BillingInterval.Months(), 0)

	for attempt := range invoiceAttempts {
		alloc, err := s.store.NextInvoiceNumber(ctx)
		if err != nil {
			return domain.Invoice{}, fmt.Errorf("allocate invoice number: %w", postgres.MapError(err))
		}

		row, err := s.store.CreateInvoice(ctx, gen.CreateInvoiceParams{
			TenantID:       sub.TenantID,
			SubscriptionID: sub.ID,
			Number:         domain.FormatInvoiceNumber(int(alloc.Year), int(alloc.NextSeq)),
			Year:           alloc.Year,
			Seq:            alloc.NextSeq,
			PlanCode:       plan.Code,
			AmountMinor:    plan.PriceMinor,
			Currency:       plan.Currency,
			Status:         string(domain.InvoiceOpen),
			PeriodStart:    start,
			PeriodEnd:      end,
			Provider:       string(provider),
		})
		if err == nil {
			return postgres.ToInvoice(row), nil
		}
		// Two invoices raised in the same instant pick the same sequence. The
		// unique index catches it; retrying picks the next one up.
		if conflictOn(postgres.MapError(err), "invoice number") && attempt < invoiceAttempts-1 {
			continue
		}
		return domain.Invoice{}, fmt.Errorf("create invoice: %w", postgres.MapError(err))
	}
	return domain.Invoice{}, fmt.Errorf("create invoice: %d attempts collided on the number", invoiceAttempts)
}

// SetCancelAtPeriodEnd records that the customer does not want to renew. The
// plan keeps working until the period they paid for runs out — cancelling is
// not a refund, and taking the plan away early would be one in reverse.
func (s *Billing) SetCancelAtPeriodEnd(ctx context.Context, tenantID uuid.UUID, cancel bool) (domain.Subscription, error) {
	row, err := s.store.SetSubscriptionCancelAtPeriodEnd(ctx, gen.SetSubscriptionCancelAtPeriodEndParams{
		TenantID: tenantID, CancelAtPeriodEnd: cancel,
	})
	if err != nil {
		return domain.Subscription{}, fmt.Errorf("set cancel at period end: %w", postgres.MapError(err))
	}
	return postgres.ToSubscription(row), nil
}

// MarkPaid settles an invoice by hand: a bank transfer that cleared, cash, or a
// comped account.
//
// This is the manual provider's whole implementation, and it is deliberately on
// the authenticated admin API rather than behind an endpoint — a payment
// recorded by a person should carry which person, and an unauthenticated route
// could not.
func (s *Billing) MarkPaid(ctx context.Context, tenantID, invoiceID uuid.UUID, memo string) (domain.Invoice, error) {
	invoice, err := s.store.GetInvoice(ctx, gen.GetInvoiceParams{ID: invoiceID, TenantID: tenantID})
	if err != nil {
		return domain.Invoice{}, fmt.Errorf("get invoice: %w", postgres.MapError(err))
	}
	if !domain.InvoiceStatus(invoice.Status).Payable() {
		return domain.Invoice{}, domain.Errorf(domain.ErrConflict,
			"Invoice %s is already %s.", invoice.Number, invoice.Status)
	}

	var settled gen.Invoice
	err = s.store.InTx(ctx, func(q *gen.Queries) error {
		var memoPtr *string
		if memo != "" {
			memoPtr = &memo
		}
		settled, err = q.MarkInvoicePaid(ctx, gen.MarkInvoicePaidParams{ID: invoiceID, Memo: memoPtr})
		if err != nil {
			if errors.Is(postgres.MapError(err), domain.ErrNotFound) {
				// The compare-and-set found no open invoice, so someone settled
				// it between the read above and here.
				return domain.Errorf(domain.ErrConflict, "Invoice %s was already settled.", invoice.Number)
			}
			return fmt.Errorf("mark invoice paid: %w", postgres.MapError(err))
		}
		return s.activate(ctx, q, settled, domain.ProviderManual, nil)
	})
	if err != nil {
		return domain.Invoice{}, err
	}

	s.logger.Info("invoice marked paid",
		"invoice", settled.Number, "tenant_id", tenantID, "plan", settled.PlanCode)
	return postgres.ToInvoice(settled), nil
}

// VoidInvoice withdraws an unpaid bill.
func (s *Billing) VoidInvoice(ctx context.Context, tenantID, invoiceID uuid.UUID, memo string) (domain.Invoice, error) {
	invoice, err := s.store.GetInvoice(ctx, gen.GetInvoiceParams{ID: invoiceID, TenantID: tenantID})
	if err != nil {
		return domain.Invoice{}, fmt.Errorf("get invoice: %w", postgres.MapError(err))
	}
	current := domain.InvoiceStatus(invoice.Status)
	if !current.CanTransition(domain.InvoiceVoid) {
		return domain.Invoice{}, domain.Errorf(domain.ErrConflict,
			"An invoice that is %s cannot be voided.", current)
	}

	var memoPtr *string
	if memo != "" {
		memoPtr = &memo
	}
	row, err := s.store.SetInvoiceStatus(ctx, gen.SetInvoiceStatusParams{
		ID: invoiceID, Status: string(domain.InvoiceVoid), FromStatus: string(current), Memo: memoPtr,
	})
	if err != nil {
		if errors.Is(postgres.MapError(err), domain.ErrNotFound) {
			return domain.Invoice{}, domain.Errorf(domain.ErrConflict, "Invoice %s changed while you were voiding it.", invoice.Number)
		}
		return domain.Invoice{}, fmt.Errorf("void invoice: %w", postgres.MapError(err))
	}
	return postgres.ToInvoice(row), nil
}

// activate moves the subscription onto the invoice's plan for the period the
// invoice bought. It runs inside the caller's transaction, so a payment either
// settles the invoice *and* extends the plan or does neither.
func (s *Billing) activate(ctx context.Context, q *gen.Queries, invoice gen.Invoice, provider domain.PaymentProvider, ref *string) error {
	planRow, err := q.GetPlan(ctx, invoice.PlanCode)
	if err != nil {
		return fmt.Errorf("get plan: %w", postgres.MapError(err))
	}
	months := domain.BillingInterval(planRow.BillingInterval).Months()

	if _, err := q.ActivateSubscription(ctx, gen.ActivateSubscriptionParams{
		ID:          invoice.SubscriptionID,
		PlanCode:    invoice.PlanCode,
		Provider:    string(provider),
		ProviderRef: ref,
		Months:      int32(months),
	}); err != nil {
		return fmt.Errorf("activate subscription: %w", postgres.MapError(err))
	}
	return nil
}

// ------------------------------------------------------------------- ledger

// The methods below are what a payment provider sees. Each one is safe to call
// repeatedly with the same arguments, because every provider retries and none
// of them can be relied on to ask first.

func (s *Billing) Invoice(ctx context.Context, id uuid.UUID) (domain.Invoice, error) {
	row, err := s.store.GetInvoiceByID(ctx, id)
	if err != nil {
		return domain.Invoice{}, postgres.MapError(err)
	}
	return postgres.ToInvoice(row), nil
}

func (s *Billing) Transaction(ctx context.Context, provider domain.PaymentProvider, txnID string) (domain.PaymentTransaction, error) {
	row, err := s.store.GetPaymentTransaction(ctx, gen.GetPaymentTransactionParams{
		Provider: string(provider), ProviderTxnID: txnID,
	})
	if err != nil {
		return domain.PaymentTransaction{}, postgres.MapError(err)
	}
	return postgres.ToPaymentTransaction(row), nil
}

func (s *Billing) LiveTransaction(ctx context.Context, invoiceID uuid.UUID) (domain.PaymentTransaction, error) {
	row, err := s.store.GetLivePaymentTransaction(ctx, invoiceID)
	if err != nil {
		if errors.Is(postgres.MapError(err), domain.ErrNotFound) {
			return domain.PaymentTransaction{}, billing.ErrNoLiveTransaction
		}
		return domain.PaymentTransaction{}, postgres.MapError(err)
	}
	return postgres.ToPaymentTransaction(row), nil
}

func (s *Billing) Begin(ctx context.Context, in billing.BeginTransaction) (domain.PaymentTransaction, error) {
	row, err := s.store.CreatePaymentTransaction(ctx, gen.CreatePaymentTransactionParams{
		InvoiceID:     in.Invoice.ID,
		TenantID:      in.Invoice.TenantID,
		Provider:      string(in.Provider),
		ProviderTxnID: in.ProviderTxn,
		AmountMinor:   in.AmountMinor,
		CreatedMs:     in.CreatedMs,
	})
	if err != nil {
		// A live transaction already exists for this invoice, or this
		// transaction id is already known. Both are conflicts, and both are the
		// unique indexes doing the job the provider cannot be trusted to do.
		return domain.PaymentTransaction{}, postgres.MapError(err)
	}
	return postgres.ToPaymentTransaction(row), nil
}

// Perform settles a payment: the transaction, the invoice and the subscription
// all move in one database transaction.
//
// The compare-and-set on the transaction state is what makes a duplicated
// callback harmless. The second one finds no row in 'created', returns the
// record as it stands, and never reaches the renewal — which is the difference
// between a retry and a customer being given two months for one payment.
func (s *Billing) Perform(ctx context.Context, txn domain.PaymentTransaction, performedMs int64) (domain.PaymentTransaction, error) {
	var out gen.PaymentTransaction

	err := s.store.InTx(ctx, func(q *gen.Queries) error {
		performed, err := q.PerformPaymentTransaction(ctx, gen.PerformPaymentTransactionParams{
			ID: txn.ID, PerformedMs: &performedMs,
		})
		if err != nil {
			if errors.Is(postgres.MapError(err), domain.ErrNotFound) {
				// Someone else settled it. Report what is on record.
				existing, getErr := q.GetPaymentTransaction(ctx, gen.GetPaymentTransactionParams{
					Provider: string(txn.Provider), ProviderTxnID: txn.ProviderTxnID,
				})
				if getErr != nil {
					return postgres.MapError(getErr)
				}
				out = existing
				return nil
			}
			return fmt.Errorf("perform transaction: %w", postgres.MapError(err))
		}
		out = performed

		invoice, err := q.MarkInvoicePaid(ctx, gen.MarkInvoicePaidParams{ID: txn.InvoiceID})
		if err != nil {
			if errors.Is(postgres.MapError(err), domain.ErrNotFound) {
				// The invoice was settled by another route — a manual payment
				// that crossed with this one. The money is accounted for and
				// the plan is already active, so there is nothing to extend.
				s.logger.Warn("payment performed against an already-settled invoice",
					"invoice_id", txn.InvoiceID, "provider", txn.Provider, "txn", txn.ProviderTxnID)
				return nil
			}
			return fmt.Errorf("mark invoice paid: %w", postgres.MapError(err))
		}

		ref := txn.ProviderTxnID
		return s.activate(ctx, q, invoice, txn.Provider, &ref)
	})
	if err != nil {
		return domain.PaymentTransaction{}, err
	}

	s.logger.Info("payment performed",
		"provider", txn.Provider, "txn", txn.ProviderTxnID, "invoice_id", txn.InvoiceID)
	return postgres.ToPaymentTransaction(out), nil
}

// Cancel reverses a transaction. When the money had already been taken the
// invoice becomes refunded rather than open again: the period it bought was
// granted, and re-opening the bill would invite a second payment for it.
//
// The subscription is deliberately left alone. Clawing a plan back the instant
// a refund lands would cut a restaurant off mid-service; the daily sweep will
// move it to past_due when the period it was granted runs out, which is the
// same path everyone else takes.
func (s *Billing) Cancel(ctx context.Context, txn domain.PaymentTransaction, cancelledMs int64, reason int32) (domain.PaymentTransaction, error) {
	var out gen.PaymentTransaction

	err := s.store.InTx(ctx, func(q *gen.Queries) error {
		var reasonPtr *int32
		if reason != 0 {
			reasonPtr = &reason
		}
		cancelled, err := q.CancelPaymentTransaction(ctx, gen.CancelPaymentTransactionParams{
			ID: txn.ID, CancelledMs: &cancelledMs, CancelReason: reasonPtr,
		})
		if err != nil {
			if errors.Is(postgres.MapError(err), domain.ErrNotFound) {
				existing, getErr := q.GetPaymentTransaction(ctx, gen.GetPaymentTransactionParams{
					Provider: string(txn.Provider), ProviderTxnID: txn.ProviderTxnID,
				})
				if getErr != nil {
					return postgres.MapError(getErr)
				}
				out = existing
				return nil
			}
			return fmt.Errorf("cancel transaction: %w", postgres.MapError(err))
		}
		out = cancelled

		to, from := domain.InvoiceVoid, domain.InvoiceOpen
		if txn.State == domain.TxnPerformed {
			to, from = domain.InvoiceRefunded, domain.InvoicePaid
		}
		if _, err := q.SetInvoiceStatus(ctx, gen.SetInvoiceStatusParams{
			ID: txn.InvoiceID, Status: string(to), FromStatus: string(from),
		}); err != nil {
			if errors.Is(postgres.MapError(err), domain.ErrNotFound) {
				// The invoice is in some other state already. The transaction
				// is what the provider asked about and it is cancelled; the
				// mismatch is worth a log and nothing more.
				s.logger.Warn("cancelled a transaction whose invoice was not in the expected state",
					"invoice_id", txn.InvoiceID, "expected", from, "provider", txn.Provider)
				return nil
			}
			return fmt.Errorf("update invoice: %w", postgres.MapError(err))
		}
		return nil
	})
	if err != nil {
		return domain.PaymentTransaction{}, err
	}

	s.logger.Info("payment cancelled",
		"provider", txn.Provider, "txn", txn.ProviderTxnID, "reason", reason)
	return postgres.ToPaymentTransaction(out), nil
}

func (s *Billing) Statement(ctx context.Context, provider domain.PaymentProvider, fromMs, toMs int64) ([]domain.PaymentTransaction, error) {
	rows, err := s.store.ListPaymentTransactionsByRange(ctx, gen.ListPaymentTransactionsByRangeParams{
		Provider: string(provider), CreatedMs: fromMs, CreatedMs_2: toMs,
	})
	if err != nil {
		return nil, postgres.MapError(err)
	}
	return postgres.ToPaymentTransactions(rows), nil
}

// -------------------------------------------------------------------- sweep

// GracePeriod is how long an unpaid subscription keeps working after its period
// ends. Long enough for a bank transfer to clear over a weekend, short enough
// that a plan is not given away.
const GracePeriod = 3 * 24 * time.Hour

// SweepResult reports what one pass changed, for the log line the operator
// actually reads.
type SweepResult struct {
	Expired   int
	Suspended int
}

// Sweep is the daily expiry pass.
//
// Two statements rather than one, because they are two different decisions:
// a period that has run out stops being active, and an account that has also
// used up its grace stops being writable. Running them separately means a
// subscription always passes through past_due, which is the state a dunning
// email is sent from.
func (s *Billing) Sweep(ctx context.Context, grace time.Duration) (SweepResult, error) {
	expired, err := s.store.ExpireSubscriptions(ctx)
	if err != nil {
		return SweepResult{}, fmt.Errorf("expire subscriptions: %w", postgres.MapError(err))
	}
	for _, row := range expired {
		s.logger.Info("subscription period ended",
			"tenant_id", row.TenantID, "plan", row.PlanCode, "status", row.Status)
	}

	suspended, err := s.store.SuspendPastDueSubscriptions(ctx, grace.Seconds())
	if err != nil {
		return SweepResult{}, fmt.Errorf("suspend subscriptions: %w", postgres.MapError(err))
	}
	for _, row := range suspended {
		s.logger.Warn("subscription suspended after grace period",
			"tenant_id", row.TenantID, "plan", row.PlanCode)
	}

	return SweepResult{Expired: len(expired), Suspended: len(suspended)}, nil
}
