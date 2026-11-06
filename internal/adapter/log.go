package adapter

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"
)

// LogAdapter renders like every other channel but only writes to the Hub's
// structured log. It is the dry-run channel: indispensable for development
// and smoke tests.
type LogAdapter struct{}

func (LogAdapter) Send(ctx context.Context, in SendInput) (Result, error) {
	start := time.Now()
	// Por defecto solo LONGITUDES: title y tags son contenido del payload
	// (pueden llevar emails, IDs de usuario, nombres de terceros) y los logs
	// Docker no son el archivo de negocio. Con el opt-in de dev se emite
	// completo (title, tags y body).
	args := []any{
		"channel", "log",
		"title_len", len(in.Title),
		"body_len", len(in.Plain),
		"n_tags", len(in.JSON.Tags),
		"priority", in.JSON.Priority,
		"elapsed_ms", time.Since(start).Milliseconds(),
	}
	if os.Getenv("RAVENHUB_LOG_CHANNEL_BODY") == "1" {
		args = append(args, "title", in.Title, "tags", in.JSON.Tags, "body", in.Plain)
	}
	slog.InfoContext(ctx, "canal log — envío simulado", args...)
	return Result{ProviderMsgID: fmt.Sprintf("log-%d", time.Now().UnixNano())}, nil
}
