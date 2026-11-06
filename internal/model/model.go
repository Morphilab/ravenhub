// Package model defines the shared domain types of RavenHub.
package model

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"sort"
	"strings"
	"time"
)

// Priority ranks notifications. Higher rank = more urgent.
type Priority string

const (
	PriorityLow    Priority = "low"
	PriorityNormal Priority = "normal"
	PriorityHigh   Priority = "high"
	PriorityUrgent Priority = "urgent"
)

// Message lifecycle states.
const (
	StatusQueued     = "queued"
	StatusDelivering = "delivering"
	StatusSent       = "sent"
	StatusFailed     = "failed"
	StatusDead       = "dead"
	StatusExpired    = "expired"
)

// Channel names.
const (
	ChannelTelegram = "telegram"
	ChannelEmail    = "email"
	ChannelLog      = "log"
	ChannelWebhook  = "webhook"
	ChannelDiscord  = "discord"
	ChannelSlack    = "slack"
)

// RoutingRule defines a tag/priority-based destination routing rule
// (`priority=urgent → oncall`). Evaluated by the Hub at intake,
// only to ADD destinations: the consumer never decides the channel.
type RoutingRule struct {
	ID             string         `json:"id"`
	Name           string         `json:"name"`
	MatchTags      []string       `json:"match_tags"`     // all must be present (AND)
	MatchPriority  string         `json:"match_priority"` // minimum priority (optional)
	MatchPayload   map[string]any `json:"match_payload"`  // subset of data (optional)
	Destinations   []string       `json:"destinations"`   // destinations/groups to add
	StopProcessing bool           `json:"stop_processing"`
	Enabled        bool           `json:"enabled"`
	Priority       int            `json:"priority"` // evaluation order, lower first
}

var priorityRank = map[Priority]int{
	PriorityLow:    0,
	PriorityNormal: 1,
	PriorityHigh:   2,
	PriorityUrgent: 3,
}

// ParsePriority validates and normalizes a priority string.
func ParsePriority(s string) (Priority, error) {
	p := Priority(strings.ToLower(strings.TrimSpace(s)))
	if p == "" {
		return PriorityNormal, nil
	}
	if _, ok := priorityRank[p]; !ok {
		return "", fmt.Errorf("prioridad inválida %q (low|normal|high|urgent)", s)
	}
	return p, nil
}

// Rank returns the ordering rank of a priority.
func (p Priority) Rank() int { return priorityRank[p] }

// ValidPriorities lists allowed values for error messages.
func ValidPriorities() string { return "low|normal|high|urgent" }

// ttlDefaults returns the message TTL per priority.
var ttlDefaults = map[Priority]time.Duration{
	PriorityLow:    24 * time.Hour,
	PriorityNormal: time.Hour,
	PriorityHigh:   time.Hour,
	PriorityUrgent: 15 * time.Minute,
}

// backoffSchedules are the retry delays per priority, in seconds.
//
// Invariante (test TestRetryBudgetFitsTTL): la suma de las esperas con el
// jitter máximo (+20%) cabe dentro del TTL por defecto de la prioridad — un
// presupuesto más largo que el TTL expiraba intentos en silencio (urgent
// tenía 6 intentos programados pero el 6º caía a los ~18 min con TTL de 15).
// Con overrides de TTL por proyecto el worker corta a DLQ de forma visible.
var backoffSchedules = map[Priority][]int{
	PriorityLow:    {300, 1800, 7200, 21600},
	PriorityNormal: {30, 90, 240, 600, 1200},
	PriorityHigh:   {15, 45, 120, 300, 600, 1200},
	PriorityUrgent: {10, 20, 45, 90, 180, 300},
}

// TTLFor returns the message TTL for a priority, honoring optional project
// overrides {"ttl": {"urgent": "10m", ...}}.
func TTLFor(p Priority, projectOptions json.RawMessage) time.Duration {
	if len(projectOptions) > 0 {
		var opts struct {
			TTL map[Priority]string `json:"ttl"`
		}
		if json.Unmarshal(projectOptions, &opts) == nil {
			if raw, ok := opts.TTL[p]; ok {
				if d, err := time.ParseDuration(raw); err == nil && d > 0 {
					return d
				}
			}
		}
	}
	return ttlDefaults[p]
}

// BackoffFor returns the retry delay after `attempt` failed attempts
// (1-based attempt count), with ±20% jitter.
func BackoffFor(p Priority, attempt int) time.Duration {
	sched := backoffSchedules[p]
	if len(sched) == 0 {
		return time.Minute
	}
	i := attempt - 1
	if i < 0 {
		i = 0
	}
	if i >= len(sched) {
		i = len(sched) - 1
	}
	base := time.Duration(sched[i]) * time.Second
	jitter := time.Duration(float64(base) * 0.2 * (2*rand.Float64() - 1))
	d := base + jitter
	if d < time.Second {
		d = time.Second
	}
	return d
}

// MaxAttempts is the retry budget for a priority before going to the DLQ.
func MaxAttempts(p Priority) int { return len(backoffSchedules[p]) }

// Payload limits enforced at intake.
const (
	TitleMaxLen     = 255
	MessageMaxLen   = 8 * 1024
	PayloadMaxBytes = 64 * 1024
)

// Scopes bound what a project token may do.
type Scopes struct {
	Admin        bool     `json:"admin,omitempty"`
	Destinations []string `json:"destinations,omitempty"` // null/empty = all destinations of the project
	MaxPriority  Priority `json:"max_priority,omitempty"`
}

// AllowsDestination reports whether the token scope covers the named destination.
func (s Scopes) AllowsDestination(name string) bool {
	if len(s.Destinations) == 0 {
		return true
	}
	for _, d := range s.Destinations {
		if d == name {
			return true
		}
	}
	return false
}

// AllowsPriority reports whether the token may use the requested priority.
// Un MaxPriority inválido (p.ej. "URGENT" de una fila legacy) se trata como
// fail-closed: solo se permite si el valor está normalizado.
func (s Scopes) AllowsPriority(p Priority) bool {
	if s.MaxPriority == "" {
		return true
	}
	// Fail-closed: una fila legacy con "HIGH"/" high" no puede quedar sin
	// techo. Los caminos de escritura ya normalizan; esto cubre DB legacy.
	rank, ok := priorityRank[s.MaxPriority]
	if !ok {
		return false
	}
	return p.Rank() <= rank
}

// DigestMaxCount caps a single digest before a new window is opened
// (anti-spam: un loop desbocado no debe crear un digest infinito).
const DigestMaxCount = 100

// ValidateProjectOptions valida el JSON de options de un proyecto en el borde
// (admin API / seed). Claves conocidas: "ttl" (mapa prioridad→duración) y
// "digest_window" (duración ≥1s). Un typo o un valor ilegible FALLA rápido:
// el runtime cae a los defaults en silencio, así que un option inválido no
// puede llegar a la DB (fail-fast > config que "no hace nada").
func ValidateProjectOptions(raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}
	if strings.TrimSpace(string(raw)) == "null" {
		return fmt.Errorf("options no puede ser null (usa {} para vacío)")
	}
	var opts map[string]json.RawMessage
	if err := json.Unmarshal(raw, &opts); err != nil {
		return fmt.Errorf("options debe ser un objeto JSON: %w", err)
	}
	for k, v := range opts {
		switch k {
		case "ttl":
			var ttl map[string]string
			if err := json.Unmarshal(v, &ttl); err != nil {
				return fmt.Errorf(`options.ttl debe ser {"prioridad":"duración"} (ej: {"urgent":"10m"}): %w`, err)
			}
			for p, d := range ttl {
				pr := Priority(strings.ToLower(strings.TrimSpace(p)))
				if _, ok := priorityRank[pr]; !ok {
					return fmt.Errorf("options.ttl: prioridad inválida %q (%s)", p, ValidPriorities())
				}
				if pr != Priority(p) {
					return fmt.Errorf("options.ttl: la prioridad %q debe ir en minúsculas (TTLFor es case-sensitive)", p)
				}
				dur, err := time.ParseDuration(d)
				if err != nil || dur <= 0 {
					return fmt.Errorf("options.ttl.%s: duración inválida %q (ej: \"10m\")", p, d)
				}
			}
		case "digest_window":
			var s string
			if err := json.Unmarshal(v, &s); err != nil {
				return fmt.Errorf(`options.digest_window debe ser una duración (ej: "10m")`)
			}
			if s != "" {
				dur, err := time.ParseDuration(s)
				if err != nil || dur < time.Second {
					return fmt.Errorf("options.digest_window inválido %q (mínimo 1s)", s)
				}
			}
		default:
			return fmt.Errorf("options: clave desconocida %q (conocidas: ttl, digest_window)", k)
		}
	}
	return nil
}

// DigestWindow returns the project's anti-spam aggregation window and whether
// it is enabled. Opt-in via project options {"digest_window": "10m"}: sin
// ventana, el proyecto no agrega nada (comportamiento por defecto intacto).
// Solo aplica a low/normal; high/urgent jamás se agregan.
func DigestWindow(projectOptions json.RawMessage) (time.Duration, bool) {
	if len(projectOptions) > 0 {
		var opts struct {
			DigestWindow string `json:"digest_window"`
		}
		if json.Unmarshal(projectOptions, &opts) == nil && opts.DigestWindow != "" {
			if d, err := time.ParseDuration(opts.DigestWindow); err == nil && d >= time.Second {
				return d, true
			}
		}
	}
	return 0, false
}

// AggregationKey generates a deterministic key for message aggregation
// ("N mensajes con mismo título+tags en una ventana"). Incluye
// los destinos para no fusionar mensajes idénticos dirigidos a sitios
// distintos, y normaliza tags/orden para que equivalencias triviales
// (["a","b"] vs ["b","a"]) colapsen juntas.
func AggregationKey(title string, tags, destinations []string) string {
	t := append([]string(nil), tags...)
	sort.Strings(t)
	d := append([]string(nil), destinations...)
	sort.Strings(d)
	// JSON/length-prefix: el join con "|" y "," colisionaba.
	raw, _ := json.Marshal(struct {
		Title        string   `json:"title"`
		Tags         []string `json:"tags"`
		Destinations []string `json:"destinations"`
	}{
		Title:        strings.ToLower(strings.TrimSpace(title)),
		Tags:         t,
		Destinations: d,
	})
	return string(raw)
}
