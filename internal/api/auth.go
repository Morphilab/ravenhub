package api

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"

	"ravenhub/internal/crypto"
	"ravenhub/internal/store"
)

// bearerAuth resolves the Authorization header to a token row and injects it
// into the context. Sin cache: revocar invalida al instante. Exige el
// esquema "Bearer ".
func (s *Server) bearerAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("Authorization")
		if !strings.HasPrefix(h, "Bearer ") {
			errorJSON(w, http.StatusUnauthorized, "falta Authorization: Bearer <token>")
			return
		}
		raw := strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
		if raw == "" {
			errorJSON(w, http.StatusUnauthorized, "falta Authorization: Bearer <token>")
			return
		}
		row, err := s.Store.FindTokenByHash(r.Context(), crypto.HashToken(raw))
		// Un fallo de DB no es un token inválido: devolver 401 lo disfrazaba
		// de revocación y no dejaba rastro.
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			slog.Error("auth: error consultando el token", "err", err)
			errorJSON(w, http.StatusServiceUnavailable, "autenticación temporalmente no disponible")
			return
		}
		// Un único mensaje para token inválido, revocado o expirado evita
		// enumeración. El throttle de fallos usa un registry propio para que
		// el intake no pueda ser estancado por fallos de autenticación.
		if err != nil || row.IsRevoked() || (row.ExpiresAt != nil && row.ExpiresAt.Before(timeNow())) {
			b := s.Auth.Get("auth:"+clientIP(r), 30, 10)
			if !b.Allow() {
				w.Header().Set("Retry-After", "60")
				errorJSON(w, http.StatusTooManyRequests, "rate limit de autenticación")
				return
			}
			errorJSON(w, http.StatusUnauthorized, "token inválido")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), identityKey, row)))
	})
}

// clientIP devuelve la IP de la conexión TCP. NO consulta X-Forwarded-For:
// es una cabecera controlable por el cliente y no debe usarse como clave de
// throttle. Detrás de Caddy, RemoteAddr es la IP del proxy y el bucket
// funciona como contador global — conservador y no spoofeable.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// projectID returns the project the token belongs to (empty for admin).
func projectIDOf(id store.TokenRow) *string {
	if id.ProjectID == nil {
		return nil
	}
	return id.ProjectID
}
