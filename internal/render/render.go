// Package render normalizes a notification into per-channel formats
// and applies server-side templates with a fail-safe fallback to plain
// text ("mejor notificar feo que no notificar").
package render

import (
	"encoding/json"
	"fmt"
	"html"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// Normalized is the channel-agnostic content of a notification.
type Normalized struct {
	Title    string         `json:"title"`
	Message  string         `json:"message"`
	Priority string         `json:"priority"`
	Tags     []string       `json:"tags,omitempty"`
	Data     map[string]any `json:"data,omitempty"`
}

// TelegramHTML renders Telegram parse_mode=HTML (stable, unlike MarkdownV2).
// Truncated to Telegram's 4096-char message limit; el bool reporta si hubo
// truncado (la entrega lo registra: un mensaje cortado no puede ser invisible).
func TelegramHTML(n Normalized) (string, bool) {
	var b strings.Builder
	if n.Title != "" {
		fmt.Fprintf(&b, "<b>%s</b>\n", html.EscapeString(n.Title))
	}
	if n.Message != "" {
		b.WriteString(html.EscapeString(n.Message))
		b.WriteString("\n")
	}
	if len(n.Tags) > 0 {
		fmt.Fprintf(&b, "<i>%s</i>\n", html.EscapeString(strings.Join(n.Tags, " · ")))
	}
	if len(n.Data) > 0 {
		b.WriteString("\n")
		for _, k := range sortedKeys(n.Data) {
			fmt.Fprintf(&b, "%s: %s\n", html.EscapeString(k), html.EscapeString(fmt.Sprintf("%v", n.Data[k])))
		}
	}
	s := strings.TrimRight(b.String(), "\n")
	return truncateHTMLSafe(s, 4096)
}

// EmailSubject builds the subject line.
func EmailSubject(n Normalized) (string, bool) {
	s := n.Title
	if s == "" {
		s = firstLine(n.Message)
	}
	if n.Priority == "urgent" {
		s = "[URGENTE] " + s
	}
	return truncateN(s, 255)
}

// EmailText renders the text/plain part.
func EmailText(n Normalized) string {
	var b strings.Builder
	b.WriteString(n.Message)
	if len(n.Tags) > 0 {
		fmt.Fprintf(&b, "\n\nTags: %s", strings.Join(n.Tags, ", "))
	}
	if len(n.Data) > 0 {
		b.WriteString("\n")
		for _, k := range sortedKeys(n.Data) {
			fmt.Fprintf(&b, "%s: %v\n", k, n.Data[k])
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// EmailHTML renders a minimal HTML part.
func EmailHTML(n Normalized) string {
	var b strings.Builder
	b.WriteString("<html><body>")
	if n.Title != "" {
		fmt.Fprintf(&b, "<h3>%s</h3>", html.EscapeString(n.Title))
	}
	fmt.Fprintf(&b, "<p>%s</p>", html.EscapeString(n.Message))
	if len(n.Data) > 0 {
		b.WriteString("<table border=\"0\" cellpadding=\"2\">")
		for _, k := range sortedKeys(n.Data) {
			fmt.Fprintf(&b, "<tr><td><b>%s</b></td><td>%s</td></tr>", html.EscapeString(k), html.EscapeString(fmt.Sprintf("%v", n.Data[k])))
		}
		b.WriteString("</table>")
	}
	b.WriteString("</body></html>")
	return b.String()
}

// Plain renders the flat form used by the log channel.
func Plain(n Normalized) (string, bool) {
	var b strings.Builder
	if n.Title != "" {
		b.WriteString(n.Title)
		b.WriteString(" — ")
	}
	b.WriteString(n.Message)
	return truncateN(b.String(), 8000)
}

// DiscordText renders the flat form for the Discord webhook (campo content).
// Discord rechaza content > 2000 chars con un 400 que clasifica como fallo
// permanente: truncar aquí salva la notificación (Plain llega a 8000). La
// sintaxis <@id>/<@&rol>/<#canal> son menciones ACTIVAS en content: el ángulo
// se neutraliza (sanitizeDiscord) — paridad con lo que ya hace sanitizeSlack.
func DiscordText(n Normalized) (string, bool) {
	var b strings.Builder
	if n.Title != "" {
		b.WriteString(sanitizeDiscord(n.Title))
		b.WriteString(" — ")
	}
	b.WriteString(sanitizeDiscord(n.Message))
	return truncateN(b.String(), 2000)
}

// sanitizeDiscord neutraliza la sintaxis de mención de Discord (<@id>,
// <@&rol>, <#canal>): el < se sustituye por su homólogo de ancho completa (＜),
// legible pero ya no es sintaxis activa — el payload de un consumidor no
// puede mencionar usuarios arbitrarios por el canal.
func sanitizeDiscord(s string) string {
	return strings.ReplaceAll(s, "<", "＜")
}

// SlackText renders the simple Slack webhook form.
func SlackText(n Normalized) (string, bool) {
	var b strings.Builder
	if n.Title != "" {
		fmt.Fprintf(&b, "*%s*\n", sanitizeSlack(n.Title))
	}
	b.WriteString(sanitizeSlack(n.Message))
	if len(n.Data) > 0 {
		b.WriteString("\n")
		for _, k := range sortedKeys(n.Data) {
			fmt.Fprintf(&b, "%s: %s\n", sanitizeSlack(k), sanitizeSlack(fmt.Sprintf("%v", n.Data[k])))
		}
	}
	return truncateN(strings.TrimRight(b.String(), "\n"), 4000)
}

// ApplyTemplate substitutes {{var}} placeholders: anything from the payload's
// data, plus the special variables {{title}}, {{message}}, {{priority}} and
// {{tags}}. An unknown variable is an error; the caller falls back to the
// plain message (mejor notificar feo que no notificar).
func ApplyTemplate(body string, n Normalized) (string, error) {
	var b strings.Builder
	for {
		start := strings.Index(body, "{{")
		if start < 0 {
			b.WriteString(body)
			break
		}
		end := strings.Index(body[start:], "}}")
		if end < 0 {
			b.WriteString(body)
			break
		}
		end += start
		b.WriteString(body[:start])
		key := strings.TrimSpace(body[start+2 : end])
		var v any
		var ok bool
		switch key {
		case "title":
			v, ok = n.Title, true
		case "message":
			v, ok = n.Message, true
		case "priority":
			v, ok = n.Priority, true
		case "tags":
			v, ok = strings.Join(n.Tags, ", "), true
		default:
			v, ok = n.Data[key]
		}
		if !ok {
			return "", fmt.Errorf("variable %q no presente en data", key)
		}
		fmt.Fprintf(&b, "%v", v)
		body = body[end+2:]
	}
	return b.String(), nil
}

// QuietHours is a per-destination silence window.
type QuietHours struct {
	Start string `json:"start"` // "23:00"
	End   string `json:"end"`   // "08:00"
	TZ    string `json:"tz"`    // IANA; default UTC
}

// ParseQuietHours decodes optional quiet hours JSON.
func ParseQuietHours(raw json.RawMessage) (*QuietHours, error) {
	s := strings.TrimSpace(string(raw))
	if len(raw) == 0 || s == "null" || s == "{}" {
		return nil, nil
	}
	var q QuietHours
	if err := json.Unmarshal(raw, &q); err != nil {
		return nil, fmt.Errorf("quiet_hours inválido: %w", err)
	}
	if q.TZ == "" {
		q.TZ = "UTC"
	}
	if _, err := time.LoadLocation(q.TZ); err != nil {
		return nil, fmt.Errorf("tz %q desconocida: %w", q.TZ, err)
	}
	if _, err := time.Parse("15:04", q.Start); err != nil {
		return nil, fmt.Errorf("quiet_hours.start inválido: %w", err)
	}
	if _, err := time.Parse("15:04", q.End); err != nil {
		return nil, fmt.Errorf("quiet_hours.end inválido: %w", err)
	}
	return &q, nil
}

// ActiveAt reports whether t (in the window's timezone) falls inside the
// silence window. Windows may wrap midnight (23:00 → 08:00).
func (q *QuietHours) ActiveAt(t time.Time) bool {
	if q == nil {
		return false
	}
	loc, err := time.LoadLocation(q.TZ)
	if err != nil {
		return false
	}
	local := t.In(loc)
	hm, _ := time.Parse("15:04", q.Start)
	he, _ := time.Parse("15:04", q.End)
	nowMin := local.Hour()*60 + local.Minute()
	startMin := hm.Hour()*60 + hm.Minute()
	endMin := he.Hour()*60 + he.Minute()
	if startMin == endMin {
		return false
	}
	if startMin < endMin {
		return nowMin >= startMin && nowMin < endMin
	}
	return nowMin >= startMin || nowMin < endMin // wraps midnight
}

// NextOpen returns the next instant after t when the window is open.
func (q *QuietHours) NextOpen(t time.Time) time.Time {
	if q == nil || !q.ActiveAt(t) {
		return t
	}
	cand := t
	for i := 0; i < 25*60 && q.ActiveAt(cand); i++ { // step 1 min, cap 25h
		cand = cand.Add(time.Minute)
	}
	return cand
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// Slack mrkdwn: neutralizamos el formato (*) y, sobre todo, la sintaxis
// activa <...> — en Slack <url|texto> crea un enlace con etiqueta arbitraria
// y <!channel>/<@user> son menciones: el payload no puede activarlas.
func sanitizeSlack(s string) string {
	s = strings.ReplaceAll(s, "*", "•")
	s = strings.ReplaceAll(s, "<", "\u0026lt;")
	s = strings.ReplaceAll(s, ">", "\u0026gt;")
	return s
}

// truncateN corta en max CARACTERES (los límites de los proveedores son de
// chars, no de bytes) reservando hueco para la elipsis: devolver max+1
// caracteres hacía que Telegram (límite 4096) rechazase con 400 justo el
// mensaje largo que el truncado existía para salvar. El bool reporta si cortó.
func truncateN(s string, max int) (string, bool) {
	if max <= 0 {
		return "", s != ""
	}
	if utf8.RuneCountInString(s) <= max {
		return s, false
	}
	cut, n := 0, 0
	for cut < len(s) && n < max-1 {
		_, size := utf8.DecodeRuneInString(s[cut:])
		cut += size
		n++
	}
	return s[:cut] + "…", true
}

// truncateHTMLSafe cuts at max bytes without leaving a broken HTML entity or
// tag: Telegram rechaza el mensaje COMPLETO si parse_mode=HTML llega malformado.
// Cubre entidades (&am…) y tags partidos (<b…); además cierra <b>/<i>/<u>/<s>/
// <code> abiertos.
func truncateHTMLSafe(s string, max int) (string, bool) {
	s, truncated := truncateN(s, max)
	if i := strings.LastIndexByte(s, '&'); i >= 0 && !strings.Contains(s[i:], ";") {
		s = s[:i] + "…"
	}
	// tag partido: '<' sin '>' posterior → cortar antes del '<'
	if lt := strings.LastIndexByte(s, '<'); lt >= 0 && !strings.Contains(s[lt:], ">") {
		s = s[:lt] + "…"
	}
	// cerrar tags simples abiertos sin cerrar
	for _, tag := range []string{"b", "i", "u", "s", "code"} {
		open, close := strings.Count(s, "<"+tag+">"), strings.Count(s, "</"+tag+">")
		for i := 0; i < open-close; i++ {
			s += "</" + tag + ">"
		}
	}
	return s, truncated
}
