package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"quota-server/internal/app"
)

func main() {
	logger := log.New(os.Stdout, "quota-server ", log.LstdFlags)

	cfg := app.Config{
		StateFile:            envOrDefault("STATE_FILE", "./state.json"),
		OnScript:             envOrDefault("ON_SCRIPT", "./on.sh"),
		OffScript:            envOrDefault("OFF_SCRIPT", "./off.sh"),
		SessionTTL:           sessionTTL(),
		InitialAdminPassword: envOrDefault("INITIAL_ADMIN_PASSWORD", "admin"),
	}
	addr := envOrDefault("ADDR", ":8080")

	a, err := app.New(cfg, logger)
	if err != nil {
		logger.Fatalf("failed to initialize app: %v", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	a.StartBackground(ctx)

	srv := &http.Server{
		Addr:              addr,
		Handler:           a.Router(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			logger.Printf("shutdown error: %v", err)
		}
	}()

	logger.Printf("server listening on %s", addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Fatalf("server error: %v", err)
	}
}

func envOrDefault(key, def string) string {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	return v
}

func sessionTTL() time.Duration {
	raw := envOrDefault("SESSION_TTL_HOURS", "24")
	hours, err := strconv.Atoi(raw)
	if err != nil || hours <= 0 {
		return 24 * time.Hour
	}
	return time.Duration(hours) * time.Hour
}
