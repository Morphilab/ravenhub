package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"ravenhub/internal/adapter"
	"ravenhub/internal/model"
	"ravenhub/internal/render"
	"ravenhub/internal/store"
)

var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// --- Proyectos --------------------------------------------------------------

type createProjectReq struct {
	Name      string          `json:"name"`
	Options   json.RawMessage `json:"options"`
	TokenName string          `json:"token_name"` // opcional: crea el primer token
}

func (s *Server) handleCreateProject(w http.ResponseWriter, r *http.Request, admin store.TokenRow) {
	var req createProjectReq
	if !decodeBody(w, r, 8192, &req) {
		return
	}
	if req.Name == "" {
		errorJSON(w, http.StatusBadRequest, "'name' es obligatorio")
		return
	}
	// misma validación estricta que el seed y el PATCH: un typo en las
	// options no puede entrar por create y ser ignorado en runtime.
	if err := model.ValidateProjectOptions(req.Options); err != nil {
		errorJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	p, err := s.Store.CreateProject(r.Context(), req.Name, req.Options)
	if errors.Is(err, store.ErrConflict) {
		errorJSON(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		errorJSON(w, http.StatusInternalServerError, "no se pudo crear el proyecto")
		return
	}
	s.Store.InsertAudit(r.Context(), &p.ID, admin.Prefix, "project.created", p.Name, nil)
	resp := map[string]any{"id": p.ID, "name": p.Name}
	if req.TokenName != "" {
		full, row, err := s.Store.CreateToken(r.Context(), &p.ID, req.TokenName, model.Scopes{}, nil)
		if err != nil {
			// el proyecto SÍ se creó: 201 con la causa visible; el alta no
			// debe parecer exitosa sin credencial.
			resp["token_error"] = err.Error()
			writeJSON(w, http.StatusCreated, resp)
			return
		}
		s.Store.InsertAudit(r.Context(), &p.ID, admin.Prefix, "token.created", row.Prefix, nil)
		// El token en claro se muestra UNA vez.
		resp["token"] = full
		resp["token_prefix"] = row.Prefix
	}
	writeJSON(w, http.StatusCreated, resp)
}

func (s *Server) handleListProjects(w http.ResponseWriter, r *http.Request, _ store.TokenRow) {
	ps, err := s.Store.ListProjects(r.Context())
	if err != nil {
		errorJSON(w, http.StatusInternalServerError, "error listando")
		return
	}
	writeJSON(w, http.StatusOK, ps)
}

// handleUpdateProject: PATCH /admin/projects/{id} — reemplaza las options del
// proyecto (declarativo, sin merge; el mismo camino que el seed). Validación
// estricta: un typo en las claves o un valor ilegible falla en el borde.
func (s *Server) handleUpdateProject(w http.ResponseWriter, r *http.Request, admin store.TokenRow) {
	var req struct {
		Options json.RawMessage `json:"options"`
	}
	if !decodeBody(w, r, 8192, &req) {
		return
	}
	if len(req.Options) == 0 {
		errorJSON(w, http.StatusBadRequest, "'options' es obligatorio (objeto JSON)")
		return
	}
	if err := model.ValidateProjectOptions(req.Options); err != nil {
		errorJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	id := pathVar(r, "id")
	changed, err := s.Store.UpdateProjectOptions(r.Context(), id, req.Options)
	if errors.Is(err, store.ErrNotFound) {
		errorJSON(w, http.StatusNotFound, "proyecto no encontrado")
		return
	}
	if err != nil {
		errorJSON(w, http.StatusInternalServerError, "no se pudieron actualizar las options")
		return
	}
	if changed {
		s.Store.InsertAudit(r.Context(), &id, admin.Prefix, "project.options_updated", id, req.Options)
	}
	writeJSON(w, http.StatusOK, map[string]any{"updated": changed})
}

// handleDeleteProject elimina un proyecto y su grafo OPERATIVO (tokens,
// cuentas, destinos, grupos, plantillas, reglas, mensajes y entregas) vía FK
// CASCADE. Store.DeleteProject anonimiza también sus filas de auditoría en la
// misma transacción (project_id/token_prefix/subject/detail), conservando el
// evento agregado sin PII; el tombstone global `project.deleted` se inserta
// aquí con project_id NULL para que no dependa de una fila que ya no existe.
func (s *Server) handleDeleteProject(w http.ResponseWriter, r *http.Request, admin store.TokenRow) {
	id := pathVar(r, "id")
	if err := s.Store.DeleteProject(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			errorJSON(w, http.StatusNotFound, "proyecto no encontrado")
			return
		}
		errorJSON(w, http.StatusInternalServerError, "no se pudo borrar el proyecto")
		return
	}
	s.Store.InsertAudit(r.Context(), nil, admin.Prefix, "project.deleted", id, nil)
	writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
}

// --- Tokens -----------------------------------------------------------------

type createTokenReq struct {
	Name         string   `json:"name"`
	Destinations []string `json:"destinations"` // vacío = todos
	MaxPriority  string   `json:"max_priority"`
	ExpiresIn    string   `json:"expires_in"` // ej "720h"; vacío = sin caducidad
}

func (s *Server) handleCreateToken(w http.ResponseWriter, r *http.Request, admin store.TokenRow) {
	var req createTokenReq
	if !decodeBody(w, r, 8192, &req) {
		return
	}
	if req.Name == "" {
		errorJSON(w, http.StatusBadRequest, "'name' es obligatorio")
		return
	}
	scopes := model.Scopes{Destinations: req.Destinations}
	if req.MaxPriority != "" {
		p, err := model.ParsePriority(req.MaxPriority)
		if err != nil {
			errorJSON(w, http.StatusBadRequest, err.Error())
			return
		}
		// guardar el valor NORMALIZADO de ParsePriority: un valor crudo
		// ("HIGH", " high") no debe dejar el techo de prioridad fail-open.
		scopes.MaxPriority = p
	}
	var expires *time.Time
	if req.ExpiresIn != "" {
		d, err := time.ParseDuration(req.ExpiresIn)
		if err != nil || d <= 0 {
			errorJSON(w, http.StatusBadRequest, "expires_in inválido (ej: 720h)")
			return
		}
		t := timeNow().Add(d)
		expires = &t
	}
	full, row, err := s.Store.CreateToken(r.Context(), strPtr(pathVar(r, "id")), req.Name, scopes, expires)
	if errors.Is(err, store.ErrConflict) {
		errorJSON(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		errorJSON(w, http.StatusInternalServerError, "no se pudo crear el token")
		return
	}
	s.Store.InsertAudit(r.Context(), strPtr(pathVar(r, "id")), admin.Prefix, "token.created", row.Prefix, nil)
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": row.ID, "prefix": row.Prefix, "token": full, // solo esta vez
	})
}

func (s *Server) handleListTokens(w http.ResponseWriter, r *http.Request, _ store.TokenRow) {
	rows, err := s.Store.ListTokens(r.Context(), pathVar(r, "id"))
	if err != nil {
		errorJSON(w, http.StatusInternalServerError, "error listando")
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, t := range rows {
		out = append(out, map[string]any{
			"id": t.ID, "name": t.Name, "prefix": t.Prefix, "scopes": t.Scopes,
			"revoked": t.IsRevoked(), "expires_at": t.ExpiresAt,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleRevokeToken(w http.ResponseWriter, r *http.Request, admin store.TokenRow) {
	ok, err := s.Store.RevokeToken(r.Context(), pathVar(r, "id"))
	if err != nil {
		errorJSON(w, http.StatusInternalServerError, "error revocando")
		return
	}
	if ok {
		s.Store.InsertAudit(r.Context(), nil, admin.Prefix, "token.revoked", pathVar(r, "id"), nil)
	}
	writeJSON(w, http.StatusOK, map[string]bool{"revoked": ok})
}

func (s *Server) handleRotateToken(w http.ResponseWriter, r *http.Request, admin store.TokenRow) {
	full, row, err := s.Store.RotateToken(r.Context(), pathVar(r, "id"))
	if errors.Is(err, store.ErrNotFound) {
		errorJSON(w, http.StatusNotFound, "token no encontrado")
		return
	}
	if err != nil {
		errorJSON(w, http.StatusInternalServerError, "error rotando")
		return
	}
	s.Store.InsertAudit(r.Context(), row.ProjectID, admin.Prefix, "token.rotated", row.Prefix, nil)
	writeJSON(w, http.StatusOK, map[string]any{
		"id": row.ID, "prefix": row.Prefix, "token": full,
	})
}

// --- Cuentas ----------------------------------------------------------------

type createAccountReq struct {
	Channel string          `json:"channel"`
	Label   string          `json:"label"`
	Creds   json.RawMessage `json:"creds"`
	Limits  json.RawMessage `json:"limits"`
}

func (s *Server) handleCreateAccount(w http.ResponseWriter, r *http.Request, admin store.TokenRow) {
	var req createAccountReq
	if !decodeBody(w, r, model.PayloadMaxBytes, &req) {
		return
	}
	if !adapter.Supported(req.Channel) {
		errorJSON(w, http.StatusBadRequest,
			"canal no soportado: "+req.Channel+" (soportados: "+fmtChannels()+")")
		return
	}
	if strings.TrimSpace(req.Label) == "" {
		errorJSON(w, http.StatusBadRequest, "'label' es obligatorio")
		return
	}
	a, err := s.Store.CreateAccount(r.Context(), pathVar(r, "id"), req.Channel, req.Label, req.Creds, req.Limits)
	if errors.Is(err, store.ErrConflict) {
		errorJSON(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		errorJSON(w, http.StatusInternalServerError, "no se pudo crear la cuenta")
		return
	}
	s.Store.InsertAudit(r.Context(), &a.ProjectID, admin.Prefix, "account.created", a.Label,
		map[string]string{"channel": a.Channel})
	// Nunca devolver creds: solo metadatos.
	writeJSON(w, http.StatusCreated, map[string]any{"id": a.ID, "channel": a.Channel, "label": a.Label})
}

func (s *Server) handleListAccounts(w http.ResponseWriter, r *http.Request, _ store.TokenRow) {
	rows, err := s.Store.ListAccounts(r.Context(), pathVar(r, "id"))
	if err != nil {
		errorJSON(w, http.StatusInternalServerError, "error listando")
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, a := range rows {
		out = append(out, map[string]any{"id": a.ID, "channel": a.Channel, "label": a.Label, "limits": a.Limits})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleDeleteAccount(w http.ResponseWriter, r *http.Request, admin store.TokenRow) {
	projectID, label, dests, err := s.Store.DeleteAccount(r.Context(), pathVar(r, "id"))
	if errors.Is(err, store.ErrNotFound) {
		errorJSON(w, http.StatusNotFound, "cuenta no encontrada")
		return
	}
	if err != nil {
		errorJSON(w, http.StatusInternalServerError, "error borrando")
		return
	}
	// el CASCADE de destinations.account_id se llevó los destinos de la
	// cuenta: esa pérdida queda auditada con su inventario (era la única
	// operación de admin sin asiento — la causa del borrado era invisible)
	s.Store.InsertAudit(r.Context(), &projectID, admin.Prefix, "account.deleted", label,
		map[string]any{"destinations": dests})
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true, "destinations_removed": dests})
}

// --- Destinos y grupos ------------------------------------------------------

type createDestinationReq struct {
	Name       string          `json:"name"`
	AccountID  string          `json:"account_id"`
	Target     json.RawMessage `json:"target"`
	QuietHours json.RawMessage `json:"quiet_hours"`
}

func (s *Server) handleCreateDestination(w http.ResponseWriter, r *http.Request, admin store.TokenRow) {
	var req createDestinationReq
	if !decodeBody(w, r, 8192, &req) {
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		errorJSON(w, http.StatusBadRequest, "'name' es obligatorio")
		return
	}
	if strings.TrimSpace(req.AccountID) == "" {
		errorJSON(w, http.StatusBadRequest, "'account_id' es obligatorio")
		return
	}
	// Fail-fast del silencio: un typo en quiet_hours no puede desactivarlo en
	// la entrega.
	if _, err := render.ParseQuietHours(req.QuietHours); err != nil {
		errorJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	d, err := s.Store.CreateDestination(r.Context(), pathVar(r, "id"), req.Name, req.AccountID, req.Target, req.QuietHours)
	if errors.Is(err, store.ErrConflict) || errors.Is(err, store.ErrNotFound) {
		errorJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	if err != nil {
		errorJSON(w, http.StatusInternalServerError, "no se pudo crear el destino")
		return
	}
	s.Store.InsertAudit(r.Context(), &d.ProjectID, admin.Prefix, "destination.created", d.Name, nil)
	writeJSON(w, http.StatusCreated, map[string]any{"id": d.ID, "name": d.Name})
}

func (s *Server) handleListDestinations(w http.ResponseWriter, r *http.Request, _ store.TokenRow) {
	rows, err := s.Store.ListDestinations(r.Context(), pathVar(r, "id"))
	if err != nil {
		errorJSON(w, http.StatusInternalServerError, "error listando")
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

func (s *Server) handleDeleteDestination(w http.ResponseWriter, r *http.Request, admin store.TokenRow) {
	id := pathVar(r, "id")
	dest, err := s.Store.GetDestinationByID(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		errorJSON(w, http.StatusNotFound, "destino no encontrado")
		return
	}
	if err != nil {
		errorJSON(w, http.StatusInternalServerError, "error borrando")
		return
	}
	if err := s.Store.DeleteDestinationByID(r.Context(), id); err != nil {
		errorJSON(w, http.StatusInternalServerError, "error borrando")
		return
	}
	s.Store.InsertAudit(r.Context(), &dest.ProjectID, admin.Prefix, "destination.deleted", dest.Name, nil)
	writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
}

type createGroupReq struct {
	Name         string   `json:"name"`
	Destinations []string `json:"destinations"`
}

func (s *Server) handleCreateGroup(w http.ResponseWriter, r *http.Request, admin store.TokenRow) {
	var req createGroupReq
	if !decodeBody(w, r, 8192, &req) {
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		errorJSON(w, http.StatusBadRequest, "'name' es obligatorio")
		return
	}
	if err := s.Store.CreateGroup(r.Context(), pathVar(r, "id"), req.Name, req.Destinations); err != nil {
		if errors.Is(err, store.ErrConflict) || errors.Is(err, store.ErrNotFound) {
			errorJSON(w, http.StatusBadRequest, err.Error())
			return
		}
		errorJSON(w, http.StatusInternalServerError, "no se pudo crear el grupo")
		return
	}
	s.Store.InsertAudit(r.Context(), strPtr(pathVar(r, "id")), admin.Prefix, "group.created", req.Name, nil)
	writeJSON(w, http.StatusCreated, map[string]any{"name": req.Name})
}

func (s *Server) handleListGroups(w http.ResponseWriter, r *http.Request, _ store.TokenRow) {
	rows, err := s.Store.ListGroups(r.Context(), pathVar(r, "id"))
	if err != nil {
		errorJSON(w, http.StatusInternalServerError, "error listando")
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

// handleDeleteGroup: DELETE /admin/projects/{id}/groups/{name} — paridad con
// create/list.
func (s *Server) handleDeleteGroup(w http.ResponseWriter, r *http.Request, admin store.TokenRow) {
	projectID, name := pathVar(r, "id"), pathVar(r, "name")
	if err := s.Store.DeleteGroup(r.Context(), projectID, name); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			errorJSON(w, http.StatusNotFound, "grupo no encontrado")
			return
		}
		errorJSON(w, http.StatusInternalServerError, "no se pudo borrar el grupo")
		return
	}
	s.Store.InsertAudit(r.Context(), &projectID, admin.Prefix, "group.deleted", name, nil)
	writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
}

// --- Reglas de enrutamiento -----------------------------------

type routingRuleReq struct {
	Name           string         `json:"name"`
	MatchTags      []string       `json:"match_tags"`     // todos deben estar
	MatchPriority  string         `json:"match_priority"` // prioridad mínima
	MatchPayload   map[string]any `json:"match_payload"`  // subconjunto de data
	Destinations   []string       `json:"destinations"`   // destinos/grupos a añadir
	StopProcessing bool           `json:"stop_processing"`
	Enabled        *bool          `json:"enabled"`  // default true
	Priority       int            `json:"priority"` // orden: menor = primero
}

func (s *Server) handleCreateRoutingRule(w http.ResponseWriter, r *http.Request, admin store.TokenRow) {
	var req routingRuleReq
	if !decodeBody(w, r, 8192, &req) {
		return
	}
	if req.Name == "" {
		errorJSON(w, http.StatusBadRequest, "'name' es obligatorio")
		return
	}
	if len(req.Destinations) == 0 {
		errorJSON(w, http.StatusBadRequest, "'destinations' es obligatorio")
		return
	}
	if req.MatchPriority != "" {
		if _, err := model.ParsePriority(req.MatchPriority); err != nil {
			errorJSON(w, http.StatusBadRequest, "match_priority inválido: "+err.Error())
			return
		}
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	if req.Priority == 0 {
		req.Priority = 100
	}
	rule := model.RoutingRule{
		Name: req.Name, MatchTags: req.MatchTags, MatchPriority: req.MatchPriority,
		MatchPayload: req.MatchPayload, Destinations: req.Destinations,
		StopProcessing: req.StopProcessing, Enabled: enabled, Priority: req.Priority,
	}
	created, err := s.Store.CreateRoutingRule(r.Context(), pathVar(r, "id"), rule)
	if errors.Is(err, store.ErrConflict) {
		errorJSON(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		errorJSON(w, http.StatusInternalServerError, "no se pudo crear la regla")
		return
	}
	s.Store.InsertAudit(r.Context(), strPtr(pathVar(r, "id")), admin.Prefix, "routing_rule.created", created.Name, nil)
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) handleListRoutingRules(w http.ResponseWriter, r *http.Request, _ store.TokenRow) {
	rules, err := s.Store.ListRoutingRules(r.Context(), pathVar(r, "id"))
	if err != nil {
		errorJSON(w, http.StatusInternalServerError, "error listando reglas")
		return
	}
	if rules == nil {
		rules = []model.RoutingRule{}
	}
	writeJSON(w, http.StatusOK, rules)
}

func (s *Server) handleDeleteRoutingRule(w http.ResponseWriter, r *http.Request, admin store.TokenRow) {
	id := pathVar(r, "id")
	if err := s.Store.DeleteRoutingRule(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			errorJSON(w, http.StatusNotFound, "regla no encontrada")
			return
		}
		errorJSON(w, http.StatusInternalServerError, "no se pudo borrar la regla")
		return
	}
	s.Store.InsertAudit(r.Context(), nil, admin.Prefix, "routing_rule.deleted", id, nil)
	writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
}

type upsertTemplateReq struct {
	Name      string          `json:"name"`
	Body      string          `json:"body"`
	Overrides json.RawMessage `json:"overrides"`
}

func (s *Server) handleUpsertTemplate(w http.ResponseWriter, r *http.Request, admin store.TokenRow) {
	var req upsertTemplateReq
	if !decodeBody(w, r, model.PayloadMaxBytes, &req) {
		return
	}
	if req.Name == "" || req.Body == "" {
		errorJSON(w, http.StatusBadRequest, "'name' y 'body' son obligatorios")
		return
	}
	if err := s.Store.UpsertTemplate(r.Context(), pathVar(r, "id"), req.Name, req.Body, req.Overrides); err != nil {
		errorJSON(w, http.StatusInternalServerError, "no se pudo guardar la plantilla")
		return
	}
	s.Store.InsertAudit(r.Context(), strPtr(pathVar(r, "id")), admin.Prefix, "template.upserted", req.Name, nil)
	writeJSON(w, http.StatusOK, map[string]bool{"saved": true})
}

// --- Mensajes (admin) -------------------------------------------------------

func (s *Server) handleAdminListMessages(w http.ResponseWriter, r *http.Request, _ store.TokenRow) {
	limit := 100
	if l, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && l > 0 && l <= 1000 {
		limit = l
	}
	var project *string
	if p := r.URL.Query().Get("project_id"); p != "" {
		if !uuidRe.MatchString(p) {
			errorJSON(w, http.StatusBadRequest, "project_id inválido (uuid)")
			return
		}
		project = &p
	}
	rows, err := s.Store.ListMessages(r.Context(), project, r.URL.Query().Get("status"), limit, nil)
	if err != nil {
		errorJSON(w, http.StatusInternalServerError, "error listando")
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, m := range rows {
		out = append(out, s.messageToView(m))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleReplay(w http.ResponseWriter, r *http.Request, admin store.TokenRow) {
	m, err := s.Store.ReplayMessage(r.Context(), pathVar(r, "id"))
	if errors.Is(err, store.ErrNotFound) {
		errorJSON(w, http.StatusNotFound, "mensaje no encontrado o no reenviable (debe estar dead/failed/expired)")
		return
	}
	if err != nil {
		errorJSON(w, http.StatusInternalServerError, "error reenviando")
		return
	}
	s.Store.InsertAudit(r.Context(), &m.ProjectID, admin.Prefix, "message.replayed", m.ID, nil)
	writeJSON(w, http.StatusOK, map[string]any{"id": m.ID, "status": m.Status})
}

func (s *Server) handleTestDestination(w http.ResponseWriter, r *http.Request, admin store.TokenRow) {
	dest, err := s.Store.GetDestinationByID(r.Context(), pathVar(r, "id"))
	if errors.Is(err, store.ErrNotFound) {
		errorJSON(w, http.StatusNotFound, "destino no encontrado")
		return
	}
	if err != nil {
		errorJSON(w, http.StatusInternalServerError, "error buscando destino")
		return
	}
	payload, _ := json.Marshal(map[string]any{
		"to":       []string{dest.Name},
		"title":    "Prueba RavenHub",
		"message":  "Mensaje de prueba del destino " + dest.Name,
		"priority": string(model.PriorityNormal),
	})
	res, err := s.Store.EnqueueMessage(r.Context(), dest.ProjectID, "", payload,
		model.PriorityNormal, nil, 15*time.Minute, store.InsertOpts{NextAttemptAt: time.Now()})
	if err != nil {
		errorJSON(w, http.StatusInternalServerError, "no se pudo encolar la prueba")
		return
	}
	s.Store.InsertAudit(r.Context(), &dest.ProjectID, admin.Prefix, "destination.test", dest.Name,
		map[string]string{"message_id": res.ID})
	writeJSON(w, http.StatusAccepted, map[string]any{"message_id": res.ID})
}

func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request, _ store.TokenRow) {
	limit := 100
	if l, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && l > 0 && l <= 1000 {
		limit = l
	}
	var project *string
	if p := r.URL.Query().Get("project_id"); p != "" {
		if !uuidRe.MatchString(p) {
			errorJSON(w, http.StatusBadRequest, "project_id inválido (uuid)")
			return
		}
		project = &p
	}
	rows, err := s.Store.ListAudit(r.Context(), project, limit)
	if err != nil {
		errorJSON(w, http.StatusInternalServerError, "error listando auditoría")
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request, _ store.TokenRow) {
	// ?project_id= filtra el conteo: sin él, el conteo global suma el
	// historial de TODOS los proyectos de la DB y falsea umbrales por tenant
	pid := r.URL.Query().Get("project_id")
	if pid != "" {
		if !uuidRe.MatchString(pid) {
			errorJSON(w, http.StatusBadRequest, "project_id inválido (uuid)")
			return
		}
	}
	out := map[string]any{}
	for _, st := range []string{"queued", "delivering", "sent", "failed", "dead", "expired"} {
		var (
			n   int
			err error
		)
		if pid != "" {
			n, err = s.Store.CountByStatusForProject(r.Context(), pid, st)
		} else {
			n, err = s.Store.CountByStatus(r.Context(), st)
		}
		if err == nil {
			out[st] = n
		}
	}
	if pid != "" {
		out["project_id"] = pid
	}
	writeJSON(w, http.StatusOK, out)
}

// --- helpers ----------------------------------------------------------------

func strPtr(s string) *string { return &s }

func fmtChannels() string {
	out := ""
	for i, c := range adapter.Channels() {
		if i > 0 {
			out += ", "
		}
		out += c
	}
	return out
}
