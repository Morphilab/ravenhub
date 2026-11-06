// Package config loads RavenHub runtime configuration from the environment.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"ravenhub/internal/crypto"
)

// Config holds all runtime settings. Every value has a sane default except
// the DSN and the master key, which are required to serve.
type Config struct {
	DSN          string // RAVENHUB_DSN
	Listen       string // RAVENHUB_LISTEN (default :8080)
	MasterKey    string // RAVENHUB_MASTER_KEY (passphrase; AES key = PBKDF2)
	OldMasterKey string // RAVENHUB_MASTER_KEY_OLD (solo para `ravenhub re-encrypt`)
	AdminToken   string // RAVENHUB_ADMIN_TOKEN (bootstrap admin)
	Workers      int    // RAVENHUB_WORKERS (default 8)
	SoftLimit    int    // RAVENHUB_MAX_QUEUE_SOFT (429 for low/normal above this)
	HardLimit    int    // RAVENHUB_MAX_QUEUE_HARD (503 for everyone above this)
	// ProjectHardLimit acota la cola de UN proyecto para que un tenant no
	// pueda llenar el hard global y bloquear a los demás.
	ProjectHardLimit int           // RAVENHUB_MAX_QUEUE_PROJECT (default 2000)
	RatePerMin       int           // RAVENHUB_RATE_PER_MIN (per project intake)
	RateBurst        int           // RAVENHUB_RATE_BURST
	Lease            time.Duration // RAVENHUB_LEASE (delivering → queued reaper threshold)
	SyncWait         time.Duration // RAVENHUB_SYNC_WAIT (?sync=true deadline)
	LogLevel         string        // RAVENHUB_LOG_LEVEL (debug|info|warn|error)
	// Dev solo permite el token admin AUTOGENERADO (impreso en stdout).
	// Sin RAVENHUB_ADMIN_TOKEN y sin RAVENHUB_DEV, arrancar es un error:
	// una credencial admin impresa en docker logs no puede ser el default
	// de producción (fail-closed).
	Dev bool // RAVENHUB_DEV=1
	// Retención: mensajes 30d, auditoría 90d; 0 = desactivado
	RetentionMessages time.Duration // RAVENHUB_RETENTION_MESSAGES
	RetentionAudit    time.Duration // RAVENHUB_RETENTION_AUDIT
}

// LoadEnvFile reads a .env file (KEY=VALUE lines) and sets any variable not
// already present in the environment. Missing file is not an error.
func LoadEnvFile(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// quita comentarios en línea ("KEY=VAL # nota") SOLO si el '#' va
		// precedido de espacio y el balance de comillas es par: un valor con
		// '#' pegado (p.ej. SMTP_PASSWORD=se#creto) se conserva entero
		if idx := strings.Index(line, "#"); idx >= 0 {
			rest := line[:idx]
			quotes := strings.Count(rest, `"`) + strings.Count(rest, `'`)
			if quotes%2 == 0 && idx > 0 && (line[idx-1] == ' ' || line[idx-1] == '\t') {
				line = strings.TrimSpace(rest)
			}
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		if _, exists := os.LookupEnv(k); !exists {
			_ = os.Setenv(k, v)
		}
	}
}

// Load reads configuration from the environment.
func Load() (*Config, error) {
	c := &Config{
		DSN:          os.Getenv("RAVENHUB_DSN"),
		Listen:       env("RAVENHUB_LISTEN", ":8080"),
		MasterKey:    os.Getenv("RAVENHUB_MASTER_KEY"),
		OldMasterKey: os.Getenv("RAVENHUB_MASTER_KEY_OLD"),
		AdminToken:   os.Getenv("RAVENHUB_ADMIN_TOKEN"),
		LogLevel:     env("RAVENHUB_LOG_LEVEL", "info"),
	}
	var err error
	// Boolean estricto: solo 1/true/yes/on activan dev; 0/false/no/off lo
	// desactivan y un valor ambiguo es error. Dev permite autogenerar el token
	// admin e imprimirlo en stdout; no puede ser el default de producción.
	if c.Dev, err = envBool("RAVENHUB_DEV"); err != nil {
		return nil, err
	}
	if c.Workers, err = envInt("RAVENHUB_WORKERS", 8); err != nil {
		return nil, err
	}
	if c.SoftLimit, err = envInt("RAVENHUB_MAX_QUEUE_SOFT", 5000); err != nil {
		return nil, err
	}
	if c.HardLimit, err = envInt("RAVENHUB_MAX_QUEUE_HARD", 7500); err != nil {
		return nil, err
	}
	if c.ProjectHardLimit, err = envInt("RAVENHUB_MAX_QUEUE_PROJECT", 2000); err != nil {
		return nil, err
	}
	if c.RatePerMin, err = envInt("RAVENHUB_RATE_PER_MIN", 60); err != nil {
		return nil, err
	}
	if c.RateBurst, err = envInt("RAVENHUB_RATE_BURST", 10); err != nil {
		return nil, err
	}
	leaseStr := env("RAVENHUB_LEASE", "2m")
	if c.Lease, err = time.ParseDuration(leaseStr); err != nil {
		return nil, fmt.Errorf("RAVENHUB_LEASE inválida: %w", err)
	}
	syncStr := env("RAVENHUB_SYNC_WAIT", "8s")
	if c.SyncWait, err = time.ParseDuration(syncStr); err != nil {
		return nil, fmt.Errorf("RAVENHUB_SYNC_WAIT inválida: %w", err)
	}
	if c.RetentionMessages, err = envDuration("RAVENHUB_RETENTION_MESSAGES", 30*24*time.Hour); err != nil {
		return nil, err
	}
	if c.RetentionAudit, err = envDuration("RAVENHUB_RETENTION_AUDIT", 90*24*time.Hour); err != nil {
		return nil, err
	}
	return c, nil
}

// MasterKeyBytes derives the 32-byte AES key (PBKDF2 v2, ver crypto.MasterKey).
func (c *Config) MasterKeyBytes() []byte {
	return crypto.MasterKey(c.MasterKey)
}

// MidMasterKeyBytes derives the KDF v1 (PBKDF2 210k, previous) — solo dual-read.
func (c *Config) MidMasterKeyBytes() []byte {
	return crypto.MasterKeyV1(c.MasterKey)
}

// OldMasterKeyChain devuelve la cadena de claves de la passphrase ANTERIOR
// (v2→v1→legacy) para `ravenhub re-encrypt` durante una rotación. Vacía si
// RAVENHUB_MASTER_KEY_OLD no está definida.
func (c *Config) OldMasterKeyChain() [][]byte {
	if c.OldMasterKey == "" {
		return nil
	}
	return [][]byte{
		crypto.MasterKey(c.OldMasterKey),
		crypto.MasterKeyV1(c.OldMasterKey),
		crypto.LegacyMasterKey(c.OldMasterKey),
	}
}

// LegacyMasterKeyBytes derives the legacy key (SHA256 directo), solo para
// dual-read de creds cifradas antes de la migración de KDF.
func (c *Config) LegacyMasterKeyBytes() []byte {
	return crypto.LegacyMasterKey(c.MasterKey)
}

// Validate checks required values for serving.
func (c *Config) Validate() error {
	if c.DSN == "" {
		return fmt.Errorf("RAVENHUB_DSN es obligatorio (ej: postgres://ravenhub:pass@localhost:5432/ravenhub)")
	}
	if c.MasterKey == "" {
		return fmt.Errorf("RAVENHUB_MASTER_KEY es obligatorio (genera uno: openssl rand -hex 32)")
	}
	if len(c.MasterKey) < 16 {
		return fmt.Errorf("RAVENHUB_MASTER_KEY demasiado corta (mínimo 16 caracteres)")
	}
	// Rechaza el placeholder histórico de example.env: una clave conocida
	// pública no puede proteger credenciales en reposo. Fail-closed.
	if c.MasterKey == "dev-master-key-cambiar-en-produccion" {
		return fmt.Errorf("RAVENHUB_MASTER_KEY es un placeholder público: genera una clave real (openssl rand -hex 32)")
	}
	if c.AdminToken != "" && len(c.AdminToken) < 20 {
		return fmt.Errorf("RAVENHUB_ADMIN_TOKEN demasiado corto (mínimo 20 caracteres)")
	}
	switch strings.ToLower(strings.TrimSpace(c.LogLevel)) {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("RAVENHUB_LOG_LEVEL inválido: %q (debug|info|warn|error)", c.LogLevel)
	}
	if c.SoftLimit <= 0 || c.HardLimit <= c.SoftLimit {
		return fmt.Errorf("RAVENHUB_MAX_QUEUE_HARD debe ser mayor que RAVENHUB_MAX_QUEUE_SOFT")
	}
	if c.ProjectHardLimit <= 0 {
		return fmt.Errorf("RAVENHUB_MAX_QUEUE_PROJECT debe ser > 0")
	}
	if c.ProjectHardLimit > c.HardLimit {
		return fmt.Errorf("RAVENHUB_MAX_QUEUE_PROJECT (%d) no puede superar RAVENHUB_MAX_QUEUE_HARD (%d)", c.ProjectHardLimit, c.HardLimit)
	}
	if c.Workers <= 0 {
		return fmt.Errorf("RAVENHUB_WORKERS debe ser > 0")
	}
	// cotas superiores: los workers comparten un pool de 16 conexiones y el
	// lease compite con el timeout de los adaptadores — valores absurdos
	// (100000 workers, leases de días) pasaban la validación
	if c.Workers > 128 {
		return fmt.Errorf("RAVENHUB_WORKERS debe ser ≤ 128 (el pool de Postgres tiene 16 conexiones)")
	}
	if c.RatePerMin <= 0 || c.RateBurst <= 0 {
		return fmt.Errorf("RAVENHUB_RATE_PER_MIN y RAVENHUB_RATE_BURST deben ser > 0")
	}
	if c.Lease <= 0 {
		return fmt.Errorf("RAVENHUB_LEASE debe ser > 0 (ej: 2m)")
	}
	// el lease debe superar holgadamente los deadlines de los adaptadores
	// (15-20s por destino): con el latido por ticker, un lease menor deja al
	// reaper a un blip de DB de robar lotes en curso (entregas duplicadas)
	if c.Lease < 30*time.Second {
		return fmt.Errorf("RAVENHUB_LEASE debe ser ≥ 30s (los adaptadores tardan hasta 20s por destino; menor = el reaper roba lotes en curso)")
	}
	if c.Lease > time.Hour {
		return fmt.Errorf("RAVENHUB_LEASE debe ser ≤ 1h")
	}
	if c.SyncWait <= 0 || c.SyncWait > 30*time.Second {
		return fmt.Errorf("RAVENHUB_SYNC_WAIT debe estar entre 1ms y 30s")
	}
	return nil
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// envBool interpreta banderas booleanas de forma estricta. El valor vacío es
// false (default); 1/true/yes/on son true; 0/false/no/off son false. Cualquier
// otra cosa es error de configuración: un valor ambiguo no debe activar dev.
func envBool(key string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "", "0", "false", "no", "off":
		return false, nil
	case "1", "true", "yes", "on":
		return true, nil
	default:
		return false, fmt.Errorf("%s inválido: %q (usa 1/true/yes/on o 0/false/no/off)", key, os.Getenv(key))
	}
}

func envInt(key string, def int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s inválido: %w", key, err)
	}
	return n, nil
}

func envDuration(key string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err == nil && d < 0 {
		err = fmt.Errorf("duración negativa")
	}
	if err != nil {
		return 0, fmt.Errorf("%s inválido (ej: 720h; 0 = desactivado): %w", key, err)
	}
	return d, nil
}
