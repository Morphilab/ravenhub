package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"ravenhub/internal/metrics"
	"ravenhub/internal/model"
	"ravenhub/internal/render"
	"ravenhub/internal/store"
)

type notifyRequest struct {
	To       []string       `json:"to"`
	Priority string         `json:"priority"`
	Title    string         `json:"title"`
	Message  string         `json:"message"`
	Template string         `json:"template"`
	Data     map[string]any `json:"data"`
	Tags     []string       `json:"tags"`
	SendAt   string         `json:"send_at"` // RFC3339 opcional (difundir)
}

type notifyResponse struct {
	ID         string         `json:"id"`
	Duplicate  bool           `json:"duplicate,omitempty"`
	Aggregated bool           `json:"aggregated,omitempty"` // plegado en un digest abierto
	Status     string         `json:"status,omitempty"`
	Deliveries []deliveryView `json:"deliveries,omitempty"`
}

type deliveryView struct {
	DestName      string  `json:"destination"`
	Channel       string  `json:"channel"`
	Status        string  `json:"status"`
	Truncated     bool    `json:"truncated,omitempty"` // el contenido se cortó por el límite del canal
	Error         *string `json:"error,omitempty"`
	ProviderMsgID *string `json:"provider_msg_id,omitempty"`
	LatencyMs     *int    `json:"latency_ms,omitempty"`
}

// handleNotify implements POST /v1/notify — the single standardized call
// every agent uses.
func (s *Server) handleNotify(w http.ResponseWriter, r *http.Request) {
	id, ok := identityFrom(r.Context())
	if !ok {
		errorJSON(w, http.StatusUnauthorized, "token inválido")
		return
	}
	if id.IsAdmin() {
		// el admin puede notificar, pero un proyecto debe ser explícito →
		// exigimos ?project= para evitar notificaciones huérfanas
		errorJSON(w, http.StatusBadRequest, "usa un token de proyecto (el token admin no notifica)")
		return
	}

	var req notifyRequest
	if !decodeBody(w, r, model.PayloadMaxBytes, &req) {
		return
	}

	// 1) Validación estricta
	if len(req.To) == 0 {
		errorJSON(w, http.StatusBadRequest, "'to' es obligatorio (destinos o grupos del proyecto)")
		return
	}
	if len(req.To) > 20 {
		errorJSON(w, http.StatusBadRequest, "'to' excede 20 destinos por mensaje")
		return
	}
	trimmed := make([]string, 0, len(req.To))
	for _, d := range req.To {
		if t := strings.TrimSpace(d); t != "" {
			trimmed = append(trimmed, t)
		}
	}
	if len(trimmed) == 0 {
		errorJSON(w, http.StatusBadRequest, "'to' es obligatorio (destinos o grupos del proyecto)")
		return
	}
	req.To = trimmed
	if utf8.RuneCountInString(req.Title) > model.TitleMaxLen {
		errorJSON(w, http.StatusBadRequest, fmt.Sprintf("title excede %d caracteres", model.TitleMaxLen))
		return
	}
	if utf8.RuneCountInString(req.Message) > model.MessageMaxLen {
		errorJSON(w, http.StatusBadRequest, fmt.Sprintf("message excede %d caracteres", model.MessageMaxLen))
		return
	}
	priority, err := model.ParsePriority(req.Priority)
	if err != nil {
		errorJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	sendAt, err := parseSendAt(req.SendAt)
	if err != nil {
		errorJSON(w, http.StatusBadRequest, err.Error())
		return
	}

	// 2) Scopes del token — también sobre lo que añaden las
	//    reglas de enrutamiento: un token acotado a ["a"] no puede
	//    alcanzar "oncall" vía tags.
	for _, d := range req.To {
		if !id.Scopes.AllowsDestination(d) {
			errorJSON(w, http.StatusForbidden, "destino no permitido para este token: "+d)
			return
		}
	}
	if !id.Scopes.AllowsPriority(priority) {
		errorJSON(w, http.StatusForbidden,
			fmt.Sprintf("prioridad %s excede el máximo del token (%s)", priority, id.Scopes.MaxPriority))
		return
	}

	// 3) Rate limit de entrada por proyecto. Clave por
	// ProjectID (el nombre puede cambiar; el ID no).
	b := s.Intake.Get("intake:"+*id.ProjectID, s.Cfg.RatePerMin, s.Cfg.RateBurst)
	if !b.Allow() {
		// Contrapresión/limitación con métrica: sin esto no se podía responder
		// "¿cuántas notificaciones se descartaron?" (fail-fast: solo métrica,
		// nada de escrituras en DB justo durante la sobrecarga).
		metrics.Inc("ravenhub_shed_total", "scope", "intake", "code", "429")
		w.Header().Set("Retry-After", fmt.Sprintf("%d", int(b.RetryAfter().Seconds())+1))
		errorJSON(w, http.StatusTooManyRequests, "rate limit de entrada; reintenta más tarde")
		return
	}

	// 4) Resolver lo que pidió el agente dentro del namespace del proyecto
	dests, missing, err := s.Store.ResolveDestinations(r.Context(), *id.ProjectID, req.To)
	if err != nil {
		errorJSON(w, http.StatusInternalServerError, "error resolviendo destinos")
		return
	}
	if len(missing) > 0 {
		errorJSON(w, http.StatusBadRequest, "destinos desconocidos: "+fmt.Sprint(missing))
		return
	}

	// 5) Reglas de enrutamiento por tags/prioridad (: priority=urgent
	//    → oncall). Solo AÑADEN destinos; el consumidor no decide el canal.
	finalTo := append([]string(nil), req.To...)
	added, err := s.routingAdditions(r, *id.ProjectID, &req, priority)
	if err != nil {
		errorJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	if len(added) > 0 {
		for _, d := range added {
			if !id.Scopes.AllowsDestination(d) {
				errorJSON(w, http.StatusForbidden, "destino de regla no permitido para este token: "+d)
				return
			}
		}
		extra, extraMissing, err := s.Store.ResolveDestinations(r.Context(), *id.ProjectID, added)
		if err != nil {
			errorJSON(w, http.StatusInternalServerError, "error resolviendo destinos de reglas")
			return
		}
		if len(extraMissing) > 0 {
			errorJSON(w, http.StatusInternalServerError,
				"las reglas de enrutamiento referencian destinos desconocidos: "+fmt.Sprint(extraMissing))
			return
		}
		dests = append(dests, extra...)
		finalTo = appendUnique(finalTo, added...)
	}
	finalTo = dedupeNames(finalTo)
	if len(dests) == 0 {
		errorJSON(w, http.StatusBadRequest, "no queda ningún destino activo para "+fmt.Sprint(req.To))
		return
	}
	// Un grupo puede expandir a muchos miembros; acotar el fan-out evita que
	// un mensaje monopolice workers y multiplique el coste por tenant.
	if len(dests) > 200 {
		errorJSON(w, http.StatusBadRequest, fmt.Sprintf("el mensaje resuelve %d destinos (máximo 200)", len(dests)))
		return
	}

	// 6) Contrapresión: crecer degrada, nunca crashea.
	// Se aplica global y por proyecto para que un tenant hambriento no
	// degrade a todos. La profundidad cuenta queued+delivering: contar solo
	// 'queued' dejaría fuera lo que los workers ya tienen en vuelo y
	// subestimaría la saturación.
	depth, err := s.Store.CountPending(r.Context())
	if err != nil {
		errorJSON(w, http.StatusInternalServerError, "error de cola")
		return
	}
	metrics.SetGauge("ravenhub_queue_depth", float64(depth))
	if depth >= s.Cfg.HardLimit {
		metrics.Inc("ravenhub_shed_total", "scope", "global", "code", "503")
		w.Header().Set("Retry-After", "30")
		errorJSON(w, http.StatusServiceUnavailable, "cola saturada (hard limit); reintenta más tarde")
		return
	}
	if pdepth, err := s.Store.CountQueuedForProject(r.Context(), *id.ProjectID); err == nil {
		// Cuota dura por proyecto: sin esto un tenant podía llenar el hard
		// global con programados/high/urgent y bloquear a todos.
		if pdepth >= s.Cfg.ProjectHardLimit {
			metrics.Inc("ravenhub_shed_total", "scope", "project", "code", "503")
			w.Header().Set("Retry-After", "30")
			errorJSON(w, http.StatusServiceUnavailable, "cola del proyecto saturada (hard limit); reintenta más tarde")
			return
		}
		if pdepth >= s.Cfg.SoftLimit && priority.Rank() <= model.PriorityNormal.Rank() {
			metrics.Inc("ravenhub_shed_total", "scope", "project", "code", "429")
			w.Header().Set("Retry-After", "10")
			errorJSON(w, http.StatusTooManyRequests, "cola del proyecto saturada: low/normal rechazados (shedding); urgent/high pasan")
			return
		}
	}
	if depth >= s.Cfg.SoftLimit && priority.Rank() <= model.PriorityNormal.Rank() {
		metrics.Inc("ravenhub_shed_total", "scope", "global", "code", "429")
		w.Header().Set("Retry-After", "10")
		errorJSON(w, http.StatusTooManyRequests, "cola saturada: low/normal rechazados (shedding); urgent/high pasan")
		return
	}

	// 7) Plantilla server-side con fallback a texto plano
	norm := render.Normalized{Title: req.Title, Message: req.Message, Priority: string(priority), Tags: req.Tags, Data: req.Data}
	if req.Template != "" {
		body, err := s.Store.GetTemplate(r.Context(), *id.ProjectID, req.Template)
		if err == nil {
			if rendered, terr := render.ApplyTemplate(body, norm); terr == nil {
				norm.Message = rendered
			} else {
				// render falló → texto plano (mejor notificar feo que no notificar)
				slog.Warn("plantilla con error de render; fallback a texto plano",
					"template", req.Template, "err", terr)
			}
		} else if errors.Is(err, store.ErrNotFound) {
			slog.Warn("plantilla inexistente; fallback a texto plano", "template", req.Template)
		}
	}

	// 8) Idempotencia + send_at + agregación + insert
	var dedup *string
	if key := r.Header.Get("Idempotency-Key"); key != "" {
		if len(key) > 128 {
			errorJSON(w, http.StatusBadRequest, "Idempotency-Key excede 128 caracteres")
			return
		}
		dedup = &key
	}
	payloadJSON, _ := json.Marshal(map[string]any{
		"to":       finalTo,
		"title":    norm.Title,
		"message":  norm.Message,
		"priority": string(priority),
		"tags":     norm.Tags,
		"data":     norm.Data,
	})
	// Postgres JSONB rechaza \u0000 (SQLSTATE 22P05) → 500. Es un 400.
	if bytes.Contains(payloadJSON, []byte{0}) {
		errorJSON(w, http.StatusBadRequest, "el payload contiene NUL (\\u0000), no soportado")
		return
	}
	ttl := model.TTLFor(priority, id.ProjectOpts)
	opts := store.InsertOpts{SendAt: sendAt, NextAttemptAt: time.Now()}
	if sendAt != nil && sendAt.After(opts.NextAttemptAt) {
		opts.NextAttemptAt = *sendAt
	}
	// Anti-spam: solo low/normal y solo si el proyecto opta
	// con options.digest_window. high/urgent jamás se agregan.
	if window, on := model.DigestWindow(id.ProjectOpts); on && sendAt == nil && priority.Rank() <= model.PriorityNormal.Rank() {
		key := model.AggregationKey(norm.Title, norm.Tags, finalTo)
		opts.AggregationKey = &key
		opts.Window = window
		next := time.Now().Add(window)
		if sendAt != nil && sendAt.After(next) {
			next = *sendAt
		}
		opts.NextAttemptAt = next
	}
	// El TTL debe cubrir el programado: una notificación con send_at futuro no
	// debe expirar antes de su hora.
	if opts.NextAttemptAt.After(time.Now()) {
		ttl += time.Until(opts.NextAttemptAt) + time.Minute
	}

	res, err := s.Store.EnqueueMessage(r.Context(), *id.ProjectID, id.ID, payloadJSON, priority, dedup, ttl, opts)
	if err != nil {
		slog.Error("no se pudo encolar", "err", err)
		errorJSON(w, http.StatusInternalServerError, "no se pudo encolar")
		return
	}
	switch {
	case res.Duplicate:
		metrics.Inc("ravenhub_messages_total", "result", "duplicate")
	case res.Aggregated:
		metrics.Inc("ravenhub_messages_total", "result", "aggregated")
		s.Store.InsertAudit(r.Context(), id.ProjectID, id.Prefix, "notify.aggregated",
			res.ID, map[string]any{"to": req.To, "priority": string(priority), "into": res.ID, "count": res.Count})
	default:
		metrics.Inc("ravenhub_messages_total", "result", "accepted")
		s.Store.InsertAudit(r.Context(), id.ProjectID, id.Prefix, "notify",
			res.ID, map[string]any{"to": req.To, "priority": string(priority)})
	}

	// 9) sync=true: esperar el resultado real (para smoke tests). Mismos
	// scopes de destinos que GET /v1/messages/{id}: una Idempotency-Key es
	// por proyecto, no por token — un token acodado que reusa la clave de
	// otro no debe ver entregas fuera de su scope.
	if r.URL.Query().Get("sync") == "true" {
		if row, err := s.waitTerminal(r, res.ID); err == nil {
			if messageVisible(id.Scopes.Destinations, row.Payload) {
				dels, _ := s.Store.ListDeliveries(r.Context(), res.ID)
				writeJSON(w, http.StatusOK, notifyResponse{
					ID: res.ID, Duplicate: res.Duplicate, Aggregated: res.Aggregated,
					Status: row.Status, Deliveries: toDeliveryViews(filterDeliveries(id.Scopes.Destinations, dels)),
				})
				return
			}
			writeJSON(w, http.StatusOK, notifyResponse{ID: res.ID, Duplicate: res.Duplicate, Aggregated: res.Aggregated})
			return
		}
	}

	writeJSON(w, http.StatusAccepted, notifyResponse{ID: res.ID, Duplicate: res.Duplicate, Aggregated: res.Aggregated})
}

// routingAdditions evaluates the enabled rules of the project and returns the
// destination names they add. Una regla con stop_processing=true
// corta la cadena. El match por payload es un subconjunto sobre data.
func (s *Server) routingAdditions(r *http.Request, projectID string, req *notifyRequest, priority model.Priority) ([]string, error) {
	rules, err := s.Store.ListRoutingRules(r.Context(), projectID)
	if err != nil {
		return nil, errors.New("error leyendo reglas de enrutamiento")
	}
	var added []string
	for _, rule := range rules {
		if !rule.Enabled {
			continue
		}
		if rule.MatchPriority != "" {
			minP, err := model.ParsePriority(rule.MatchPriority)
			if err != nil {
				slog.Warn("regla con match_priority inválido; se omite", "rule", rule.Name, "err", err)
				continue
			}
			if priority.Rank() < minP.Rank() {
				continue
			}
		}
		if !allTagsPresent(rule.MatchTags, req.Tags) {
			continue
		}
		if !payloadSubset(rule.MatchPayload, req.Data) {
			continue
		}
		added = append(added, rule.Destinations...)
		if rule.StopProcessing {
			break
		}
	}
	return added, nil
}

// allTagsPresent reports whether every required tag is in the message tags.
func allTagsPresent(required, have []string) bool {
	for _, want := range required {
		found := false
		for _, got := range have {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// payloadSubset reports whether sub is a subset of data (values must match).
func payloadSubset(sub, data map[string]any) bool {
	for k, v := range sub {
		got, ok := data[k]
		if !ok || !jsonEqual(got, v) {
			return false
		}
	}
	return true
}

func jsonEqual(a, b any) bool {
	aj, _ := json.Marshal(a)
	bj, _ := json.Marshal(b)
	return string(aj) == string(bj)
}

// parseSendAt validates the optional RFC3339 send_at. Un valor en
// el pasado se acepta y se entrega ya (misma semántica que "no programo").
// Cota : máximo 30d futuro — sin cota un token válido
// anclaba cola/TTL indefinidamente.
func parseSendAt(raw string) (*time.Time, error) {
	if raw == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return nil, fmt.Errorf("send_at debe ser RFC3339 (ej: %s)", time.Now().UTC().Format(time.RFC3339))
	}
	if t.After(time.Now().Add(30 * 24 * time.Hour)) {
		return nil, fmt.Errorf("send_at excede el máximo de 30 días futuros")
	}
	return &t, nil
}

// appendUnique appends names not already present.
func appendUnique(dst []string, names ...string) []string {
	for _, n := range names {
		dup := false
		for _, d := range dst {
			if d == n {
				dup = true
				break
			}
		}
		if !dup {
			dst = append(dst, n)
		}
	}
	return dst
}

// dedupeNames removes duplicated names preserving order.
func dedupeNames(names []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(names))
	for _, n := range names {
		if seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out
}

// waitTerminal polls the message until it reaches a terminal state or the
// sync deadline passes.
func (s *Server) waitTerminal(r *http.Request, id string) (store.MessageRow, error) {
	deadline := time.Now().Add(s.Cfg.SyncWait)
	for time.Now().Before(deadline) {
		identity, _ := identityFrom(r.Context())
		row, err := s.Store.GetMessage(r.Context(), projectIDOf(identity), id)
		if err != nil {
			return store.MessageRow{}, err
		}
		switch row.Status {
		case model.StatusSent, model.StatusFailed, model.StatusDead, model.StatusExpired:
			return row, nil
		}
		select {
		case <-r.Context().Done():
			return store.MessageRow{}, r.Context().Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return store.MessageRow{}, errors.New("timeout esperando terminal")
}

func (s *Server) messageToView(m store.MessageRow) map[string]any {
	return map[string]any{
		"id":               m.ID,
		"project_id":       m.ProjectID,
		"payload":          m.Payload,
		"priority":         m.Priority,
		"status":           m.Status,
		"expires_at":       m.ExpiresAt,
		"attempts":         m.Attempts,
		"next_attempt_at":  m.NextAttemptAt,
		"created_at":       m.CreatedAt,
		"send_at":          m.SendAt,
		"aggregated_count": m.AggregatedCount,
		"is_digest":        m.IsDigest,
	}
}

// messageToViewFor aplica el scope de destinos también al campo `to` del
// payload: un token limitado a A que lee un mensaje A+B ve el contenido (es
// destinatario legítimo) pero no la lista completa de destinos del proyecto.
// Sin scopes (o admin) devuelve la vista íntegra.
func (s *Server) messageToViewFor(id store.TokenRow, m store.MessageRow) map[string]any {
	view := s.messageToView(m)
	if len(id.Scopes.Destinations) == 0 {
		return view
	}
	var p map[string]any
	if json.Unmarshal(m.Payload, &p) != nil {
		return view
	}
	rawTo, ok := p["to"].([]any)
	if !ok {
		return view
	}
	filtered := make([]any, 0, len(rawTo))
	for _, v := range rawTo {
		name, ok := v.(string)
		if ok && id.Scopes.AllowsDestination(name) {
			filtered = append(filtered, name)
		}
	}
	p["to"] = filtered
	view["payload"] = p
	return view
}

func toDeliveryViews(rows []store.DeliveryRow) []deliveryView {
	out := make([]deliveryView, 0, len(rows))
	for _, d := range rows {
		out = append(out, deliveryView{
			DestName: d.DestName, Channel: d.Channel, Status: d.Status, Truncated: d.Truncated,
			Error: d.Error, ProviderMsgID: d.ProviderMsgID, LatencyMs: d.LatencyMs,
		})
	}
	return out
}
