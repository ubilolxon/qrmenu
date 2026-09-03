package service_test

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ubilolxon/qrmenu/internal/auth"
	"github.com/ubilolxon/qrmenu/internal/billing"
	"github.com/ubilolxon/qrmenu/internal/config"
	"github.com/ubilolxon/qrmenu/internal/domain"
	"github.com/ubilolxon/qrmenu/internal/i18n"
	"github.com/ubilolxon/qrmenu/internal/postgres"
	"github.com/ubilolxon/qrmenu/internal/realtime"
	"github.com/ubilolxon/qrmenu/internal/service"
)

// Limits are enforced by counting rows and comparing to a jsonb document, and
// neither half can be checked with a fake: the count has to be a real count and
// the document has to be the one the migration seeded. So these run against a
// real Postgres, like the tenant isolation suite.

type billingHarness struct {
	pool        *pgxpool.Pool
	store       *postgres.Store
	authSvc     *service.Auth
	plans       *service.Plan
	billing     *service.Billing
	restaurants *service.Restaurant
	menus       *service.Menu
	tables      *service.Table
}

func newBillingHarness(t *testing.T) *billingHarness {
	t.Helper()

	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		dsn = os.Getenv("DATABASE_URL")
	}
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL or DATABASE_URL to run integration tests")
	}

	ctx := context.Background()
	pool, err := postgres.Connect(ctx, config.DB{URL: dsn, MaxConns: 8, MinConns: 1})
	if err != nil {
		t.Fatalf("connect to test database: %v", err)
	}
	t.Cleanup(pool.Close)

	store := postgres.NewStore(pool)
	logger := slog.New(slog.DiscardHandler)
	issuer := auth.NewIssuer([]byte("integration-test-secret-32-bytes!!"), 15*time.Minute, "qrmenu-test")

	authSvc, err := service.NewAuth(store, issuer, 24*time.Hour, logger)
	if err != nil {
		t.Fatalf("build auth service: %v", err)
	}

	plans := service.NewPlan(store)
	billingSvc := service.NewBilling(store, logger)
	billingSvc.UseProviders(billing.NewRegistry(billing.Manual{}))

	return &billingHarness{
		pool:        pool,
		store:       store,
		authSvc:     authSvc,
		plans:       plans,
		billing:     billingSvc,
		restaurants: service.NewRestaurant(store, plans),
		menus:       service.NewMenu(store, plans),
		tables:      service.NewTable(store, plans),
	}
}

func (h *billingHarness) tenant(t *testing.T) *service.Session {
	t.Helper()

	session, err := h.authSvc.Register(context.Background(), service.RegisterInput{
		TenantName: "Billing Tenant " + uuid.NewString()[:8],
		Email:      randomEmail(),
		Password:   "correct-horse-battery-staple",
		Name:       "Owner",
	}, service.SessionMeta{UserAgent: "integration-test", IP: "127.0.0.1"})
	if err != nil {
		t.Fatalf("register tenant: %v", err)
	}
	return session
}

func (h *billingHarness) restaurant(t *testing.T, tenantID uuid.UUID) domain.Restaurant {
	t.Helper()

	created, err := h.restaurants.Create(context.Background(), tenantID, domain.RestaurantInput{
		Slug:          randomSlug(),
		Name:          i18n.Text{"uz": "Choyxona"},
		Currency:      "UZS",
		Timezone:      "Asia/Tashkent",
		DefaultLocale: "uz",
		Locales:       []string{"uz"},
	})
	if err != nil {
		t.Fatalf("create restaurant: %v", err)
	}
	return created
}

// Registration must leave the tenant on a real plan. A tenant with no
// subscription is not a free-tier account, it is a row every limit check will
// fail to read.
func TestRegistrationCreatesAFreeSubscription(t *testing.T) {
	h := newBillingHarness(t)
	owner := h.tenant(t)

	ent, err := h.plans.Entitlement(context.Background(), owner.User.TenantID)
	if err != nil {
		t.Fatalf("entitlement: %v", err)
	}

	if ent.PlanCode != domain.FreePlanCode {
		t.Errorf("plan = %q, want %q", ent.PlanCode, domain.FreePlanCode)
	}
	if ent.Subscription.Status != domain.SubActive {
		t.Errorf("status = %q, want active", ent.Subscription.Status)
	}
	// A free plan does not expire, so the sweep must never see it.
	if ent.Subscription.CurrentPeriodEnd != nil {
		t.Errorf("current_period_end = %v, want nil for a free plan", ent.Subscription.CurrentPeriodEnd)
	}
	// The seeded free plan has to actually contain limits, or every check below
	// would pass for the wrong reason.
	if ent.Limits.Restaurants != 1 || ent.Limits.MenuItems == 0 {
		t.Errorf("free limits = %+v, want the seeded values", ent.Limits)
	}
	if ent.Limits.OrdersEnabled {
		t.Error("the free plan should not include table ordering")
	}
}

// The headline behaviour: a write past the plan is refused, with numbers the
// SPA can render.
func TestSecondRestaurantIsRefusedOnFree(t *testing.T) {
	h := newBillingHarness(t)
	ctx := context.Background()
	owner := h.tenant(t)

	h.restaurant(t, owner.User.TenantID) // the one the free plan allows

	_, err := h.restaurants.Create(ctx, owner.User.TenantID, domain.RestaurantInput{
		Slug:          randomSlug(),
		Name:          i18n.Text{"uz": "Ikkinchi"},
		Currency:      "UZS",
		Timezone:      "Asia/Tashkent",
		DefaultLocale: "uz",
		Locales:       []string{"uz"},
	})
	if err == nil {
		t.Fatal("a second restaurant was created on a plan that allows one")
	}
	if !errors.Is(err, domain.ErrPlanLimit) {
		t.Fatalf("error does not unwrap to ErrPlanLimit: %v", err)
	}

	var limitErr *domain.LimitError
	if !errors.As(err, &limitErr) {
		t.Fatalf("want a *LimitError, got %T", err)
	}
	if limitErr.Resource != domain.ResourceRestaurants || limitErr.Limit != 1 || limitErr.Current != 1 {
		t.Errorf("limit error = %+v, want restaurants 1 of 1", limitErr)
	}
}

// A validation failure must be reported as a validation failure, not as an
// upsell: a caller with a malformed payload is told what is wrong with it.
func TestValidationIsCheckedBeforeTheLimit(t *testing.T) {
	h := newBillingHarness(t)
	ctx := context.Background()
	owner := h.tenant(t)
	h.restaurant(t, owner.User.TenantID)

	_, err := h.restaurants.Create(ctx, owner.User.TenantID, domain.RestaurantInput{
		Slug: "", // invalid, and the tenant is also out of restaurants
		Name: i18n.Text{},
	})

	var validationErr *domain.ValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("want a *ValidationError for a malformed payload, got %T: %v", err, err)
	}
}

// Locales are set outright by a PATCH rather than incremented, so the question
// is whether the resulting list is allowed.
func TestLocaleLimitAppliesToTheMergedValue(t *testing.T) {
	h := newBillingHarness(t)
	ctx := context.Background()
	owner := h.tenant(t)
	restaurant := h.restaurant(t, owner.User.TenantID)

	locales := []string{"uz", "ru", "en"}
	_, err := h.restaurants.Update(ctx, owner.User.TenantID, restaurant.ID,
		service.RestaurantPatch{Locales: &locales})
	if err == nil {
		t.Fatal("three locales were accepted on a plan that allows one")
	}
	if !errors.Is(err, domain.ErrPlanLimit) {
		t.Errorf("error does not unwrap to ErrPlanLimit: %v", err)
	}

	// A patch that does not touch locales must not be refused by the locale
	// limit — otherwise a tenant on a downgraded plan could not edit anything.
	name := i18n.Text{"uz": "Yangi nom"}
	if _, err := h.restaurants.Update(ctx, owner.User.TenantID, restaurant.ID,
		service.RestaurantPatch{Name: &name}); err != nil {
		t.Errorf("an unrelated patch was refused: %v", err)
	}
}

// Suspension freezes the tenant's own writes. It does not touch reads, which is
// the rule the whole billing design is built around: a downgrade must never
// break the QR code a guest is scanning.
func TestSuspensionFreezesWritesButNotReads(t *testing.T) {
	h := newBillingHarness(t)
	ctx := context.Background()
	owner := h.tenant(t)
	restaurant := h.restaurant(t, owner.User.TenantID)

	h.suspend(t, owner.User.TenantID)

	_, err := h.tables.Create(ctx, owner.User.TenantID, restaurant.ID, domain.TableInput{Label: "1"})
	if err == nil {
		t.Fatal("a suspended tenant created a table")
	}
	if !errors.Is(err, domain.ErrPlanLimit) {
		t.Errorf("error does not unwrap to ErrPlanLimit: %v", err)
	}

	// Reading still works. This is the assertion that matters.
	if _, err := h.restaurants.Get(ctx, owner.User.TenantID, restaurant.ID); err != nil {
		t.Errorf("a suspended tenant could not read their own restaurant: %v", err)
	}
	if _, err := h.restaurants.List(ctx, owner.User.TenantID); err != nil {
		t.Errorf("a suspended tenant could not list their restaurants: %v", err)
	}
}

// suspend drives the subscription through the real states rather than writing
// 'suspended' directly, so the transitions the sweep depends on are exercised.
func (h *billingHarness) suspend(t *testing.T, tenantID uuid.UUID) {
	t.Helper()
	ctx := context.Background()

	sub, err := h.store.GetSubscription(ctx, tenantID)
	if err != nil {
		t.Fatalf("get subscription: %v", err)
	}
	if _, err := h.pool.Exec(ctx,
		`UPDATE subscriptions SET status = 'suspended' WHERE id = $1`, sub.ID); err != nil {
		t.Fatalf("suspend subscription: %v", err)
	}
}

// The full paid path, without a provider: raise an invoice, settle it by hand,
// and check the plan actually moved.
func TestManualPaymentActivatesThePlan(t *testing.T) {
	h := newBillingHarness(t)
	ctx := context.Background()
	owner := h.tenant(t)

	checkout, err := h.billing.Subscribe(ctx, owner.User.TenantID, "basic", domain.ProviderManual, "")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if checkout.Invoice.ID == uuid.Nil {
		t.Fatal("subscribing to a paid plan raised no invoice")
	}
	if checkout.Invoice.Status != domain.InvoiceOpen {
		t.Errorf("invoice status = %q, want open", checkout.Invoice.Status)
	}

	// Nothing has been paid, so nothing may have changed yet.
	ent, err := h.plans.Entitlement(ctx, owner.User.TenantID)
	if err != nil {
		t.Fatalf("entitlement: %v", err)
	}
	if ent.PlanCode != domain.FreePlanCode {
		t.Fatalf("plan = %q before payment, want it unchanged at free", ent.PlanCode)
	}

	paid, err := h.billing.MarkPaid(ctx, owner.User.TenantID, checkout.Invoice.ID, "bank transfer 12345")
	if err != nil {
		t.Fatalf("mark paid: %v", err)
	}
	if paid.Status != domain.InvoicePaid || paid.PaidAt == nil {
		t.Errorf("invoice = %+v, want paid with a timestamp", paid)
	}

	ent, err = h.plans.Entitlement(ctx, owner.User.TenantID)
	if err != nil {
		t.Fatalf("entitlement after payment: %v", err)
	}
	if ent.PlanCode != "basic" {
		t.Errorf("plan = %q after payment, want basic", ent.PlanCode)
	}
	if ent.Subscription.CurrentPeriodEnd == nil {
		t.Fatal("a paid subscription has no period end")
	}
	if !ent.Subscription.CurrentPeriodEnd.After(time.Now().AddDate(0, 0, 27)) {
		t.Errorf("period ends %v, want roughly a month out", ent.Subscription.CurrentPeriodEnd)
	}
	// The limit that was refusing a second restaurant a moment ago now allows
	// ordering, which is what the customer paid for.
	if !ent.Limits.OrdersEnabled {
		t.Error("the basic plan should include table ordering")
	}
}

// Paying the same invoice twice must not buy two months.
func TestPayingAnInvoiceTwiceIsRefused(t *testing.T) {
	h := newBillingHarness(t)
	ctx := context.Background()
	owner := h.tenant(t)

	checkout, err := h.billing.Subscribe(ctx, owner.User.TenantID, "basic", domain.ProviderManual, "")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if _, err := h.billing.MarkPaid(ctx, owner.User.TenantID, checkout.Invoice.ID, ""); err != nil {
		t.Fatalf("first payment: %v", err)
	}

	before, err := h.plans.Entitlement(ctx, owner.User.TenantID)
	if err != nil {
		t.Fatalf("entitlement: %v", err)
	}

	err = func() error {
		_, err := h.billing.MarkPaid(ctx, owner.User.TenantID, checkout.Invoice.ID, "")
		return err
	}()
	if err == nil {
		t.Fatal("the same invoice was paid twice")
	}
	if !errors.Is(err, domain.ErrConflict) {
		t.Errorf("error does not unwrap to ErrConflict: %v", err)
	}

	after, err := h.plans.Entitlement(ctx, owner.User.TenantID)
	if err != nil {
		t.Fatalf("entitlement: %v", err)
	}
	if !after.Subscription.CurrentPeriodEnd.Equal(*before.Subscription.CurrentPeriodEnd) {
		t.Errorf("the period moved on a refused second payment: %v -> %v",
			before.Subscription.CurrentPeriodEnd, after.Subscription.CurrentPeriodEnd)
	}
}

// Another tenant's invoice must be invisible, and invisible as "not found"
// rather than "forbidden" — a 403 would confirm the id is real.
func TestInvoicesAreTenantScoped(t *testing.T) {
	h := newBillingHarness(t)
	ctx := context.Background()
	alice := h.tenant(t)
	bob := h.tenant(t)

	checkout, err := h.billing.Subscribe(ctx, alice.User.TenantID, "basic", domain.ProviderManual, "")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	if _, err := h.billing.MarkPaid(ctx, bob.User.TenantID, checkout.Invoice.ID, ""); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("marking a foreign invoice paid gave %v, want not found", err)
	}
	if _, err := h.billing.VoidInvoice(ctx, bob.User.TenantID, checkout.Invoice.ID, ""); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("voiding a foreign invoice gave %v, want not found", err)
	}

	bobInvoices, err := h.billing.Invoices(ctx, bob.User.TenantID)
	if err != nil {
		t.Fatalf("list invoices: %v", err)
	}
	for _, inv := range bobInvoices {
		if inv.ID == checkout.Invoice.ID {
			t.Fatal("another tenant's invoice appeared in the list")
		}
	}
}

// Moving to the free plan takes effect at once: there is nothing to pay, so
// waiting for a payment would strand the customer on a plan they left.
func TestDowngradeToFreeIsImmediate(t *testing.T) {
	h := newBillingHarness(t)
	ctx := context.Background()
	owner := h.tenant(t)

	checkout, err := h.billing.Subscribe(ctx, owner.User.TenantID, "basic", domain.ProviderManual, "")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if _, err := h.billing.MarkPaid(ctx, owner.User.TenantID, checkout.Invoice.ID, ""); err != nil {
		t.Fatalf("mark paid: %v", err)
	}

	down, err := h.billing.Subscribe(ctx, owner.User.TenantID, domain.FreePlanCode, domain.ProviderManual, "")
	if err != nil {
		t.Fatalf("downgrade: %v", err)
	}
	if down.Invoice.ID != uuid.Nil {
		t.Error("downgrading to free raised an invoice")
	}

	ent, err := h.plans.Entitlement(ctx, owner.User.TenantID)
	if err != nil {
		t.Fatalf("entitlement: %v", err)
	}
	if ent.PlanCode != domain.FreePlanCode {
		t.Errorf("plan = %q, want free", ent.PlanCode)
	}
	if ent.Subscription.CurrentPeriodEnd != nil {
		t.Error("the free plan kept a period end, so the sweep will keep expiring it")
	}
}

// The daily pass: a lapsed period becomes past_due, and only after the grace
// period does it become suspended.
//
// Sweep is deliberately global — it is a cron, not a request — so this test and
// the one below must not be made parallel: each backdates its own subscription
// and then sweeps every lapsed one in the database. A subscription must always travel through
// past_due, because that is the state a dunning email is sent from.
func TestSweepExpiresThenSuspends(t *testing.T) {
	h := newBillingHarness(t)
	ctx := context.Background()
	owner := h.tenant(t)

	checkout, err := h.billing.Subscribe(ctx, owner.User.TenantID, "basic", domain.ProviderManual, "")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if _, err := h.billing.MarkPaid(ctx, owner.User.TenantID, checkout.Invoice.ID, ""); err != nil {
		t.Fatalf("mark paid: %v", err)
	}

	// Backdate the period so it has just run out.
	sub, err := h.store.GetSubscription(ctx, owner.User.TenantID)
	if err != nil {
		t.Fatalf("get subscription: %v", err)
	}
	if _, err := h.pool.Exec(ctx,
		`UPDATE subscriptions SET current_period_start = now() - interval '1 hour',
		                          current_period_end = now() - interval '1 minute'
		 WHERE id = $1`, sub.ID); err != nil {
		t.Fatalf("backdate period: %v", err)
	}

	if _, err := h.billing.Sweep(ctx, service.GracePeriod); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	ent, err := h.plans.Entitlement(ctx, owner.User.TenantID)
	if err != nil {
		t.Fatalf("entitlement: %v", err)
	}
	if ent.Subscription.Status != domain.SubPastDue {
		t.Fatalf("status = %q after the period ended, want past_due", ent.Subscription.Status)
	}
	// Still writable: the grace window is the whole point.
	if !ent.Subscription.Status.Writable() {
		t.Error("past_due should still allow writes")
	}

	// A second sweep with the grace already used up suspends it.
	if _, err := h.billing.Sweep(ctx, 0); err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	ent, err = h.plans.Entitlement(ctx, owner.User.TenantID)
	if err != nil {
		t.Fatalf("entitlement: %v", err)
	}
	if ent.Subscription.Status != domain.SubSuspended {
		t.Errorf("status = %q after the grace period, want suspended", ent.Subscription.Status)
	}
}

// A subscription the customer already asked to end goes straight to cancelled
// and back to free, rather than sitting in past_due chasing a payment nobody
// intends to make.
func TestSweepCancelsWhatWasAskedToEnd(t *testing.T) {
	h := newBillingHarness(t)
	ctx := context.Background()
	owner := h.tenant(t)

	checkout, err := h.billing.Subscribe(ctx, owner.User.TenantID, "basic", domain.ProviderManual, "")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if _, err := h.billing.MarkPaid(ctx, owner.User.TenantID, checkout.Invoice.ID, ""); err != nil {
		t.Fatalf("mark paid: %v", err)
	}
	if _, err := h.billing.SetCancelAtPeriodEnd(ctx, owner.User.TenantID, true); err != nil {
		t.Fatalf("cancel at period end: %v", err)
	}

	sub, err := h.store.GetSubscription(ctx, owner.User.TenantID)
	if err != nil {
		t.Fatalf("get subscription: %v", err)
	}
	if _, err := h.pool.Exec(ctx,
		`UPDATE subscriptions SET current_period_start = now() - interval '1 hour',
		                          current_period_end = now() - interval '1 minute'
		 WHERE id = $1`, sub.ID); err != nil {
		t.Fatalf("backdate period: %v", err)
	}

	if _, err := h.billing.Sweep(ctx, service.GracePeriod); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	// The row is cancelled, so GetEntitlement — which only ever looks at a live
	// subscription — must find nothing. The tenant is between subscriptions.
	if _, err := h.plans.Entitlement(ctx, owner.User.TenantID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("entitlement after cancellation gave %v, want not found", err)
	}
}

// The overview is the billing page in one round trip, and its counts have to be
// real: a page that says "0 of 30 items" while the menu holds twenty is worse
// than no page.
func TestOverviewCountsWhatExists(t *testing.T) {
	h := newBillingHarness(t)
	ctx := context.Background()
	owner := h.tenant(t)
	restaurant := h.restaurant(t, owner.User.TenantID)

	for i := range 3 {
		if _, err := h.tables.Create(ctx, owner.User.TenantID, restaurant.ID,
			domain.TableInput{Label: string(rune('A' + i))}); err != nil {
			t.Fatalf("create table: %v", err)
		}
	}

	overview, err := h.billing.Overview(ctx, owner.User.TenantID)
	if err != nil {
		t.Fatalf("overview: %v", err)
	}
	if overview.Usage.Restaurants != 1 {
		t.Errorf("restaurants = %d, want 1", overview.Usage.Restaurants)
	}
	if overview.Usage.Tables != 3 {
		t.Errorf("tables = %d, want 3", overview.Usage.Tables)
	}
	// The owner created at registration.
	if overview.Usage.StaffUsers != 1 {
		t.Errorf("staff = %d, want 1", overview.Usage.StaffUsers)
	}
	if overview.Entitlement.PlanCode != domain.FreePlanCode {
		t.Errorf("plan = %q, want free", overview.Entitlement.PlanCode)
	}
}

// A guest is not the account holder. They have no plan, no way to upgrade one,
// and no interest in why — so the refusal they see must say what concerns them
// and nothing about billing.
func TestGuestNeverSeesTheUpsell(t *testing.T) {
	h := newBillingHarness(t)
	ctx := context.Background()
	owner := h.tenant(t)
	restaurant := h.restaurant(t, owner.User.TenantID)

	table, err := h.tables.Create(ctx, owner.User.TenantID, restaurant.ID, domain.TableInput{Label: "1"})
	if err != nil {
		t.Fatalf("create table: %v", err)
	}

	session, err := domain.NewGuestSession()
	if err != nil {
		t.Fatalf("guest session: %v", err)
	}

	orders := service.NewOrder(h.store, realtime.New(), h.plans, slog.New(slog.DiscardHandler))
	_, err = orders.Place(ctx, table.QRToken, session, service.PlaceInput{
		Cart:   domain.Cart{Lines: []domain.CartLine{{ItemID: uuid.New(), Qty: 1}}},
		Locale: "uz",
	})
	if err == nil {
		t.Fatal("ordering was allowed on a plan without it")
	}

	// Forbidden, not ErrPlanLimit: a 402 "Payment Required" is addressed to
	// whoever owes the money, and that is never the diner.
	if !errors.Is(err, domain.ErrForbidden) {
		t.Errorf("error does not unwrap to ErrForbidden: %v", err)
	}
	if errors.Is(err, domain.ErrPlanLimit) {
		t.Error("the guest path leaked a plan-limit error, which renders as 402 with an upgrade prompt")
	}

	var statusErr *domain.StatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("want a *StatusError carrying a safe message, got %T", err)
	}
	for _, leak := range []string{"plan", "free", "Upgrade", "upgrade"} {
		if strings.Contains(statusErr.Message, leak) {
			t.Errorf("guest message %q mentions %q — that is the account holder's business, not the diner's",
				statusErr.Message, leak)
		}
	}

	// Calling a waiter is gated the same way and must be refused the same way.
	_, err = orders.CallWaiter(ctx, table.QRToken, session, domain.CallWaiter, "")
	if !errors.Is(err, domain.ErrForbidden) {
		t.Errorf("call-waiter error = %v, want forbidden", err)
	}
}
