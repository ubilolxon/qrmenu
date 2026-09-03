package handler

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/ubilolxon/qrmenu/internal/billing"
	"github.com/ubilolxon/qrmenu/internal/domain"
	"github.com/ubilolxon/qrmenu/internal/http/middleware"
	"github.com/ubilolxon/qrmenu/internal/httperr"
	"github.com/ubilolxon/qrmenu/internal/service"
)

// Billing is the owner's view of what they are paying for. Every route here is
// owner-only: a manager runs the menu, not the bank account.
type Billing struct {
	svc    *service.Billing
	plans  *service.Plan
	logger *slog.Logger
}

func NewBilling(svc *service.Billing, plans *service.Plan, logger *slog.Logger) Billing {
	return Billing{svc: svc, plans: plans, logger: logger}
}

// ------------------------------------------------------------------ requests

type subscribeRequest struct {
	PlanCode string `json:"plan_code"`
	Provider string `json:"provider"`
	// ReturnTo is where the provider sends the customer once they are done. It
	// is passed through to the provider verbatim and never followed by us.
	ReturnTo string `json:"return_to"`
}

type cancelRequest struct {
	CancelAtPeriodEnd bool `json:"cancel_at_period_end"`
}

type invoiceMemoRequest struct {
	Memo string `json:"memo"`
}

// ------------------------------------------------------------------ handlers

// Plans lists what can be bought, and how. The payment methods come from the
// registry rather than a constant, so a deployment with no Payme credentials
// does not offer Payme.
func (h Billing) Plans(w http.ResponseWriter, r *http.Request) {
	plans, err := h.plans.List(r.Context())
	if err != nil {
		RenderError(w, r, err)
		return
	}

	methods := []domain.PaymentProvider{}
	if reg := h.svc.Providers(); reg != nil {
		methods = reg.Names()
	}
	httperr.JSON(w, http.StatusOK, map[string]any{
		"plans":           plans,
		"payment_methods": methods,
	})
}

// Overview is the billing page: the plan, what it allows, what is used, and the
// invoice history.
func (h Billing) Overview(w http.ResponseWriter, r *http.Request) {
	identity := middleware.MustIdentity(r.Context())

	overview, err := h.svc.Overview(r.Context(), identity.TenantID)
	if err != nil {
		RenderError(w, r, err)
		return
	}
	httperr.JSON(w, http.StatusOK, overview)
}

// Subscribe raises an invoice and starts a checkout. It changes no plan: that
// happens when the money arrives.
func (h Billing) Subscribe(w http.ResponseWriter, r *http.Request) {
	identity := middleware.MustIdentity(r.Context())

	var req subscribeRequest
	if err := DecodeJSON(w, r, &req); err != nil {
		RenderError(w, r, err)
		return
	}

	var v domain.Validator
	req.PlanCode = strings.ToLower(strings.TrimSpace(req.PlanCode))
	v.Check(req.PlanCode != "", "plan_code", "is required")
	if err := v.Err(); err != nil {
		RenderError(w, r, err)
		return
	}

	provider := domain.PaymentProvider(strings.TrimSpace(req.Provider))
	if provider == "" {
		provider = domain.ProviderManual
	}

	checkout, err := h.svc.Subscribe(r.Context(), identity.TenantID, req.PlanCode, provider, req.ReturnTo)
	if err != nil {
		RenderError(w, r, err)
		return
	}

	// A free plan takes effect immediately and raises no invoice, so there is
	// nothing to create — 200 rather than 201 says the change is already done.
	status := http.StatusCreated
	if checkout.Invoice.ID == uuid.Nil {
		status = http.StatusOK
	}
	httperr.JSON(w, status, checkout)
}

// Cancel schedules — or unschedules — the end of the subscription. The plan
// keeps working until the period already paid for runs out.
func (h Billing) Cancel(w http.ResponseWriter, r *http.Request) {
	identity := middleware.MustIdentity(r.Context())

	var req cancelRequest
	if err := DecodeJSON(w, r, &req); err != nil {
		RenderError(w, r, err)
		return
	}

	sub, err := h.svc.SetCancelAtPeriodEnd(r.Context(), identity.TenantID, req.CancelAtPeriodEnd)
	if err != nil {
		RenderError(w, r, err)
		return
	}
	httperr.JSON(w, http.StatusOK, sub)
}

func (h Billing) Invoices(w http.ResponseWriter, r *http.Request) {
	identity := middleware.MustIdentity(r.Context())

	invoices, err := h.svc.Invoices(r.Context(), identity.TenantID)
	if err != nil {
		RenderError(w, r, err)
		return
	}
	httperr.JSON(w, http.StatusOK, map[string]any{"invoices": invoices})
}

// Pay records a payment made outside any provider — a transfer that cleared,
// cash, a comped account. It is the manual provider's entire implementation,
// and it lives on the authenticated API precisely so the payment carries which
// person recorded it.
func (h Billing) Pay(w http.ResponseWriter, r *http.Request) {
	identity := middleware.MustIdentity(r.Context())

	id, ok := pathID(w, r, "invoiceID", "invoice_id")
	if !ok {
		return
	}

	var req invoiceMemoRequest
	if err := DecodeJSON(w, r, &req); err != nil {
		RenderError(w, r, err)
		return
	}

	invoice, err := h.svc.MarkPaid(r.Context(), identity.TenantID, id, strings.TrimSpace(req.Memo))
	if err != nil {
		RenderError(w, r, err)
		return
	}

	// The service logs the invoice; this records the person, which the service
	// has no way to know. Money moving without a name attached is the one thing
	// an audit of this table could not reconstruct later.
	h.logger.Info("invoice marked paid by hand",
		"invoice", invoice.Number,
		"user_id", identity.UserID,
		"tenant_id", identity.TenantID,
		"request_id", middleware.RequestIDFrom(r.Context()))

	httperr.JSON(w, http.StatusOK, invoice)
}

func (h Billing) Void(w http.ResponseWriter, r *http.Request) {
	identity := middleware.MustIdentity(r.Context())

	id, ok := pathID(w, r, "invoiceID", "invoice_id")
	if !ok {
		return
	}

	var req invoiceMemoRequest
	if err := DecodeJSON(w, r, &req); err != nil {
		RenderError(w, r, err)
		return
	}

	invoice, err := h.svc.VoidInvoice(r.Context(), identity.TenantID, id, strings.TrimSpace(req.Memo))
	if err != nil {
		RenderError(w, r, err)
		return
	}
	httperr.JSON(w, http.StatusOK, invoice)
}

// MountWebhooks hangs each configured provider's callback under /webhooks.
//
// The path is the provider's own name, so the URL a merchant account is
// configured with is derivable rather than arbitrary. Providers with no
// callback — the manual one — mount nothing, which is why the check is here
// and not at the provider.
func MountWebhooks(mount func(pattern string, h http.Handler), reg *billing.Registry) {
	if reg == nil {
		return
	}
	reg.Each(func(p billing.Provider) {
		if h := p.Webhook(); h != nil {
			mount("/"+string(p.Name()), h)
		}
	})
}
