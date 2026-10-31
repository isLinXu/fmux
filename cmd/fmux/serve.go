package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/linxu/fmux/internal/api"
	"github.com/linxu/fmux/internal/build"
	"github.com/linxu/fmux/internal/config"
	"github.com/spf13/cobra"
)

func newServeCmd() *cobra.Command {
	var (
		cfgPath string
		listen  string
		mock    bool
		logLvl  string
	)
	c := &cobra.Command{
		Use:   "serve",
		Short: "Start the gateway",
		Long: "Start the OpenAI-compatible gateway. Clients point at\n" +
			"http://127.0.0.1:1976/v1 and select models by alias.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if cfgPath == "" {
				cfgPath = config.DefaultPath()
			}
			cfg, err := config.Load(cfgPath)
			if err != nil {
				return err
			}
			if mock {
				cfg.Backends = []config.Backend{{Name: "mock", Type: "mock", Kind: "local"}}
				cfg.Aliases = map[string]config.Alias{
					"mock-model": {Backend: "mock", Model: "mock-model"},
				}
				cfg.Default = "mock-model"
			}
			if listen != "" {
				cfg.Listen = listen
			}
			if logLvl != "" {
				cfg.LogLevel = logLvl
			}

			log := newLogger(cfg.LogLevel)
			bundle, err := build.Build(cfg, log)
			if err != nil {
				return err
			}

			srv := api.New(api.Options{
				Config:     cfg,
				Registry:   bundle.Registry,
				Supervisor: bundle.Supervisor,
				Budget:     bundle.Budget,
				Metrics:    bundle.Metrics,
				Log:        log,
			})
			httpSrv := &http.Server{
				Addr:              cfg.Listen,
				Handler:           srv.Handler(),
				ReadHeaderTimeout: 10 * time.Second,
				// No WriteTimeout: streaming responses can legitimately run long.
				IdleTimeout: 120 * time.Second,
			}

			ln, err := net.Listen("tcp", cfg.Listen)
			if err != nil {
				return fmt.Errorf("listen %s: %w", cfg.Listen, err)
			}
			log.Info("fmux listening",
				"addr", ln.Addr().String(),
				"config", cfg.SourcePath,
				"backends", bundle.Registry.Names(),
				"supervised", bundle.Supervisor.Names(),
				"budget_mb", bundle.Budget.LimitMB(),
				"version", api.Version,
			)

			errCh := make(chan error, 1)
			go func() {
				if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
					errCh <- err
				}
			}()

			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			// Idle local servers are stopped in the background so a laptop that
			// answered one request does not keep 18 GB resident all day.
			reapEvery := cfg.ReapInterval
			if reapEvery <= 0 {
				reapEvery = 30 * time.Second
			}
			bundle.Supervisor.StartWatcher(ctx, reapEvery)

			select {
			case err := <-errCh:
				return err
			case <-ctx.Done():
				log.Info("shutting down")
				shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				bundle.Supervisor.Shutdown(shutdownCtx)
				return httpSrv.Shutdown(shutdownCtx)
			}
		},
	}
	c.Flags().StringVar(&cfgPath, "config", "", "config file (default ~/.fmux/config.yaml)")
	c.Flags().StringVar(&listen, "listen", "", "bind address (default 127.0.0.1:1976)")
	c.Flags().BoolVar(&mock, "mock", false, "run with the built-in mock backend (no real inference)")
	c.Flags().StringVar(&logLvl, "log-level", "", "debug|info|warn|error")
	return c
}

func newLogger(level string) *slog.Logger {
	var lv slog.Level
	switch level {
	case "debug":
		lv = slog.LevelDebug
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	h := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lv})
	return slog.New(h)
}
