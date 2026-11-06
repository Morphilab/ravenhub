// Package worker runs the delivery engine: claims queued messages with
// FOR UPDATE SKIP LOCKED, renders and sends them through channel adapters,
// retries with backoff + jitter, expires by TTL and feeds the DLQ.
package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"ravenhub/internal/adapter"
	"ravenhub/internal/metrics"
	"ravenhub/internal/model"
	"ravenhub/internal/ratelimit"
	"ravenhub/internal/render"
	"ravenhub/internal/store"
)

// Worker is a pool of claiming loops plus a maintenance ticker.
type Worker struct {
	Store       *store.Store
	Concurrency int
	Lease       time.Duration
	// Retención: barrido horario de terminales/auditoría; 0 = off
	RetentionMessages time.Duration
	RetentionAudit    time.Duration
	destBuckets       *ratelimit.Registry
}

func New(st *store.Store, concurrency int, lease time.Duration) *Worker {
	if concurrency < 1 {
		concurrency = 1
	}
	return &Worker{Store: st, Concurrency: concurrency, Lease: lease, destBuckets: ratelimit.NewRegistry()}
}

// Run blocks until ctx is cancelled, spawning the pool and the maintainer.
func (w *Worker) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for i := 0; i < w.Concurrency; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			w.loop(ctx, n)
		}(i)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		w.maintain(ctx)
	}()
	wg.Wait()
	slog.Info("worker detenido")
}

// claimBatch is how many messages one worker grabs per transaction — it
// amortizes the SKIP LOCKED scan and keeps the drain fast under bursts.
const claimBatch = 16

func (w *Worker) loop(ctx context.Context, n int) {
	for {
		if ctx.Err() != nil {
			return
		}
		msgs, err := w.Store.ClaimMessages(ctx, claimBatch)
		switch {
		case err != nil:
			slog.Error("worker: error reclamando mensajes", "err", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
		case len(msgs) == 0:
			// cola vacía: dormir un poco (con cancelación inmediata)
			select {
			case <-ctx.Done():
				return
			case <-time.After(300 * time.Millisecond):
			}
		default:
			for i := range msgs {
				if ctx.Err() != nil {
					return
				}
				// latido de lease DURANTE la entrega: una entrega lenta (hasta
				// 20 destinos × deadline del proveedor) puede durar más que el
				// lease, y sin latido el reaper reencola el mensaje EN CURSO y
				// otro worker lo duplica. Mientras process trabaja, un ticker
				// refresca updated_at del mensaje en curso y del resto del
				// lote; si este worker muere, el latido muere con él y el
				// reaper recupera el lote (crash recovery intacto).
				hbCtx, stopBeat := context.WithCancel(ctx)
				go w.heartbeat(hbCtx, messageIDs(msgs[i:]))
				w.processSafe(ctx, msgs[i])
				stopBeat()
			}
		}
	}
}

func messageIDs(msgs []store.MessageRow) []string {
	ids := make([]string, 0, len(msgs))
	for _, m := range msgs {
		ids = append(ids, m.ID)
	}
	return ids
}

// heartbeat refresca updated_at del lote en curso mientras process trabaja.
// Intervalo lease/3 (mínimo 1s): la entrega más lenta posible sigue siendo
// más corta que el lease y el reaper jamás roba un lote vivo — solo lo hace
// si este latido muere con el worker.
func (w *Worker) heartbeat(ctx context.Context, ids []string) {
	if len(ids) == 0 {
		return
	}
	interval := w.Lease / 3
	if interval < time.Second {
		interval = time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := w.Store.TouchMessages(ctx, ids); err != nil {
				slog.Warn("latido de lease falló", "err", err)
			}
		}
	}
}

// maintain runs the reaper + expirer, refreshes queue gauges and sweeps
// retention hourly.
func (w *Worker) maintain(ctx context.Context) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	lastSweep := time.Now() // no barrer retención en el arranque
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n, err := w.Store.RequeueStale(ctx, w.Lease); err != nil {
				slog.Error("reaper: no se pudo reencolar", "err", err)
			} else if n > 0 {
				slog.Warn("reaper: mensajes 'delivering' con lease vencido vueltos a la cola", "n", n)
			}
			if n, err := w.SweepExpired(ctx); err != nil {
				slog.Error("expirer: error", "err", err)
			} else if n > 0 {
				// "puede degradarse, jamás en silencio": cada expiración deja
				// contador por prioridad + auditoría.
				slog.Warn("expirer: mensajes expirados por TTL (revisables y replayables)", "n", n)
			}
			if q, err := w.Store.CountByStatus(ctx, model.StatusQueued); err == nil {
				metrics.SetGauge("ravenhub_queue_depth", float64(q))
			}
			if d, err := w.Store.CountByStatus(ctx, model.StatusDead); err == nil {
				metrics.SetGauge("ravenhub_dlq_size", float64(d))
			}
			if time.Since(lastSweep) >= time.Hour {
				lastSweep = time.Now()
				w.sweepRetention(ctx)
			}
		}
	}
}

// SweepExpired retira los mensajes con TTL vencido y deja rastro (contador por
// prioridad + auditoría append-only). Exportado además para poder verificarlo
// sin esperar al ticker del mantenedor.
func (w *Worker) SweepExpired(ctx context.Context) (int, error) {
	expired, err := w.Store.ExpireMessages(ctx, w.Lease)
	if err != nil {
		return 0, err
	}
	if len(expired) == 0 {
		return 0, nil
	}
	w.auditExpired(ctx, expired)
	return len(expired), nil
}

// auditExpired deja rastro de cada pérdida por TTL: contador por prioridad +
// auditoría append-only en una sola sentencia (el expirer puede retirar
// cientos de golpe). Un mensaje expirado es una notificación que no llegó.
func (w *Worker) auditExpired(ctx context.Context, expired []store.ExpiredMessage) {
	entries := make([]store.AuditEntry, 0, len(expired))
	for i := range expired {
		e := &expired[i]
		metrics.Inc("ravenhub_expired_total", "priority", string(e.Priority))
		metrics.Inc("ravenhub_messages_total", "result", "expired")
		entries = append(entries, store.AuditEntry{
			ProjectID: &e.ProjectID,
			Action:    "message.expired",
			Subject:   e.ID,
			Detail:    map[string]any{"priority": string(e.Priority)},
		})
	}
	w.Store.InsertAuditBatch(ctx, entries)
}

// sweepRetention deletes terminal messages and audit rows past their
// retention window (: mensajes 30d, auditoría 90d, configurable).
func (w *Worker) sweepRetention(ctx context.Context) {
	if w.RetentionMessages > 0 {
		n, err := w.Store.DeleteOldMessages(ctx, time.Now().Add(-w.RetentionMessages))
		switch {
		case err != nil:
			slog.Error("retención: no se pudieron borrar mensajes viejos", "err", err)
		case n > 0:
			slog.Info("retención: mensajes terminales borrados", "n", n, "dias", int(w.RetentionMessages.Hours()/24))
		}
	}
	if w.RetentionAudit > 0 {
		n, err := w.Store.DeleteOldAudit(ctx, time.Now().Add(-w.RetentionAudit))
		switch {
		case err != nil:
			slog.Error("retención: no se pudo borrar auditoría vieja", "err", err)
		case n > 0:
			slog.Info("retención: registros de auditoría borrados", "n", n, "dias", int(w.RetentionAudit.Hours()/24))
		}
	}
}

// processSafe aísla un pánico del camino de entrega: sin esto, un nil
// inesperado en una rama nueva de adaptador tumbaba el binario ENTERO. El
// mensaje se reprograma con presupuesto y el hub sigue vivo.
func (w *Worker) processSafe(ctx context.Context, m store.MessageRow) {
	defer func() {
		if p := recover(); p != nil {
			slog.Error("pánico en la entrega; mensaje reprogramado", "id", m.ID, "panic", p)
			w.infraRetry(ctx, m, "pánico en la entrega", fmt.Errorf("panic: %v", p))
		}
	}()
	w.process(ctx, m)
}

// process delivers one claimed message to all its pending destinations.
func (w *Worker) process(ctx context.Context, m store.MessageRow) {
	var payload struct {
		To       []string       `json:"to"`
		Title    string         `json:"title"`
		Message  string         `json:"message"`
		Priority string         `json:"priority"`
		Tags     []string       `json:"tags"`
		Data     map[string]any `json:"data"`
	}
	if err := json.Unmarshal(m.Payload, &payload); err != nil {
		slog.Error("payload ilegible; mensaje a DLQ", "id", m.ID, "err", err)
		w.dlq(ctx, m, "payload ilegible")
		return
	}

	dests, missing, err := w.Store.ResolveDestinations(ctx, m.ProjectID, payload.To)
	if err != nil {
		w.infraRetry(ctx, m, "no se pudieron resolver destinos", err)
		return
	}
	// Sin destinos activos (todos borrados o un grupo que quedó vacío) no es
	// un envío exitoso: a DLQ, nunca "sent con 0 deliveries".
	if len(dests) == 0 {
		reason := "sin destinos activos antes de la entrega"
		if len(missing) > 0 {
			reason += ": " + fmt.Sprint(missing)
		}
		w.dlq(ctx, m, reason)
		return
	}
	// Borrado PARCIAL: quedan destinos pero alguno desapareció. Antes se
	// omitía en silencio y el mensaje podía acabar 'sent' sin rastro de la
	// entrega que nunca se intentó.
	if len(missing) > 0 {
		slog.Warn("destinos borrados desde el enqueue; se entrega al resto",
			"id", m.ID, "faltan", missing)
		w.Store.InsertAudit(ctx, &m.ProjectID, "", "message.destination_missing", m.ID,
			map[string]any{"missing": missing})
	}
	if err := w.Store.EnsureDeliveries(ctx, m.ID, dests); err != nil {
		w.infraRetry(ctx, m, "no se pudieron crear entregas", err)
		return
	}

	pending, err := w.Store.PendingDeliveries(ctx, m.ID)
	if err != nil {
		w.infraRetry(ctx, m, "no se pudieron leer entregas pendientes", err)
		return
	}

	norm := render.Normalized{
		Title:    payload.Title,
		Message:  payload.Message,
		Priority: payload.Priority,
		Tags:     payload.Tags,
		Data:     payload.Data,
	}

	// Digest: el payload guarda el ÚLTIMO mensaje; entregamos
	// colapsado ("⚠️ N mensajes iguales en la ventana: <último>").
	if m.IsDigest && m.AggregatedCount > 1 {
		norm.Message = fmt.Sprintf("⚠️ %d mensajes iguales en la ventana (último): %s",
			m.AggregatedCount, norm.Message)
	}

	var rescheduleAt time.Time
	for _, d := range pending {
		nextOpen, deferred, err := w.deliverOne(ctx, m, d, norm)
		if err != nil {
			slog.Error("entrega con error", "message", m.ID, "dest", d.DestName, "err", err)
		}
		if deferred {
			// pacing (rate limit) o quiet hours: entrega pospuesta, no fallida
			if nextOpen.After(rescheduleAt) {
				rescheduleAt = nextOpen
			}
		}
	}

	// Si ALGÚN destino quedó pospuesto, reprogramar al instante más lejano SIN
	// gastar reintentos (el pacing/quiet no es un fallo). El caso mixto
	// (1 enviado + 1 en quiet hours) no debe quemar un intento ni mandar
	// mensajes vivos a la DLQ.
	if rescheduleAt.After(time.Now()) {
		if err := w.Store.QuietRequeue(ctx, m.ID, rescheduleAt); err != nil {
			slog.Error("no se pudo reprogramar", "id", m.ID, "err", err)
		}
		return
	}
	w.finalize(ctx, m)
}

// deliverOne sends one pending delivery. deferred=true means the delivery was
// postponed (quiet hours o rate limit del destino) y nextOpen indica cuándo
// reintentarla; el caller decide el reencolado (nunca esta función).
func (w *Worker) deliverOne(ctx context.Context, m store.MessageRow, d store.DeliveryRow, norm render.Normalized) (time.Time, bool, error) {
	start := time.Now()

	// Destino borrado en vuelo → fallo permanente de esa entrega.
	if d.DestID == nil || d.AccountID == nil {
		err := w.Store.SetDeliveryFailed(ctx, d.ID, "destino o cuenta eliminada", false)
		metrics.Inc("ravenhub_deliveries_total", "channel", d.Channel, "result", "failed")
		return time.Time{}, false, err
	}

	// Quiet hours: normal/low/high se difieren; solo urgent atraviesa.
	qh, err := render.ParseQuietHours(d.QuietHours)
	if err != nil {
		// Fail-closed: una config rota no puede desactivar el silencio y
		// entregar de noche. La entrega queda failed visible.
		ferr := w.Store.SetDeliveryFailed(ctx, d.ID, "quiet_hours inválido: "+err.Error(), false)
		metrics.Inc("ravenhub_deliveries_total", "channel", d.Channel, "result", "failed")
		return time.Time{}, false, ferr
	}
	if qh != nil && qh.ActiveAt(time.Now()) && m.Priority != model.PriorityUrgent {
		return qh.NextOpen(time.Now()), true, nil
	}

	// Rate limit por destino (límites reales del proveedor).
	// Un rebote por pacing NO es un intento fallido: se reencola con el
	// reintento devuelto (QuietRequeue) + jitter, o la ráfaga quemaría el
	// presupuesto y mandaría mensajes vivos a la DLQ.
	acct, err := w.Store.GetAccount(ctx, *d.AccountID)
	if err != nil {
		// solo "no existe" es permanente: un error transitorio de la DB
		// (pool exhausto, blip de red) pospone SIN quemar intento.
		if !errors.Is(err, store.ErrNotFound) {
			_ = w.Store.NoteDeliveryRetryable(ctx, d.ID, "cuenta no disponible (transitorio): "+err.Error())
			metrics.Inc("ravenhub_deliveries_total", "channel", d.Channel, "result", "retryable")
			return time.Now().Add(5 * time.Second), true, err
		}
		_ = w.Store.SetDeliveryFailed(ctx, d.ID, "cuenta no encontrada", false)
		metrics.Inc("ravenhub_deliveries_total", "channel", d.Channel, "result", "failed")
		return time.Time{}, false, err
	}
	perMin, burst := w.destinationLimits(d, acct.Limits)
	b := w.destBuckets.Get(*d.DestID, perMin, burst)
	if !b.Allow() {
		wait := b.RetryAfter() + time.Duration(rand.Int64N(250))*time.Millisecond
		if wait > 2*time.Second { // cota defensiva: nunca esperar minutos por pacing
			wait = 2 * time.Second
		}
		_ = w.Store.NoteDeliveryRetryable(ctx, d.ID, fmt.Sprintf("rate limit destino: espera %s", wait.Round(time.Millisecond)))
		return time.Now().Add(wait), true, nil // pospuesto, no fallido
	}

	credsPlain, err := w.Store.DecryptCreds(acct)
	if err != nil {
		_ = w.Store.SetDeliveryFailed(ctx, d.ID, "creds no descifrables (¿master key correcta?)", false)
		metrics.Inc("ravenhub_deliveries_total", "channel", d.Channel, "result", "failed")
		return time.Time{}, false, err
	}

	adapt, err := adapter.Get(d.Channel)
	if err != nil {
		_ = w.Store.SetDeliveryFailed(ctx, d.ID, err.Error(), false)
		return time.Time{}, false, err
	}
	content, truncated := renderContent(norm)
	in := adapter.SendInput{
		Creds:   credsPlain,
		Target:  d.Target,
		Content: content,
	}
	res, err := adapt.Send(ctx, in)
	latency := int(time.Since(start).Milliseconds())
	switch {
	case err == nil:
		wasTruncated := truncated[d.Channel]
		_ = w.Store.SetDeliverySent(ctx, d.ID, res.ProviderMsgID, latency, wasTruncated)
		metrics.Inc("ravenhub_deliveries_total", "channel", d.Channel, "result", "sent")
		if wasTruncated {
			metrics.Inc("ravenhub_truncated_total", "channel", d.Channel)
		}
		metrics.Add("ravenhub_delivery_latency_ms_sum", float64(latency), "channel", d.Channel)
		metrics.Add("ravenhub_delivery_latency_ms_count", 1, "channel", d.Channel)
		return time.Time{}, false, nil
	case res.Retryable:
		_ = w.Store.NoteDeliveryRetryable(ctx, d.ID, err.Error())
		metrics.Inc("ravenhub_deliveries_total", "channel", d.Channel, "result", "retryable")
		if res.RetryAfter > 0 {
			// 429/Retry-After del proveedor (: "se respeta 429/Retry-
			// After con backoff propio"): esperar lo que pide el proveedor SIN
			// quemar intento — el pacing no es un fallo. El TTL acota el bucle.
			// Cota defensiva : proveedor loco no pospone más de 1h.
			wait := res.RetryAfter + time.Duration(rand.Int64N(250))*time.Millisecond
			if wait > time.Hour {
				wait = time.Hour
			}
			return time.Now().Add(wait), true, nil
		}
		return time.Time{}, false, nil
	default:
		// el render ya se hizo: si el canal truncó, la marca se persiste
		// también en el fallo.
		_ = w.Store.SetDeliveryFailed(ctx, d.ID, err.Error(), truncated[d.Channel])
		metrics.Inc("ravenhub_deliveries_total", "channel", d.Channel, "result", "failed")
		return time.Time{}, false, err
	}
}

// renderContent renders every channel form once and reports which channels had
// to truncate (se persiste en deliveries.truncated + contador por canal: un
// mensaje cortado por el límite del proveedor no puede quedar invisible).
func renderContent(norm render.Normalized) (adapter.Content, map[string]bool) {
	tg, tgTrunc := render.TelegramHTML(norm)
	subject, subjTrunc := render.EmailSubject(norm)
	plain, plainTrunc := render.Plain(norm)
	slack, slackTrunc := render.SlackText(norm)
	discord, discordTrunc := render.DiscordText(norm)
	return adapter.Content{
			Title:        norm.Title,
			TelegramHTML: tg,
			EmailSubject: subject,
			EmailHTML:    render.EmailHTML(norm),
			EmailText:    render.EmailText(norm),
			SlackText:    slack,
			DiscordText:  discord,
			Plain:        plain,
			JSON:         norm,
		}, map[string]bool{
			model.ChannelTelegram: tgTrunc,
			model.ChannelEmail:    subjTrunc,
			model.ChannelSlack:    slackTrunc,
			model.ChannelDiscord:  discordTrunc,
			model.ChannelLog:      plainTrunc,
		}
}

// destinationLimits resolves the per-destination send pacing: defaults per
// channel, overridables per account via limits {"per_minute": N, "burst": N}.
func (w *Worker) destinationLimits(d store.DeliveryRow, accountLimits json.RawMessage) (int, int) {
	defaults := map[string][2]int{
		model.ChannelTelegram: {60, 3}, // ≈1 msg/s por chat (empírico)
		model.ChannelEmail:    {30, 10},
		model.ChannelLog:      {6000, 100},
		model.ChannelWebhook:  {120, 20},
		model.ChannelDiscord:  {30, 5},
		model.ChannelSlack:    {60, 10},
	}
	lim, ok := defaults[d.Channel]
	if !ok {
		lim = [2]int{60, 10}
	}
	if len(accountLimits) > 0 {
		var o struct {
			PerMinute int `json:"per_minute"`
			Burst     int `json:"burst"`
		}
		if json.Unmarshal(accountLimits, &o) == nil {
			if o.PerMinute > 0 {
				lim[0] = o.PerMinute
			}
			if o.Burst > 0 {
				lim[1] = o.Burst
			}
		}
	}
	return lim[0], lim[1]
}

// infraRetry pospone un mensaje cuyo fallo es de infraestructura (resolver
// destinos, crear entregas, leer pendientes) con reintento en 1 min — pero con
// presupuesto: agotados los intentos de su prioridad, va a la DLQ en vez de
// girar en el bucle de +1min hasta que el TTL lo expire.
func (w *Worker) infraRetry(ctx context.Context, m store.MessageRow, stage string, err error) {
	slog.Error(stage, "id", m.ID, "err", err)
	if m.Attempts >= model.MaxAttempts(m.Priority) {
		w.dlq(ctx, m, stage+" (reintentos agotados)")
		return
	}
	next := time.Now().Add(time.Minute)
	if !next.Before(m.ExpiresAt) {
		w.dlq(ctx, m, stage+" (TTL agotado antes del siguiente reintento)")
		return
	}
	_ = w.Store.SetMessageRetry(ctx, m.ID, next)
}

// dlq manda el mensaje a la DLQ cerrando antes sus entregas pendientes y
// deja rastro completo: métrica, log Y auditoría message.dead con motivo
// (simetría con message.expired: una pérdida por reintentos agotados o por
// guardia de TTL no puede ser invisible en /admin/audit).
func (w *Worker) dlq(ctx context.Context, m store.MessageRow, reason string) {
	_ = w.Store.FailPendingDeliveries(ctx, m.ID, reason)
	_ = w.Store.SetMessageStatus(ctx, m.ID, model.StatusDead)
	metrics.Inc("ravenhub_messages_total", "result", "dead")
	w.Store.InsertAudit(ctx, &m.ProjectID, "", "message.dead", m.ID,
		map[string]any{"reason": reason, "attempts": m.Attempts})
	slog.Warn("mensaje a DLQ", "id", m.ID, "motivo", reason, "attempts", m.Attempts)
}

// finalize moves the message to its terminal state or schedules a retry.
func (w *Worker) finalize(ctx context.Context, m store.MessageRow) {
	pending, err := w.Store.PendingDeliveries(ctx, m.ID)
	if err != nil {
		slog.Error("finalize: error leyendo pendientes", "id", m.ID, "err", err)
		return
	}
	if len(pending) == 0 {
		status, err := w.Store.FinalizeMessage(ctx, m.ID)
		if err != nil {
			slog.Error("finalize: error", "id", m.ID, "err", err)
		}
		if status == "failed" {
			metrics.Inc("ravenhub_messages_total", "result", "failed")
		}
		return
	}
	if m.Attempts >= model.MaxAttempts(m.Priority) {
		w.dlq(ctx, m, "reintentos agotados (DLQ)")
		return
	}
	next := time.Now().Add(model.BackoffFor(m.Priority, m.Attempts))
	if !next.Before(m.ExpiresAt) {
		// El siguiente intento caería más allá del TTL: DLQ visible (gauge +
		// replay + auditoría) en vez de expiración silenciosa. Ocurre con
		// overrides de TTL por proyecto más cortos que el presupuesto de
		// reintentos; los defaults cumplen el invariante (TestRetryBudgetFitsTTL).
		w.dlq(ctx, m, "TTL agotado antes del siguiente reintento")
		return
	}
	if err := w.Store.SetMessageRetry(ctx, m.ID, next); err != nil {
		slog.Error("no se pudo reprogramar reintento", "id", m.ID, "err", err)
	}
}
