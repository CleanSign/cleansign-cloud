package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/CleanSign/cleansign/server/internal/app"
	"github.com/CleanSign/cleansign/server/internal/config"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))

	// `server` runs the API (with an embedded analysis worker unless WORKER_EMBEDDED=false);
	// `server worker` runs only the analysis worker.
	if len(os.Args) > 1 {
		if os.Args[1] == "worker" {
			return runWorker(log)
		}
		return fmt.Errorf("unknown command %q: run with no argument for the API server, or \"worker\"", os.Args[1])
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	a, err := app.New(ctx, cfg, log)
	if err != nil {
		return err
	}
	return a.Run(ctx, cfg.Port)
}

func runWorker(log *slog.Logger) error {
	cfg, err := config.LoadWorker()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	a, err := app.NewWorkerApp(ctx, cfg, log)
	if err != nil {
		return err
	}
	return a.Run(ctx, cfg.Worker.HealthPort)
}
