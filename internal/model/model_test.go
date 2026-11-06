package model

import (
	"encoding/json"
	"testing"
	"time"
)

func TestParsePriority(t *testing.T) {
	if p, _ := ParsePriority(""); p != PriorityNormal {
		t.Fatal("vacío debería ser normal")
	}
	if p, _ := ParsePriority("URGENT"); p != PriorityUrgent {
		t.Fatal("case-insensitivity rota")
	}
	if _, err := ParsePriority("muy-alta"); err == nil {
		t.Fatal("prioridad inválida aceptada")
	}
}

func TestTTLFor(t *testing.T) {
	if TTLFor(PriorityUrgent, nil) != 15*time.Minute {
		t.Fatal("TTL urgent por defecto")
	}
	if TTLFor(PriorityLow, nil) != 24*time.Hour {
		t.Fatal("TTL low por defecto")
	}
	opts, _ := json.Marshal(map[string]any{"ttl": map[string]string{"urgent": "10m"}})
	if TTLFor(PriorityUrgent, opts) != 10*time.Minute {
		t.Fatal("override de proyecto no aplicado")
	}
}

func TestBackoffOrdering(t *testing.T) {
	prev := time.Duration(0)
	for i := 1; i <= 4; i++ {
		d := BackoffFor(PriorityNormal, i)
		if d <= prev && i > 1 {
			t.Fatalf("backoff no creciente en intento %d: %v <= %v", i, d, prev)
		}
		prev = d
	}
	// jitter ±20% sobre el primer escalón de normal (con tolerancia de 1ms:
	// el ±20% flotante no es exacto en binario)
	base := time.Duration(backoffSchedules[PriorityNormal][0]) * time.Second
	d := BackoffFor(PriorityNormal, 1)
	if d < base-base/5-time.Millisecond || d > base+base/5+time.Millisecond {
		t.Fatalf("jitter fuera de rango: %v (base %v)", d, base)
	}
	// attempts fuera de rango usa el último escalón
	last := BackoffFor(PriorityNormal, 999)
	if last <= 0 {
		t.Fatal("backoff no positivo tras agotar escalones")
	}
}

// TestRetryBudgetFitsTTL: la suma de las esperas con jitter máximo (+20%)
// cabe dentro del TTL por defecto de la prioridad. Un presupuesto más largo
// que el TTL expiraba intentos en silencio (urgent: el 6º intento caía a los
// ~18 min con TTL de 15m). Con overrides de TTL por proyecto, el worker corta
// a DLQ de forma visible.
func TestRetryBudgetFitsTTL(t *testing.T) {
	for _, p := range []Priority{PriorityLow, PriorityNormal, PriorityHigh, PriorityUrgent} {
		var total time.Duration
		for _, w := range backoffSchedules[p] {
			total += time.Duration(float64(w)*1.2*float64(time.Second)) + time.Second
		}
		ttl := TTLFor(p, nil)
		if total >= ttl {
			t.Fatalf("presupuesto de reintentos de %s (%v) no cabe en su TTL (%v)", p, total, ttl)
		}
	}
}

// La validación de options falla rápido en el borde: un typo o un valor
// ilegible no puede llegar a la DB y "no hacer nada" en silencio.
func TestValidateProjectOptions(t *testing.T) {
	ok := []string{
		``,
		`{}`,
		`{"ttl":{"urgent":"10m","low":"48h"}}`,
		`{"digest_window":"10m"}`,
		`{"ttl":{"normal":"2h"},"digest_window":"1s"}`,
	}
	for _, s := range ok {
		if err := ValidateProjectOptions(json.RawMessage(s)); err != nil {
			t.Fatalf("options válidas rechazadas %s: %v", s, err)
		}
	}
	bad := []string{
		`{"ttl":{"URGENT":"10m"}}`,  // case-sensitive en TTLFor → jamás aplicaría
		`{"ttl":{"urgent":"-5m"}}`,  // duración no positiva
		`{"ttl":{"urgent":"abc"}}`,  // duración ilegible
		`{"ttl":{"alta":"10m"}}`,    // prioridad inexistente
		`{"digest_window":"500ms"}`, // por debajo del mínimo
		`{"digest_windows":"10m"}`,  // typo: clave desconocida
		`{"ttl":[]}`,                // forma inválida
		`[1,2]`,                     // no es objeto
	}
	for _, s := range bad {
		if err := ValidateProjectOptions(json.RawMessage(s)); err == nil {
			t.Fatalf("options inválidas aceptadas: %s", s)
		}
	}
}

func TestMaxAttempts(t *testing.T) {
	if MaxAttempts(PriorityUrgent) != 6 {
		t.Fatal("urgent debería tener 6 intentos")
	}
}

func TestScopes(t *testing.T) {
	s := Scopes{Destinations: []string{"oncall"}, MaxPriority: PriorityHigh}
	if !s.AllowsDestination("oncall") || s.AllowsDestination("otro") {
		t.Fatal("filtro de destinos roto")
	}
	if !s.AllowsPriority(PriorityHigh) || s.AllowsPriority(PriorityUrgent) {
		t.Fatal("techo de prioridad roto")
	}
	all := Scopes{}
	if !all.AllowsDestination("cualquiera") || !all.AllowsPriority(PriorityUrgent) {
		t.Fatal("scopes vacíos deberían permitir todo")
	}
}

func TestAggregationKeyNormalizes(t *testing.T) {
	a := AggregationKey("Error en pipeline", []string{"error", "prod"}, []string{"log:a", "log:b"})
	b := AggregationKey("  error en pipeline ", []string{"prod", "error"}, []string{"log:b", "log:a"})
	if a != b {
		t.Fatalf("claves deberían colapsar: %q vs %q", a, b)
	}
	if a == AggregationKey("Otro título", []string{"error", "prod"}, []string{"log:a", "log:b"}) {
		t.Fatal("títulos distintos no deben colapsar")
	}
	if AggregationKey("a|b", nil, []string{"c"}) == AggregationKey("a", []string{"b"}, []string{"c"}) {
		t.Fatal("AggregationKey colisiona por delimitadores")
	}
	if a == AggregationKey("Error en pipeline", []string{"error", "prod"}, []string{"log:a"}) {
		t.Fatal("destinos distintos no deben colapsar")
	}
}

func TestDigestWindow(t *testing.T) {
	if w, on := DigestWindow(nil); on || w != 0 {
		t.Fatal("sin opciones no debe haber digest")
	}
	if w, on := DigestWindow(json.RawMessage(`{"digest_window":"abc"}`)); on || w != 0 {
		t.Fatal("ventana inválida no debe activar digest")
	}
	w, on := DigestWindow(json.RawMessage(`{"digest_window":"10m"}`))
	if !on || w != 10*time.Minute {
		t.Fatalf("digest 10m: on=%v w=%v", on, w)
	}
}

func TestAllowsPriorityInvalidMaxIsFailClosed(t *testing.T) {
	s := Scopes{MaxPriority: Priority("URGENT")} // fila legacy sin normalizar
	for _, p := range []Priority{PriorityLow, PriorityNormal, PriorityHigh, PriorityUrgent} {
		if s.AllowsPriority(p) {
			t.Fatalf("MaxPriority inválido no bloqueó %s (fail-open)", p)
		}
	}
	if !(Scopes{MaxPriority: PriorityNormal}).AllowsPriority(PriorityNormal) {
		t.Fatal("normal con max normal bloqueado")
	}
	if (Scopes{MaxPriority: PriorityNormal}).AllowsPriority(PriorityHigh) {
		t.Fatal("high con max normal permitido")
	}
}
