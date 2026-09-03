// Package metrics exposes the service's Prometheus instrumentation.
//
// Two rules shape everything here.
//
// First, **labels are bounded**. A histogram labelled with a raw URL path grows
// one time series per order id, and the first busy night turns the scrape into
// the largest thing in the cluster. Every label below is a route *pattern*, a
// method, or a status class — values from a fixed set.
//
// Second, **the expensive numbers are pulled, not pushed**. Pool saturation,
// open SSE connections and cache hit ratio are all already counted somewhere;
// re-reporting them through a metrics call at every use would couple those
// packages to Prometheus for no gain. Instead a collector reads them when a
// scrape arrives, which is also the only moment anyone wants them.
package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Namespace prefixes every series so they group in a shared Prometheus.
const Namespace = "qrmenu"

type Metrics struct {
	registry *prometheus.Registry

	requestDuration  *prometheus.HistogramVec
	requestsInFlight prometheus.Gauge
	rateLimited      *prometheus.CounterVec
}

func New() *Metrics {
	reg := prometheus.NewRegistry()

	m := &Metrics{
		registry: reg,
		requestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: Namespace,
			Name:      "http_request_duration_seconds",
			Help:      "Request latency by route pattern, method and status class.",
			// Tuned for this service rather than taken from the default: a
			// cached menu answers in single-digit milliseconds and the useful
			// question is "did the cache miss", so the low buckets are dense.
			// The top bucket is 10s because anything slower is already a
			// timeout somewhere else.
			Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
		}, []string{"route", "method", "status"}),

		requestsInFlight: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: Namespace,
			Name:      "http_requests_in_flight",
			Help:      "Requests currently being served, SSE streams included.",
		}),

		rateLimited: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: Namespace,
			Name:      "rate_limited_total",
			Help:      "Requests refused by a rate limit, by route pattern.",
		}, []string{"route"}),
	}

	reg.MustRegister(
		m.requestDuration,
		m.requestsInFlight,
		m.rateLimited,
		// Explicit rather than inherited from the default registry: this
		// process should export exactly what it means to.
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return m
}

// Register adds a pull-based collector — see the package comment.
func (m *Metrics) Register(c prometheus.Collector) error { return m.registry.Register(c) }

// Handler serves the scrape endpoint.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{
		// A broken collector should show up as a failed scrape, not as a
		// silently incomplete one.
		ErrorHandling: promhttp.HTTPErrorOnError,
	})
}

// Middleware times every request.
//
// The route pattern is read **after** the handler returns, and that is not a
// stylistic choice: chi fills the pattern in as the request descends the router,
// so before ServeHTTP it is empty. Reading it too early would label every series
// with "" and quietly produce one useless bucket for the whole service.
func (m *Metrics) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		m.requestsInFlight.Inc()
		defer m.requestsInFlight.Dec()

		rec := &recorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)

		route := routePattern(r)
		status := rec.status
		if status == 0 {
			status = http.StatusOK
		}

		m.requestDuration.
			WithLabelValues(route, r.Method, strconv.Itoa(status)).
			Observe(time.Since(start).Seconds())

		if status == http.StatusTooManyRequests {
			m.rateLimited.WithLabelValues(route).Inc()
		}
	})
}

// routePattern returns the matched pattern, or "other" when nothing matched.
//
// "other" is what keeps the label bounded. An unrouted request carries an
// arbitrary path, and a scanner walking /wp-admin, /.env and ten thousand other
// guesses would otherwise mint a time series for each one — which is a denial of
// service against the monitoring rather than the service.
func routePattern(r *http.Request) string {
	rctx := chi.RouteContext(r.Context())
	if rctx == nil {
		return "other"
	}
	if pattern := rctx.RoutePattern(); pattern != "" {
		return pattern
	}
	return "other"
}

// recorder captures the status code, which http.ResponseWriter does not expose.
type recorder struct {
	http.ResponseWriter
	status int
}

func (rec *recorder) WriteHeader(code int) {
	if rec.status == 0 {
		rec.status = code
		rec.ResponseWriter.WriteHeader(code)
	}
}

func (rec *recorder) Write(b []byte) (int, error) {
	if rec.status == 0 {
		rec.status = http.StatusOK
	}
	return rec.ResponseWriter.Write(b)
}

// Unwrap keeps http.ResponseController working through this wrapper, which is
// what the SSE handler needs to flush and to clear its write deadline. Without
// it, adding metrics would silently break the staff stream — the same way the
// logging middleware once did.
func (rec *recorder) Unwrap() http.ResponseWriter { return rec.ResponseWriter }
