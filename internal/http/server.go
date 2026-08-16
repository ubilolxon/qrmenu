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
)

// Serve runs the HTTP server until ctx is cancelled, then drains in-flight
// requests within the configured shutdown timeout.
//
// Draining matters more here than in a typical CRUD service: from Phase 5 the
// staff SSE streams are long-lived connections, and killing them abruptly
// leaves kitchen tablets showing stale orders until they notice and reconnect.
func Serve(ctx context.Context, cfg config.HTTP, logger *slog.Logger, h http.Handler) error {
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
		BaseContext:  func(_ net.Listener) context.Context { return ctx },
	}

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
