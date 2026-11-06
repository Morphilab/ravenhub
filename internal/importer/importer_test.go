package importer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"ravenhub/internal/model"
)

func writeSeed(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "seed.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// Una variable ${ENV} referenciada y ausente aborta el import ANTES de
// tocar la DB: re-importar con el entorno incompleto sobrescribía en claro
// creds que funcionaban (upsert silencioso a vacío).
func TestImportFailsOnMissingEnv(t *testing.T) {
	p := writeSeed(t, `projects:
  - name: p1
    accounts:
      - label: bot
        channel: telegram
        creds:
          bot_token: ${TELEGRAM_BOT_TOKEN_INEXISTENTE_PRUEBA}
`)
	err := Import(context.Background(), nil, p)
	if err == nil || !strings.Contains(err.Error(), "TELEGRAM_BOT_TOKEN_INEXISTENTE_PRUEBA") {
		t.Fatalf("esperaba error nombrando la variable ausente, got: %v", err)
	}
}

// una ${var_minuscula} no debe quedar LITERAL en silencio: un mecanismo
// fail-fast que falla en silencio es lo contrario de su propósito.
func TestImportFailsOnMissingLowercaseEnv(t *testing.T) {
	p := writeSeed(t, `projects:
  - name: p1
    accounts:
      - label: bot
        channel: telegram
        creds:
          bot_token: ${token_minuscula_inexistente}
`)
	err := Import(context.Background(), nil, p)
	if err == nil || !strings.Contains(err.Error(), "token_minuscula_inexistente") {
		t.Fatalf("esperaba error nombrando la variable en minúsculas ausente, got: %v", err)
	}
}

// Un campo desconocido (typo) no puede pasar en silencio: el "re-import OK"
// sin crear la sección era el fallo más caro de diagnosticar.
func TestImportRejectsUnknownFields(t *testing.T) {
	p := writeSeed(t, `projects:
  - name: p1
    destinatios:
      - name: x
`)
	err := Import(context.Background(), nil, p)
	if err == nil || !strings.Contains(err.Error(), "destinatios") {
		t.Fatalf("esperaba error de campo desconocido, got: %v", err)
	}
}

// options en YAML debe decodificar en las dos formas documentadas (block y
// flow). yaml.v3 no vuelca un mapping en json.RawMessage: el ejemplo de
// seed.example.yaml rompía el import ENTERO ("cannot unmarshal !!map into
// json.RawMessage") — la config por proyecto del seed era inaplicable.
func TestSeedOptionsDecodeFromYAML(t *testing.T) {
	for _, doc := range []string{
		"projects:\n  - name: p1\n    options:\n      ttl:\n        urgent: 10m\n      digest_window: 10m\n",
		"projects:\n  - name: p1\n    options: {\"ttl\": {\"urgent\": \"10m\"}}\n",
	} {
		var seed seedFile
		if err := yaml.Unmarshal([]byte(doc), &seed); err != nil {
			t.Fatalf("options no decodifican (%q): %v", doc, err)
		}
		got := string(seed.Projects[0].Options.json())
		if !strings.Contains(got, "urgent") {
			t.Fatalf("options perdidas: %q", got)
		}
		if err := model.ValidateProjectOptions(seed.Projects[0].Options.json()); err != nil {
			t.Fatalf("options decodificadas no validan: %v", err)
		}
	}
	// sin options declaradas → nil (no se toca nada al re-importar)
	var seed seedFile
	if err := yaml.Unmarshal([]byte("projects:\n  - name: p1\n"), &seed); err != nil {
		t.Fatal(err)
	}
	if len(seed.Projects[0].Options.json()) != 0 {
		t.Fatalf("options ausentes deberían ser nil, got %s", seed.Projects[0].Options.json())
	}
}

// Pre-flight: un option inválido aborta ANTES de tocar la DB (el runtime
// ignoraría la clave en silencio: fail-fast > config que "no hace nada").
func TestImportRejectsInvalidOptions(t *testing.T) {
	cases := []struct{ name, options, want string }{
		{"clave desconocida (typo)", `{"digest_windows":"10m"}`, "desconocida"},
		{"ttl ilegible", `{"ttl":{"urgent":"abc"}}`, "duración inválida"},
		{"ttl en mayúsculas", `{"ttl":{"URGENT":"10m"}}`, "minúsculas"},
		{"digest_window por debajo del mínimo", `{"digest_window":"500ms"}`, "mínimo 1s"},
	}
	for _, c := range cases {
		p := writeSeed(t, "projects:\n  - name: p1\n    options: "+c.options+"\n")
		err := Import(context.Background(), nil, p)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: esperaba error con %q, got: %v", c.name, c.want, err)
		}
	}
}

// Pre-flight: reglas de enrutamiento mal declaradas → error sin tocar la DB.
func TestImportRejectsInvalidRoutingRule(t *testing.T) {
	p := writeSeed(t, `projects:
  - name: p1
    routing_rules:
      - name: sin-destinos
        match_tags: [alerta]
`)
	if err := Import(context.Background(), nil, p); err == nil || !strings.Contains(err.Error(), "destinations") {
		t.Fatalf("esperaba error de destinations, got: %v", err)
	}
	p2 := writeSeed(t, `projects:
  - name: p1
    routing_rules:
      - name: mala-prioridad
        match_priority: altísima
        destinations: [oncall]
`)
	if err := Import(context.Background(), nil, p2); err == nil || !strings.Contains(err.Error(), "match_priority") {
		t.Fatalf("esperaba error de match_priority, got: %v", err)
	}
}
