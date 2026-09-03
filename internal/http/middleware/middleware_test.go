package middleware_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ubilolxon/qrmenu/internal/http/middleware"
)

// ClientIP is a rate limiter key, not only a log field, so a value that
// collapses many callers into one is a way for any of them to lock out the rest.
func TestClientIP(t *testing.T) {
	tests := []struct {
		name       string
		remoteAddr string
		headers    map[string]string
		want       string
	}{
		{"ipv4 with port", "203.0.113.7:54321", nil, "203.0.113.7"},
		{
			// The case that was wrong: cutting at the first colon returns "[",
			// so every IPv6 client shared one bucket.
			name:       "ipv6 with port",
			remoteAddr: "[2001:db8::1]:54321",
			want:       "2001:db8::1",
		},
		{"ipv6 loopback", "[::1]:8080", nil, "::1"},
		{"no port", "203.0.113.7", nil, "203.0.113.7"},
		{
			name:       "forwarded chain names the client, not the proxy",
			remoteAddr: "10.0.0.1:443",
			headers:    map[string]string{"X-Forwarded-For": "203.0.113.7, 70.41.3.18, 150.172.238.178"},
			want:       "203.0.113.7",
		},
		{
			name:       "real ip header",
			remoteAddr: "10.0.0.1:443",
			headers:    map[string]string{"X-Real-IP": "203.0.113.9"},
			want:       "203.0.113.9",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = tc.remoteAddr
			for k, v := range tc.headers {
				r.Header.Set(k, v)
			}
			if got := middleware.ClientIP(r); got != tc.want {
				t.Errorf("ClientIP = %q, want %q", got, tc.want)
			}
		})
	}
}

// Two different IPv6 clients must not share a rate-limit bucket.
func TestDistinctIPv6ClientsDoNotCollide(t *testing.T) {
	ip := func(addr string) string {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = addr
		return middleware.ClientIP(r)
	}

	if a, b := ip("[2001:db8::1]:1000"), ip("[2001:db8::2]:1000"); a == b {
		t.Fatalf("two IPv6 clients both resolved to %q — they share a rate-limit bucket", a)
	}
}

// A handler that cannot learn about shutdown must keep working, not spin. A nil
// channel blocks forever in a select, which is exactly that.
func TestDrainingIsNilWithoutAServer(t *testing.T) {
	if ch := middleware.Draining(context.Background()); ch != nil {
		t.Errorf("Draining on a bare context = %v, want nil", ch)
	}
}

func TestDrainingSignalsOnce(t *testing.T) {
	ch := make(chan struct{})
	ctx := middleware.WithDraining(context.Background(), ch)

	got := middleware.Draining(ctx)
	if got == nil {
		t.Fatal("Draining returned nil for a context that carries the channel")
	}

	select {
	case <-got:
		t.Fatal("the channel was ready before shutdown began")
	default:
	}

	close(ch)
	select {
	case <-got:
	case <-time.After(time.Second):
		t.Fatal("closing the channel did not signal the handler")
	}
}
