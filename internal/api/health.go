package api

import (
	"context"
	"net/http"
	"time"

	"ravenhub/internal/metrics"
)

// handleHealthz: liveness — el proceso responde.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleReadyz: readiness — DB accesible y worker vivo. Ping con timeout
// propio (con la DB colgada colgaba el readyz entero).
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	// Público y con counts: sin throttle, un cliente puede martillear la DB.
	if b := s.Auth.Get("readyz:"+clientIP(r), 60, 10); !b.Allow() {
		w.Header().Set("Retry-After", "30")
		errorJSON(w, http.StatusTooManyRequests, "rate limit de readiness")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if err := s.Store.Pool.Ping(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"status": "degraded", "reason": "postgres inaccesible",
		})
		return
	}
	if s.Ready != nil && !s.Ready() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"status": "degraded", "reason": "worker detenido",
		})
		return
	}
	// counts con el mismo presupuesto del ping; un error aquí no puede
	// reportarse como ceros con "ready" (falsos tranquilizadores): readyz
	// falla honesto. queue_depth = trabajo vivo (queued+delivering), igual
	// que el gauge y el shedding global.
	q, qErr := s.Store.CountPending(ctx)
	d, dErr := s.Store.CountByStatus(ctx, "dead")
	e, eErr := s.Store.CountByStatus(ctx, "expired")
	if qErr != nil || dErr != nil || eErr != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"status": "degraded", "reason": "postgres accesible pero los conteos fallaron",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ready", "queue_depth": q, "dlq_size": d, "expired": e,
	})
}

// handleMetrics: exposición Prometheus en texto (sin dependencias).
// Los gauges de cola/DLQ los refresca el mantenedor del worker.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write([]byte(metrics.Render()))
}
