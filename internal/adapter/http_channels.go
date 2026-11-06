package adapter

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

var httpClient = &http.Client{
	Timeout: 15 * time.Second,
	// Un redirect puede llevar el Bearer/bot token/secreto a otro host. El
	// cliente guardado ya lo bloqueaba; este (Telegram y allow_internal) no.
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// TelegramAdapter sends via the Bot API sendMessage with parse_mode HTML.
type TelegramAdapter struct{}

type telegramTarget struct {
	ChatID json.RawMessage `json:"chat_id"` // number or string, passed through
}

func (TelegramAdapter) Send(ctx context.Context, in SendInput) (Result, error) {
	var creds struct {
		BotToken string `json:"bot_token"`
		// APIBase: override para servidores Bot API self-hosted y para los
		// tests con stub local. Las creds solo las configura el
		// admin (mismo nivel de confianza que la URL de un webhook).
		APIBase string `json:"api_base"`
	}
	if err := json.Unmarshal(in.Creds, &creds); err != nil || creds.BotToken == "" {
		return Result{}, errors.New("creds de telegram requieren bot_token")
	}
	var tgt telegramTarget
	if err := json.Unmarshal(in.Target, &tgt); err != nil || len(tgt.ChatID) == 0 {
		return Result{}, errors.New("target de telegram requiere chat_id")
	}
	body, _ := json.Marshal(map[string]any{
		"chat_id":                  json.RawMessage(tgt.ChatID),
		"text":                     in.TelegramHTML,
		"parse_mode":               "HTML",
		"disable_web_page_preview": true,
	})
	base := creds.APIBase
	if base == "" {
		base = "https://api.telegram.org"
	}
	url := fmt.Sprintf("%s/bot%s/sendMessage", strings.TrimSuffix(base, "/"), creds.BotToken)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		// el error de parseo puede incluir la URL con el token
		return Result{}, errors.New(redactSecret(err.Error(), creds.BotToken))
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		// net/http embebe la URL completa en el error → el bot token no debe
		// quedar en claro ni en el log ni en la columna error de deliveries
		// (los demás secretos viajan cifrados en reposo; esto no puede).
		return Result{Retryable: true}, fmt.Errorf("red: %s", redactSecret(err.Error(), creds.BotToken))
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	var tr struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
		Result      struct {
			MessageID int64 `json:"message_id"`
		} `json:"result"`
		Parameters struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	_ = json.Unmarshal(raw, &tr)

	switch {
	case resp.StatusCode == http.StatusOK:
		// 200 = Telegram aceptó el mensaje. Un cuerpo ilegible (reset a mitad
		// de respuesta, JSON truncado) NO es fallo del envío: marcar
		// permanente algo que sí se envió perdería la notificación.
		id := ""
		if tr.OK {
			id = strconv.FormatInt(tr.Result.MessageID, 10)
		}
		return Result{ProviderMsgID: id}, nil
	case resp.StatusCode == http.StatusTooManyRequests:
		// Rate limit del proveedor: reintentable, y devolvemos su retry_after
		// real para que el worker no reintente antes de que el proveedor
		// esté listo (si el header Retry-After viene, manda; si no, el JSON).
		after := time.Duration(tr.Parameters.RetryAfter) * time.Second
		if h := resp.Header.Get("Retry-After"); h != "" {
			if secs, err := strconv.Atoi(strings.TrimSpace(h)); err == nil {
				after = time.Duration(secs) * time.Second
			}
		}
		// el chat_id es un identificador de un tercero (PII): deliveries.error
		// se persiste y se lee por API — misma redacción que las direcciones
		// de email (redactEmails) y las URLs con secreto.
		return Result{Retryable: true, RetryAfter: after},
			fmt.Errorf("telegram 429: retry_after=%s %s", after, redactSecret(tr.Description, string(tgt.ChatID)))
	case resp.StatusCode >= 500:
		return Result{Retryable: true}, fmt.Errorf("telegram %d: %s", resp.StatusCode, redactSecret(tr.Description, string(tgt.ChatID)))
	default:
		// 400/401/403: chat inexistente, bot token inválido → permanente.
		return Result{}, fmt.Errorf("telegram %d: %s", resp.StatusCode, redactSecret(tr.Description, string(tgt.ChatID)))
	}
}

// SlackAdapter posts {text} to an incoming webhook.
type SlackAdapter struct{}

func (SlackAdapter) Send(ctx context.Context, in SendInput) (Result, error) {
	return postWebhook(ctx, in, map[string]any{"text": in.SlackText}, "slack")
}

// DiscordAdapter posts {content} to a webhook.
type DiscordAdapter struct{}

func (DiscordAdapter) Send(ctx context.Context, in SendInput) (Result, error) {
	// Discord limita content a 2000 chars (400 permanente por encima):
	// DiscordText trunca; Plain (8000) lo mataba con cualquier payload largo.
	// allowed_mentions.parse=[] desactiva TODA mención (incluido @everyone/
	// @here, que no usan la sintaxis <@…> y por tanto no los cubría el
	// sanitizado de ángulos): el payload de un consumidor no puede pingear
	// al servidor entero.
	return postWebhook(ctx, in, map[string]any{
		"content":          in.DiscordText,
		"allowed_mentions": map[string]any{"parse": []string{}},
	}, "discord")
}

// WebhookAdapter POSTs the normalized JSON to an arbitrary HTTP endpoint.
type WebhookAdapter struct{}

func (WebhookAdapter) Send(ctx context.Context, in SendInput) (Result, error) {
	payload, _ := json.Marshal(in.JSON)
	return postWebhook(ctx, in, payload, "webhook")
}

func postWebhook(ctx context.Context, in SendInput, payload any, name string) (Result, error) {
	var creds struct {
		URL    string `json:"url"`
		Secret string `json:"secret"`
		// AllowInternal desactiva el guard SSRF entero (por defecto solo se
		// bloquea link-local/metadata: local-first significa que los webhooks
		// a 127.0.0.1, 10.x y 192.168.x de la LAN siguen funcionando).
		AllowInternal bool `json:"allow_internal"`
	}
	if err := json.Unmarshal(in.Creds, &creds); err != nil || creds.URL == "" {
		// webhook permite la URL también en el target (p.ej. discord rápido)
		var tgt struct {
			URL string `json:"url"`
		}
		_ = json.Unmarshal(in.Target, &tgt)
		creds.URL = tgt.URL
		if creds.URL == "" {
			return Result{}, fmt.Errorf("creds de %s requieren url", name)
		}
	}
	// Guard SSRF (harden): bloquea el pivote clásico de red — el
	// endpoint de metadata de la nube y link-local — salvo autorización
	// explícita del admin (allow_internal).
	if !creds.AllowInternal {
		if err := guardMetadataURL(ctx, creds.URL); err != nil {
			// url.Parse incrusta la URL cruda en el error, y en Slack/Discord la
			// URL ES el secreto (token en el path): redactar antes de devolver
			return Result{}, fmt.Errorf("%s: %s", name,
				redactSecret(redactSecret(err.Error(), creds.URL), creds.Secret))
		}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return Result{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, creds.URL, bytes.NewReader(body))
	if err != nil {
		// ídem: el error de parseo lleva la URL con el token dentro
		return Result{}, errors.New(redactSecret(redactSecret(err.Error(), creds.URL), creds.Secret))
	}
	req.Header.Set("Content-Type", "application/json")
	if creds.Secret != "" {
		// Cabecera legada (contrato existente: el receptor reenvía el secreto
		// compartido) + firma HMAC-SHA256 del cuerpo, aditiva y no rompedora:
		// los receptores pueden migrar a verificar la firma sin downtime.
		req.Header.Set("X-RavenHub-Secret", creds.Secret)
		mac := hmac.New(sha256.New, []byte(creds.Secret))
		mac.Write(body)
		req.Header.Set("X-RavenHub-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
		// V2 incluye timestamp para que el receptor pueda rechazar replays.
		ts := strconv.FormatInt(time.Now().Unix(), 10)
		macV2 := hmac.New(sha256.New, []byte(creds.Secret))
		macV2.Write([]byte(ts))
		macV2.Write([]byte("."))
		macV2.Write(body)
		req.Header.Set("X-RavenHub-Timestamp", ts)
		req.Header.Set("X-RavenHub-Signature-V2", "sha256="+hex.EncodeToString(macV2.Sum(nil)))
	}
	client := httpClient
	if !creds.AllowInternal {
		// cliente compartido sin redirects (un redirect a 169.254.169.254
		// burlaría el guard) y con DNS pineado; reutiliza conexiones para no
		// abrir TCP+TLS nuevos por mensaje.
		client = guardedWebhookClient()
	}
	resp, err := client.Do(req)
	if err != nil {
		// net/http embebe la URL completa en el error → la URL del webhook
		// ES el secreto (bearer). Redactarla igual que el bot de Telegram:
		// deliveries.error es legible vía GET /v1/messages.
		msg := redactSecret(err.Error(), creds.URL)
		msg = redactSecret(msg, creds.Secret)
		return Result{Retryable: true}, errors.New(msg)
	}
	defer resp.Body.Close()
	// drenar el body (hasta 1MB) permite reusar la conexión del pool
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return Result{ProviderMsgID: truncateHeader(resp.Header.Get("X-Request-Id"))}, nil
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		after := parseRetryAfter(resp.Header.Get("Retry-After"))
		return Result{Retryable: true, RetryAfter: after}, fmt.Errorf("%s %d", name, resp.StatusCode)
	default:
		return Result{}, fmt.Errorf("%s %d", name, resp.StatusCode)
	}
}

// guardedWebhookClient es el cliente para webhooks con guard activo: sin
// redirects y con DNS pineado. Compartido entre envíos para reutilizar
// conexiones (se construye una única vez).
var (
	guardedClientOnce sync.Once
	guardedClient     *http.Client
)

func guardedWebhookClient() *http.Client {
	guardedClientOnce.Do(func() {
		guardedClient = &http.Client{
			Timeout: 15 * time.Second,
			// no seguir redirects: un redirect a 169.254.169.254 burlaría el guard
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
			Transport:     pinnedTransport(nil, false),
		}
	})
	return guardedClient
}

// pinnedTransport devuelve un RoundTripper que resuelve el host UNA sola vez
// por conexión y marca a esa IP elegida (anti-rebinding: si se resolviera en
// el guard y net/http resolviera OTRA vez al conectar, un rebinding entre
// ambas resoluciones atravesaría). Rechaza link-local y las IPs de metadata
// salvo allowInternal, y siempre unspecified. base==nil usa http.DefaultTransport.
func pinnedTransport(base http.RoundTripper, allowInternal bool) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	tr, ok := base.(*http.Transport)
	if !ok {
		tr = &http.Transport{}
	} else {
		tr = tr.Clone()
	}
	// Un HTTP_PROXY haría que DialContext validara la IP del proxy, no la del
	// destino: el guard SSRF quedaría bypassado.
	tr.Proxy = nil
	dialer := &net.Dialer{Timeout: 15 * time.Second}
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		ip, err := pickOutboundIP(ctx, host, allowInternal)
		if err != nil {
			return nil, err
		}
		return dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
	}
	return tr
}

// metadataIPs son endpoints de metadata de cloud que NO caen en link-local:
// Aliyun (100.100.100.200, unicast global) y AWS IMDSv1 IPv6 (fd00:ec2::254,
// unique-local). El guard los bloquea igual que 169.254.169.254.
var metadataIPs = map[string]bool{
	"100.100.100.200": true,
	"192.0.0.192":     true, // Oracle Cloud metadata
	"fd00:ec2::254":   true,
}

// isMetadataIP cubre también IPv4 embebida en NAT64 (64:ff9b::/96) y 6to4
// (2002::/16), que si no se decodifican se cuelan como "IPv6 pública".
func isMetadataIP(ip net.IP) bool {
	if metadataIPs[ip.String()] {
		return true
	}
	v4 := ip.To4()
	if v4 != nil {
		return metadataIPs[v4.String()]
	}
	ip16 := ip.To16()
	if ip16 == nil {
		return false
	}
	// NAT64 well-known prefix 64:ff9b::/96
	if ip16.Mask(net.CIDRMask(96, 128)).Equal(net.ParseIP("64:ff9b::").To16()) {
		return isMetadataIP(net.IPv4(ip16[12], ip16[13], ip16[14], ip16[15]))
	}
	// 6to4 2002::/16
	if ip16[0] == 0x20 && ip16[1] == 0x02 {
		return isMetadataIP(net.IPv4(ip16[2], ip16[3], ip16[4], ip16[5]))
	}
	return false
}

// isLocalOrPrivateHost permite HTTP solo en loopback/LAN local (local-first).
// Los nombres .local/.lan se aceptan para n8n/Home Assistant en la LAN; un
// host público por HTTP exige https o el opt-out allow_internal.
func isLocalOrPrivateHost(host string) bool {
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback() || ip.IsPrivate()
	}
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	return h == "localhost" || strings.HasSuffix(h, ".localhost") ||
		strings.HasSuffix(h, ".local") || strings.HasSuffix(h, ".lan")
}

func isBlockedIP(ip net.IP, allowInternal bool) bool {
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
		return true
	}
	return !allowInternal && isMetadataIP(ip)
}

func pickOutboundIP(ctx context.Context, host string, allowInternal bool) (net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		if isBlockedIP(ip, allowInternal) {
			return nil, fmt.Errorf("destino bloqueado (%s); usa allow_internal si es intencional", host)
		}
		if ip.IsUnspecified() {
			return nil, fmt.Errorf("destino unspecified bloqueado (%s)", host)
		}
		return ip, nil
	}
	// resuelve con el ctx del envío: un DNS en blackhole no cuelga el intento
	// tras la cancelación del delivery
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("no se pudo resolver %q: %w", host, err)
	}
	for _, ip := range ips {
		if ip.IsUnspecified() {
			continue
		}
		if !allowInternal && (ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast()) {
			continue
		}
		if isMetadataIP(ip) {
			continue
		}
		return ip, nil
	}
	return nil, fmt.Errorf("sin IPs utilizables para %q; usa allow_internal si es intencional", host)
}

// redactSecret removes a secret value from an error string (net/http embeds
// the full URL — including the bot token — in transport errors).
func redactSecret(msg, secret string) string {
	if secret == "" {
		return msg
	}
	return strings.ReplaceAll(msg, secret, "***")
}

// parseRetryAfter parses the Retry-After header (seconds or HTTP-date).
func parseRetryAfter(h string) time.Duration {
	h = strings.TrimSpace(h)
	if h == "" {
		return 0
	}
	if secs, err := strconv.Atoi(h); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(h); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// guardMetadataURL rejects webhook URLs pointing at link-local ranges — the
// classic SSRF pivot to cloud metadata (169.254.169.254) — , the cloud
// metadata endpoints outside link-local, or unspecified addresses.
// Loopback y rangos privados se permiten: el proyecto es local-first y los
// webhooks internos (n8n en la LAN, etc.) son el caso normal.
func guardMetadataURL(ctx context.Context, raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("URL inválida: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("esquema no permitido %q (http|https)", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return errors.New("URL sin host")
	}
	// HTTP solo para loopback/LAN local. Un webhook público por HTTP lleva el
	// secreto HMAC y el payload en claro; allow_internal es el opt-out explícito
	// (también desactiva el guard SSRF, así que es una decisión consciente).
	if u.Scheme == "http" && !isLocalOrPrivateHost(host) {
		return fmt.Errorf("webhook http no local (%s): usa https o allow_internal", host)
	}
	if ip := net.ParseIP(host); ip != nil {
		if isBlockedIP(ip, false) {
			return fmt.Errorf("destino bloqueado (%s); usa allow_internal si es intencional", host)
		}
		return nil
	}
	// nombre de host: resolvemos (con el ctx del envío) y comprobamos todas las IPs
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
	if err != nil {
		return fmt.Errorf("no se pudo resolver %q: %w", host, err)
	}
	for _, ip := range ips {
		if isBlockedIP(ip, false) {
			return fmt.Errorf("destino bloqueado (%s → %s); usa allow_internal si es intencional", host, ip)
		}
	}
	return nil
}

// truncateHeader acota cabeceras arbitrarias del receptor antes de
// persistirlas como provider_msg_id (visibles por la API del proyecto).
func truncateHeader(s string) string {
	if len(s) > 128 {
		return s[:128]
	}
	return s
}
