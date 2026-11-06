package metrics

import (
	"strings"
	"testing"
)

func TestRenderTypePerFamily(t *testing.T) {
	Inc("ravenhub_test_requests_total", "code", "200")
	Inc("ravenhub_test_requests_total", "code", "429")
	SetGauge("ravenhub_test_queue_depth", 7)
	out := Render()

	// un TYPE por familia real, no familias inventadas
	if !strings.Contains(out, "# TYPE ravenhub_test_requests_total counter") {
		t.Fatalf("sin TYPE de la familia counter:\n%s", out)
	}
	if !strings.Contains(out, "# TYPE ravenhub_test_queue_depth gauge") {
		t.Fatalf("sin TYPE de la familia gauge:\n%s", out)
	}
	if strings.Contains(out, "ravenhub_total") || strings.Contains(out, "ravenhub_gauge") {
		t.Fatalf("quedan familias fantasma:\n%s", out)
	}
	// las series van debajo de su TYPE, con sus labels intactos
	if !strings.Contains(out, `ravenhub_test_requests_total{code="200"}`) {
		t.Fatalf("serie con labels ausente:\n%s", out)
	}
}
