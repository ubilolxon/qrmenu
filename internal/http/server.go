package http

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/ubilolxon/qrmenu/internal/config"
	"github.com/ubilolxon/qrmenu/internal/http/middleware"
)

// Serve runs the HTTP server until ctx is cancelled, then drains in-flight
// requests within the configured shutdown timeout.
//
// Draining matters more here than in a typical CRUD service: from Phase 5 the
// staff SSE streams are long-lived connections, and killing them abruptly
// leaves kitchen tablets showing stale orders until they notice and reconnect.
func Serve(ctx context.Context, cfg config.HTTP, logger *slog.Logger, h http.Handler) error {
	// Closed when shutdown begins. See middleware.Draining for why this is a
	// separate signal from cancelling the request contexts.
	draining := make(chan struct{})

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second, // caps slowloris-style header trickling
		ReadTimeout:       cfg.ReadTimeout,
		// Note for Phase 5: WriteTimeout kills a response mid-stream, which
		// would sever every SSE connection on a fixed interval. The staff
		// stream handler must clear its own deadline via
		// http.ResponseController.SetWriteDeadline(time.Time{}).
		WriteTimeout: cfg.WriteTimeout,
		IdleTimeout:  cfg.IdleTimeout,
		// Deliberately NOT derived from ctx — see Draining. A request context
		// that dies with the process turns every in-flight write into an
		// unknown outcome for its client.
		BaseContext: func(_ net.Listener) context.Context {
			return middleware.WithDraining(context.Background(), draining)
		},
	}

	// Runs in its own goroutine the moment Shutdown is called, before it starts
	// waiting for connections to go idle. Closing the channel here rather than
	// at the ctx.Done() below is what guarantees the streams are already
	// letting go by the time anything is waiting on them.
	srv.RegisterOnShutdown(func() { close(draining) })

	errCh := make(chan error, 1)
	go func() {
		logger.Info("http server listening", slog.String("addr", cfg.Addr))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("listen: %w", err)
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		logger.Info("shutdown signal received, draining connections",
			slog.Duration("timeout", cfg.ShutdownTimeout))

		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.ShutdownTimeout)
		defer cancel()

		if err := srv.Shutdown(shutdownCtx); err != nil {
			// Requests still running past the deadline get cut off here.
			_ = srv.Close()
			return fmt.Errorf("graceful shutdown failed: %w", err)
		}
		logger.Info("http server stopped cleanly")
		return nil
	}
}

// ServeMetrics runs the Prometheus scrape endpoint on its own listener.
//
// A separate port, not a route on the main router, and the reason is
// operational rather than architectural: the public listener is the one behind
// the load balancer, and /metrics on it would publish pool saturation, request
// volumes and route names to anyone who asked. Binding it elsewhere means the
// endpoint cannot be reached from the internet even if someone forgets to write
// the rule that would have blocked it.
//
// It is deliberately unauthenticated on that port. A scraper is a piece of
// infrastructure, and the boundary being relied on is the network.
func ServeMetrics(ctx context.Context, addr string, logger *slog.Logger, h http.Handler) error {
	if addr == "" {
		logger.Info("metrics endpoint disabled")
		return nil
	}

	mux := http.NewServeMux()
	mux.Handle("/metrics", h)

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		BaseContext:       func(_ net.Listener) context.Context { return ctx },
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("metrics listening", slog.String("addr", addr), slog.String("path", "/metrics"))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("metrics listen: %w", err)
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		return nil
	}
}
