// Command gateway runs GatewayKit.
//
// The config path is taken from, in order: -config, the first positional
// argument, $GATEWAY_CONFIG, then ./gateway.yaml.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/sgteeples/gateway-kit/internal/config"
	"github.com/sgteeples/gateway-kit/internal/gateway"
)

const shutdownGrace = 15 * time.Second

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(logger); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	configFlag := flag.String("config", "", "path to gateway YAML config (or $GATEWAY_CONFIG)")
	flag.Parse()
	path := configPath(*configFlag, flag.Arg(0), os.Getenv("GATEWAY_CONFIG"))

	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	gw, err := gateway.New(cfg, logger)
	if err != nil {
		return err
	}
	defer gw.Close()

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Gateway.Port),
		Handler:           gw,
		ReadHeaderTimeout: 10 * time.Second, // slowloris protection
		IdleTimeout:       120 * time.Second,
		// No WriteTimeout: it would cap every response regardless of the
		// per-route upstream timeouts, which already bound slow upstreams.
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.ListenAndServe() }()
	logger.Info("gateway listening", "addr", srv.Addr, "config", path, "routes", len(cfg.Routes))

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
	}

	logger.Info("shutting down; draining in-flight requests", "grace", shutdownGrace.String())
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}

func configPath(candidates ...string) string {
	for _, c := range candidates {
		if c != "" {
			return c
		}
	}
	return "gateway.yaml"
}
