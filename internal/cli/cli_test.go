package cli

import (
	"strings"
	"testing"

	"ravenhub/internal/config"
)

// El gate fail-closed del token admin: sin token y
// sin RAVENHUB_DEV arrancar es un error; SOLO en dev se autogenera. Extraído
// a resolveAdminToken precisamente para poder anclarlo con tests.
func TestResolveAdminToken(t *testing.T) {
	t.Run("token configurado se usa tal cual", func(t *testing.T) {
		cfg := &config.Config{AdminToken: "rva_admin_xxxxxxxxxxxxxxxxxxxx"}
		tok, err := resolveAdminToken(cfg)
		if err != nil || tok != cfg.AdminToken {
			t.Fatalf("esperaba el token del entorno, got %q, %v", tok, err)
		}
	})

	t.Run("sin token y sin DEV es error (fail-closed)", func(t *testing.T) {
		cfg := &config.Config{AdminToken: "", Dev: false}
		_, err := resolveAdminToken(cfg)
		if err == nil {
			t.Fatal("sin RAVENHUB_ADMIN_TOKEN y sin RAVENHUB_DEV arrancar debe fallar")
		}
		if !strings.Contains(err.Error(), "RAVENHUB_ADMIN_TOKEN es obligatorio") {
			t.Fatalf("el error debe decir cómo arreglarlo: %v", err)
		}
	})

	t.Run("sin token con DEV=1 autogenera uno admin", func(t *testing.T) {
		cfg := &config.Config{AdminToken: "", Dev: true}
		tok, err := resolveAdminToken(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(tok, "rva_admin_") || len(tok) < 20 {
			t.Fatalf("token autogenerado inesperado: %q", tok)
		}
	})
}
