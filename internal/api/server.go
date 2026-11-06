// Package api implements the HTTP surface: intake, queries, admin and ops.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"ravenhub/internal/config"
	"ravenhub/internal/metrics"
	"ravenhub/internal/ratelimit"
	"ravenhub/internal/store"
)

// Server carries the API dependencies.
type Server struct {
	Cfg    *config.Config
	Store  *store.Store
	Intake *ratelimit.Registry // buckets por proyecto: entrada de notify y lecturas
	Auth   *ratelimit.Registry // buckets de fallos de autenticación por IP
	Ready  func() bool         // readyz extra (worker vivo)
}

func NewServer(cfg *config.Config, st *store.Store, ready func() bool) *Server {
	return &Server{Cfg: cfg, Store: st, Intake: ratelimit.NewRegistry(), Auth: ratelimit.NewRegistry(), Ready: ready}
}

// Routes builds the http.Handler with method-based routing (Go 1.22 patterns).
// Health/ops son públicos; todo lo demás exige Bearer token y admin exige rol.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	// Operación
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)
	mux.HandleFunc("GET /metrics", s.handleMetrics)

	// Consumo (v1)
	mux.Handle("POST /v1/notify", s.bearerAuth(http.HandlerFunc(s.handleNotify)))
	mux.Handle("GET /v1/messages/{id}", s.bearerAuth(http.HandlerFunc(s.handleGetMessage)))
	mux.Handle("GET /v1/messages", s.bearerAuth(http.HandlerFunc(s.handleListMessages)))

	// Admin
	mux.Handle("POST /admin/projects", s.bearerAuth(s.admin(s.handleCreateProject)))
	mux.Handle("GET /admin/projects", s.bearerAuth(s.admin(s.handleListProjects)))
	mux.Handle("PATCH /admin/projects/{id}", s.bearerAuth(s.admin(s.handleUpdateProject)))
	mux.Handle("DELETE /admin/projects/{id}", s.bearerAuth(s.admin(s.handleDeleteProject)))
	mux.Handle("POST /admin/projects/{id}/tokens", s.bearerAuth(s.admin(s.handleCreateToken)))
	mux.Handle("GET /admin/projects/{id}/tokens", s.bearerAuth(s.admin(s.handleListTokens)))
	mux.Handle("POST /admin/tokens/{id}/revoke", s.bearerAuth(s.admin(s.handleRevokeToken)))
	mux.Handle("POST /admin/tokens/{id}/rotate", s.bearerAuth(s.admin(s.handleRotateToken)))
	mux.Handle("POST /admin/projects/{id}/accounts", s.bearerAuth(s.admin(s.handleCreateAccount)))
	mux.Handle("GET /admin/projects/{id}/accounts", s.bearerAuth(s.admin(s.handleListAccounts)))
	mux.Handle("DELETE /admin/accounts/{id}", s.bearerAuth(s.admin(s.handleDeleteAccount)))
	mux.Handle("POST /admin/projects/{id}/destinations", s.bearerAuth(s.admin(s.handleCreateDestination)))
	mux.Handle("GET /admin/projects/{id}/destinations", s.bearerAuth(s.admin(s.handleListDestinations)))
	mux.Handle("POST /admin/projects/{id}/groups", s.bearerAuth(s.admin(s.handleCreateGroup)))
	mux.Handle("GET /admin/projects/{id}/groups", s.bearerAuth(s.admin(s.handleListGroups)))
	mux.Handle("DELETE /admin/projects/{id}/groups/{name}", s.bearerAuth(s.admin(s.handleDeleteGroup)))
	mux.Handle("POST /admin/projects/{id}/routing-rules", s.bearerAuth(s.admin(s.handleCreateRoutingRule)))
	mux.Handle("GET /admin/projects/{id}/routing-rules", s.bearerAuth(s.admin(s.handleListRoutingRules)))
	mux.Handle("DELETE /admin/routing-rules/{id}", s.bearerAuth(s.admin(s.handleDeleteRoutingRule)))
	mux.Handle("DELETE /admin/destinations/{id}", s.bearerAuth(s.admin(s.handleDeleteDestination)))
	mux.Handle("POST /admin/destinations/{id}/test", s.bearerAuth(s.admin(s.handleTestDestination)))
	mux.Handle("POST /admin/projects/{id}/templates", s.bearerAuth(s.admin(s.handleUpsertTemplate)))
	mux.Handle("GET /admin/messages", s.bearerAuth(s.admin(s.handleAdminListMessages)))
	mux.Handle("POST /admin/messages/{id}/replay", s.bearerAuth(s.admin(s.handleReplay)))
	mux.Handle("GET /admin/audit", s.bearerAuth(s.admin(s.handleAudit)))
	mux.Handle("GET /admin/stats", s.bearerAuth(s.admin(s.handleStats)))

	return s.middleware(mux)
}

type ctxKey int

const identityKey ctxKey = 1

func identityFrom(ctx context.Context) (store.TokenRow, bool) {
	t, ok := ctx.Value(identityKey).(store.TokenRow)
	return t, ok
}

// middleware wraps recover + logging + request metrics.
func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: 200}
		// Respuestas con PII no deben quedar en cachés de proxies/clientes.
		if strings.HasPrefix(r.URL.Path, "/v1/") || strings.HasPrefix(r.URL.Path, "/admin/") {
			rec.Header().Set("Cache-Control", "no-store")
		}
		defer func() {
			if p := recover(); p != nil {
				slog.Error("pánico HTTP", "path", r.URL.Path, "panic", p)
				// la métrica debe reflejar el 500 aunque ya se hubiera escrito
				rec.status = http.StatusInternalServerError
				// si el handler ya respondió (posible a mitad del body), escribir
				// otro status solo corrompería la respuesta del cliente
				if !rec.wrote {
					errorJSON(rec, http.StatusInternalServerError, "error interno")
				}
			}
			metrics.Inc("ravenhub_http_requests_total", "code", itoa(rec.status))
			slog.Debug("http", "method", r.Method, "path", r.URL.Path,
				"status", rec.status, "elapsed_ms", time.Since(start).Milliseconds())
		}()
		next.ServeHTTP(rec, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.wrote = true
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	r.wrote = true
	return r.ResponseWriter.Write(b)
}

// admin wraps a handler requiring an admin token.
func (s *Server) admin(next func(http.ResponseWriter, *http.Request, store.TokenRow)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := identityFrom(r.Context())
		if !ok || !id.IsAdmin() {
			errorJSON(w, http.StatusForbidden, "se requiere token de admin")
			return
		}
		next(w, r, id)
	}
}

// JSON helpers --------------------------------------------------------------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func errorJSON(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func decodeBody(w http.ResponseWriter, r *http.Request, max int64, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, max)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			errorJSON(w, http.StatusRequestEntityTooLarge, "payload demasiado grande")
		} else {
			errorJSON(w, http.StatusBadRequest, "payload inválido: "+err.Error())
		}
		return false
	}
	// basura tras el JSON ({"to":[...]} basura, o un `}` extra) no se acepta
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		errorJSON(w, http.StatusBadRequest, "payload inválido: datos tras el JSON")
		return false
	}
	return true
}

func pathVar(r *http.Request, name string) string { return r.PathValue(name) }

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
