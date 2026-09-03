// Package payme implements the Payme Merchant API.
//
// Payme does not push events at a webhook. It calls one endpoint with JSON-RPC
// and asks questions about a transaction it names by id — CreateTransaction,
// PerformTransaction, CancelTransaction, CheckTransaction — and expects the
// same answer every time it asks. That makes idempotency structural rather
// than something to remember: every method here is a lookup by the provider's
// own id, and re-running one returns the state that already exists.
package payme

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ubilolxon/qrmenu/internal/billing"
	"github.com/ubilolxon/qrmenu/internal/domain"
)

// Config is what a merchant account gives you.
type Config struct {
	// MerchantID appears in the checkout URL.
	MerchantID string
	// Key authenticates Payme's calls to us, as the password half of HTTP Basic
	// with the fixed username below. It is not the same secret as the one used
	// for the checkout link.
	Key string
	// CheckoutURL is the hosted payment page. Overridable so the test
	// environment can be pointed somewhere else.
	CheckoutURL string
}

// DefaultCheckoutURL is Payme's hosted checkout.
const DefaultCheckoutURL = "https://checkout.paycom.uz"

// BasicUser is the fixed username Payme authenticates with; only the password
// varies per merchant.
const BasicUser = "Paycom"

// AccountField is the key inside the JSON-RPC "account" object that carries our
// invoice id. It is configured in the merchant cabinet and must match exactly.
const AccountField = "invoice_id"

// TiyinPerSom converts our amounts to Payme's.
//
// This is the one place the zero-decimal treatment of UZS has to be undone.
// internal/money treats price_minor as whole som because tiyin are defunct in
// every shop in the country — but Payme's API is denominated in tiyin
// regardless, so an amount crossing this boundary is multiplied by 100. Getting
// this wrong is a hundredfold over- or undercharge, which is why it is a named
// constant with this comment rather than a literal.
const TiyinPerSom = 100

type Provider struct {
	cfg    Config
	ledger billing.Ledger
	logger *slog.Logger
	now    func() time.Time
}

func New(cfg Config, ledger billing.Ledger, logger *slog.Logger) *Provider {
	if cfg.CheckoutURL == "" {
		cfg.CheckoutURL = DefaultCheckoutURL
	}
	return &Provider{cfg: cfg, ledger: ledger, logger: logger, now: time.Now}
}

func (p *Provider) Name() domain.PaymentProvider { return domain.ProviderPayme }

// Checkout builds the hosted-page URL.
//
// Payme takes its parameters as one base64url-encoded, semicolon-separated
// string rather than as query parameters. Nothing here is secret — it is the
// merchant id, the amount and our invoice id — so the encoding is a transport
// detail, not a signature, and it authorises nothing on its own.
func (p *Provider) Checkout(_ context.Context, req billing.CheckoutRequest) (billing.CheckoutResult, error) {
	if p.cfg.MerchantID == "" {
		return billing.CheckoutResult{}, fmt.Errorf("payme: merchant id is not configured")
	}
	return billing.CheckoutResult{RedirectURL: p.buildCheckoutURL(req.Invoice, req.ReturnTo)}, nil
}

func (p *Provider) buildCheckoutURL(inv domain.Invoice, returnTo string) string {
	params := []string{
		"m=" + p.cfg.MerchantID,
		fmt.Sprintf("ac.%s=%s", AccountField, inv.ID),
		fmt.Sprintf("a=%d", inv.AmountMinor*TiyinPerSom),
		"l=uz",
	}
	if returnTo != "" {
		params = append(params, "c="+returnTo)
	}
	encoded := base64.URLEncoding.EncodeToString([]byte(strings.Join(params, ";")))
	return strings.TrimRight(p.cfg.CheckoutURL, "/") + "/" + encoded
}

// ---------------------------------------------------------------- JSON-RPC

type request struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

type response struct {
	// JSONRPC is echoed because Payme's own examples carry it. The id is
	// echoed verbatim as raw JSON: the spec allows a number or a string, and
	// re-encoding it through a Go type is a way to hand back something subtly
	// different from what was sent.
	JSONRPC string          `json:"jsonrpc,omitempty"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// rpcError is Payme's error shape. The message is a translation object rather
// than a string, and it is shown to the payer in their own language on Payme's
// page — so these are written for a customer, not for a log.
type rpcError struct {
	Code    int               `json:"code"`
	Message map[string]string `json:"message"`
	Data    string            `json:"data,omitempty"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("payme error %d: %s", e.Code, e.Message["en"]) }

// Error codes from the Merchant API. The -318xx range is transport-level, the
// -313xx range is transaction-level, and -31050 downward is reserved for the
// merchant's own account errors — which is where "no such invoice" belongs.
const (
	codeParse            = -32700
	codeInvalidRequest   = -32600
	codeMethodNotFound   = -32601
	codeUnauthorized     = -32504
	codeWrongAmount      = -31001
	codeTxnNotFound      = -31003
	codeCannotPerform    = -31008
	codeCannotCancel     = -31007
	codeInvoiceNotFound  = -31050
	codeInvoiceNotPayabl = -31051
)

func errorf(code int, uz, ru, en string) *rpcError {
	return &rpcError{Code: code, Message: map[string]string{"uz": uz, "ru": ru, "en": en}}
}

var (
	errNoInvoice = errorf(codeInvoiceNotFound,
		"Hisob topilmadi", "Счёт не найден", "Invoice not found")
	errNotPayable = errorf(codeInvoiceNotPayabl,
		"Hisob to'langan yoki bekor qilingan", "Счёт уже оплачен или отменён", "Invoice is not open for payment")
	errWrongAmount = errorf(codeWrongAmount,
		"Noto'g'ri summa", "Неверная сумма", "Incorrect amount")
	errTxnNotFound = errorf(codeTxnNotFound,
		"Tranzaksiya topilmadi", "Транзакция не найдена", "Transaction not found")
	errCannotPerform = errorf(codeCannotPerform,
		"Amalni bajarib bo'lmadi", "Невозможно выполнить операцию", "Unable to perform the operation")
	errCannotCancel = errorf(codeCannotCancel,
		"Bekor qilib bo'lmaydi", "Невозможно отменить", "Unable to cancel the transaction")
)

// Payme's transaction states, which are its own vocabulary and not ours.
const (
	stateCreated            = 1
	statePerformed          = 2
	stateCancelled          = -1
	stateCancelledAfterPay  = -2
	reasonTransactionExpiry = 4
)

// Webhook is the merchant endpoint Payme calls.
func (p *Provider) Webhook() http.Handler { return http.HandlerFunc(p.serve) }

func (p *Provider) serve(w http.ResponseWriter, r *http.Request) {
	// Authenticate before parsing. An unauthenticated caller must not be able
	// to learn anything from how we react to their payload — including whether
	// it was well-formed.
	if !p.authorised(r) {
		write(w, response{}, errorf(codeUnauthorized,
			"Ruxsat yo'q", "Недостаточно привилегий", "Insufficient privileges"))
		return
	}

	var req request
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(&req); err != nil {
		write(w, response{}, errorf(codeParse,
			"So'rovni o'qib bo'lmadi", "Не удалось разобрать запрос", "Could not parse the request"))
		return
	}

	result, rpcErr := p.dispatch(r, req)
	write(w, response{JSONRPC: "2.0", ID: req.ID, Result: result}, rpcErr)
}

const maxBody = 1 << 20

func (p *Provider) dispatch(r *http.Request, req request) (any, *rpcError) {
	ctx := r.Context()

	switch req.Method {
	case "CheckPerformTransaction":
		return p.checkPerform(ctx, req.Params)
	case "CreateTransaction":
		return p.create(ctx, req.Params)
	case "PerformTransaction":
		return p.perform(ctx, req.Params)
	case "CancelTransaction":
		return p.cancel(ctx, req.Params)
	case "CheckTransaction":
		return p.check(ctx, req.Params)
	case "GetStatement":
		return p.statement(ctx, req.Params)
	default:
		return nil, errorf(codeMethodNotFound,
			"Metod topilmadi", "Метод не найден", "Method not found")
	}
}

// authorised checks the HTTP Basic credentials in constant time.
func (p *Provider) authorised(r *http.Request) bool {
	user, pass, ok := r.BasicAuth()
	if !ok || p.cfg.Key == "" {
		return false
	}
	// Both halves are compared, and both in constant time: comparing only the
	// key would let any username through, and comparing with == would leak the
	// key one byte at a time to a caller who can measure.
	userOK := subtle.ConstantTimeCompare([]byte(user), []byte(BasicUser)) == 1
	keyOK := subtle.ConstantTimeCompare([]byte(pass), []byte(p.cfg.Key)) == 1
	return userOK && keyOK
}

func write(w http.ResponseWriter, res response, rpcErr *rpcError) {
	if rpcErr != nil {
		res.Error = rpcErr
		res.Result = nil
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// Always 200. JSON-RPC carries its own error channel, and Payme reads the
	// body rather than the status — a 4xx here is reported as a transport
	// failure and retried forever instead of being understood.
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(res)
}

// ----------------------------------------------------------------- methods

type accountParams struct {
	Amount  int64             `json:"amount"`
	Account map[string]string `json:"account"`
}

type txnParams struct {
	ID      string            `json:"id"`
	Time    int64             `json:"time"`
	Amount  int64             `json:"amount"`
	Account map[string]string `json:"account"`
	Reason  *int32            `json:"reason"`
}

type rangeParams struct {
	From int64 `json:"from"`
	To   int64 `json:"to"`
}

// invoiceFor resolves and validates the invoice a request names. Every method
// that touches money starts here, so the three ways a payment can be wrong —
// no such invoice, already settled, wrong amount — are answered identically
// wherever they are asked.
func (p *Provider) invoiceFor(ctx context.Context, account map[string]string, amount int64) (domain.Invoice, *rpcError) {
	raw := strings.TrimSpace(account[AccountField])
	id, err := uuid.Parse(raw)
	if err != nil {
		return domain.Invoice{}, errNoInvoice
	}
	inv, err := p.ledger.Invoice(ctx, id)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return domain.Invoice{}, errNoInvoice
		}
		p.logger.Error("payme: look up invoice", "error", err, "invoice_id", id)
		return domain.Invoice{}, errCannotPerform
	}
	if !inv.Status.Payable() {
		return domain.Invoice{}, errNotPayable
	}
	if amount != inv.AmountMinor*TiyinPerSom {
		return domain.Invoice{}, errWrongAmount
	}
	return inv, nil
}

func (p *Provider) checkPerform(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var in accountParams
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, errorf(codeInvalidRequest, "Noto'g'ri so'rov", "Неверный запрос", "Invalid request")
	}
	if _, rpcErr := p.invoiceFor(ctx, in.Account, in.Amount); rpcErr != nil {
		return nil, rpcErr
	}
	return map[string]any{"allow": true}, nil
}

func (p *Provider) create(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var in txnParams
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, errorf(codeInvalidRequest, "Noto'g'ri so'rov", "Неверный запрос", "Invalid request")
	}

	// A repeat of a transaction we already know is answered from the record,
	// not created again. This is the retry path, and it is the common one:
	// Payme repeats CreateTransaction whenever our reply did not arrive.
	if existing, err := p.ledger.Transaction(ctx, domain.ProviderPayme, in.ID); err == nil {
		if existing.State != domain.TxnCreated {
			return nil, errCannotPerform
		}
		// An abandoned checkout is cancelled rather than resumed, so the
		// invoice it is holding is freed for another attempt.
		if existing.Timeout(p.now()) {
			if _, err := p.ledger.Cancel(ctx, existing, p.nowMs(), reasonTransactionExpiry); err != nil {
				p.logger.Error("payme: expire transaction", "error", err, "txn", in.ID)
			}
			return nil, errCannotPerform
		}
		return createResult(existing), nil
	} else if !errors.Is(err, domain.ErrNotFound) {
		p.logger.Error("payme: look up transaction", "error", err, "txn", in.ID)
		return nil, errCannotPerform
	}

	inv, rpcErr := p.invoiceFor(ctx, in.Account, in.Amount)
	if rpcErr != nil {
		return nil, rpcErr
	}

	created, err := p.ledger.Begin(ctx, billing.BeginTransaction{
		Invoice:     inv,
		Provider:    domain.ProviderPayme,
		ProviderTxn: in.ID,
		AmountMinor: inv.AmountMinor,
		CreatedMs:   in.Time,
	})
	if err != nil {
		// The usual cause is a second transaction against an invoice already
		// being paid, which the unique index refuses. Reporting it as
		// "unable to perform" is exactly right: there is nothing Payme can do
		// but wait for the first one to settle or expire.
		if errors.Is(err, domain.ErrConflict) {
			return nil, errCannotPerform
		}
		p.logger.Error("payme: begin transaction", "error", err, "invoice_id", inv.ID)
		return nil, errCannotPerform
	}
	return createResult(created), nil
}

func (p *Provider) perform(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var in txnParams
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, errorf(codeInvalidRequest, "Noto'g'ri so'rov", "Неверный запрос", "Invalid request")
	}

	txn, rpcErr := p.lookup(ctx, in.ID)
	if rpcErr != nil {
		return nil, rpcErr
	}

	switch txn.State {
	case domain.TxnPerformed:
		// Already settled. Answering from the record rather than settling again
		// is what stops a retried callback from renewing the subscription
		// twice, which is the failure this whole design exists to prevent.
		return performResult(txn), nil
	case domain.TxnCancelled:
		return nil, errCannotPerform
	}

	if txn.Timeout(p.now()) {
		if _, err := p.ledger.Cancel(ctx, txn, p.nowMs(), reasonTransactionExpiry); err != nil {
			p.logger.Error("payme: expire transaction", "error", err, "txn", in.ID)
		}
		return nil, errCannotPerform
	}

	performed, err := p.ledger.Perform(ctx, txn, p.nowMs())
	if err != nil {
		p.logger.Error("payme: perform transaction", "error", err, "txn", in.ID)
		return nil, errCannotPerform
	}
	return performResult(performed), nil
}

func (p *Provider) cancel(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var in txnParams
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, errorf(codeInvalidRequest, "Noto'g'ri so'rov", "Неверный запрос", "Invalid request")
	}

	txn, rpcErr := p.lookup(ctx, in.ID)
	if rpcErr != nil {
		return nil, rpcErr
	}
	if txn.State == domain.TxnCancelled {
		return cancelResult(txn), nil
	}

	var reason int32
	if in.Reason != nil {
		reason = *in.Reason
	}
	cancelled, err := p.ledger.Cancel(ctx, txn, p.nowMs(), reason)
	if err != nil {
		p.logger.Error("payme: cancel transaction", "error", err, "txn", in.ID)
		return nil, errCannotCancel
	}
	return cancelResult(cancelled), nil
}

func (p *Provider) check(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var in txnParams
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, errorf(codeInvalidRequest, "Noto'g'ri so'rov", "Неверный запрос", "Invalid request")
	}
	txn, rpcErr := p.lookup(ctx, in.ID)
	if rpcErr != nil {
		return nil, rpcErr
	}
	return map[string]any{
		"create_time":  txn.CreatedMs,
		"perform_time": zeroIfNil(txn.PerformedMs),
		"cancel_time":  zeroIfNil(txn.CancelledMs),
		"transaction":  txn.ID.String(),
		"state":        state(txn),
		"reason":       txn.CancelReason,
	}, nil
}

func (p *Provider) statement(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var in rangeParams
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, errorf(codeInvalidRequest, "Noto'g'ri so'rov", "Неверный запрос", "Invalid request")
	}
	rows, err := p.ledger.Statement(ctx, domain.ProviderPayme, in.From, in.To)
	if err != nil {
		p.logger.Error("payme: statement", "error", err)
		return nil, errCannotPerform
	}

	out := make([]map[string]any, 0, len(rows))
	for _, t := range rows {
		out = append(out, map[string]any{
			"id":           t.ProviderTxnID,
			"time":         t.CreatedMs,
			"amount":       t.AmountMinor * TiyinPerSom,
			"account":      map[string]string{AccountField: t.InvoiceID.String()},
			"create_time":  t.CreatedMs,
			"perform_time": zeroIfNil(t.PerformedMs),
			"cancel_time":  zeroIfNil(t.CancelledMs),
			"transaction":  t.ID.String(),
			"state":        state(t),
			"reason":       t.CancelReason,
		})
	}
	return map[string]any{"transactions": out}, nil
}

func (p *Provider) lookup(ctx context.Context, txnID string) (domain.PaymentTransaction, *rpcError) {
	txn, err := p.ledger.Transaction(ctx, domain.ProviderPayme, txnID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return domain.PaymentTransaction{}, errTxnNotFound
		}
		p.logger.Error("payme: look up transaction", "error", err, "txn", txnID)
		return domain.PaymentTransaction{}, errCannotPerform
	}
	return txn, nil
}

// ----------------------------------------------------------------- results

func createResult(t domain.PaymentTransaction) map[string]any {
	return map[string]any{
		"create_time": t.CreatedMs,
		"transaction": t.ID.String(),
		"state":       stateCreated,
	}
}

func performResult(t domain.PaymentTransaction) map[string]any {
	return map[string]any{
		"perform_time": zeroIfNil(t.PerformedMs),
		"transaction":  t.ID.String(),
		"state":        statePerformed,
	}
}

func cancelResult(t domain.PaymentTransaction) map[string]any {
	return map[string]any{
		"cancel_time": zeroIfNil(t.CancelledMs),
		"transaction": t.ID.String(),
		"state":       state(t),
	}
}

// state translates our three-state vocabulary into Payme's, which distinguishes
// a cancellation before payment from a refund after it — a distinction we keep
// in whether performed_ms was ever set.
func state(t domain.PaymentTransaction) int {
	switch t.State {
	case domain.TxnPerformed:
		return statePerformed
	case domain.TxnCancelled:
		if t.PerformedMs != nil {
			return stateCancelledAfterPay
		}
		return stateCancelled
	default:
		return stateCreated
	}
}

func zeroIfNil(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}

func (p *Provider) nowMs() int64 { return p.now().UnixMilli() }
