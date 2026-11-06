package render

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestTelegramHTMLEscaping(t *testing.T) {
	n := Normalized{
		Title:    "Error <grave> & \"crítico\"",
		Message:  "linea con <tags> y &",
		Priority: "high",
		Tags:     []string{"error", "prod"},
		Data:     map[string]any{"service": "scr<b>aper"},
	}
	out, truncated := TelegramHTML(n)
	if truncated {
		t.Fatal("mensaje corto no debería reportar truncado")
	}
	if strings.Contains(out, "<grave>") || strings.Contains(out, "&\"") {
		t.Fatalf("HTML sin escapar: %s", out)
	}
	if !strings.Contains(out, "&lt;grave&gt;") || !strings.Contains(out, "&amp;") {
		t.Fatalf("escape esperado ausente: %s", out)
	}
	if !strings.Contains(out, "<b>") {
		t.Fatalf("título sin negrita: %s", out)
	}
}

func TestTelegramTruncation(t *testing.T) {
	n := Normalized{Title: "t", Message: strings.Repeat("x", 6000)}
	out, truncated := TelegramHTML(n)
	if len(out) > 4096+3 { // … multibyte
		t.Fatalf("no truncó a 4096: %d", len(out))
	}
	if !strings.HasSuffix(out, "…") {
		t.Fatal("sin indicador de truncado")
	}
	// el truncado debe REPORTARSE (la entrega lo persiste en deliveries.truncated)
	if !truncated {
		t.Fatal("truncado no reportado al caller")
	}
}

// El truncado no debe partir una entidad HTML: Telegram rechaza el mensaje
// COMPLETO si llega HTML malformado (violación de "mejor feo que no notificar").
func TestTelegramTruncationEntitySafe(t *testing.T) {
	// 2000 '&' → 12000 bytes escapados: el corte cae en medio de &
	out, _ := TelegramHTML(Normalized{Title: "t", Message: strings.Repeat("&", 2000)})
	if len(out) > 4096+3 {
		t.Fatalf("no truncó a 4096: %d", len(out))
	}
	if strings.HasSuffix(out, "&") || strings.HasSuffix(strings.TrimSuffix(out, "…"), "&") {
		t.Fatalf("entidad cortada en el límite: %q", out[len(out)-16:])
	}
	// ninguna entidad incompleta al final (& sin ';' detrás)
	tail := out
	if i := strings.LastIndexByte(tail, '&'); i >= 0 && !strings.Contains(tail[i:], ";") {
		t.Fatalf("entidad incompleta tras el truncado: %q", tail[i:])
	}
}

func TestApplyTemplate(t *testing.T) {
	n := Normalized{
		Title:   "Error",
		Message: "raw",
		Data:    map[string]any{"service": "scraper", "exit_code": 137},
		Tags:    []string{"a", "b"},
	}
	out, err := ApplyTemplate("El servicio {{service}} murió ({{exit_code}}) — {{tags}}", n)
	if err != nil {
		t.Fatal(err)
	}
	want := "El servicio scraper murió (137) — a, b"
	if out != want {
		t.Fatalf("got %q want %q", out, want)
	}
	// variable ausente → error → el caller hace fallback a texto plano
	if _, err := ApplyTemplate("{{inexistente}}", n); err == nil {
		t.Fatal("variable inexistente no falló")
	}
	if _, err := ApplyTemplate("{{title}} {{priority}}", n); err != nil {
		t.Fatalf("vars especiales fallaron: %v", err)
	}
}

func TestEmailSubjectUrgent(t *testing.T) {
	if got, _ := EmailSubject(Normalized{Title: "caída", Priority: "urgent"}); got != "[URGENTE] caída" {
		t.Fatalf("subject: %q", got)
	}
	if got, _ := EmailSubject(Normalized{Title: "caída", Priority: "normal"}); got != "caída" {
		t.Fatalf("subject normal: %q", got)
	}
	// subject desmedido: truncado y reportado
	got, truncated := EmailSubject(Normalized{Title: strings.Repeat("x", 500)})
	if utf8.RuneCountInString(got) > 255 || !truncated {
		t.Fatalf("subject no truncado/reportado: runes=%d truncated=%v", utf8.RuneCountInString(got), truncated)
	}
}

func TestQuietHoursWrapMidnight(t *testing.T) {
	q, err := ParseQuietHours([]byte(`{"start":"23:00","end":"08:00","tz":"Europe/Madrid"}`))
	if err != nil {
		t.Fatal(err)
	}
	loc, _ := time.LoadLocation("Europe/Madrid")
	// 01:00 Madrid → dentro de la ventana
	inside := time.Date(2026, 10, 11, 1, 0, 0, 0, loc)
	if !q.ActiveAt(inside) {
		t.Fatal("01:00 debería estar en silencio (23:00–08:00)")
	}
	// 12:00 Madrid → fuera
	noon := time.Date(2026, 10, 11, 12, 0, 0, 0, loc)
	if q.ActiveAt(noon) {
		t.Fatal("12:00 no debería estar en silencio")
	}
	// NextOpen desde dentro de la ventana apunta a ~08:00 del mismo día
	next := q.NextOpen(inside)
	if next.Hour() != 8 {
		t.Fatalf("NextOpen apunta a %02d:%02d, esperaba 08:xx", next.Hour(), next.Minute())
	}
	if q.ActiveAt(noon) && q.NextOpen(noon).Before(noon) {
		t.Fatal("NextOpen regresivo")
	}
}

func TestParseQuietHoursValidation(t *testing.T) {
	if _, err := ParseQuietHours(nil); err != nil {
		t.Fatal("nil debe ser válido (sin quiet hours)")
	}
	if _, err := ParseQuietHours([]byte(`{"start":"25:00","end":"08:00"}`)); err == nil {
		t.Fatal("hora inválida aceptada")
	}
	if _, err := ParseQuietHours([]byte(`{"start":"22:00","end":"08:00","tz":"Marte/Olympus"}`)); err == nil {
		t.Fatal("tz inválida aceptada")
	}
}

// El truncado debe dejar el resultado DENTRO del límite del proveedor
// contando la elipsis: devolver max+1 caracteres hacía que Telegram
// (4096) rechazase con 400 justo el mensaje largo que venía a salvar.
func TestTruncateReservesEllipsisRoom(t *testing.T) {
	if got, cut := truncateN("abcdefgh", 5); got != "abcd…" || !cut {
		t.Fatalf("truncateN(abcdefgh,5) = %q cut=%v", got, cut)
	}
	// multibyte: el corte respeta runes y el total (runes) queda ≤ max
	if got, _ := truncateN("ññññññññ", 4); got != "ñññ…" {
		t.Fatalf("truncateN multibyte = %q", got)
	}
	if got, cut := truncateN("corto", 10); got != "corto" || cut {
		t.Fatalf("truncateN sin corte = %q cut=%v", got, cut)
	}
	// el caso real: HTML ASCII puro de 4096+ con la elipsis incluida
	long, _ := TelegramHTML(Normalized{Message: strings.Repeat("a", 8000)})
	if n := utf8.RuneCountInString(long); n > 4096 {
		t.Fatalf("TelegramHTML excede el límite de Telegram: %d runes", n)
	}
	if !strings.HasSuffix(long, "…") {
		t.Fatal("el truncado debería terminar en elipsis")
	}
}

// Discord limita content a 2000 chars (400 permanente por encima): la forma
// dedicada debe acotarse aunque el mensaje aceptado por la API sea de 8192.
func TestDiscordTextLimit(t *testing.T) {
	got, truncated := DiscordText(Normalized{Title: "t", Message: strings.Repeat("x", 8000)})
	if n := utf8.RuneCountInString(got); n > 2000 {
		t.Fatalf("DiscordText excede el límite de Discord: %d runes", n)
	}
	if n := utf8.RuneCountInString(got); n < 1990 {
		t.Fatalf("truncó demasiado agresivo: %d runes", n)
	}
	if !truncated {
		t.Fatal("DiscordText no reportó el truncado")
	}
}

// El canal log usa Plain (8000): también reporta truncado.
func TestPlainTruncationReported(t *testing.T) {
	got, truncated := Plain(Normalized{Message: strings.Repeat("x", 8100)})
	if !truncated || utf8.RuneCountInString(got) > 8000 {
		t.Fatalf("Plain no truncó/reportó: runes=%d truncated=%v", utf8.RuneCountInString(got), truncated)
	}
	if _, truncated := Plain(Normalized{Message: "corto"}); truncated {
		t.Fatal("Plain reportó truncado en mensaje corto")
	}
}

// Slack interpreta <url|texto> (enlace con etiqueta falsa) y <!channel>:
// el payload del agente no debe poder activar esa sintaxis.
func TestSlackTextNeutralizesAngleSyntax(t *testing.T) {
	got, _ := SlackText(Normalized{
		Message: "pincha <http://evil.example|tu-deploy-va-bien> y avisa <!channel>",
		Data:    map[string]any{"url": "<http://evil2|ok>"},
	})
	if strings.Contains(got, "<http") || strings.Contains(got, "<!") {
		t.Fatalf("sintaxis activa de Slack sin neutralizar: %q", got)
	}
	if !strings.Contains(got, "\u0026lt;") {
		t.Fatalf("debería escapar como entidad: %q", got)
	}
}

// Discord: <@id>/<@&rol>/<#canal> son menciones ACTIVAS en content — el
// payload del consumidor no puede mencionar usuarios arbitrarios.
func TestDiscordNeutralizesMentionSyntax(t *testing.T) {
	got, _ := DiscordText(Normalized{
		Title:   "hola <@123456>",
		Message: "mira <@&99> y <#56>",
	})
	if strings.Contains(got, "<@") || strings.Contains(got, "<#") {
		t.Fatalf("sintaxis de mención activa en Discord: %q", got)
	}
}
