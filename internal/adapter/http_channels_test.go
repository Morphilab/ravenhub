package adapter

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRedactSecret(t *testing.T) {
	if got := redactSecret("Post \"https://api.telegram.org/bot123:ABC\"/sendMessage\": timeout", "123:ABC"); got != "Post \"https://api.telegram.org/bot***\"/sendMessage\": timeout" {
		t.Fatalf("redacción falló: %q", got)
	}
	if got := redactSecret("sin secreto", ""); got != "sin secreto" {
		t.Fatalf("vacío no debe tocar: %q", got)
	}
	if got := redactSecret("abc abc", "x"); got != "abc abc" {
		t.Fatalf("secreto ausente no debe alterar: %q", got)
	}
}

func TestParseRetryAfter(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want time.Duration
	}{
		{"vacío", "", 0},
		{"segundos", "120", 120 * time.Second},
		{"cero", "0", 0},
		{"invalido", "abc", 0},
		{"fecha http", time.Now().Add(5 * time.Minute).UTC().Format(http.TimeFormat), 5 * time.Minute},
		{"fecha pasada", time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat), 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseRetryAfter(tc.in)
			// para fechas: cojemos la ventana; para el resto exacto
			if tc.want == 0 && got != 0 {
				t.Fatalf("esperaba 0, got %v", got)
			}
			if tc.want > 0 && (got < tc.want-time.Second || got > tc.want+2*time.Minute) {
				t.Fatalf("esperaba ≈%v, got %v", tc.want, got)
			}
		})
	}
}

func TestGuardMetadataURL(t *testing.T) {
	allowed := []string{
		"https://hooks.slack.com/services/AAA/BBB",
		"https://discord.com/api/webhooks/1/2",
		"http://127.0.0.1:5678/webhook", // local-first: loopback OK
		"http://10.0.0.5:8080/hook",     // LAN privada OK
		"http://192.168.1.20:3000/hook", // LAN privada OK
		"http://localhost:8765/n8n",     // local dev OK
		"https://example.com/hook",      // público con TLS OK
	}
	blocked := []string{
		"http://example.com/hook",                       // público sin TLS
		"http://169.254.169.254/latest/meta-data/",      // metadata cloud
		"http://169.254.170.2/credentials",              // ECS metadata
		"https://[fe80::1]/x",                           // link-local IPv6
		"http://0.0.0.0:8080/x",                         // unspecified
		"ftp://example.com/x",                           // esquema no http(s)
		"http://",                                       // sin host
		"http://100.100.100.200/latest/meta-data/",      // metadata Aliyun (unicast global)
		"http://192.0.0.192/latest/meta-data/",          // Oracle Cloud
		"http://[fd00:ec2::254]/latest/meta-data/",      // IMDS IPv6 de AWS (unique-local)
		"http://[64:ff9b::a9fe:a9fe]/latest/meta-data/", // NAT64 → 169.254.169.254
		"http://[2002:a9fe:a9fe::]/latest/meta-data/",   // 6to4 → 169.254.169.254
	}
	for _, u := range allowed {
		if err := guardMetadataURL(context.Background(), u); err != nil {
			t.Errorf("%q debería estar permitida: %v", u, err)
		}
	}
	for _, u := range blocked {
		if err := guardMetadataURL(context.Background(), u); err == nil {
			t.Errorf("%q debería estar bloqueada", u)
		}
	}
}

// Los errores de parseo de URL (url.Parse / NewRequestWithContext) incrustan
// la URL cruda — y en Slack/Discord la URL ES el secreto (token en el path):
// ninguna ruta de error puede devolverla sin redactar.
func TestWebhookURLOperatorsRedactedOnParseError(t *testing.T) {
	const token = "SECRETTOKEN123"
	cases := []struct {
		name  string
		creds string
	}{
		{"guard (parse en el guard)", `{"url":"https://discord.com/api/webhooks/1/2/` + token + `%zz"}`},
		{"allow_internal (parse en NewRequest)", `{"url":"https://discord.com/api/webhooks/1/2/` + token + `%zz","allow_internal":true}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := (DiscordAdapter{}).Send(context.Background(), SendInput{
				Creds:  json.RawMessage(tc.creds),
				Target: json.RawMessage(`{}`),
			})
			if err == nil {
				t.Fatal("esperaba error de URL inválida")
			}
			if strings.Contains(err.Error(), token) {
				t.Fatalf("el error filtra el token del webhook: %q", err)
			}
			if !strings.Contains(err.Error(), "***") {
				t.Fatalf("el error debería mostrar la redacción: %q", err)
			}
		})
	}
}

// Un 307/308 no puede reenviar el secreto del webhook (ni el bot token de
// Telegram) a otro host. El cliente con allow_internal no debe seguirlos.
func TestWebhookDoesNotFollowRedirect(t *testing.T) {
	var hit atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit.Store(true)
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()

	creds, _ := json.Marshal(map[string]any{"url": redirect.URL, "allow_internal": true})
	_, err := (DiscordAdapter{}).Send(context.Background(), SendInput{
		Creds:  creds,
		Target: json.RawMessage(`{}`),
	})
	if err == nil {
		t.Fatal("el adaptador aceptó un redirect")
	}
	if hit.Load() {
		t.Fatal("el adaptador siguió el redirect y envió el secreto al segundo host")
	}
}
