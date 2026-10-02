package http

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ubilolxon/qrmenu/internal/auth"
	"github.com/ubilolxon/qrmenu/internal/config"
	"github.com/ubilolxon/qrmenu/internal/domain"
	"github.com/ubilolxon/qrmenu/internal/http/middleware"
)

// testRouter builds a router with no database. Every route exercised here is
// dependency-free; /readyz is covered separately against a real Postgres.
func testRouter(t *testing.T, origins ...string) http.Handler {
	t.Helper()
	if len(origins) == 0 {
		origins = []string{"http://localhost:5173"}
	}
	return NewRouter(Deps{
		Config:  config.Config{Env: "dev", HTTP: config.HTTP{AllowedOrigins: origins}},
		Logger:  slog.New(slog.DiscardHandler),
		Version: "test",
		Issuer:  testIssuer(),
	})
}

// The services are nil here on purpose: these tests cover routing, auth and
// role enforcement, all of which must reject a request *before* any handler
// runs. If one of them ever lets a request through, the nil service panics and
// the test fails loudly instead of quietly passing.
func testIssuer() *auth.Issuer {
	return auth.NewIssuer([]byte("test-secret-that-is-long-enough-32b"), time.Minute, "qrmenu-test")
}

func tokenFor(t *testing.T, role domain.Role) string {
	t.Helper()
	token, _, err := testIssuer().IssueAccess(uuid.New(), uuid.New(), role)
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}
	return token
}

func bearer(token string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + token}
}

func do(t *testing.T, h http.Handler, method, path string, headers map[string]string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Result()
}

// Liveness must not touch dependencies: a database outage is never fixed by
// restarting the process, and a failing /healthz would cause a restart loop.
func TestLivenessNeedsNoDependencies(t *testing.T) {
	resp := do(t, testRouter(t), http.MethodGet, "/healthz", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var body map[string]string
	decode(t, resp, &body)
	if body["status"] != "ok" {
		t.Errorf("status field = %q, want ok", body["status"])
	}
	if body["version"] != "test" {
		t.Errorf("version = %q, want test", body["version"])
	}
}

func TestRequestIDIsAlwaysReturned(t *testing.T) {
	resp := do(t, testRouter(t), http.MethodGet, "/healthz", nil)
	if resp.Header.Get("X-Request-ID") == "" {
		t.Error("X-Request-ID header missing")
	}
}

func TestRequestIDEchoesInboundValue(t *testing.T) {
	resp := do(t, testRouter(t), http.MethodGet, "/healthz",
		map[string]string{"X-Request-ID": "trace-me"})
	if got := resp.Header.Get("X-Request-ID"); got != "trace-me" {
		t.Errorf("X-Request-ID = %q, want trace-me", got)
	}
}

// Every error the API returns uses one envelope, so the SPA has a single shape
// to handle and can switch on a stable code.
func TestErrorEnvelopeShape(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		path       string
		wantStatus int
		wantCode   string
	}{
		{"unknown route", http.MethodGet, "/nope", http.StatusNotFound, "NOT_FOUND"},
		{"wrong method", http.MethodPost, "/healthz", http.StatusMethodNotAllowed, "BAD_REQUEST"},
		{"unbuilt public route", http.MethodGet, "/api/v1/public/x", http.StatusNotImplemented, "NOT_IMPLEMENTED"},
		// Admin and staff are built, so an unknown path under either is a
		// genuine 404 rather than a not-yet-implemented surface.
		{"unknown admin route", http.MethodGet, "/api/v1/admin/x", http.StatusNotFound, "NOT_FOUND"},
		{"unknown staff route", http.MethodGet, "/api/v1/staff/x", http.StatusNotFound, "NOT_FOUND"},
		// A provider this deployment has no credentials for is not mounted at
		// all, so it 404s rather than accepting callbacks it could not verify.
		{"unconfigured webhook provider", http.MethodGet, "/webhooks/stripe", http.StatusNotFound, "NOT_FOUND"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := do(t, testRouter(t), tc.method, tc.path, nil)
			if resp.StatusCode != tc.wantStatus {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}

			var body struct {
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			decode(t, resp, &body)

			if body.Error.Code != tc.wantCode {
				t.Errorf("error.code = %q, want %q", body.Error.Code, tc.wantCode)
			}
			if body.Error.Message == "" {
				t.Error("error.message is empty")
			}
		})
	}
}

func TestCORSAllowsConfiguredOrigin(t *testing.T) {
	resp := do(t, testRouter(t), http.MethodOptions, "/api/v1/admin/x",
		map[string]string{"Origin": "http://localhost:5173"})

	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("preflight status = %d, want 204", resp.StatusCode)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "http://localhost:5173" {
		t.Errorf("Allow-Origin = %q, want the request origin", got)
	}
	// Required for the refresh cookie to be sent by the browser.
	if got := resp.Header.Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Errorf("Allow-Credentials = %q, want true", got)
	}
	// Without Vary the response could be cached and served to another origin.
	if resp.Header.Get("Vary") != "Origin" {
		t.Errorf("Vary = %q, want Origin", resp.Header.Get("Vary"))
	}
}

// The critical case: an origin that is not configured must receive no CORS
// headers at all, so the browser blocks the response.
func TestCORSRejectsUnknownOrigin(t *testing.T) {
	resp := do(t, testRouter(t), http.MethodOptions, "/api/v1/admin/x",
		map[string]string{"Origin": "https://evil.example"})

	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Allow-Origin = %q, want empty for a disallowed origin", got)
	}
}

// A panic in any handler must become a clean 500 rather than a dropped
// connection, and must not leak the panic value to the client.
func TestPanicBecomesInternalError(t *testing.T) {
	panicking := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("kaboom")
	})
	// Same order the router applies.
	h := middleware.RequestID(
		middleware.Logger(slog.New(slog.DiscardHandler))(
			middleware.Recover(panicking)))

	resp := do(t, h, http.MethodGet, "/boom", nil)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}

	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	decode(t, resp, &body)

	if body.Error.Code != "INTERNAL" {
		t.Errorf("error.code = %q, want INTERNAL", body.Error.Code)
	}
	// The panic value must never reach the client.
	if body.Error.Message == "kaboom" {
		t.Error("panic value leaked to the client")
	}
}

// Every admin route except the auth entry points must reject an anonymous
// caller. This is the test that catches a route accidentally added outside the
// Authenticate group.
func TestAdminRoutesRequireAuthentication(t *testing.T) {
	routes := []struct{ method, path string }{
		{http.MethodGet, "/api/v1/admin/me"},
		{http.MethodGet, "/api/v1/admin/restaurants/"},
		{http.MethodPost, "/api/v1/admin/restaurants/"},
		{http.MethodGet, "/api/v1/admin/restaurants/" + uuid.NewString() + "/"},
		{http.MethodPatch, "/api/v1/admin/restaurants/" + uuid.NewString() + "/"},
		{http.MethodDelete, "/api/v1/admin/restaurants/" + uuid.NewString() + "/"},
	}

	for _, rt := range routes {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			resp := do(t, testRouter(t), rt.method, rt.path, nil)
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", resp.StatusCode)
			}
		})
	}
}

// The staff surface is where a missing auth check would be worst: the stream
// pushes every order in a restaurant, live, to whoever is holding the socket.
// The nil service in testRouter is what makes this assertion strong — anything
// that reaches a handler panics rather than quietly returning something.
func TestStaffRoutesRequireAuthentication(t *testing.T) {
	routes := []struct{ method, path string }{
		{http.MethodGet, "/api/v1/staff/stream?restaurant_id=" + uuid.NewString()},
		{http.MethodGet, "/api/v1/staff/orders?restaurant_id=" + uuid.NewString()},
		{http.MethodPatch, "/api/v1/staff/orders/" + uuid.NewString() + "/status"},
		{http.MethodGet, "/api/v1/staff/calls?restaurant_id=" + uuid.NewString()},
		{http.MethodPatch, "/api/v1/staff/calls/" + uuid.NewString() + "/status"},
	}

	for _, rt := range routes {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			resp := do(t, testRouter(t), rt.method, rt.path, nil)
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", resp.StatusCode)
			}
		})
	}
}

// Billing is where a role mistake costs money: a waiter who could reach
// /billing/invoices/{id}/pay could mark the account's bills settled. Every
// route here must reject an anonymous caller *and* every role but owner.
//
// The nil service in testRouter is what makes this strong — a request that gets
// past the middleware panics rather than quietly succeeding.
func TestBillingRoutesAreOwnerOnly(t *testing.T) {
	invoiceID := uuid.NewString()
	routes := []struct{ method, path string }{
		{http.MethodGet, "/api/v1/admin/billing/"},
		{http.MethodPost, "/api/v1/admin/billing/subscribe"},
		{http.MethodPost, "/api/v1/admin/billing/cancel"},
		{http.MethodGet, "/api/v1/admin/billing/invoices/"},
		{http.MethodPost, "/api/v1/admin/billing/invoices/" + invoiceID + "/pay"},
		{http.MethodPost, "/api/v1/admin/billing/invoices/" + invoiceID + "/void"},
	}

	for _, rt := range routes {
		t.Run("anonymous "+rt.method+" "+rt.path, func(t *testing.T) {
			resp := do(t, testRouter(t), rt.method, rt.path, nil)
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", resp.StatusCode)
			}
		})

		for _, role := range []domain.Role{domain.RoleManager, domain.RoleWaiter, domain.RoleKitchen} {
			t.Run(string(role)+" "+rt.method+" "+rt.path, func(t *testing.T) {
				resp := do(t, testRouter(t), rt.method, rt.path, bearer(tokenFor(t, role)))
				if resp.StatusCode != http.StatusForbidden {
					t.Errorf("status = %d, want 403 for role %s", resp.StatusCode, role)
				}
			})
		}
	}
}

// Staff management decides who can sign in and what they may do, so the role
// rules on the routes are load-bearing. The nil service in testRouter is what
// makes this strong — anything that gets past the middleware panics.
func TestUserRoutesEnforceRoles(t *testing.T) {
	userID := uuid.NewString()

	tests := []struct {
		method, path string
		// allowed lists every role that should reach the handler; everyone else
		// must get a 403.
		allowed []domain.Role
	}{
		{http.MethodGet, "/api/v1/admin/users/", []domain.Role{domain.RoleOwner, domain.RoleManager}},
		{http.MethodPost, "/api/v1/admin/users/", []domain.Role{domain.RoleOwner, domain.RoleManager}},
		{http.MethodGet, "/api/v1/admin/users/" + userID + "/", []domain.Role{domain.RoleOwner, domain.RoleManager}},
		{http.MethodPatch, "/api/v1/admin/users/" + userID + "/", []domain.Role{domain.RoleOwner, domain.RoleManager}},
		{http.MethodPut, "/api/v1/admin/users/" + userID + "/active", []domain.Role{domain.RoleOwner, domain.RoleManager}},
		// Deleting an account and taking one over are owner-only: they are not
		// changes to what someone may do, they are changes to who has access.
		{http.MethodDelete, "/api/v1/admin/users/" + userID + "/", []domain.Role{domain.RoleOwner}},
		{http.MethodPut, "/api/v1/admin/users/" + userID + "/password", []domain.Role{domain.RoleOwner}},
	}

	for _, tc := range tests {
		t.Run("anonymous "+tc.method+" "+tc.path, func(t *testing.T) {
			resp := do(t, testRouter(t), tc.method, tc.path, nil)
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", resp.StatusCode)
			}
		})

		for _, role := range []domain.Role{domain.RoleOwner, domain.RoleManager, domain.RoleWaiter, domain.RoleKitchen} {
			if slices.Contains(tc.allowed, role) {
				continue
			}
			t.Run(string(role)+" "+tc.method+" "+tc.path, func(t *testing.T) {
				resp := do(t, testRouter(t), tc.method, tc.path, bearer(tokenFor(t, role)))
				if resp.StatusCode != http.StatusForbidden {
					t.Errorf("status = %d, want 403 for role %s", resp.StatusCode, role)
				}
			})
		}
	}
}

// Changing your own password is not staff management: every role has one, so
// it must not sit behind the owner/manager group.
func TestEveryRoleCanChangeTheirOwnPassword(t *testing.T) {
	resp := do(t, testRouter(t), http.MethodPut, "/api/v1/admin/me/password", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("anonymous status = %d, want 401", resp.StatusCode)
	}

	// Authenticated as a kitchen account, the role middleware must not refuse
	// it. The nil service then panics, which the recoverer turns into a 500 —
	// so anything other than 403 proves the route is reachable.
	resp = do(t, testRouter(t), http.MethodPut, "/api/v1/admin/me/password",
		bearer(tokenFor(t, domain.RoleKitchen)))
	if resp.StatusCode == http.StatusForbidden {
		t.Error("a kitchen account was refused its own password change")
	}
}

func TestRejectsMalformedAndForgedTokens(t *testing.T) {
	forged := auth.NewIssuer([]byte("a-different-secret-of-sufficient-len"), time.Minute, "qrmenu-test")
	forgedToken, _, err := forged.IssueAccess(uuid.New(), uuid.New(), domain.RoleOwner)
	if err != nil {
		t.Fatalf("issue forged token: %v", err)
	}

	tests := map[string]map[string]string{
		"garbage token":         bearer("not-a-jwt"),
		"empty bearer":          {"Authorization": "Bearer "},
		"wrong scheme":          {"Authorization": "Basic " + tokenFor(t, domain.RoleOwner)},
		"signed with wrong key": bearer(forgedToken),
	}

	for name, headers := range tests {
		t.Run(name, func(t *testing.T) {
			resp := do(t, testRouter(t), http.MethodGet, "/api/v1/admin/me", headers)
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", resp.StatusCode)
			}
		})
	}
}

// An expired token gets its own code so the SPA refreshes instead of bouncing
// the user to the login screen.
func TestExpiredTokenIsDistinguishable(t *testing.T) {
	expired := auth.NewIssuer([]byte("test-secret-that-is-long-enough-32b"), -time.Minute, "qrmenu-test")
	token, _, err := expired.IssueAccess(uuid.New(), uuid.New(), domain.RoleOwner)
	if err != nil {
		t.Fatalf("issue expired token: %v", err)
	}

	resp := do(t, testRouter(t), http.MethodGet, "/api/v1/admin/me", bearer(token))
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}

	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	decode(t, resp, &body)
	if body.Error.Code != "TOKEN_EXPIRED" {
		t.Errorf("error.code = %q, want TOKEN_EXPIRED", body.Error.Code)
	}
}

// Roles are an explicit allow-list per route, so a valid token for the wrong
// job is a 403 rather than access.
func TestRoleEnforcement(t *testing.T) {
	tests := []struct {
		name   string
		role   domain.Role
		method string
		path   string
		want   int
	}{
		{"waiter cannot list restaurants", domain.RoleWaiter, http.MethodGet, "/api/v1/admin/restaurants/", http.StatusForbidden},
		{"kitchen cannot list restaurants", domain.RoleKitchen, http.MethodGet, "/api/v1/admin/restaurants/", http.StatusForbidden},
		{"manager cannot create a restaurant", domain.RoleManager, http.MethodPost, "/api/v1/admin/restaurants/", http.StatusForbidden},
		{"manager cannot delete a restaurant", domain.RoleManager, http.MethodDelete, "/api/v1/admin/restaurants/" + uuid.NewString() + "/", http.StatusForbidden},
		{"waiter cannot delete a restaurant", domain.RoleWaiter, http.MethodDelete, "/api/v1/admin/restaurants/" + uuid.NewString() + "/", http.StatusForbidden},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := do(t, testRouter(t), tc.method, tc.path, bearer(tokenFor(t, tc.role)))
			if resp.StatusCode != tc.want {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}
}

func decode(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q, want JSON", ct)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
}
