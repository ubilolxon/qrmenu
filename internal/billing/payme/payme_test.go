package payme_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ubilolxon/qrmenu/internal/billing"
	"github.com/ubilolxon/qrmenu/internal/billing/payme"
	"github.com/ubilolxon/qrmenu/internal/domain"
)

// These tests exercise the endpoint Payme actually calls, over HTTP, with a
// fake ledger underneath. The ledger counts how many times money was moved,
// because the failure this whole design exists to prevent is a retried callback
// settling the same invoice twice.

const (
	testKey    = "merchant-key-that-is-long-enough"
	testAmount = 149_000 // som, as stored
	testTiyin  = testAmount * payme.TiyinPerSom
)

// ------------------------------------------------------------------- ledger

type fakeLedger struct {
	mu sync.Mutex

	invoice domain.Invoice
	txns    map[string]*domain.PaymentTransaction

	// performs counts settlements. It is the assertion that matters most here:
	// two PerformTransaction calls must leave it at one.
	performs int
	cancels  int
	failNext error
}

func newLedger() *fakeLedger {
	return &fakeLedger{
		invoice: domain.Invoice{
			ID:          uuid.New(),
			TenantID:    uuid.New(),
			Number:      "INV-2026-0001",
			AmountMinor: testAmount,
			Currency:    "UZS",
			Status:      domain.InvoiceOpen,
		},
		txns: map[string]*domain.PaymentTransaction{},
	}
}

func (l *fakeLedger) Invoice(_ context.Context, id uuid.UUID) (domain.Invoice, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if id != l.invoice.ID {
		return domain.Invoice{}, domain.ErrNotFound
	}
	return l.invoice, nil
}

func (l *fakeLedger) Transaction(_ context.Context, _ domain.PaymentProvider, txnID string) (domain.PaymentTransaction, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	t, ok := l.txns[txnID]
	if !ok {
		return domain.PaymentTransaction{}, domain.ErrNotFound
	}
	return *t, nil
}

func (l *fakeLedger) LiveTransaction(_ context.Context, _ uuid.UUID) (domain.PaymentTransaction, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, t := range l.txns {
		if t.State != domain.TxnCancelled {
			return *t, nil
		}
	}
	return domain.PaymentTransaction{}, billing.ErrNoLiveTransaction
}

func (l *fakeLedger) Begin(_ context.Context, in billing.BeginTransaction) (domain.PaymentTransaction, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.failNext != nil {
		err := l.failNext
		l.failNext = nil
		return domain.PaymentTransaction{}, err
	}
	// Stands in for payment_transactions_invoice_live_uniq.
	for _, t := range l.txns {
		if t.InvoiceID == in.Invoice.ID && t.State != domain.TxnCancelled {
			return domain.PaymentTransaction{}, domain.Errorf(domain.ErrConflict,
				"a payment is already in progress for this invoice")
		}
	}
	txn := &domain.PaymentTransaction{
		ID: uuid.New(), InvoiceID: in.Invoice.ID, TenantID: in.Invoice.TenantID,
		Provider: in.Provider, ProviderTxnID: in.ProviderTxn, State: domain.TxnCreated,
		AmountMinor: in.AmountMinor, CreatedMs: in.CreatedMs,
	}
	l.txns[in.ProviderTxn] = txn
	return *txn, nil
}

func (l *fakeLedger) Perform(_ context.Context, txn domain.PaymentTransaction, performedMs int64) (domain.PaymentTransaction, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	stored, ok := l.txns[txn.ProviderTxnID]
	if !ok {
		return domain.PaymentTransaction{}, domain.ErrNotFound
	}
	// The compare-and-set the real ledger does in SQL.
	if stored.State != domain.TxnCreated {
		return *stored, nil
	}
	stored.State = domain.TxnPerformed
	stored.PerformedMs = &performedMs
	l.invoice.Status = domain.InvoicePaid
	l.performs++
	return *stored, nil
}

func (l *fakeLedger) Cancel(_ context.Context, txn domain.PaymentTransaction, cancelledMs int64, reason int32) (domain.PaymentTransaction, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	stored, ok := l.txns[txn.ProviderTxnID]
	if !ok {
		return domain.PaymentTransaction{}, domain.ErrNotFound
	}
	if stored.State == domain.TxnCancelled {
		return *stored, nil
	}
	stored.State = domain.TxnCancelled
	stored.CancelledMs = &cancelledMs
	if reason != 0 {
		r := reason
		stored.CancelReason = &r
	}
	l.cancels++
	return *stored, nil
}

func (l *fakeLedger) Statement(_ context.Context, _ domain.PaymentProvider, fromMs, toMs int64) ([]domain.PaymentTransaction, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []domain.PaymentTransaction
	for _, t := range l.txns {
		if t.CreatedMs >= fromMs && t.CreatedMs <= toMs {
			out = append(out, *t)
		}
	}
	return out, nil
}

// --------------------------------------------------------------------- rig

type rig struct {
	t      *testing.T
	server *httptest.Server
	ledger *fakeLedger
}

func newRig(t *testing.T) *rig {
	t.Helper()
	ledger := newLedger()
	p := payme.New(payme.Config{MerchantID: "test-merchant", Key: testKey},
		ledger, slog.New(slog.DiscardHandler))

	server := httptest.NewServer(p.Webhook())
	t.Cleanup(server.Close)
	return &rig{t: t, server: server, ledger: ledger}
}

type rpcResponse struct {
	Result map[string]any `json:"result"`
	Error  *struct {
		Code    int               `json:"code"`
		Message map[string]string `json:"message"`
	} `json:"error"`
}

// call sends an authenticated JSON-RPC request.
func (r *rig) call(method string, params map[string]any) rpcResponse {
	r.t.Helper()
	return r.callAs(payme.BasicUser, testKey, method, params)
}

func (r *rig) callAs(user, key, method string, params map[string]any) rpcResponse {
	r.t.Helper()

	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": method, "params": params,
	})
	if err != nil {
		r.t.Fatalf("marshal request: %v", err)
	}

	req, err := http.NewRequest(http.MethodPost, r.server.URL, bytes.NewReader(body))
	if err != nil {
		r.t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(user, key)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatalf("call %s: %v", method, err)
	}
	defer resp.Body.Close()

	// Always 200: JSON-RPC carries its own error channel, and Payme reads the
	// body rather than the status.
	if resp.StatusCode != http.StatusOK {
		r.t.Fatalf("%s status = %d, want 200 — a non-200 is read as a transport failure and retried forever",
			method, resp.StatusCode)
	}

	var out rpcResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		r.t.Fatalf("decode %s response: %v", method, err)
	}
	return out
}

func (r *rig) account() map[string]string {
	return map[string]string{payme.AccountField: r.ledger.invoice.ID.String()}
}

// -------------------------------------------------------------------- tests

func TestUnauthenticatedCallIsRefused(t *testing.T) {
	r := newRig(t)

	got := r.callAs(payme.BasicUser, "wrong-key", "CheckPerformTransaction", map[string]any{
		"amount": testTiyin, "account": r.account(),
	})
	if got.Error == nil {
		t.Fatal("a wrong key was accepted")
	}
	if got.Error.Code != -32504 {
		t.Errorf("error code = %d, want -32504 (insufficient privileges)", got.Error.Code)
	}

	// The username is checked too. Accepting any username would mean the whole
	// credential is one guessable half.
	got = r.callAs("someone-else", testKey, "CheckTransaction", map[string]any{"id": "x"})
	if got.Error == nil || got.Error.Code != -32504 {
		t.Errorf("a wrong username was accepted: %+v", got.Error)
	}
}

func TestAmountIsComparedInTiyin(t *testing.T) {
	r := newRig(t)

	// The correct amount, converted.
	got := r.call("CheckPerformTransaction", map[string]any{
		"amount": testTiyin, "account": r.account(),
	})
	if got.Error != nil {
		t.Fatalf("the correct amount was refused: %+v", got.Error)
	}
	if allow, _ := got.Result["allow"].(bool); !allow {
		t.Errorf("result = %v, want allow: true", got.Result)
	}

	// The stored amount sent as-is, without the conversion. This is the
	// hundredfold undercharge the TiyinPerSom constant exists to prevent, and
	// it must be refused rather than accepted as "close enough".
	got = r.call("CheckPerformTransaction", map[string]any{
		"amount": testAmount, "account": r.account(),
	})
	if got.Error == nil {
		t.Fatal("an amount 100x too small was accepted")
	}
	if got.Error.Code != -31001 {
		t.Errorf("error code = %d, want -31001 (incorrect amount)", got.Error.Code)
	}
}

func TestUnknownInvoiceIsAnAccountError(t *testing.T) {
	r := newRig(t)

	got := r.call("CheckPerformTransaction", map[string]any{
		"amount":  testTiyin,
		"account": map[string]string{payme.AccountField: uuid.New().String()},
	})
	if got.Error == nil {
		t.Fatal("an unknown invoice was accepted")
	}
	// The -31050 range is where a merchant's own account errors belong, and it
	// is what makes Payme show "no such order" rather than a generic failure.
	if got.Error.Code > -31050 {
		t.Errorf("error code = %d, want an account error at -31050 or below", got.Error.Code)
	}
	if got.Error.Message["ru"] == "" || got.Error.Message["uz"] == "" {
		t.Errorf("message = %v, want a translation for each language the payer might read",
			got.Error.Message)
	}
}

func TestPaidInvoiceCannotBePaidAgain(t *testing.T) {
	r := newRig(t)
	r.ledger.invoice.Status = domain.InvoicePaid

	got := r.call("CheckPerformTransaction", map[string]any{
		"amount": testTiyin, "account": r.account(),
	})
	if got.Error == nil {
		t.Fatal("an already-paid invoice was offered for payment again")
	}
}

// The retry path, and the common one: Payme repeats CreateTransaction whenever
// our reply did not arrive. The second call must return the same transaction,
// not create a second one.
func TestCreateTransactionIsIdempotent(t *testing.T) {
	r := newRig(t)
	params := map[string]any{
		"id": "payme-txn-1", "time": time.Now().UnixMilli(),
		"amount": testTiyin, "account": r.account(),
	}

	first := r.call("CreateTransaction", params)
	if first.Error != nil {
		t.Fatalf("create failed: %+v", first.Error)
	}
	second := r.call("CreateTransaction", params)
	if second.Error != nil {
		t.Fatalf("the retry failed: %+v", second.Error)
	}

	if first.Result["transaction"] != second.Result["transaction"] {
		t.Errorf("retry produced a different transaction: %v then %v",
			first.Result["transaction"], second.Result["transaction"])
	}
	if len(r.ledger.txns) != 1 {
		t.Errorf("ledger holds %d transactions, want 1", len(r.ledger.txns))
	}
	if got := first.Result["state"]; got != float64(1) {
		t.Errorf("state = %v, want 1 (created)", got)
	}
}

// A second, differently-identified transaction against an invoice already being
// paid is a double charge. The unique index refuses it; this checks the refusal
// reaches Payme as something it can act on.
func TestSecondTransactionForOneInvoiceIsRefused(t *testing.T) {
	r := newRig(t)
	now := time.Now().UnixMilli()

	if got := r.call("CreateTransaction", map[string]any{
		"id": "payme-txn-1", "time": now, "amount": testTiyin, "account": r.account(),
	}); got.Error != nil {
		t.Fatalf("first create failed: %+v", got.Error)
	}

	got := r.call("CreateTransaction", map[string]any{
		"id": "payme-txn-2", "time": now, "amount": testTiyin, "account": r.account(),
	})
	if got.Error == nil {
		t.Fatal("a second transaction was created for an invoice already being paid")
	}
	if got.Error.Code != -31008 {
		t.Errorf("error code = %d, want -31008 (unable to perform)", got.Error.Code)
	}
}

// The test this file exists for.
func TestPerformTwiceSettlesOnce(t *testing.T) {
	r := newRig(t)
	now := time.Now().UnixMilli()

	if got := r.call("CreateTransaction", map[string]any{
		"id": "payme-txn-1", "time": now, "amount": testTiyin, "account": r.account(),
	}); got.Error != nil {
		t.Fatalf("create failed: %+v", got.Error)
	}

	first := r.call("PerformTransaction", map[string]any{"id": "payme-txn-1"})
	if first.Error != nil {
		t.Fatalf("perform failed: %+v", first.Error)
	}
	second := r.call("PerformTransaction", map[string]any{"id": "payme-txn-1"})
	if second.Error != nil {
		t.Fatalf("the repeated perform failed: %+v", second.Error)
	}

	if r.ledger.performs != 1 {
		t.Errorf("the ledger settled %d times, want exactly 1 — a duplicated callback billed twice",
			r.ledger.performs)
	}
	if first.Result["perform_time"] != second.Result["perform_time"] {
		t.Errorf("perform_time changed on the retry: %v then %v",
			first.Result["perform_time"], second.Result["perform_time"])
	}
	if got := second.Result["state"]; got != float64(2) {
		t.Errorf("state = %v, want 2 (performed)", got)
	}
}

func TestPerformUnknownTransaction(t *testing.T) {
	r := newRig(t)

	got := r.call("PerformTransaction", map[string]any{"id": "never-created"})
	if got.Error == nil {
		t.Fatal("performing an unknown transaction succeeded")
	}
	if got.Error.Code != -31003 {
		t.Errorf("error code = %d, want -31003 (transaction not found)", got.Error.Code)
	}
}

// An abandoned checkout must not hold its invoice hostage forever.
func TestStaleTransactionIsExpiredRatherThanPerformed(t *testing.T) {
	r := newRig(t)
	stale := time.Now().Add(-domain.MaxTxnAge - time.Hour).UnixMilli()

	if got := r.call("CreateTransaction", map[string]any{
		"id": "old-txn", "time": stale, "amount": testTiyin, "account": r.account(),
	}); got.Error != nil {
		t.Fatalf("create failed: %+v", got.Error)
	}

	got := r.call("PerformTransaction", map[string]any{"id": "old-txn"})
	if got.Error == nil {
		t.Fatal("a transaction older than the timeout was performed")
	}
	if r.ledger.performs != 0 {
		t.Errorf("the ledger settled %d times for an expired transaction, want 0", r.ledger.performs)
	}
	if r.ledger.cancels != 1 {
		t.Errorf("the expired transaction was cancelled %d times, want 1 — otherwise the invoice stays blocked",
			r.ledger.cancels)
	}
}

// Payme distinguishes a cancellation before payment (-1) from a refund after
// it (-2), and we keep that distinction in whether perform_time was ever set.
func TestCancelStateDependsOnWhetherMoneyMoved(t *testing.T) {
	t.Run("before payment", func(t *testing.T) {
		r := newRig(t)
		now := time.Now().UnixMilli()
		r.call("CreateTransaction", map[string]any{
			"id": "txn", "time": now, "amount": testTiyin, "account": r.account(),
		})

		got := r.call("CancelTransaction", map[string]any{"id": "txn", "reason": 3})
		if got.Error != nil {
			t.Fatalf("cancel failed: %+v", got.Error)
		}
		if state := got.Result["state"]; state != float64(-1) {
			t.Errorf("state = %v, want -1 (cancelled before payment)", state)
		}
	})

	t.Run("after payment", func(t *testing.T) {
		r := newRig(t)
		now := time.Now().UnixMilli()
		r.call("CreateTransaction", map[string]any{
			"id": "txn", "time": now, "amount": testTiyin, "account": r.account(),
		})
		r.call("PerformTransaction", map[string]any{"id": "txn"})

		got := r.call("CancelTransaction", map[string]any{"id": "txn", "reason": 5})
		if got.Error != nil {
			t.Fatalf("cancel failed: %+v", got.Error)
		}
		if state := got.Result["state"]; state != float64(-2) {
			t.Errorf("state = %v, want -2 (cancelled after payment)", state)
		}
	})
}

func TestCancelIsIdempotent(t *testing.T) {
	r := newRig(t)
	now := time.Now().UnixMilli()
	r.call("CreateTransaction", map[string]any{
		"id": "txn", "time": now, "amount": testTiyin, "account": r.account(),
	})

	first := r.call("CancelTransaction", map[string]any{"id": "txn", "reason": 3})
	second := r.call("CancelTransaction", map[string]any{"id": "txn", "reason": 3})
	if first.Error != nil || second.Error != nil {
		t.Fatalf("cancel errored: %+v / %+v", first.Error, second.Error)
	}
	if r.ledger.cancels != 1 {
		t.Errorf("the ledger cancelled %d times, want 1", r.ledger.cancels)
	}
	if first.Result["cancel_time"] != second.Result["cancel_time"] {
		t.Errorf("cancel_time changed on the retry: %v then %v",
			first.Result["cancel_time"], second.Result["cancel_time"])
	}
}

func TestCheckTransactionReportsState(t *testing.T) {
	r := newRig(t)
	now := time.Now().UnixMilli()
	r.call("CreateTransaction", map[string]any{
		"id": "txn", "time": now, "amount": testTiyin, "account": r.account(),
	})

	got := r.call("CheckTransaction", map[string]any{"id": "txn"})
	if got.Error != nil {
		t.Fatalf("check failed: %+v", got.Error)
	}
	if got.Result["create_time"] != float64(now) {
		t.Errorf("create_time = %v, want the time Payme sent (%d)", got.Result["create_time"], now)
	}
	// Absent times are reported as 0, not null: the spec has no null here and a
	// client reading it as a number would fail on one.
	if got.Result["perform_time"] != float64(0) {
		t.Errorf("perform_time = %v, want 0 for an unperformed transaction", got.Result["perform_time"])
	}
}

func TestStatementReportsAmountsInTiyin(t *testing.T) {
	r := newRig(t)
	now := time.Now().UnixMilli()
	r.call("CreateTransaction", map[string]any{
		"id": "txn", "time": now, "amount": testTiyin, "account": r.account(),
	})

	got := r.call("GetStatement", map[string]any{"from": now - 1000, "to": now + 1000})
	if got.Error != nil {
		t.Fatalf("statement failed: %+v", got.Error)
	}

	rows, _ := got.Result["transactions"].([]any)
	if len(rows) != 1 {
		t.Fatalf("statement returned %d rows, want 1", len(rows))
	}
	row := rows[0].(map[string]any)
	if row["amount"] != float64(testTiyin) {
		t.Errorf("amount = %v, want %d tiyin — a statement in som would never reconcile",
			row["amount"], testTiyin)
	}
	if row["id"] != "txn" {
		t.Errorf("id = %v, want the provider's own id echoed back", row["id"])
	}
}

func TestUnknownMethod(t *testing.T) {
	r := newRig(t)

	got := r.call("DropAllTables", map[string]any{})
	if got.Error == nil {
		t.Fatal("an unknown method was accepted")
	}
	if got.Error.Code != -32601 {
		t.Errorf("error code = %d, want -32601 (method not found)", got.Error.Code)
	}
}

func TestCheckoutURLCarriesTheInvoiceAndAmount(t *testing.T) {
	ledger := newLedger()
	p := payme.New(payme.Config{MerchantID: "test-merchant", Key: testKey},
		ledger, slog.New(slog.DiscardHandler))

	got, err := p.Checkout(context.Background(), billing.CheckoutRequest{Invoice: ledger.invoice})
	if err != nil {
		t.Fatalf("checkout: %v", err)
	}
	if got.RedirectURL == "" {
		t.Fatal("checkout returned no redirect")
	}

	// The parameters are base64url-encoded rather than query-string, so decode
	// them back and check what the customer is actually being asked to pay.
	encoded := got.RedirectURL[len(payme.DefaultCheckoutURL)+1:]
	raw, err := base64Decode(encoded)
	if err != nil {
		t.Fatalf("decode checkout parameters: %v", err)
	}
	decoded := string(raw)

	for _, want := range []string{
		"m=test-merchant",
		payme.AccountField + "=" + ledger.invoice.ID.String(),
		"a=" + itoa(testTiyin),
	} {
		if !bytes.Contains([]byte(decoded), []byte(want)) {
			t.Errorf("checkout parameters %q are missing %q", decoded, want)
		}
	}
}

func base64Decode(s string) ([]byte, error) {
	return base64.URLEncoding.DecodeString(s)
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
