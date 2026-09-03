// Command loadtest drives the public menu path and reports what it costs.
//
// The public menu is the only endpoint that has to survive a crowd: everything
// else is used by staff, and there are a handful of them per restaurant. So
// this measures that one path rather than pretending to be a general-purpose
// benchmark, and it reports the two numbers that actually decide whether the
// service holds up — the tail latency, and the cache hit ratio underneath it.
//
// A hit ratio below about 95% under steady load means something is bumping
// menu_version that should not be, and no amount of tuning elsewhere will
// matter until that is fixed.
//
//	go run ./cmd/loadtest -url http://localhost:8080/api/v1/public/t/TOKEN -c 50 -d 30s
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

func main() {
	var (
		target      = flag.String("url", "", "URL to hammer (required)")
		concurrency = flag.Int("c", 50, "concurrent clients")
		duration    = flag.Duration("d", 30*time.Second, "how long to run")
		metricsURL  = flag.String("metrics", "http://localhost:9091/metrics", "scrape endpoint, for the cache ratio; empty to skip")
		timeout     = flag.Duration("timeout", 10*time.Second, "per-request timeout")
		clients     = flag.Int("clients", 0, "present as N distinct client IPs via X-Forwarded-For (0 = one IP)")
		rps         = flag.Int("rps", 0, "target requests per second across all clients (0 = as fast as possible)")
	)
	flag.Parse()

	if *target == "" {
		flag.Usage()
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *duration)
	defer cancel()

	before := scrapeCache(*metricsURL)

	result := run(ctx, *target, *concurrency, *clients, *rps, *timeout)
	after := scrapeCache(*metricsURL)

	result.report(*concurrency, before, after)
}

type outcome struct {
	mu      sync.Mutex
	samples []time.Duration

	ok     atomic.Int64
	failed atomic.Int64
	// Counted separately from failures: a 429 means the limiter is doing its
	// job, and folding it into errors would make a working rate limit look like
	// a broken service.
	limited atomic.Int64
	bytes   atomic.Int64
	elapsed time.Duration
}

// spoofIP builds a distinct X-Forwarded-For for worker n.
//
// Needed because the public menu is limited per IP, and a load test from one
// address measures the rate limiter rather than the menu — it answers 429 after
// sixty requests and reports a very fast service that served almost nothing.
//
// This works only because the service trusts X-Forwarded-For to identify a
// client, which is correct behind a proxy that sets it and is exactly why the
// service must never be exposed without one: the header is a claim, and any
// caller can make it. Documented in ClientIP.
//
// The addresses come from 198.18.0.0/15, the range RFC 2544 reserves for
// exactly this — network benchmarking — and which can never be a real client.
// A documentation range such as TEST-NET-3 would have been the obvious choice
// and is the wrong one: a /24 is 254 addresses, so asking for five thousand
// clients would silently give you 254 of them, each making twenty times its
// share of the requests and tripping a per-IP limit that was never the thing
// under test.
func spoofIP(n int) string {
	n %= 1 << 17 // the usable size of a /15
	return fmt.Sprintf("198.18.%d.%d", n>>8&0x1ff, n&0xff)
}

func run(ctx context.Context, url string, concurrency, clients, rps int, timeout time.Duration) *outcome {
	// One transport shared by every worker, with enough idle connections to go
	// round. The default MaxIdleConnsPerHost is 2, so without this the test
	// spends its time in TCP and TLS setup and measures the dial, not the API.
	transport := &http.Transport{
		MaxIdleConns:        concurrency * 2,
		MaxIdleConnsPerHost: concurrency * 2,
		IdleConnTimeout:     90 * time.Second,
	}
	client := &http.Client{Transport: transport, Timeout: timeout}

	out := &outcome{samples: make([]time.Duration, 0, 1<<16)}
	// Shared across workers so every simulated client is used once before any
	// is used twice.
	var seq atomic.Int64
	start := time.Now()

	var wg sync.WaitGroup
	for range concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			local := make([]time.Duration, 0, 1024)

			// Pacing matters more than raw throughput here. Unpaced, this tool
			// finds the rate limiter — sixty menu reads per IP per minute — and
			// then spends the rest of the run measuring how fast a 429 can be
			// written, which is a real number about the wrong thing. Pacing
			// keeps each simulated client under its own budget so what gets
			// measured is the menu.
			var pace *time.Ticker
			if rps > 0 {
				interval := time.Duration(float64(time.Second) / (float64(rps) / float64(concurrency)))
				pace = time.NewTicker(interval)
				defer pace.Stop()
			}

			for ctx.Err() == nil {
				// Rotated per request, not per worker: with one address per
				// goroutine there are only ever `concurrency` distinct clients,
				// so a hundred workers pretending to be four thousand people
				// still trip a per-IP limit meant for one.
				forwarded := ""
				if clients > 0 {
					forwarded = spoofIP(int(seq.Add(1)) % clients)
				}

				if pace != nil {
					select {
					case <-pace.C:
					case <-ctx.Done():
						continue
					}
				}
				took, status, n, err := once(ctx, client, url, forwarded)
				switch {
				case err != nil:
					out.failed.Add(1)
					continue
				case status == http.StatusTooManyRequests:
					out.limited.Add(1)
					// Deliberately not sampled: a refusal is written without
					// touching Postgres or Redis, so folding it into the
					// latency distribution reports a service that looks faster
					// the more of it is broken.
					continue
				case status >= 400:
					out.failed.Add(1)
				default:
					out.ok.Add(1)
					out.bytes.Add(n)
				}
				local = append(local, took)
			}

			// Appended once at the end rather than under a lock per request:
			// the lock would otherwise be the thing being measured.
			out.mu.Lock()
			out.samples = append(out.samples, local...)
			out.mu.Unlock()
		}()
	}
	wg.Wait()

	out.elapsed = time.Since(start)
	return out
}

func once(ctx context.Context, client *http.Client, url, forwarded string) (time.Duration, int, int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, 0, 0, err
	}
	if forwarded != "" {
		req.Header.Set("X-Forwarded-For", forwarded)
	}

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return 0, 0, 0, err
	}
	// Drained, not just closed: an unread body means the connection cannot be
	// reused, and the test would silently become a connection benchmark.
	n, _ := io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	return time.Since(start), resp.StatusCode, n, nil
}

func (o *outcome) report(concurrency int, before, after cacheCounts) {
	sort.Slice(o.samples, func(i, j int) bool { return o.samples[i] < o.samples[j] })

	total := o.ok.Load() + o.failed.Load() + o.limited.Load()
	fmt.Printf("\n%d clients, %s\n", concurrency, o.elapsed.Round(time.Millisecond))
	fmt.Printf("  requests   %d  (%.0f/s)\n", total, float64(total)/o.elapsed.Seconds())
	fmt.Printf("  ok         %d\n", o.ok.Load())
	fmt.Printf("  rate limited %d\n", o.limited.Load())
	fmt.Printf("  failed     %d\n", o.failed.Load())
	fmt.Printf("  transferred %.1f MiB\n", float64(o.bytes.Load())/(1<<20))

	if len(o.samples) > 0 {
		fmt.Printf("\n  latency of served requests (refusals excluded)\n")
		fmt.Printf("           p50 %-9s p95 %-9s p99 %-9s max %s\n",
			percentile(o.samples, 0.50).Round(time.Microsecond),
			percentile(o.samples, 0.95).Round(time.Microsecond),
			percentile(o.samples, 0.99).Round(time.Microsecond),
			o.samples[len(o.samples)-1].Round(time.Microsecond))
	}

	hits, misses := after.hits-before.hits, after.misses-before.misses
	if hits+misses > 0 {
		ratio := float64(hits) / float64(hits+misses) * 100
		fmt.Printf("\n  menu cache  %d hits, %d misses  (%.2f%% hit ratio)\n", hits, misses, ratio)
		if ratio < 95 {
			fmt.Printf("  ⚠ below 95%%: something is bumping menu_version, or the cache is not reachable\n")
		}
	}
	fmt.Println()
}

// percentile uses nearest-rank, which needs no interpolation and cannot report
// a latency that nobody actually observed.
func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	i := int(p * float64(len(sorted)))
	if i >= len(sorted) {
		i = len(sorted) - 1
	}
	return sorted[i]
}

type cacheCounts struct{ hits, misses int64 }

// scrapeCache reads the two counters straight out of the exposition format. A
// Prometheus client library to parse four lines would be a dependency for the
// load tool that the service itself does not need.
func scrapeCache(url string) cacheCounts {
	if url == "" {
		return cacheCounts{}
	}
	resp, err := http.Get(url)
	if err != nil {
		log.Printf("metrics unavailable (%v); the cache ratio will not be reported", err)
		return cacheCounts{}
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return cacheCounts{}
	}

	var counts cacheCounts
	for _, line := range splitLines(string(body)) {
		switch {
		case hasPrefix(line, "qrmenu_menu_cache_hits_total "):
			counts.hits = parseValue(line)
		case hasPrefix(line, "qrmenu_menu_cache_misses_total "):
			counts.misses = parseValue(line)
		}
	}
	return counts
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := range len(s) {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

// parseValue reads the number after the metric name. Prometheus writes counters
// as floats ("42" or "4.2e+01"), so this parses as one and truncates.
func parseValue(line string) int64 {
	var v float64
	i := len(line) - 1
	for i > 0 && line[i] != ' ' {
		i--
	}
	if _, err := fmt.Sscanf(line[i+1:], "%g", &v); err != nil {
		return 0
	}
	return int64(v)
}
