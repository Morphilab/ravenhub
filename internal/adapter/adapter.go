// Package adapter defines the channel interface and its implementations.
// Every channel speaks its own format; the adapter hides that from the core
// (los adaptadores son la única pieza que conoce al proveedor).
package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"ravenhub/internal/render"
)

// SendInput carries everything an adapter needs for one delivery.
type SendInput struct {
	Creds  json.RawMessage // decrypted account credentials (channel-specific)
	Target json.RawMessage // destination target (chat_id, to[], url, ...)
	Content
}

// Content holds the pre-rendered forms; adapters pick what they need.
type Content struct {
	Title        string
	TelegramHTML string
	EmailSubject string
	EmailHTML    string
	EmailText    string
	SlackText    string
	DiscordText  string
	Plain        string
	JSON         render.Normalized
}

// Result of a send attempt.
type Result struct {
	ProviderMsgID string
	Retryable     bool // true → message may be retried later
	// RetryAfter is an optional provider hint (e.g. Telegram 429 retry_after,
	// webhook Retry-After) that the worker honors over its own backoff when
	// it is larger ("Se respeta 429 / Retry-After del proveedor").
	RetryAfter time.Duration
}

// Adapter sends one notification to one destination of a channel.
type Adapter interface {
	Send(ctx context.Context, in SendInput) (Result, error)
}

var registry = map[string]Adapter{}

// Get returns the adapter for a channel.
func Get(channel string) (Adapter, error) {
	a, ok := registry[channel]
	if !ok {
		return nil, fmt.Errorf("canal %q sin adaptador", channel)
	}
	return a, nil
}

// Supported reports whether a channel has an adapter.
func Supported(channel string) bool { _, ok := registry[channel]; return ok }

// Channels lists registered channels.
func Channels() []string {
	out := make([]string, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	return out
}

func register(channel string, a Adapter) { registry[channel] = a }

func init() {
	register("log", &LogAdapter{})
	register("telegram", &TelegramAdapter{})
	register("email", &EmailAdapter{})
	register("webhook", &WebhookAdapter{})
	register("discord", &DiscordAdapter{})
	register("slack", &SlackAdapter{})
}
