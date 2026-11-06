package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"ravenhub/internal/store"
)

// handleGetMessage: GET /v1/messages/{id} — estado y entregas, scoped al
// proyecto del token (aislamiento por construcción) y a sus scopes de
// destinos.
func (s *Server) handleGetMessage(w http.ResponseWriter, r *http.Request) {
	id, _ := identityFrom(r.Context())
	readKey := "read:admin"
	if id.ProjectID != nil {
		readKey = "read:" + *id.ProjectID
	}
	if b := s.Intake.Get(readKey, 120, 60); !b.Allow() {
		w.Header().Set("Retry-After", "10")
		errorJSON(w, http.StatusTooManyRequests, "rate limit de lectura; reintenta más tarde")
		return
	}
	row, err := s.Store.GetMessage(r.Context(), projectIDOf(id), pathVar(r, "id"))
	if errors.Is(err, store.ErrNotFound) {
		errorJSON(w, http.StatusNotFound, "mensaje no encontrado")
		return
	}
	if err != nil {
		errorJSON(w, http.StatusInternalServerError, "error consultando")
		return
	}
	if !messageVisible(id.Scopes.Destinations, row.Payload) {
		errorJSON(w, http.StatusForbidden, "mensaje fuera del scope de este token")
		return
	}
	dels, _ := s.Store.ListDeliveries(r.Context(), row.ID)
	view := s.messageToViewFor(id, row)
	view["deliveries"] = toDeliveryViews(filterDeliveries(id.Scopes.Destinations, dels))
	writeJSON(w, http.StatusOK, view)
}

// handleListMessages: GET /v1/messages?status=&limit=
func (s *Server) handleListMessages(w http.ResponseWriter, r *http.Request) {
	id, _ := identityFrom(r.Context())
	if id.IsAdmin() {
		errorJSON(w, http.StatusForbidden, "usa un token de proyecto (el admin tiene /admin/messages)")
		return
	}
	limit := 50
	if l, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && l > 0 && l <= 200 {
		limit = l
	}
	// Las lecturas pesan (payload completo por fila, hasta 500): mismo
	// limiter de entrada con techo propio para lecturas — sin esto, un token
	// válido podía serializar decenas de MB por segundo contra la DB compartida.
	if b := s.Intake.Get("read:"+*id.ProjectID, 120, 60); !b.Allow() {
		w.Header().Set("Retry-After", "10")
		errorJSON(w, http.StatusTooManyRequests, "rate limit de lectura; reintenta más tarde")
		return
	}
	// El filtro por scopes de destinos lo hace el SQL: filtrar tras el LIMIT
	// podía devolver una página vacía aunque existieran mensajes visibles más
	// atrás.
	rows, err := s.Store.ListMessages(r.Context(), projectIDOf(id), r.URL.Query().Get("status"), limit, id.Scopes.Destinations)
	if err != nil {
		errorJSON(w, http.StatusInternalServerError, "error listando")
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, m := range rows {
		out = append(out, s.messageToViewFor(id, m))
	}
	writeJSON(w, http.StatusOK, out)
}

// messageVisible indica si un token con scopes de destinos puede ver un
// mensaje (intersección con su "to"). Sin scopes = todo el proyecto.
func messageVisible(allowed []string, payload json.RawMessage) bool {
	if len(allowed) == 0 {
		return true
	}
	var p struct {
		To []string `json:"to"`
	}
	if json.Unmarshal(payload, &p) != nil {
		return false
	}
	for _, t := range p.To {
		for _, a := range allowed {
			if t == a {
				return true
			}
		}
	}
	return false
}

func filterDeliveries(allowed []string, dels []store.DeliveryRow) []store.DeliveryRow {
	if len(allowed) == 0 {
		return dels
	}
	out := dels[:0]
	for _, d := range dels {
		for _, a := range allowed {
			if d.DestName == a {
				out = append(out, d)
				break
			}
		}
	}
	return out
}
