// Package e2e runs the full stack (API + workers) against a real Postgres to
// verify the end-to-end acceptance criteria. Tests skip without
// RAVENHUB_TEST_DSN (scripts/test-integration.sh provisions one).
package e2e

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"ravenhub/internal/api"
	"ravenhub/internal/config"
	"ravenhub/internal/crypto"
	"ravenhub/internal/db"
	"ravenhub/internal/importer"
	"ravenhub/internal/model"
	"ravenhub/internal/store"
	"ravenhub/internal/worker"

	"github.com/jackc/pgx/v5/pgxpool"
)

const adminToken = "rva_admin_e2e0000000000000000000000"

var (
	poolOnce sync.Once
	pool     *pgxpool.Pool
	poolErr  error
)

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("RAVENHUB_TEST_DSN")
	if dsn == "" {
		t.Skip("RAVENHUB_TEST_DSN no definido; tests de integración omitidos")
	}
	poolOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		pool, poolErr = db.Open(ctx, dsn)
		if poolErr == nil {
			poolErr = db.Migrate(ctx, pool)
		}
	})
	if poolErr != nil {
		t.Fatalf("preparando DB: %v", poolErr)
	}
	return pool
}

type stack struct {
	URL      string
	Admin    string
	Store    *store.Store
	ts       *httptest.Server
	cancel   func()
	project  string
	projID   string
	token    string
	teardown func()
}

// newStack boots API (+worker opcional) con un proyecto fresco y su token.
func newStack(t *testing.T, withWorker bool, mutate func(*config.Config)) *stack {
	return newStackOpts(t, withWorker, mutate, nil)
}

// newStackOpts igual, pero con opciones de proyecto (p.ej. digest_window).
func newStackOpts(t *testing.T, withWorker bool, mutate func(*config.Config), projectOptions map[string]any) *stack {
	t.Helper()
	st := &store.Store{Pool: testPool(t), MasterKey: crypto.MasterKey("e2e-master-key")}

	cfg := &config.Config{
		DSN:              "unused",
		Listen:           ":0",
		MasterKey:        "e2e-master-key",
		AdminToken:       adminToken,
		Workers:          4,
		SoftLimit:        5000,
		HardLimit:        7500,
		ProjectHardLimit: 5000,
		RatePerMin:       100000,
		RateBurst:        5000,
		Lease:            2 * time.Minute,
		SyncWait:         3 * time.Second,
		LogLevel:         "error",
	}
	if mutate != nil {
		mutate(cfg)
	}

	ctx, cancel := context.WithCancel(context.Background())
	if err := st.EnsureAdminToken(ctx, adminToken); err != nil {
		t.Fatalf("admin token: %v", err)
	}

	srv := api.NewServer(cfg, st, nil)
	ts := httptest.NewServer(srv.Routes())

	s := &stack{URL: ts.URL, Admin: adminToken, Store: st, ts: ts, cancel: cancel}

	if withWorker {
		w := worker.New(st, cfg.Workers, cfg.Lease)
		go w.Run(ctx)
	}

	// proyecto fresco + token + cuenta log + destino
	s.project = fmt.Sprintf("e2e-%s-%d", t.Name(), time.Now().UnixNano()%1000000)
	body := map[string]any{"name": s.project, "token_name": "main"}
	if projectOptions != nil {
		body["options"] = projectOptions
	}
	var resp struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	s.do(t, http.MethodPost, "/admin/projects", body, &resp, s.Admin)
	s.projID, s.token = resp.ID, resp.Token

	var acct struct {
		ID string `json:"id"`
	}
	s.do(t, http.MethodPost, "/admin/projects/"+s.projID+"/accounts", map[string]any{
		"channel": "log", "label": "log-acct", "creds": map[string]any{},
	}, &acct, s.Admin)
	s.do(t, http.MethodPost, "/admin/projects/"+s.projID+"/destinations", map[string]any{
		"name": "log:e2e", "account_id": acct.ID, "target": map[string]any{},
	}, nil, s.Admin)

	s.teardown = func() {
		cancel()
		ts.Close()
		// mismo camino que la API: anonimiza la auditoría del proyecto antes
		// de borrarlo (el teardown por SQL directo dejaba huérfanas).
		_ = st.DeleteProject(context.Background(), s.projID)
	}
	t.Cleanup(s.teardown)
	return s
}

func (s *stack) do(t *testing.T, method, path string, body any, out any, token string) *http.Response {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req, err := http.NewRequest(method, s.URL+path, &buf)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	if out != nil {
		_ = json.NewDecoder(resp.Body).Decode(out)
	}
	return resp
}

func (s *stack) notify(t *testing.T, token string, body map[string]any, headers map[string]string) (*http.Response, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	_ = json.NewEncoder(&buf).Encode(body)
	req, _ := http.NewRequest(http.MethodPost, s.URL+"/v1/notify", &buf)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

func (s *stack) waitForStatus(t *testing.T, id string, statuses ...string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		var msg map[string]any
		// admin: el harness necesita ver el estado/payload completo; la
		// visibilidad de un token acotado se prueba aparte (scoped reads).
		s.do(t, http.MethodGet, "/v1/messages/"+id, nil, &msg, s.Admin)
		if st, _ := msg["status"].(string); contains(statuses, st) {
			return msg
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("el mensaje %s no alcanzó %v a tiempo", id, statuses)
	return nil
}

// metricValue lee el valor actual de una serie de /metrics (0 si la serie aún
// no existe: registro perezoso). Los tests de métricas asertan por DELTA —
// el registro es global al binario y las series de tests previos harían
// tautológico cualquier strings.Contains.
func metricValue(t *testing.T, url, series string) float64 {
	t.Helper()
	resp, err := http.Get(url + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, series+" ") {
			v, _ := strconv.ParseFloat(strings.TrimSpace(line[len(series):]), 64)
			return v
		}
	}
	return 0
}

func contains(list []string, s string) bool {
	for _, e := range list {
		if e == s {
			return true
		}
	}
	return false
}

// Criterio 1: un agente notifica con una llamada y llega (canal log real). ---
func TestNotifyE2ELogChannel(t *testing.T) {
	s := newStack(t, true, nil)

	resp, out := s.notify(t, s.token, map[string]any{
		"to":       []string{"log:e2e"},
		"title":    "Error en pipeline",
		"message":  "El agente X falló",
		"priority": "high",
		"tags":     []string{"error"},
		"data":     map[string]any{"exit_code": 137},
	}, nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("esperaba 202, got %d: %v", resp.StatusCode, out)
	}
	id, _ := out["id"].(string)
	if id == "" {
		t.Fatal("respuesta sin id")
	}

	msg := s.waitForStatus(t, id, model.StatusSent)
	dels, _ := msg["deliveries"].([]any)
	if len(dels) != 1 {
		t.Fatalf("esperaba 1 entrega, got %d", len(dels))
	}
	d := dels[0].(map[string]any)
	if d["status"] != "sent" || d["channel"] != "log" {
		t.Fatalf("entrega incorrecta: %v", d)
	}

	// el token del proyecto ve SU mensaje; la auditoría registró el envío
	var audit []map[string]any
	s.do(t, http.MethodGet, "/admin/audit?project_id="+s.projID, nil, &audit, s.Admin)
	if len(audit) == 0 {
		t.Fatal("auditoría vacía tras notificar")
	}
}

// Criterio: reenvío con mismo Idempotency-Key → mismo id, un solo envío. ----
func TestIdempotency(t *testing.T) {
	s := newStack(t, true, nil)
	key := "op-42-única"

	r1, o1 := s.notify(t, s.token, map[string]any{
		"to": []string{"log:e2e"}, "title": "t", "message": "m",
	}, map[string]string{"Idempotency-Key": key})
	if r1.StatusCode != http.StatusAccepted {
		t.Fatalf("primera: %d", r1.StatusCode)
	}
	r2, o2 := s.notify(t, s.token, map[string]any{
		"to": []string{"log:e2e"}, "title": "t", "message": "m",
	}, map[string]string{"Idempotency-Key": key})
	if r2.StatusCode != http.StatusAccepted {
		t.Fatalf("segunda: %d", r2.StatusCode)
	}
	if o1["id"] != o2["id"] {
		t.Fatalf("ids distintos: %v vs %v", o1["id"], o2["id"])
	}
	if dup, _ := o2["duplicate"].(bool); !dup {
		t.Fatal("la segunda no se marcó como duplicada")
	}
	var n int
	if err := s.Store.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM messages WHERE project_id=$1 AND dedup_key=$2`, s.projID, key).Scan(&n); err != nil || n != 1 {
		t.Fatalf("esperaba 1 fila de mensaje, hay %d (err=%v)", n, err)
	}
}

// Criterio: revocar un token invalida al instante. --------------------------
func TestRevocationInstant(t *testing.T) {
	s := newStack(t, false, nil)

	var tok struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	s.do(t, http.MethodPost, "/admin/projects/"+s.projID+"/tokens", map[string]any{
		"name": "revocable",
	}, &tok, s.Admin)

	// el token nuevo funciona (202 con cola parada también acepta)
	if resp, _ := s.notify(t, tok.Token, map[string]any{"to": []string{"log:e2e"}, "title": "t", "message": "m"}, nil); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("antes de revocar debería aceptar: %d", resp.StatusCode)
	}
	s.do(t, http.MethodPost, "/admin/tokens/"+tok.ID+"/revoke", nil, nil, s.Admin)

	resp, _ := s.notify(t, tok.Token, map[string]any{"to": []string{"log:e2e"}, "title": "t", "message": "m"}, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("tras revocar esperaba 401 inmediato, got %d", resp.StatusCode)
	}
}

// Aislamiento cross-tenant. ----------------------------------
func TestCrossTenantIsolation(t *testing.T) {
	s := newStack(t, false, nil)

	// mensaje de A
	r, out := s.notify(t, s.token, map[string]any{"to": []string{"log:e2e"}, "title": "secreto", "message": "de A"}, nil)
	if r.StatusCode != http.StatusAccepted {
		t.Fatalf("setup: %d", r.StatusCode)
	}
	idA := out["id"].(string)

	// proyecto B con su propio token
	var projB struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	s.do(t, http.MethodPost, "/admin/projects", map[string]any{
		"name": s.project + "-B", "token_name": "main",
	}, &projB, s.Admin)
	t.Cleanup(func() {
		_, _ = s.Store.Pool.Exec(context.Background(), `DELETE FROM projects WHERE id=$1`, projB.ID)
	})

	// B no puede usar el destino de A (namespace distinto → desconocido)
	if resp, _ := s.notify(t, projB.Token, map[string]any{"to": []string{"log:e2e"}, "title": "x", "message": "y"}, nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("B usando destino de A: esperaba 400, got %d", resp.StatusCode)
	}
	// B no puede leer el mensaje de A
	req, _ := http.NewRequest(http.MethodGet, s.URL+"/v1/messages/"+idA, nil)
	req.Header.Set("Authorization", "Bearer "+projB.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("B leyendo mensaje de A: esperaba 404, got %d", resp.StatusCode)
	}
}

// Scopes: destinos permitidos y prioridad máxima. ------------
func TestScopesEnforcement(t *testing.T) {
	s := newStack(t, false, nil)

	var tok struct {
		Token string `json:"token"`
	}
	s.do(t, http.MethodPost, "/admin/projects/"+s.projID+"/tokens", map[string]any{
		"name": "acotado", "destinations": []string{"log:e2e"}, "max_priority": "low",
	}, &tok, s.Admin)

	// urgent excede el techo del token
	if resp, _ := s.notify(t, tok.Token, map[string]any{
		"to": []string{"log:e2e"}, "title": "t", "message": "m", "priority": "urgent",
	}, nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("urgent con techo low: esperaba 403, got %d", resp.StatusCode)
	}
	// destino fuera de scope
	if resp, _ := s.notify(t, tok.Token, map[string]any{
		"to": []string{"otro:destino"}, "title": "t", "message": "m", "priority": "low",
	}, nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("destino fuera de scope: esperaba 403, got %d", resp.StatusCode)
	}
}

// Contrapresión: crecer degrada (429/503), nunca crashea. ------
func TestBackpressure(t *testing.T) {
	s := newStack(t, false, func(c *config.Config) { // SIN worker: la cola se llena
		c.SoftLimit, c.HardLimit = 3, 6
	})

	accepted, shed, hard := 0, 0, 0
	for i := 0; i < 12; i++ {
		prio := "normal"
		if i%2 == 1 {
			prio = "urgent"
		}
		resp, _ := s.notify(t, s.token, map[string]any{
			"to": []string{"log:e2e"}, "title": "p", "message": "m", "priority": prio,
		}, nil)
		switch resp.StatusCode {
		case http.StatusAccepted:
			accepted++
		case http.StatusTooManyRequests:
			if resp.Header.Get("Retry-After") == "" {
				t.Fatal("429 sin Retry-After")
			}
			shed++
		case http.StatusServiceUnavailable:
			hard++
		default:
			t.Fatalf("código inesperado %d en iteración %d", resp.StatusCode, i)
		}
	}
	if accepted != 6 || hard == 0 || shed == 0 {
		t.Fatalf("contrapresión rota: accepted=%d shed=%d hard=%d", accepted, shed, hard)
	}
}

// la cuota dura por proyecto bloquea también high/urgent antes de
// que un tenant llene el hard global y deje sin servicio a los demás.
func TestProjectHardLimitBlocksAllPriorities(t *testing.T) {
	s := newStack(t, false, func(c *config.Config) {
		c.SoftLimit, c.HardLimit, c.ProjectHardLimit = 100, 1000, 1
	})
	resp, _ := s.notify(t, s.token, map[string]any{
		"to": []string{"log:e2e"}, "title": "a", "message": "m", "priority": "high",
	}, nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("primer high: %d", resp.StatusCode)
	}
	resp, _ = s.notify(t, s.token, map[string]any{
		"to": []string{"log:e2e"}, "title": "b", "message": "m", "priority": "urgent",
	}, nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("urgent con cuota de proyecto llena: esperaba 503, got %d", resp.StatusCode)
	}
}

// Criterio: ráfaga de estrés de 1.000 mensajes — nada se pierde, memoria acotada.
func TestBurst1000(t *testing.T) {
	if testing.Short() {
		t.Skip("ráfaga omitida en -short")
	}
	s := newStack(t, true, func(c *config.Config) { c.Workers = 8 })

	const total = 1000
	codes := make(chan int, total)
	var wg sync.WaitGroup
	for w := 0; w < 20; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			client := &http.Client{Timeout: 15 * time.Second}
			for i := 0; i < total/20; i++ {
				body, _ := json.Marshal(map[string]any{
					"to": []string{"log:e2e"}, "title": "ráfaga", "message": fmt.Sprintf("msg %d-%d", w, i),
				})
				req, _ := http.NewRequest(http.MethodPost, s.URL+"/v1/notify", bytes.NewReader(body))
				req.Header.Set("Authorization", "Bearer "+s.token)
				req.Header.Set("Content-Type", "application/json")
				resp, err := client.Do(req)
				if err != nil {
					codes <- -1
					continue
				}
				resp.Body.Close()
				codes <- resp.StatusCode
			}
		}(w)
	}
	wg.Wait()
	close(codes)

	accepted := 0
	for c := range codes {
		if c == http.StatusAccepted {
			accepted++
		}
	}
	if accepted != total {
		t.Fatalf("perdidas en la admisión: %d/%d aceptadas", accepted, total)
	}

	// todos alcanzan estado terminal 'sent' (canal log, sin errores);
	// 120s cubre también la sobrecarga del race detector
	deadline := time.Now().Add(120 * time.Second)
	for time.Now().Before(deadline) {
		var stats map[string]any
		// stats POR PROYECTO: el conteo global suma el historial de TODOS los
		// tests de la corrida y el drenaje podía quedar "verde" por otros
		s.do(t, http.MethodGet, "/admin/stats?project_id="+s.projID, nil, &stats, s.Admin)
		if n, _ := stats["sent"].(float64); int(n) >= total {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	var stats map[string]any
	s.do(t, http.MethodGet, "/admin/stats?project_id="+s.projID, nil, &stats, s.Admin)
	t.Fatalf("la ráfaga no se drenó a tiempo: %v", stats)
}

// Plantillas con fallback. --------------------------------------
func TestTemplateFallback(t *testing.T) {
	s := newStack(t, true, func(c *config.Config) { c.SyncWait = 5 * time.Second })

	s.do(t, http.MethodPost, "/admin/projects/"+s.projID+"/templates", map[string]any{
		"name": "ok", "body": "Servicio {{service}} caído",
	}, nil, s.Admin)
	s.do(t, http.MethodPost, "/admin/projects/"+s.projID+"/templates", map[string]any{
		"name": "rota", "body": "Falta {{variable_inexistente}}",
	}, nil, s.Admin)

	// plantilla válida renderiza
	_, out := s.notify(t, s.token, map[string]any{
		"to": []string{"log:e2e"}, "title": "t", "message": "raw",
		"template": "ok", "data": map[string]any{"service": "scraper"},
	}, nil)
	req, _ := http.NewRequest(http.MethodGet, s.URL+"/v1/messages/"+out["id"].(string), nil)
	req.Header.Set("Authorization", "Bearer "+s.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var msg map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&msg)
	resp.Body.Close()
	payload, _ := msg["payload"].(map[string]any)
	if payload["message"] != "Servicio scraper caído" {
		t.Fatalf("plantilla no renderizó: %v", payload["message"])
	}

	// plantilla rota → fallback a texto plano, y el envío sale igual
	_, out2 := s.notify(t, s.token, map[string]any{
		"to": []string{"log:e2e"}, "title": "t", "message": "texto plano de emergencia",
		"template": "rota", "data": map[string]any{},
	}, nil)
	msg2 := s.waitForStatus(t, out2["id"].(string), model.StatusSent)
	payload2, _ := msg2["payload"].(map[string]any)
	if payload2["message"] != "texto plano de emergencia" {
		t.Fatalf("fallback a texto plano roto: %v", payload2["message"])
	}
}

// Quiet hours: normal se difiere, urgent pasa. ------------------
func TestQuietHoursDefers(t *testing.T) {
	s := newStack(t, true, func(c *config.Config) { c.SyncWait = 1 * time.Second })

	// ventana que cubre AHORA (±1h, UTC)
	now := time.Now().UTC().Add(-time.Hour)
	var dest struct {
		ID string `json:"id"`
	}
	s.do(t, http.MethodPost, "/admin/projects/"+s.projID+"/destinations", map[string]any{
		"name": "log:quiet",
		"account_id": func() string {
			var accts []map[string]any
			s.do(t, http.MethodGet, "/admin/projects/"+s.projID+"/accounts", nil, &accts, s.Admin)
			return accts[0]["id"].(string)
		}(),
		"target": map[string]any{},
		"quiet_hours": map[string]any{
			"start": now.Format("15:04"), "end": time.Now().UTC().Add(time.Hour).Format("15:04"), "tz": "UTC",
		},
	}, &dest, s.Admin)

	// normal → diferido (sigue queued pasado el sync)
	_, out := s.notify(t, s.token, map[string]any{
		"to": []string{"log:quiet"}, "title": "t", "message": "m", "priority": "normal",
	}, nil)
	msg := s.waitForStatus(t, out["id"].(string), model.StatusQueued)
	if next, _ := msg["next_attempt_at"].(string); next == "" {
		t.Fatal("sin next_attempt_at tras defer por quiet hours")
	}
	// y NO se entrega mientras dura el silencio (el assert anterior era
	// trivial — el mensaje nace queued; con worker, un defer roto entregaría
	// en milisegundos y este poll lo caza)
	time.Sleep(700 * time.Millisecond)
	var during map[string]any
	s.do(t, http.MethodGet, "/v1/messages/"+out["id"].(string), nil, &during, s.token)
	if st, _ := during["status"].(string); st == model.StatusSent {
		t.Fatal("quiet hours roto: mensaje entregado durante el silencio")
	}

	// urgent → atraviesa el silencio
	_, out2 := s.notify(t, s.token, map[string]any{
		"to": []string{"log:quiet"}, "title": "t", "message": "m", "priority": "urgent",
	}, nil)
	s.waitForStatus(t, out2["id"].(string), model.StatusSent)
}

// Validaciones de entrada. --------------------------------------
func TestInputValidation(t *testing.T) {
	s := newStack(t, false, nil)
	cases := []struct {
		name string
		body map[string]any
		code int
	}{
		{"sin to", map[string]any{"title": "t", "message": "m"}, 400},
		{"title largo", map[string]any{"to": []string{"log:e2e"}, "title": string(make([]byte, 256)), "message": "m"}, 400},
		{"message grande", map[string]any{"to": []string{"log:e2e"}, "title": "t", "message": string(make([]byte, 9*1024))}, 400},
		{"prioridad inválida", map[string]any{"to": []string{"log:e2e"}, "title": "t", "message": "m", "priority": "máxima"}, 400},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if resp, _ := s.notify(t, s.token, tc.body, nil); resp.StatusCode != tc.code {
				t.Fatalf("esperaba %d, got %d", tc.code, resp.StatusCode)
			}
		})
	}
	// token basura → 401
	req, _ := http.NewRequest(http.MethodPost, s.URL+"/v1/notify",
		bytes.NewBufferString(`{"to":["log:e2e"],"title":"t","message":"m"}`))
	req.Header.Set("Authorization", "Bearer rvn_fake_token")
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("token falso: esperaba 401, got %d", resp.StatusCode)
	}
}

// Retención: los terminales viejos se borran, lo vivo nunca;
// la auditoría solo se borra dentro del barrido marcado (trigger intacto).
func TestRetentionSweep(t *testing.T) {
	s := newStack(t, false, nil)
	ctx := context.Background()

	_, o1 := s.notify(t, s.token, map[string]any{"to": []string{"log:e2e"}, "title": "viejo", "message": "m"}, nil)
	_, o2 := s.notify(t, s.token, map[string]any{"to": []string{"log:e2e"}, "title": "vivo", "message": "m"}, nil)
	oldID, liveID := o1["id"].(string), o2["id"].(string)

	// simular un mensaje enviado hace 40 días y una auditoría de hace 100
	// (created_at Y updated_at: un terminal viejo no se toca desde entonces)
	if _, err := s.Store.Pool.Exec(ctx,
		`UPDATE messages SET status='sent', created_at = now() - interval '40 days', updated_at = now() - interval '40 days' WHERE id=$1`, oldID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.Pool.Exec(ctx, `
		INSERT INTO audit_log(project_id, token_prefix, action, subject, created_at)
		VALUES ($1, 'test', 'retention', 'old-audit', now() - interval '100 days')`, s.projID); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Store.DeleteOldMessages(ctx, time.Now().Add(-30*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	var exists bool
	if err := s.Store.Pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM messages WHERE id=$1)`, oldID).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("el mensaje terminal viejo no se borró")
	}
	if err := s.Store.Pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM messages WHERE id=$1)`, liveID).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatal("la retención borró un mensaje VIVO (queued)")
	}

	if n, err := s.Store.DeleteOldAudit(ctx, time.Now().Add(-90*24*time.Hour)); err != nil || n == 0 {
		t.Fatalf("barrido de auditoría: n=%d err=%v", n, err)
	}
	if err := s.Store.Pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM audit_log WHERE subject='old-audit')`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("la auditoría vieja no se borró en el barrido")
	}

	// fuera del barrido, el trigger append-only sigue bloqueando el DELETE
	if _, err := s.Store.Pool.Exec(ctx, `
		INSERT INTO audit_log(project_id, token_prefix, action, subject)
		VALUES ($1, 'test', 'retention', 'inmutable')`, s.projID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.Pool.Exec(ctx, `DELETE FROM audit_log WHERE subject='inmutable'`); err == nil {
		t.Fatal("DELETE de auditoría sin el flag del barrido fue aceptado")
	}
}

// Criterio: matar el proceso a mitad de la cola no pierde ni duplica.
// Simulación determinista: mensajes 'delivering' con lease vencido (proceso
// muerto tras reclamar) → el reaper los reencola y entrega UNA sola vez.
func TestCrashRecoveryRequeuesStale(t *testing.T) {
	s := newStack(t, false, nil)

	ids := make([]string, 3)
	for i := range ids {
		_, out := s.notify(t, s.token, map[string]any{
			"to": []string{"log:e2e"}, "title": "crash", "message": fmt.Sprintf("m%d", i),
		}, nil)
		ids[i] = out["id"].(string)
	}
	if _, err := s.Store.Pool.Exec(context.Background(), `
		UPDATE messages SET status='delivering', attempts=1,
		       updated_at = now() - interval '3 hours'
		WHERE id = ANY($1::uuid[])`, ids); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	w := worker.New(s.Store, 4, 2*time.Minute)
	go w.Run(ctx)

	for _, id := range ids {
		msg := s.waitForStatus(t, id, model.StatusSent)
		if att := int(msg["attempts"].(float64)); att != 2 {
			t.Fatalf("attempts=%d, esperaba 2 (1 del crash + 1 entrega real)", att)
		}
	}
	var n int
	if err := s.Store.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM deliveries WHERE message_id = ANY($1::uuid[])`, ids).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != len(ids) {
		t.Fatalf("entregas duplicadas tras el crash: %d filas para %d mensajes", n, len(ids))
	}
}

// Replay de la DLQ: la entrega failed se reintegra y se reenvía; lo ya
// enviado NO se duplica.
func TestReplayDLQ(t *testing.T) {
	s := newStack(t, true, nil)
	ctx := context.Background()

	_, out := s.notify(t, s.token, map[string]any{"to": []string{"log:e2e"}, "title": "dlq", "message": "m"}, nil)
	id := out["id"].(string)
	s.waitForStatus(t, id, model.StatusSent)

	// forzar estado DLQ: mensaje dead con su entrega marcada failed
	if _, err := s.Store.Pool.Exec(ctx,
		`UPDATE messages SET status='dead' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.Pool.Exec(ctx,
		`UPDATE deliveries SET status='failed', error='reintentos agotados (DLQ)' WHERE message_id=$1`, id); err != nil {
		t.Fatal(err)
	}

	s.do(t, http.MethodPost, "/admin/messages/"+id+"/replay", nil, nil, s.Admin)
	s.waitForStatus(t, id, model.StatusSent)

	var total, sent int
	if err := s.Store.Pool.QueryRow(ctx,
		`SELECT count(*), count(*) FILTER (WHERE status='sent') FROM deliveries WHERE message_id=$1`, id).
		Scan(&total, &sent); err != nil {
		t.Fatal(err)
	}
	if total != 1 || sent != 1 {
		t.Fatalf("replay duplicó la entrega: total=%d sent=%d", total, sent)
	}
}

// POST /admin/destinations/{id}/test encola una prueba real del destino.
func TestAdminTestDestination(t *testing.T) {
	s := newStack(t, true, nil)

	var dests []map[string]any
	s.do(t, http.MethodGet, "/admin/projects/"+s.projID+"/destinations", nil, &dests, s.Admin)
	if len(dests) == 0 {
		t.Fatal("sin destinos para probar")
	}
	var resp struct {
		MessageID string `json:"message_id"`
	}
	s.do(t, http.MethodPost, "/admin/destinations/"+dests[0]["id"].(string)+"/test", nil, &resp, s.Admin)
	if resp.MessageID == "" {
		t.Fatal("test de destino sin message_id")
	}
	s.waitForStatus(t, resp.MessageID, model.StatusSent)
}

// ?sync=true: la respuesta espera la entrega real (para smoke tests).
func TestSyncTrueWaitsForDelivery(t *testing.T) {
	s := newStack(t, true, nil)

	var buf bytes.Buffer
	_ = json.NewEncoder(&buf).Encode(map[string]any{
		"to": []string{"log:e2e"}, "title": "sync", "message": "m", "priority": "high",
	})
	req, _ := http.NewRequest(http.MethodPost, s.URL+"/v1/notify?sync=true", &buf)
	req.Header.Set("Authorization", "Bearer "+s.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("sync=true debería responder 200 con el resultado, got %d", resp.StatusCode)
	}
	var out struct {
		ID         string           `json:"id"`
		Status     string           `json:"status"`
		Deliveries []map[string]any `json:"deliveries"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if out.Status != model.StatusSent {
		t.Fatalf("sync=true sin estado terminal: %q", out.Status)
	}
	if len(out.Deliveries) != 1 || out.Deliveries[0]["status"] != "sent" {
		t.Fatalf("entregas de sync inesperadas: %+v", out.Deliveries)
	}
}

// addAccountDestination crea cuenta+destino log de segunda vía (para reglas).
func (s *stack) addLogDestination(t *testing.T, name string) {
	t.Helper()
	var acct struct {
		ID string `json:"id"`
	}
	s.do(t, http.MethodPost, "/admin/projects/"+s.projID+"/accounts", map[string]any{
		"channel": "log", "label": name, "creds": map[string]any{},
	}, &acct, s.Admin)
	s.do(t, http.MethodPost, "/admin/projects/"+s.projID+"/destinations", map[string]any{
		"name": name, "account_id": acct.ID, "target": map[string]any{},
	}, nil, s.Admin)
}

// send_at: el mensaje espera en cola hasta la hora programada.
func TestSendAtSchedulesDelivery(t *testing.T) {
	s := newStack(t, true, nil)

	future := time.Now().Add(3 * time.Second).UTC().Format(time.RFC3339)
	resp, out := s.notify(t, s.token, map[string]any{
		"to": []string{"log:e2e"}, "title": "difusión", "message": "en 3s",
		"send_at": future,
	}, nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("esperaba 202, got %d: %v", resp.StatusCode, out)
	}
	id := out["id"].(string)

	// inmediatamente: queued con next_attempt_at futuro (no se reclama aún)
	msg := s.waitForStatus(t, id, model.StatusQueued)
	if next, _ := msg["next_attempt_at"].(string); next == "" || !time.Now().Before(mustParseRFC3339(t, next)) {
		t.Fatalf("next_attempt_at no está en el futuro: %v", msg["next_attempt_at"])
	}
	// cuando pasa la hora, el worker lo entrega
	s.waitForStatus(t, id, model.StatusSent)
}

func mustParseRFC3339(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("fecha no RFC3339: %q", s)
	}
	return tm
}

// Reglas de enrutamiento: priority=urgent + tag error añade otro destino.
func TestRoutingRulesAppendDestinations(t *testing.T) {
	s := newStack(t, true, nil)
	s.addLogDestination(t, "log:alerta")

	// regla: tag "error" Y prioridad mínima urgent → añadir log:alerta
	s.do(t, http.MethodPost, "/admin/projects/"+s.projID+"/routing-rules", map[string]any{
		"name": "urgente-a-alerta", "match_tags": []string{"error"},
		"match_priority": "urgent", "destinations": []string{"log:alerta"},
	}, nil, s.Admin)

	// la regla NO aplica a normal (prioridad mínima urgent → no alcanza el rango)
	_, out := s.notify(t, s.token, map[string]any{
		"to": []string{"log:e2e"}, "title": "t", "message": "m", "priority": "normal",
		"tags": []string{"error"},
	}, nil)
	msg := s.waitForStatus(t, out["id"].(string), model.StatusSent)
	if n := len(msg["deliveries"].([]any)); n != 1 {
		t.Fatalf("normal no debería activar la regla: %d entregas", n)
	}

	// urgent + tag error → la regla añade log:alerta (2 entregas, ambas sent)
	_, out2 := s.notify(t, s.token, map[string]any{
		"to": []string{"log:e2e"}, "title": "t", "message": "m", "priority": "urgent",
		"tags": []string{"error"},
	}, nil)
	msg2 := s.waitForStatus(t, out2["id"].(string), model.StatusSent)
	dels, _ := msg2["deliveries"].([]any)
	if len(dels) != 2 {
		t.Fatalf("la regla debería añadir log:alerta: %d entregas: %v", len(dels), msg2["deliveries"])
	}
	for _, d := range dels {
		if d.(map[string]any)["status"] != "sent" {
			t.Fatalf("entrega no sent: %v", d)
		}
	}
	// y el payload guarda los destinos definitivos
	var payloadTo json.RawMessage
	if err := s.Store.Pool.QueryRow(context.Background(),
		`SELECT payload->'to' FROM messages WHERE id=$1`, out2["id"].(string)).Scan(&payloadTo); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(payloadTo), "log:alerta") {
		t.Fatalf("el payload debe incluir el destino añadido por la regla: %s", payloadTo)
	}
}

// Agregación anti-spam: N idénticos en la ventana → 1 digest con count.
func TestAggregationDigest(t *testing.T) {
	s := newStackOpts(t, true, nil, map[string]any{"digest_window": "2s"})

	var firstID string
	for i, m := range []string{"m1", "m2", "m3"} {
		resp, out := s.notify(t, s.token, map[string]any{
			"to": []string{"log:e2e"}, "title": "fallo repetido", "message": m,
			"priority": "low", "tags": []string{"error"},
		}, nil)
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("msg %d: esperaba 202, got %d: %v", i, resp.StatusCode, out)
		}
		if i == 0 {
			firstID = out["id"].(string)
			if agg, _ := out["aggregated"].(bool); agg {
				t.Fatal("el primero no debería ser aggregated")
			}
			continue
		}
		if out["id"].(string) != firstID {
			t.Fatalf("msg %d debería unirse al digest %s, got %s", i, firstID, out["id"])
		}
		if agg, _ := out["aggregated"].(bool); !agg {
			t.Fatalf("msg %d debería marcar aggregated=true", i)
		}
	}

	// el digest se entrega tras cerrarse la ventana (2s)
	s.waitForStatus(t, firstID, model.StatusSent)

	// una sola fila, count=3, el payload guarda el ÚLTIMO mensaje
	var count int
	var payload string
	if err := s.Store.Pool.QueryRow(context.Background(),
		`SELECT aggregated_count, COALESCE((payload->>'message')::text,'')
		 FROM messages WHERE id=$1`, firstID).Scan(&count, &payload); err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Fatalf("aggregated_count=%d, esperaba 3", count)
	}
	if payload != "m3" {
		t.Fatalf("el digest debe guardar el último mensaje, got %q", payload)
	}
	var total int
	if err := s.Store.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM messages WHERE id=$1 OR aggregation_key IS NOT NULL`, firstID).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != 1 {
		t.Fatalf("debe existir UNA sola fila para el digest, hay %d", total)
	}
}

// Webhook: entrega real a un receptor local con firma HMAC y cabecera legada.
func TestWebhookDeliveryWithSignature(t *testing.T) {
	s := newStack(t, true, nil)

	var (
		mu        sync.Mutex
		gotBody   []byte
		gotSig    string
		gotSigV2  string
		gotTS     string
		gotSecret string
		gotReqID  string
	)
	recv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		gotBody, _ = io.ReadAll(r.Body)
		gotSig = r.Header.Get("X-RavenHub-Signature")
		gotSigV2 = r.Header.Get("X-RavenHub-Signature-V2")
		gotTS = r.Header.Get("X-RavenHub-Timestamp")
		gotSecret = r.Header.Get("X-RavenHub-Secret")
		gotReqID = "req-123"
		w.Header().Set("X-Request-Id", "req-123")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(recv.Close)

	const secret = "webhook-secret-e2e"
	var acct struct {
		ID string `json:"id"`
	}
	s.do(t, http.MethodPost, "/admin/projects/"+s.projID+"/accounts", map[string]any{
		"channel": "webhook", "label": "wh", "creds": map[string]any{"url": recv.URL, "secret": secret},
	}, &acct, s.Admin)
	s.do(t, http.MethodPost, "/admin/projects/"+s.projID+"/destinations", map[string]any{
		"name": "wh:hook", "account_id": acct.ID, "target": map[string]any{},
	}, nil, s.Admin)

	resp, out := s.notify(t, s.token, map[string]any{
		"to": []string{"wh:hook"}, "title": "wh", "message": "hola hook",
	}, map[string]string{"Idempotency-Key": "wh-1"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("esperaba 202, got %d: %v", resp.StatusCode, out)
	}
	s.waitForStatus(t, out["id"].(string), model.StatusSent)

	mu.Lock()
	defer mu.Unlock()
	if gotReqID == "" {
		t.Fatal("el receptor no recibió nada")
	}
	if gotSecret != secret {
		t.Fatalf("cabecera legada X-RavenHub-Secret rota: %q", gotSecret)
	}
	// firma = sha256=HMAC-SHA256(secret, body)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(gotBody)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if gotSig != want {
		t.Fatalf("firma incorrecta: got %q want %q (body %d bytes)", gotSig, want, len(gotBody))
	}
	// V2: timestamp + firma sobre timestamp.body (anti-replay si el receptor
	// aplica una ventana).
	if gotTS == "" {
		t.Fatal("falta X-RavenHub-Timestamp")
	}
	macV2 := hmac.New(sha256.New, []byte(secret))
	macV2.Write([]byte(gotTS))
	macV2.Write([]byte("."))
	macV2.Write(gotBody)
	wantV2 := "sha256=" + hex.EncodeToString(macV2.Sum(nil))
	if gotSigV2 != wantV2 {
		t.Fatalf("firma V2 incorrecta: got %q want %q", gotSigV2, wantV2)
	}
}

// DELETE /admin/projects/{id} borra el proyecto y su grafo (FK CASCADE),
// y reborrar da 404. Usado para limpiar proyectos smoke-*.
func TestDeleteProject(t *testing.T) {
	s := newStack(t, false, nil)

	var before []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	s.do(t, http.MethodGet, "/admin/projects", nil, &before, s.Admin)
	found := false
	for _, p := range before {
		if p.ID == s.projID {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("el proyecto del stack debería existir")
	}

	resp := s.do(t, http.MethodDelete, "/admin/projects/"+s.projID, nil, nil, s.Admin)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("borrar proyecto: esperaba 200, got %d", resp.StatusCode)
	}

	// el grafo quedó vacío: ni mensajes, ni token, ni cuenta del proyecto
	var n int
	if err := s.Store.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM messages WHERE project_id=$1`, s.projID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("mensajes residuales tras borrar el proyecto: %d", n)
	}
	if err := s.Store.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM accounts WHERE project_id=$1`, s.projID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("cuentas residuales tras borrar el proyecto: %d", n)
	}
	// el grafo también incluye tokens y destinos: el comentario de este test
	// los prometía pero no se contaban
	if err := s.Store.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM tokens WHERE project_id=$1`, s.projID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("tokens residuales tras borrar el proyecto: %d", n)
	}
	if err := s.Store.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM destinations WHERE project_id=$1`, s.projID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("destinos residuales tras borrar el proyecto: %d", n)
	}
	// la auditoría del proyecto se anonimiza (no queda project_id con PII).
	if err := s.Store.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_log WHERE project_id=$1`, s.projID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("auditoría con project_id residual tras borrar: %d", n)
	}
	if err := s.Store.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_log WHERE action='project.deleted'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("falta el tombstone global project.deleted")
	}

	// reborrar → 404
	resp = s.do(t, http.MethodDelete, "/admin/projects/"+s.projID, nil, nil, s.Admin)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("reborrar debería dar 404, got %d", resp.StatusCode)
	}
}

// Rotación: el gemelo nuevo funciona, el viejo muere al instante y el nombre
// queda con UN solo token activo (la rotación estaba rota al 100%: el índice
// único de tokens activos rechazaba el INSERT del gemelo mientras el viejo
// seguía visible dentro de la misma tx).
func TestRotateTokenFlow(t *testing.T) {
	s := newStack(t, false, nil)

	var created struct {
		ID     string `json:"id"`
		Token  string `json:"token"`
		Prefix string `json:"prefix"`
	}
	s.do(t, http.MethodPost, "/admin/projects/"+s.projID+"/tokens", map[string]any{
		"name": "rotativo", "max_priority": "high",
	}, &created, s.Admin)

	var rotated struct {
		ID     string `json:"id"`
		Token  string `json:"token"`
		Prefix string `json:"prefix"`
	}
	resp := s.do(t, http.MethodPost, "/admin/tokens/"+created.ID+"/rotate", nil, &rotated, s.Admin)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("rotate: esperaba 200, got %d: %v", resp.StatusCode, rotated)
	}
	if rotated.Token == "" || rotated.ID == "" || rotated.ID == created.ID {
		t.Fatalf("rotate debería mintear un token nuevo: %+v", rotated)
	}

	// el nuevo notifica (202); el VIEJO queda 401 al instante
	respNew := s.do(t, http.MethodPost, "/v1/notify", map[string]any{
		"to": []string{"log:e2e"}, "title": "t", "message": "m",
	}, nil, rotated.Token)
	if respNew.StatusCode != http.StatusAccepted {
		t.Fatalf("el token rotado debería notificar (202), got %d", respNew.StatusCode)
	}
	respOld := s.do(t, http.MethodPost, "/v1/notify", map[string]any{
		"to": []string{"log:e2e"}, "title": "t", "message": "m",
	}, nil, created.Token)
	if respOld.StatusCode != http.StatusUnauthorized {
		t.Fatalf("el token viejo debería dar 401 tras rotar, got %d", respOld.StatusCode)
	}

	// UN solo token activo con ese nombre, y el rotado quedó revocado
	var active, revoked int
	if err := s.Store.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM tokens WHERE project_id=$1 AND name='rotativo' AND revoked_at IS NULL`,
		s.projID).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 1 {
		t.Fatalf("debe quedar exactamente 1 token 'rotativo' activo, hay %d", active)
	}
	if err := s.Store.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM tokens WHERE project_id=$1 AND name='rotativo' AND revoked_at IS NOT NULL`,
		s.projID).Scan(&revoked); err != nil {
		t.Fatal(err)
	}
	if revoked != 1 {
		t.Fatalf("el token rotado debe quedar revocado, hay %d", revoked)
	}

	// el token nuevo respeta los scopes heredados (max_priority=high → urgent rechazado)
	scoped := s.do(t, http.MethodPost, "/v1/notify", map[string]any{
		"to": []string{"log:e2e"}, "title": "t", "message": "m", "priority": "urgent",
	}, nil, rotated.Token)
	if scoped.StatusCode != http.StatusForbidden {
		t.Fatalf("scopes heredados: urgent debería dar 403, got %d", scoped.StatusCode)
	}
}

// Quiet hours difiere SIN consumir TTL: el mensaje diferido sobrevive al
// expirer; un normal (TTL 1h) diferido a un horario hábil no debe morir
// "expired" antes de entregarse.
func TestQuietHoursDeferKeepsTTL(t *testing.T) {
	s := newStack(t, true, func(c *config.Config) { c.SyncWait = 1 * time.Second })

	// ventana que cubre AHORA (±1h, UTC), como TestQuietHoursDefers
	now := time.Now().UTC().Add(-time.Hour)
	var dest struct {
		ID string `json:"id"`
	}
	s.do(t, http.MethodPost, "/admin/projects/"+s.projID+"/destinations", map[string]any{
		"name": "log:quiet-ttl",
		"account_id": func() string {
			var accts []map[string]any
			s.do(t, http.MethodGet, "/admin/projects/"+s.projID+"/accounts", nil, &accts, s.Admin)
			return accts[0]["id"].(string)
		}(),
		"target": map[string]any{},
		"quiet_hours": map[string]any{
			"start": now.Format("15:04"), "end": time.Now().UTC().Add(time.Hour).Format("15:04"), "tz": "UTC",
		},
	}, &dest, s.Admin)

	_, out := s.notify(t, s.token, map[string]any{
		"to": []string{"log:quiet-ttl"}, "title": "t", "message": "m", "priority": "normal",
	}, nil)
	id, _ := out["id"].(string)
	s.waitForStatus(t, id, model.StatusQueued)

	// diferido → el expirer NO debe matarlo aunque su TTL base (1h) sea menor
	// que la espera al próximo horario hábil
	if expired, err := s.Store.ExpireMessages(context.Background(), 2*time.Minute); err != nil {
		t.Fatal(err)
	} else if len(expired) != 0 {
		t.Fatalf("expirer mató %d mensaje(s) diferidos por quiet hours", len(expired))
	}
	msg2 := s.waitForStatus(t, id, model.StatusQueued)
	next, _ := msg2["next_attempt_at"].(string)
	if next == "" {
		t.Fatal("sin next_attempt_at tras el defer")
	}
	// el expires_at quedó desplazado más allá del next_attempt (la ventana de
	// vida viaja con el diferido)
	var expires, nextAt time.Time
	if err := s.Store.Pool.QueryRow(context.Background(),
		`SELECT expires_at, next_attempt_at FROM messages WHERE id=$1`, id).Scan(&expires, &nextAt); err != nil {
		t.Fatal(err)
	}
	if !expires.After(nextAt) {
		t.Fatalf("expires_at (%s) debe superar next_attempt_at (%s) tras el defer", expires, nextAt)
	}
}

// Digest + Idempotency-Key: la clave de un mensaje plegado no se pierde;
// reintentar con la misma clave responde duplicate.
func TestDigestIdempotency(t *testing.T) {
	s := newStackOpts(t, true, nil, map[string]any{"digest_window": "2s"})

	// primer mensaje abre el digest
	_, first := s.notify(t, s.token, map[string]any{
		"to": []string{"log:e2e"}, "title": "reintento", "message": "m1", "priority": "low",
	}, map[string]string{"Idempotency-Key": "digest-k1"})
	digestID, _ := first["id"].(string)

	// segundo mensaje con SU clave se pliega
	_, second := s.notify(t, s.token, map[string]any{
		"to": []string{"log:e2e"}, "title": "reintento", "message": "m2", "priority": "low",
	}, map[string]string{"Idempotency-Key": "digest-k2"})
	if id, _ := second["id"].(string); id != digestID {
		t.Fatalf("el mensaje debería plegarse en %s, got %s", digestID, id)
	}
	if agg, _ := second["aggregated"].(bool); !agg {
		t.Fatal("el segundo debería marcar aggregated=true")
	}

	// reintento con la MISMA clave del plegado → duplicate, no re-contado
	_, third := s.notify(t, s.token, map[string]any{
		"to": []string{"log:e2e"}, "title": "reintento", "message": "m2", "priority": "low",
	}, map[string]string{"Idempotency-Key": "digest-k2"})
	if id, _ := third["id"].(string); id != digestID {
		t.Fatalf("reintento con la misma clave debería volver al digest %s, got %s", digestID, id)
	}
	if dup, _ := third["duplicate"].(bool); !dup {
		t.Fatalf("reintento con la misma clave debería ser duplicate=true, got %v", third)
	}

	// el digest se entrega tras la ventana con el conteo exacto (2, no 3)
	s.waitForStatus(t, digestID, model.StatusSent)
	var count int
	if err := s.Store.Pool.QueryRow(context.Background(),
		`SELECT aggregated_count FROM messages WHERE id=$1`, digestID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("aggregated_count=%d, esperaba 2 (el reintento no debe contar)", count)
	}
}

// max_priority se guarda NORMALIZADO: "HIGH" (mayúsculas) no debe dejar el
// techo fail-open; el token "acotado" no puede enviar urgent.
func TestScopesMaxPriorityNormalized(t *testing.T) {
	s := newStack(t, false, nil)

	var created struct {
		ID     string `json:"id"`
		Token  string `json:"token"`
		Prefix string `json:"prefix"`
	}
	resp := s.do(t, http.MethodPost, "/admin/projects/"+s.projID+"/tokens", map[string]any{
		"name": "techo-alto", "max_priority": "HIGH",
	}, &created, s.Admin)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("crear token con max_priority HIGH: %d", resp.StatusCode)
	}

	// urgent → 403 (el techo es real pese al casing); high → 202
	r1 := s.do(t, http.MethodPost, "/v1/notify", map[string]any{
		"to": []string{"log:e2e"}, "title": "t", "message": "m", "priority": "urgent",
	}, nil, created.Token)
	if r1.StatusCode != http.StatusForbidden {
		t.Fatalf("urgent con techo HIGH debería dar 403, got %d", r1.StatusCode)
	}
	r2 := s.do(t, http.MethodPost, "/v1/notify", map[string]any{
		"to": []string{"log:e2e"}, "title": "t", "message": "m", "priority": "high",
	}, nil, created.Token)
	if r2.StatusCode != http.StatusAccepted {
		t.Fatalf("high con techo HIGH debería dar 202, got %d", r2.StatusCode)
	}
}

// Una Idempotency-Key es por proyecto, no por token: el path sync de un
// duplicate aplica los scopes del token que pregunta — un token acodado que
// reusa la clave de otro no ve sus entregas.
func TestSyncDuplicateRespectsScopes(t *testing.T) {
	s := newStack(t, true, nil)

	key := "sync-scopes-" + time.Now().Format("150405.000000000")
	_, out := s.notify(t, s.token, map[string]any{
		"to": []string{"log:e2e"}, "title": "t", "message": "m",
	}, map[string]string{"Idempotency-Key": key})
	id, _ := out["id"].(string)
	s.waitForStatus(t, id, model.StatusSent)

	// token acodado a un destino AJENO al mensaje
	var acctID string
	{
		var accts []map[string]any
		s.do(t, http.MethodGet, "/admin/projects/"+s.projID+"/accounts", nil, &accts, s.Admin)
		acctID = accts[0]["id"].(string)
	}
	s.do(t, http.MethodPost, "/admin/projects/"+s.projID+"/destinations", map[string]any{
		"name": "log:otro", "account_id": acctID, "target": map[string]any{},
	}, nil, s.Admin)
	var scoped struct {
		Token string `json:"token"`
	}
	s.do(t, http.MethodPost, "/admin/projects/"+s.projID+"/tokens", map[string]any{
		"name": "ajeno", "destinations": []string{"log:otro"},
	}, &scoped, s.Admin)

	_, dup := s.notify(t, scoped.Token, map[string]any{
		"to": []string{"log:otro"}, "title": "t", "message": "m",
	}, map[string]string{"Idempotency-Key": key})
	if isDup, _ := dup["duplicate"].(bool); !isDup {
		t.Fatalf("esperaba duplicate=true, got %v", dup)
	}
	if _, has := dup["deliveries"]; has {
		t.Fatalf("el token acodado no debería ver las entregas del original: %v", dup)
	}
	if _, has := dup["status"]; has {
		t.Fatalf("ni el status del mensaje ajeno: %v", dup)
	}
}

// El listado filtra por scopes EN SQL: un token acotado ve SU mensaje aunque
// existan mensajes más recientes fuera de su scope.
func TestListMessagesScopedSeesOwn(t *testing.T) {
	s := newStack(t, true, nil)

	// segundo destino sobre la misma cuenta log
	var acctID string
	{
		var accts []map[string]any
		s.do(t, http.MethodGet, "/admin/projects/"+s.projID+"/accounts", nil, &accts, s.Admin)
		acctID = accts[0]["id"].(string)
	}
	s.do(t, http.MethodPost, "/admin/projects/"+s.projID+"/destinations", map[string]any{
		"name": "log:otro", "account_id": acctID, "target": map[string]any{},
	}, nil, s.Admin)

	var scoped struct {
		Token string `json:"token"`
	}
	s.do(t, http.MethodPost, "/admin/projects/"+s.projID+"/tokens", map[string]any{
		"name": "solo-propio", "destinations": []string{"log:e2e"},
	}, &scoped, s.Admin)

	// mensaje VISIBLE (log:e2e) y luego dos mensajes fuera de scope (más recientes)
	_, mine := s.notify(t, s.token, map[string]any{
		"to": []string{"log:e2e"}, "title": "mío", "message": "m",
	}, nil)
	s.waitForStatus(t, mine["id"].(string), model.StatusSent)
	for i := 0; i < 2; i++ {
		_, out := s.notify(t, s.token, map[string]any{
			"to": []string{"log:otro"}, "title": "ajeno", "message": "m",
		}, nil)
		s.waitForStatus(t, out["id"].(string), model.StatusSent)
	}

	// limit=1: con el filtro en Go el mensaje ajeno ocupaba la página y el
	// propio quedaba invisible; con el filtro SQL el propio ES la página
	var rows []map[string]any
	s.do(t, http.MethodGet, "/v1/messages?limit=1", nil, &rows, scoped.Token)
	if len(rows) != 1 {
		t.Fatalf("esperaba 1 mensaje, got %d", len(rows))
	}
	if rows[0]["id"] != mine["id"] {
		t.Fatalf("el mensaje visible debería ser el propio, got %v", rows[0]["id"])
	}

	// El scope también filtra el campo `to` del payload: el token de log:e2e
	// puede leer el contenido de un mensaje A+B (es destinatario), pero no la
	// lista completa de destinos ni las entregas de log:otro.
	_, multi := s.notify(t, s.token, map[string]any{
		"to": []string{"log:e2e", "log:otro"}, "title": "multi", "message": "contenido-compartido",
	}, nil)
	s.waitForStatus(t, multi["id"].(string), model.StatusSent)
	var got map[string]any
	s.do(t, http.MethodGet, "/v1/messages/"+multi["id"].(string), nil, &got, scoped.Token)
	payload, _ := got["payload"].(map[string]any)
	to, _ := payload["to"].([]any)
	if len(to) != 1 || to[0] != "log:e2e" {
		t.Fatalf("payload.to no filtrado por scope: %v", payload["to"])
	}
	if payload["message"] != "contenido-compartido" {
		t.Fatalf("se perdió el contenido visible: %v", payload["message"])
	}
	dels, _ := got["deliveries"].([]any)
	if len(dels) != 1 {
		t.Fatalf("deliveries no filtradas por scope: %v", dels)
	}
	if dm, _ := dels[0].(map[string]any); dm["destination"] != "log:e2e" {
		t.Fatalf("entrega de otro destino visible: %v", dm)
	}
}

// --- adopción y silencio ------------------------------------------------

// Un mensaje que expira por TTL deja rastro completo: status expired,
// contador por prioridad, auditoría append-only y visible en /readyz.
// Antes: solo un conteo en el log del hub.
func TestExpiredLeavesTrace(t *testing.T) {
	s := newStack(t, false, nil) // sin worker: el mensaje queda en cola
	expiredBefore := metricValue(t, s.URL, `ravenhub_expired_total{priority="urgent"}`)
	_, out := s.notify(t, s.token, map[string]any{
		"to": []string{"log:e2e"}, "title": "t", "message": "m", "priority": "urgent",
	}, nil)
	id, _ := out["id"].(string)
	if id == "" {
		t.Fatalf("sin id: %v", out)
	}
	s.waitForStatus(t, id, model.StatusQueued)

	// TTL vencido a mano (no esperamos los 15m de urgent)
	if _, err := s.Store.Pool.Exec(context.Background(),
		`UPDATE messages SET expires_at = now() - interval '1s' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	w := worker.New(s.Store, 1, time.Minute)
	if n, err := w.SweepExpired(context.Background()); err != nil || n != 1 {
		t.Fatalf("SweepExpired = %d, %v (esperaba 1)", n, err)
	}
	s.waitForStatus(t, id, model.StatusExpired)

	// auditoría append-only por mensaje
	var audit []map[string]any
	s.do(t, http.MethodGet, "/admin/audit?project_id="+s.projID, nil, &audit, s.Admin)
	found := false
	for _, a := range audit {
		if a["action"] == "message.expired" && a["subject"] == id {
			found = true
		}
	}
	if !found {
		t.Fatalf("sin auditoría message.expired para %s: %v", id, audit)
	}

	// contador por prioridad en /metrics — por DELTA (registro global al binario)
	if got := metricValue(t, s.URL, `ravenhub_expired_total{priority="urgent"}`); got-expiredBefore < 1 {
		t.Fatalf("métrica de expiración sin delta (antes %v, ahora %v)", expiredBefore, got)
	}

	// /readyz expone los expirados (el watchdog/operador los ve sin SQL)
	var ready map[string]any
	s.do(t, http.MethodGet, "/readyz", nil, &ready, "")
	if n, _ := ready["expired"].(float64); n < 1 {
		t.Fatalf("readyz sin contador de expirados: %v", ready)
	}
}

// Con un TTL de proyecto más corto que el backoff, el reintento no expira en
// silencio: el mensaje va a DLQ con motivo explícito (visible en el gauge y
// replayable), no se queda girando hasta morir como "expired" mudo.
func TestTTLBudgetGuardGoesToDLQ(t *testing.T) {
	s := newStackOpts(t, true, nil, map[string]any{"ttl": map[string]any{"normal": "2s"}})

	// cuenta webhook → puerto cerrado: fallo RETRYABLE (no permanente)
	var acct struct {
		ID string `json:"id"`
	}
	s.do(t, http.MethodPost, "/admin/projects/"+s.projID+"/accounts", map[string]any{
		"channel": "webhook", "label": "wh-dead",
		"creds": map[string]any{"url": "http://127.0.0.1:1/hook"},
	}, &acct, s.Admin)
	s.do(t, http.MethodPost, "/admin/projects/"+s.projID+"/destinations", map[string]any{
		"name": "wh:dead", "account_id": acct.ID, "target": map[string]any{},
	}, nil, s.Admin)

	_, out := s.notify(t, s.token, map[string]any{
		"to": []string{"wh:dead"}, "title": "t", "message": "m", "priority": "normal",
	}, nil)
	id, _ := out["id"].(string)
	msg := s.waitForStatus(t, id, model.StatusDead, model.StatusExpired)
	if msg["status"] != model.StatusDead {
		t.Fatalf("esperaba dead (guardia TTL), got %v", msg["status"])
	}
	var errMsg string
	if err := s.Store.Pool.QueryRow(context.Background(),
		`SELECT error FROM deliveries WHERE message_id=$1`, id).Scan(&errMsg); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errMsg, "TTL agotado") {
		t.Fatalf("motivo de DLQ sin guardia TTL: %q", errMsg)
	}
	// la transición a dead deja asiento de auditoría (message.dead con
	// motivo); la pérdida por reintentos/TTL no queda invisible.
	// La auditoría se inserta después de la transición a dead; poll breve
	// para no convertir esa ventana en un falso negativo del test.
	deadline := time.Now().Add(3 * time.Second)
	for {
		var audit []map[string]any
		s.do(t, http.MethodGet, "/admin/audit?project_id="+s.projID, nil, &audit, s.Admin)
		found := false
		for _, a := range audit {
			if a["action"] == "message.dead" && a["subject"] == id {
				found = true
			}
		}
		if found {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("sin auditoría message.dead para %s: %v", id, audit)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Un mensaje cortado por el límite del canal queda MARCADO
// (deliveries.truncated + ravenhub_truncated_total): deja de ser invisible.
func TestTruncatedDeliveryFlagged(t *testing.T) {
	s := newStack(t, true, nil)
	truncBefore := metricValue(t, s.URL, `ravenhub_truncated_total{channel="log"}`)

	long := strings.Repeat("x", 8100) // > 8000 de Plain (canal log)
	_, out := s.notify(t, s.token, map[string]any{
		"to": []string{"log:e2e"}, "title": "t", "message": long, "priority": "high",
	}, nil)
	id, _ := out["id"].(string)
	msg := s.waitForStatus(t, id, model.StatusSent)
	dels, _ := msg["deliveries"].([]any)
	if len(dels) != 1 {
		t.Fatalf("entregas: %v", msg["deliveries"])
	}
	if d := dels[0].(map[string]any); d["truncated"] != true {
		t.Fatalf("truncado no marcado: %v", d)
	}

	// métrica por canal — por DELTA (registro global al binario)
	if got := metricValue(t, s.URL, `ravenhub_truncated_total{channel="log"}`); got-truncBefore < 1 {
		t.Fatalf("métrica de truncado sin delta (antes %v, ahora %v)", truncBefore, got)
	}

	// mensaje corto → sin marca (omitempty: el campo no aparece)
	_, out2 := s.notify(t, s.token, map[string]any{
		"to": []string{"log:e2e"}, "title": "t", "message": "corto",
	}, nil)
	msg2 := s.waitForStatus(t, out2["id"].(string), model.StatusSent)
	if d := msg2["deliveries"].([]any)[0].(map[string]any); d["truncated"] != nil {
		t.Fatalf("marca de truncado espuria: %v", d)
	}
}

// La contrapresión deja métrica: responde "¿cuántas notificaciones se
// descartaron?".
func TestShedMetric(t *testing.T) {
	s := newStack(t, false, func(c *config.Config) { c.SoftLimit = 1; c.HardLimit = 7500 })
	shedBefore := metricValue(t, s.URL, `ravenhub_shed_total{scope="project",code="429"}`)

	resp1, _ := s.notify(t, s.token, map[string]any{
		"to": []string{"log:e2e"}, "title": "a", "message": "m", "priority": "low",
	}, nil)
	if resp1.StatusCode != http.StatusAccepted {
		t.Fatalf("1er mensaje: %d", resp1.StatusCode)
	}
	// cola del proyecto (1 queued) ≥ soft → shedding de low/normal
	resp2, _ := s.notify(t, s.token, map[string]any{
		"to": []string{"log:e2e"}, "title": "b", "message": "m", "priority": "low",
	}, nil)
	if resp2.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("2º mensaje: %d (esperaba 429)", resp2.StatusCode)
	}

	// por DELTA: TestBackpressure ya emitió esta serie en el registro GLOBAL
	// del binario — un Contains era tautológico
	if got := metricValue(t, s.URL, `ravenhub_shed_total{scope="project",code="429"}`); got-shedBefore < 1 {
		t.Fatalf("el shedding propio no dejó métrica (delta %g): serie global contaminada", got-shedBefore)
	}
}

// El seed cubre TODO el provisioning: options (aplicadas también al
// re-import), destinos, reglas de enrutamiento y scopes de tokens existentes.
func TestSeedProvisioningEndToEnd(t *testing.T) {
	s := newStack(t, true, nil)
	ctx := context.Background()

	seedPath := filepath.Join(t.TempDir(), "seed.yaml")
	body := fmt.Sprintf(`projects:
  - name: %s
    options:
      ttl:
        urgent: 10m
    tokens:
      - name: main
        scopes:
          destinations: [log:e2e]
          max_priority: normal
    destinations:
      - name: log:regla
        account: log-acct
        target: {}
    routing_rules:
      - name: urgentes-a-regla
        match_priority: urgent
        destinations: [log:regla]
`, s.project)
	if err := os.WriteFile(seedPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := importer.Import(ctx, s.Store, seedPath); err != nil {
		t.Fatalf("import: %v", err)
	}

	// options aplicadas al proyecto existente
	var opts string
	if err := s.Store.Pool.QueryRow(ctx,
		`SELECT options::text FROM projects WHERE id=$1`, s.projID).Scan(&opts); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(opts, "10m") {
		t.Fatalf("options del seed no aplicadas: %s", opts)
	}

	// scopes del token EXISTENTE sincronizados
	var scopes string
	if err := s.Store.Pool.QueryRow(ctx,
		`SELECT scopes::text FROM tokens WHERE project_id=$1 AND name='main'`, s.projID).Scan(&scopes); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(scopes, "normal") || !strings.Contains(scopes, "log:e2e") {
		t.Fatalf("scopes no sincronizados: %s", scopes)
	}

	// la regla declarada enruta de verdad: un token sin scopes notifica a
	// log:e2e con prioridad urgent y la regla AÑADE log:regla
	var free struct {
		Token string `json:"token"`
	}
	s.do(t, http.MethodPost, "/admin/projects/"+s.projID+"/tokens", map[string]any{"name": "libre"}, &free, s.Admin)
	_, out := s.notify(t, free.Token, map[string]any{
		"to": []string{"log:e2e"}, "title": "t", "message": "m", "priority": "urgent",
	}, nil)
	id, _ := out["id"].(string)
	msg := s.waitForStatus(t, id, model.StatusSent)
	payload, _ := msg["payload"].(map[string]any)
	to, _ := payload["to"].([]any)
	if len(to) != 2 {
		t.Fatalf("la regla del seed no añadió el destino: %v", payload["to"])
	}

	// re-import idempotente: no vuelve a "cambiar" nada
	if err := importer.Import(ctx, s.Store, seedPath); err != nil {
		t.Fatalf("re-import: %v", err)
	}
	var opts2, scopes2 string
	_ = s.Store.Pool.QueryRow(ctx, `SELECT options::text FROM projects WHERE id=$1`, s.projID).Scan(&opts2)
	_ = s.Store.Pool.QueryRow(ctx,
		`SELECT scopes::text FROM tokens WHERE project_id=$1 AND name='main'`, s.projID).Scan(&scopes2)
	if opts2 != opts || scopes2 != scopes {
		t.Fatalf("re-import no idempotente: opts %s→%s scopes %s→%s", opts, opts2, scopes, scopes2)
	}
}

// Paridad admin: PATCH de options (con validación estricta) y DELETE de grupo.
func TestAdminProjectOptionsAndGroupDelete(t *testing.T) {
	s := newStack(t, false, nil)

	var out map[string]any
	resp := s.do(t, http.MethodPatch, "/admin/projects/"+s.projID,
		map[string]any{"options": map[string]any{"ttl": map[string]any{"urgent": "5m"}}}, &out, s.Admin)
	if resp.StatusCode != http.StatusOK || out["updated"] != true {
		t.Fatalf("PATCH options: %d %v", resp.StatusCode, out)
	}
	// repetido → idempotente (updated=false)
	resp = s.do(t, http.MethodPatch, "/admin/projects/"+s.projID,
		map[string]any{"options": map[string]any{"ttl": map[string]any{"urgent": "5m"}}}, &out, s.Admin)
	if resp.StatusCode != http.StatusOK || out["updated"] != false {
		t.Fatalf("PATCH repetido: %d %v", resp.StatusCode, out)
	}
	// inválidas → 400 (fail-fast, no se guarda basura que el runtime ignoraría)
	resp = s.do(t, http.MethodPatch, "/admin/projects/"+s.projID,
		map[string]any{"options": map[string]any{"ttl": map[string]any{"urgent": "abc"}}}, nil, s.Admin)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("PATCH inválido: %d (esperaba 400)", resp.StatusCode)
	}

	// grupo: crear → borrar → 404 al repetir
	s.do(t, http.MethodPost, "/admin/projects/"+s.projID+"/groups",
		map[string]any{"name": "g-test", "destinations": []string{"log:e2e"}}, nil, s.Admin)
	resp = s.do(t, http.MethodDelete, "/admin/projects/"+s.projID+"/groups/g-test", nil, nil, s.Admin)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE grupo: %d", resp.StatusCode)
	}
	resp = s.do(t, http.MethodDelete, "/admin/projects/"+s.projID+"/groups/g-test", nil, nil, s.Admin)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("DELETE grupo repetido: %d (esperaba 404)", resp.StatusCode)
	}
}

// --- remediación ---------------------------------------------------------

// Fan-out de grupos: to:["grupo"] entrega a TODOS los miembros (contrato)
// — la promesa no tenía ningún test de entrega.
func TestGroupFanOutDeliversToAll(t *testing.T) {
	s := newStack(t, true, nil)

	var acct struct {
		ID string `json:"id"`
	}
	s.do(t, http.MethodPost, "/admin/projects/"+s.projID+"/accounts", map[string]any{
		"channel": "log", "label": "log-acct-2", "creds": map[string]any{},
	}, &acct, s.Admin)
	s.do(t, http.MethodPost, "/admin/projects/"+s.projID+"/destinations", map[string]any{
		"name": "log:otro", "account_id": acct.ID, "target": map[string]any{},
	}, nil, s.Admin)
	s.do(t, http.MethodPost, "/admin/projects/"+s.projID+"/groups", map[string]any{
		"name": "ambos", "destinations": []string{"log:e2e", "log:otro"},
	}, nil, s.Admin)

	_, out := s.notify(t, s.token, map[string]any{
		"to": []string{"ambos"}, "title": "fan-out", "message": "a los dos",
	}, nil)
	id, _ := out["id"].(string)
	msg := s.waitForStatus(t, id, model.StatusSent)
	dels, _ := msg["deliveries"].([]any)
	if len(dels) != 2 {
		t.Fatalf("fan-out del grupo: esperaba 2 entregas, got %v", msg["deliveries"])
	}
	for _, d := range dels {
		dm, _ := d.(map[string]any)
		if dm["status"] != model.StatusSent {
			t.Fatalf("entrega del grupo no sent: %v", dm)
		}
	}
}

// El throttle de fallos de autenticación se ancla en RemoteAddr: un atacante
// no puede sortearlo inventando X-Forwarded-For por petición.
func TestAuthThrottleIgnoresForwardedFor(t *testing.T) {
	s := newStack(t, false, nil)
	saw429 := false
	for i := 0; i < 15; i++ {
		var buf bytes.Buffer
		_ = json.NewEncoder(&buf).Encode(map[string]any{"to": []string{"log:e2e"}, "title": "t", "message": "m"})
		req, _ := http.NewRequest(http.MethodPost, s.URL+"/v1/notify", &buf)
		req.Header.Set("Authorization", "Bearer rvn_falso_"+strconv.Itoa(i))
		req.Header.Set("X-Forwarded-For", fmt.Sprintf("10.0.0.%d", i)) // spoof
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests {
			saw429 = true
		}
	}
	if !saw429 {
		t.Fatal("15 fallos con X-Forwarded-For variable no agotaron el bucket: el throttle sería spoofeable")
	}
}

// La clave de un miembro plegado a un digest YA ENTREGADO sigue dedupeando
// (vida de fila): reintentar con ella tras el cierre NO crea un mensaje nuevo
// ni duplica la notificación del mismo evento.
func TestFoldedKeyDedupAfterDigestClosed(t *testing.T) {
	s := newStackOpts(t, true, nil, map[string]any{"digest_window": "1s"})

	// A abre el digest; B se pliega con SU PROPIA clave (va a folded_keys)
	respA, outA := s.notify(t, s.token, map[string]any{
		"to": []string{"log:e2e"}, "title": "fallo repetido", "message": "a",
		"priority": "low", "tags": []string{"error"},
	}, map[string]string{"Idempotency-Key": "fold-open"})
	if respA.StatusCode != http.StatusAccepted {
		t.Fatalf("A: %d %v", respA.StatusCode, outA)
	}
	digestID, _ := outA["id"].(string)
	respB, outB := s.notify(t, s.token, map[string]any{
		"to": []string{"log:e2e"}, "title": "fallo repetido", "message": "b",
		"priority": "low", "tags": []string{"error"},
	}, map[string]string{"Idempotency-Key": "fold-member"})
	if respB.StatusCode != http.StatusAccepted || outB["id"] != digestID {
		t.Fatalf("B debería plegarse al digest %s: %d %v", digestID, respB.StatusCode, outB)
	}

	// el digest se entrega y la ventana CIERRA
	s.waitForStatus(t, digestID, model.StatusSent)

	// reintento con la clave del MIEMBRO tras el cierre → duplicate, no nuevo
	respR, outR := s.notify(t, s.token, map[string]any{
		"to": []string{"log:e2e"}, "title": "fallo repetido", "message": "b (reintento)",
		"priority": "low", "tags": []string{"error"},
	}, map[string]string{"Idempotency-Key": "fold-member"})
	if respR.StatusCode != http.StatusAccepted {
		t.Fatalf("reintento: %d %v", respR.StatusCode, outR)
	}
	if outR["id"] != digestID || outR["duplicate"] != true {
		t.Fatalf("la clave del miembro plegado se perdió tras cerrar el digest: %v (esperaba duplicate del %s)", outR, digestID)
	}

	// la misma clave con high (que NO agrega) también debe ser duplicate.
	respH, outH := s.notify(t, s.token, map[string]any{
		"to": []string{"log:e2e"}, "title": "fallo repetido", "message": "b (high)",
		"priority": "high", "tags": []string{"error"},
	}, map[string]string{"Idempotency-Key": "fold-member"})
	if respH.StatusCode != http.StatusAccepted || outH["duplicate"] != true || outH["id"] != digestID {
		t.Fatalf("clave plegada con high no deduplicó: %d %v", respH.StatusCode, outH)
	}
}

// Borrar una cuenta arrastra sus destinos por CASCADE: la pérdida queda
// auditada con el inventario exacto (era la única operación de admin sin
// asiento).
func TestDeleteAccountAuditsCascade(t *testing.T) {
	s := newStack(t, false, nil)

	var acct struct {
		ID string `json:"id"`
	}
	s.do(t, http.MethodPost, "/admin/projects/"+s.projID+"/accounts", map[string]any{
		"channel": "log", "label": "log-cascada", "creds": map[string]any{},
	}, &acct, s.Admin)
	s.do(t, http.MethodPost, "/admin/projects/"+s.projID+"/destinations", map[string]any{
		"name": "log:uno", "account_id": acct.ID, "target": map[string]any{},
	}, nil, s.Admin)
	s.do(t, http.MethodPost, "/admin/projects/"+s.projID+"/destinations", map[string]any{
		"name": "log:dos", "account_id": acct.ID, "target": map[string]any{},
	}, nil, s.Admin)

	var out map[string]any
	resp := s.do(t, http.MethodDelete, "/admin/accounts/"+acct.ID, nil, &out, s.Admin)
	if resp.StatusCode != http.StatusOK || out["deleted"] != true {
		t.Fatalf("DELETE cuenta: %d %v", resp.StatusCode, out)
	}
	removed, _ := out["destinations_removed"].([]any)
	if len(removed) != 2 {
		t.Fatalf("el CASCADE no reportó sus 2 destinos: %v", out)
	}

	// el asiento existe y nombra lo borrado
	var audit []map[string]any
	s.do(t, http.MethodGet, "/admin/audit?project_id="+s.projID, nil, &audit, s.Admin)
	found := false
	for _, a := range audit {
		if a["action"] == "account.deleted" && a["subject"] == "log-cascada" {
			found = true
		}
	}
	if !found {
		t.Fatalf("sin auditoría account.deleted: %v", audit)
	}
	// y los destinos efectivamente desaparecieron
	var n int
	if err := s.Store.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM destinations WHERE account_id=$1`, acct.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("destinos sobrevivieron al CASCADE: %d", n)
	}
}

// un grupo que queda vacío antes de la entrega no puede terminar
// como "sent con 0 deliveries"; debe ir a DLQ con rastro.
func TestEmptyGroupGoesToDLQ(t *testing.T) {
	s := newStack(t, true, nil)

	var dests []map[string]any
	s.do(t, http.MethodGet, "/admin/projects/"+s.projID+"/destinations", nil, &dests, s.Admin)
	destID := ""
	for _, d := range dests {
		if d["name"] == "log:e2e" {
			destID, _ = d["id"].(string)
		}
	}
	if destID == "" {
		t.Fatal("no se encontró log:e2e")
	}
	s.do(t, http.MethodPost, "/admin/projects/"+s.projID+"/groups", map[string]any{
		"name": "g-vacio", "destinations": []string{"log:e2e"},
	}, nil, s.Admin)

	_, out := s.notify(t, s.token, map[string]any{
		"to": []string{"g-vacio"}, "title": "grupo", "message": "m",
		"send_at": time.Now().Add(2 * time.Second).UTC().Format(time.RFC3339),
	}, nil)
	id, _ := out["id"].(string)
	// vaciar el grupo antes de que el worker lo reclame
	resp := s.do(t, http.MethodDelete, "/admin/destinations/"+destID, nil, nil, s.Admin)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE destino: %d", resp.StatusCode)
	}
	msg := s.waitForStatus(t, id, model.StatusDead)
	if msg["status"] != model.StatusDead {
		t.Fatalf("grupo vacío no fue a DLQ: %v", msg["status"])
	}
}

// borrar un destino no deja destination_id colgante en deliveries; el
// histórico (dest_name/error) sigue con el mensaje hasta su retención.
func TestDeleteDestinationClearsDeliveryRef(t *testing.T) {
	s := newStack(t, true, nil)

	var dests []map[string]any
	s.do(t, http.MethodGet, "/admin/projects/"+s.projID+"/destinations", nil, &dests, s.Admin)
	destID := ""
	for _, d := range dests {
		if d["name"] == "log:e2e" {
			destID, _ = d["id"].(string)
		}
	}
	if destID == "" {
		t.Fatal("no se encontró el destino log:e2e")
	}

	_, out := s.notify(t, s.token, map[string]any{
		"to": []string{"log:e2e"}, "title": "dest", "message": "m",
	}, nil)
	id, _ := out["id"].(string)
	s.waitForStatus(t, id, model.StatusSent)

	var before string
	if err := s.Store.Pool.QueryRow(context.Background(),
		`SELECT destination_id::text FROM deliveries WHERE message_id=$1`, id).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if before != destID {
		t.Fatalf("delivery.destination_id = %s; esperaba %s", before, destID)
	}

	resp := s.do(t, http.MethodDelete, "/admin/destinations/"+destID, nil, nil, s.Admin)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE destino: %d", resp.StatusCode)
	}
	var isNull bool
	if err := s.Store.Pool.QueryRow(context.Background(),
		`SELECT destination_id IS NULL FROM deliveries WHERE message_id=$1`, id).Scan(&isNull); err != nil {
		t.Fatal(err)
	}
	if !isNull {
		t.Fatal("delivery.destination_id quedó colgante tras borrar el destino")
	}
}

// La entrega por Telegram (adaptador COMPLETO: parseo de la respuesta del Bot
// API, provider_msg_id, body) contra un stub HTTP local vía api_base.
func TestTelegramStubDelivery(t *testing.T) {
	var gotPath, gotBody string
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":77}}`))
	}))
	defer stub.Close()

	s := newStack(t, true, nil)
	var acct struct {
		ID string `json:"id"`
	}
	s.do(t, http.MethodPost, "/admin/projects/"+s.projID+"/accounts", map[string]any{
		"channel": "telegram", "label": "tg-stub",
		"creds": map[string]any{"bot_token": "123:ABC", "api_base": stub.URL},
	}, &acct, s.Admin)
	s.do(t, http.MethodPost, "/admin/projects/"+s.projID+"/destinations", map[string]any{
		"name": "tg:stub", "account_id": acct.ID, "target": map[string]any{"chat_id": 42},
	}, nil, s.Admin)

	_, out := s.notify(t, s.token, map[string]any{
		"to": []string{"tg:stub"}, "title": "hola", "message": "mundo <b>",
	}, nil)
	msg := s.waitForStatus(t, out["id"].(string), model.StatusSent)
	dels, _ := msg["deliveries"].([]any)
	if len(dels) != 1 {
		t.Fatalf("entregas: %v", msg["deliveries"])
	}
	dm, _ := dels[0].(map[string]any)
	if dm["status"] != model.StatusSent {
		t.Fatalf("entrega telegram no sent: %v", dm)
	}
	if pid, _ := dm["provider_msg_id"].(string); pid != "77" {
		t.Fatalf("provider_msg_id del stub no propagado: %v", dm)
	}
	if !strings.Contains(gotPath, "/bot123:ABC/sendMessage") {
		t.Fatalf("path del Bot API inesperado: %q", gotPath)
	}
	if !strings.Contains(gotBody, `"parse_mode":"HTML"`) || !strings.Contains(gotBody, `"chat_id":42`) {
		t.Fatalf("body del Bot API inesperado: %q", gotBody)
	}
}

// Discord por webhook real contra stub local: valida el adaptador Y la
// neutralización de menciones activas <@…> en content.
func TestDiscordStubDelivery(t *testing.T) {
	var gotBody string
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer stub.Close()

	s := newStack(t, true, nil)
	var acct struct {
		ID string `json:"id"`
	}
	s.do(t, http.MethodPost, "/admin/projects/"+s.projID+"/accounts", map[string]any{
		"channel": "discord", "label": "dc-stub",
		"creds": map[string]any{"url": stub.URL},
	}, &acct, s.Admin)
	s.do(t, http.MethodPost, "/admin/projects/"+s.projID+"/destinations", map[string]any{
		"name": "dc:stub", "account_id": acct.ID, "target": map[string]any{},
	}, nil, s.Admin)

	_, out := s.notify(t, s.token, map[string]any{
		"to": []string{"dc:stub"}, "title": "alerta", "message": "mira <@123456> y @everyone esto",
	}, nil)
	s.waitForStatus(t, out["id"].(string), model.StatusSent)
	if !strings.Contains(gotBody, `"content"`) {
		t.Fatalf("payload discord sin content: %q", gotBody)
	}
	if strings.Contains(gotBody, "<@") {
		t.Fatalf("mención activa llegó al webhook de Discord: %q", gotBody)
	}
	// @everyone/@here no usan <…>: se desactivan con allowed_mentions.parse=[].
	if !strings.Contains(gotBody, `"allowed_mentions"`) || !strings.Contains(gotBody, `"parse":[]`) {
		t.Fatalf("payload discord sin allowed_mentions.parse=[]: %q", gotBody)
	}
}

// Rotación de master key: re-encrypt descifra con la cadena de la
// passphrase VIEJA y re-cifra con la nueva en formato v3; cambiar la master
// key no debe dejar las creds indescifrables.
func TestReEncryptRotatesKey(t *testing.T) {
	s := newStack(t, false, nil)
	var acct struct {
		ID string `json:"id"`
	}
	s.do(t, http.MethodPost, "/admin/projects/"+s.projID+"/accounts", map[string]any{
		"channel": "webhook", "label": "wh-rotada",
		"creds": map[string]any{"url": "http://127.0.0.1:9/hook"},
	}, &acct, s.Admin)

	newKey := "clave-nueva-post-rotacion"
	st2 := &store.Store{
		Pool:            s.Store.Pool,
		MasterKey:       crypto.MasterKey(newKey),
		MidMasterKey:    crypto.MasterKeyV1(newKey),
		LegacyMasterKey: crypto.LegacyMasterKey(newKey),
	}
	oldChain := [][]byte{
		crypto.MasterKey("e2e-master-key"),
		crypto.MasterKeyV1("e2e-master-key"),
		crypto.LegacyMasterKey("e2e-master-key"),
	}
	ok, failed, err := st2.ReEncryptAccounts(context.Background(), oldChain)
	if err != nil || failed != 0 || ok < 1 {
		t.Fatalf("re-encrypt: ok=%d failed=%d err=%v", ok, failed, err)
	}

	// con la clave nueva se descifra la creds original…
	row, err := st2.GetAccountByLabel(context.Background(), s.projID, "wh-rotada")
	if err != nil {
		t.Fatal(err)
	}
	plain, err := st2.DecryptCreds(row)
	if err != nil || !strings.Contains(string(plain), "127.0.0.1:9") {
		t.Fatalf("creds tras rotación: %v %q", err, plain)
	}
	// …y con la vieja ya NO (la rotación cambia la clave de verdad)
	st1 := &store.Store{
		Pool:            s.Store.Pool,
		MasterKey:       crypto.MasterKey("e2e-master-key"),
		MidMasterKey:    crypto.MasterKeyV1("e2e-master-key"),
		LegacyMasterKey: crypto.LegacyMasterKey("e2e-master-key"),
	}
	if _, err := st1.DecryptCreds(row); err == nil {
		t.Fatal("la clave vieja sigue abriendo las creds tras la rotación")
	}
}

// M1: un digest RECLAMADO (delivering) no puede recibir folds — el fold
// sin guardia de estado plegaba sobre uno en entrega y la copia se perdía en
// silencio (su clave quedaba en folded_keys de un digest que ya llevaba el
// texto viejo). El mensaje que llegaría tarde abre digest nuevo.
func TestDigestFoldSkipsClaimedDigest(t *testing.T) {
	s := newStackOpts(t, true, nil, map[string]any{"digest_window": "60s"})

	_, out := s.notify(t, s.token, map[string]any{
		"to": []string{"log:e2e"}, "title": "fallo", "message": "m1", "priority": "low",
	}, nil)
	digestID := out["id"].(string)
	if agg, _ := out["aggregated"].(bool); agg {
		t.Fatal("el primero no debería ser aggregated")
	}

	// reclamar el digest a mano (el worker real no lo toca: la ventana de 60s
	// sigue abierta) — simula el interleaving SELECT(digest abierto) → claim → fold
	tag, err := s.Store.Pool.Exec(context.Background(),
		`UPDATE messages SET status='delivering', attempts=1, updated_at=now() WHERE id=$1`, digestID)
	if err != nil {
		t.Fatal(err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatal("no se pudo reclamar el digest de prueba")
	}

	resp, out2 := s.notify(t, s.token, map[string]any{
		"to": []string{"log:e2e"}, "title": "fallo", "message": "m2", "priority": "low",
	}, nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("esperaba 202, got %d: %v", resp.StatusCode, out2)
	}
	if agg, _ := out2["aggregated"].(bool); agg {
		t.Fatal("plegó sobre un digest EN ENTREGA (la copia se habría perdido)")
	}
	if out2["id"].(string) == digestID {
		t.Fatal("el mensaje debería abrir fila nueva, no unirse al digest reclamado")
	}

	// y el digest en entrega queda intacto: ni count ni folded_keys mutados
	var count int
	var folded []string
	if err := s.Store.Pool.QueryRow(context.Background(),
		`SELECT aggregated_count, COALESCE(folded_keys, '{}'::text[]) FROM messages WHERE id=$1`,
		digestID).Scan(&count, &folded); err != nil {
		t.Fatal(err)
	}
	if count != 1 || len(folded) != 0 {
		t.Fatalf("el digest en entrega fue mutado: count=%d folded=%v", count, folded)
	}
}

// M2: el expirer no corta una entrega viva — un 'delivering' con latido
// fresco (updated_at dentro del lease) no se marca 'expired' aunque su TTL
// esté agotado; con el latido muerto sí expira (crash recovery intacto).
func TestExpireSkipsLiveDelivering(t *testing.T) {
	s := newStack(t, false, nil) // sin worker: controlamos los estados a mano

	_, out := s.notify(t, s.token, map[string]any{
		"to": []string{"log:e2e"}, "title": "t", "message": "m", "priority": "urgent",
	}, nil)
	id := out["id"].(string)

	// reclamar y dejar el TTL vencido: exactamente el estado de una entrega
	// que dura más que el TTL
	if _, err := s.Store.Pool.Exec(context.Background(),
		`UPDATE messages SET status='delivering', attempts=1, updated_at=now(),
		 expires_at=now() - interval '1 minute' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}

	expired, err := s.Store.ExpireMessages(context.Background(), 2*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(expired) != 0 {
		t.Fatalf("el expirer cortó una entrega viva: %v", expired)
	}
	var status string
	if err := s.Store.Pool.QueryRow(context.Background(),
		`SELECT status FROM messages WHERE id=$1`, id).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != model.StatusDelivering {
		t.Fatalf("status=%s, esperaba delivering", status)
	}

	// latido muerto (updated_at fuera del lease): ahora sí expira
	if _, err := s.Store.Pool.Exec(context.Background(),
		`UPDATE messages SET updated_at=now() - interval '3 minutes' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	expired, err = s.Store.ExpireMessages(context.Background(), 2*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(expired) != 1 || expired[0].ID != id {
		t.Fatalf("esperaba la expiración del mensaje con latido muerto, got %v", expired)
	}
}

// M3: la cuota por proyecto aplica la misma exclusión de send_at lejanos
// que CountPending — 2000 programados a 30 días no bloquean al proyecto con
// la cola inminente vacía.
func TestProjectQuotaIgnoresFarScheduled(t *testing.T) {
	s := newStack(t, false, func(c *config.Config) {
		c.SoftLimit, c.HardLimit, c.ProjectHardLimit = 100, 1000, 2
	})

	far := time.Now().Add(30 * 24 * time.Hour).UTC().Format(time.RFC3339)
	for i := 0; i < 2; i++ {
		resp, out := s.notify(t, s.token, map[string]any{
			"to": []string{"log:e2e"}, "title": "sched", "message": "m", "priority": "normal",
			"send_at": far,
		}, nil)
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("programado lejano %d: esperaba 202, got %d: %v", i, resp.StatusCode, out)
		}
	}

	// urgent pasa: los 2 send_at están fuera del horizonte de 1h del contador
	resp, _ := s.notify(t, s.token, map[string]any{
		"to": []string{"log:e2e"}, "title": "ahora", "message": "m", "priority": "urgent",
	}, nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("urgent bloqueado por send_at lejanos del propio proyecto: %d", resp.StatusCode)
	}

	// dirección contraria: un send_at DENTRO del horizonte SÍ cuenta — un
	// segundo programado cercano alcanza la cuota (2) y el siguiente se corta
	near := time.Now().Add(30 * time.Minute).UTC().Format(time.RFC3339)
	resp, _ = s.notify(t, s.token, map[string]any{
		"to": []string{"log:e2e"}, "title": "cerca", "message": "m", "priority": "normal",
		"send_at": near,
	}, nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("programado cercano: esperaba 202, got %d", resp.StatusCode)
	}
	resp, _ = s.notify(t, s.token, map[string]any{
		"to": []string{"log:e2e"}, "title": "ahora2", "message": "m", "priority": "urgent",
	}, nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("esperaba 503 con la cuota inminente llena, got %d", resp.StatusCode)
	}
}
