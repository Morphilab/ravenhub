// Package cli holds the command dispatch shared by the ravenhub and
// ravenctl binaries (same codebase, two entry points).
package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"ravenhub/internal/api"
	"ravenhub/internal/config"
	"ravenhub/internal/crypto"
	"ravenhub/internal/db"
	"ravenhub/internal/importer"
	"ravenhub/internal/mcp"
	"ravenhub/internal/metrics"
	"ravenhub/internal/store"
	"ravenhub/internal/worker"
)

var Version = "dev"

// Run executes a subcommand and returns the process exit code.
func Run(args []string) int {
	cmd := "serve"
	if len(args) > 0 {
		cmd = args[0]
	}
	var err error
	switch cmd {
	case "serve":
		err = runServe()
	case "migrate":
		err = runMigrate()
	case "import":
		if len(args) < 2 {
			err = fmt.Errorf("uso: import <seed.yaml>")
		} else {
			err = runImport(args[1])
		}
	case "mcp":
		err = mcp.Run()
	case "re-encrypt":
		err = runReEncrypt()
	case "version":
		fmt.Println("ravenhub " + Version)
	default:
		fmt.Fprintf(os.Stderr, "uso: ravenhub [serve|migrate|import <seed.yaml>|mcp|re-encrypt|version]\n")
		return 2
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	return 0
}

// setLogLevel aplica RAVENHUB_LOG_LEVEL al logger global. Antes el valor se
// leía en Config pero no se usaba (RAVENHUB_LOG_LEVEL=debug no hacía nada).
func setLogLevel(level string) {
	l := slog.LevelInfo
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		l = slog.LevelDebug
	case "warn":
		l = slog.LevelWarn
	case "error":
		l = slog.LevelError
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: l})))
}

func setup() (*config.Config, *store.Store, error) {
	if fi, err := os.Stat(".env"); err == nil && fi.Mode().Perm()&0077 != 0 {
		// .env con bot tokens y master key legible por grupo/otros
		slog.Warn(".env legible por grupo/otros: chmod 600 .env",
			"mode", fi.Mode().Perm().String())
	}
	cfg, err := config.Load()
	if err != nil {
		return nil, nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pool, err := db.Open(ctx, cfg.DSN)
	if err != nil {
		return nil, nil, err
	}
	// Las migraciones pueden crear índices/ALTER sobre tablas grandes: 15s
	// era un timeout de conexión, no de migración.
	migCtx, migCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer migCancel()
	if err := db.Migrate(migCtx, pool); err != nil {
		return nil, nil, fmt.Errorf("migraciones: %w", err)
	}
	st := &store.Store{Pool: pool, MasterKey: cfg.MasterKeyBytes(),
		MidMasterKey: cfg.MidMasterKeyBytes(), LegacyMasterKey: cfg.LegacyMasterKeyBytes()}
	return cfg, st, nil
}

// resolveAdminToken decide el token admin al arrancar: el del entorno o, SOLO
// con RAVENHUB_DEV=1, uno autogenerado impreso UNA vez por stdout. Sin token y
// sin DEV es un error (fail-closed): docker logs y journald persisten stdout,
// así que en producción una credencial admin en un log eterno no puede ser el
// default. Extraída de runServe para poder testearla sin arrancar el hub.
func resolveAdminToken(cfg *config.Config) (string, error) {
	if cfg.AdminToken != "" {
		return cfg.AdminToken, nil
	}
	if !cfg.Dev {
		return "", errors.New("RAVENHUB_ADMIN_TOKEN es obligatorio (genera uno: openssl rand -hex 32); " +
			"en desarrollo puedes usar RAVENHUB_DEV=1 para autogenerarlo")
	}
	full, _, err := crypto.NewAdminToken()
	if err != nil {
		return "", err
	}
	fmt.Println("RAVENHUB_ADMIN_TOKEN generado SOLO PARA DEV:", full)
	slog.Warn("RAVENHUB_ADMIN_TOKEN no definido: token admin generado e impreso en stdout (solo dev)",
		"nota", "configúralo en .env para producción")
	return full, nil
}

func runServe() error {
	cfg, st, err := setup()
	if err != nil {
		return err
	}
	setLogLevel(cfg.LogLevel)

	// Bootstrap del token admin (ravenctl → admin API).
	adminToken, err := resolveAdminToken(cfg)
	if err != nil {
		return err
	}
	bootCtx, bootCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer bootCancel()
	if err := st.EnsureAdminToken(bootCtx, adminToken); err != nil {
		return fmt.Errorf("bootstrap admin token: %w", err)
	}

	var workerAlive atomic.Bool
	workerAlive.Store(true)
	srv := api.NewServer(cfg, st, workerAlive.Load)
	// WriteTimeout cubre ?sync=true: el cliente espera hasta SyncWait y la
	// respuesta viaja después; un WriteTimeout menor corta la conexión con el
	// mensaje ya encolado (el agente reintenta y duplica). 20s mínimo de margen
	// para el resto de respuestas.
	writeTimeout := 20 * time.Second
	if d := cfg.SyncWait + 5*time.Second; d > writeTimeout {
		writeTimeout = d
	}
	httpServer := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		slog.Info("señal recibida: apagado con drenado de workers")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()

	w := worker.New(st, cfg.Workers, cfg.Lease)
	w.RetentionMessages = cfg.RetentionMessages
	w.RetentionAudit = cfg.RetentionAudit
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		// false SOLO cuando los workers terminan: /readyz refleja su estado real
		defer workerAlive.Store(false)
		w.Run(ctx)
	}()

	// Pre-registra las series del contrato de silencio en 0: un contador
	// ausente en /metrics no es lo mismo que un contador en 0
	metrics.Prewarm()

	slog.Info("ravenhub sirviendo", "addr", cfg.Listen, "workers", cfg.Workers)
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	// drenaje acotado: dejar que los envíos en curso asienten su resultado
	// (lo que no llegue a tiempo lo recupera el reaper de leases al reiniciar)
	select {
	case <-workerDone:
	case <-time.After(8 * time.Second):
		slog.Warn("workers no terminaron a tiempo en el apagado; el reaper recuperará lo pendiente")
	}
	return nil
}

func runMigrate() error {
	if _, _, err := setup(); err != nil {
		return err
	}
	fmt.Println("migraciones aplicadas")
	return nil
}

func runImport(seedPath string) error {
	// Credenciales de seed separadas del runtime del Hub. Si no existe el
	// archivo, se mantiene la compatibilidad con .env (ya cargado por main).
	seedEnv := os.Getenv("RAVENHUB_SEED_ENV")
	if seedEnv == "" {
		seedEnv = ".env.seed"
	}
	config.LoadEnvFile(seedEnv)

	if _, st, err := setup(); err != nil {
		return err
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := importer.Import(ctx, st, seedPath); err != nil {
			return err
		}
	}
	fmt.Println("seed aplicado")
	return nil
}

// runReEncrypt re-cifra las creds de todas las cuentas con la passphrase
// ACTUAL (RAVENHUB_MASTER_KEY) en el formato v3. Rotación de master key:
//  1. exporta la passphrase vieja en RAVENHUB_MASTER_KEY_OLD
//  2. pon la nueva en RAVENHUB_MASTER_KEY (.env)
//  3. ravenhub re-encrypt  →  4) retira RAVENHUB_MASTER_KEY_OLD
//
// Sin la OLD definida, normaliza el formato (upgrade de filas pre-v3) sin
// rotar nada. Sale ≠0 si alguna cuenta no fue descifrable (quedó intacta).
func runReEncrypt() error {
	cfg, st, err := setup()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	ok, failed, err := st.ReEncryptAccounts(ctx, cfg.OldMasterKeyChain())
	if err != nil {
		return err
	}
	fmt.Printf("re-cifradas %d cuentas", ok)
	if failed > 0 {
		fmt.Printf(" (%d NO descifrables con las claves disponibles — quedaron intactas)", failed)
	}
	fmt.Println()
	if failed > 0 {
		return fmt.Errorf("%d cuentas no descifrables: revisa RAVENHUB_MASTER_KEY_OLD", failed)
	}
	return nil
}
