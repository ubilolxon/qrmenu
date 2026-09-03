package metrics

import (
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
)

// The collectors below are read at scrape time rather than updated at use time.
// Every number they report is already being counted by the package that owns it
// — the pool, the SSE hub, the menu cache — so pushing it through a metrics call
// on every request would add work to the hot path and a Prometheus import to
// three packages that are better off without one.

// PoolCollector reports connection-pool saturation.
//
// Saturation is the number worth alerting on, and it is not "connections in
// use": a pool at its maximum with nothing waiting is a pool doing its job. The
// signal is `acquire_wait_seconds_total` climbing — requests queueing for a
// connection — which shows up as latency everywhere at once and is otherwise
// very hard to attribute.
//
// One caveat on that number, because it is not what pgx's name suggests.
// AcquireDuration covers everything Acquire does, and this service installs a
// BeforeAcquire hook that sets the row-level-security scope — one round trip
// per acquire. So the counter is (queueing + one SET), and its *floor* under
// zero contention is roughly one round trip per acquire rather than zero. Alert
// on its rate of change against a baseline, not on an absolute value.
type PoolCollector struct {
	pool *pgxpool.Pool

	acquired      *prometheus.Desc
	idle          *prometheus.Desc
	total         *prometheus.Desc
	max           *prometheus.Desc
	acquireCount  *prometheus.Desc
	emptyAcquires *prometheus.Desc
	waitSeconds   *prometheus.Desc
}

func NewPoolCollector(pool *pgxpool.Pool) *PoolCollector {
	name := func(s, help string) *prometheus.Desc {
		return prometheus.NewDesc(Namespace+"_db_pool_"+s, help, nil, nil)
	}
	return &PoolCollector{
		pool:          pool,
		acquired:      name("acquired_connections", "Connections currently checked out."),
		idle:          name("idle_connections", "Connections open and unused."),
		total:         name("total_connections", "Connections open."),
		max:           name("max_connections", "Configured ceiling."),
		acquireCount:  name("acquires_total", "Successful acquires since start."),
		emptyAcquires: name("empty_acquires_total", "Acquires that had to wait for a free connection."),
		waitSeconds:   name("acquire_wait_seconds_total", "Time spent waiting for a connection. The saturation signal."),
	}
}

func (c *PoolCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.acquired
	ch <- c.idle
	ch <- c.total
	ch <- c.max
	ch <- c.acquireCount
	ch <- c.emptyAcquires
	ch <- c.waitSeconds
}

func (c *PoolCollector) Collect(ch chan<- prometheus.Metric) {
	if c.pool == nil {
		return
	}
	s := c.pool.Stat()
	gauge := func(d *prometheus.Desc, v float64) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, v)
	}
	counter := func(d *prometheus.Desc, v float64) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.CounterValue, v)
	}

	gauge(c.acquired, float64(s.AcquiredConns()))
	gauge(c.idle, float64(s.IdleConns()))
	gauge(c.total, float64(s.TotalConns()))
	gauge(c.max, float64(s.MaxConns()))
	counter(c.acquireCount, float64(s.AcquireCount()))
	counter(c.emptyAcquires, float64(s.EmptyAcquireCount()))
	counter(c.waitSeconds, s.AcquireDuration().Seconds())
}

// StreamSource is the SSE hub, named as an interface so this package does not
// import internal/realtime and internal/realtime does not import Prometheus.
type StreamSource interface {
	// Total is the number of open subscriptions across every restaurant.
	Total() int
	// Dropped counts events discarded because a subscriber's buffer was full.
	Dropped() uint64
}

// StreamCollector reports the staff stream.
//
// Dropped is the interesting one. It is not an error — dropping rather than
// blocking is deliberate, so one wedged tablet cannot become the speed limit for
// the whole room — but a dropped event means a kitchen screen was told to
// resync, and a rising rate means something is genuinely not keeping up.
type StreamCollector struct {
	source      StreamSource
	connections *prometheus.Desc
	dropped     *prometheus.Desc
}

func NewStreamCollector(source StreamSource) *StreamCollector {
	return &StreamCollector{
		source: source,
		connections: prometheus.NewDesc(Namespace+"_sse_connections",
			"Open staff stream subscriptions on this instance.", nil, nil),
		dropped: prometheus.NewDesc(Namespace+"_sse_dropped_events_total",
			"Events dropped because a subscriber was not reading fast enough.", nil, nil),
	}
}

func (c *StreamCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.connections
	ch <- c.dropped
}

func (c *StreamCollector) Collect(ch chan<- prometheus.Metric) {
	if c.source == nil {
		return
	}
	ch <- prometheus.MustNewConstMetric(c.connections, prometheus.GaugeValue, float64(c.source.Total()))
	ch <- prometheus.MustNewConstMetric(c.dropped, prometheus.CounterValue, float64(c.source.Dropped()))
}

// CacheSource is the public menu service.
type CacheSource interface {
	CacheHits() uint64
	CacheMisses() uint64
}

// CacheCollector reports the public menu cache.
//
// The hit ratio is the single best health signal this service has: the menu is
// the hot path, a hit is a Redis round trip and a miss is a dozen queries plus
// a render. A ratio that falls without traffic changing means menu_version is
// being bumped by something that should not be bumping it.
type CacheCollector struct {
	source CacheSource
	hits   *prometheus.Desc
	misses *prometheus.Desc
}

func NewCacheCollector(source CacheSource) *CacheCollector {
	return &CacheCollector{
		source: source,
		hits: prometheus.NewDesc(Namespace+"_menu_cache_hits_total",
			"Public menu reads served from cache.", nil, nil),
		misses: prometheus.NewDesc(Namespace+"_menu_cache_misses_total",
			"Public menu reads that had to be rebuilt from Postgres.", nil, nil),
	}
}

func (c *CacheCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.hits
	ch <- c.misses
}

func (c *CacheCollector) Collect(ch chan<- prometheus.Metric) {
	if c.source == nil {
		return
	}
	ch <- prometheus.MustNewConstMetric(c.hits, prometheus.CounterValue, float64(c.source.CacheHits()))
	ch <- prometheus.MustNewConstMetric(c.misses, prometheus.CounterValue, float64(c.source.CacheMisses()))
}
