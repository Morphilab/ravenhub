package api

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestClientIPIgnoresForwardedFor(t *testing.T) {
	// el throttle de autenticación se ancla en RemoteAddr; X-Forwarded-For
	// es cliente-controlable y no debe usarse como clave.
	r, _ := http.NewRequest("GET", "/", nil)
	r.RemoteAddr = "203.0.113.7:44321"
	for _, xff := range []string{"", "1.2.3.4", "9.9.9.9, 1.2.3.4"} {
		if xff != "" {
			r.Header.Set("X-Forwarded-For", xff)
		}
		if ip := clientIP(r); ip != "203.0.113.7" {
			t.Fatalf("clientIP debe usar RemoteAddr, no XFF (%q): got %q", xff, ip)
		}
	}
}

func TestAllTagsPresent(t *testing.T) {
	if !allTagsPresent([]string{"error", "prod"}, []string{"prod", "error", "x"}) {
		t.Fatal("todos los tags requeridos están, debería pasar")
	}
	if allTagsPresent([]string{"error", "prod"}, []string{"error"}) {
		t.Fatal("falta prod, debería fallar")
	}
	if !allTagsPresent(nil, nil) {
		t.Fatal("sin requisitos siempre pasa")
	}
}

func TestPayloadSubset(t *testing.T) {
	data := map[string]any{"service": "scraper", "exit_code": float64(137), "x": "y"}
	if !payloadSubset(map[string]any{"service": "scraper", "exit_code": float64(137)}, data) {
		t.Fatal("subconjunto exacto debería coincidir")
	}
	if payloadSubset(map[string]any{"service": "otro"}, data) {
		t.Fatal("valor distinto no debe coincidir")
	}
	if payloadSubset(map[string]any{"inexistente": 1}, data) {
		t.Fatal("clave ausente no debe coincidir")
	}
	if !payloadSubset(nil, data) {
		t.Fatal("sin condiciones siempre pasa")
	}
}

func TestParseSendAt(t *testing.T) {
	if v, err := parseSendAt(""); err != nil || v != nil {
		t.Fatalf("vacío → nil, nil; got %v, %v", v, err)
	}
	v, err := parseSendAt("2026-10-12T20:00:00Z")
	if err != nil || v == nil || !strings.HasPrefix(v.Format(time.RFC3339), "2026-10-12T20:00:00") {
		t.Fatalf("RFC3339 válido falló: %v %v", v, err)
	}
	if _, err := parseSendAt("mañana"); err == nil {
		t.Fatal("formato inválido debe fallar")
	}
}
