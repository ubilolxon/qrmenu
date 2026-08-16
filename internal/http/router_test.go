package http

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ubilolxon/qrmenu/internal/config"
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
	})
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
		{"unbuilt admin route", http.MethodGet, "/api/v1/admin/x", http.StatusNotImplemented, "NOT_IMPLEMENTED"},
		{"unbuilt staff route", http.MethodGet, "/api/v1/staff/x", http.StatusNotImplemented, "NOT_IMPLEMENTED"},
		{"unbuilt webhook", http.MethodGet, "/webhooks/stripe", http.StatusNotImplemented, "NOT_IMPLEMENTED"},
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
