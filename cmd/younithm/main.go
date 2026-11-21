package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"younithm/internal/config"
	"younithm/internal/db"
	"younithm/internal/secrets"
	"younithm/internal/server"
	"younithm/internal/version"
)

func main() {
	cfg := config.Load()
	if err := cfg.Validate(); err != nil {
		log.Fatalf("config: %v", err)
	}

	// Master key first: a bad or unreadable key must stop startup, never
	// silently produce a fresh key that orphans stored secrets.
	box, keySrc, err := secrets.OpenBox(secrets.KeyOptions{ConfigDir: cfg.ConfigDir})
	if err != nil {
		log.Fatalf("master key: %v", err)
	}
	redactor := secrets.NewRedactor()
	log.SetOutput(redactor.Writer(os.Stderr))
	log.Printf("master key loaded (source: %s)", keySrc)

	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "reset-admin":
			if err := runResetAdmin(cfg.ConfigDir, os.Args[2:], os.Stdin, os.Stdout); err != nil {
				log.Fatalf("reset-admin: %v", err)
			}
			return
		case "serve":
		default:
			log.Fatalf("unknown command %q (commands: serve, reset-admin)", os.Args[1])
		}
	}

	pools, err := db.OpenPools(cfg.ConfigDir)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer pools.Close()

	if _, err := db.MigrateWithBackup(pools); err != nil {
		log.Fatalf("migrate: %v", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           server.New(pools, server.WithSecrets(box, redactor), server.WithBackground(ctx)),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("younithm %s listening on %s (config dir: %s)", version.Version, cfg.Addr, cfg.ConfigDir)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("serve: %v", err)
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}
