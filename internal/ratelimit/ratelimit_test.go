package ratelimit

import (
	"fmt"
	"testing"
	"time"
)

func TestBucketBurstThenDeny(t *testing.T) {
	b := NewBucket(60, 3) // 1/s, burst 3
	for i := 0; i < 3; i++ {
		if !b.Allow() {
			t.Fatalf("iteración %d denegada dentro del burst", i)
		}
	}
	if b.Allow() {
		t.Fatal("cuarta petición permitida fuera del burst")
	}
	if b.RetryAfter() <= 0 || b.RetryAfter() > time.Second {
		t.Fatalf("RetryAfter fuera de rango: %s", b.RetryAfter())
	}
}

func TestBucketRefills(t *testing.T) {
	b := NewBucket(6000, 1) // 100/s
	if !b.Allow() {
		t.Fatal("primer allow denegado")
	}
	if b.Allow() {
		t.Fatal("segundo allow inmediato debería denegarse")
	}
	time.Sleep(15 * time.Millisecond) // ~1.5 tokens
	if !b.Allow() {
		t.Fatal("token no repuso tras esperar")
	}
}

func TestRegistrySameBucketPerKey(t *testing.T) {
	r := NewRegistry()
	a := r.Get("dest-1", 60, 1)
	b := r.Get("dest-1", 60, 1)
	if a != b {
		t.Fatal("registry devolvió buckets distintos para la misma clave")
	}
	a.Allow()
	if a.Allow() {
		t.Fatal("bucket compartido no respeta burst")
	}
	c := r.Get("dest-2", 60, 10)
	if !c.Allow() {
		t.Fatal("otra clave heredó el estado de la primera")
	}
}

func TestRegistryUpdatesLimits(t *testing.T) {
	r := NewRegistry()
	b1 := r.Get("k", 60, 10)
	b2 := r.Get("k", 60, 10)
	if b1 != b2 {
		t.Fatal("misma clave, límites iguales: bucket distinto")
	}
	b3 := r.Get("k", 600, 100)
	if b3 == b1 {
		t.Fatal("límites cambiados sin recrear bucket (config congelada)")
	}
}

// LRU O(1): al superar la capacidad se evicta el visto menos recientemente
// — no un escaneo de todo el mapa — y refrescar una clave la salva.
func TestRegistryLRUEviction(t *testing.T) {
	r := NewRegistry()
	for i := 0; i < r.limit; i++ {
		r.Get(fmt.Sprintf("k-%d", i), 60, 10)
	}
	// refresca la primera: deja de ser candidata a evicción
	r.Get("k-0", 60, 10)
	// una clave más fuerza UNA evicción: debe ser k-1 (la más vieja ahora)
	r.Get("nueva", 60, 10)
	if len(r.buckets) != r.limit {
		t.Fatalf("capacidad: %d, esperaba %d", len(r.buckets), r.limit)
	}
	if _, ok := r.buckets["k-1"]; ok {
		t.Fatal("k-1 (menos reciente) debería haber sido evictada")
	}
	if _, ok := r.buckets["k-0"]; !ok {
		t.Fatal("k-0 refrescada no debería evictarse")
	}
	if _, ok := r.buckets["nueva"]; !ok {
		t.Fatal("la clave nueva debería existir")
	}
}
