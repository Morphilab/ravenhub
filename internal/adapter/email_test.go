package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/textproto"
	"strings"
	"testing"
)

// Un destino o un from con CR/LF no deben llegar nunca al MIME: interpolan
// cabeceras arbitrarias (header injection). Se valida antes de conectar SMTP.
func TestEmailRejectsHeaderInjection(t *testing.T) {
	in := SendInput{
		Creds:  json.RawMessage(`{"host":"smtp.example.com","port":587,"username":"u","password":"p","from":"alerts@x.com"}`),
		Target: json.RawMessage(`{"to":["ops@x.com\r\nBcc: attacker@evil.com"]}`),
	}
	if _, err := (EmailAdapter{}).Send(context.Background(), in); err == nil ||
		!strings.Contains(err.Error(), "dirección inválida") {
		t.Fatalf("inyección CRLF en dirección aceptada: %v", err)
	}

	in2 := SendInput{
		Creds:  json.RawMessage(`{"host":"smtp.example.com","from":"alerts@x.com\r\nBcc: evil@evil.com"}`),
		Target: json.RawMessage(`{"to":["ops@x.com"]}`),
	}
	if _, err := (EmailAdapter{}).Send(context.Background(), in2); err == nil ||
		!strings.Contains(err.Error(), "from inválido") {
		t.Fatalf("inyección CRLF en from aceptada: %v", err)
	}
}

func TestSMTPTLSRequired(t *testing.T) {
	cases := []struct {
		name          string
		host          string
		tlsUsed       bool
		allowInsecure bool
		want          bool
	}{
		{"loopback sin TLS", "127.0.0.1", false, false, false},
		{"localhost sin TLS", "localhost", false, false, false},
		{"remoto sin TLS", "smtp.example.com", false, false, true},
		{"remoto con TLS", "smtp.example.com", true, false, false},
		{"remoto con opt-out", "smtp.example.com", false, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := smtpTLSRequired(tc.host, tc.tlsUsed, tc.allowInsecure); got != tc.want {
				t.Fatalf("smtpTLSRequired = %v; want %v", got, tc.want)
			}
		})
	}
}

func TestRedactEmails(t *testing.T) {
	in := "550 5.1.1 <ops@example.com>: Recipient address rejected; reply to alerts+tag@sub.example.co.uk\r\nBcc: evil@example.net"
	got := redactEmails(in)
	if strings.Contains(got, "ops@example.com") || strings.Contains(got, "alerts+tag@sub.example.co.uk") || strings.Contains(got, "evil@example.net") {
		t.Fatalf("dirección sin redactar: %q", got)
	}
	if strings.ContainsAny(got, "\r\n") {
		t.Fatalf("el error conserva CR/LF (log injection): %q", got)
	}
	if !strings.Contains(got, "550 5.1.1") || !strings.Contains(got, "<email>") {
		t.Fatalf("se perdió el diagnóstico SMTP: %q", got)
	}
}

// La clasificación transitorio/permanente es por TIPO de error: el matching
// de cadenas clasificaba context.Canceled (apagado), resets y EOF como
// permanentes, y era sensible al texto del veredicto SMTP.
func TestEmailTransientClassification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"contexto cancelado (apagado)", context.Canceled, true},
		{"deadline del intento", context.DeadlineExceeded, true},
		{"error de red (refused/reset/timeout)", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}, true},
		{"EOF a mitad de sesión", io.EOF, true},
		{"EOF inesperado (TLS cortado)", io.ErrUnexpectedEOF, true},
		{"SMTP 421 (busy)", &textproto.Error{Code: 421, Msg: "Service not available"}, true},
		{"SMTP 452 (mailbox full)", &textproto.Error{Code: 452, Msg: "insufficient storage"}, true},
		{"SMTP 550 (mailbox inexistente)", &textproto.Error{Code: 550, Msg: "no mailbox"}, false},
		{"SMTP 535 (auth fallido)", &textproto.Error{Code: 535, Msg: "authentication failed"}, false},
		{"texto con '421' dentro de un 5xx", &textproto.Error{Code: 553, Msg: "relay denied (see RFC 4210)"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isTransientSMTP(tc.err); got != tc.want {
				t.Fatalf("isTransientSMTP(%v) = %v, esperaba %v", tc.err, got, tc.want)
			}
		})
	}
}
