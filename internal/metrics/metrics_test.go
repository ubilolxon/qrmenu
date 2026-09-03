package metrics_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/ubilolxon/qrmenu/internal/metrics"
)

// The label cardinality guarantee. A histogram labelled with raw paths grows one
// time series per order id, and the first busy night makes the scrape the
// largest thing in the cluster — so this is the test that has to hold.
func TestRoutesAreLabelledByPatternNotPath(t *testing.T) {
	m := metrics.New()

	r := chi.NewRouter()
	r.Use(m.Middleware)
	r.Get("/api/v1/public/orders/{orderID}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Three different ids must produce one series, not three.
	for _, id := range []string{"aaaa", "bbbb", "cccc"} {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/public/orders/"+id, nil))
	}

	body := scrape(t, m)
	if !strings.Contains(body, `route="/api/v1/public/orders/{orderID}"`) {
		t.Errorf("no series labelled with the route pattern:\n%s", body)
	}
	for _, id := range []string{"aaaa", "bbbb", "cccc"} {
		if strings.Contains(body, "orders/"+id) {
			t.Errorf("a concrete order id leaked into a label — that is one time series per order")
		}
	}
}

// A scanner walking /wp-admin, /.env and ten thousand other guesses must not be
// able to mint a time series for each one.
func TestUnmatchedPathsCollapseToOther(t *testing.T) {
	m := metrics.New()

	r := chi.NewRouter()
	r.Use(m.Middleware)
	// A real route as well as the NotFound handler: chi short-circuits a router
	// with an empty routing tree and never runs the middleware chain at all, so
	// a router with only a NotFound handler would test nothing.
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	r.NotFound(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })

	for _, path := range []string{"/wp-admin", "/.env", "/phpmyadmin/index.php"} {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	}

	body := scrape(t, m)
	if !strings.Contains(body, `route="other"`) {
		t.Errorf("unmatched requests were not collapsed:\n%s", body)
	}
	for _, path := range []string{"wp-admin", ".env", "phpmyadmin"} {
		if strings.Contains(body, path) {
			t.Errorf("scanner path %q became a label — the monitoring is now the attack surface", path)
		}
	}
}

func TestStatusAndRateLimitsAreCounted(t *testing.T) {
	m := metrics.New()

	r := chi.NewRouter()
	r.Use(m.Middleware)
	r.Get("/limited", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	})

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/limited", nil))

	body := scrape(t, m)
	if !strings.Contains(body, `status="429"`) {
		t.Errorf("status label missing:\n%s", body)
	}
	if !strings.Contains(body, `qrmenu_rate_limited_total{route="/limited"}`) {
		t.Errorf("a 429 was not counted as rate limited:\n%s", body)
	}
}

// The recorder wraps the ResponseWriter, and a wrapper that does not implement
// Unwrap silently breaks flushing — which is how the logging middleware once
// broke every SSE connection. Adding metrics must not reintroduce that.
func TestRecorderKeepsResponseControllerWorking(t *testing.T) {
	m := metrics.New()

	var flushErr error
	r := chi.NewRouter()
	r.Use(m.Middleware)
	r.Get("/stream", func(w http.ResponseWriter, _ *http.Request) {
		rc := http.NewResponseController(w)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: hello\n\n"))
		flushErr = rc.Flush()
	})

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/stream", nil))

	if flushErr != nil {
		t.Fatalf("Flush through the metrics wrapper failed: %v — this is what severs the staff stream", flushErr)
	}
}

func scrape(t *testing.T, m *metrics.Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("scrape returned %d: %s", rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}
