package adapter

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/smtp"
	"net/textproto"
	"regexp"
	"strings"
	"time"
)

// EmailAdapter sends multipart/alternative (HTML + texto plano) vía SMTP
// con STARTTLS. Sin dependencias externas: net/smtp.
type EmailAdapter struct{}

type emailCreds struct {
	Host          string `json:"host"`
	Port          int    `json:"port"`
	Username      string `json:"username"`
	Password      string `json:"password"`
	From          string `json:"from"`
	AllowInsecure bool   `json:"allow_insecure"` // solo para relays de confianza sin STARTTLS
}

type emailTarget struct {
	To  []string `json:"to"`
	CC  []string `json:"cc"`
	BCC []string `json:"bcc"`
}

func (EmailAdapter) Send(ctx context.Context, in SendInput) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{Retryable: true}, err
	}
	var creds emailCreds
	if err := json.Unmarshal(in.Creds, &creds); err != nil || creds.Host == "" {
		return Result{}, errors.New("creds de email requieren host (y from)")
	}
	if creds.Port == 0 {
		creds.Port = 587
	}
	if creds.From == "" {
		return Result{}, errors.New("creds de email requieren from")
	}
	if !strings.Contains(creds.From, "@") {
		return Result{}, errors.New("creds de email requieren from con '@'")
	}
	var tgt emailTarget
	if err := json.Unmarshal(in.Target, &tgt); err != nil || len(tgt.To) == 0 {
		return Result{}, errors.New("target de email requiere to: [...]")
	}
	// sin CR/LF/control en direcciones: cabeceras MIME y sobre SMTP
	// se construyen por interpolación y una dirección con retorno de carro
	// inyectaría cabeceras arbitrarias (header injection). La coma también se
	// rechaza: "a@x, b@y" inyectaría destinatarios de display.
	if strings.ContainsAny(creds.From, "\r\n\x00,;<>\"()\\ ") {
		return Result{}, errors.New("from inválido: contiene caracteres de control o no soportados")
	}
	for _, addr := range append(append(append([]string{}, tgt.To...), tgt.CC...), tgt.BCC...) {
		// No incluir la dirección en el error: se persiste en deliveries.error
		// y se loguea (PII). El motivo basta para corregir la config.
		if strings.ContainsAny(addr, "\r\n\x00,;<>\"()\\ ") || !strings.Contains(addr, "@") {
			return Result{}, errors.New("dirección inválida: caracteres no soportados o falta @")
		}
	}

	payload := buildMIME(creds.From, tgt, in.Content)
	recipients := append(append([]string{}, tgt.To...), tgt.CC...)
	recipients = append(recipients, tgt.BCC...)

	// smtp.SendMail usa net.Dial sin timeout: un host en blackhole cuelga la
	// conexión y el worker se queda atascado. Dial acotado + secuencia explícita
	// (con STARTTLS si el servidor lo anuncia), igual que SendMail pero sin
	// poder colgarse indefinidamente (robustez ante hosts que cuelgan el envío).
	addr := fmt.Sprintf("%s:%d", creds.Host, creds.Port)
	err := sendMailTimeout(ctx, addr, smtpAuth(creds), creds.From, recipients, payload, creds.AllowInsecure)
	if err != nil {
		// El veredicto SMTP suele citar la dirección rechazada (550 5.1.1
		// <persona@example.com>): deliveries.error es legible por el proyecto
		// y se replica en logs. La dirección es PII; se redacta sin perder el
		// código/razón que necesita el operador.
		return Result{Retryable: isTransientSMTP(err)}, fmt.Errorf("%s", redactEmails(err.Error()))
	}
	return Result{ProviderMsgID: fmt.Sprintf("smtp-%d", time.Now().UnixNano())}, nil
}

var emailAddrRe = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)

// redactEmails sustituye direcciones de correo por <email> en errores que se
// persisten (deliveries.error) o se loguean. No toca códigos SMTP ni el resto
// del diagnóstico.
func redactEmails(s string) string {
	s = emailAddrRe.ReplaceAllString(s, "<email>")
	// el error de una dirección con CRLF no puede inyectar líneas en el log
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return s
}

// isTransientSMTP clasifica el fallo de envío por TIPO de error, no por el
// texto: la coincidencia de cadenas ("timeout", "421"...) clasificaba como
// permanentes los context.Canceled (apagado), los resets a mitad de sesión y
// los EOF; y un 5xx con "421" en el texto se colaba como transitorio.
func isTransientSMTP(err error) bool {
	// cancelación/deadline (apagado del worker, presupuesto del intento):
	// reintentar, no enterrar
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	// fallos de red (refused, reset, timeout del dial, TLS roto): transitorios
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true // relay que corta la sesión a mitad de conversación
	}
	// veredicto SMTP del servidor: 4yz transitorio, 5yz permanente
	var tpErr *textproto.Error
	if errors.As(err, &tpErr) {
		return tpErr.Code >= 400 && tpErr.Code < 500
	}
	return false // 535 auth, 550 mailbox inexistente… permanente
}

// smtpDialTimeout acota el dial y la handshake SMTP completa: un relay caído
// no puede dejar el intento colgado más allá de este presupuesto.
const smtpDialTimeout = 20 * time.Second

// sendMailTimeout replica smtp.SendMail (STARTTLS si se anuncia + AUTH opcional)
// pero con un dial con timeout explícito y error de timeout por contexto.
func sendMailTimeout(ctx context.Context, addr string, a smtp.Auth, from string, to []string, msg []byte, allowInsecure bool) error {
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	dialer := &net.Dialer{Timeout: smtpDialTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	// acota TODA la conversación SMTP (no solo el dial): un relay que se
	// atasca a mitad de DATA no puede dejar el intento colgado para siempre
	_ = conn.SetDeadline(time.Now().Add(smtpDialTimeout))
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		conn.Close()
		return err
	}
	// STARTTLS oportunista: si el servidor lo anuncia se negocia; si no,
	// smtpTLSRequired rechaza relays no-loopback (TLS obligatorio) y el envío
	// con AUTH en claro solo se permite en loopback (dev/Mailpit).
	tlsUsed := false
	if ok, _ := c.Extension("STARTTLS"); ok {
		if err := c.StartTLS(&tls.Config{ServerName: host}); err != nil {
			c.Close()
			return err
		}
		tlsUsed = true
	}
	if smtpTLSRequired(host, tlsUsed, allowInsecure) {
		c.Close()
		return errors.New("smtp: el servidor no ofrece STARTTLS y el host no es loopback (TLS obligatorio; usa allow_insecure solo para un relay de confianza)")
	}
	if a != nil && !tlsUsed && !isLoopbackHost(host) {
		c.Close()
		return errors.New("smtp: el servidor no ofrece STARTTLS y no es loopback (AUTH en claro rechazado)")
	}
	if a != nil {
		if ok, _ := c.Extension("AUTH"); !ok {
			c.Close()
			return errors.New("smtp: el servidor no anuncia AUTH")
		}
		if err := c.Auth(a); err != nil {
			c.Close()
			return err
		}
	}
	if err := c.Mail(from); err != nil {
		c.Close()
		return err
	}
	for _, r := range to {
		if err := c.Rcpt(r); err != nil {
			c.Close()
			return err
		}
	}
	w, err := c.Data()
	if err != nil {
		c.Close()
		return err
	}
	_, werr := w.Write(msg)
	if cerr := w.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		c.Close()
		return werr
	}
	// DATA ya fue aceptado por el relay: un fallo de QUIT (EOF/reset) no
	// puede convertir el envío en retry y duplicar el correo.
	_ = c.Quit()
	c.Close()
	return nil
}

func smtpAuth(c emailCreds) smtp.Auth {
	if c.Username == "" {
		return nil
	}
	return smtp.PlainAuth("", c.Username, c.Password, c.Host)
}

// isLoopbackHost indica relay local de dev (Mailpit): loopback IP o localhost.
func isLoopbackHost(host string) bool {
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	return h == "localhost" || strings.HasSuffix(h, ".localhost")
}

// smtpTLSRequired exige TLS para cualquier relay que no sea loopback, aunque
// no haya AUTH. Antes, un SMTP remoto sin STARTTLS y sin usuario enviaba el
// correo (y su contenido) en claro. allow_insecure es el opt-out explícito.
func smtpTLSRequired(host string, tlsUsed, allowInsecure bool) bool {
	return !tlsUsed && !allowInsecure && !isLoopbackHost(host)
}

func buildMIME(from string, tgt emailTarget, c Content) []byte {
	// boundary aleatorio por mensaje: evita que el contenido del propio
	// mensaje lo incluya y contrabandee partes MIME anidadas
	var rnd [8]byte
	_, _ = rand.Read(rnd[:])
	boundary := "ravenhub-" + hex.EncodeToString(rnd[:])
	var b bytes.Buffer
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", strings.Join(tgt.To, ", "))
	if len(tgt.CC) > 0 {
		fmt.Fprintf(&b, "Cc: %s\r\n", strings.Join(tgt.CC, ", "))
	}
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", c.EmailSubject))
	fmt.Fprintf(&b, "MIME-Version: 1.0\r\n")
	fmt.Fprintf(&b, "Content-Type: multipart/alternative; boundary=%s\r\n", boundary)
	fmt.Fprintf(&b, "\r\n")
	fmt.Fprintf(&b, "--%s\r\n", boundary)
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n\r\n")
	b.WriteString(strings.ReplaceAll(c.EmailText, "\n", "\r\n"))
	b.WriteString("\r\n")
	fmt.Fprintf(&b, "--%s\r\n", boundary)
	b.WriteString("Content-Type: text/html; charset=utf-8\r\n\r\n")
	b.WriteString(strings.ReplaceAll(c.EmailHTML, "\n", "\r\n"))
	b.WriteString("\r\n")
	fmt.Fprintf(&b, "--%s--\r\n", boundary)
	return b.Bytes()
}
