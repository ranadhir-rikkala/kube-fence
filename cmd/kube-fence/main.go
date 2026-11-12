package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ranadhir-rikkala/kube-fence/internal/webhook"
	"github.com/ranadhir-rikkala/kube-fence/pkg/ledger"
	"github.com/ranadhir-rikkala/kube-fence/pkg/policy"
)

func main() {
	var (
		addr       = flag.String("addr", ":8443", "listen address")
		configPath = flag.String("config", "/etc/kube-fence/config.yaml", "policy config file (YAML or JSON)")
		certFile   = flag.String("tls-cert", "/etc/kube-fence/tls/tls.crt", "TLS certificate")
		keyFile    = flag.String("tls-key", "/etc/kube-fence/tls/tls.key", "TLS private key")
		retention  = flag.Duration("ledger-retention", 24*time.Hour, "how long ledger entries are kept")
	)
	flag.Parse()
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	if err := run(log, *addr, *configPath, *certFile, *keyFile, *retention); err != nil {
		log.Error("exiting", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger, addr, configPath, certFile, keyFile string, retention time.Duration) error {
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return err
	}
	cfg, err := policy.ParseConfig(raw)
	if err != nil {
		return err
	}
	engine := policy.NewEngine(cfg, ledger.NewMemory(retention), time.Now)
	srv := &http.Server{
		Addr:              addr,
		Handler:           webhook.NewServer(engine, log, time.Now).Handler(),
		TLSConfig:         &tls.Config{MinVersion: tls.VersionTLS12},
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServeTLS(certFile, keyFile) }()
	log.Info("kube-fence listening", "addr", addr, "mode", cfg.Mode, "dedupeWindow", cfg.Dedupe.Window.String())

	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
	return nil
}
