// external-dns-omada runs the Omada DNS provider as an ExternalDNS webhook
// provider: it listens on localhost for the webhook requests of an
// external-dns instance running with --provider=webhook and applies the
// planned changes to the LAN DNS entries of a TP-Link Omada site.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/RonaldPhilipsen/external-dns-omada/internal/config"
	"github.com/RonaldPhilipsen/external-dns-omada/internal/omada"
	"github.com/RonaldPhilipsen/external-dns-omada/internal/webhook"
)

const shutdownTimeout = 30 * time.Second

// BuildVersion and BuildCommitSha are set at build time via -ldflags, see
// the Dockerfile and the release workflow.
var (
	BuildVersion   = "devel"
	BuildCommitSha = "unknown"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("external-dns-omada: %v", err)
	}
}

func run() error {
	cfg, err := config.Parse(os.Args[1:])
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}
	setupLogging(cfg)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	provider, err := omada.New(ctx, cfg.Omada, omada.NoopTelemetry())
	if err != nil {
		return fmt.Errorf("initializing omada provider: %w", err)
	}
	log.Infof("connected to Omada controller for site %q; managed LAN networks: %v", cfg.Omada.SiteName, provider.LanNetworkIDs())

	web := webhook.NewServer(provider, cfg.MaxBodySize)

	providerSrv := &http.Server{
		Addr:              cfg.WebhookListen,
		Handler:           web.ProviderHandler(),
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	exposedSrv := &http.Server{
		Addr:              cfg.MetricsListen,
		Handler:           web.ExposedHandler(),
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	providerL, err := net.Listen("tcp", cfg.WebhookListen)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", cfg.WebhookListen, err)
	}
	exposedL, err := net.Listen("tcp", cfg.MetricsListen)
	if err != nil {
		providerL.Close()
		return fmt.Errorf("listening on %s: %w", cfg.MetricsListen, err)
	}

	log.Infof("serving the webhook provider API on %s", providerL.Addr().String())
	log.Infof("exposing /healthz and /metrics on %s", exposedL.Addr().String())

	errCh := make(chan error, 2)
	go func() { errCh <- providerSrv.Serve(providerL) }()
	go func() { errCh <- exposedSrv.Serve(exposedL) }()

	select {
	case <-ctx.Done():
		log.Infof("shutdown signal received")
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serving: %w", err)
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	var errs []error
	if err := providerSrv.Shutdown(shutdownCtx); err != nil {
		errs = append(errs, fmt.Errorf("shutting down provider API: %w", err))
	}
	if err := exposedSrv.Shutdown(shutdownCtx); err != nil {
		errs = append(errs, fmt.Errorf("shutting down exposed endpoints: %w", err))
	}
	return errors.Join(errs...)
}

func setupLogging(cfg *config.Config) {
	level, err := log.ParseLevel(cfg.LogLevel)
	if err != nil {
		log.Fatalf("invalid --log-level %q: %v", cfg.LogLevel, err)
	}
	log.SetLevel(level)
	switch cfg.LogFormat {
	case "json":
		log.SetFormatter(&log.JSONFormatter{})
	default:
		log.SetFormatter(&log.TextFormatter{FullTimestamp: true})
	}
	log.Infof("starting external-dns-omada %s (%s) (log level %s, format %s)", BuildVersion, BuildCommitSha, cfg.LogLevel, cfg.LogFormat)
}
