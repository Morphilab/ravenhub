// Package metrics implements a minimal Prometheus text-format registry
// (counters + gauges) without external dependencies.
package metrics

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

var (
	mu       sync.Mutex
	counters = map[string]float64{}
	labels   = map[string]string{} // series key -> label string for HELP/TYPE
	gauges   = map[string]float64{}
)

func seriesKey(name string, kv ...string) string {
	if len(kv) == 0 {
		return name
	}
	var b strings.Builder
	b.WriteString(name)
	b.WriteByte('{')
	for i := 0; i < len(kv)-1; i += 2 {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%s=%q", kv[i], kv[i+1])
	}
	b.WriteByte('}')
	return b.String()
}

// Inc adds 1 (or delta) to a counter series.
func Inc(name string, kv ...string) { Add(name, 1, kv...) }

// Prewarm registra en cero las series clave del contrato de silencio: un
// contador AUSENTE no es lo mismo que un contador en 0 para Prometheus
// (rate()/increase() y alertas por ausencia se comportan distinto). Los sets
// son acotados (labels enumeradas: 4 prioridades, 6 canales, 4 scope/code,
// 4 resultados), así que pre-registrarlos no explota cardinalidad.
func Prewarm() {
	for _, p := range []string{"low", "normal", "high", "urgent"} {
		Add("ravenhub_expired_total", 0, "priority", p)
	}
	for _, ch := range []string{"telegram", "email", "log", "webhook", "discord", "slack"} {
		Add("ravenhub_truncated_total", 0, "channel", ch)
	}
	for _, sc := range []struct{ scope, code string }{
		{"intake", "429"}, {"project", "429"}, {"project", "503"}, {"global", "429"}, {"global", "503"},
	} {
		Add("ravenhub_shed_total", 0, "scope", sc.scope, "code", sc.code)
	}
	for _, r := range []string{"sent", "failed", "dead", "expired"} {
		Add("ravenhub_messages_total", 0, "result", r)
	}
}

// Add adds delta to a counter series.
func Add(name string, delta float64, kv ...string) {
	key := seriesKey(name, kv...)
	mu.Lock()
	counters[key] += delta
	labels[key] = name
	mu.Unlock()
}

// SetGauge sets the value of a gauge series.
func SetGauge(name string, v float64) {
	mu.Lock()
	gauges[name] = v
	mu.Unlock()
}

// Render produces the Prometheus text exposition of all series, grouped by
// metric family (the name without labels) with one TYPE line per family.
func Render() string {
	mu.Lock()
	defer mu.Unlock()
	var b strings.Builder
	writeFamilies := func(typ string, samples map[string]float64) {
		families := map[string][]string{}
		for k := range samples {
			fam := k
			if i := strings.IndexByte(k, '{'); i >= 0 {
				fam = k[:i]
			}
			families[fam] = append(families[fam], k)
		}
		fams := make([]string, 0, len(families))
		for f := range families {
			fams = append(fams, f)
		}
		sort.Strings(fams)
		for _, fam := range fams {
			fmt.Fprintf(&b, "# TYPE %s %s\n", fam, typ)
			keys := families[fam]
			sort.Strings(keys)
			for _, k := range keys {
				fmt.Fprintf(&b, "%s %v\n", k, samples[k])
			}
		}
	}
	writeFamilies("counter", counters)
	writeFamilies("gauge", gauges)
	return b.String()
}
