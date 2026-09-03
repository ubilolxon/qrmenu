package service

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/ubilolxon/qrmenu/internal/domain"
	"github.com/ubilolxon/qrmenu/internal/postgres"
	"github.com/ubilolxon/qrmenu/internal/postgres/gen"
)

// Plan answers "may this tenant do one more of that". It is the only place
// limits are enforced, and it is consulted on the write path only.
//
// Reads are never gated. A tenant who downgrades, stops paying, or is suspended
// keeps serving the menu a guest is scanning right now — the lever is that they
// cannot add anything new, not that the QR code stuck to their table goes dark.
type Plan struct {
	store *postgres.Store
}

func NewPlan(store *postgres.Store) *Plan {
	return &Plan{store: store}
}

// A nil *Plan permits everything. Every service that enforces limits takes one,
// and the tests that care about tenant isolation rather than billing pass nil
// instead of standing up a plan table — the same shape the rate limiter uses on
// the guest ordering path.
func (s *Plan) enabled() bool { return s != nil && s.store != nil }

func (s *Plan) List(ctx context.Context) ([]domain.Plan, error) {
	rows, err := s.store.ListPublicPlans(ctx)
	if err != nil {
		return nil, fmt.Errorf("list plans: %w", postgres.MapError(err))
	}
	return postgres.ToPlans(rows)
}

// Entitlement resolves what a tenant is allowed to do right now.
//
// Every tenant has exactly one live subscription — registration creates it and
// the partial unique index holds it — so a missing row is a broken invariant,
// not a free-tier tenant, and it is reported rather than defaulted away.
func (s *Plan) Entitlement(ctx context.Context, tenantID uuid.UUID) (domain.Entitlement, error) {
	row, err := s.store.GetEntitlement(ctx, tenantID)
	if err != nil {
		return domain.Entitlement{}, fmt.Errorf("get entitlement: %w", postgres.MapError(err))
	}
	return postgres.ToEntitlement(row)
}

// Check refuses a write that would take the tenant past their plan.
//
// The count is taken here rather than passed in, so a call site is one line and
// cannot pass the wrong number. Two concurrent creates can both read the same
// count and both pass; the result is one row over the limit, which is a
// tolerable outcome for a billing ceiling and much cheaper than serialising
// every write behind a lock on the subscription row.
func (s *Plan) Check(ctx context.Context, tenantID uuid.UUID, r domain.Resource) error {
	if !s.enabled() {
		return nil
	}
	ent, err := s.Entitlement(ctx, tenantID)
	if err != nil {
		return err
	}
	// Unlimited resources never need the count, and the count is the expensive
	// half — a tenant on the top plan pays nothing for these checks.
	if ent.Limits.Cap(r) == domain.Unlimited && ent.Subscription.Status.Writable() {
		return nil
	}
	current, err := s.usage(ctx, tenantID, r)
	if err != nil {
		return err
	}
	return ent.Check(r, current)
}

// CheckValue is for a resource a request sets outright instead of incrementing:
// the locales array on a restaurant is replaced wholesale by a PATCH, so the
// question is "may it be this long", not "may there be one more".
func (s *Plan) CheckValue(ctx context.Context, tenantID uuid.UUID, r domain.Resource, wanted int) error {
	if !s.enabled() {
		return nil
	}
	ent, err := s.Entitlement(ctx, tenantID)
	if err != nil {
		return err
	}
	return ent.CheckAtLeast(r, wanted)
}

// CheckStorage refuses an upload that would push the tenant over their quota.
//
// incoming is the declared size of the file about to be stored. Declared, not
// verified — the real size is known only after the bytes land — so this is a
// first gate, and the upload-complete step is where the true size is counted.
func (s *Plan) CheckStorage(ctx context.Context, tenantID uuid.UUID, incoming int64) error {
	if !s.enabled() {
		return nil
	}
	ent, err := s.Entitlement(ctx, tenantID)
	if err != nil {
		return err
	}
	if ent.Limits.Cap(domain.ResourceStorageMB) == domain.Unlimited && ent.Subscription.Status.Writable() {
		return nil
	}
	used, err := s.store.SumImageBytesByTenant(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("sum image bytes: %w", postgres.MapError(err))
	}
	// Rounded up, so a tenant one byte over their quota is over it. Rounding
	// down would let a 50 MB plan hold 50.9 MB and report 50.
	usedMB := int((used + incoming + bytesPerMB - 1) / bytesPerMB)
	return ent.CheckAtLeast(domain.ResourceStorageMB, usedMB)
}

const bytesPerMB = 1 << 20

// CheckFeature gates a plan flag. The flag is read off the entitlement here so
// a call site names the feature once instead of reaching into Limits itself.
func (s *Plan) CheckFeature(ctx context.Context, tenantID uuid.UUID, name string, pick func(domain.Limits) bool) error {
	if !s.enabled() {
		return nil
	}
	ent, err := s.Entitlement(ctx, tenantID)
	if err != nil {
		return err
	}
	return ent.CheckFeature(name, pick(ent.Limits))
}

// usage counts what exists now for one resource, across the whole tenant —
// which is the unit a plan is sold in, not one restaurant.
func (s *Plan) usage(ctx context.Context, tenantID uuid.UUID, r domain.Resource) (int, error) {
	var (
		n   int64
		err error
	)
	switch r {
	case domain.ResourceRestaurants:
		n, err = s.store.CountRestaurants(ctx, tenantID)
	case domain.ResourceMenuItems:
		n, err = s.store.CountItemsByTenant(ctx, tenantID)
	case domain.ResourceTables:
		n, err = s.store.CountTablesByTenant(ctx, tenantID)
	case domain.ResourceStaffUsers:
		n, err = s.store.CountActiveUsersByTenant(ctx, tenantID)
	default:
		// Reached only by a resource with no counter wired up, which would
		// otherwise be an unenforced limit that looks enforced.
		return 0, fmt.Errorf("no usage counter for resource %q", r)
	}
	if err != nil {
		return 0, fmt.Errorf("count %s: %w", r, postgres.MapError(err))
	}
	return int(n), nil
}

// EnsureSubscription puts a brand-new tenant on the free plan.
//
// It runs inside the registration transaction, so a signup that fails halfway
// leaves neither a tenant nor a dangling subscription.
func EnsureSubscription(ctx context.Context, q *gen.Queries, tenantID uuid.UUID) error {
	_, err := q.CreateSubscription(ctx, gen.CreateSubscriptionParams{
		TenantID: tenantID,
		PlanCode: domain.FreePlanCode,
		Status:   string(domain.SubActive),
		Provider: string(domain.ProviderManual),
		// No period end: a free plan does not expire, and the daily sweep skips
		// every row where this is null.
		CurrentPeriodEnd: nil,
	})
	if err != nil {
		return fmt.Errorf("create subscription: %w", postgres.MapError(err))
	}
	return nil
}
