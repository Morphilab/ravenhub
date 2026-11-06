// Package ratelimit implements in-memory token buckets used for
// per-project intake limiting and per-destination send pacing.
package ratelimit

import (
	"container/list"
	"sync"
	"time"
)

// Bucket is a classic token bucket: rate tokens/second, capacity burst.
// Thread-safe: varios workers comparten el bucket de un mismo destino.
type Bucket struct {
	mu     sync.Mutex
	rate   float64 // tokens per second
	burst  float64
	tokens float64
	last   time.Time
}

// NewBucket creates a bucket filled to burst.
func NewBucket(perMinute, burst int) *Bucket {
	return &Bucket{
		rate:   float64(perMinute) / 60.0,
		burst:  float64(burst),
		tokens: float64(burst),
		last:   time.Now(),
	}
}

func (b *Bucket) refillLocked() {
	now := time.Now()
	elapsed := now.Sub(b.last).Seconds()
	if elapsed > 0 { // el reloj nunca retrocede dentro del bucket
		b.tokens += elapsed * b.rate
		if b.tokens > b.burst {
			b.tokens = b.burst
		}
	}
	b.last = now
}

// Allow consumes one token; false means the caller should wait RetryAfter.
func (b *Bucket) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refillLocked()
	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

// RetryAfter returns how long until a token is available.
func (b *Bucket) RetryAfter() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refillLocked()
	if b.tokens >= 1 {
		return 0
	}
	need := 1 - b.tokens
	if b.rate <= 0 {
		return time.Minute
	}
	return time.Duration(need / b.rate * float64(time.Second))
}

// Registry holds named buckets with lazy creation (thread-safe). Con
// expiración y actualización de límites. La evicción por capacidad es LRU
// O(1) (lista doblemente enlazada): un flujo de claves únicas no puede
// estancar el mutex global escaneando 10 000 entradas por cada inserción.
type Registry struct {
	mu      sync.Mutex
	buckets map[string]*list.Element
	order   *list.List // front = visto recientemente; back = candidato a evicción
	limit   int
}

type entry struct {
	key   string
	b     *Bucket
	rate  int
	burst int
}

func NewRegistry() *Registry {
	return &Registry{buckets: map[string]*list.Element{}, order: list.New(), limit: 10000}
}

// Get returns the bucket for key, creating it with the given limits if absent.
// Si ya existe pero los límites cambiaron, los actualiza (bucket nuevo).
func (r *Registry) Get(key string, perMinute, burst int) *Bucket {
	r.mu.Lock()
	defer r.mu.Unlock()
	if el, ok := r.buckets[key]; ok {
		e := el.Value.(*entry)
		r.order.MoveToFront(el)
		if e.rate != perMinute || e.burst != burst {
			e.b = NewBucket(perMinute, burst)
			e.rate, e.burst = perMinute, burst
		}
		return e.b
	}
	for r.order.Len() >= r.limit {
		oldest := r.order.Back()
		if oldest == nil {
			break
		}
		delete(r.buckets, oldest.Value.(*entry).key)
		r.order.Remove(oldest)
	}
	b := NewBucket(perMinute, burst)
	r.buckets[key] = r.order.PushFront(&entry{key: key, b: b, rate: perMinute, burst: burst})
	return b
}
