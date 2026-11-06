package adapter

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"ravenhub/internal/render"
)

// El canal log no debe volcar el cuerpo (posible PII) por defecto; el opt-in
// RAVENHUB_LOG_CHANNEL_BODY=1 es solo para dev.
func TestLogAdapterBodyOptIn(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(old)

	in := SendInput{Content: Content{
		Title: "t",
		Plain: "PII_MARKER",
		JSON:  render.Normalized{Priority: "normal"},
	}}
	t.Setenv("RAVENHUB_LOG_CHANNEL_BODY", "")
	if _, err := (LogAdapter{}).Send(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "PII_MARKER") {
		t.Fatalf("el cuerpo se logueó sin opt-in: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "body_len") {
		t.Fatalf("falta body_len: %s", buf.String())
	}

	buf.Reset()
	t.Setenv("RAVENHUB_LOG_CHANNEL_BODY", "1")
	if _, err := (LogAdapter{}).Send(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "PII_MARKER") {
		t.Fatalf("el opt-in no emitió el cuerpo: %s", buf.String())
	}
}
