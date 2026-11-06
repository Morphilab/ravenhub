package mcp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// El Bearer de MCP no puede saltar de host por un redirect del Hub.
func TestCallNotifyDoesNotFollowRedirect(t *testing.T) {
	var hit atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit.Store(true)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()

	args, _ := json.Marshal(map[string]any{
		"title": "t", "message": "m", "to": []string{"log:dev"},
	})
	_, isErr := callNotify(redirect.URL, "rvn_test_token", args)
	if !isErr {
		t.Fatal("callNotify aceptó un redirect")
	}
	if hit.Load() {
		t.Fatal("callNotify siguió el redirect y envió el Bearer al segundo host")
	}
}
