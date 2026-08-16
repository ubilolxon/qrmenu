// Package handler contains the HTTP handlers. It is split by surface —
// public (guests), admin (SPA), staff (waiter/kitchen) — from Phase 1 onward.
package handler

import (
	"context"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/ubilolxon/qrmenu/internal/httperr"
)

type Health struct {
	DB      *pgxpool.Pool
	Redis   *redis.Client
	Version string
}

// Live reports that the process is running. It touches no dependencies on
// purpose: liveness answers "should this process be restarted?", and a database
// outage is never fixed by restarting the API.
func (h Health) Live(w http.ResponseWriter, _ *http.Request) {
	httperr.JSON(w, http.StatusOK, map[string]string{"status": "ok", "version": h.Version})
}

type readyResponse struct {
	Status  string            `json:"status"` // ok | degraded | unavailable
	Version string            `json:"version"`
	Checks  map[string]string `json:"checks"`
}

// Ready reports whether this instance can serve traffic.
//
// Postgres is required: without it nothing works, so it returns 503 and the
// instance is pulled from the load balancer. Redis is only a cache — a miss
// falls through to Postgres — so losing it reports "degraded" and keeps
// serving. Treating both as fatal would let a Redis blip take down a
// perfectly healthy fleet.
func (h Health) Ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	resp := readyResponse{Status: "ok", Version: h.Version, Checks: map[string]string{}}
	status := http.StatusOK

	if err := h.DB.Ping(ctx); err != nil {
		resp.Checks["postgres"] = "error: " + err.Error()
		resp.Status = "unavailable"
		status = http.StatusServiceUnavailable
	} else {
		resp.Checks["postgres"] = "ok"
	}

	if h.Redis == nil {
		resp.Checks["redis"] = "disabled"
	} else if err := h.Redis.Ping(ctx).Err(); err != nil {
		resp.Checks["redis"] = "error: " + err.Error()
		if resp.Status == "ok" {
			resp.Status = "degraded"
		}
	} else {
		resp.Checks["redis"] = "ok"
	}

	httperr.JSON(w, status, resp)
}
