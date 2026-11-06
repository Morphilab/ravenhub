package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeEnv(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadEnvFileKeepsHashInsideValues(t *testing.T) {
	path := writeEnv(t, `
PASS_CON_HASH=se#creto
PASS_COMENTADA=valor  # nota al final
PASS_ENTRECOMILLADA="v # r"
`)
	for _, k := range []string{"PASS_CON_HASH", "PASS_COMENTADA", "PASS_ENTRECOMILLADA"} {
		os.Unsetenv(k)
	}
	LoadEnvFile(path)

	if got := os.Getenv("PASS_CON_HASH"); got != "se#creto" {
		t.Fatalf("valor con # sin espacio se corrompió: %q", got)
	}
	if got := os.Getenv("PASS_COMENTADA"); got != "valor" {
		t.Fatalf("comentario en línea no se recortó: %q", got)
	}
	if got := os.Getenv("PASS_ENTRECOMILLADA"); got != "v # r" {
		t.Fatalf("# entre comillas no se respetó: %q", got)
	}
}

func TestLoadEnvFileDoesNotOverrideExisting(t *testing.T) {
	t.Setenv("RVNH_EXISTENTE", "prevalece")
	path := writeEnv(t, "RVNH_EXISTENTE=no-prevalece\nRVNH_NUEVA=si\n")
	os.Unsetenv("RVNH_NUEVA")
	LoadEnvFile(path)
	if os.Getenv("RVNH_EXISTENTE") != "prevalece" {
		t.Fatal("el .env pisó una variable del entorno")
	}
	if os.Getenv("RVNH_NUEVA") != "si" {
		t.Fatal("variable nueva no cargada")
	}
}

func TestEnvBool(t *testing.T) {
	cases := []struct {
		val  string
		want bool
		bad  bool
	}{
		{"", false, false},
		{"0", false, false},
		{"false", false, false},
		{"FALSE", false, false},
		{"no", false, false},
		{"off", false, false},
		{"1", true, false},
		{"true", true, false},
		{"YES", true, false},
		{"on", true, false},
		{" 1 ", true, false},
		{"produccion", false, true},
		{"2", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.val, func(t *testing.T) {
			t.Setenv("RVNH_BOOL", tc.val)
			got, err := envBool("RVNH_BOOL")
			if tc.bad {
				if err == nil {
					t.Fatalf("valor %q debería ser error", tc.val)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("envBool(%q) = %v, %v; want %v", tc.val, got, err, tc.want)
			}
		})
	}
}

func TestLoadDevFlagStrict(t *testing.T) {
	t.Setenv("RAVENHUB_DEV", "0")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Dev {
		t.Fatal("RAVENHUB_DEV=0 activó dev (peligro: token admin autogenerado)")
	}
	t.Setenv("RAVENHUB_DEV", "false")
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Dev {
		t.Fatal("RAVENHUB_DEV=false activó dev")
	}
}

func TestEnvDurationValidation(t *testing.T) {
	t.Setenv("RVNH_DUR_OK", "720h")
	t.Setenv("RVNH_DUR_NEG", "-1h")
	t.Setenv("RVNH_DUR_BASURA", "veinte dias")

	if v, err := envDuration("RVNH_DUR_OK", time.Hour); err != nil || v != 720*time.Hour {
		t.Fatalf("duración válida rechazada: v=%v err=%v", v, err)
	}
	if _, err := envDuration("RVNH_DUR_NEG", time.Hour); err == nil {
		t.Fatal("duración negativa aceptada")
	}
	if _, err := envDuration("RVNH_DUR_BASURA", time.Hour); err == nil {
		t.Fatal("duración ilegible aceptada")
	}
	if v, err := envDuration("RVNH_DUR_INEXISTENTE", time.Hour); err != nil || v != time.Hour {
		t.Fatalf("default de duración roto: v=%v err=%v", v, err)
	}
	// 0 = desactivado (valor explícito, no error)
	t.Setenv("RVNH_DUR_OFF", "0h")
	if v, err := envDuration("RVNH_DUR_OFF", time.Hour); err != nil || v != 0 {
		t.Fatalf("0h debería desactivar sin error: v=%v err=%v", v, err)
	}
}

func TestValidateBounds(t *testing.T) {
	base := &Config{DSN: "postgres://x", MasterKey: "1234567890123456", SoftLimit: 5, HardLimit: 10, ProjectHardLimit: 5, Workers: 1, RatePerMin: 1, RateBurst: 1, Lease: 120000000000, SyncWait: 1000000000, LogLevel: "info"}
	if err := base.Validate(); err != nil {
		t.Fatalf("base válida rechazada: %v", err)
	}
	for name, mutate := range map[string]func(*Config){
		"workers 0":    func(c *Config) { c.Workers = 0 },
		"lease 0":      func(c *Config) { c.Lease = 0 },
		"lease corto":  func(c *Config) { c.Lease = 29 * time.Second },
		"rate 0":       func(c *Config) { c.RatePerMin = 0 },
		"sync 0":       func(c *Config) { c.SyncWait = 0 },
		"sync >30s":    func(c *Config) { c.SyncWait = 31 * 1000000000 },
		"master corta": func(c *Config) { c.MasterKey = "corta" },
		"master placeholder": func(c *Config) {
			c.MasterKey = "dev-master-key-cambiar-en-produccion"
		},
		"admin corto":  func(c *Config) { c.AdminToken = "corto" },
		"soft>=hard":   func(c *Config) { c.SoftLimit, c.HardLimit = 10, 10 },
		"project 0":    func(c *Config) { c.ProjectHardLimit = 0 },
		"project>hard": func(c *Config) { c.ProjectHardLimit = 11 },
		"log level":    func(c *Config) { c.LogLevel = "verbose" },
	} {
		c := *base
		mutate(&c)
		if err := c.Validate(); err == nil {
			t.Fatalf("%s aceptado", name)
		}
	}
}
