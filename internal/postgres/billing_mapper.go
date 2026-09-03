package postgres

import (
	"fmt"

	"github.com/ubilolxon/qrmenu/internal/domain"
	"github.com/ubilolxon/qrmenu/internal/i18n"
	"github.com/ubilolxon/qrmenu/internal/postgres/gen"
)

func ToPlan(p gen.Plan) (domain.Plan, error) {
	name, err := i18n.FromRaw(p.Name)
	if err != nil {
		return domain.Plan{}, fmt.Errorf("plan %s name: %w", p.Code, err)
	}
	description, err := i18n.FromRaw(p.Description)
	if err != nil {
		return domain.Plan{}, fmt.Errorf("plan %s description: %w", p.Code, err)
	}
	limits, err := domain.ParseLimits(p.Limits)
	if err != nil {
		return domain.Plan{}, fmt.Errorf("plan %s: %w", p.Code, err)
	}
	return domain.Plan{
		Code:            p.Code,
		Name:            name,
		Description:     description,
		PriceMinor:      p.PriceMinor,
		Currency:        p.Currency,
		BillingInterval: domain.BillingInterval(p.BillingInterval),
		TrialDays:       int(p.TrialDays),
		Limits:          limits,
		IsPublic:        p.IsPublic,
		SortOrder:       int(p.SortOrder),
	}, nil
}

func ToPlans(rows []gen.Plan) ([]domain.Plan, error) {
	out := make([]domain.Plan, len(rows))
	for i, r := range rows {
		p, err := ToPlan(r)
		if err != nil {
			return nil, err
		}
		out[i] = p
	}
	return out, nil
}

func ToSubscription(s gen.Subscription) domain.Subscription {
	return domain.Subscription{
		ID:                 s.ID,
		TenantID:           s.TenantID,
		PlanCode:           s.PlanCode,
		Status:             domain.SubscriptionStatus(s.Status),
		Provider:           domain.PaymentProvider(s.Provider),
		ProviderRef:        s.ProviderRef,
		CurrentPeriodStart: s.CurrentPeriodStart,
		CurrentPeriodEnd:   s.CurrentPeriodEnd,
		CancelAtPeriodEnd:  s.CancelAtPeriodEnd,
		CancelledAt:        s.CancelledAt,
		CreatedAt:          s.CreatedAt,
		UpdatedAt:          s.UpdatedAt,
	}
}

// ToEntitlement flattens the subscription-plus-plan join into the one value the
// limit checks ask their questions of.
func ToEntitlement(row gen.GetEntitlementRow) (domain.Entitlement, error) {
	limits, err := domain.ParseLimits(row.PlanLimits)
	if err != nil {
		return domain.Entitlement{}, fmt.Errorf("plan %s: %w", row.Subscription.PlanCode, err)
	}
	name, err := i18n.FromRaw(row.PlanName)
	if err != nil {
		return domain.Entitlement{}, fmt.Errorf("plan %s name: %w", row.Subscription.PlanCode, err)
	}
	return domain.Entitlement{
		Subscription: ToSubscription(row.Subscription),
		PlanCode:     row.Subscription.PlanCode,
		PlanName:     name,
		PriceMinor:   row.PlanPriceMinor,
		Currency:     row.PlanCurrency,
		Interval:     domain.BillingInterval(row.PlanBillingInterval),
		Limits:       limits,
	}, nil
}

func ToInvoice(i gen.Invoice) domain.Invoice {
	return domain.Invoice{
		ID:             i.ID,
		TenantID:       i.TenantID,
		SubscriptionID: i.SubscriptionID,
		Number:         i.Number,
		PlanCode:       i.PlanCode,
		AmountMinor:    i.AmountMinor,
		Currency:       i.Currency,
		Status:         domain.InvoiceStatus(i.Status),
		PeriodStart:    i.PeriodStart,
		PeriodEnd:      i.PeriodEnd,
		Provider:       domain.PaymentProvider(i.Provider),
		PaidAt:         i.PaidAt,
		Memo:           i.Memo,
		CreatedAt:      i.CreatedAt,
	}
}

func ToInvoices(rows []gen.Invoice) []domain.Invoice {
	out := make([]domain.Invoice, len(rows))
	for i, r := range rows {
		out[i] = ToInvoice(r)
	}
	return out
}

func ToPaymentTransaction(t gen.PaymentTransaction) domain.PaymentTransaction {
	return domain.PaymentTransaction{
		ID:            t.ID,
		InvoiceID:     t.InvoiceID,
		TenantID:      t.TenantID,
		Provider:      domain.PaymentProvider(t.Provider),
		ProviderTxnID: t.ProviderTxnID,
		State:         domain.TxnState(t.State),
		AmountMinor:   t.AmountMinor,
		CreatedMs:     t.CreatedMs,
		PerformedMs:   t.PerformedMs,
		CancelledMs:   t.CancelledMs,
		CancelReason:  t.CancelReason,
		CreatedAt:     t.CreatedAt,
	}
}

func ToPaymentTransactions(rows []gen.PaymentTransaction) []domain.PaymentTransaction {
	out := make([]domain.PaymentTransaction, len(rows))
	for i, r := range rows {
		out[i] = ToPaymentTransaction(r)
	}
	return out
}
