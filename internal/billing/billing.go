// Package billing holds the payment-provider abstraction: what every provider
// must be able to do, and the narrow view of our own data they are allowed to
// see.
//
// It deliberately does not import the service layer. A provider talks to a
// Ledger, which the service layer implements — so adding Stripe means writing
// one file here, not threading a new dependency through the application.
package billing

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"

	"github.com/ubilolxon/qrmenu/internal/domain"
)

// Provider is one way of taking money.
//
// Checkout hands back somewhere to send the customer; the provider then tells
// us what happened through its own callback, which is why Webhook is part of
// the interface rather than something wired separately — a provider that
// cannot report a payment is not a usable provider.
type Provider interface {
	Name() domain.PaymentProvider

	// Checkout returns the URL to send the customer to. It does not mark
	// anything paid: the only thing that may do that is a callback the provider
	// itself makes, verified against the provider's own credentials.
	Checkout(ctx context.Context, req CheckoutRequest) (CheckoutResult, error)

	// Webhook is the provider's callback endpoint, or nil when it has none —
	// the manual provider, where a human marks the invoice paid instead.
	//
	// It is an http.Handler rather than a "parse this request into an event"
	// function because the providers this targets do not push events: Payme
	// speaks JSON-RPC and expects a specific reply envelope for every call,
	// including its errors, and flattening that into a generic event type would
	// mean reconstructing the reply afterwards from less information.
	Webhook() http.Handler
}

type CheckoutRequest struct {
	Invoice  domain.Invoice
	ReturnTo string // where the provider should send the customer afterwards
}

type CheckoutResult struct {
	// RedirectURL is where the browser goes next. Empty means the provider
	// needs no redirect, which is what manual payment looks like.
	RedirectURL string `json:"redirect_url,omitempty"`
	// Instructions is shown instead of a redirect: bank details for a transfer,
	// or "an invoice has been emailed to you".
	Instructions string `json:"instructions,omitempty"`
}

// Ledger is the only view of our data a provider gets.
//
// Every method is written to be called more than once with the same arguments
// and reach the same end state, because every provider retries. That is the
// whole of the idempotency strategy: the guarantee lives in the ledger, not in
// each provider remembering to ask first.
type Ledger interface {
	// Invoice looks one up by id with no tenant scope, because an id is all a
	// provider was ever given.
	Invoice(ctx context.Context, id uuid.UUID) (domain.Invoice, error)

	// Transaction finds a provider's own transaction by its id.
	Transaction(ctx context.Context, provider domain.PaymentProvider, txnID string) (domain.PaymentTransaction, error)

	// LiveTransaction finds the unfinished or settled transaction for an
	// invoice, if there is one. It is how a second provider transaction against
	// an invoice already being paid is refused.
	LiveTransaction(ctx context.Context, invoiceID uuid.UUID) (domain.PaymentTransaction, error)

	// Begin records a provider's intent to collect. It must fail rather than
	// create a second live transaction for the same invoice.
	Begin(ctx context.Context, in BeginTransaction) (domain.PaymentTransaction, error)

	// Perform settles the transaction, marks the invoice paid and extends the
	// subscription — in one transaction, so a provider that receives a timeout
	// and retries cannot renew twice.
	Perform(ctx context.Context, txn domain.PaymentTransaction, performedMs int64) (domain.PaymentTransaction, error)

	// Cancel reverses a transaction, and the invoice with it when the money had
	// already been taken.
	Cancel(ctx context.Context, txn domain.PaymentTransaction, cancelledMs int64, reason int32) (domain.PaymentTransaction, error)

	// Statement lists transactions in a time range, which is how a provider
	// reconciles its records against ours.
	Statement(ctx context.Context, provider domain.PaymentProvider, fromMs, toMs int64) ([]domain.PaymentTransaction, error)
}

// ErrNoLiveTransaction is what a Ledger returns when an invoice has no
// transaction in flight. It is a normal answer, not a failure.
var ErrNoLiveTransaction = errors.New("no live transaction")

type BeginTransaction struct {
	Invoice     domain.Invoice
	Provider    domain.PaymentProvider
	ProviderTxn string
	AmountMinor int64
	CreatedMs   int64
}

// Registry is the set of providers this deployment has credentials for.
//
// A provider with no configuration is simply absent, so a missing secret
// produces "that payment method is not available" at checkout rather than a
// half-configured provider that fails at the callback — by which point the
// customer believes they have paid.
type Registry struct {
	providers map[domain.PaymentProvider]Provider
}

func NewRegistry(providers ...Provider) *Registry {
	r := &Registry{providers: make(map[domain.PaymentProvider]Provider, len(providers))}
	for _, p := range providers {
		if p != nil {
			r.providers[p.Name()] = p
		}
	}
	return r
}

func (r *Registry) Get(name domain.PaymentProvider) (Provider, error) {
	p, ok := r.providers[name]
	if !ok {
		return nil, domain.Errorf(domain.ErrNotFound,
			"Payment method %q is not available.", name)
	}
	return p, nil
}

// Names lists what is configured, in a stable order, for the pricing page.
func (r *Registry) Names() []domain.PaymentProvider {
	order := []domain.PaymentProvider{
		domain.ProviderPayme, domain.ProviderClick, domain.ProviderStripe, domain.ProviderManual,
	}
	out := make([]domain.PaymentProvider, 0, len(r.providers))
	for _, name := range order {
		if _, ok := r.providers[name]; ok {
			out = append(out, name)
		}
	}
	return out
}

// Each returns every configured provider, for mounting their callbacks.
func (r *Registry) Each(fn func(Provider)) {
	for _, name := range r.Names() {
		fn(r.providers[name])
	}
}

// Manual is payment by human: a bank transfer that arrived, cash, or a comped
// account. An admin marks the invoice paid.
//
// It ships in the MVP and stays afterwards. Every payment system eventually
// needs an override, and having one from the start means the first customer can
// be onboarded before any provider integration is finished.
type Manual struct {
	// Instructions is what the customer is shown at checkout — bank details,
	// typically. Configured rather than hard-coded because it is per-business.
	Instructions string
}

func (m Manual) Name() domain.PaymentProvider { return domain.ProviderManual }

func (m Manual) Checkout(_ context.Context, req CheckoutRequest) (CheckoutResult, error) {
	instructions := m.Instructions
	if instructions == "" {
		instructions = fmt.Sprintf("Pay invoice %s and we will activate your plan once it clears.", req.Invoice.Number)
	}
	return CheckoutResult{Instructions: instructions}, nil
}

// Webhook returns nil: there is no callback, which is the point. A manual
// payment is settled through the admin API by an authenticated person, and
// that path is auditable in a way an unauthenticated endpoint would not be.
func (m Manual) Webhook() http.Handler { return nil }
