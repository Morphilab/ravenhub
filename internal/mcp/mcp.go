// Package mcp exposes RavenHub's notify as an MCP tool over stdio, so
// agents (ZCode/OpenCode/Claude/CrewAI…) can call it natively.
// Protocol: JSON-RPC 2.0, one message per line. Config: RAVENHUB_URL y
// NOTIFY_TOKEN en el entorno del agente.
package mcp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const protocolVersion = "2024-11-05"

// Run serves MCP over stdio until stdin closes.
func Run() error {
	hubURL := strings.TrimRight(envOr("RAVENHUB_URL", "http://localhost:8085"), "/")
	token := os.Getenv("NOTIFY_TOKEN")
	if token == "" {
		fmt.Fprintln(os.Stderr, "mcp: NOTIFY_TOKEN no definido en el entorno")
	}
	if u, err := url.Parse(hubURL); err == nil && u.Scheme == "http" &&
		u.Hostname() != "localhost" && u.Hostname() != "127.0.0.1" && u.Hostname() != "::1" {
		fmt.Fprintln(os.Stderr, "mcp: RAVENHUB_URL usa http:// fuera de loopback; usa https:// para no exponer el token")
	}

	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()

	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var req struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Method  string          `json:"method"`
			Params  struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			} `json:"params"`
		}
		if err := json.Unmarshal(line, &req); err != nil {
			writeError(out, nil, -32700, "JSON inválido")
			continue
		}

		switch req.Method {
		case "initialize":
			writeResult(out, req.ID, map[string]any{
				"protocolVersion": protocolVersion,
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "ravenhub-mcp", "version": "1.0.0"},
			})
		case "notifications/initialized", "notifications/cancelled":
			// notificación: sin respuesta
		case "ping":
			writeResult(out, req.ID, map[string]any{})
		case "tools/list":
			writeResult(out, req.ID, map[string]any{"tools": []map[string]any{notifyToolSpec()}})
		case "tools/call":
			text, isError := callNotify(hubURL, token, req.Params.Arguments)
			writeResult(out, req.ID, map[string]any{
				"content": []map[string]any{{"type": "text", "text": text}},
				"isError": isError,
			})
		default:
			if req.ID != nil {
				writeError(out, req.ID, -32601, "método desconocido: "+req.Method)
			}
		}
		out.Flush()
	}
	return sc.Err()
}

func notifyToolSpec() map[string]any {
	return map[string]any{
		"name":        "notify",
		"description": "Envía una notificación por RavenHub (Telegram/email/webhook…) a los destinos nombrados del proyecto.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"title":    map[string]any{"type": "string", "description": "Título corto de la notificación"},
				"message":  map[string]any{"type": "string", "description": "Cuerpo del mensaje"},
				"to":       map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Destinos o grupos del proyecto (obligatorio en el Hub; vacío = 400)"},
				"priority": map[string]any{"type": "string", "enum": []string{"low", "normal", "high", "urgent"}},
				"data":     map[string]any{"type": "object", "description": "Campos estructurados opcionales"},
			},
			"required": []string{"title", "message", "to"},
		},
	}
}

// callNotify posts to the Hub and returns (text, isError).
func callNotify(baseURL, token string, args json.RawMessage) (string, bool) {
	if token == "" {
		return "NOTIFY_TOKEN no está definido en el entorno del agente", true
	}
	var body struct {
		Title    string         `json:"title"`
		Message  string         `json:"message"`
		To       []string       `json:"to"`
		Priority string         `json:"priority"`
		Data     map[string]any `json:"data"`
	}
	if err := json.Unmarshal(args, &body); err != nil {
		return "argumentos inválidos: " + err.Error(), true
	}
	payload, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, baseURL+"/v1/notify", bytes.NewReader(payload))
	if err != nil {
		return "error construyendo la petición: " + err.Error(), true
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", fmt.Sprintf("mcp-%d", time.Now().UnixNano()))
	client := &http.Client{
		Timeout: 15 * time.Second,
		// El Bearer no puede saltar de host por un 3xx del Hub.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		return "el Hub no responde: " + err.Error(), true
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	switch {
	case resp.StatusCode == http.StatusAccepted:
		return "notificación aceptada: " + string(raw), false
	case resp.StatusCode == http.StatusOK:
		return "notificación entregada: " + string(raw), false
	default:
		return fmt.Sprintf("el Hub rechazó (HTTP %d): %s", resp.StatusCode, string(raw)), true
	}
}

func writeResult(w *bufio.Writer, id json.RawMessage, result any) {
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	w.Write(b)
	w.WriteByte('\n')
}

func writeError(w *bufio.Writer, id json.RawMessage, code int, msg string) {
	b, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": id,
		"error": map[string]any{"code": code, "message": msg},
	})
	w.Write(b)
	w.WriteByte('\n')
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
